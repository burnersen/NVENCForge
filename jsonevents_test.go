//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

// jsonevents_test.go — Tests für den -json-Ereigniskanal.
//
// Der wichtigste Test dieser Datei ist TestEmitEventStaysSilentWithoutFlag:
// Er sichert die Zusage ab, dass NVENCForge ohne -json kein einziges Byte
// zusätzlich ausgibt. Bräche das, würde jede Ausgabe des Programms für normale
// Nutzer mit JSON-Zeilen verschmutzt.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pterm/pterm"
)

// captureJSON schaltet den JSON-Modus für die Dauer eines Tests ein, fängt alles
// ab, was geschrieben wird, und stellt danach den Ursprungszustand wieder her.
func captureJSON(t *testing.T, enabled bool, body func()) []string {
	t.Helper()

	prevMode, prevSink := jsonMode, jsonSink
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	jsonMode, jsonSink = enabled, w
	t.Cleanup(func() {
		jsonMode, jsonSink = prevMode, prevSink
	})

	body()
	_ = w.Close()

	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	_ = r.Close()
	return lines
}

func TestEmitEventStaysSilentWithoutFlag(t *testing.T) {
	lines := captureJSON(t, false, func() {
		emitEvent(eventRun{Ev: "run", Version: "test"})
		emitStage("encode")
		emitFileStart(1, 1, `C:\tmp\film.mkv`)
		emitFileResult(1, ProcessResult{InputFile: `C:\tmp\film.mkv`, Success: true})
		emitRunSummary([]ProcessResult{{Success: true}}, time.Second)
	})
	if len(lines) != 0 {
		t.Fatalf("ohne -json darf nichts geschrieben werden, es kamen %d Zeilen: %v",
			len(lines), lines)
	}
}

func TestEmitEventWritesOneJSONLinePerEvent(t *testing.T) {
	lines := captureJSON(t, true, func() {
		emitEvent(eventRun{Ev: "run", Version: "9.9.9", Mode: "convert", Files: 2})
		emitStage("analyze")
	})
	if len(lines) != 2 {
		t.Fatalf("erwartet 2 Zeilen, bekommen %d: %v", len(lines), lines)
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Errorf("Zeile %d ist kein gültiges JSON: %q", i+1, l)
		}
	}

	var run eventRun
	if err := json.Unmarshal([]byte(lines[0]), &run); err != nil {
		t.Fatalf("run-Ereignis nicht lesbar: %v", err)
	}
	if run.Ev != "run" || run.Version != "9.9.9" || run.Files != 2 {
		t.Errorf("run-Ereignis falsch übertragen: %+v", run)
	}
}

// Die Reihenfolge der Abfragen in resultStatus ist die eigentliche Logik:
// ein abgebrochener Lauf mit Vorschaudatei ist weder "success" noch "failed".
func TestResultStatusOrdering(t *testing.T) {
	cases := []struct {
		name string
		in   ProcessResult
		want string
	}{
		{"erfolgreich", ProcessResult{Success: true}, "success"},
		{"uebersprungen", ProcessResult{Skipped: true}, "skipped"},
		{"fehlgeschlagen", ProcessResult{ErrMsg: "kaputt"}, "failed"},
		{"vorschau schlaegt erfolg", ProcessResult{IsPreview: true, Success: true}, "preview"},
		{"vorschau schlaegt uebersprungen", ProcessResult{IsPreview: true, Skipped: true}, "preview"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resultStatus(c.in); got != c.want {
				t.Errorf("resultStatus = %q, erwartet %q", got, c.want)
			}
		})
	}
}

func TestEmitFileResultComputesSavings(t *testing.T) {
	lines := captureJSON(t, true, func() {
		emitFileResult(3, ProcessResult{
			InputFile:  `C:\tmp\film.mkv`,
			OutputFile: `C:\tmp\output\film.h265.mkv`,
			InputMB:    200,
			SavedMB:    50,
			Success:    true,
		})
	})
	if len(lines) != 1 {
		t.Fatalf("erwartet 1 Zeile, bekommen %d", len(lines))
	}

	var res eventResult
	if err := json.Unmarshal([]byte(lines[0]), &res); err != nil {
		t.Fatalf("result-Ereignis nicht lesbar: %v", err)
	}
	if res.OutMB != 150 {
		t.Errorf("out_mb = %v, erwartet 150 (200 - 50)", res.OutMB)
	}
	if res.SavedPct != 25 {
		t.Errorf("saved_pct = %v, erwartet 25", res.SavedPct)
	}
	if res.Name != "film.mkv" {
		t.Errorf("name = %q, erwartet den reinen Dateinamen", res.Name)
	}
	if res.Index != 3 {
		t.Errorf("index = %d, erwartet 3", res.Index)
	}
}

// Eine übersprungene Datei hat keine bekannte Eingangsgröße. Dann darf die
// Ausgabegröße NICHT als negativer Wert oder als Division durch null erscheinen.
func TestEmitFileResultWithoutSizesStaysZero(t *testing.T) {
	lines := captureJSON(t, true, func() {
		emitFileResult(1, ProcessResult{InputFile: `C:\tmp\film.mkv`, Skipped: true})
	})
	var res eventResult
	if err := json.Unmarshal([]byte(lines[0]), &res); err != nil {
		t.Fatalf("result-Ereignis nicht lesbar: %v", err)
	}
	if res.OutMB != 0 || res.SavedPct != 0 {
		t.Errorf("ohne Größen müssen out_mb und saved_pct 0 sein, sind %v / %v",
			res.OutMB, res.SavedPct)
	}
	if res.Status != "skipped" {
		t.Errorf("status = %q, erwartet skipped", res.Status)
	}
}

func TestEmitRunSummaryCounts(t *testing.T) {
	results := []ProcessResult{
		{Success: true, SavedMB: 10},
		{Success: true, SavedMB: 5},
		{Skipped: true},
		{ErrMsg: "kaputt"},
		{IsPreview: true, PreviewPct: 40},
	}
	lines := captureJSON(t, true, func() {
		emitRunSummary(results, 90*time.Second)
	})

	var sum eventSummary
	if err := json.Unmarshal([]byte(lines[0]), &sum); err != nil {
		t.Fatalf("summary nicht lesbar: %v", err)
	}
	if sum.Files != 5 {
		t.Errorf("files = %d, erwartet 5", sum.Files)
	}
	if sum.Success != 2 {
		t.Errorf("success = %d, erwartet 2", sum.Success)
	}
	if sum.Skipped != 1 {
		t.Errorf("skipped = %d, erwartet 1", sum.Skipped)
	}
	// Abbruch mit Vorschau zählt bewusst NICHT als Erfolg.
	if sum.Failed != 2 {
		t.Errorf("failed = %d, erwartet 2 (Fehler + Vorschau)", sum.Failed)
	}
	if sum.SavedMB != 15 {
		t.Errorf("saved_mb = %v, erwartet 15", sum.SavedMB)
	}
	if sum.ElapsedSec != 90 {
		t.Errorf("elapsed_sec = %v, erwartet 90", sum.ElapsedSec)
	}
}

// emitStage holt sich die Dateinummer aus jsonCurrentIndex, das emitFileStart
// setzt. Bricht diese Kopplung, meldet die Oberfläche die Phase der falschen
// Datei — bei einem Stapel besonders irreführend.
func TestStageUsesIndexFromLastFileStart(t *testing.T) {
	prev := jsonCurrentIndex.Load()
	t.Cleanup(func() { jsonCurrentIndex.Store(prev) })

	lines := captureJSON(t, true, func() {
		emitFileStart(7, 9, `C:\tmp\film.mkv`)
		emitStage("encode")
	})
	if len(lines) != 2 {
		t.Fatalf("erwartet 2 Zeilen, bekommen %d", len(lines))
	}

	var stage eventStage
	if err := json.Unmarshal([]byte(lines[1]), &stage); err != nil {
		t.Fatalf("stage nicht lesbar: %v", err)
	}
	if stage.Index != 7 {
		t.Errorf("stage.index = %d, erwartet 7 (aus dem file-Ereignis)", stage.Index)
	}
}

// captureOSStreams biegt die beiden Betriebssystem-Kanäle für die Dauer eines
// Tests auf Röhren um und gibt zurück, was dort ankam.
//
// Nötig, weil die bewegten Anzeigen NICHT über die JSON-Senke laufen, sondern
// über die ganz normale Bildschirmausgabe — genau dort liest die Oberfläche
// mit, und genau dort entstand die Zeilenflut.
func captureOSStreams(t *testing.T, body func()) (fromStdout, fromStderr string) {
	t.Helper()

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	prevStdout, prevStderr := os.Stdout, os.Stderr
	t.Cleanup(func() {
		os.Stdout, os.Stderr = prevStdout, prevStderr
		pterm.SetDefaultOutput(prevStdout)
	})
	os.Stdout, os.Stderr = wOut, wErr

	body()

	_ = wOut.Close()
	_ = wErr.Close()
	out, _ := io.ReadAll(rOut)
	errOut, _ := io.ReadAll(rErr)
	_ = rOut.Close()
	_ = rErr.Close()
	return string(out), string(errOut)
}

// Die mehrzeilige Fortschrittsanzeige ist der lauteste Teil der Ausgabe: Sie
// zeichnet sich zehnmal je Sekunde neu. An einem Terminal überschreibt sie sich
// dabei selbst, an einer Leitung wird aus jeder Neuzeichnung eine eigene Zeile.
// Im -json-Modus muss sie deshalb ganz schweigen — ohne das Flag aber
// unverändert zeichnen.
func TestStartProgressAreaSilentOnlyInJSONMode(t *testing.T) {
	prevMode := jsonMode
	t.Cleanup(func() { jsonMode = prevMode })

	const content = "Frame 1\nPosition 0:01\nFrames/s 60"

	jsonMode = true
	out, errOut := captureOSStreams(t, func() {
		area := startProgressArea()
		area.Update(content)
		_ = area.Stop()
	})
	if out != "" || errOut != "" {
		t.Errorf("mit -json darf die Fortschrittsanzeige nichts schreiben, kam: stdout=%q stderr=%q",
			out, errOut)
	}

	jsonMode = false
	out, _ = captureOSStreams(t, func() {
		area := startProgressArea()
		area.Update(content)
		_ = area.Stop()
	})
	if !strings.Contains(out, "Frames/s 60") {
		t.Errorf("ohne -json muss die gewohnte Anzeige weiter zeichnen, kam: %q", out)
	}
}

// Der Spinner der Auto-CQ-Analyse war die zweite Quelle der Zeilenflut: rund
// 100 Zeilen je 4K-Datei. Dieser Test lässt ihn wirklich laufen, statt nur
// seinen Ausgabekanal zu vergleichen — sonst bliebe ungeprüft, ob er ihn
// überhaupt benutzt.
func TestEnableJSONModeSilencesSpinnerAndProgressbar(t *testing.T) {
	prevMode, prevSink := jsonMode, jsonSink
	prevSpinner, prevBar := pterm.DefaultSpinner, pterm.DefaultProgressbar
	t.Cleanup(func() {
		jsonMode, jsonSink = prevMode, prevSink
		pterm.DefaultSpinner, pterm.DefaultProgressbar = prevSpinner, prevBar
	})

	out, errOut := captureOSStreams(t, func() {
		jsonMode = true
		enableJSONMode()

		spinner, _ := pterm.DefaultSpinner.
			WithText("Auto-CQ: measuring VMAF at CQ 26...").
			Start()
		// Der Spinner zeichnet alle 100 ms; drei Runden genügen als Nachweis.
		time.Sleep(300 * time.Millisecond)
		_ = spinner.Stop()
		// Der Spinner läuft in einer eigenen Goroutine — ihr Zeit zum Auslaufen
		// geben, sonst schriebe sie erst nach dem Schließen der Röhre.
		time.Sleep(50 * time.Millisecond)
	})
	if out != "" || errOut != "" {
		t.Errorf("mit -json darf der Spinner nichts schreiben, kam: stdout=%q stderr=%q",
			out, errOut)
	}

	if pterm.DefaultProgressbar.Writer != io.Discard {
		t.Errorf("der Fortschrittsbalken schreibt nach %v statt ins Leere",
			pterm.DefaultProgressbar.Writer)
	}
}

func TestConsumeJSONFlag(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantFound bool
		wantArgs  []string
	}{
		{"ohne Flag", []string{"NVENCForge.exe", "film.mkv"}, false,
			[]string{"NVENCForge.exe", "film.mkv"}},
		{"mit Flag", []string{"NVENCForge.exe", "-json", "film.mkv"}, true,
			[]string{"NVENCForge.exe", "film.mkv"}},
		{"Grossschreibung egal", []string{"NVENCForge.exe", "-JSON", "film.mkv"}, true,
			[]string{"NVENCForge.exe", "film.mkv"}},
		{"Dateiname bleibt unangetastet", []string{"NVENCForge.exe", "json.mkv"}, false,
			[]string{"NVENCForge.exe", "json.mkv"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev := os.Args
			t.Cleanup(func() { os.Args = prev })

			os.Args = append([]string{}, c.args...)
			got := consumeJSONFlag()
			if got != c.wantFound {
				t.Errorf("consumeJSONFlag = %v, erwartet %v", got, c.wantFound)
			}
			if len(os.Args) != len(c.wantArgs) {
				t.Fatalf("os.Args = %v, erwartet %v", os.Args, c.wantArgs)
			}
			for i := range c.wantArgs {
				if os.Args[i] != c.wantArgs[i] {
					t.Errorf("os.Args[%d] = %q, erwartet %q", i, os.Args[i], c.wantArgs[i])
				}
			}
		})
	}
}

//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

// JSONEvents.go — maschinenlesbare Ausgabe für Oberflächen und Skripte (-json).
//
// Zweck: Eine grafische Oberfläche soll den Lauf verfolgen können, ohne die für
// Menschen gedachte Bildschirmausgabe auseinanderzupflücken. Jede Änderung an
// der Anzeige würde so ein Mitlesen sonst brechen (v1.15.0 war genau so eine
// reine Anzeige-Version).
//
// Aufbau: Mit -json wandert die KOMPLETTE Bildschirmausgabe auf den Fehlerkanal
// (stderr) und der Hauptkanal (stdout) trägt nur noch eine JSON-Zeile je
// Ereignis. Für den Menschen ändert sich dadurch nichts — beide Kanäle landen
// in einem Terminal ohnehin nebeneinander.
//
// Ohne -json ist diese Datei vollständig wirkungslos: jede Emit-Funktion kehrt
// sofort zurück, und keine einzige Ausgabe wird umgeleitet.
//
// Gemessen 2026-08-17: os.Stdout umzubiegen allein genügt NICHT. Es erwischt
// fmt.* und die Fortschritts-Area, aber weder die pterm-Drucker noch Tabellen
// oder Balken — die binden ihren Ausgabekanal beim Programmstart. Erst alle drei
// Umleitungen in enableJSONMode zusammen halten stdout sauber.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pterm/pterm"
)

// jsonMode ist true, sobald -json übergeben wurde. Wird einmal zu Beginn von
// main() gesetzt und danach nur noch gelesen.
var jsonMode bool

// jsonSink ist der ECHTE Hauptkanal, gesichert bevor os.Stdout umgebogen wird.
// Nur hierhin werden Ereignisse geschrieben.
var jsonSink *os.File

// jsonMu schützt das Schreiben: die Fortschrittsauswertung liest FFmpeg in einer
// eigenen Goroutine, zwei halbe Zeilen ineinander wären für die Gegenseite nicht
// mehr lesbar.
var jsonMu sync.Mutex

// jsonCurrentIndex ist die Nummer der Datei, die gerade bearbeitet wird.
//
// Hintergrund: Die Phasen-Meldung "analyze" gehört an die Stelle, an der die
// Qualitätsmessung WIRKLICH beginnt — tief in autoDetectCQ, hinter allen
// Abbruchgründen (zu kurzes Video, unbekannte Bildrate). Diese Funktion kennt
// die Dateinummer nicht, und ihre Signatur nur für eine Anzeige zu erweitern
// wäre ein unnötiger Eingriff. Da jeder Programmlauf immer genau eine Datei
// gleichzeitig bearbeitet — auch wenn mehrere Programme parallel laufen, denn
// jedes ist ein eigener Prozess — genügt diese eine Angabe.
var jsonCurrentIndex atomic.Int64

// consumeJSONFlag sucht in os.Args nach "-json" (Groß-/Kleinschreibung egal),
// entfernt den Eintrag und meldet, ob er da war. Das Entfernen ist wichtig,
// damit das Flag später nicht als Dateiname behandelt wird — genauso wie
// consumeDebugFlag es für -debug macht.
func consumeJSONFlag() bool {
	found := false
	out := os.Args[:0]
	for _, a := range os.Args {
		if strings.EqualFold(a, "-json") {
			found = true
			continue
		}
		out = append(out, a)
	}
	os.Args = out
	return found
}

// enableJSONMode trennt die beiden Kanäle. Muss so früh wie möglich in main()
// laufen — vor der ersten Ausgabe, sonst steht sie schon im falschen Kanal.
//
// Die drei Umleitungen sind einzeln nötig und wurden am 2026-08-17 gemessen:
//   - os.Stdout erwischt fmt.* und die Fortschritts-Area,
//   - SetDefaultOutput die Drucker, Überschriften und Tabellen,
//   - der Fortschrittsbalken hat einen eigenen Writer und ignoriert beides.
func enableJSONMode() {
	jsonSink = os.Stdout
	os.Stdout = os.Stderr
	pterm.SetDefaultOutput(os.Stderr)
	pterm.DefaultProgressbar = *pterm.DefaultProgressbar.WithWriter(os.Stderr)
}

// emitEvent schreibt ein Ereignis als eine Zeile JSON.
//
// Fehler werden bewusst verschluckt: Der einzige realistische Fall ist eine
// Oberfläche, die sich beendet hat, während die Konvertierung noch läuft. Die
// Konvertierung deshalb abzubrechen wäre falsch — sie soll ungestört zu Ende
// laufen. Ein Marshal-Fehler kann bei diesen Strukturen nicht auftreten (keine
// Kanäle, keine Funktionen, keine Zyklen).
func emitEvent(v any) {
	if !jsonMode || jsonSink == nil {
		return
	}
	line, err := json.Marshal(v)
	if err != nil {
		return
	}
	line = append(line, '\n')

	jsonMu.Lock()
	defer jsonMu.Unlock()
	_, _ = jsonSink.Write(line)
}

// ----------------------------------------------------------------------------
// Die Ereignisse
//
// Je Ereignisart eine eigene Struktur statt einer Sammelstruktur mit dreißig
// optionalen Feldern: so kann der Übersetzer falsche Felder abfangen, und wer
// die Datei liest, sieht sofort, welche Angaben zu welchem Ereignis gehören.
// ----------------------------------------------------------------------------

// eventRun eröffnet den Lauf: was für ein Programm, welcher Modus, wie viele
// Dateien. Kommt genau einmal, bevor die erste Datei beginnt.
type eventRun struct {
	Ev      string `json:"ev"` // "run"
	Version string `json:"version"`
	Mode    string `json:"mode"`  // convert | davinci | split | join
	Codec   string `json:"codec"` // h265 | av1
	Encoder string `json:"encoder"`
	Files   int    `json:"files"`
}

// eventFile meldet den Beginn einer Datei.
type eventFile struct {
	Ev    string  `json:"ev"` // "file"
	Index int     `json:"index"`
	Total int     `json:"total"`
	Name  string  `json:"name"`
	Path  string  `json:"path"`
	InMB  float64 `json:"in_mb"`
}

// eventStage sagt, WAS gerade passiert. Ohne diese Angabe stünde die Anzeige
// während der Qualitätsmessung minutenlang bei null, ohne dass jemand wüsste,
// warum: die Analyse erzeugt keine Fortschrittswerte.
type eventStage struct {
	Ev    string `json:"ev"` // "stage"
	Index int    `json:"index"`
	Stage string `json:"stage"` // analyze | encode | remux | mp4
}

// eventProgress ist die einzige Ereignisart, die häufig kommt (rund zehnmal je
// Sekunde, im selben Takt wie die Bildschirmanzeige).
type eventProgress struct {
	Ev      string  `json:"ev"` // "progress"
	Index   int     `json:"index"`
	Pct     float64 `json:"pct"`
	PosSec  float64 `json:"pos_sec"`
	ETA     string  `json:"eta"`
	FPS     string  `json:"fps"`
	Speed   string  `json:"speed"`
	Bitrate string  `json:"bitrate"`
	EstMB   float64 `json:"est_mb,omitempty"`
	InMB    float64 `json:"in_mb,omitempty"`
}

// eventResult schließt eine Datei ab. Status "preview" bedeutet: abgebrochen,
// aber das angefangene Stück ist als abspielbare Datei erhalten.
type eventResult struct {
	Ev       string  `json:"ev"` // "result"
	Index    int     `json:"index"`
	Status   string  `json:"status"` // success | skipped | failed | preview
	Name     string  `json:"name"`
	Output   string  `json:"output,omitempty"`
	InMB     float64 `json:"in_mb"`
	OutMB    float64 `json:"out_mb"`
	SavedMB  float64 `json:"saved_mb"`
	SavedPct float64 `json:"saved_pct"`
	NoAudio  bool    `json:"no_audio,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// eventSummary beendet den Lauf. Danach folgt keine weitere Zeile.
type eventSummary struct {
	Ev         string  `json:"ev"` // "summary"
	Files      int     `json:"files"`
	Success    int     `json:"success"`
	Skipped    int     `json:"skipped"`
	Failed     int     `json:"failed"`
	SavedMB    float64 `json:"saved_mb"`
	ElapsedSec float64 `json:"elapsed_sec"`
}

// ----------------------------------------------------------------------------
// Bequeme Absender
// ----------------------------------------------------------------------------

// emitStage meldet, was gerade passiert. Die Dateinummer holt sich die Funktion
// selbst (siehe jsonCurrentIndex), damit sie auch tief im Ablauf mit einer
// einzigen Zeile aufgerufen werden kann.
func emitStage(stage string) {
	if !jsonMode {
		return
	}
	emitEvent(eventStage{
		Ev:    "stage",
		Index: int(jsonCurrentIndex.Load()),
		Stage: stage,
	})
}

// resultStatus übersetzt das interne Ergebnis in einen der vier Zustände, die
// eine Oberfläche anzeigen kann. Die Reihenfolge der Abfragen ist wichtig:
// ein Vorschau-Ergebnis ist nicht erfolgreich, und übersprungen ist kein Fehler.
func resultStatus(r ProcessResult) string {
	switch {
	case r.IsPreview:
		return "preview"
	case r.Skipped:
		return "skipped"
	case r.Success:
		return "success"
	default:
		return "failed"
	}
}

// runModeName benennt den Betriebsmodus so, wie eine Oberfläche ihn anzeigen
// würde. Die drei Werkzeug-Modi schließen sich gegenseitig aus, weil sie das
// erste Argument belegen.
func runModeName() string {
	switch {
	case davinciMode:
		return "davinci"
	case splitMode:
		return "split"
	case joinMode:
		return "join"
	default:
		return "convert"
	}
}

// emitRunStart eröffnet den Lauf.
func emitRunStart(cfg *AppConfig, files int) {
	if !jsonMode {
		return
	}
	codec := "h265"
	if cfg.av1 {
		codec = "av1"
	}
	encoder := encoderNvidia
	if cpuModeActive {
		encoder = encoderCPU
	}
	emitEvent(eventRun{
		Ev:      "run",
		Version: appVersion,
		Mode:    runModeName(),
		Codec:   codec,
		Encoder: encoder,
		Files:   files,
	})
}

// emitFileStart meldet, dass die nächste Datei an der Reihe ist, und hinterlegt
// ihre Nummer für alle Meldungen, die sie selbst nicht kennen.
func emitFileStart(index, total int, path string) {
	if !jsonMode {
		return
	}
	jsonCurrentIndex.Store(int64(index))
	emitEvent(eventFile{
		Ev:    "file",
		Index: index,
		Total: total,
		Name:  filepath.Base(path),
		Path:  path,
		InMB:  float64(fileSizeBytes(path)) / 1024 / 1024,
	})
}

// emitFileResult schließt eine Datei ab.
//
// Die Ausgabegröße wird aus Eingangsgröße minus Ersparnis berechnet, weil das
// Programm sie intern genau so führt; ohne bekannte Eingangsgröße (übersprungene
// Dateien) bleiben beide Werte bei null statt zu raten.
func emitFileResult(index int, r ProcessResult) {
	if !jsonMode {
		return
	}
	outMB, savedPct := 0.0, 0.0
	if r.InputMB > 0 {
		outMB = r.InputMB - r.SavedMB
		savedPct = r.SavedMB / r.InputMB * 100
	}
	emitEvent(eventResult{
		Ev:       "result",
		Index:    index,
		Status:   resultStatus(r),
		Name:     filepath.Base(r.InputFile),
		Output:   r.OutputFile,
		InMB:     r.InputMB,
		OutMB:    outMB,
		SavedMB:  r.SavedMB,
		SavedPct: savedPct,
		NoAudio:  r.NoAudio,
		Message:  r.ErrMsg,
	})
}

// emitRunSummary beendet den Lauf. Zählt selbst durch, statt sich in die
// Anzeige-Funktion printSummary einzuklinken: so bleibt die Bildschirmausgabe
// unangetastet, und beide Wege können sich nicht gegenseitig beschädigen.
func emitRunSummary(results []ProcessResult, elapsed time.Duration) {
	if !jsonMode {
		return
	}
	sum := eventSummary{
		Ev:         "summary",
		Files:      len(results),
		ElapsedSec: elapsed.Seconds(),
	}
	for _, r := range results {
		switch resultStatus(r) {
		case "success":
			sum.Success++
			sum.SavedMB += r.SavedMB
		case "skipped":
			sum.Skipped++
		default: // failed und preview zählen beide als nicht erledigt
			sum.Failed++
		}
	}
	emitEvent(sum)
}

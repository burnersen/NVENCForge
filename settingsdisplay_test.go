//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// plainError schneidet den internen Vorspann für die Bildschirmanzeige weg.
// Die Fälle hier sind echte Fehlerketten aus dem Programm.
func TestPlainErrorKeepsTheUsefulPart(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
	}{
		{
			"FFmpeg-Ursache hinter dem Trenner",
			errors.New("Converter.go: runFFmpeg: exit status 1 | Last output: Unknown encoder 'hevc_nvenc'"),
			"Unknown encoder 'hevc_nvenc'",
		},
		{
			"verschachtelte Kette",
			errors.New("Streams.go: writeVideoOnlyMP4: Converter.go: runFFmpeg: exit status 1 | Last output: No space left on device"),
			"No space left on device",
		},
		{
			"Vorspann ohne FFmpeg-Trenner",
			errors.New("Streams.go: probeStreams: JSON parse error: unexpected end of input"),
			"JSON parse error: unexpected end of input",
		},
		{
			"Fehler ganz ohne Vorspann bleibt unangetastet",
			errors.New("The system cannot find the file specified."),
			"The system cannot find the file specified.",
		},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		if got := plainError(c.in); got != c.want {
			t.Errorf("%s: plainError() = %q, want %q", c.name, got, c.want)
		}
	}
}

// Ein Fehler darf durch das Kürzen NIE leer werden — eine leere Fehlerzeile
// wäre schlimmer als eine technische.
func TestPlainErrorNeverSwallowsEverything(t *testing.T) {
	tricky := []error{
		errors.New("Converter.go: runFFmpeg: exit status 1 | Last output: "),
		errors.New("Streams.go: runMerge: "),
		errors.New("main.go: "),
		fmt.Errorf("wrapped: %w", errors.New("Streams.go: probeStreams: timeout")),
	}
	for _, e := range tricky {
		if got := plainError(e); strings.TrimSpace(got) == "" {
			t.Errorf("plainError(%q) returned nothing at all", e)
		}
	}
}

// Hintergrund (2026-08-16): aqStrength war seit seiner Einführung in v1.14.0
// der einzige INI-Schlüssel, der die Dateigröße spürbar bewegt — und tauchte in
// der Einstellungs-Anzeige überhaupt nicht auf. Genau diese Lücke fällt beim
// Lesen des Codes nicht auf, weil die Anzeige eine reine Aufzählung ist.
// Die Tests hier halten deshalb fest: jeder Wert, nach dem man bei der
// Fehlersuche greift, MUSS auf dem Bildschirm stehen.

// captureStdout sammelt ein, was f() nach os.Stdout schreibt. pterm schreibt
// über einen eigenen Writer, der os.Stdout bei jedem Aufruf frisch ausliest —
// deshalb genügt das Umbiegen der Variablen ohne weitere Vorkehrung.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("cannot create pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	f()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}

// settingsScreen rendert die Einstellungs-Anzeige mit den übergebenen
// Einstellungen und stellt den globalen Zustand danach wieder her.
func settingsScreen(t *testing.T, s AppSettings, cfg *AppConfig) string {
	t.Helper()
	origSettings, origCPU, origFFmpeg := appSettings, cpuModeActive, ffmpegSource
	t.Cleanup(func() {
		appSettings, cpuModeActive, ffmpegSource = origSettings, origCPU, origFFmpeg
	})
	appSettings = s
	cpuModeActive = false
	ffmpegSource = "own copy"
	return captureStdout(t, func() { printActiveSettings(cfg) })
}

// Die Größen-Prognose während des Encodes muss am Ende die Wahrheit sagen —
// sie ist die Zahl, an der der Nutzen des Programms abgelesen wird.
func TestSmoothOutputEstimateLandsOnTheTruth(t *testing.T) {
	// Erster Messwert: nichts zu glätten.
	if got := smoothOutputEstimate(0, 42, 7); got != 42 {
		t.Errorf("first value = %.1f, want 42", got)
	}
	// Bei 100 % zählt nur noch der gemessene Wert.
	if got := smoothOutputEstimate(13, 26, 100); got != 26 {
		t.Errorf("at 100%% = %.1f, want exactly the measured 26", got)
	}
	// Früh im Lauf wirkt die Glättung noch stark.
	if got := smoothOutputEstimate(100, 200, 5); got > 120 {
		t.Errorf("at 5%% = %.1f, want a heavily damped value near 100", got)
	}

	// Der eigentliche Fehlerfall: ein kurzer Lauf mit einer zu niedrigen
	// Anfangsschätzung muss den echten Wert einholen, bevor er endet.
	est := 13.0
	for pct := 10.0; pct <= 100; pct += 10 {
		est = smoothOutputEstimate(est, 26, pct)
	}
	if est < 25.9 || est > 26.1 {
		t.Errorf("short run ended at %.1f MB, want ~26 MB", est)
	}
}

func TestSettingsScreenShowsEveryEncoderKnob(t *testing.T) {
	s := defaultAppSettings()
	s.aqStrength = 7
	s.bFrames = 3
	s.nvencPreset = "p4"
	s.nvencLookahead = 24
	s.maxBitrate1080p = 7500
	s.autoCQTargetVMAF = 94
	s.autoCQTolerance = 1.5
	s.casStrength = 0
	s.audioKbpsPerChannel = 80
	s.fallbackAudioBitrate = 160
	s.gpuDecode = true
	s.gpuDecodeMaxMbit = 42

	out := settingsScreen(t, s, &AppConfig{autoCQ: true, maxBitrateKbps: s.maxBitrate1080p})

	// Jeder Eintrag: Beschriftung UND der Wert, der wirklich gefahren wird.
	want := []string{
		"AQ strength", "7",
		"B-frames", "3",
		"NVENC preset", "p4",
		"Lookahead", "24",
		"Max bitrate", "7500",
		"Quality target", "VMAF 94",
		"92.5", // Ziel minus Toleranz = das echte Suchziel
		"Decoding", "42",
		"Sharpening", "off",
		"Audio", "80",
		"Audio fallback", "160",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("settings screen is missing %q\n--- screen ---\n%s", w, out)
		}
	}
}

// Mutationsprobe: eine Anzeige, die einen fest verdrahteten Text ausgibt statt
// des tatsächlichen Wertes, bestünde den Test oben. Hier wird derselbe
// Schlüssel zweimal mit verschiedenen Werten gerendert — der Bildschirm MUSS
// sich unterscheiden.
func TestSettingsScreenFollowsTheActualValue(t *testing.T) {
	low := defaultAppSettings()
	low.aqStrength = 1
	high := defaultAppSettings()
	high.aqStrength = 12

	cfg := &AppConfig{autoCQ: true}
	outLow := settingsScreen(t, low, cfg)
	outHigh := settingsScreen(t, high, cfg)

	if outLow == outHigh {
		t.Fatal("aqStrength 1 and 12 produced the same screen — the value is not really read")
	}
	if !strings.Contains(outHigh, "12") {
		t.Errorf("aqStrength 12 does not appear on screen:\n%s", outHigh)
	}
}

// Im CPU-Modus gibt es weder NVENC-Preset noch AQ-Stärke — sie stünden dort
// als glatte Falschaussage.
func TestSettingsScreenSwapsKnobsInCPUMode(t *testing.T) {
	s := defaultAppSettings()
	s.cpuPreset = "medium"
	s.cpuThreads = 6

	origSettings, origCPU := appSettings, cpuModeActive
	t.Cleanup(func() { appSettings, cpuModeActive = origSettings, origCPU })
	appSettings = s
	cpuModeActive = true

	out := captureStdout(t, func() { printActiveSettings(&AppConfig{autoCQ: true}) })

	for _, w := range []string{"CPU preset", "medium", "Threads", "6"} {
		if !strings.Contains(out, w) {
			t.Errorf("CPU mode screen is missing %q\n--- screen ---\n%s", w, out)
		}
	}
	for _, unwanted := range []string{"NVENC preset", "AQ strength"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("CPU mode screen must not advertise %q (it has no effect there)", unwanted)
		}
	}
}

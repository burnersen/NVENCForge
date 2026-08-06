//go:build windows && amd64

// Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

// withGPUDecodeDefaults stellt die globalen Einstellungen nach jedem Test
// wieder her — appSettings und die beiden Schalter sind paketweit.
func withGPUDecodeDefaults(t *testing.T) {
	t.Helper()
	oldSettings := appSettings
	oldDisabled := gpuDecodeDisabled
	oldCPUMode := cpuModeActive
	t.Cleanup(func() {
		appSettings = oldSettings
		gpuDecodeDisabled = oldDisabled
		cpuModeActive = oldCPUMode
	})
	appSettings = defaultAppSettings()
	gpuDecodeDisabled = false
	cpuModeActive = false
}

// Die Bitratengrenze ist der eigentliche Absturzschutz: ein Treiberabsturz
// (TDR) lässt sich nachträglich nicht auffangen, er muss vorher vermieden
// werden. Deshalb wird hier scharf geprüft, dass im Zweifel IMMER der
// Prozessor-Weg gewinnt.
func TestGPUDecodeArgsSafetyGate(t *testing.T) {
	const mbit = 1_000_000

	cases := []struct {
		name    string
		prepare func()
		stats   *VideoStats
		wantGPU bool
	}{
		{
			name:    "normale 4K-Quelle wird beschleunigt",
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 15 * mbit},
			wantGPU: true,
		},
		{
			name:    "genau auf der Grenze ist noch erlaubt",
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: gpuDecodeDefaultMaxMbit * mbit},
			wantGPU: true,
		},
		{
			name:    "ein Bit ueber der Grenze nicht mehr",
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: gpuDecodeDefaultMaxMbit*mbit + 1},
			wantGPU: false,
		},
		{
			name:    "der bekannte Absturzfall mit 400 Mbit bleibt auf der CPU",
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 400 * mbit},
			wantGPU: false,
		},
		{
			name:    "unbekannte Bitrate ist nicht pruefbar, also CPU",
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 0},
			wantGPU: false,
		},
		{
			name:    "negative Bitrate (kaputte Datei) ebenfalls CPU",
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: -1},
			wantGPU: false,
		},
		{
			name:    "fremder Codec bleibt auf der CPU",
			stats:   &VideoStats{VideoCodec: "mpeg2video", BitrateBps: 5 * mbit},
			wantGPU: false,
		},
		{
			name:    "Grossschreibung im Codecnamen wird erkannt",
			stats:   &VideoStats{VideoCodec: "HEVC", BitrateBps: 15 * mbit},
			wantGPU: true,
		},
		{
			name:    "fehlende Videodaten fuehren nicht zum Absturz",
			stats:   nil,
			wantGPU: false,
		},
		{
			name:    "per INI abgeschaltet",
			prepare: func() { appSettings.gpuDecode = false },
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 15 * mbit},
			wantGPU: false,
		},
		{
			name:    "nach einem Fehlschlag fuer den Rest des Laufs aus",
			prepare: func() { gpuDecodeDisabled = true },
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 15 * mbit},
			wantGPU: false,
		},
		{
			name:    "im CPU-Modus gibt es keine Grafikkarte zu nutzen",
			prepare: func() { cpuModeActive = true },
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 15 * mbit},
			wantGPU: false,
		},
		{
			name:    "eigene niedrigere Grenze aus der INI wird beachtet",
			prepare: func() { appSettings.gpuDecodeMaxMbit = 10 },
			stats:   &VideoStats{VideoCodec: "hevc", BitrateBps: 15 * mbit},
			wantGPU: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withGPUDecodeDefaults(t)
			if c.prepare != nil {
				c.prepare()
			}
			got := gpuDecodeArgs(c.stats)
			if c.wantGPU {
				if strings.Join(got, " ") != "-hwaccel cuda" {
					t.Errorf("Grafikkarte erwartet, bekam %q", got)
				}
				return
			}
			if got != nil {
				t.Errorf("Prozessor-Weg erwartet, bekam %q", got)
			}
		})
	}
}

// "-hwaccel" gilt in FFmpeg immer nur für die unmittelbar folgende Eingabe.
// Bei drei Messfenstern muss es deshalb dreimal auftauchen — steht es nur
// einmal vorne, wird stillschweigend nur das erste Fenster beschleunigt.
func TestAutoCQWindowInputsRepeatsHwaccel(t *testing.T) {
	windows := [][2]float64{{36, 8}, {84, 8}, {132, 8}}
	hw := []string{"-hwaccel", "cuda"}

	args := autoCQWindowInputs("C:\\videos\\in.mp4", windows, hw)
	s := strings.Join(args, " ")

	if got := strings.Count(s, "-hwaccel cuda"); got != len(windows) {
		t.Errorf("%d mal -hwaccel, erwartet %d\n%s", got, len(windows), s)
	}
	if got := strings.Count(s, "-i "); got != len(windows) {
		t.Errorf("%d Eingaben, erwartet %d", got, len(windows))
	}
	// Jedes -hwaccel muss VOR seinem -i stehen, sonst wirkt es nicht.
	for _, teil := range strings.Split(s, "-i ")[:len(windows)] {
		if !strings.Contains(teil, "-hwaccel cuda") {
			t.Errorf("Eingabe ohne vorangestelltes -hwaccel: %q", teil)
		}
	}

	// Ohne Grafikkarte darf kein einziges Argument dazukommen.
	ohne := strings.Join(autoCQWindowInputs("C:\\videos\\in.mp4", windows, nil), " ")
	if strings.Contains(ohne, "hwaccel") {
		t.Errorf("ohne Beschleunigung darf kein -hwaccel erscheinen: %s", ohne)
	}
}

// casStrength=0 soll das Nachschärfen komplett aus der Filterkette nehmen,
// nicht mit Stärke 0 mitlaufen lassen (der Filter würde sonst jedes Bild
// anfassen, ohne etwas zu ändern — gemessen rund 8 s je 90 s Video).
func TestBuildVideoFilterCASOff(t *testing.T) {
	withGPUDecodeDefaults(t)

	appSettings.casStrength = 0
	aus := buildVideoFilter(true, false, false)
	if strings.Contains(aus, "cas") {
		t.Errorf("casStrength=0 darf keinen cas-Filter erzeugen: %s", aus)
	}
	for _, want := range []string{"scale=", "format=p010le"} {
		if !strings.Contains(aus, want) {
			t.Errorf("Filterkette ohne %q: %s", want, aus)
		}
	}
	if strings.Contains(aus, ",,") {
		t.Errorf("Filterkette hat ein leeres Glied: %s", aus)
	}

	appSettings.casStrength = 0.4
	an := buildVideoFilter(true, false, false)
	if !strings.Contains(an, "cas=strength=0.4") {
		t.Errorf("casStrength=0.4 fehlt in der Kette: %s", an)
	}

	// Ohne Skalierung gab es noch nie ein Nachschärfen — das bleibt so.
	if got := buildVideoFilter(false, false, false); strings.Contains(got, "cas") {
		t.Errorf("ohne Skalierung darf kein cas auftauchen: %s", got)
	}
}

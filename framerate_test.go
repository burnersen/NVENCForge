//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import "testing"

// TestPickFrameRate locks in the 1.19.0 fix: r_frame_rate is only the timebase
// denominator, so a variable-frame-rate file can claim 300 fps while really
// running at 60. Auto-CQ then measured five times the frames, ran into the
// bitrate cap and picked a CQ that was too soft. The decisive case is
// "VFR: timebase claims 300, average says 60"; the CFR cases are the
// regression guard — for clean sources both values match and nothing changes.
func TestPickFrameRate(t *testing.T) {
	cases := []struct {
		name     string
		avgRate  string
		realRate string
		wantNum  int
		wantDen  int
		wantNote bool
	}{
		{"VFR: timebase claims 300, average says 60",
			"20833200/347221", "300/1", 20833200, 347221, true},
		{"CFR 60: both values agree", "60/1", "60/1", 60, 1, false},
		{"CFR 30000/1001: both values agree", "30000/1001", "30000/1001", 30000, 1001, false},
		{"average missing: fall back to the timebase", "0/0", "25/1", 25, 1, false},
		{"average unparsable: fall back to the timebase", "N/A", "25/1", 25, 1, false},
		{"timebase missing: the average alone is enough", "50/1", "0/0", 50, 1, false},
		{"nothing usable: frame rate stays unknown", "0/0", "0/0", 0, 0, false},
		{"high frame rate, both agree: used, but flagged", "240/1", "240/1", 240, 1, true},
		// Ein kleiner Unterschied ist keine kaputte Zeitbasis: dort bleibt es
		// beim bisherigen Wert, damit heute korrekt laufende Dateien
		// unverändert bleiben.
		{"slight difference: timebase stays in charge", "59/1", "60/1", 60, 1, false},
		// Löcher in der Zeitachse drücken den Durchschnitt ebenfalls, ohne dass
		// die Zeitbasis falsch wäre — deshalb muss die Zeitbasis zusätzlich
		// unrealistisch hoch sein, bevor umgestellt wird.
		{"holes in the timeline: timebase stays in charge", "15/1", "30/1", 30, 1, false},
		{"real 1663939946/56143267 file: unchanged at 29.97",
			"1663939946/56143267", "30000/1001", 30000, 1001, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			num, den, note := pickFrameRate(c.avgRate, c.realRate)
			if num != c.wantNum || den != c.wantDen {
				t.Errorf("pickFrameRate(%q, %q) = %d/%d, want %d/%d",
					c.avgRate, c.realRate, num, den, c.wantNum, c.wantDen)
			}
			if (note != "") != c.wantNote {
				t.Errorf("pickFrameRate(%q, %q) note = %q, want note: %v",
					c.avgRate, c.realRate, note, c.wantNote)
			}
		})
	}
}

// TestCalcGOPFollowsFrameRate proves point 3 of the analysis: with the real
// frame rate the keyframe distance lands at four seconds again. The 300 fps
// artefact pushed it into the 600 clamp, i.e. two seconds of a 60 fps timeline.
// The range instead of a fixed 240 is on purpose: an average frame rate is a
// raw fraction (59.9998 fps here), and calcGOP truncates — 239 is correct.
func TestCalcGOPFollowsFrameRate(t *testing.T) {
	realNum, realDen, _ := pickFrameRate("20833200/347221", "300/1")
	if got := calcGOP(realNum, realDen); got < 236 || got > 244 {
		t.Errorf("calcGOP(%d/%d) = %d, want about 240 (4 s at 60 fps)", realNum, realDen, got)
	}
	if got := calcGOP(300, 1); got != 600 {
		t.Errorf("calcGOP(300/1) = %d, want 600 (the old, clamped value)", got)
	}
}

// TestFrameRateFromPacketCount sichert den Ausweg für Dateien ab, bei denen
// ffprobe gar keine glaubwürdige Bildrate meldet (alte WMV: r_frame_rate
// 1000/1, avg_frame_rate 0/0). Der entscheidende Fall ist der nachgestellte:
// 200 Bilder in 8,003 Sekunden sind 25 fps, nicht 1000.
func TestFrameRateFromPacketCount(t *testing.T) {
	cases := []struct {
		name     string
		packets  int
		duration float64
		wantNum  int
		wantDen  int
		wantOK   bool
	}{
		{"the measured WMV: 200 frames in 8.003 s", 200, 8.003, 25, 1, true},
		{"NTSC film snaps to 24000/1001", 143856, 6000, 24000, 1001, true},
		{"24 fps is not pulled over to 23.976", 144000, 6000, 24, 1, true},
		{"29.97 snaps to 30000/1001", 179820, 6000, 30000, 1001, true},
		{"an odd rate stays what it is", 1250, 100, 12500, 1000, true},
		// Echtes Hochgeschwindigkeitsmaterial: das Zählen bestätigt die hohe
		// Rate, sie darf dann nicht durch etwas anderes ersetzt werden.
		{"real 240 fps is no replacement", 24000, 100, 0, 0, false},
		{"slide show below 1 fps is no replacement", 5, 100, 0, 0, false},
		{"no frames counted", 0, 100, 0, 0, false},
		{"duration unknown", 200, 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			num, den, ok := frameRateFromPacketCount(c.packets, c.duration)
			if num != c.wantNum || den != c.wantDen || ok != c.wantOK {
				t.Errorf("frameRateFromPacketCount(%d, %v) = %d/%d ok=%v, want %d/%d ok=%v",
					c.packets, c.duration, num, den, ok, c.wantNum, c.wantDen, c.wantOK)
			}
		})
	}
}

// TestCountedRateReachesTheEncoder: Die nachgezählte Bildrate nützt nichts,
// wenn sie nur im Protokoll steht. FFmpeg nähme ohne "-r" weiter die Angabe
// des Containers und füllte auf 1000 Bilder pro Sekunde auf.
func TestCountedRateReachesTheEncoder(t *testing.T) {
	hasRate := func(args []string, rate string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-r" && args[i+1] == rate {
				return true
			}
		}
		return false
	}
	job := convJob{inputPath: "in.wmv", outputPath: "out.mkv", outputRate: "25/1"}
	if !hasRate(job.buildConvertArgs(), "25/1") {
		t.Error("die nachgezählte Bildrate fehlt in der Befehlszeile des Encoders")
	}
	// Der Normalfall darf sich nicht ändern: ohne Nachzählen kein "-r".
	plain := convJob{inputPath: "in.mkv", outputPath: "out.mkv"}
	for _, arg := range plain.buildConvertArgs() {
		if arg == "-r" {
			t.Error("ohne nachgezählte Bildrate darf kein -r in der Befehlszeile stehen")
		}
	}
}

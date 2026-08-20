//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

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

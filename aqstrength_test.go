//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseOneSetting writes a one-line INI into a temporary folder and parses it,
// so each case starts from the shipped defaults and cannot affect the next.
func parseOneSetting(t *testing.T, line string) (AppSettings, []invalidSetting) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
	if err := os.WriteFile(path, []byte(line+"\r\n"), 0644); err != nil {
		t.Fatalf("cannot write test config: %v", err)
	}
	s, invalids, _ := parseAppConfig(path)
	return s, invalids
}

// Background for all tests in this file (measured 2026-08-15, four real
// sources at a fixed CQ): -aq-strength used to be hard-wired to 8, which was
// too aggressive — it pushed bits into busy areas that did not need them.
// At 2, together with one more B-frame, files came out 8-28% smaller at the
// same quality and the same encode time. Both numbers are now settings, so
// these tests pin that they really reach the encoder.

// TestAQStrengthReachesEncoders guards the actual point of the change: the
// value from the INI has to end up in the FFmpeg call. A hard-wired number
// here would silently ignore the user's setting.
func TestAQStrengthReachesEncoders(t *testing.T) {
	prev := appSettings
	defer func() { appSettings = prev }()

	appSettings.aqStrength = 5
	appSettings.bFrames = 3
	appSettings.nvencPreset = "p5"
	appSettings.nvencLookahead = 32
	nvencAdvancedAQ = true

	hevc := strings.Join(buildNVENCOptsWithCQ(30, "8000k", "16000k", 240), " ")
	if !strings.Contains(hevc, "-aq-strength 5") {
		t.Errorf("H.265 must pass the configured AQ strength\n%s", hevc)
	}
	if strings.Contains(hevc, "-aq-strength 8") {
		t.Errorf("H.265 still carries the old hard-wired 8\n%s", hevc)
	}

	av1 := strings.Join(buildAV1OptsWithCQ(32, "6000k", "12000k", 240), " ")
	if !strings.Contains(av1, "-aq-strength 5") {
		t.Errorf("AV1 must pass the configured AQ strength\n%s", av1)
	}
	if strings.Contains(av1, "-aq-strength 8") {
		t.Errorf("AV1 still carries the old hard-wired 8\n%s", av1)
	}
}

// TestAQStrengthAndBFrameDefaults pins the measured defaults. Anyone changing
// them should have to change this test too — and read why they are what they
// are.
func TestAQStrengthAndBFrameDefaults(t *testing.T) {
	d := defaultAppSettings()
	if d.aqStrength != 2 {
		t.Errorf("default aqStrength = %d, want 2 (measured 2026-08-15)", d.aqStrength)
	}
	if d.bFrames != 5 {
		t.Errorf("default bFrames = %d, want 5 (measured 2026-08-15)", d.bFrames)
	}
}

// TestAQStrengthConfigRange checks the INI validation. 0 must be rejected:
// FFmpeg's own range is 1-15, and spatial AQ is switched off with
// -spatial-aq, not with a strength of zero.
func TestAQStrengthConfigRange(t *testing.T) {
	cases := []struct {
		val     string
		wantOK  bool
		wantNum int
	}{
		{"1", true, 1},
		{"2", true, 2},
		{"8", true, 8},
		{"15", true, 15},
		{"0", false, 0},
		{"16", false, 0},
		{"-3", false, 0},
		{"zwei", false, 0},
	}
	for _, c := range cases {
		s, invalids := parseOneSetting(t, "aqStrength="+c.val)
		if c.wantOK {
			if len(invalids) != 0 {
				t.Errorf("aqStrength=%s should be accepted, got %v", c.val, invalids)
			}
			if s.aqStrength != c.wantNum {
				t.Errorf("aqStrength=%s parsed to %d, want %d", c.val, s.aqStrength, c.wantNum)
			}
			continue
		}
		if len(invalids) == 0 {
			t.Errorf("aqStrength=%s should be rejected", c.val)
		}
		if s.aqStrength != defaultAppSettings().aqStrength {
			t.Errorf("rejected aqStrength=%s must leave the default in place, got %d", c.val, s.aqStrength)
		}
	}
}

// TestBFrameRangeAcceptsFive is the regression guard for the range widening:
// before this change the INI capped bFrames at 4, so the measured 5 would
// have been thrown out as invalid and silently reset.
func TestBFrameRangeAcceptsFive(t *testing.T) {
	s, invalids := parseOneSetting(t, "bFrames=5")
	if len(invalids) != 0 {
		t.Fatalf("bFrames=5 must be accepted, got %v", invalids)
	}
	if s.bFrames != 5 {
		t.Errorf("bFrames=5 parsed to %d", s.bFrames)
	}
	if _, invalids := parseOneSetting(t, "bFrames=6"); len(invalids) == 0 {
		t.Error("bFrames=6 must still be rejected (NVENC HEVC allows at most 5)")
	}
}

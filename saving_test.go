//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The numbers throughout this file are the measured ones from 2026-08-28
// (SZ1978, 1080p50, source 12004 kbit/s): the CQ 26 sample costs 6625 kbit/s
// and the CQ 30 sample 3690, with CQ 28 measured at 5008 in between.
const (
	testSourceKbps  = 12004
	testAnchorLow   = 6625
	testAnchorHigh  = 3690
	testMiddleKbps  = 5008
	testSampleSec   = 24
	testWindowedLen = 8
)

// writeSampleFile creates a stand-in for an Auto-CQ sample encode, sized so it
// yields exactly the wanted bitrate over sampleSec seconds.
func writeSampleFile(t *testing.T, dir string, cq int, kbps, sampleSec float64) {
	t.Helper()
	size := int64(kbps * 1000 / 8 * sampleSec)
	path := filepath.Join(dir, fmt.Sprintf("sample_cq%d.mkv", cq))
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("cannot write %s: %v", path, err)
	}
}

// TestAutoCQSampleKbps checks the size-to-bitrate conversion and its guards.
func TestAutoCQSampleKbps(t *testing.T) {
	dir := t.TempDir()
	writeSampleFile(t, dir, 26, testAnchorLow, testSampleSec)

	got, err := autoCQSampleKbps(dir, 26, testSampleSec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(got-testAnchorLow) > 1 {
		t.Errorf("got %.1f kbit/s, want %d", got, testAnchorLow)
	}
	if _, err := autoCQSampleKbps(dir, 26, 0); err == nil {
		t.Error("a zero sample length must be an error, not a division by zero")
	}
	if _, err := autoCQSampleKbps(dir, 99, testSampleSec); err == nil {
		t.Error("a missing sample file must be an error")
	}
}

// TestAutoCQWindowSourceKbps covers the weighting. The saving prediction has
// to read the source rate at the sampled seconds, not the whole-file average:
// guided placement puts the windows on the heaviest scenes on purpose, and
// judging those against the file average would get the share wrong.
func TestAutoCQWindowSourceKbps(t *testing.T) {
	buckets := []bitrateBucket{
		{startSec: 0, kbps: 1000},
		{startSec: 8, kbps: 2000},
		{startSec: 16, kbps: 3000},
		{startSec: 24, kbps: 4000},
	}
	cases := []struct {
		name    string
		buckets []bitrateBucket
		windows [][2]float64
		want    float64
	}{
		{"window on one bucket", buckets, [][2]float64{{16, 8}}, 3000},
		{"heaviest bucket only", buckets, [][2]float64{{24, 8}}, 4000},
		{"two windows averaged", buckets, [][2]float64{{0, 8}, {24, 8}}, 2500},
		{"window straddling two buckets", buckets, [][2]float64{{4, 8}}, 1500},
		{"quarter into the next bucket", buckets, [][2]float64{{6, 8}}, 1750},
		{"window past the profile", buckets, [][2]float64{{400, 8}}, 0},
		{"no buckets", nil, [][2]float64{{0, 8}}, 0},
		{"no windows", buckets, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := autoCQWindowSourceKbps(c.buckets, c.windows, testWindowedLen)
			if math.Abs(got-c.want) > 0.001 {
				t.Errorf("got %.3f, want %.3f", got, c.want)
			}
		})
	}
}

// TestAutoCQBitrateModel holds the exponential model against the measurement
// it was built from: it must reproduce both anchors exactly and land close to
// the independently measured point between them.
func TestAutoCQBitrateModel(t *testing.T) {
	sc := hevcAutoCQScale
	rate := autoCQBitrateRate(sc, testAnchorLow, testAnchorHigh)
	if rate <= 0 {
		t.Fatalf("rate must be positive on a falling curve, got %.4f", rate)
	}
	if got := autoCQEstimateKbps(sc, testAnchorLow, rate, sc.anchorLow); math.Abs(got-testAnchorLow) > 0.5 {
		t.Errorf("low anchor: got %.1f, want %d", got, testAnchorLow)
	}
	if got := autoCQEstimateKbps(sc, testAnchorLow, rate, sc.anchorHigh); math.Abs(got-testAnchorHigh) > 0.5 {
		t.Errorf("high anchor: got %.1f, want %d", got, testAnchorHigh)
	}
	got := autoCQEstimateKbps(sc, testAnchorLow, rate, 28)
	if math.Abs(got-testMiddleKbps)/testMiddleKbps > 0.03 {
		t.Errorf("CQ 28: got %.1f, measured %d — model off by more than 3%%", got, testMiddleKbps)
	}
}

// TestAutoCQBitrateRateRejectsBadCurves makes sure an anchor pair that does not
// fall (measurement noise on flat material) yields no model at all, instead of
// an extrapolation built on nonsense.
func TestAutoCQBitrateRateRejectsBadCurves(t *testing.T) {
	sc := hevcAutoCQScale
	cases := []struct {
		name              string
		kbpsLow, kbpsHigh float64
	}{
		{"rising", 3000, 4000},
		{"identical", 4000, 4000},
		{"zero low", 0, 3000},
		{"zero high", 6000, 0},
		{"negative", -1, 3000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := autoCQBitrateRate(sc, c.kbpsLow, c.kbpsHigh); got != 0 {
				t.Errorf("got %.4f, want 0", got)
			}
		})
	}
}

// TestAutoCQExpectedSavingPercent: only the picture is re-encoded, sound and
// subtitles are counted unchanged (CloudForge's formula).
func TestAutoCQExpectedSavingPercent(t *testing.T) {
	const durationSec = 1000.0
	// 1000 s at 8000 kbit/s picture = 1 000 000 000 bytes; the file holds
	// another 100 000 000 bytes of sound.
	videoBytes := 8000.0 * 1000 / 8 * durationSec
	fileMB := (videoBytes + 100e6) / 1048576

	cases := []struct {
		name     string
		fileMB   float64
		videoKb  float64
		share    float64
		wantPct  float64
		wantKnow bool
	}{
		// Picture halved: 500 MB saved of 1100 MB.
		{"picture halved", fileMB, 8000, 0.5, 500e6 / 1100e6 * 100, true},
		// Picture as big as before: nothing saved.
		{"no gain", fileMB, 8000, 1.0, 0, true},
		// Picture grows: the prediction turns negative — the caller remuxes.
		{"picture grows", fileMB, 8000, 1.2, -200e6 / 1100e6 * 100, true},
		// Video rate above the whole file (a broken header): everything counts
		// as picture instead of predicting from nonsense.
		{"split unknown", 100, 1e9, 0.5, 50, true},
		{"no share", fileMB, 8000, 0, 0, false},
		{"no file size", 0, 8000, 0.5, 0, false},
	}
	for _, c := range cases {
		got, known := autoCQExpectedSavingPercent(c.fileMB, c.videoKb, durationSec, c.share)
		if known != c.wantKnow || math.Abs(got-c.wantPct) > 1e-6 {
			t.Errorf("%s: got %.4f%% known=%v, want %.4f%% known=%v",
				c.name, got, known, c.wantPct, c.wantKnow)
		}
	}
}

// TestAutoCQSavingText: a re-encode that would grow must read as "larger",
// never as "-21% smaller".
func TestAutoCQSavingText(t *testing.T) {
	for pct, want := range map[float64]string{
		23:    "about 23% smaller",
		0:     "about 0% smaller",
		-21.4: "about 21% larger than the source",
	} {
		if got := autoCQSavingText(pct); got != want {
			t.Errorf("autoCQSavingText(%.1f) = %q, want %q", pct, got, want)
		}
	}
}

// TestAutoCQSizeProbeNeeded: the size probe only runs where the first
// prediction's error (up to 13 points measured) could flip the decision.
func TestAutoCQSizeProbeNeeded(t *testing.T) {
	cases := []struct {
		expected, minimum float64
		want              bool
	}{
		{20, 20, true},
		{35, 20, true},  // right at the edge of the band
		{5, 20, true},   // the other edge
		{36, 20, false}, // clearly worth it
		{4, 20, false},  // clearly not worth it
		{60, 20, false}, // the usual H.264 source
		{-10, 0, true},  // "never larger" still probes near zero
	}
	for _, c := range cases {
		if got := autoCQSizeProbeNeeded(c.expected, c.minimum); got != c.want {
			t.Errorf("expected %.0f%% vs minimum %.0f%%: probe=%v, want %v",
				c.expected, c.minimum, got, c.want)
		}
	}
}

// TestAutoCQSizeProbeWindows: ten stretches, evenly over the WHOLE film and
// centred in their sections — intro and credits included, never past the end.
func TestAutoCQSizeProbeWindows(t *testing.T) {
	windows := autoCQSizeProbeWindows(1000, 8, 10)
	if len(windows) != 10 {
		t.Fatalf("got %d windows, want 10", len(windows))
	}
	for i, w := range windows {
		wantStart := 1000*(float64(i)+0.5)/10 - 4
		if math.Abs(w[0]-wantStart) > 1e-9 || w[1] != 8 {
			t.Errorf("window %d = %v, want start %.1f length 8", i, w, wantStart)
		}
	}

	// A film shorter than the stretches together: every window stays inside it.
	for _, w := range autoCQSizeProbeWindows(50, 8, 10) {
		if w[0] < 0 || w[0]+w[1] > 50+1e-9 {
			t.Errorf("window %v leaves a 50 s film", w)
		}
	}
	if autoCQSizeProbeWindows(0, 8, 10) != nil || autoCQSizeProbeWindows(100, 8, 0) != nil {
		t.Error("an unknown duration or no spots must yield no windows")
	}
}

// TestMinSavePercentParsing covers the value check of the key that replaced
// the cost cap in 2.0.0.
func TestMinSavePercentParsing(t *testing.T) {
	cases := []struct {
		val   string
		want  float64
		valid bool
	}{
		{"0", 0, true},
		{"20", 20, true},
		{"12.5", 12.5, true},
		{"90", 90, true},
		{"91", 0, false},
		{"-1", 0, false},
		{"twenty", 0, false},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
			if err := os.WriteFile(path, []byte("minSavePercent="+c.val+"\n"), 0o644); err != nil {
				t.Fatalf("cannot write config: %v", err)
			}
			parsed, invalids, _ := parseAppConfig(path)
			if c.valid {
				if len(invalids) != 0 {
					t.Fatalf("%q was rejected but should be allowed", c.val)
				}
				if parsed.minSavePercent != c.want {
					t.Errorf("got %.4g, want %.4g", parsed.minSavePercent, c.want)
				}
				return
			}
			if len(invalids) == 0 {
				t.Fatalf("%q was accepted but must be rejected", c.val)
			}
			if parsed.minSavePercent != defaultAppSettings().minSavePercent {
				t.Errorf("a rejected value must leave the default in place, got %.4g",
					parsed.minSavePercent)
			}
		})
	}
	if got := defaultAppSettings().minSavePercent; got != 20 {
		t.Errorf("default minSavePercent = %.4g, want 20 (the user's choice from CloudForge)", got)
	}
}

// TestEncodeSavesEnough: a finished re-encode is kept only when it saves the
// minimum; at 0 only "smaller at all" counts, as before 2.0.0.
func TestEncodeSavesEnough(t *testing.T) {
	cases := []struct {
		name           string
		source, result float64
		minimum        float64
		want           bool
	}{
		{"saves 50 %", 1000, 500, 20, true},
		{"saves exactly the minimum", 1000, 800, 20, true},
		{"saves only 15 %", 1000, 850, 20, false},
		{"grows", 1000, 1010, 20, false},
		{"minimum 0 keeps any saving", 1000, 999, 0, true},
		{"minimum 0 still rejects equal size", 1000, 1000, 0, false},
		{"unknown source size never passes", 0, 500, 0, false},
	}
	for _, c := range cases {
		if got := encodeSavesEnough(c.source, c.result, c.minimum); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMinSaveFor: a downscaled file only has to get smaller — the smaller
// picture is the user's explicit maxResolution wish.
func TestMinSaveFor(t *testing.T) {
	old := appSettings
	defer func() { appSettings = old }()
	appSettings.minSavePercent = 20
	if got := minSaveFor(false); got != 20 {
		t.Errorf("resolution kept: minimum %.4g, want 20", got)
	}
	if got := minSaveFor(true); got != 0 {
		t.Errorf("scaled down: minimum %.4g, want 0 (only never larger)", got)
	}
}

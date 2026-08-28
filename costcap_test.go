//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

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

// TestAutoCQWindowSourceKbps covers the weighting. The cap has to read the
// source rate at the sampled seconds, not the whole-file average: guided
// placement puts the windows on the heaviest scenes on purpose, and judging
// those against the file average would fire the cap on cheap material.
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

// TestAutoCQCostCapTarget drives the cap through its real inputs — sample
// files on disk plus a bitrate profile — so it covers the wiring, not just
// the arithmetic.
func TestAutoCQCostCapTarget(t *testing.T) {
	sc := hevcAutoCQScale
	windows := [][2]float64{{100, 8}, {200, 8}, {300, 8}}
	buckets := []bitrateBucket{
		{startSec: 96, kbps: testSourceKbps},
		{startSec: 104, kbps: testSourceKbps},
		{startSec: 200, kbps: testSourceKbps},
		{startSec: 296, kbps: testSourceKbps},
		{startSec: 304, kbps: testSourceKbps},
	}
	// A directory holding both anchor samples, as the search leaves it behind.
	newDir := func() string {
		dir := t.TempDir()
		writeSampleFile(t, dir, sc.anchorLow, testAnchorLow, testSampleSec)
		writeSampleFile(t, dir, sc.anchorHigh, testAnchorHigh, testSampleSec)
		return dir
	}

	t.Run("off by default", func(t *testing.T) {
		budget, err := autoCQCostCapTarget(sc, newDir(), buckets, windows, testSampleSec, 0, 26)
		if err == nil {
			t.Fatal("a disabled cap must say why it did nothing")
		}
		if budget.fires(26) {
			t.Error("a disabled cap must never move the pick")
		}
	})

	t.Run("pick already fits", func(t *testing.T) {
		// CQ 30 costs 3690 = 31 % of the source — inside a 40 % budget.
		budget, err := autoCQCostCapTarget(sc, newDir(), buckets, windows, testSampleSec, 40, 30)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if budget.fires(30) {
			t.Errorf("cap fired on a pick that fits, moved to CQ %d", budget.pick)
		}
	})

	t.Run("cap moves an expensive pick", func(t *testing.T) {
		budget, err := autoCQCostCapTarget(sc, newDir(), buckets, windows, testSampleSec, 40, 26)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !budget.fires(26) {
			t.Fatal("cap must fire: CQ 26 costs 55 % of the source")
		}
		// 12004 * 0.40 = 4802 kbit/s, which the model reaches just below CQ 29.
		if budget.pick != 29 {
			t.Errorf("got CQ %d, want CQ 29", budget.pick)
		}
		if share := budget.sharePct(budget.pickKbps); math.Abs(share-55.2) > 1 {
			t.Errorf("share of the incoming pick: got %.1f %%, want about 55 %%", share)
		}
	})

	t.Run("cap only saves, never spends", func(t *testing.T) {
		budget, err := autoCQCostCapTarget(sc, newDir(), buckets, windows, testSampleSec, 40, 33)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if budget.pick != 33 {
			t.Errorf("got CQ %d, want the thriftier pick left at 33", budget.pick)
		}
	})

	t.Run("an unreachable cap keeps out of it", func(t *testing.T) {
		// 10 % of the source is out of reach even at the clamp ceiling on this
		// curve. Clamping there would give the worst picture the scale allows
		// AND still miss the cap, so the cap has to stay out altogether.
		budget, err := autoCQCostCapTarget(sc, newDir(), buckets, windows, testSampleSec, 10, 26)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !budget.unreachable {
			t.Error("cap must report itself unreachable")
		}
		if budget.fires(26) {
			t.Errorf("an unreachable cap must not move the pick, got CQ %d", budget.pick)
		}
		if budget.thriftiestKbps <= budget.capKbps {
			t.Errorf("thriftiest estimate %.0f should exceed the cap %.0f",
				budget.thriftiestKbps, budget.capKbps)
		}
	})

	t.Run("never runs past the clamp ceiling", func(t *testing.T) {
		// A cap met exactly AT the clamp ceiling is the one case where the
		// rounding up could still push the pick past the end of the scale.
		dir := t.TempDir()
		writeSampleFile(t, dir, sc.anchorLow, 6625, testSampleSec)
		writeSampleFile(t, dir, sc.anchorHigh, 3690, testSampleSec)
		// CQ 34 costs about 2056 kbit/s on this curve, i.e. 17.1 % of 12004.
		budget, err := autoCQCostCapTarget(sc, dir, buckets, windows, testSampleSec, 17.2, 26)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if budget.unreachable {
			t.Fatal("a cap the clamp ceiling meets must count as reachable")
		}
		if budget.pick > sc.clampMax {
			t.Errorf("got CQ %d, must never exceed the clamp ceiling %d", budget.pick, sc.clampMax)
		}
	})

	t.Run("no profile means no cap", func(t *testing.T) {
		budget, err := autoCQCostCapTarget(sc, newDir(), nil, windows, testSampleSec, 40, 26)
		if err == nil {
			t.Fatal("a missing bitrate profile must be reported, not guessed around")
		}
		if budget.fires(26) {
			t.Error("without a profile the quality pick has to stand")
		}
	})

	t.Run("missing anchor sample is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeSampleFile(t, dir, sc.anchorLow, testAnchorLow, testSampleSec)
		if _, err := autoCQCostCapTarget(sc, dir, buckets, windows, testSampleSec, 40, 26); err == nil {
			t.Fatal("a missing anchor sample must be reported")
		}
	})

	t.Run("flat curve yields no cap", func(t *testing.T) {
		// Both anchors the same size: no usable model, so the pick stands.
		dir := t.TempDir()
		writeSampleFile(t, dir, sc.anchorLow, testAnchorLow, testSampleSec)
		writeSampleFile(t, dir, sc.anchorHigh, testAnchorLow, testSampleSec)
		budget, err := autoCQCostCapTarget(sc, dir, buckets, windows, testSampleSec, 40, 26)
		if err == nil {
			t.Fatal("a curve that does not fall must be reported")
		}
		if budget.fires(26) {
			t.Error("without a model the pick must stay put")
		}
	})
}

// TestAutoCQCostCapAV1Scale guards the cap on the wider AV1 scale: the same
// budget has to land on a different CQ there, and still inside its clamp range.
// TestAutoCQCostCapAgainstRealLibrary is the regression that matters most: it
// replays four sources measured on 2026-08-28 from a real library, at the same
// 35 % cap, and pins down WHICH of them the cap may touch.
//
// The two thin sources are the reason the reachability check exists. Already
// compressed hard, they need a HIGHER share of their own bitrate than the fat
// ones — one of them sits at 53 % from CQ 26 through CQ 30 and would still be
// near 52 % at the clamp ceiling. Capping those clamps the picture to the worst
// the scale allows and misses the cap anyway. The fat sources drop below 20 %
// over the same span, so there the cap does real work.
func TestAutoCQCostCapAgainstRealLibrary(t *testing.T) {
	sc := hevcAutoCQScale
	windows := [][2]float64{{100, 8}}
	cases := []struct {
		name              string
		sourceKbps        float64
		kbpsLow, kbpsHigh float64 // measured samples at CQ 26 and CQ 30
		wantUnreachable   bool
		wantFires         bool
	}{
		{"thin 60 fps at 2.8 Mbit", 2819, 1498, 1485, true, false},
		{"thin 30 fps at 3.7 Mbit", 3667, 1831, 1686, true, false},
		{"fat 25 fps at 12 Mbit", 11969, 4230, 2393, false, true},
		{"fat 50 fps at 11 Mbit", 11006, 6625, 3690, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSampleFile(t, dir, sc.anchorLow, c.kbpsLow, testSampleSec)
			writeSampleFile(t, dir, sc.anchorHigh, c.kbpsHigh, testSampleSec)
			buckets := []bitrateBucket{
				{startSec: 96, kbps: c.sourceKbps},
				{startSec: 104, kbps: c.sourceKbps},
			}
			budget, err := autoCQCostCapTarget(sc, dir, buckets, windows, testSampleSec, 35, sc.anchorLow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if budget.unreachable != c.wantUnreachable {
				t.Errorf("unreachable: got %v, want %v (thriftiest %.0f vs cap %.0f)",
					budget.unreachable, c.wantUnreachable, budget.thriftiestKbps, budget.capKbps)
			}
			if got := budget.fires(sc.anchorLow); got != c.wantFires {
				t.Errorf("fires: got %v, want %v (pick CQ %d)", got, c.wantFires, budget.pick)
			}
			if c.wantFires && budget.pick > sc.clampMax {
				t.Errorf("pick CQ %d ran past the clamp ceiling %d", budget.pick, sc.clampMax)
			}
		})
	}
}

func TestAutoCQCostCapAV1Scale(t *testing.T) {
	sc := av1AutoCQScale
	windows := [][2]float64{{100, 8}}
	buckets := []bitrateBucket{{startSec: 96, kbps: testSourceKbps}, {startSec: 104, kbps: testSourceKbps}}
	dir := t.TempDir()
	writeSampleFile(t, dir, sc.anchorLow, testAnchorLow, testSampleSec)
	writeSampleFile(t, dir, sc.anchorHigh, testAnchorHigh, testSampleSec)

	budget, err := autoCQCostCapTarget(sc, dir, buckets, windows, testSampleSec, 40, sc.anchorLow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !budget.fires(sc.anchorLow) {
		t.Fatal("cap must fire on the AV1 scale too")
	}
	if budget.pick <= sc.anchorLow || budget.pick > sc.clampMax {
		t.Errorf("got CQ %d, want a value above %d and at most %d",
			budget.pick, sc.anchorLow, sc.clampMax)
	}
}

// TestAutoCQMaxSourcePercentParsing covers the value check of the new key.
// The lower bound matters: a typo like "4" instead of "40" would otherwise
// quietly cap a whole batch at a bitrate no material survives.
func TestAutoCQMaxSourcePercentParsing(t *testing.T) {
	cases := []struct {
		val   string
		want  float64
		valid bool
	}{
		{"0", 0, true},
		{"10", 10, true},
		{"40", 40, true},
		{"100", 100, true},
		{"4", 0, false},
		{"9.9", 0, false},
		{"101", 0, false},
		{"-1", 0, false},
		{"forty", 0, false},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
			if err := os.WriteFile(path, []byte("autoCQMaxSourcePercent="+c.val+"\n"), 0o644); err != nil {
				t.Fatalf("cannot write config: %v", err)
			}
			parsed, invalids, _ := parseAppConfig(path)
			if c.valid {
				if len(invalids) != 0 {
					t.Fatalf("%q was rejected but should be allowed", c.val)
				}
				if parsed.autoCQMaxSourcePercent != c.want {
					t.Errorf("got %.4g, want %.4g", parsed.autoCQMaxSourcePercent, c.want)
				}
				return
			}
			if len(invalids) == 0 {
				t.Fatalf("%q was accepted but must be rejected", c.val)
			}
			if parsed.autoCQMaxSourcePercent != defaultAppSettings().autoCQMaxSourcePercent {
				t.Errorf("a rejected value must leave the default in place, got %.4g",
					parsed.autoCQMaxSourcePercent)
			}
		})
	}
}

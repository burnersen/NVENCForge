//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVMAFPercentile: linear between neighbouring ranks, like numpy and
// CloudForge — and the caller's slice keeps its order.
func TestVMAFPercentile(t *testing.T) {
	scores := []float64{97, 91, 99, 93, 95} // sorted: 91 93 95 97 99
	cases := []struct {
		percentile int
		want       float64
	}{
		{0, 91},
		{5, 91.4}, // rank 0.2 → 91 + 0.2 · 2
		{25, 93},
		{50, 95},
		{100, 99},
	}
	for _, c := range cases {
		if got := vmafPercentile(scores, c.percentile); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("percentile %d = %.4f, want %.4f", c.percentile, got, c.want)
		}
	}
	if scores[0] != 97 || scores[4] != 95 {
		t.Errorf("the input was reordered: %v", scores)
	}
	if got := vmafPercentile([]float64{93.5}, 5); got != 93.5 {
		t.Errorf("a single frame is its own percentile, got %.4f", got)
	}
}

// TestReadVMAFMeasurement reads a real-shaped libvmaf JSON log.
func TestReadVMAFMeasurement(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	frames := `"frames": [
		{"frameNum": 0, "metrics": {"integer_adm2": 0.99, "vmaf": 97.0}},
		{"frameNum": 3, "metrics": {"integer_adm2": 0.98, "vmaf": 91.0}},
		{"frameNum": 6, "metrics": {"integer_adm2": 0.99, "vmaf": 99.0}},
		{"frameNum": 9, "metrics": {"integer_adm2": 0.98, "vmaf": 93.0}},
		{"frameNum": 12, "metrics": {"integer_adm2": 0.99, "vmaf": 95.0}}]`
	pooled := `"pooled_metrics": {"vmaf": {"min": 91.0, "max": 99.0, "mean": 95.0, "harmonic_mean": 94.9}}`
	log := write("vmaf.json", `{"version": "3.0.0", `+frames+`, `+pooled+`}`)

	m, err := readVMAFMeasurement(log, 5)
	if err != nil {
		t.Fatal(err)
	}
	if m.mean != 95 || math.Abs(m.low-91.4) > 1e-9 {
		t.Errorf("got mean %.4f / 5th percentile %.4f, want 95 / 91.4", m.mean, m.low)
	}
	// Without a percentile only the pooled mean counts, exactly as up to 2.1.0.
	m, err = readVMAFMeasurement(log, 0)
	if err != nil || m.mean != 95 || m.low != 95 {
		t.Errorf("percentile 0: got %+v, %v — want mean = low = 95", m, err)
	}

	noFrames := write("noframes.json", `{"frames": [], `+pooled+`}`)
	if _, err := readVMAFMeasurement(noFrames, 5); err == nil {
		t.Error("a log without frame scores must fail when a percentile is asked for")
	}
	if _, err := readVMAFMeasurement(noFrames, 0); err != nil {
		t.Errorf("the mean alone needs no frame scores: %v", err)
	}
	noScore := write("noscore.json", `{"frames": [], "pooled_metrics": {}}`)
	if _, err := readVMAFMeasurement(noScore, 0); err == nil {
		t.Error("a log without a pooled mean must fail")
	}
	if _, err := readVMAFMeasurement(filepath.Join(dir, "missing.json"), 5); err == nil {
		t.Error("a missing log must fail")
	}
}

// TestAutoCQCriterionScore: with the safety net a measurement holds the
// search target exactly when the percentile AND the mean hold theirs.
func TestAutoCQCriterionScore(t *testing.T) {
	crit := autoCQCriterion{percentile: 5, targetMean: 95, targetLow: 92}
	if crit.target() != 92 {
		t.Fatalf("search target %.4g, want the percentile's 92", crit.target())
	}
	for low := 88.0; low <= 97; low += 0.25 {
		for mean := low; mean <= 99; mean += 0.25 {
			m := autoCQMeasurement{mean: mean, low: low}
			holds := crit.score(m) >= crit.target()
			want := low >= 92 && mean >= 95
			if holds != want {
				t.Fatalf("low %.2f / mean %.2f: holds = %v, want %v", low, mean, holds, want)
			}
		}
	}

	// The mean alone: score and target are the mean's, as up to 2.1.0.
	plain := autoCQCriterion{percentile: 0, targetMean: 96, targetLow: 92}
	if got := plain.score(autoCQMeasurement{mean: 95.5, low: 90}); got != 95.5 || plain.target() != 96 {
		t.Errorf("mean only: score %.4g / target %.4g, want 95.5 / 96", got, plain.target())
	}
}

// TestSafetyNetHoldsTheMean replays the case that made CloudForge 0.19.1: a
// very even film (mean and 5th percentile only ~2.2 apart) held its percentile
// target at a step whose mean had already dropped below the user's 95. The
// search helper must not pick that step any more — and on a film with weak
// scenes the percentile, not the mean, must decide.
func TestSafetyNetHoldsTheMean(t *testing.T) {
	crit := autoCQCriterion{percentile: 5, targetMean: 95, targetLow: 92}
	thriftiest := func(c autoCQCriterion, film map[int]autoCQMeasurement) int {
		scores := make(map[int]float64, len(film))
		for cq, m := range film {
			scores[cq] = c.score(m)
		}
		best, found := autoCQThriftiestHolding(scores, c.target())
		if !found {
			t.Fatal("no CQ holds the target")
		}
		return best.cq
	}

	even := map[int]autoCQMeasurement{
		30: {mean: 96.5, low: 94.3},
		32: {mean: 95.8, low: 93.6},
		34: {mean: 95.1, low: 92.9},
		36: {mean: 94.4, low: 92.2},
		38: {mean: 93.6, low: 91.4},
	}
	if got := thriftiest(crit, even); got != 34 {
		t.Errorf("even film: CQ %d, want 34 (mean 95.1) — CQ 36 holds the percentile but its mean is 94.4", got)
	}
	// Counter-check: without the net (a mean target of 0) CQ 36 would win.
	noNet := autoCQCriterion{percentile: 5, targetMean: 0, targetLow: 92}
	if got := thriftiest(noNet, even); got != 36 {
		t.Errorf("counter-check without the net: CQ %d, want 36", got)
	}

	weakScenes := map[int]autoCQMeasurement{
		28: {mean: 97.3, low: 92.6},
		30: {mean: 96.8, low: 91.9},
		32: {mean: 96.1, low: 91.2},
		34: {mean: 95.4, low: 90.5},
	}
	if got := thriftiest(crit, weakScenes); got != 28 {
		t.Errorf("film with weak scenes: CQ %d, want 28 — the percentile must decide", got)
	}
	if got := thriftiest(autoCQCriterion{percentile: 0, targetMean: 95}, weakScenes); got != 34 {
		t.Errorf("same film judged by the mean alone: CQ %d, want 34", got)
	}
}

// TestAutoCQCriterionTexts: with the mean alone every log text is exactly the
// one of 2.1.0; with the net the log shows real values, never the folded score.
func TestAutoCQCriterionTexts(t *testing.T) {
	plain := autoCQCriterion{percentile: 0, targetMean: 96, targetLow: 92}
	m := autoCQMeasurement{mean: 95.64, low: 92.37}
	checks := []struct{ got, want string }{
		{plain.targetText(), "96"},
		{plain.overviewText(), "VMAF 96"},
		{plain.measurementText(m, 1), "95.6"},
		{plain.measurementText(m, 2), "95.64"},
		{plain.levelText(94.26), "~94.3"},
		{plain.resultText(m, true, 95.64), "VMAF 95.6, target 96"},
		{plain.resultText(m, false, 95.21), "VMAF 95.2, target 96"},
	}
	net := autoCQCriterion{percentile: 5, targetMean: 95, targetLow: 92}
	checks = append(checks, []struct{ got, want string }{
		{net.targetText(), "92 (5th percentile) and 95 (mean)"},
		{net.overviewText(), "VMAF 92 at 5th pct, 95 mean"},
		{net.measurementText(m, 1), "92.4 at the 5th percentile, mean 95.6"},
		{net.levelText(91.84), "~91.8 (5th-percentile scale)"},
		{net.resultText(m, true, 92.37), "VMAF 95.6, 5th percentile 92.4 — targets 95 and 92"},
		{net.resultText(m, false, 92.1),
			"estimated 92.1 on the 5th-percentile scale, target 92 (5th percentile) and 95 (mean)"},
	}...)
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if got := net.meanFloor(92.1); math.Abs(got-95.1) > 1e-9 {
		t.Errorf("meanFloor(92.1) = %.4f, want 95.1", got)
	}
	if got := plain.meanFloor(95.2); got != 95.2 {
		t.Errorf("mean only: meanFloor must return the score, got %.4f", got)
	}
}

func TestOrdinal(t *testing.T) {
	for n, want := range map[int]string{
		1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 5: "5th", 10: "10th",
		11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd", 50: "50th",
	} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestPercentileSettings: defaults, ranges, and that a rejected value keeps
// the default.
func TestPercentileSettings(t *testing.T) {
	d := defaultAppSettings()
	if d.autoCQVMAFPercentile != 5 || d.autoCQTargetVMAF != 95 || d.autoCQTargetVMAFPercentile != 92 {
		t.Errorf("defaults: percentile %d, mean target %.4g, percentile target %.4g — want 5 / 95 / 92",
			d.autoCQVMAFPercentile, d.autoCQTargetVMAF, d.autoCQTargetVMAFPercentile)
	}
	if d.autoCQPlateauTolerance != 0.5 {
		t.Errorf("plateau tolerance %.4g, want 0.5 (the user's choice of 2026-09-11)", d.autoCQPlateauTolerance)
	}
	cases := []struct {
		line    string
		wantBad bool
		check   func(s AppSettings) bool
	}{
		{"autoCQVMAFPercentile=0", false, func(s AppSettings) bool { return s.autoCQVMAFPercentile == 0 }},
		{"autoCQVMAFPercentile=50", false, func(s AppSettings) bool { return s.autoCQVMAFPercentile == 50 }},
		{"autoCQVMAFPercentile=51", true, func(s AppSettings) bool { return s.autoCQVMAFPercentile == 5 }},
		{"autoCQVMAFPercentile=-1", true, func(s AppSettings) bool { return s.autoCQVMAFPercentile == 5 }},
		{"autoCQVMAFPercentile=5.5", true, func(s AppSettings) bool { return s.autoCQVMAFPercentile == 5 }},
		{"autoCQTargetVMAFPercentile=92.5", false, func(s AppSettings) bool { return s.autoCQTargetVMAFPercentile == 92.5 }},
		{"autoCQTargetVMAFPercentile=50", false, func(s AppSettings) bool { return s.autoCQTargetVMAFPercentile == 50 }},
		{"autoCQTargetVMAFPercentile=49.9", true, func(s AppSettings) bool { return s.autoCQTargetVMAFPercentile == 92 }},
		{"autoCQTargetVMAFPercentile=99.5", true, func(s AppSettings) bool { return s.autoCQTargetVMAFPercentile == 92 }},
		{"autoCQTargetVMAFPercentile=hoch", true, func(s AppSettings) bool { return s.autoCQTargetVMAFPercentile == 92 }},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
		if err := os.WriteFile(path, []byte(c.line+"\r\n"), 0644); err != nil {
			t.Fatalf("cannot write test config: %v", err)
		}
		s, invalids, _ := parseAppConfig(path)
		if gotBad := len(invalids) > 0; gotBad != c.wantBad {
			t.Errorf("%s: rejected = %v, want %v", c.line, gotBad, c.wantBad)
		}
		if !c.check(s) {
			t.Errorf("%s: wrong value after parsing", c.line)
		}
	}
	// The template carries both keys with an allowed range (configtemplate_test
	// checks the rest of the round trip).
	var b strings.Builder
	for _, key := range []string{"autoCQVMAFPercentile", "autoCQTargetVMAFPercentile"} {
		if _, ok := defaultConfigStrings()[key]; !ok {
			b.WriteString(key + " ")
		}
	}
	if b.Len() > 0 {
		t.Errorf("missing from defaultConfigStrings: %s", b.String())
	}
}

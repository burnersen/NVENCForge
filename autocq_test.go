//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

func TestAutoCQSampleWindows(t *testing.T) {
	cases := []struct {
		name       string
		duration   float64
		wantCount  int
		wantWinLen float64
	}{
		{"zero duration", 0, 0, 0},
		{"too short", 29.9, 0, 0},
		{"minimum length", 30, 2, autoCQShortWindowSec},
		{"short video", 180, 2, autoCQShortWindowSec},
		{"boundary to long", 240, 4, autoCQWindowSec},
		{"movie length", 7200, 4, autoCQWindowSec},
	}
	for _, c := range cases {
		got := autoCQSampleWindows(c.duration)
		if len(got) != c.wantCount {
			t.Errorf("%s: got %d windows, want %d", c.name, len(got), c.wantCount)
			continue
		}
		prevEnd := -1.0
		for i, w := range got {
			start, length := w[0], w[1]
			if length != c.wantWinLen {
				t.Errorf("%s: window %d has length %.1f, want %.1f", c.name, i, length, c.wantWinLen)
			}
			if start < 0 {
				t.Errorf("%s: window %d starts before 0 (%.2f)", c.name, i, start)
			}
			if start+length > c.duration {
				t.Errorf("%s: window %d (%.2f+%.2f) exceeds duration %.2f",
					c.name, i, start, length, c.duration)
			}
			if start <= prevEnd {
				t.Errorf("%s: window %d overlaps the previous one", c.name, i)
			}
			prevEnd = start + length
		}
	}
}

func TestInterpolateAutoCQ(t *testing.T) {
	cases := []struct {
		name                   string
		vmafLow, vmafHigh, tgt float64
		wantCQ                 int
	}{
		// Real-world anchors from the 2026-07-04 measurement series
		// (test.mp4: CQ26=97.93, CQ30=95.38): target 95 lands right above
		// CQ 30 — the sanity expectation for that file is CQ 30±1.
		{"measurement series", 97.93, 95.38, 95, 31},
		{"target exactly at high anchor", 97, 95, 95, 30},
		{"target exactly at low anchor", 97, 95, 97, 26},
		{"target between anchors", 97, 93, 95, 28},
		// Hard/synthetic material needs a CQ below the anchors.
		{"hard clip", 94, 88, 95, 25},
		{"very hard clip", 90, 85, 95, 22},
		// Unreachable target clamps at the quality end.
		{"unreachable target", 80, 70, 95, autoCQClampMin},
		// Very easy material extrapolates towards the saving end.
		{"easy material", 99.5, 99.2, 95, autoCQClampMax},
		// Flat slope = measurement noise → clamp edge by target side.
		{"flat above target", 99.9, 99.9, 95, autoCQClampMax},
		{"flat below target", 80, 80, 95, autoCQClampMin},
		// Rising slope (noise) is treated like flat.
		{"rising noise above target", 96, 96.2, 95, autoCQClampMax},
	}
	for _, c := range cases {
		gotCQ, predicted := interpolateAutoCQ(c.vmafLow, c.vmafHigh, c.tgt)
		if gotCQ != c.wantCQ {
			t.Errorf("%s: got CQ %d, want %d", c.name, gotCQ, c.wantCQ)
		}
		if gotCQ < autoCQClampMin || gotCQ > autoCQClampMax {
			t.Errorf("%s: CQ %d outside clamp range [%d, %d]",
				c.name, gotCQ, autoCQClampMin, autoCQClampMax)
		}
		if predicted <= 0 || predicted > 101 {
			t.Errorf("%s: implausible predicted VMAF %.2f", c.name, predicted)
		}
	}
}

func TestBuildAutoCQArgs(t *testing.T) {
	windows := [][2]float64{{36, 8}, {84, 8}, {132, 8}, {180, 8}}
	const chain = "crop=trunc(iw/2)*2:trunc(ih/2)*2,format=p010le"

	enc := buildAutoCQEncodeArgs("C:\\videos\\in.mp4", windows, chain,
		30, "8000k", "16000k", 120, "sample_cq30.mkv")
	encStr := strings.Join(enc, " ")
	if got := strings.Count(encStr, "-ss "); got != len(windows) {
		t.Errorf("encode args: %d -ss occurrences, want %d", got, len(windows))
	}
	for _, want := range []string{
		"-cq 30", "-maxrate 8000k", "-bufsize 16000k", "-g 120",
		"concat=n=4:v=1:a=0," + chain, "setpts=PTS-STARTPTS",
		"-map [out]", "-an", "-sn",
	} {
		if !strings.Contains(encStr, want) {
			t.Errorf("encode args missing %q\n%s", want, encStr)
		}
	}
	if enc[len(enc)-1] != "sample_cq30.mkv" {
		t.Errorf("encode args must end with the sample name, got %q", enc[len(enc)-1])
	}

	vmaf := buildAutoCQVMAFArgs("C:\\videos\\in.mp4", windows, chain,
		30000, 1001, "sample_cq30.mkv", "vmaf_cq30.json")
	vmafStr := strings.Join(vmaf, " ")
	for _, want := range []string{
		// Both VMAF inputs must get frame-number-based timestamps (the
		// documented Matroska millisecond-rounding pitfall) and 10-bit format.
		"settb=AVTB,setpts=N*1001/30000/TB",
		"format=yuv420p10le",
		"log_path=vmaf_cq30.json",
		"n_subsample=3",
		"[dist][ref]libvmaf",
	} {
		if !strings.Contains(vmafStr, want) {
			t.Errorf("vmaf args missing %q\n%s", want, vmafStr)
		}
	}
	if vmaf[len(vmaf)-1] != "-" || vmaf[len(vmaf)-2] != "null" {
		t.Errorf("vmaf args must end with '-f null -', got %v", vmaf[len(vmaf)-2:])
	}
}

func TestBucketsFromPacketCSV(t *testing.T) {
	// 25 s at 8 s windows = 3 full buckets; the 24.5 s packet falls into the
	// partial tail and must be dropped. N/A, garbage, negative timestamps and
	// zero sizes are skipped.
	csv := "0.000000,1000\n" +
		"4.000000,1000\n" +
		"N/A,500\n" +
		"garbage\n" +
		"-1.000000,500\n" +
		"9.500000,4000\n" +
		"17.000000,8000\n" +
		"18.000000,0\n" +
		"24.500000,9999\n"
	got := bucketsFromPacketCSV(csv, 25, 8)
	if len(got) != 3 {
		t.Fatalf("got %d buckets, want 3 (%v)", len(got), got)
	}
	wantKbps := []float64{2, 4, 8} // bytes*8/1000/8s
	for i, b := range got {
		if wantStart := float64(i) * 8; b.startSec != wantStart {
			t.Errorf("bucket %d starts at %.1f, want %.1f", i, b.startSec, wantStart)
		}
		if diff := b.kbps - wantKbps[i]; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("bucket %d has %.3f kbps, want %.3f", i, b.kbps, wantKbps[i])
		}
	}

	if bucketsFromPacketCSV(csv, 25, 0) != nil {
		t.Error("windowLen 0 must yield nil")
	}
	if bucketsFromPacketCSV(csv, 5, 8) != nil {
		t.Error("source shorter than one window must yield nil")
	}
}

// testProfile builds n windowLen-buckets with mild variance plus explicit
// peaks (bucket index → kbps).
func testProfile(n int, windowLen float64, peaks map[int]float64) []bitrateBucket {
	out := make([]bitrateBucket, n)
	for i := range out {
		kbps := 1000 + float64(i%7)*50
		if p, ok := peaks[i]; ok {
			kbps = p
		}
		out[i] = bitrateBucket{startSec: float64(i) * windowLen, kbps: kbps}
	}
	return out
}

func TestAutoCQGuidedWindows(t *testing.T) {
	const winLen = 8.0

	// Standard case: the heaviest bucket (index 50 → 400 s) must be sampled,
	// all windows disjoint, chronological and inside the 5% edge margins.
	buckets := testProfile(100, winLen, map[int]float64{50: 9000})
	got := autoCQGuidedWindows(buckets, 800, 4, winLen)
	if len(got) != 4 {
		t.Fatalf("got %d windows, want 4 (%v)", len(got), got)
	}
	foundPeak := false
	prevStart := -winLen
	for i, w := range got {
		if w[1] != winLen {
			t.Errorf("window %d has length %.1f, want %.1f", i, w[1], winLen)
		}
		if w[0] < 800*0.05 || w[0]+winLen > 800*0.95 {
			t.Errorf("window %d at %.1f s violates the edge margin", i, w[0])
		}
		if w[0] < prevStart+winLen {
			t.Errorf("window %d at %.1f s overlaps/precedes the previous one", i, w[0])
		}
		prevStart = w[0]
		if w[0] == 400 {
			foundPeak = true
		}
	}
	if !foundPeak {
		t.Errorf("heaviest bucket (400 s) missing from windows %v", got)
	}

	// A peak inside the edge margin must not be picked; the heaviest bucket
	// WITHIN the margins wins instead.
	buckets = testProfile(100, winLen, map[int]float64{2: 9000, 60: 5000})
	got = autoCQGuidedWindows(buckets, 800, 4, winLen)
	if len(got) != 4 {
		t.Fatalf("edge case: got %d windows, want 4", len(got))
	}
	foundInner := false
	for _, w := range got {
		if w[0] == 16 {
			t.Errorf("edge-margin peak at 16 s must not be sampled (%v)", got)
		}
		if w[0] == 480 {
			foundInner = true
		}
	}
	if !foundInner {
		t.Errorf("inner peak (480 s) missing from windows %v", got)
	}

	// Flat profile (CBR-ish): no placement signal → nil (fixed positions).
	flat := make([]bitrateBucket, 100)
	for i := range flat {
		flat[i] = bitrateBucket{startSec: float64(i) * winLen, kbps: 1000}
	}
	if autoCQGuidedWindows(flat, 800, 4, winLen) != nil {
		t.Error("flat profile must yield nil")
	}

	// Too few full buckets between the margins → nil.
	small := testProfile(8, winLen, map[int]float64{4: 9000})
	if autoCQGuidedWindows(small, 64, 4, winLen) != nil {
		t.Error("too-small profile must yield nil")
	}
}

func TestAutoCQSaturated(t *testing.T) {
	cases := []struct {
		name              string
		cq                int
		verified, vmafLow float64
		want              bool
	}{
		// The real-world case that triggered the brake (2026-07-04):
		// anchor CQ26=96.37, pick 24 measured 96.40 → 0.015 gain per step.
		{"saturated P2P source", 24, 96.40, 96.37, true},
		{"healthy slope", 24, 97.20, 96.37, false}, // 0.415/step
		{"noise dip below anchor", 23, 96.10, 96.37, true},
		{"just above threshold", 24, 96.58, 96.37, false}, // 0.105/step
		{"pick at low anchor", 26, 96.37, 96.37, false},   // guard: only below anchor
		{"pick above anchors", 33, 90.00, 96.37, false},
	}
	for _, c := range cases {
		if got := autoCQSaturated(c.cq, c.verified, c.vmafLow); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAutoCQPlateauPick(t *testing.T) {
	cases := []struct {
		name              string
		vmafLow, vmafHigh float64
		tolerance         float64
		wantCQ            int
		wantVMAF          float64
	}{
		// Real anchors from the saturation report: 26→30 loses 0.325/step.
		// Without tolerance the plateau ends at the low anchor; 0.5 buys
		// exactly one step (0.5/0.325 → 1).
		{"sloped anchors, no tolerance", 96.37, 95.07, 0, autoCQAnchorLow, 96.37},
		{"sloped anchors, tolerance 0.5", 96.37, 95.07, 0.5, 27, 96.045},
		// Gentler slope (0.225/step): the same tolerance buys two steps.
		{"gentle slope, tolerance 0.5", 96.5, 95.6, 0.5, 28, 96.05},
		// Whole curve flat (0.025/step): even CQ 30 sits on the plateau.
		{"flat anchors", 96.40, 96.30, 0, autoCQAnchorHigh, 96.30},
		// A huge tolerance never picks past the high anchor (no measurement
		// exists beyond it).
		{"tolerance capped at high anchor", 96.37, 95.07, 5, autoCQAnchorHigh, 95.07},
	}
	for _, c := range cases {
		cq, vmaf := autoCQPlateauPick(c.vmafLow, c.vmafHigh, c.tolerance)
		if cq != c.wantCQ {
			t.Errorf("%s: got CQ %d, want %d", c.name, cq, c.wantCQ)
		}
		if diff := vmaf - c.wantVMAF; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: got VMAF %.3f, want %.3f", c.name, vmaf, c.wantVMAF)
		}
	}
}

// TestAutoCQClimbCandidates pins the probing order of the plateau climb:
// the cheapest rung (clamp ceiling) first, the midpoint as fallback, and
// nothing at or below the high anchor. Hard-coded values on purpose — a
// change to the anchor/clamp constants must consciously revisit the climb.
func TestAutoCQClimbCandidates(t *testing.T) {
	want := []int{34, 32}
	got := autoCQClimbCandidates()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, rung := range got {
		if rung <= autoCQAnchorHigh || rung > autoCQClampMax {
			t.Errorf("rung %d outside the measurable range (%d, %d]",
				rung, autoCQAnchorHigh, autoCQClampMax)
		}
	}
}

func TestAutoCQStepDown(t *testing.T) {
	cases := []struct {
		name             string
		cq               int
		target, verified float64
		slope            float64
		wantCQ           int
		wantPred         float64
	}{
		// slope -0.5: one CQ step buys ~0.5 VMAF.
		{"two steps", 28, 95, 94.2, -0.5, 26, 95.2},
		{"tiny miss, one step", 28, 95, 94.9, -0.5, 27, 95.4},
		{"big miss capped at 3", 28, 95, 91, -0.5, 25, 92.5},
		{"flat slope defaults to one step", 28, 95, 94, 0, 27, 94},
		{"already at clamp floor", 20, 95, 90, -0.5, 20, 90},
		{"clamped into floor", 21, 95, 90, -0.5, 20, 90.5},
		{"prediction capped at 100", 24, 99.9, 99.5, -3, 23, 100},
	}
	for _, c := range cases {
		gotCQ, gotPred := autoCQStepDown(c.cq, c.target, c.verified, c.slope)
		if gotCQ != c.wantCQ {
			t.Errorf("%s: got CQ %d, want %d", c.name, gotCQ, c.wantCQ)
		}
		if diff := gotPred - c.wantPred; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: got predicted %.3f, want %.3f", c.name, gotPred, c.wantPred)
		}
	}
}

func TestAutoCQSpinnerText(t *testing.T) {
	// Every phase text must come out at exactly the shared width: a longer
	// text would leave fragments of itself behind the next repaint, a
	// shorter one proves the padding is missing.
	cases := []struct {
		name string
		got  string
	}{
		{"scan phase", autoCQSpinnerText(autoCQSpinnerScanText)},
		{"encode phase", autoCQSpinnerText("Auto-CQ: encoding samples at CQ %d...", autoCQClampMax)},
		{"measure phase", autoCQSpinnerText("Auto-CQ: measuring VMAF at CQ %d...", autoCQClampMax)},
	}
	for _, c := range cases {
		if len(c.got) != autoCQSpinnerTextWidth {
			t.Errorf("%s: padded width %d, want %d (%q)",
				c.name, len(c.got), autoCQSpinnerTextWidth, c.got)
		}
	}
}

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
		sc                     autoCQScale
		vmafLow, vmafHigh, tgt float64
		wantCQ                 int
	}{
		// --- H.265 (anchors 26/30, clamp 20-34) — unchanged from before ---
		// Real-world anchors from the 2026-07-04 series (test.mp4: CQ26=97.93,
		// CQ30=95.38): target 95 lands right above CQ 30 → CQ 30±1.
		{"h265 measurement series", hevcAutoCQScale, 97.93, 95.38, 95, 31},
		{"h265 target at high anchor", hevcAutoCQScale, 97, 95, 95, 30},
		{"h265 target at low anchor", hevcAutoCQScale, 97, 95, 97, 26},
		{"h265 target between anchors", hevcAutoCQScale, 97, 93, 95, 28},
		{"h265 hard clip", hevcAutoCQScale, 94, 88, 95, 25},
		{"h265 very hard clip", hevcAutoCQScale, 90, 85, 95, 22},
		{"h265 unreachable target", hevcAutoCQScale, 80, 70, 95, hevcAutoCQScale.clampMin},
		{"h265 easy material", hevcAutoCQScale, 99.5, 99.2, 95, hevcAutoCQScale.clampMax},
		{"h265 flat above target", hevcAutoCQScale, 99.9, 99.9, 95, hevcAutoCQScale.clampMax},
		{"h265 flat below target", hevcAutoCQScale, 80, 80, 95, hevcAutoCQScale.clampMin},
		{"h265 rising noise above target", hevcAutoCQScale, 96, 96.2, 95, hevcAutoCQScale.clampMax},
		// --- AV1 (anchors 24/32, clamp 16-44) — real 2026-07-06 anchors ---
		// Exodus CQ24=96.24, CQ32=94.03: target 97 sits above the low anchor
		// (→ CQ ~21), the 96.5 search target lands just under it (→ CQ 23).
		{"av1 target above low anchor", av1AutoCQScale, 96.24, 94.03, 97, 21},
		{"av1 search target", av1AutoCQScale, 96.24, 94.03, 96.5, 23},
		// Jellyfish CQ24=95.53, CQ32=91.82: harder, needs a lower CQ.
		{"av1 hard clip", av1AutoCQScale, 95.53, 91.82, 95, 25},
		{"av1 unreachable target", av1AutoCQScale, 90, 85, 97, av1AutoCQScale.clampMin},
		{"av1 easy material", av1AutoCQScale, 99.5, 99.2, 95, av1AutoCQScale.clampMax},
		{"av1 flat above target", av1AutoCQScale, 99.9, 99.9, 95, av1AutoCQScale.clampMax},
	}
	for _, c := range cases {
		gotCQ, predicted := interpolateAutoCQ(c.sc, c.vmafLow, c.vmafHigh, c.tgt)
		if gotCQ != c.wantCQ {
			t.Errorf("%s: got CQ %d, want %d", c.name, gotCQ, c.wantCQ)
		}
		if gotCQ < c.sc.clampMin || gotCQ > c.sc.clampMax {
			t.Errorf("%s: CQ %d outside clamp range [%d, %d]",
				c.name, gotCQ, c.sc.clampMin, c.sc.clampMax)
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
		30, "8000k", "16000k", 120, "sample_cq30.mkv", buildNVENCOptsWithCQ)
	encStr := strings.Join(enc, " ")
	if got := strings.Count(encStr, "-ss "); got != len(windows) {
		t.Errorf("encode args: %d -ss occurrences, want %d", got, len(windows))
	}
	for _, want := range []string{
		"-c:v hevc_nvenc", "-cq 30", "-maxrate 8000k", "-bufsize 16000k", "-g 120",
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

	// AV1 samples must run through av1_nvenc at the requested CQ (same builder,
	// different encoder profile) — the whole point of the per-codec buildOpts.
	av1enc := buildAutoCQEncodeArgs("C:\\videos\\in.mp4", windows, chain,
		32, "6000k", "12000k", 120, "sample_cq32.mkv", buildAV1OptsWithCQ)
	av1Str := strings.Join(av1enc, " ")
	for _, want := range []string{"-c:v av1_nvenc", "-cq 32", "-maxrate 6000k"} {
		if !strings.Contains(av1Str, want) {
			t.Errorf("AV1 encode args missing %q\n%s", want, av1Str)
		}
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
		sc                autoCQScale
		cq                int
		verified, vmafLow float64
		want              bool
	}{
		// H.265 (anchor 26, slope 0.1). The real case that triggered the brake
		// (2026-07-04): anchor CQ26=96.37, pick 24 measured 96.40 → 0.015/step.
		{"h265 saturated P2P source", hevcAutoCQScale, 24, 96.40, 96.37, true},
		{"h265 healthy slope", hevcAutoCQScale, 24, 97.20, 96.37, false}, // 0.415/step
		{"h265 noise dip below anchor", hevcAutoCQScale, 23, 96.10, 96.37, true},
		{"h265 just above threshold", hevcAutoCQScale, 24, 96.58, 96.37, false}, // 0.105/step
		{"h265 pick at low anchor", hevcAutoCQScale, 26, 96.37, 96.37, false},   // guard: only below anchor
		{"h265 pick above anchors", hevcAutoCQScale, 33, 90.00, 96.37, false},
		// AV1 (anchor 24, slope 0.05 — half, one AV1 step is worth ~half a VMAF step).
		{"av1 saturated", av1AutoCQScale, 22, 95.60, 95.53, true},      // 0.035/step
		{"av1 healthy slope", av1AutoCQScale, 22, 95.90, 95.53, false}, // 0.185/step
		{"av1 pick at low anchor", av1AutoCQScale, 24, 95.53, 95.53, false},
	}
	for _, c := range cases {
		if got := autoCQSaturated(c.sc, c.cq, c.verified, c.vmafLow); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAutoCQPlateauPick(t *testing.T) {
	cases := []struct {
		name              string
		sc                autoCQScale
		vmafLow, vmafHigh float64
		tolerance         float64
		wantCQ            int
		wantVMAF          float64
	}{
		// H.265: real anchors from the saturation report, 26→30 loses 0.325/step.
		// Without tolerance the plateau ends at the low anchor; 0.5 buys exactly
		// one step (0.5/0.325 → 1).
		{"h265 sloped, no tolerance", hevcAutoCQScale, 96.37, 95.07, 0, hevcAutoCQScale.anchorLow, 96.37},
		{"h265 sloped, tolerance 0.5", hevcAutoCQScale, 96.37, 95.07, 0.5, 27, 96.045},
		// Gentler slope (0.225/step): the same tolerance buys two steps.
		{"h265 gentle slope, tolerance 0.5", hevcAutoCQScale, 96.5, 95.6, 0.5, 28, 96.05},
		// Whole curve flat (0.025/step): even the high anchor sits on the plateau.
		{"h265 flat anchors", hevcAutoCQScale, 96.40, 96.30, 0, hevcAutoCQScale.anchorHigh, 96.30},
		// A huge tolerance never picks past the high anchor (no measurement beyond).
		{"h265 tolerance capped", hevcAutoCQScale, 96.37, 95.07, 5, hevcAutoCQScale.anchorHigh, 95.07},
		// AV1: anchors 24/32 (span 8). Exodus 96.24/94.03 loses 0.27625/step,
		// so 0.5 tolerance buys one step (24→25); a huge tolerance stops at 32.
		{"av1 sloped, tolerance 0.5", av1AutoCQScale, 96.24, 94.03, 0.5, 25, 95.96375},
		{"av1 flat anchors", av1AutoCQScale, 95.60, 95.55, 0, av1AutoCQScale.anchorHigh, 95.55},
		{"av1 tolerance capped", av1AutoCQScale, 96.24, 94.03, 5, av1AutoCQScale.anchorHigh, 94.03},
	}
	for _, c := range cases {
		cq, vmaf := autoCQPlateauPick(c.sc, c.vmafLow, c.vmafHigh, c.tolerance)
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
	cases := []struct {
		name string
		sc   autoCQScale
		want []int
	}{
		{"h265 climb 34,32", hevcAutoCQScale, []int{34, 32}},
		{"av1 climb 44,38", av1AutoCQScale, []int{44, 38}},
	}
	for _, c := range cases {
		got := autoCQClimbCandidates(c.sc)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
		for _, rung := range got {
			if rung <= c.sc.anchorHigh || rung > c.sc.clampMax {
				t.Errorf("%s: rung %d outside the measurable range (%d, %d]",
					c.name, rung, c.sc.anchorHigh, c.sc.clampMax)
			}
		}
	}
}

func TestAutoCQClimbFloor(t *testing.T) {
	cases := []struct {
		name          string
		sc            autoCQScale
		vmafHigh, tol float64
		rungScore     float64 // a candidate rung's measured VMAF
		wantAccept    bool    // rung taken (score >= floor)?
	}{
		// H.265 keeps factor 1.0: floor = 94.16 - 0.5 = 93.66, so CQ 38 at 93.65
		// is NOT taken — the pre-fix behaviour, unchanged.
		{"h265 unchanged floor", hevcAutoCQScale, 94.16, 0.5, 93.65, false},
		// AV1 real Big Buck Bunny case (user 2026-07-06): anchor CQ 32 = 94.16,
		// CQ 38 = 93.65. Factor 2.0 → floor 93.16 → CQ 38 IS now taken (was 32).
		{"av1 flat plateau (BBB) climbs", av1AutoCQScale, 94.16, 0.5, 93.65, true},
		// AV1 steep plateau (detail-rich source, measured 2026-07-06): CQ 38 at
		// 91.83 sits well below floor 92.59, so AV1 stays at the high anchor —
		// the climb does not overreach into real quality loss.
		{"av1 steep plateau stays put", av1AutoCQScale, 93.59, 0.5, 91.83, false},
	}
	for _, c := range cases {
		floor := autoCQClimbFloor(c.sc, c.vmafHigh, c.tol)
		if got := c.rungScore >= floor; got != c.wantAccept {
			t.Errorf("%s: rung %.2f vs floor %.2f → accept=%v, want %v",
				c.name, c.rungScore, floor, got, c.wantAccept)
		}
	}
}

// TestAV1AutoCQFallback pins the decoupled AV1 Auto-CQ fallback: when the
// analysis cannot run, AV1 must fall back to av1AutoCQFallbackCQ (the low
// anchor, ~VMAF 96), NOT to the lean manual av1TargetCQ (32, ~VMAF 94). This
// guards the 2026-07-07 decoupling from being silently reverted.
func TestAV1AutoCQFallback(t *testing.T) {
	if got := av1AutoCQScale.fallbackCQ(); got != av1AutoCQFallbackCQ {
		t.Errorf("av1 fallback CQ = %d, want %d", got, av1AutoCQFallbackCQ)
	}
	if av1AutoCQFallbackCQ != av1AutoCQScale.anchorLow {
		t.Errorf("av1 fallback %d should equal the low anchor %d (near the VMAF target)",
			av1AutoCQFallbackCQ, av1AutoCQScale.anchorLow)
	}
	if av1AutoCQFallbackCQ < av1AutoCQScale.clampMin || av1AutoCQFallbackCQ > av1AutoCQScale.clampMax {
		t.Errorf("av1 fallback %d outside clamp [%d, %d]",
			av1AutoCQFallbackCQ, av1AutoCQScale.clampMin, av1AutoCQScale.clampMax)
	}
}

func TestAutoCQStepDown(t *testing.T) {
	cases := []struct {
		name             string
		sc               autoCQScale
		cq               int
		target, verified float64
		slope            float64
		wantCQ           int
		wantPred         float64
		wantCapped       bool
	}{
		// H.265 (maxStepDown 3, clampMin 20). slope -0.5: one step buys ~0.5 VMAF.
		{"h265 two steps", hevcAutoCQScale, 28, 95, 94.2, -0.5, 26, 95.2, false},
		{"h265 tiny miss, one step", hevcAutoCQScale, 28, 95, 94.9, -0.5, 27, 95.4, false},
		{"h265 big miss capped at 3", hevcAutoCQScale, 28, 95, 91, -0.5, 25, 92.5, true},
		{"h265 flat slope defaults to one step", hevcAutoCQScale, 28, 95, 94, 0, 27, 94, false},
		{"h265 already at clamp floor", hevcAutoCQScale, 20, 95, 90, -0.5, 20, 90, true},
		{"h265 clamped into floor", hevcAutoCQScale, 21, 95, 90, -0.5, 20, 90.5, true},
		{"h265 prediction capped at 100", hevcAutoCQScale, 24, 99.9, 99.5, -3, 23, 100, false},
		// AV1 (maxStepDown 6, clampMin 16) — the wider scale allows deeper steps.
		{"av1 big miss capped at 6", av1AutoCQScale, 28, 95, 90, -0.5, 22, 93, true},
		{"av1 clamped into floor", av1AutoCQScale, 18, 95, 90, -0.5, 16, 91, true},
		// Real 2026-07-10 CreamPiled AV1 case, second stage: after the capped
		// jump 44→38 the re-measurement (94.9) still misses 95.5, and the LOCAL
		// slope from the two fresh points (-0.792/step) buys one ordinary,
		// uncapped final step to CQ 37.
		{"av1 local slope after re-measure", av1AutoCQScale, 38, 95.5, 94.9, -0.792, 37, 95.692, false},
	}
	for _, c := range cases {
		gotCQ, gotPred, gotCapped := autoCQStepDown(c.sc, c.cq, c.target, c.verified, c.slope)
		if gotCQ != c.wantCQ {
			t.Errorf("%s: got CQ %d, want %d", c.name, gotCQ, c.wantCQ)
		}
		if diff := gotPred - c.wantPred; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: got predicted %.3f, want %.3f", c.name, gotPred, c.wantPred)
		}
		if gotCapped != c.wantCapped {
			t.Errorf("%s: got capped=%v, want %v", c.name, gotCapped, c.wantCapped)
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
		{"encode phase", autoCQSpinnerText("Auto-CQ: encoding samples at CQ %d...", av1AutoCQScale.clampMax)},
		{"measure phase", autoCQSpinnerText("Auto-CQ: measuring VMAF at CQ %d...", av1AutoCQScale.clampMax)},
	}
	for _, c := range cases {
		if len(c.got) != autoCQSpinnerTextWidth {
			t.Errorf("%s: padded width %d, want %d (%q)",
				c.name, len(c.got), autoCQSpinnerTextWidth, c.got)
		}
	}
}

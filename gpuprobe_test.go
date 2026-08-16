//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"errors"
	"strings"
	"testing"
)

// Background (2026-08-16): the GPU probe used to fall from "5 B-frames plus
// Temporal AQ" straight to "none of it". A card that merely allows FEWER
// B-frames therefore lost them completely — and B-frames are one of the biggest
// size savers we have. The probe now counts down to the card's real maximum and
// checks -b_ref_mode as a capability of its own.
//
// Because probeNVENCFeatures takes its trial encoder as a parameter, all of this
// can be tested against simulated cards, with no NVIDIA hardware involved.

// fakeGPU stands in for tryEncode and rejects exactly what the described card
// cannot do. Kept deliberately literal — a clever fake would test itself.
func fakeGPU(maxBFrames int, advancedAQ, bRefMode bool, maxLookahead int) func(nvencProbe) (string, error) {
	return func(p nvencProbe) (string, error) {
		switch {
		case p.lookahead > maxLookahead:
			return "out of memory", errors.New("lookahead too large")
		case p.bFrames > maxBFrames:
			return "Max B-frames exceed", errors.New("too many B-frames")
		case p.advancedAQ && !advancedAQ:
			return "temporal AQ not supported", errors.New("no temporal AQ")
		case p.bRefMode && !bRefMode:
			return "b_ref_mode not supported", errors.New("no b_ref_mode")
		}
		return "", nil
	}
}

// restoreProbeState puts the package-level GPU flags and settings back, so one
// test cannot leak its simulated card into the next.
func restoreProbeState(t *testing.T) {
	t.Helper()
	settings, aq, ref := appSettings, nvencAdvancedAQ, nvencBFrameRefMode
	t.Cleanup(func() {
		appSettings, nvencAdvancedAQ, nvencBFrameRefMode = settings, aq, ref
	})
	appSettings.bFrames = 5
	appSettings.nvencLookahead = 32
	appSettings.nvencPreset = "p5"
	nvencAdvancedAQ = true
	nvencBFrameRefMode = true
}

// TestProbeKeepsHighestBFrameCount is the whole point of the rewrite: a card
// limited to 4 must end up with 4, not 0.
func TestProbeKeepsHighestBFrameCount(t *testing.T) {
	restoreProbeState(t)

	if err := probeNVENCFeatures(fakeGPU(4, true, true, 32)); err != nil {
		t.Fatalf("a usable card must not fail the probe: %v", err)
	}
	if appSettings.bFrames != 4 {
		t.Errorf("probe settled on %d B-frames, want 4 (the card's maximum)", appSettings.bFrames)
	}
	if !nvencAdvancedAQ {
		t.Error("Temporal AQ was switched off although the card supports it")
	}
	if !nvencBFrameRefMode {
		t.Error("b_ref_mode was switched off although the card supports it")
	}
}

// TestProbeSplitsBFramesFromRefMode covers the second half of the split: a card
// that encodes B-frames but rejects b_ref_mode must keep its B-frames.
func TestProbeSplitsBFramesFromRefMode(t *testing.T) {
	restoreProbeState(t)

	if err := probeNVENCFeatures(fakeGPU(5, true, false, 32)); err != nil {
		t.Fatalf("a usable card must not fail the probe: %v", err)
	}
	if appSettings.bFrames != 5 {
		t.Errorf("B-frames dropped to %d because b_ref_mode is missing — they are independent", appSettings.bFrames)
	}
	if nvencBFrameRefMode {
		t.Error("b_ref_mode must be switched off when the card rejects it")
	}
}

// TestProbeMissingTemporalAQKeepsBFrames pins that the two Turing-era features
// no longer drag each other down.
func TestProbeMissingTemporalAQKeepsBFrames(t *testing.T) {
	restoreProbeState(t)

	if err := probeNVENCFeatures(fakeGPU(5, false, true, 32)); err != nil {
		t.Fatalf("a usable card must not fail the probe: %v", err)
	}
	if nvencAdvancedAQ {
		t.Error("Temporal AQ must be switched off when the card rejects it")
	}
	if appSettings.bFrames != 5 {
		t.Errorf("B-frames dropped to %d although only Temporal AQ is missing", appSettings.bFrames)
	}
}

// TestProbePreTuringCard is the old behaviour, still correct: no B-frames at
// all, no Temporal AQ, but the card keeps working.
func TestProbePreTuringCard(t *testing.T) {
	restoreProbeState(t)

	if err := probeNVENCFeatures(fakeGPU(0, false, false, 32)); err != nil {
		t.Fatalf("a pre-Turing card must still be usable: %v", err)
	}
	if appSettings.bFrames != 0 {
		t.Errorf("B-frames must be 0 on a card without support, got %d", appSettings.bFrames)
	}
	if nvencAdvancedAQ || nvencBFrameRefMode {
		t.Error("neither Temporal AQ nor b_ref_mode may stay on for a pre-Turing card")
	}
}

// TestProbeShrinksLookahead covers the memory case the INI help text describes:
// instead of giving up, the probe offers the card a smaller window.
func TestProbeShrinksLookahead(t *testing.T) {
	restoreProbeState(t)

	if err := probeNVENCFeatures(fakeGPU(5, true, true, 16)); err != nil {
		t.Fatalf("a card with little memory must still be usable: %v", err)
	}
	if appSettings.nvencLookahead != 16 {
		t.Errorf("lookahead is %d, want 16 (the largest the card accepts)", appSettings.nvencLookahead)
	}
	if appSettings.bFrames != 5 {
		t.Errorf("B-frames dropped to %d although only the lookahead was too large", appSettings.bFrames)
	}
}

// TestProbeRejectsUnusableCard makes sure we still fail loudly when nothing
// works — silently "succeeding" would break every single file later.
func TestProbeRejectsUnusableCard(t *testing.T) {
	restoreProbeState(t)

	dead := func(nvencProbe) (string, error) {
		return "no NVENC capable devices found", errors.New("dead card")
	}
	err := probeNVENCFeatures(dead)
	if err == nil {
		t.Fatal("an unusable GPU must return an error")
	}
	if !strings.Contains(err.Error(), "no NVENC capable devices found") {
		t.Errorf("the error must carry FFmpeg's own message, got: %v", err)
	}
}

// TestProbeRespectsUserZero guards a small edge case: somebody who set
// bFrames=0 themselves must not be told their GPU cannot do B-frames.
func TestProbeRespectsUserZero(t *testing.T) {
	restoreProbeState(t)
	appSettings.bFrames = 0

	if err := probeNVENCFeatures(fakeGPU(5, true, true, 32)); err != nil {
		t.Fatalf("probe must not fail: %v", err)
	}
	if appSettings.bFrames != 0 {
		t.Errorf("the user's own 0 must stay 0, got %d", appSettings.bFrames)
	}
	if nvencBFrameRefMode {
		t.Error("without B-frames there is nothing for b_ref_mode to reference")
	}
}

// TestRefModeReachesEncoder closes the loop: probing is pointless if the real
// encode ignores the result.
func TestRefModeReachesEncoder(t *testing.T) {
	restoreProbeState(t)
	appSettings.bFrames = 4

	nvencBFrameRefMode = true
	with := strings.Join(buildNVENCOptsWithCQ(30, "8000k", "16000k", 240), " ")
	if !strings.Contains(with, "-b_ref_mode 2") {
		t.Errorf("a capable card must still get -b_ref_mode\n%s", with)
	}

	nvencBFrameRefMode = false
	without := strings.Join(buildNVENCOptsWithCQ(30, "8000k", "16000k", 240), " ")
	if strings.Contains(without, "-b_ref_mode") {
		t.Errorf("-b_ref_mode must disappear when the GPU rejects it\n%s", without)
	}
	if !strings.Contains(without, "-bf 4") {
		t.Errorf("B-frames must survive a missing b_ref_mode\n%s", without)
	}
}

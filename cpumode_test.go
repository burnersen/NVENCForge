//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

// withCPUMode runs fn with the CPU backend active and restores the previous
// state afterwards, so one test cannot leak its backend into the next.
func withCPUMode(on bool, fn func()) {
	prev := cpuModeActive
	cpuModeActive = on
	defer func() { cpuModeActive = prev }()
	fn()
}

// TestBuildCPUEncoderOpts pins what the CPU encoders actually emit. The
// 10-bit pixel format matters most: p010le is the GPU layout, the CPU
// encoders need yuv420p10le, and silently dropping to 8 bit would cost
// quality without any error message.
func TestBuildCPUEncoderOpts(t *testing.T) {
	prev := appSettings
	defer func() { appSettings = prev }()
	appSettings.cpuPreset = "fast"
	appSettings.cpuAV1Preset = 6
	appSettings.cpuThreads = 0

	x265 := strings.Join(buildX265OptsWithCQ(19, "8000k", "16000k", 120), " ")
	for _, want := range []string{
		"-c:v libx265", "-crf 19", "-maxrate 8000k", "-bufsize 16000k",
		"-profile:v main10", "-pix_fmt yuv420p10le", "-preset fast",
		"-g 120", "-fps_mode cfr",
	} {
		if !strings.Contains(x265, want) {
			t.Errorf("x265 opts missing %q\n%s", want, x265)
		}
	}
	// NVENC-only switches must not leak into the CPU command line: FFmpeg
	// would abort the whole encode on an unknown option.
	for _, forbidden := range []string{"nvenc", "-rc vbr", "-tune hq", "-b_ref_mode", "-temporal-aq", "-multipass"} {
		if strings.Contains(x265, forbidden) {
			t.Errorf("x265 opts must not contain %q\n%s", forbidden, x265)
		}
	}

	svt := strings.Join(buildSVTAV1OptsWithCQ(24, "6000k", "12000k", 96), " ")
	for _, want := range []string{
		"-c:v libsvtav1", "-crf 24", "-maxrate 6000k", "-bufsize 12000k",
		"-pix_fmt yuv420p10le", "-preset 6", "-g 96",
	} {
		if !strings.Contains(svt, want) {
			t.Errorf("svt opts missing %q\n%s", want, svt)
		}
	}
	// SVT-AV1 has no profile option; passing one aborts the encode.
	if strings.Contains(svt, "-profile") {
		t.Errorf("svt opts must not set a profile\n%s", svt)
	}
}

// TestCPUThreadLimit checks the thread cap: 0 means "all cores" and must not
// emit any option at all, while a set value has to use the switch the
// respective encoder actually honours (libsvtav1 ignores -threads).
func TestCPUThreadLimit(t *testing.T) {
	prev := appSettings
	defer func() { appSettings = prev }()

	appSettings.cpuThreads = 0
	if got := strings.Join(buildX265OptsWithCQ(19, "8000k", "16000k", 120), " "); strings.Contains(got, "-threads") {
		t.Errorf("cpuThreads=0 must not emit -threads\n%s", got)
	}
	if got := strings.Join(buildSVTAV1OptsWithCQ(24, "6000k", "12000k", 96), " "); strings.Contains(got, "lp=") {
		t.Errorf("cpuThreads=0 must not emit lp=\n%s", got)
	}

	appSettings.cpuThreads = 8
	if got := strings.Join(buildX265OptsWithCQ(19, "8000k", "16000k", 120), " "); !strings.Contains(got, "-threads 8") {
		t.Errorf("x265 opts missing -threads 8\n%s", got)
	}
	if got := strings.Join(buildSVTAV1OptsWithCQ(24, "6000k", "12000k", 96), " "); !strings.Contains(got, "lp=8") {
		t.Errorf("svt opts missing lp=8\n%s", got)
	}
}

// TestActiveBackendSelection walks all four combinations of target codec and
// backend. This is the single place where GPU and CPU part ways, so a wrong
// pick here would silently encode with the wrong encoder.
func TestActiveBackendSelection(t *testing.T) {
	prev := appSettings
	defer func() { appSettings = prev }()
	appSettings.targetCQ = 26
	appSettings.av1TargetCQ = 32
	appSettings.cpuTargetCRF = 18
	appSettings.cpuAV1TargetCRF = 30

	cases := []struct {
		name       string
		cpu, av1   bool
		wantEncStr string
		wantCQ     int
		wantScale  autoCQScale
	}{
		{"gpu h265", false, false, "hevc_nvenc", 26, hevcAutoCQScale},
		{"gpu av1", false, true, "av1_nvenc", 32, av1AutoCQScale},
		{"cpu h265", true, false, "libx265", 18, x265AutoCQScale},
		{"cpu av1", true, true, "libsvtav1", 30, svtav1AutoCQScale},
	}
	for _, c := range cases {
		withCPUMode(c.cpu, func() {
			opts := strings.Join(activeVideoOptsBuilder(c.av1)(20, "8000k", "16000k", 120), " ")
			if !strings.Contains(opts, c.wantEncStr) {
				t.Errorf("%s: encoder %q not found\n%s", c.name, c.wantEncStr, opts)
			}
			if got := activeManualCQ(c.av1); got != c.wantCQ {
				t.Errorf("%s: manual CQ = %d, want %d", c.name, got, c.wantCQ)
			}
			if got := activeAutoCQScale(c.av1); got.codecLabel != c.wantScale.codecLabel {
				t.Errorf("%s: scale = %q, want %q", c.name, got.codecLabel, c.wantScale.codecLabel)
			}
		})
	}
}

// TestCPUAutoCQScales guards the anchors measured on 2026-07-25 against
// accidental edits and checks the invariants every scale profile must hold.
func TestCPUAutoCQScales(t *testing.T) {
	prev := appSettings
	defer func() { appSettings = prev }()
	appSettings.cpuTargetCRF = 18

	for _, sc := range []autoCQScale{x265AutoCQScale, svtav1AutoCQScale} {
		if sc.anchorLow >= sc.anchorHigh {
			t.Errorf("%s: anchorLow %d must be below anchorHigh %d",
				sc.codecLabel, sc.anchorLow, sc.anchorHigh)
		}
		if sc.clampMin > sc.anchorLow || sc.clampMax < sc.anchorHigh {
			t.Errorf("%s: clamp [%d, %d] must contain both anchors %d/%d",
				sc.codecLabel, sc.clampMin, sc.clampMax, sc.anchorLow, sc.anchorHigh)
		}
		if sc.buildOpts == nil || sc.fallbackCQ == nil {
			t.Errorf("%s: scale needs both buildOpts and fallbackCQ", sc.codecLabel)
		}
		if cq := sc.fallbackCQ(); cq < sc.clampMin || cq > sc.clampMax {
			t.Errorf("%s: fallback %d outside clamp [%d, %d]",
				sc.codecLabel, cq, sc.clampMin, sc.clampMax)
		}
		if sc.saturationSlope <= 0 || sc.climbToleranceFactor <= 0 || sc.maxStepDown <= 0 {
			t.Errorf("%s: slope/factor/stepDown must all be positive", sc.codecLabel)
		}
		if sc.minGainPerStep <= 0 {
			t.Errorf("%s: minGainPerStep must be positive", sc.codecLabel)
		}
	}

	// The measured anchors themselves (VMAF 96 sits at x265 CRF ~18; the SVT
	// scale mirrors av1_nvenc because their step widths match).
	if x265AutoCQScale.anchorLow != 18 || x265AutoCQScale.anchorHigh != 22 {
		t.Errorf("x265 anchors = %d/%d, want 18/22 (measurement 2026-07-25)",
			x265AutoCQScale.anchorLow, x265AutoCQScale.anchorHigh)
	}
	if svtav1AutoCQScale.anchorLow != av1AutoCQScale.anchorLow ||
		svtav1AutoCQScale.anchorHigh != av1AutoCQScale.anchorHigh {
		t.Errorf("svt anchors = %d/%d, want the av1_nvenc pair %d/%d",
			svtav1AutoCQScale.anchorLow, svtav1AutoCQScale.anchorHigh,
			av1AutoCQScale.anchorLow, av1AutoCQScale.anchorHigh)
	}
	// Same reasoning as the av1_nvenc fallback: an unmeasurable clip must land
	// near the quality target, not on the lean manual value.
	if svtav1AutoCQFallbackCRF != svtav1AutoCQScale.anchorLow {
		t.Errorf("svt fallback %d should equal the low anchor %d",
			svtav1AutoCQFallbackCRF, svtav1AutoCQScale.anchorLow)
	}
}

// TestCPUSampleEncodesMatchRealEncode is the reason Auto-CQ can be trusted at
// all: the sample encodes have to run with exactly the options of the real
// encode, otherwise the measured VMAF describes a file nobody ever gets.
func TestCPUSampleEncodesMatchRealEncode(t *testing.T) {
	prev := appSettings
	defer func() { appSettings = prev }()
	appSettings.cpuPreset = "fast"
	appSettings.cpuAV1Preset = 6
	appSettings.cpuThreads = 4

	windows := [][2]float64{{36, 8}, {84, 8}, {132, 8}}
	const chain = "crop=trunc(iw/2)*2:trunc(ih/2)*2,format=p010le"

	withCPUMode(true, func() {
		for _, c := range []struct {
			av1  bool
			want string
		}{{false, "-c:v libx265"}, {true, "-c:v libsvtav1"}} {
			real := strings.Join(activeVideoOptsBuilder(c.av1)(20, "8000k", "16000k", 120), " ")
			sample := strings.Join(buildAutoCQEncodeArgs("C:\\videos\\in.mp4", windows, nil, chain,
				30000, 1001, 20, "8000k", "16000k", 120, "sample.mkv",
				activeAutoCQScale(c.av1).buildOpts), " ")
			if !strings.Contains(sample, real) {
				t.Errorf("sample encode does not carry the real options\nreal:   %s\nsample: %s", real, sample)
			}
			if !strings.Contains(sample, c.want) {
				t.Errorf("sample encode missing %q\n%s", c.want, sample)
			}
		}
	})
}

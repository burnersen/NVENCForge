//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"math"
	"testing"
)

// Die Zahlen der Kalibrier-Messreihe vom 2026-08-15 (vidz-Ordner) sind hier
// als Regression festgenagelt: die magere Quelle MUSS unter der Schwelle
// liegen, die gesunden deutlich darüber. Verschiebt eine Formel-Änderung
// diese Einstufung, soll der Test laut werden.

func leanStats(codec string, w, h, fpsNum, fpsDen int) *VideoStats {
	return &VideoStats{VideoCodec: codec, Width: w, Height: h,
		FPSNum: fpsNum, FPSDen: fpsDen}
}

func TestLeanSourceBPPFCalibrationFiles(t *testing.T) {
	cases := []struct {
		name      string
		stats     *VideoStats
		videoKbps int64
		wantLean  bool
	}{
		// niedrigerate.mp4: 1080p29.97 H.264, 3102 kbps → BPPF ~0,050
		{"niedrigerate", leanStats("h264", 1920, 1080, 30000, 1001), 3102, true},
		// hohebitrate.mp4: 1080p59.94 H.264, 12504 kbps → BPPF ~0,142
		{"hohebitrate", leanStats("h264", 1920, 1080, 60000, 1001), 12504, false},
		// test.mp4: wie hohebitrate
		{"test", leanStats("h264", 1920, 1080, 60000, 1001), 12487, false},
		// 1080p59.94 H.264, 10992 kbps → BPPF ~0,125
		{"misty-1080p60", leanStats("h264", 1920, 1080, 2997, 50), 10992, false},
	}
	for _, tc := range cases {
		bppf, eligible := leanSourceBPPF(tc.stats, tc.videoKbps)
		if !eligible {
			t.Fatalf("%s: unerwartet nicht bewertbar", tc.name)
		}
		isLean := bppf < leanCheckThreshold(tc.stats.Height, false)
		if isLean != tc.wantLean {
			t.Errorf("%s: BPPF %.4f → lean=%v, erwartet %v",
				tc.name, bppf, isLean, tc.wantLean)
		}
	}
}

func TestLeanSourceBPPFKnownValue(t *testing.T) {
	// Handrechnung niedrigerate.mp4: 3102000 * 1.0 / (1920*1080*sqrt(30*29.97))
	stats := leanStats("h264", 1920, 1080, 30000, 1001)
	bppf, eligible := leanSourceBPPF(stats, 3102)
	if !eligible {
		t.Fatal("unerwartet nicht bewertbar")
	}
	fps := 30000.0 / 1001.0
	want := 3102000.0 / (1920.0 * 1080.0 * math.Sqrt(30.0*fps))
	if math.Abs(bppf-want) > 1e-9 {
		t.Errorf("BPPF %.6f, erwartet %.6f", bppf, want)
	}
}

func TestLeanSourceBPPFFPSDamping(t *testing.T) {
	// Wurzel-Dämpfung: 60 fps bei doppelter Bitrate muss BESSER dastehen
	// (höherer BPPF-Wert) als 30 fps bei einfacher Bitrate — linear gerechnet
	// wären beide gleich, genau das wäre der Fehler.
	bppf30, _ := leanSourceBPPF(leanStats("h264", 1920, 1080, 30, 1), 3000)
	bppf60, _ := leanSourceBPPF(leanStats("h264", 1920, 1080, 60, 1), 6000)
	if bppf60 <= bppf30 {
		t.Errorf("60 fps bei doppelter Bitrate: BPPF %.4f, muss über %.4f (30 fps) liegen",
			bppf60, bppf30)
	}
}

func TestCodecEfficiencyFactor(t *testing.T) {
	cases := []struct {
		codec    string
		want     float64
		eligible bool
	}{
		{"h264", 1.0, true},
		{"H264", 1.0, true}, // Groß-/Kleinschreibung egal
		{"hevc", 1.5, true},
		{"vp9", 1.5, true},
		{"av1", 1.9, true},
		{"mpeg2video", 0.6, true},
		{"vc1", 0.7, true},
		// Zwischenformate und Unbekanntes: nie als mager einstufen.
		{"prores", 0, false},
		{"dnxhd", 0, false},
		{"ffv1", 0, false},
		{"mjpeg", 0, false},
		{"", 0, false},
		{"somethingnew", 0, false},
	}
	for _, tc := range cases {
		got, eligible := codecEfficiencyFactor(tc.codec)
		if eligible != tc.eligible || (eligible && got != tc.want) {
			t.Errorf("codecEfficiencyFactor(%q) = %v/%v, erwartet %v/%v",
				tc.codec, got, eligible, tc.want, tc.eligible)
		}
	}
}

func TestLeanSourceBPPFBadMetadata(t *testing.T) {
	// Unbrauchbare Metadaten dürfen NIE zu einem Skip führen (eligible=false).
	cases := []struct {
		name      string
		stats     *VideoStats
		videoKbps int64
	}{
		{"fps 0", leanStats("h264", 1920, 1080, 0, 1), 3000},
		{"fpsDen 0", leanStats("h264", 1920, 1080, 30, 0), 3000},
		{"Breite 0", leanStats("h264", 0, 1080, 30, 1), 3000},
		{"Höhe 0", leanStats("h264", 1920, 0, 30, 1), 3000},
		{"Bitrate 0", leanStats("h264", 1920, 1080, 30, 1), 0},
		{"Bitrate negativ", leanStats("h264", 1920, 1080, 30, 1), -500},
	}
	for _, tc := range cases {
		if _, eligible := leanSourceBPPF(tc.stats, tc.videoKbps); eligible {
			t.Errorf("%s: unerwartet bewertbar", tc.name)
		}
	}
}

func TestLeanCheckThreshold(t *testing.T) {
	base := appSettings.leanCheckBPPF
	if got := leanCheckThreshold(1080, false); got != base {
		t.Errorf("1080p: %v, erwartet Basiswert %v", got, base)
	}
	if got := leanCheckThreshold(720, false); got != base {
		t.Errorf("720p: %v, erwartet Basiswert %v", got, base)
	}
	if got := leanCheckThreshold(1440, false); got != base*0.9 {
		t.Errorf("1440p: %v, erwartet %v", got, base*0.9)
	}
	if got := leanCheckThreshold(2160, false); got != base*0.8 {
		t.Errorf("2160p: %v, erwartet %v", got, base*0.8)
	}
	// AV1-Ziel skippt seltener → niedrigere Schwelle.
	if got := leanCheckThreshold(1080, true); got != base*0.85 {
		t.Errorf("1080p AV1: %v, erwartet %v", got, base*0.85)
	}
}

//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

// Seit 2.0.0 gibt es keinen Bitraten-Deckel mehr. Vor jeder Analyse steht nur
// fest, was ohne Messung umgepackt wird: Bilder unter 720p und Quellen, die
// schon am Boden ihrer Auflösung liegen. Diese Regel gibt es genau EINMAL
// (upfrontRemuxReason) — die Übersprung-Prüfung und die Weiche fragen beide dort.

// TestUpfrontRemuxLeanRuleUnchanged hält die alte Mager-Regel fest: bis 1.34.0
// stand sie als "max(80 % der Quelle, Boden) < Quelle" im Code, und das war
// rechnerisch genau "Quelle > Boden". Der Umbau darf daran nichts verschieben.
func TestUpfrontRemuxLeanRuleUnchanged(t *testing.T) {
	for _, height := range []int{720, 1080, 1440, 2160} {
		floor := bitrateFloorKbps(height)
		stats := &VideoStats{Width: height * 16 / 9, Height: height}
		for source := int64(300); source <= 60000; source += 100 {
			oldTarget := max(source*80/100, floor) // der alte Wert ohne Obergrenze
			oldRemux := oldTarget >= source
			newRemux := upfrontRemuxReason(stats, false, source) != ""
			if oldRemux != newRemux {
				t.Fatalf("%dp at %d kbps: old rule remux=%v, new rule remux=%v",
					height, source, oldRemux, newRemux)
			}
		}
	}
}

func TestUpfrontRemuxReason(t *testing.T) {
	cases := []struct {
		name       string
		w, h       int
		doScale    bool
		sourceKbps int64
		wantRemux  bool
		wantWord   string // steht im Grund, damit die Meldung stimmt
	}{
		{"1080p with room", 1920, 1080, false, 8000, false, ""},
		{"1080p at the floor", 1920, 1080, false, 1500, true, "lean"},
		{"1080p just above the floor", 1920, 1080, false, 1501, false, ""},
		{"480p is below 720p", 854, 480, false, 8000, true, "below 720p"},
		{"576p PAL SD is below 720p", 720, 576, false, 6000, true, "below 720p"},
		{"cinemascope 1280x536 counts as 720p", 1280, 536, false, 4000, false, ""},
		{"portrait 720x1280 counts as 720p", 720, 1280, false, 4000, false, ""},
		// Wird verkleinert, wird immer neu kodiert — auch eine magere Quelle.
		{"4K scaled down, lean", 3840, 2160, true, 3000, false, ""},
		// Unbekannte Maße zählen nie als klein.
		{"unknown size", 0, 0, false, 8000, false, ""},
	}
	for _, c := range cases {
		stats := &VideoStats{Width: c.w, Height: c.h}
		reason := upfrontRemuxReason(stats, c.doScale, c.sourceKbps)
		if (reason != "") != c.wantRemux {
			t.Errorf("%s: reason %q, want remux=%v", c.name, reason, c.wantRemux)
		}
		if c.wantWord != "" && !strings.Contains(reason, c.wantWord) {
			t.Errorf("%s: reason %q should mention %q", c.name, reason, c.wantWord)
		}
	}
}

func TestBelow720p(t *testing.T) {
	cases := []struct {
		w, h int
		want bool
	}{
		{1280, 720, false},
		{1279, 719, true},
		{854, 480, true},
		{720, 404, true},
		{1280, 536, false}, // lange Kante reicht
		{960, 720, false},  // kurze Kante reicht
		{480, 854, true},   // hochkant
		{0, 480, false},    // unbekannt
		{-1, -1, false},
	}
	for _, c := range cases {
		if got := below720p(c.w, c.h); got != c.want {
			t.Errorf("below720p(%d, %d) = %v, want %v", c.w, c.h, got, c.want)
		}
	}
}

// TestEncodedFrameSize spiegelt buildVideoFilter: danach entscheidet Auto-CQ,
// ob die VMAF-Messung auf Full-HD-Größe hochgerechnet wird.
func TestEncodedFrameSize(t *testing.T) {
	old := appSettings
	defer func() { appSettings = old }()
	appSettings.maxResolution = 1080

	cases := []struct {
		name         string
		w, h         int
		doScale      bool
		crop         cropRect
		wantW, wantH int
	}{
		{"1080p stays", 1920, 1080, false, cropRect{}, 1920, 1080},
		{"odd edges are trimmed to even", 1281, 721, false, cropRect{}, 1280, 720},
		{"4K scaled into the box", 3840, 2160, true, cropRect{}, 1920, 1080},
		{"4K scope scaled by width", 3840, 1600, true, cropRect{}, 1920, 800},
		{"portrait 4K scaled", 2160, 3840, true, cropRect{}, 1080, 1920},
		{"crop wins", 1920, 1080, false, cropRect{w: 1920, h: 800, x: 0, y: 140}, 1920, 800},
	}
	for _, c := range cases {
		stats := &VideoStats{Width: c.w, Height: c.h}
		w, h := encodedFrameSize(stats, c.doScale, c.crop)
		if w != c.wantW || h != c.wantH {
			t.Errorf("%s: got %dx%d, want %dx%d", c.name, w, h, c.wantW, c.wantH)
		}
	}
}

func TestPrerollInPacketFlags(t *testing.T) {
	cases := []struct {
		name, out string
		want      bool
	}{
		{"plain keyframe start", "K__\n___\n___\n", false},
		{"discarded pre-roll", "K_D\n__D\n___\nK__\n", true},
		{"nothing read", "", false},
	}
	for _, c := range cases {
		if got := prerollInPacketFlags(c.out); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanRenameInPlace(t *testing.T) {
	aac := []AudioStreamInfo{{Codec: "aac", Channels: 2}}
	dts := []AudioStreamInfo{{Codec: "dts", Channels: 6}}
	cases := []struct {
		name    string
		codec   string
		ext     string
		audio   []AudioStreamInfo
		mp4Mode bool
		doScale bool
		want    bool
	}{
		{"AV1 MKV in AV1 mode", "av1", ".mkv", aac, false, false, true},
		{"H.264 MKV is no target file", "h264", ".mkv", aac, false, false, false},
		{"AV1 MP4 must be remuxed", "av1", ".mp4", aac, false, false, false},
		{"sound that needs converting", "av1", ".mkv", dts, false, false, false},
		{"mp4 mode wants an MP4", "av1", ".mkv", aac, true, false, false},
		{"scaled down is re-encoded", "av1", ".mkv", aac, false, true, false},
	}
	for _, c := range cases {
		stats := &VideoStats{VideoCodec: c.codec, AudioStreams: c.audio}
		if got := canRenameInPlace(stats, "av1", c.ext, c.mp4Mode, c.doScale); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestUpperFirst(t *testing.T) {
	for in, want := range map[string]string{
		"source already lean": "Source already lean",
		"über":                "Über",
		"":                    "",
	} {
		if got := upperFirst(in); got != want {
			t.Errorf("upperFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

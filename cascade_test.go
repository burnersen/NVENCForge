//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func labels(at []cascadeAttempt) []string {
	out := make([]string, len(at))
	for i, a := range at {
		out[i] = a.label
	}
	return out
}

func TestBuildCascadeAttempts(t *testing.T) {
	cases := []struct {
		name                        string
		hasSubs, hasAudio, pureCopy bool
		want                        int
	}{
		{"subs+audio", true, true, false, 5},
		{"no subs", false, true, false, 3},
		{"subs+copyaudio", true, true, true, 3},
		{"no subs+copyaudio", false, true, true, 2},
		{"video only source", false, false, false, 1},
		{"subs, no audio", true, false, false, 2},
	}
	for _, c := range cases {
		got := buildCascadeAttempts(c.hasSubs, c.hasAudio, c.pureCopy, "audio 1:1")
		if len(got) != c.want {
			t.Errorf("%s: got %d attempts %v, want %d", c.name, len(got), labels(got), c.want)
		}
		for _, a := range got {
			if a.withSubs && !c.hasSubs {
				t.Errorf("%s: SUBS rung without subtitles in source", c.name)
			}
			if !a.audioCopy && !a.noAudio && c.pureCopy {
				t.Errorf("%s: AAC rung despite -copyaudio", c.name)
			}
		}
		if c.hasAudio && got[len(got)-1].noAudio == false {
			t.Errorf("%s: last rung must be VIDEO-ONLY when audio exists", c.name)
		}
	}
}

// TestKeptAudioText: the "copy" rungs say what really happens to the sound.
// In audioMode=aac they still re-encode every track DaVinci Resolve cannot
// read; until 2.1.0 the log said "audio 1:1" even then.
func TestKeptAudioText(t *testing.T) {
	ac3 := AudioStreamInfo{Codec: "ac3", Layout: "5.1(side)", Channels: 6, SampleRate: 48000}
	aac := AudioStreamInfo{Codec: "aac", Layout: "stereo", Channels: 2, SampleRate: 48000}
	dts := AudioStreamInfo{Codec: "dts", Layout: "5.1(side)", Channels: 6, SampleRate: 48000}
	cases := []struct {
		name     string
		streams  []AudioStreamInfo
		pureCopy bool
		want     string
	}{
		{"AAC stereo only", []AudioStreamInfo{aac}, false, "audio 1:1"},
		{"AC3 in aac mode", []AudioStreamInfo{ac3}, false, "audio → AAC (formats DaVinci Resolve cannot read)"},
		{"AC3 + AAC in aac mode", []AudioStreamInfo{ac3, aac}, false, "audio 1:1 except 1 of 2 tracks → AAC"},
		{"AC3 + DTS + AAC in aac mode", []AudioStreamInfo{ac3, dts, aac}, false, "audio 1:1 except 2 of 3 tracks → AAC"},
		{"AC3 with -copyaudio", []AudioStreamInfo{ac3, dts}, true, "audio 1:1"},
	}
	for _, c := range cases {
		got := keptAudioText(countAACTracks(c.streams, c.pureCopy), len(c.streams))
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// The text must reach both "copy" rungs, not only be computed.
	for _, a := range buildCascadeAttempts(true, true, false, "audio 1:1 except 1 of 2 tracks → AAC") {
		if a.audioCopy && !a.noAudio && !strings.HasSuffix(a.human, "audio 1:1 except 1 of 2 tracks → AAC") {
			t.Errorf("rung %s says %q", a.label, a.human)
		}
	}
}

func TestClassifyFFmpegError(t *testing.T) {
	cases := []struct {
		msg  string
		want ffmpegFailKind
	}{
		{"Subtitle encoding currently only possible from text to text or bitmap to bitmap", failSubtitle},
		{"Error initializing output stream 0:2 -- Error while opening encoder for output stream #0:2 - mov_text", failSubtitle},
		{"OpenEncodeSessionEx failed: out of memory (10): (no details) [hevc_nvenc]", failVideo},
		{"Cannot load nvcuda.dll / no capable devices found", failVideo},
		{"Error while decoding stream #0:1: Invalid data found when processing input [dca @ 0x...]", failAudio},
		{"aac bitstream error, channel element 3.0 is not allocated", failAudio},
		{"Specified channel layout '7.1' is not supported by the aac encoder", failAudio},
		{"Invalid data found when processing input", failUnknown},
		{"", failUnknown},
	}
	for _, c := range cases {
		if got := classifyFFmpegError(c.msg); got != c.want {
			t.Errorf("classify(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}

func TestResetInvalidConfigLines(t *testing.T) {
	// INI with invalid values (targetCQ, maxResolution, nvencPreset), a valid
	// value (casStrength), a comment and an unknown key. Only the invalid
	// lines must change; everything else must stay byte-for-byte.
	ini := "# my notes\n" +
		"targetCQ=77\n" +
		"casStrength=0.2\n" +
		"maxResolution=108\n" +
		"nvencPreset=p8\n" +
		"autoCQ=vielleicht\n" +
		"autoCQTargetVMAF=120\n" +
		"autoCQVMAFPercentile=60\n" +
		"autoCQTargetVMAFPercentile=40\n" +
		"minSavePercent=-1\n" +
		"autoCQPlateauTolerance=11\n" +
		"unknownKey=keepme\n"
	path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
	if err := os.WriteFile(path, []byte(ini), 0644); err != nil {
		t.Fatal(err)
	}

	_, invalids, _ := parseAppConfig(path)
	if len(invalids) != 9 {
		t.Fatalf("got %d invalid settings, want 9 (%v)", len(invalids), invalids)
	}
	if err := resetInvalidConfigLines(path, invalids); err != nil {
		t.Fatalf("reset failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"# my notes",                    // comment untouched
		"targetCQ=26",                   // reset to default
		"casStrength=0.2",               // valid value untouched
		"maxResolution=1080",            // reset to default
		"nvencPreset=p5",                // reset to default
		"autoCQ=true",                   // reset to default
		"autoCQTargetVMAF=95",           // reset to default
		"autoCQVMAFPercentile=5",        // reset to default
		"autoCQTargetVMAFPercentile=92", // reset to default
		"minSavePercent=7",              // reset to default
		"autoCQPlateauTolerance=0.5",    // reset to default
		"unknownKey=keepme",             // unknown key untouched
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config after reset missing %q\n---\n%s", want, got)
		}
	}
	// Line-precise negative check: "maxResolution=1080" must NOT count as a
	// surviving "maxResolution=108".
	lines := strings.Split(strings.ReplaceAll(got, "\r\n", "\n"), "\n")
	hasLine := func(s string) bool {
		for _, l := range lines {
			if l == s {
				return true
			}
		}
		return false
	}
	if hasLine("targetCQ=77") || hasLine("maxResolution=108") ||
		hasLine("nvencPreset=p8") || hasLine("autoCQ=vielleicht") ||
		hasLine("autoCQTargetVMAF=120") || hasLine("minSavePercent=-1") ||
		hasLine("autoCQPlateauTolerance=11") {
		t.Errorf("an invalid value survived the reset:\n%s", got)
	}

	// Second pass: the file is now clean, nothing left to report or rewrite.
	if _, invalids2, _ := parseAppConfig(path); len(invalids2) != 0 {
		t.Errorf("after reset still %d invalids: %v", len(invalids2), invalids2)
	}
}

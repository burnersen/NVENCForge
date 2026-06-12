//go:build windows && amd64

package main

import "testing"

func labels(at []cascadeAttempt) []string {
	out := make([]string, len(at))
	for i, a := range at {
		out[i] = a.label
	}
	return out
}

func TestBuildCascadeAttempts(t *testing.T) {
	cases := []struct {
		name                       string
		hasSubs, hasAudio, pureCopy bool
		want                       int
	}{
		{"subs+audio", true, true, false, 5},
		{"no subs", false, true, false, 3},
		{"subs+copyaudio", true, true, true, 3},
		{"no subs+copyaudio", false, true, true, 2},
		{"video only source", false, false, false, 1},
		{"subs, no audio", true, false, false, 2},
	}
	for _, c := range cases {
		got := buildCascadeAttempts(c.hasSubs, c.hasAudio, c.pureCopy)
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

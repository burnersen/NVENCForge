//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Collision-free output names
//
// These two functions decide whether an unrelated file survives a split. The
// split runs FFmpeg with -y, so handing back a name that is already taken does
// not produce a warning - it destroys the file that was there. Everything in
// this section therefore checks the refusal, not just the happy path.
// ----------------------------------------------------------------------------

// touch creates an empty file, failing the test if that is not possible.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("could not create %s: %v", path, err)
	}
}

func TestUniquePathKeepsFreeName(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "Clip.mkv")

	got, err := uniquePath(want)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("free name was changed: got %q, want %q", got, want)
	}
}

func TestUniquePathNeverReturnsAnExistingFile(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		src      string
		want     string
	}{
		{"name taken once", []string{"Clip.mkv"}, "Clip.mkv", "Clip.2.mkv"},
		{"first alternative taken too", []string{"Clip.mkv", "Clip.2.mkv"}, "Clip.mkv", "Clip.3.mkv"},
		{"gap is reused", []string{"Clip.mkv", "Clip.3.mkv"}, "Clip.mkv", "Clip.2.mkv"},
		{"dots in the stem", []string{"Movie.2024.mkv"}, "Movie.2024.mkv", "Movie.2024.2.mkv"},
		{"no extension at all", []string{"README"}, "README", "README.2"},
	}

	for _, c := range cases {
		dir := t.TempDir()
		for _, e := range c.existing {
			touch(t, filepath.Join(dir, e))
		}

		got, err := uniquePath(filepath.Join(dir, c.src))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != filepath.Join(dir, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, filepath.Base(got), c.want)
			continue
		}
		// The real requirement behind the expected string: whatever comes
		// back must not exist yet.
		if _, err := os.Stat(got); err == nil {
			t.Errorf("%s: returned %q, which already exists - FFmpeg -y would destroy it",
				c.name, filepath.Base(got))
		}
	}
}

func TestUniquePathFailsInsteadOfOverwriting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Clip.mkv")
	touch(t, src)
	for n := 2; n <= 99; n++ {
		touch(t, filepath.Join(dir, fmt.Sprintf("Clip.%d.mkv", n)))
	}

	got, err := uniquePath(src)
	if err == nil {
		t.Fatalf("every candidate is taken, but uniquePath returned %q instead of an error",
			filepath.Base(got))
	}
	if got != "" {
		t.Errorf("on failure the returned path must be empty, got %q", got)
	}
}

func TestUniqueVobSubNeedsBothNamesFree(t *testing.T) {
	// VobSub is two files: FFmpeg writes the .sub next to the .idx it was
	// given. A candidate whose .sub half is occupied is therefore unusable,
	// even when the .idx half looks free.
	cases := []struct {
		name     string
		existing []string
		want     string
	}{
		{"nothing taken", nil, "Subs.idx"},
		{"idx taken", []string{"Subs.idx"}, "Subs.2.idx"},
		{"only the sub half taken", []string{"Subs.sub"}, "Subs.2.idx"},
		{"both halves taken", []string{"Subs.idx", "Subs.sub"}, "Subs.2.idx"},
		{"alternative blocked by its sub half", []string{"Subs.idx", "Subs.2.sub"}, "Subs.3.idx"},
	}

	for _, c := range cases {
		dir := t.TempDir()
		for _, e := range c.existing {
			touch(t, filepath.Join(dir, e))
		}

		got, err := uniqueVobSubPath(filepath.Join(dir, "Subs.idx"))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != filepath.Join(dir, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, filepath.Base(got), c.want)
			continue
		}
		// Both halves of the chosen pair have to be free, not just the one
		// that was asked about.
		subHalf := got[:len(got)-len(".idx")] + ".sub"
		for _, p := range []string{got, subHalf} {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s: chose %q although %q already exists",
					c.name, filepath.Base(got), filepath.Base(p))
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Output names and containers
//
// These are pure functions, but the batch modes depend on two of them agreeing
// with the done-check about the exact name. Drift there means a stack run
// processes files a second time instead of skipping them.
// ----------------------------------------------------------------------------

func TestTrimToolSuffixes(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"nothing to trim", "Movie", "Movie"},
		{"one own suffix", "Movie.h265", "Movie"},
		{"case is ignored", "Movie.H265", "Movie"},
		{"chain from repeated cycles", "Movie.sub.h265.subbed.h265", "Movie"},
		{"numbered subtitle", "Movie.sub2", "Movie"},
		{"joined and remuxed", "Movie.joined.remux", "Movie"},
		{"silent picture", "Movie.NoSound", "Movie"},
		{"a year is not a suffix", "Movie.2024", "Movie.2024"},
		{"sub with trailing letters stays", "Movie.subx", "Movie.subx"},
		{"leading dot is not a suffix", ".h265", ".h265"},
		{"empty input", "", ""},
	}
	for _, c := range cases {
		if got := trimToolSuffixes(c.in); got != c.want {
			t.Errorf("%s: trimToolSuffixes(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestLosslessVideoContainerExt(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"mp4 stays mp4", "Clip.mp4", ".mp4"},
		{"m4v is mp4 family", "Clip.m4v", ".mp4"},
		{"mov is mp4 family", "Clip.mov", ".mp4"},
		{"uppercase is handled", "Clip.MOV", ".mp4"},
		// Transport streams must NOT go to MKV: their timestamps are
		// discontinuous and the last packet often has none at all, which
		// Matroska rejects outright ("unknown timestamp") and the silent
		// picture step then fails.
		{"ts goes to mp4", "Clip.ts", ".mp4"},
		{"m2ts goes to mp4", "Clip.m2ts", ".mp4"},
		{"mts goes to mp4", "Clip.mts", ".mp4"},
		{"m2t goes to mp4", "Clip.m2t", ".mp4"},
		{"mkv stays mkv", "Clip.mkv", ".mkv"},
		{"webm goes to mkv", "Clip.webm", ".mkv"},
		{"avi goes to mkv", "Clip.avi", ".mkv"},
		{"unknown goes to mkv", "Clip.xyz", ".mkv"},
		{"no extension goes to mkv", "Clip", ".mkv"},
	}
	for _, c := range cases {
		if got := losslessVideoContainerExt(c.in); got != c.want {
			t.Errorf("%s: losslessVideoContainerExt(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestLosslessAudioExt(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"aac", "aac", "m4a"},
		{"ac3", "ac3", "ac3"},
		{"eac3", "eac3", "eac3"},
		{"dts", "dts", "dts"},
		{"truehd", "truehd", "thd"},
		{"mlp is truehd", "mlp", "thd"},
		{"flac", "flac", "flac"},
		{"opus", "opus", "opus"},
		{"vorbis lands in ogg", "vorbis", "ogg"},
		{"mp3", "mp3", "mp3"},
		{"pcm little endian", "pcm_s16le", "wav"},
		{"pcm big endian", "pcm_s24be", "wav"},
		{"pcm a-law", "pcm_alaw", "wav"},
		{"uppercase is handled", "AC3", "ac3"},
		{"surrounding space is handled", "  flac  ", "flac"},
		{"unknown codec lands in mka", "some_new_codec", "mka"},
		{"empty lands in mka", "", "mka"},
	}
	for _, c := range cases {
		if got := losslessAudioExt(c.in); got != c.want {
			t.Errorf("%s: losslessAudioExt(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestSplitOutputNamesStayPutAndDoNotGrow(t *testing.T) {
	const dir = `C:\videos`
	cases := []struct {
		name    string
		src     string
		noSound string
		davinci string
	}{
		{"plain source", "Movie.mkv", "Movie.NoSound.mkv", "Movie.NoSound.mp4"},
		{"own suffix is stripped first", "Movie.h265.mkv", "Movie.NoSound.mkv", "Movie.NoSound.mp4"},
		{"a year survives", "Movie.2024.mkv", "Movie.2024.NoSound.mkv", "Movie.2024.NoSound.mp4"},
		{"container choice is followed", "Movie.ts", "Movie.NoSound.mp4", "Movie.NoSound.mp4"},
		// The important one: running the name through a second time must
		// return the same name. Otherwise the batch done-check looks for
		// Movie.NoSound.NoSound.mkv, never finds it, and converts the file
		// again on every run.
		{"already a NoSound name", "Movie.NoSound.mkv", "Movie.NoSound.mkv", "Movie.NoSound.mp4"},
	}
	for _, c := range cases {
		src := filepath.Join(dir, c.src)

		gotNo := splitNoSoundOutPath(src)
		if filepath.Dir(gotNo) != dir {
			t.Errorf("%s: silent picture left the source folder: %q", c.name, gotNo)
		}
		if filepath.Base(gotNo) != c.noSound {
			t.Errorf("%s: splitNoSoundOutPath = %q, want %q", c.name, filepath.Base(gotNo), c.noSound)
		}

		gotDa := splitVideoOutPath(src)
		if filepath.Dir(gotDa) != dir {
			t.Errorf("%s: DaVinci output left the source folder: %q", c.name, gotDa)
		}
		if filepath.Base(gotDa) != c.davinci {
			t.Errorf("%s: splitVideoOutPath = %q, want %q", c.name, filepath.Base(gotDa), c.davinci)
		}
		if !strings.HasSuffix(gotDa, ".mp4") {
			t.Errorf("%s: the DaVinci path must always be MP4, got %q", c.name, gotDa)
		}
	}
}

func TestSplitNoSoundNameIsStable(t *testing.T) {
	// Feeding an output name back in must be a fixed point, whatever the
	// container. This is what keeps a repeated batch run from redoing work.
	for _, src := range []string{
		`C:\videos\Movie.mkv`, `C:\videos\Movie.ts`, `C:\videos\Movie.mp4`,
		`C:\videos\My Movie (2024) [1080p].mkv`, `C:\videos\Movie.h265.mkv`,
	} {
		once := splitNoSoundOutPath(src)
		twice := splitNoSoundOutPath(once)
		if once != twice {
			t.Errorf("name grows on repeat: %q -> %q -> %q", filepath.Base(src),
				filepath.Base(once), filepath.Base(twice))
		}
	}
}

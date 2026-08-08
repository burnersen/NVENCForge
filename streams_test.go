//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
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

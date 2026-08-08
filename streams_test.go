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

// ----------------------------------------------------------------------------
// Subtitle handling
//
// Pure text work, so everything here runs without FFmpeg. The parser is a
// state machine with a blank-line lookahead, which is where subtitles with
// empty lines inside a block get lost if it goes wrong.
// ----------------------------------------------------------------------------

func TestSRTNormalize(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"windows line endings", "a\r\nb", "a\nb"},
		{"old mac line endings", "a\rb", "a\nb"},
		{"byte order mark is dropped", "\xEF\xBB\xBF1", "1"},
		{"bom only at the start", "a\xEF\xBB\xBFb", "a\xEF\xBB\xBFb"},
		{"already clean", "a\nb", "a\nb"},
	}
	for _, c := range cases {
		if got := srtNormalize(c.in); got != c.want {
			t.Errorf("%s: srtNormalize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestSRTTimeRoundTrip(t *testing.T) {
	for _, ts := range []string{"00:00:00,000", "00:00:01,500", "01:23:45,678", "99:59:59,999"} {
		if got := srtFormatTimeMS(srtParseTimeMS(ts)); got != ts {
			t.Errorf("round trip of %q produced %q", ts, got)
		}
	}
	// A dot instead of a comma is accepted on the way in and normalised on
	// the way out - some tools write SRT that way.
	if got := srtFormatTimeMS(srtParseTimeMS("00:00:02.250")); got != "00:00:02,250" {
		t.Errorf("dotted timestamp became %q, want %q", got, "00:00:02,250")
	}
}

func TestSRTTimestampPatternGuardsTheParser(t *testing.T) {
	// srtParseTimeMS ignores whether Sscanf actually matched, so malformed
	// input silently becomes 0 - a subtitle jumping to the start of the film
	// with no error anywhere. That is only safe because every caller passes a
	// capture group of srtTSRegex. This test holds the two halves together:
	// loosen the pattern and this fails here, instead of in a subtitle file.
	//
	// Note the whole LINE is tested, not a bare timestamp: the pattern spans
	// both stamps and the arrow, so a lone timestamp never matches it and
	// would prove nothing.
	for _, bad := range []string{
		"1:2:3,4 --> 5:6:7,8",
		"0:00:01,000 --> 0:00:02,000",
		"00:00:01 --> 00:00:02",
		"00:00:01.5 --> 00:00:02.5",
		"00:00:01,00 --> 00:00:02,00",
		"garbage --> nonsense",
		"",
	} {
		if srtTSRegex.MatchString(bad) {
			t.Errorf("pattern accepts %q - srtParseTimeMS would then take it unchecked", bad)
		}
	}

	// The other half of the guarantee: whatever the pattern DOES accept has
	// to survive srtParseTimeMS unchanged. This is what the ignored error
	// code leans on.
	for _, good := range []string{
		"00:00:01,000 --> 00:00:02,000",
		"01:23:45,678 --> 99:59:59,999",
		"00:00:01.500 --> 00:00:02.500",
	} {
		m := srtTSRegex.FindStringSubmatch(good)
		if m == nil {
			t.Errorf("pattern rejects the valid line %q", good)
			continue
		}
		for _, ts := range m[1:] {
			want := strings.ReplaceAll(ts, ".", ",")
			if got := srtFormatTimeMS(srtParseTimeMS(ts)); got != want {
				t.Errorf("%q: capture %q came back as %q - malformed input is reaching the parser",
					good, ts, got)
			}
		}
	}

	if got := srtParseTimeMS("garbage"); got != 0 {
		t.Errorf("unparsable input now returns %d instead of 0 - the assumption above changed", got)
	}
}

func TestSRTParse(t *testing.T) {
	content := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n" +
		"2\n00:00:03,000 --> 00:00:04,500\nLine one\nLine two\n"

	blocks, malformed := srtParse(content)
	if malformed != 0 {
		t.Errorf("clean input reported %d malformed blocks", malformed)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Text != "Hello" {
		t.Errorf("first text = %q, want %q", blocks[0].Text, "Hello")
	}
	if blocks[1].Text != "Line one\nLine two" {
		t.Errorf("multi-line text = %q", blocks[1].Text)
	}
	if blocks[1].StartMS != 3000 || blocks[1].EndMS != 4500 {
		t.Errorf("second block times = %d..%d, want 3000..4500", blocks[1].StartMS, blocks[1].EndMS)
	}
	if blocks[0].Number != 1 || blocks[1].Number != 2 {
		t.Errorf("numbers = %d,%d, want 1,2", blocks[0].Number, blocks[1].Number)
	}
}

func TestSRTParseKeepsBlankLineInsideABlock(t *testing.T) {
	// The blank line here belongs to the subtitle, not between blocks. Losing
	// the lookahead would end the block early and drop "Line B".
	content := "1\n00:00:01,000 --> 00:00:02,000\nLine A\n\nLine B\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nNext\n"

	blocks, _ := srtParse(content)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if !strings.Contains(blocks[0].Text, "Line B") {
		t.Errorf("text after the inner blank line was dropped: %q", blocks[0].Text)
	}
}

func TestSRTParseCountsMissingNumbers(t *testing.T) {
	// A block that starts straight with the timestamp is malformed but must
	// still survive - and get a number of its own.
	content := "00:00:01,000 --> 00:00:02,000\nNo number above me\n"

	blocks, malformed := srtParse(content)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if malformed == 0 {
		t.Error("missing block number was not counted as malformed")
	}
	if blocks[0].Number != 1 {
		t.Errorf("block got number %d, want 1", blocks[0].Number)
	}
}

func TestSRTFilter(t *testing.T) {
	blocks := []srtBlock{
		{Number: 1, Text: "<i>Italic</i> text"},
		{Number: 2, Text: "{\an8}Positioned"},
		{Number: 3, Text: "Ampersand &amp; entity"},
		{Number: 4, Text: "   "},
		{Number: 5, Text: "Subtitles by someone"},
		{Number: 6, Text: "advert"},
		{Number: 7, Text: "advert in a sentence"},
		{Number: 8, Text: "Invisible\u200Bcharacter"},
	}
	phrases := []string{"subtitles by", "=advert"}

	clean, removed := srtFilter(blocks, phrases)

	want := []string{"Italic text", "Positioned", "Ampersand & entity",
		"advert in a sentence", "Invisiblecharacter"}
	if len(clean) != len(want) {
		t.Fatalf("got %d blocks, want %d: %+v", len(clean), len(want), clean)
	}
	for i, w := range want {
		if clean[i].Text != w {
			t.Errorf("block %d text = %q, want %q", i, clean[i].Text, w)
		}
		// Survivors are renumbered from 1 without gaps, otherwise players
		// stumble over the sequence.
		if clean[i].Number != i+1 {
			t.Errorf("block %d has number %d, want %d", i, clean[i].Number, i+1)
		}
	}
	if removed != 3 {
		t.Errorf("removed %d blocks, want 3 (empty, phrase match, exact match)", removed)
	}
}

func TestSRTApplyMicroGaps(t *testing.T) {
	blocks := []srtBlock{
		{StartMS: 0, EndMS: 1000, Timestamp: "00:00:00,000 --> 00:00:01,000"},
		{StartMS: 1000, EndMS: 2000, Timestamp: "00:00:01,000 --> 00:00:02,000"},
		{StartMS: 3000, EndMS: 4000, Timestamp: "00:00:03,000 --> 00:00:04,000"},
	}
	srtApplyMicroGaps(blocks)

	if blocks[1].StartMS != 1001 {
		t.Errorf("touching block starts at %d, want 1001", blocks[1].StartMS)
	}
	// The printed timestamp has to move with the value, or the file says
	// something different from the parsed data.
	if blocks[1].Timestamp != "00:00:01,001 --> 00:00:02,000" {
		t.Errorf("timestamp text was not updated: %q", blocks[1].Timestamp)
	}
	if blocks[2].StartMS != 3000 {
		t.Errorf("block with a real gap was moved to %d", blocks[2].StartMS)
	}
	if blocks[0].StartMS != 0 {
		t.Errorf("first block was moved to %d", blocks[0].StartMS)
	}
}

func TestLoadOrCreateSRTConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SRTCleaner_config.txt")

	phrases := loadOrCreateSRTConfig(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config was not created: %v", err)
	}
	if len(phrases) == 0 {
		t.Fatal("freshly created config yielded no phrases")
	}
	for _, p := range phrases {
		if strings.HasPrefix(p, "#") {
			t.Errorf("comment line leaked into the phrase list: %q", p)
		}
		if p != strings.ToLower(p) {
			t.Errorf("phrase is not lower-cased: %q", p)
		}
		if strings.TrimSpace(p) == "" {
			t.Error("blank line leaked into the phrase list")
		}
	}
}

func TestLoadOrCreateSRTConfigKeepsAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SRTCleaner_config.txt")
	own := "# my own list\n\nOnlyThis\n"
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	phrases := loadOrCreateSRTConfig(path)
	if len(phrases) != 1 || phrases[0] != "onlythis" {
		t.Errorf("existing config was not used as-is: %v", phrases)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != own {
		t.Error("an existing config file must never be rewritten")
	}
}

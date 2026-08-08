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

// ----------------------------------------------------------------------------
// Sorting the dropped files, and the small helpers around them
// ----------------------------------------------------------------------------

func TestCategorizeArgs(t *testing.T) {
	args := []string{
		`C:\v\Movie.mkv`, `C:\v\Clip.MP4`, `C:\v\Clip.m4v`, `C:\v\Clip.mov`,
		`C:\v\Subs.srt`, `C:\v\Subs.sup`, `C:\v\Subs.ass`, `C:\v\Subs.ssa`, `C:\v\Subs.vtt`,
		`C:\v\Track.ac3`, `C:\v\Track.flac`, `C:\v\Track.mka`,
		`C:\v\Notes.txt`, `C:\v\Cover.jpg`,
	}
	mkvs, mp4s, audios, srts, others := categorizeArgs(args)

	if len(mkvs) != 1 {
		t.Errorf("mkvs = %v, want 1 entry", mkvs)
	}
	if len(mp4s) != 3 {
		t.Errorf("mp4s = %v, want 3 entries (mp4/m4v/mov, upper case included)", mp4s)
	}
	if len(srts) != 5 {
		t.Errorf("subtitles = %v, want 5 entries", srts)
	}
	if len(audios) != 3 {
		t.Errorf("audios = %v, want 3 entries", audios)
	}
	if len(others) != 2 {
		t.Errorf("others = %v, want 2 entries", others)
	}
}

func TestCategorizeArgsPairsVobSub(t *testing.T) {
	// A .sub belongs to its .idx and must not be handed over separately -
	// FFmpeg picks it up through the .idx on its own. Without a matching
	// .idx, though, it is a stray file and belongs in "others" rather than
	// being silently swallowed. (The comment in the code calls this SUB-02.)
	cases := []struct {
		name      string
		args      []string
		wantSrts  int
		wantOther int
	}{
		{"pair stays together", []string{`C:\v\S.idx`, `C:\v\S.sub`}, 1, 0},
		{"orphaned sub is not swallowed", []string{`C:\v\S.sub`}, 0, 1},
		{"pairing ignores case", []string{`C:\v\S.IDX`, `C:\v\S.SUB`}, 1, 0},
		{"different stem does not pair", []string{`C:\v\A.idx`, `C:\v\B.sub`}, 1, 1},
	}
	for _, c := range cases {
		_, _, _, srts, others := categorizeArgs(c.args)
		if len(srts) != c.wantSrts {
			t.Errorf("%s: subtitles = %v, want %d", c.name, srts, c.wantSrts)
		}
		if len(others) != c.wantOther {
			t.Errorf("%s: others = %v, want %d", c.name, others, c.wantOther)
		}
	}
}

func TestLangHelpers(t *testing.T) {
	langCases := []struct{ in, wantCode, wantName string }{
		{"de", "ger", "German"},
		{"deu", "ger", "German"},
		{"ger", "ger", "German"},
		{"EN", "eng", "English"},
		{"  fr  ", "fra", "French"},
		{"zzz", "und", ""},
		{"", "und", ""},
	}
	for _, c := range langCases {
		if got := normalizeLang(c.in); got != c.wantCode {
			t.Errorf("normalizeLang(%q) = %q, want %q", c.in, got, c.wantCode)
		}
		if got := langDisplayName(c.in); got != c.wantName {
			t.Errorf("langDisplayName(%q) = %q, want %q", c.in, got, c.wantName)
		}
	}
}

func TestParseSubTags(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		wantLang   string
		wantForced bool
		wantSDH    bool
	}{
		{"plain language", `C:\v\Movie.de.srt`, "ger", false, false},
		{"three letter code", `C:\v\Movie.eng.srt`, "eng", false, false},
		{"forced flag", `C:\v\Movie.de.forced.srt`, "ger", true, false},
		{"sdh flag", `C:\v\Movie.en.sdh.srt`, "eng", false, true},
		{"numbered track", `C:\v\Movie.de.2.srt`, "ger", false, false},
		{"stereo suffix", `C:\v\Movie.de.stereo.m4a`, "ger", false, false},
		{"no language at all", `C:\v\Movie.srt`, "und", false, false},
		{"unknown language code", `C:\v\Movie.zz.srt`, "und", false, false},
		{"long word is not a code", `C:\v\My.movie.srt`, "und", false, false},
	}
	for _, c := range cases {
		lang, forced, sdh := parseSubTags(c.file)
		if lang != c.wantLang {
			t.Errorf("%s: lang = %q, want %q", c.name, lang, c.wantLang)
		}
		if forced != c.wantForced {
			t.Errorf("%s: forced = %v, want %v", c.name, forced, c.wantForced)
		}
		if sdh != c.wantSDH {
			t.Errorf("%s: sdh = %v, want %v", c.name, sdh, c.wantSDH)
		}
	}
}

func TestTailBufferKeepsOnlyTheTail(t *testing.T) {
	tb := &tailBuffer{max: 10}

	// The io.Writer contract matters here: exec.Cmd treats a short write as a
	// failure and kills the pipe, so Write must always report the full length.
	writes := []string{"abcde", "fghij", "klmno"}
	for _, w := range writes {
		n, err := tb.Write([]byte(w))
		if n != len(w) || err != nil {
			t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", w, n, err, len(w))
		}
	}
	if got := tb.String(); got != "fghijklmno" {
		t.Errorf("buffer = %q, want %q", got, "fghijklmno")
	}
	if len(tb.buf) > tb.max {
		t.Errorf("buffer grew past its cap: %d bytes", len(tb.buf))
	}

	// A single write larger than the cap keeps its tail, not its head.
	tb2 := &tailBuffer{max: 4}
	if _, err := tb2.Write([]byte("0123456789")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := tb2.String(); got != "6789" {
		t.Errorf("oversized write kept %q, want %q", got, "6789")
	}
}

func TestLastErrorLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"single line", "boom", "boom"},
		{"last non-empty wins", "first\nsecond\nthird", "third"},
		{"trailing blanks are skipped", "real error\n\n   \n", "real error"},
		{"empty input", "", ""},
		{"only whitespace", "   \n\t\n", ""},
		{"surrounding space is trimmed", "  padded  ", "padded"},
	}
	for _, c := range cases {
		if got := lastErrorLine(c.in); got != c.want {
			t.Errorf("%s: lastErrorLine(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	long := strings.Repeat("x", 250)
	got := lastErrorLine(long)
	if len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Errorf("overlong line = %d chars ending %q, want 203 chars ending in an ellipsis",
			len(got), got[max(0, len(got)-3):])
	}
}

func TestEstimateAudioTrackMB(t *testing.T) {
	const dur = 3600.0 // one hour

	// A reported bitrate always wins over the per-codec guess.
	real := estimateAudioTrackMB(ffprobeStream{BitRate: "128000", CodecName: "truehd"}, dur)
	if want := 128000.0 * dur / 8 / (1024 * 1024); real != want {
		t.Errorf("reported bitrate ignored: got %.3f, want %.3f", real, want)
	}

	cases := []struct {
		name  string
		codec string
		ch    int
		wantK float64
	}{
		{"ac3", "ac3", 6, 384},
		{"eac3", "eac3", 6, 256},
		{"dts", "dts", 6, 1024},
		{"truehd", "truehd", 8, 3000},
		{"flac scales with channels", "flac", 2, 800},
		{"pcm scales with channels", "pcm_s16le", 2, 1536},
		{"unknown codec falls back", "brandnew", 6, 576},
		{"missing channel count assumes stereo", "brandnew", 0, 192},
	}
	for _, c := range cases {
		got := estimateAudioTrackMB(ffprobeStream{CodecName: c.codec, Channels: c.ch}, dur)
		want := c.wantK * 1000 * dur / 8 / (1024 * 1024)
		if got != want {
			t.Errorf("%s: got %.3f MB, want %.3f MB", c.name, got, want)
		}
	}

	// No duration means no estimate, not a negative or absurd number.
	for _, d := range []float64{0, -1} {
		if got := estimateAudioTrackMB(ffprobeStream{CodecName: "ac3", Channels: 6}, d); got != 0 {
			t.Errorf("duration %v produced %.3f, want 0", d, got)
		}
	}
}

//go:build windows && amd64

// Streams.go — MKV ↔ MP4/Audio/Subs Stream-Splitter und Merger

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/pterm/pterm"
)

// ----------------------------------------------------------------------------
// Language code mapping
// ----------------------------------------------------------------------------

var langMap = map[string]string{
	"de": "ger", "deu": "ger", "ger": "ger",
	"en": "eng", "eng": "eng",
	"fr": "fra", "fra": "fra", "fre": "fra",
	"es": "spa", "spa": "spa",
	"it": "ita", "ita": "ita",
	"ja": "jpn", "jpn": "jpn",
	"ko": "kor", "kor": "kor",
	"zh": "chi", "chi": "chi", "zho": "chi",
	"pt": "por", "por": "por",
	"ru": "rus", "rus": "rus",
	"nl": "dut", "nld": "dut", "dut": "dut",
	"pl": "pol", "pol": "pol",
	"sv": "swe", "swe": "swe",
	"da": "dan", "dan": "dan",
	"no": "nor", "nor": "nor",
	"fi": "fin", "fin": "fin",
	"tr": "tur", "tur": "tur",
	"ar": "ara", "ara": "ara",
	"cs": "cze", "ces": "cze", "cze": "cze",
	"el": "gre", "ell": "gre", "gre": "gre",
	"hu": "hun", "hun": "hun",
	"ro": "rum", "ron": "rum", "rum": "rum",
	"th": "tha", "tha": "tha",
	"uk": "ukr", "ukr": "ukr",
	"hi": "hin", "hin": "hin",
	"bg": "bul", "bul": "bul",
	"hr": "hrv", "hrv": "hrv",
	"sk": "slo", "slk": "slo", "slo": "slo",
	"sl": "slv", "slv": "slv",
	"sr": "srp", "srp": "srp",
	"und": "und",
}

var langDisplay = map[string]string{
	"de": "German", "deu": "German", "ger": "German",
	"en": "English", "eng": "English",
	"fr": "French", "fra": "French", "fre": "French",
	"es": "Spanish", "spa": "Spanish",
	"it": "Italian", "ita": "Italian",
	"ja": "Japanese", "jpn": "Japanese",
	"ko": "Korean", "kor": "Korean",
	"zh": "Chinese", "chi": "Chinese", "zho": "Chinese",
	"pt": "Portuguese", "por": "Portuguese",
	"ru": "Russian", "rus": "Russian",
	"nl": "Dutch", "nld": "Dutch", "dut": "Dutch",
	"pl": "Polish", "pol": "Polish",
	"sv": "Swedish", "swe": "Swedish",
	"da": "Danish", "dan": "Danish",
	"no": "Norwegian", "nor": "Norwegian",
	"fi": "Finnish", "fin": "Finnish",
	"tr": "Turkish", "tur": "Turkish",
	"ar": "Arabic", "ara": "Arabic",
	"cs": "Czech", "ces": "Czech", "cze": "Czech",
	"el": "Greek", "ell": "Greek", "gre": "Greek",
	"hu": "Hungarian", "hun": "Hungarian",
	"ro": "Romanian", "ron": "Romanian", "rum": "Romanian",
	"th": "Thai", "tha": "Thai",
	"uk": "Ukrainian", "ukr": "Ukrainian",
	"hi": "Hindi", "hin": "Hindi",
	"bg": "Bulgarian", "bul": "Bulgarian",
	"hr": "Croatian", "hrv": "Croatian",
	"sk": "Slovak", "slk": "Slovak", "slo": "Slovak",
	"sl": "Slovenian", "slv": "Slovenian",
	"sr": "Serbian", "srp": "Serbian",
}

func normalizeLang(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if v, ok := langMap[code]; ok {
		return v
	}
	return "und"
}

func langDisplayName(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if v, ok := langDisplay[code]; ok {
		return v
	}
	return ""
}

// ----------------------------------------------------------------------------
// Audio adapter to NVENCForge logic
// ----------------------------------------------------------------------------

func printDavinciAudioInfo(infos []AudioStreamInfo, pureCopy bool) {
	for i, s := range infos {
		var langPart string
		if s.Language != "" {
			if name := langDisplayName(s.Language); name != "" {
				langPart = name + " / "
			} else {
				langPart = strings.ToUpper(strings.TrimSpace(s.Language)) + " / "
			}
		}
		codec := strings.ToUpper(s.Codec)
		var davinciTrack string
		switch s.Channels {
		case 1:
			davinciTrack = "Mono"
		case 2:
			davinciTrack = "Stereo"
		case 6:
			davinciTrack = "5.1"
		case 8:
			davinciTrack = "7.1"
		default:
			davinciTrack = fmt.Sprintf("%d channels", s.Channels)
		}
		// >6 channels are downmixed to 5.1 on AAC re-encode (DaVinci cannot
		// decode 8ch AAC) — show the track that actually lands in Resolve.
		if !pureCopy && s.Channels > davinciMaxAudioChannels &&
			needsAudioReencode(s.Codec, s.Layout, s.Channels, s.SampleRate) {
			davinciTrack = fmt.Sprintf("5.1 (downmix from %s)", davinciTrack)
		}
		// With -copyaudio an incompatible track is kept 1:1 — Resolve will not
		// read it at all, so it must not be announced as a "DaVinci Track".
		label := "→ DaVinci Track:"
		value := pterm.LightBlue(davinciTrack)
		if pureCopy && needsAudioReencode(s.Codec, s.Layout, s.Channels, s.SampleRate) {
			label = "→ Copy 1:1:"
			value = pterm.LightYellow(davinciTrack + " (not readable in DaVinci)")
		}
		fmt.Printf("  %s %s %s %s\n",
			pterm.Gray(fmt.Sprintf("Audio %d", i+1)),
			pterm.LightWhite(fmt.Sprintf("(%s%s)", langPart, codec)),
			pterm.Gray(label),
			value,
		)
	}
}

// ----------------------------------------------------------------------------
// FFmpeg/FFprobe wrappers (no progress display)
// ----------------------------------------------------------------------------

func runFFmpegSub(ctx context.Context, args []string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	args = append([]string{"-v", "warning", "-nostats"}, args...)
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP |
			winCREATE_NO_WINDOW | winIDLE_PRIORITY_CLASS,
	}
	// Bounded buffer: FFmpeg can emit warnings endlessly on badly damaged
	// input; an unbounded buffer would exhaust memory, while only the last
	// line is ever reported (see lastErrorLine).
	stderr := &tailBuffer{max: 64 * 1024}
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("Streams.go: runFFmpegSub (StdinPipe): %w", err)
	}
	defer stdin.Close()

	cmd.Cancel = func() error {
		_, werr := stdin.Write([]byte("q\n"))
		return werr
	}
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := lastErrorLine(stderr.String())
		if msg != "" {
			return fmt.Errorf("Streams.go: runFFmpegSub: %w – %s", err, msg)
		}
		return fmt.Errorf("Streams.go: runFFmpegSub: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// tailBuffer is an io.Writer that keeps only the last max bytes written,
// so stderr collection stays at a fixed memory cap no matter how much
// output the process produces.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	if drop := len(t.buf) + n - t.max; drop > 0 {
		t.buf = append(t.buf[:0], t.buf[drop:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

func lastErrorLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if len(l) > 200 {
			l = l[:200] + "..."
		}
		return l
	}
	return ""
}

func probeStreams(ctx context.Context, file string) (*ffprobeOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", file)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("Streams.go: probeStreams: FFprobe timeout after 30s: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			msg := lastErrorLine(string(exitErr.Stderr))
			if msg != "" {
				return nil, fmt.Errorf("Streams.go: probeStreams: %w – %s", err, msg)
			}
		}
		return nil, fmt.Errorf("Streams.go: probeStreams: %w", err)
	}
	var p ffprobeOutput
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("Streams.go: probeStreams: JSON parse error: %w", err)
	}
	return &p, nil
}

// ----------------------------------------------------------------------------
// Entry point and argument categorisation
// ----------------------------------------------------------------------------

func runDavinciMode(ctx context.Context, args []string) {
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("For DaVinci Resolve Workflow  (Audio / Subs / Split / Merge)")
	fmt.Println()

	// No files: batch mode — split every MKV in the current folder. MP4/MOV
	// sources are deliberately excluded here: the split result is itself a
	// plain .mp4, so a second batch run would consume its own outputs.
	if len(args) == 0 {
		runBatchSplit(ctx)
		return
	}

	mkvs, mp4s, audios, srts, others := categorizeArgs(args)

	switch {
	case len(others) > 0:
		pErr.Printf("Unknown file types: %v\n", others)
		fmt.Println()
		showUsage()

	case len(mkvs) > 0 && len(mp4s) == 0 && len(audios) == 0 && len(srts) == 0:
		runDemuxFromMKV(ctx, mkvs)

	case len(mp4s) > 0 && len(mkvs) == 0 && len(audios) == 0 && len(srts) == 0:
		runExtractFromMP4(ctx, mp4s)

	case len(mp4s) == 1 && len(mkvs) == 0 && (len(audios) > 0 || len(srts) > 0):
		runMerge(ctx, mp4s[0], audios, srts)

	case len(mkvs) == 1 && len(mp4s) == 0 && (len(audios) > 0 || len(srts) > 0):
		runMerge(ctx, mkvs[0], audios, srts)

	case len(mp4s)+len(mkvs) > 1:
		pErr.Println("Please drop only ONE video file (MP4 or MKV) together with the audio/SRT files.")
		fmt.Println()
		showUsage()

	case (len(mp4s) == 1 || len(mkvs) == 1) && len(audios) == 0 && len(srts) == 0:
		pErr.Println("Please drop at least one audio or SRT file in addition to the video.")
		fmt.Println()
		showUsage()

	default:
		pErr.Println("Invalid file combination.")
		fmt.Println()
		showUsage()
	}
}

var audioExtensions = map[string]bool{
	".m4a": true, ".aac": true, ".mp3": true, ".wav": true,
	".ac3": true, ".eac3": true, ".ec3": true, ".dts": true,
	".flac": true, ".opus": true, ".ogg": true,
	".mka": true, ".thd": true,
}

// categorizeArgs sorts drag-and-drop arguments by type.
// FIX SUB-02: orphaned .sub files without a matching .idx → "others".
func categorizeArgs(args []string) (mkvs, mp4s, audios, srts, others []string) {
	idxStems := map[string]bool{}
	for _, a := range args {
		if strings.ToLower(filepath.Ext(a)) == ".idx" {
			stem := strings.TrimSuffix(strings.ToLower(filepath.Base(a)), ".idx")
			idxStems[stem] = true
		}
	}

	for _, a := range args {
		ext := strings.ToLower(filepath.Ext(a))
		switch {
		case ext == ".mkv":
			mkvs = append(mkvs, a)
		case ext == ".mp4" || ext == ".m4v" || ext == ".mov":
			mp4s = append(mp4s, a)
		case ext == ".srt" || ext == ".sup" || ext == ".idx" ||
			ext == ".ass" || ext == ".ssa" || ext == ".vtt":
			srts = append(srts, a)
		case ext == ".sub":
			stem := strings.TrimSuffix(strings.ToLower(filepath.Base(a)), ".sub")
			if idxStems[stem] {
				continue
			}
			others = append(others, a)
		case audioExtensions[ext]:
			audios = append(audios, a)
		default:
			others = append(others, a)
		}
	}
	return
}

func showUsage() {
	pInfo.Println("Usage (drag-and-drop or command line with -davinci):")
	fmt.Println()
	fmt.Printf("  %s\n", pterm.LightWhite("1) Drop one or more MKV files (split)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .NoSound.mp4 (video-only, stream copy)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.m4a / .mp3 / .wav (audio separate, DaVinci-ready)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.stereo.m4a (optional stereo downmix — own number in the track selection)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.srt / .sup / .idx (subtitles)"))
	fmt.Println()
	fmt.Printf("  %s\n", pterm.LightWhite("1b) Drop one or more MP4/MOV/M4V files (extract)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .NoSound.mp4 (video-only, stream copy)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.m4a / .mp3 / .wav (audio separate)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.srt (subtitles, incl. mov_text/tx3g)"))
	fmt.Println()
	fmt.Printf("  %s\n", pterm.LightWhite("2) Drop a base video (.mp4 OR .mkv) + audio/sub files (merge → MKV)"))
	fmt.Printf("     %s\n", pterm.Gray("→ creates .sub.mkv with EXACTLY the provided streams"))
	fmt.Printf("     %s\n", pterm.Gray("→ the base video contributes its picture only (embedded audio/subs are dropped)"))
	fmt.Printf("     %s\n", pterm.Gray("→ accepted audio extensions: .m4a .aac .mp3 .wav .ac3 .eac3 .ec3 .dts .flac .opus .ogg"))
	fmt.Printf("     %s\n", pterm.Gray("→ accepted sub extensions:    .srt .sup .idx .ass .ssa .vtt"))
	fmt.Printf("     %s\n", pterm.Gray("→ .sub (VobSub companion file) only drop together with .idx"))
	fmt.Println()
	fmt.Printf("  %s\n", pterm.LightWhite("3) No files: split EVERY MKV in the current folder (batch)"))
	fmt.Printf("     %s\n", pterm.Gray("→ all tracks, no stereo mixes, no questions asked"))
	fmt.Printf("     %s\n", pterm.Gray("→ several instances may run in parallel (per-file locks share the work)"))
	fmt.Println()
	fmt.Printf("  %s\n", pterm.Gray("Language is detected from the file name:"))
	fmt.Printf("  %s\n", pterm.Gray("  Movie.de.m4a → German (ger),  Movie.en.srt → English (eng)"))
	fmt.Printf("  %s\n", pterm.Gray("  Without a language tag in the name, 'und' (unknown) is set."))
	fmt.Println()
}

// ----------------------------------------------------------------------------
// Base name handling for the DaVinci Resolve workflow
// ----------------------------------------------------------------------------

// trimToolSuffixes removes trailing suffixes NVENCForge itself appends
// (.sub/.subN/.subbed/.h265/.remux/.video/.preview) so output names do not
// grow longer with every split/merge cycle
// ("Movie.sub.h265.subbed.h265" → "Movie").
func trimToolSuffixes(base string) string {
	for {
		idx := strings.LastIndex(base, ".")
		if idx <= 0 {
			return base
		}
		tok := strings.ToLower(base[idx+1:])
		isSubN := false
		if rest, ok := strings.CutPrefix(tok, "sub"); ok && rest != "" {
			if _, err := strconv.Atoi(rest); err == nil {
				isSubN = true
			}
		}
		switch {
		case tok == "sub" || tok == "subbed" || tok == "h265" || tok == "av1" ||
			tok == "remux" || tok == "video" || tok == "nosound" || tok == "joined" ||
			tok == "preview" || isSubN:
			base = base[:idx]
		default:
			return base
		}
	}
}

// ----------------------------------------------------------------------------
// Track selection for the split modes
// ----------------------------------------------------------------------------

// promptTrackSelection lists every audio/subtitle track of a split source and
// lets the user pick which ones to extract. Multichannel audio additionally
// gets its own numbered "stereo downmix" entry — that one is opt-in only:
// Enter (= all tracks) does NOT create stereo mixes. nil audioSel/subSel mean
// "all tracks"; stereoSel contains only explicitly chosen mixes (may be nil).
// With fewer than two selectable entries no question is asked at all.
// allowStereo=false (lossless -split) hides the stereo-mix entries entirely,
// because a downmix would be a re-encode and has no place in a 1:1 split.
func promptTrackSelection(streams *ffprobeOutput, allowStereo bool) (audioSel, subSel, stereoSel map[int]bool) {
	const (
		kindAudio = iota
		kindStereo
		kindSub
	)
	type entry struct {
		kind  int
		rel   int // relative index within its own track type (0:a:N / 0:s:N)
		label string
	}
	var entries []entry
	aIdx, sIdx := 0, 0
	hasStereoOption := false
	for _, s := range streams.Streams {
		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}
		switch s.CodecType {
		case "audio":
			label := fmt.Sprintf("Audio  %-3s  %s %dch",
				lang, strings.ToUpper(s.CodecName), s.Channels)
			entries = append(entries, entry{kindAudio, aIdx, label})
			if allowStereo && s.Channels > 2 {
				hasStereoOption = true
				entries = append(entries, entry{kindStereo, aIdx, fmt.Sprintf(
					"  ↳ Stereo mix of [%d]  (extra .stereo.m4a)", len(entries))})
			}
			aIdx++
		case "subtitle":
			label := fmt.Sprintf("Sub    %-3s  %s",
				lang, strings.ToUpper(s.CodecName))
			if s.Disposition.Forced == 1 {
				label += " [forced]"
			}
			if s.Disposition.HearingImpaired == 1 {
				label += " [SDH]"
			}
			entries = append(entries, entry{kindSub, sIdx, label})
			sIdx++
		}
	}
	if len(entries) < 2 {
		return nil, nil, nil
	}

	fmt.Println(pterm.Gray("  → Multiple tracks found:"))
	for i, e := range entries {
		fmt.Printf("    %s %s\n",
			pterm.LightCyan(fmt.Sprintf("[%d]", i+1)), e.label)
	}
	hint := "Enter = all"
	if hasStereoOption {
		hint = "Enter = all tracks WITHOUT stereo mix"
	}
	fmt.Printf("  Tracks to extract (%s, e.g. 1,3): ", hint)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil, nil
	}

	audioSel, subSel, stereoSel = map[int]bool{}, map[int]bool{}, map[int]bool{}
	valid := false
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(entries) {
			pWarn.Printf("Invalid selection %q ignored.\n", tok)
			continue
		}
		valid = true
		switch e := entries[n-1]; e.kind {
		case kindAudio:
			audioSel[e.rel] = true
		case kindStereo:
			stereoSel[e.rel] = true
		case kindSub:
			subSel[e.rel] = true
		}
	}
	if !valid {
		fmt.Println(pterm.Gray("  No valid selection — extracting all tracks (without stereo mix)."))
		return nil, nil, nil
	}
	return audioSel, subSel, stereoSel
}

// ----------------------------------------------------------------------------
// Collision-free output names (split/extract never overwrite existing files)
// ----------------------------------------------------------------------------

// uniquePath returns path unchanged when it is still free; otherwise a
// numbered variant (Name.2.mp4, Name.3.mp4, …). The split runs FFmpeg with
// -y, so without this an unrelated file of the same name would be destroyed.
func uniquePath(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path, nil
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for n := 2; n <= 99; n++ {
		cand := fmt.Sprintf("%s.%d%s", stem, n, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("Streams.go: uniquePath: no free name for %s", filepath.Base(path))
}

// uniqueVobSubPath is uniquePath for VobSub: FFmpeg writes the companion .sub
// next to the .idx, so BOTH names must be free for a candidate to count.
func uniqueVobSubPath(idxPath string) (string, error) {
	free := func(p string) bool {
		_, e1 := os.Stat(p)
		_, e2 := os.Stat(strings.TrimSuffix(p, ".idx") + ".sub")
		return os.IsNotExist(e1) && os.IsNotExist(e2)
	}
	if free(idxPath) {
		return idxPath, nil
	}
	stem := strings.TrimSuffix(idxPath, ".idx")
	for n := 2; n <= 99; n++ {
		cand := fmt.Sprintf("%s.%d.idx", stem, n)
		if free(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("Streams.go: uniqueVobSubPath: no free name for %s", filepath.Base(idxPath))
}

// ----------------------------------------------------------------------------
// Batch split: -davinci without files
// ----------------------------------------------------------------------------

// splitVideoOutPath returns the video-only MP4 target name for a split source
// (before any collision numbering). Shared by muxToMP4 and the batch
// done-check, so both always agree on the name. The ".NoSound" tag makes it
// obvious this is the silent picture track and prevents it from ever colliding
// with the original file name.
func splitVideoOutPath(srcPath string) string {
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	return filepath.Join(filepath.Dir(srcPath), base+".NoSound.mp4")
}

// runBatchSplit splits every MKV in the current directory into its components
// (video-only MP4 + audio + subtitle files). Fully automatic: all tracks, no
// stereo mixes, no questions. A per-file lock (same mechanism as the
// converter) plus a done-check on the video output make it safe to run
// several instances in parallel — they divide the directory between them.
func runBatchSplit(ctx context.Context) {
	dir, err := os.Getwd()
	if err != nil {
		exe, exeErr := os.Executable()
		if exeErr != nil {
			pErr.Printf("Cannot determine working directory: %v\n", err)
			return
		}
		dir = filepath.Dir(exe)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		pErr.Printf("Cannot read directory %s: %v\n", dir, err)
		return
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".mkv") {
			continue
		}
		// Never re-split a silent picture track we produced earlier.
		if strings.Contains(strings.ToLower(e.Name()), ".nosound.") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) == 0 {
		pInfo.Printf("Batch split: no MKV files found in %s\n", dir)
		fmt.Println()
		showUsage()
		return
	}

	pInfo.Printf("Batch split: %d MKV file(s) in %s\n",
		len(files), dir)
	fmt.Println(pterm.Gray("  All tracks, no stereo mixes, no questions — parallel instances share the work."))

	done, skipped := 0, 0
	for i, f := range files {
		if ctx.Err() != nil {
			fmt.Println()
			fmt.Println(pterm.Gray(fmt.Sprintf("  Skipped (aborted): %s", filepath.Base(f))))
			skipped++
			continue
		}
		fmt.Println()
		fmt.Printf("%s ", pterm.Gray(fmt.Sprintf("[%d/%d]", i+1, len(files))))

		if _, statErr := os.Stat(splitVideoOutPath(f)); statErr == nil {
			pInfo.Printf("» %s\n", filepath.Base(f))
			fmt.Println(pterm.Gray("  Skipped: video output already exists (already split)."))
			skipped++
			continue
		}
		func() {
			unlock, lockErr := acquireProcessingLock(f+".lock", getFileSizeMB(f), f)
			if lockErr != nil {
				pInfo.Printf("» %s\n", filepath.Base(f))
				fmt.Println(pterm.Gray("  Skipped: another instance is currently processing this file."))
				skipped++
				return
			}
			defer unlock()
			// Re-check after acquiring the lock: another instance may have
			// finished this file between the first check and now.
			if _, statErr := os.Stat(splitVideoOutPath(f)); statErr == nil {
				pInfo.Printf("» %s\n", filepath.Base(f))
				fmt.Println(pterm.Gray("  Skipped: output appeared after acquiring lock (another instance was faster)."))
				skipped++
				return
			}
			processOneMKV(ctx, f, false)
			if ctx.Err() == nil {
				done++
			} else {
				skipped++
			}
		}()
	}
	fmt.Println()
	pInfo.Printf("Batch split finished: %d processed, %d skipped.\n", done, skipped)
}

// ----------------------------------------------------------------------------
// Mode 1a: MKV → MP4 + individual tracks
// ----------------------------------------------------------------------------

func runDemuxFromMKV(ctx context.Context, files []string) {
	total := len(files)
	for i, f := range files {
		if ctx.Err() != nil {
			fmt.Println()
			fmt.Println(pterm.Gray(fmt.Sprintf("  Skipped (aborted): %s", filepath.Base(f))))
			continue
		}
		if i > 0 {
			fmt.Println()
		}
		if total > 1 {
			fmt.Printf("%s ", pterm.Gray(fmt.Sprintf("[%d/%d]", i+1, total)))
		}
		processOneMKV(ctx, f, true)
	}
}

// processOneMKV demuxes one MKV into video-only MP4 + individual audio/sub
// files. Ctrl+C aborts the whole split immediately (no salvage extraction).
// askTracks=false (batch mode) skips the track question: all tracks are
// extracted, stereo mixes stay opt-in-only and are never created.
func processOneMKV(ctx context.Context, mkvPath string, askTracks bool) {
	abs, _ := filepath.Abs(mkvPath)
	pInfo.Printf("» %s\n", filepath.Base(abs))

	if _, err := os.Stat(abs); err != nil {
		pErr.Printf("  File not found: %v\n", err)
		return
	}

	streams, err := probeStreams(ctx, abs)
	if err != nil {
		pErr.Printf("  Probe failed: %v\n", err)
		return
	}

	var audioInfos []AudioStreamInfo
	for _, s := range streams.Streams {
		if s.CodecType != "audio" {
			continue
		}
		lang := ""
		if s.Tags != nil {
			lang = s.Tags["language"]
		}
		sr, _ := strconv.Atoi(s.SampleRate)
		audioInfos = append(audioInfos, AudioStreamInfo{
			Codec:      s.CodecName,
			Channels:   s.Channels,
			Layout:     s.ChannelLayout,
			Language:   lang,
			SampleRate: sr,
		})
	}
	printDavinciAudioInfo(audioInfos, false)
	var audioSel, subSel, stereoSel map[int]bool
	if askTracks {
		audioSel, subSel, stereoSel = promptTrackSelection(streams, true)
	}

	// Ctrl+C means STOP: no salvage extraction of the remaining tracks.
	// Unfinished output files are already removed by the failing step itself.
	muxErr := muxToMP4(ctx, abs, streams)
	if errors.Is(muxErr, context.Canceled) {
		fmt.Println(pterm.Gray("  Aborted — unfinished video file removed."))
		return
	}
	if muxErr != nil {
		pErr.Printf("  MP4 creation failed: %v\n", muxErr)
	}
	if ctx.Err() != nil {
		fmt.Println(pterm.Gray("  Aborted."))
		return
	}

	extractAudios(ctx, abs, streams, audioSel, stereoSel)
	extractSubs(ctx, abs, streams, subSel)
}

func muxToMP4(ctx context.Context, mkvPath string, streams *ffprobeOutput) error {
	return writeVideoOnlyMP4(ctx, mkvPath, splitVideoOutPath(mkvPath), streams)
}

// writeVideoOnlyMP4 stream-copies the primary video track to mp4Out
// (no audio/subs, +faststart). Shared by the MKV demux and the MP4 extract.
func writeVideoOnlyMP4(ctx context.Context, srcPath, mp4Out string, streams *ffprobeOutput) error {
	// Never clobber an existing file of the same name (e.g. the same movie
	// already present as a real MP4) — pick a numbered name instead.
	target, err := uniquePath(mp4Out)
	if err != nil {
		return fmt.Errorf("Streams.go: writeVideoOnlyMP4: %w", err)
	}
	if target != mp4Out {
		pInfo.Printf("  Output name already taken — writing as %s\n", filepath.Base(target))
		mp4Out = target
	}

	args := []string{
		"-y", "-i", srcPath,
		"-map", "0:V:0",
		"-c:v", "copy",
		"-an", "-sn",
		"-movflags", "+faststart",
	}
	for _, s := range streams.Streams {
		if s.CodecType == "video" && s.CodecName == "hevc" {
			args = append(args, "-tag:v", "hvc1")
			break
		}
	}
	args = append(args, mp4Out)

	fmt.Println(pterm.Gray("  → Extracting video (stream copy, no re-encode)..."))
	// Progress display: remuxing a multi-GB movie takes a while — without it
	// the program looks frozen.
	durationSec, _ := strconv.ParseFloat(streams.Format.Duration, 64)
	if err := runFFmpeg(ctx, args, durationSec, 1, 1, getFileSizeMB(srcPath)); err != nil {
		_ = os.Remove(mp4Out)
		return fmt.Errorf("Streams.go: writeVideoOnlyMP4: %w", err)
	}
	pOK.Printf("    ✓ %s\n", filepath.Base(mp4Out))
	return nil
}

// muxMP4VideoOnly creates a video-only MP4 from an MP4 source, suffixed
// ".NoSound.mp4" so the original is never overwritten and it is obvious which
// file carries the silent picture.
func muxMP4VideoOnly(ctx context.Context, mp4Path string, streams *ffprobeOutput) error {
	hasVideo := false
	for _, s := range streams.Streams {
		if s.CodecType == "video" {
			hasVideo = true
			break
		}
	}
	if !hasVideo {
		return nil
	}
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(mp4Path), filepath.Ext(mp4Path)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	mp4Out := filepath.Join(filepath.Dir(mp4Path), base+".NoSound.mp4")
	return writeVideoOnlyMP4(ctx, mp4Path, mp4Out, streams)
}

// ----------------------------------------------------------------------------
// Mode 1b: MP4/MOV/M4V → extract audio + subs
// ----------------------------------------------------------------------------

func runExtractFromMP4(ctx context.Context, files []string) {
	total := len(files)
	for i, f := range files {
		if ctx.Err() != nil {
			fmt.Println()
			fmt.Println(pterm.Gray(fmt.Sprintf("  Skipped (aborted): %s", filepath.Base(f))))
			continue
		}
		if i > 0 {
			fmt.Println()
		}
		if total > 1 {
			fmt.Printf("%s ", pterm.Gray(fmt.Sprintf("[%d/%d]", i+1, total)))
		}
		processOneMP4(ctx, f)
	}
}

func processOneMP4(ctx context.Context, mp4Path string) {
	abs, _ := filepath.Abs(mp4Path)
	pInfo.Printf("» %s\n", filepath.Base(abs))

	if _, err := os.Stat(abs); err != nil {
		pErr.Printf("  File not found: %v\n", err)
		return
	}

	streams, err := probeStreams(ctx, abs)
	if err != nil {
		pErr.Printf("  Probe failed: %v\n", err)
		return
	}

	var audioInfos []AudioStreamInfo
	for _, s := range streams.Streams {
		if s.CodecType != "audio" {
			continue
		}
		lang := ""
		if s.Tags != nil {
			lang = s.Tags["language"]
		}
		sr, _ := strconv.Atoi(s.SampleRate)
		audioInfos = append(audioInfos, AudioStreamInfo{
			Codec:      s.CodecName,
			Channels:   s.Channels,
			Layout:     s.ChannelLayout,
			Language:   lang,
			SampleRate: sr,
		})
	}
	printDavinciAudioInfo(audioInfos, false)
	audioSel, subSel, stereoSel := promptTrackSelection(streams, true)

	// Ctrl+C means STOP: no salvage extraction of the remaining tracks.
	muxErr := muxMP4VideoOnly(ctx, abs, streams)
	if errors.Is(muxErr, context.Canceled) {
		fmt.Println(pterm.Gray("  Aborted — unfinished video file removed."))
		return
	}
	if muxErr != nil {
		pErr.Printf("  Video-only MP4 creation failed: %v\n", muxErr)
	}
	if ctx.Err() != nil {
		fmt.Println(pterm.Gray("  Aborted."))
		return
	}

	extractAudios(ctx, abs, streams, audioSel, stereoSel)
	extractSubs(ctx, abs, streams, subSel)
}

// ----------------------------------------------------------------------------
// extractAudios: extract all audio tracks individually
// ----------------------------------------------------------------------------

// estimateAudioTrackMB estimates the size of one audio track, from its probed
// bitrate when available, otherwise from typical per-codec bitrates. Used only
// to feed the progress display.
func estimateAudioTrackMB(s ffprobeStream, durationSec float64) float64 {
	if durationSec <= 0 {
		return 0
	}
	if br, e := strconv.ParseFloat(s.BitRate, 64); e == nil && br > 0 {
		return br * durationSec / 8.0 / (1024 * 1024)
	}
	var kbps float64
	switch strings.ToLower(s.CodecName) {
	case "ac3":
		kbps = 384
	case "eac3":
		kbps = 256
	case "dts":
		kbps = 1024
	case "truehd":
		kbps = 3000
	case "flac":
		kbps = float64(s.Channels) * 400
	case "pcm_s16le", "pcm_s24le", "pcm_s32le",
		"pcm_f32le", "pcm_f64le", "pcm_u8":
		kbps = float64(s.Channels) * 768
	default:
		ch := s.Channels
		if ch <= 0 {
			ch = 2
		}
		kbps = float64(ch) * 96
	}
	return kbps * 1000 * durationSec / 8.0 / (1024 * 1024)
}

// runStereoDownmix creates the opt-in extra stereo .m4a for audio track i.
// Full re-encode of the whole runtime, hence the progress display.
func runStereoDownmix(ctx context.Context, srcPath, base, suffix string, i, total int, s ffprobeStream, durationSec float64) {
	stPath := filepath.Join(filepath.Dir(srcPath),
		fmt.Sprintf("%s.%s.stereo.m4a", base, suffix))
	if p, uerr := uniquePath(stPath); uerr == nil {
		stPath = p
	} else {
		pWarn.Printf("    × Stereo mix skipped: %v\n", uerr)
		return
	}
	_, stBr := aacEncodeParams(2)
	fmt.Printf("    %s %s %s\n",
		pterm.Gray(fmt.Sprintf("Audio %d:", i+1)),
		pterm.LightWhite(filepath.Base(stPath)),
		pterm.LightYellow(fmt.Sprintf("(%dch → Stereo AAC %dk)", s.Channels, stBr)),
	)
	stArgs := []string{
		"-y", "-i", srcPath,
		"-map", fmt.Sprintf("0:a:%d", i),
		"-vn", "-sn",
		"-c:a", "aac",
		"-b:a", fmt.Sprintf("%dk", stBr),
		"-filter:a", "aformat=sample_rates=48000:channel_layouts=stereo",
		stPath,
	}
	if err := runFFmpeg(ctx, stArgs, durationSec, i+1, total,
		estimateAudioTrackMB(s, durationSec)); err != nil {
		_ = os.Remove(stPath)
		if !errors.Is(err, context.Canceled) {
			pErr.Printf("    × %s: %v\n", filepath.Base(stPath), err)
		}
		return
	}
	pOK.Printf("    ✓ %s %s\n", filepath.Base(stPath),
		pterm.LightYellow("(stereo downmix)"))
}

// extractAudios extracts audio tracks individually. sel == nil means all;
// otherwise only the relative indices marked true are extracted. stereoSel
// lists the tracks that additionally (or instead) get a stereo downmix —
// stereo mixes are strictly opt-in and never part of "all".
func extractAudios(ctx context.Context, mkvPath string, streams *ffprobeOutput, sel, stereoSel map[int]bool) {
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(mkvPath), filepath.Ext(mkvPath)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	dir := filepath.Dir(mkvPath)

	var audioStreams []ffprobeStream
	for _, s := range streams.Streams {
		if s.CodecType == "audio" {
			audioStreams = append(audioStreams, s)
		}
	}
	if len(audioStreams) == 0 {
		fmt.Println(pterm.Gray("  → No audio tracks found."))
		return
	}
	wantTracks, wantStereo := 0, 0
	for i := range audioStreams {
		if sel == nil || sel[i] {
			wantTracks++
		}
		if stereoSel[i] {
			wantStereo++
		}
	}
	if wantTracks == 0 && wantStereo == 0 {
		fmt.Println(pterm.Gray("  → Audio skipped (not selected)."))
		return
	}
	msg := fmt.Sprintf("  → Extracting %d audio track(s)", wantTracks)
	if wantStereo > 0 {
		msg += fmt.Sprintf(" + %d stereo mix(es)", wantStereo)
	}
	fmt.Println(pterm.Gray(msg + "..."))

	durationSec, _ := strconv.ParseFloat(streams.Format.Duration, 64)
	suffixCounter := map[string]int{}

	for i, s := range audioStreams {
		if ctx.Err() != nil {
			break
		}
		extractTrack := sel == nil || sel[i]
		makeStereo := stereoSel[i]
		if !extractTrack && !makeStereo {
			continue
		}

		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}

		suffix := lang
		seen := suffixCounter[suffix]
		suffixCounter[suffix]++
		if seen > 0 {
			suffix = fmt.Sprintf("%s.%d", suffix, seen+1)
		}

		if !extractTrack {
			// Stereo mix only — the full track itself was deselected.
			runStereoDownmix(ctx, mkvPath, base, suffix, i, len(audioStreams), s, durationSec)
			continue
		}

		var outPath string
		var ffargs []string
		var reencNote string

		sr, _ := strconv.Atoi(s.SampleRate)
		needReenc := needsAudioReencode(s.CodecName, s.ChannelLayout, s.Channels, sr)
		if needReenc {
			_, br := aacEncodeParams(s.Channels)
			var reason string
			if isDavinciIncompatibleAudio(s.CodecName) {
				reason = strings.ToUpper(s.CodecName)
			} else {
				layoutTag := s.ChannelLayout
				if layoutTag == "" {
					layoutTag = fmt.Sprintf("%dch?", s.Channels)
				}
				reason = fmt.Sprintf("%s/%s", strings.ToUpper(s.CodecName), layoutTag)
			}
			reencNote = fmt.Sprintf("%s → AAC %dk", reason, br)

			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.m4a", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{
				"-y", "-i", mkvPath,
				"-map", fmt.Sprintf("0:a:%d", i),
				"-vn", "-sn",
				"-c:a", "aac",
				"-b:a", fmt.Sprintf("%dk", br),
				"-filter:a", davinciSafeChannelLayoutsFilter,
				outPath,
			}
		} else {
			codec := strings.ToLower(s.CodecName)
			var ext string
			switch codec {
			case "aac":
				ext = "m4a"
			case "mp3":
				ext = "mp3"
			case "pcm_s16le", "pcm_s24le", "pcm_s32le",
				"pcm_f32le", "pcm_f64le", "pcm_u8":
				ext = "wav"
			default:
				ext = "m4a"
			}
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.%s", base, suffix, ext))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{
				"-y", "-i", mkvPath,
				"-map", fmt.Sprintf("0:a:%d", i),
				"-vn", "-sn",
				"-c:a", "copy",
			}
			if ext == "wav" {
				ffargs = append(ffargs, "-rf64", "auto")
			}
			ffargs = append(ffargs, outPath)
		}

		if reencNote != "" {
			fmt.Printf("    %s %s %s\n",
				pterm.Gray(fmt.Sprintf("Audio %d:", i+1)),
				pterm.LightWhite(filepath.Base(outPath)),
				pterm.LightYellow(fmt.Sprintf("(%s)", reencNote)),
			)
		}

		var ffErr error
		if needReenc {
			ffErr = runFFmpeg(ctx, ffargs, durationSec, i+1, len(audioStreams),
				estimateAudioTrackMB(s, durationSec))
		} else {
			ffErr = runFFmpegSub(ctx, ffargs)
		}

		if ffErr != nil {
			pErr.Printf("    × %s: %v\n", filepath.Base(outPath), ffErr)
			_ = os.Remove(outPath)
			continue
		}
		pOK.Printf("    ✓ %s\n", filepath.Base(outPath))

		// Opt-in stereo downmix in addition to the full track.
		if makeStereo && ctx.Err() == nil {
			runStereoDownmix(ctx, mkvPath, base, suffix, i, len(audioStreams), s, durationSec)
		}
	}
}

// ----------------------------------------------------------------------------
// extractSubs: extract all subtitle tracks individually
// ----------------------------------------------------------------------------

// extractSubs extracts subtitle tracks individually. sel == nil means all;
// otherwise only the relative indices marked true are extracted.
func extractSubs(ctx context.Context, mkvPath string, streams *ffprobeOutput, sel map[int]bool) {
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(mkvPath), filepath.Ext(mkvPath)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	dir := filepath.Dir(mkvPath)

	var subStreams []ffprobeStream
	for _, s := range streams.Streams {
		if s.CodecType == "subtitle" {
			subStreams = append(subStreams, s)
		}
	}
	if len(subStreams) == 0 {
		fmt.Println(pterm.Gray("  → No subtitle tracks found."))
		return
	}
	want := len(subStreams)
	if sel != nil {
		want = len(sel)
		if want == 0 {
			fmt.Println(pterm.Gray("  → Subtitles skipped (not selected)."))
			return
		}
	}
	fmt.Println(pterm.Gray(fmt.Sprintf("  → Extracting %d subtitle track(s)...", want)))

	suffixCounter := map[string]int{}

	for i, s := range subStreams {
		if ctx.Err() != nil {
			break
		}
		if sel != nil && !sel[i] {
			continue
		}

		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}

		suffix := lang
		if s.Disposition.Forced == 1 {
			suffix += ".forced"
		}
		if s.Disposition.HearingImpaired == 1 {
			suffix += ".sdh"
		}
		seen := suffixCounter[suffix]
		suffixCounter[suffix]++
		if seen > 0 {
			suffix = fmt.Sprintf("%s.%d", suffix, seen+1)
		}

		codec := strings.ToLower(s.CodecName)
		var outPath string
		var ffargs []string

		switch codec {
		case "subrip", "srt", "ass", "ssa", "mov_text", "webvtt":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.srt", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{
				"-y", "-i", mkvPath,
				"-map", fmt.Sprintf("0:s:%d", i),
				"-c:s", "srt", outPath,
			}
		case "hdmv_pgs_subtitle":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.sup", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{
				"-y", "-i", mkvPath,
				"-map", fmt.Sprintf("0:s:%d", i),
				"-c:s", "copy", outPath,
			}
		case "dvd_subtitle":
			// VobSub: FFmpeg writes .idx + .sub in parallel.
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.idx", base, suffix))
			if p, uerr := uniqueVobSubPath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{
				"-y", "-i", mkvPath,
				"-map", fmt.Sprintf("0:s:%d", i),
				"-c:s", "copy", outPath,
			}
		default:
			pWarn.Printf("    × Track %d: unknown sub codec '%s' (skipped)\n", i+1, codec)
			continue
		}

		if err := runFFmpegSub(ctx, ffargs); err != nil {
			pErr.Printf("    × %s: %v\n", filepath.Base(outPath), err)
			_ = os.Remove(outPath)
			if strings.HasSuffix(outPath, ".idx") {
				// FIX LOGIC-01: only delete the companion .sub for this stem.
				_ = os.Remove(strings.TrimSuffix(outPath, ".idx") + ".sub")
			}
			continue
		}
		pOK.Printf("    ✓ %s\n", filepath.Base(outPath))

		if strings.HasSuffix(outPath, ".srt") {
			cleanSRT(outPath)
		}
	}
}

// ----------------------------------------------------------------------------
// Filename language parsing helpers
// ----------------------------------------------------------------------------

// langPattern matches <stem>.<lang>(.forced|.sdh)?(<.n>)?(.stereo)?.<ext>
var langPattern = regexp.MustCompile(`\.([a-z]{2,3})(?:\.(?:forced|sdh))?(?:\.\d+)?(?:\.stereo)?\.[a-z0-9]+$`)

func langFromFilename(file string) string {
	base := strings.ToLower(filepath.Base(file))
	m := langPattern.FindStringSubmatch(base)
	if len(m) < 2 {
		return "und"
	}
	return normalizeLang(m[1])
}

func parseSubTags(file string) (lang string, isForced bool, isSDH bool) {
	lang = langFromFilename(file)
	base := strings.ToLower(filepath.Base(file))
	isForced = strings.Contains(base, ".forced.")
	isSDH = strings.Contains(base, ".sdh.")
	return
}

// ----------------------------------------------------------------------------
// Mode 2: Merge — base video + audio/SRTs → .sub.mkv
// ----------------------------------------------------------------------------

func runMerge(ctx context.Context, videoPath string, audioPaths, srtPaths []string) {
	abs, _ := filepath.Abs(videoPath)
	pInfo.Printf("» %s + %d Audio + %d SRT\n",
		filepath.Base(abs), len(audioPaths), len(srtPaths))

	if _, err := os.Stat(abs); err != nil {
		pErr.Printf("  File not found: %v\n", err)
		return
	}
	for _, a := range audioPaths {
		if _, err := os.Stat(a); err != nil {
			pErr.Printf("  Audio file not found: %v\n", err)
			return
		}
	}
	for _, srt := range srtPaths {
		if _, err := os.Stat(srt); err != nil {
			pErr.Printf("  SRT file not found: %v\n", err)
			return
		}
	}
	// Clean all SRTs before muxing (only real .srt — .sup/.idx/.ass are
	// binary or styled formats the SRT cleaner must not touch).
	for _, srt := range srtPaths {
		if strings.EqualFold(filepath.Ext(srt), ".srt") {
			cleanSRT(srt)
		}
	}

	streams, err := probeStreams(ctx, abs)
	if err != nil {
		pErr.Printf("  Probe failed: %v\n", err)
		return
	}

	// The base video contributes ONLY its video track. Audio/subtitle tracks
	// living inside it are never carried over — the merge result contains
	// exactly the dropped files, nothing else. Say so when tracks get dropped.
	internalAudio, internalSubs := 0, 0
	for _, s := range streams.Streams {
		switch s.CodecType {
		case "audio":
			internalAudio++
		case "subtitle":
			internalSubs++
		}
	}
	if internalAudio > 0 {
		if len(audioPaths) == 0 {
			pWarn.Printf("  Base video has %d audio track(s), but no audio file was dropped — the result will have NO sound.\n", internalAudio)
		} else {
			pInfo.Printf("  Base video audio (%d track(s)) is not carried over — only the dropped audio files are used.\n", internalAudio)
		}
	}
	if internalSubs > 0 {
		pWarn.Printf("  Base video contains %d subtitle track(s) — these are NOT carried over (only dropped files are merged).\n", internalSubs)
	}

	// Trim our own suffixes (.sub/.h265/…) so repeated merges never produce
	// names like ".sub.sub.mkv" or ever-growing chains.
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	dir := filepath.Dir(abs)
	outPath := filepath.Join(dir, base+".sub.mkv")

	// FIX LOGIC-02: self-overwrite guard with abort instead of infinite loop.
	// Also guards against silently overwriting an existing .sub.mkv from an
	// earlier merge: a numbered name (.sub2.mkv, …) is chosen instead.
	_, outErr := os.Stat(outPath)
	if outPath == abs || outErr == nil {
		const maxCandidates = 100
		found := false
		for n := 2; n <= maxCandidates; n++ {
			candidate := filepath.Join(dir, fmt.Sprintf("%s.sub%d.mkv", base, n))
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				outPath = candidate
				found = true
				break
			}
		}
		if !found {
			pErr.Printf("  No free output file name found (checked up to .sub%d.mkv)\n", maxCandidates)
			return
		}
		pInfo.Printf("  Output name already taken — writing as %s\n", filepath.Base(outPath))
	}

	// === Build FFmpeg args ===
	args := []string{"-y", "-i", abs}

	for _, a := range audioPaths {
		audAbs, _ := filepath.Abs(a)
		args = append(args, "-i", audAbs)
	}

	srtInputStart := 1 + len(audioPaths)
	for _, srt := range srtPaths {
		srtAbs, _ := filepath.Abs(srt)
		args = append(args, "-i", srtAbs)
	}

	// Video: primary video stream only (no cover art).
	args = append(args, "-map", "0:V:0")

	// Audio mapping.
	useExternalAudio := len(audioPaths) > 0
	var reencNotes []string

	// External audio files can be anything the user dropped (.ac3/.dts/.flac/…).
	// Probe each one and re-encode to AAC where a 1:1 copy would not be readable
	// in DaVinci — same rule set as the converter (needsAudioReencode).
	var extAudioArgs []string
	if useExternalAudio {
		for i, a := range audioPaths {
			audAbs, _ := filepath.Abs(a)
			sel := fmt.Sprintf(":a:%d", i)
			ainfo, probeErr := probeStreams(ctx, audAbs)
			if probeErr != nil {
				pErr.Printf("  Audio probe failed (%s): %v\n", filepath.Base(a), probeErr)
				return
			}
			var ast *ffprobeStream
			for j := range ainfo.Streams {
				if ainfo.Streams[j].CodecType == "audio" {
					ast = &ainfo.Streams[j]
					break
				}
			}
			if ast == nil {
				pErr.Printf("  No audio stream found in %s\n", filepath.Base(a))
				return
			}
			sr, _ := strconv.Atoi(ast.SampleRate)
			if needsAudioReencode(ast.CodecName, ast.ChannelLayout, ast.Channels, sr) {
				_, br := aacEncodeParams(ast.Channels)
				extAudioArgs = append(extAudioArgs,
					"-c"+sel, "aac",
					"-b"+sel, fmt.Sprintf("%dk", br),
					"-filter"+sel, davinciSafeChannelLayoutsFilter,
				)
				reencNotes = append(reencNotes, fmt.Sprintf("%s/%dch→AAC %dk",
					strings.ToUpper(ast.CodecName), ast.Channels, br))
			} else {
				extAudioArgs = append(extAudioArgs, "-c"+sel, "copy")
			}
		}
	}

	if useExternalAudio {
		// Map only the first audio stream of each file so output stream indices
		// always match the per-file codec/metadata/disposition arguments.
		// Base-video audio is intentionally NOT mapped.
		for i := range audioPaths {
			args = append(args, "-map", fmt.Sprintf("%d:a:0", i+1))
		}
	}

	// Sub mapping: first subtitle stream per file, so output stream indices
	// always line up with the per-file metadata/disposition arguments below.
	for i := range srtPaths {
		args = append(args, "-map", fmt.Sprintf("%d:s:0", srtInputStart+i))
	}

	// Codecs.
	args = append(args, "-c:v", "copy")
	if useExternalAudio {
		args = append(args, extAudioArgs...)
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-c:s", "copy")

	// Default audio: German preferred, else first track.
	defaultAudioIdx := 0
	if useExternalAudio {
		for i, a := range audioPaths {
			if langFromFilename(a) == "ger" {
				defaultAudioIdx = i
				break
			}
		}
	}

	// Language & disposition metadata per audio.
	for i, a := range audioPaths {
		lang := langFromFilename(a)
		args = append(args,
			fmt.Sprintf("-metadata:s:a:%d", i),
			fmt.Sprintf("language=%s", lang),
		)
		if i == defaultAudioIdx {
			args = append(args, fmt.Sprintf("-disposition:a:%d", i), "default")
		} else {
			args = append(args, fmt.Sprintf("-disposition:a:%d", i), "0")
		}

		defaultMark := ""
		if i == defaultAudioIdx {
			defaultMark = pterm.LightGreen(" [Default]")
		}
		fmt.Printf("  %s %s → %s%s\n",
			pterm.Gray("• Audio"),
			pterm.LightWhite(filepath.Base(a)),
			pterm.LightCyan(lang),
			defaultMark,
		)
	}

	// Language & disposition metadata per sub.
	for i, srt := range srtPaths {
		lang, forced, sdh := parseSubTags(srt)
		args = append(args,
			fmt.Sprintf("-metadata:s:s:%d", i),
			fmt.Sprintf("language=%s", lang),
		)
		switch {
		case forced:
			args = append(args, fmt.Sprintf("-disposition:s:%d", i), "forced")
		case sdh:
			args = append(args, fmt.Sprintf("-disposition:s:%d", i), "hearing_impaired")
		default:
			args = append(args, fmt.Sprintf("-disposition:s:%d", i), "0")
		}

		tagNote := ""
		if forced {
			tagNote = pterm.LightRed(" [Forced]")
		} else if sdh {
			tagNote = pterm.LightYellow(" [SDH]")
		}
		fmt.Printf("  %s %s → %s%s\n",
			pterm.Gray("• Sub  "),
			pterm.LightWhite(filepath.Base(srt)),
			pterm.LightCyan(lang),
			tagNote,
		)
	}

	args = append(args, outPath)

	fmt.Print(pterm.Gray("  → Building MKV"))
	if len(reencNotes) > 0 {
		fmt.Print(pterm.LightYellow(
			fmt.Sprintf(" (audio re-encode: %s)", strings.Join(reencNotes, ", "))))
	}
	fmt.Println("...")

	durationSec, _ := strconv.ParseFloat(streams.Format.Duration, 64)

	// FIX SUB-01: two-stage fallback.
	// First attempt: with all SRTs.
	// Second attempt: without SRTs (most common failure cause: bad encoding).
	// Input size 0: a merge ADDS streams, so a smaller/larger verdict against
	// the video-only input would be misleading — show output size neutrally.
	err = runFFmpeg(ctx, args, durationSec, 1, 1, 0)
	if err == nil {
		pOK.Printf("    ✓ %s\n", filepath.Base(outPath))
		return
	}
	if errors.Is(err, context.Canceled) {
		_ = os.Remove(outPath)
		return
	}
	_ = os.Remove(outPath)

	if len(srtPaths) > 0 {
		pWarn.Println("  Merge with subs failed, trying without subtitles...")

		fallbackArgs := []string{"-y", "-i", abs}
		for _, a := range audioPaths {
			audAbs, _ := filepath.Abs(a)
			fallbackArgs = append(fallbackArgs, "-i", audAbs)
		}
		fallbackArgs = append(fallbackArgs, "-map", "0:V:0")
		if useExternalAudio {
			for i := range audioPaths {
				fallbackArgs = append(fallbackArgs, "-map", fmt.Sprintf("%d:a:0", i+1))
			}
		}
		fallbackArgs = append(fallbackArgs, "-c:v", "copy")
		if useExternalAudio {
			fallbackArgs = append(fallbackArgs, extAudioArgs...)
		} else {
			fallbackArgs = append(fallbackArgs, "-an")
		}
		for i, a := range audioPaths {
			lang := langFromFilename(a)
			fallbackArgs = append(fallbackArgs,
				fmt.Sprintf("-metadata:s:a:%d", i),
				fmt.Sprintf("language=%s", lang),
			)
			if i == defaultAudioIdx {
				fallbackArgs = append(fallbackArgs,
					fmt.Sprintf("-disposition:a:%d", i), "default")
			} else {
				fallbackArgs = append(fallbackArgs,
					fmt.Sprintf("-disposition:a:%d", i), "0")
			}
		}
		fallbackArgs = append(fallbackArgs, "-sn", outPath)

		if err2 := runFFmpeg(ctx, fallbackArgs, durationSec, 1, 1, 0); err2 != nil {
			_ = os.Remove(outPath)
			pErr.Printf("  Merge failed even without subs: %v\n", err2)
			return
		}
		pOK.Printf("    ✓ %s %s\n",
			filepath.Base(outPath),
			pterm.LightYellow("(without subtitles — check SRT files)"))
		return
	}

	pErr.Printf("  Merge failed: %v\n", err)
}

// ════════════════════════════════════════════════════════════════════════════
// Lossless split / join: -split / -join
//
// Unlike -davinci (which targets DaVinci Resolve and re-encodes incompatible
// audio / converts subtitles), these two modes copy EVERY stream 1:1, with no
// re-encode and no SRT cleaning:
//
//   -split  : take a video and write the silent picture (".NoSound") plus every
//             audio track in its native container and every subtitle untouched.
//   -join   : take a (silent) picture track plus audio/subtitle files and mux
//             them back into a single MKV, copying everything 1:1.
//
// A -split followed by a -join is a true lossless round-trip.
// ════════════════════════════════════════════════════════════════════════════

// losslessVideoContainerExt picks the output container for the silent picture.
// The source container is kept when it is mp4-family (mp4/m4v/mov, always
// mp4-compatible codecs) or mkv/webm; every other (and unknown) container goes
// to MKV, which holds any video codec losslessly. Deterministic from the path
// alone, so the batch done-check needs no probe.
func losslessVideoContainerExt(srcPath string) string {
	switch strings.ToLower(filepath.Ext(srcPath)) {
	case ".mp4", ".m4v", ".mov":
		return ".mp4"
	case ".ts", ".m2ts", ".mts", ".m2t":
		// Transport streams carry huge/discontinuous PTS and often a trailing
		// packet with no timestamp at all. MKV refuses such packets ("Can't
		// write packet with unknown timestamp") and the silent-video step fails;
		// MP4 tolerates them and still copies the picture 1:1. The join always
		// produces MKV anyway, so the intermediate container does not matter.
		return ".mp4"
	default:
		return ".mkv"
	}
}

// splitNoSoundOutPath returns the silent-picture target name for a lossless
// split source (before any collision numbering). Used by the batch done-check
// and the writer so both always agree on the name.
func splitNoSoundOutPath(srcPath string) string {
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	return filepath.Join(filepath.Dir(srcPath), base+".NoSound"+losslessVideoContainerExt(srcPath))
}

// losslessAudioExt maps an audio codec to the file extension whose container
// can hold it without re-encoding. Anything unknown lands in Matroska audio
// (.mka), which swallows every codec losslessly.
func losslessAudioExt(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac":
		return "m4a"
	case "ac3":
		return "ac3"
	case "eac3":
		return "eac3"
	case "dts":
		return "dts"
	case "truehd", "mlp":
		return "thd"
	case "flac":
		return "flac"
	case "opus":
		return "opus"
	case "vorbis":
		return "ogg"
	case "mp3":
		return "mp3"
	case "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_f32le", "pcm_f64le",
		"pcm_u8", "pcm_s16be", "pcm_s24be", "pcm_s32be", "pcm_alaw", "pcm_mulaw":
		return "wav"
	default:
		return "mka"
	}
}

// ----------------------------------------------------------------------------
// -split entry point
// ----------------------------------------------------------------------------

func runSplitMode(ctx context.Context, args []string) {
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("Split  (lossless 1:1 — no re-encode, no cleaning)")
	fmt.Println()

	// No files: batch mode — split every supported video in the current folder.
	if len(args) == 0 {
		dir, err := os.Getwd()
		if err != nil {
			exe, exeErr := os.Executable()
			if exeErr != nil {
				pErr.Printf("Cannot determine working directory: %v\n", err)
				return
			}
			dir = filepath.Dir(exe)
		}
		runBatchSplitLossless(ctx, dir)
		return
	}

	// Folders are processed in batch (no questions); explicit files are split
	// interactively (Enter = all tracks, or pick numbers). Non-video arguments
	// are reported and skipped.
	var files, folders, ignored []string
	for _, a := range args {
		info, err := os.Stat(a)
		if err != nil {
			ignored = append(ignored, a)
			continue
		}
		if info.IsDir() {
			folders = append(folders, a)
			continue
		}
		if videoExtensions[strings.ToLower(filepath.Ext(a))] {
			files = append(files, a)
		} else {
			ignored = append(ignored, a)
		}
	}
	for _, ig := range ignored {
		pWarn.Printf("Not a video file, skipped: %s\n", filepath.Base(ig))
	}
	for _, d := range folders {
		if ctx.Err() != nil {
			break
		}
		runBatchSplitLossless(ctx, d)
	}
	total := len(files)
	for i, f := range files {
		if ctx.Err() != nil {
			fmt.Println()
			fmt.Println(pterm.Gray(fmt.Sprintf("  Skipped (aborted): %s", filepath.Base(f))))
			continue
		}
		if i > 0 {
			fmt.Println()
		}
		if total > 1 {
			fmt.Printf("%s ", pterm.Gray(fmt.Sprintf("[%d/%d]", i+1, total)))
		}
		processOneVideoLossless(ctx, f, true)
	}
	if len(files) == 0 && len(folders) == 0 {
		fmt.Println()
		showUsageSplitJoin()
	}
}

// runBatchSplitLossless splits every supported video in dir into its raw
// components. Fully automatic (all tracks, no questions). A per-file lock plus
// a done-check (the silent-picture output) make parallel instances safe — they
// divide the directory between them.
func runBatchSplitLossless(ctx context.Context, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		pErr.Printf("Cannot read directory %s: %v\n", dir, err)
		return
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !videoExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		// Never re-split a silent picture track we produced earlier.
		if strings.Contains(strings.ToLower(e.Name()), ".nosound.") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) == 0 {
		pInfo.Printf("Split (batch): no supported video files found in %s\n", dir)
		fmt.Println()
		showUsageSplitJoin()
		return
	}

	pInfo.Printf("Split (batch): %d video file(s) in %s\n", len(files), dir)
	fmt.Println(pterm.Gray("  All tracks, 1:1 copy, no questions — parallel instances share the work."))

	done, skipped := 0, 0
	for i, f := range files {
		if ctx.Err() != nil {
			fmt.Println()
			fmt.Println(pterm.Gray(fmt.Sprintf("  Skipped (aborted): %s", filepath.Base(f))))
			skipped++
			continue
		}
		fmt.Println()
		fmt.Printf("%s ", pterm.Gray(fmt.Sprintf("[%d/%d]", i+1, len(files))))

		if _, statErr := os.Stat(splitNoSoundOutPath(f)); statErr == nil {
			pInfo.Printf("» %s\n", filepath.Base(f))
			fmt.Println(pterm.Gray("  Skipped: silent picture already exists (already split)."))
			skipped++
			continue
		}
		func() {
			unlock, lockErr := acquireProcessingLock(f+".lock", getFileSizeMB(f), f)
			if lockErr != nil {
				pInfo.Printf("» %s\n", filepath.Base(f))
				fmt.Println(pterm.Gray("  Skipped: another instance is currently processing this file."))
				skipped++
				return
			}
			defer unlock()
			if _, statErr := os.Stat(splitNoSoundOutPath(f)); statErr == nil {
				pInfo.Printf("» %s\n", filepath.Base(f))
				fmt.Println(pterm.Gray("  Skipped: output appeared after acquiring lock (another instance was faster)."))
				skipped++
				return
			}
			processOneVideoLossless(ctx, f, false)
			if ctx.Err() == nil {
				done++
			} else {
				skipped++
			}
		}()
	}
	fmt.Println()
	pInfo.Printf("Split (batch) finished: %d processed, %d skipped.\n", done, skipped)
}

// processOneVideoLossless splits one video into silent picture + raw audio +
// raw subtitles. askTracks=false (batch) extracts everything without asking.
func processOneVideoLossless(ctx context.Context, srcPath string, askTracks bool) {
	abs, _ := filepath.Abs(srcPath)
	pInfo.Printf("» %s\n", filepath.Base(abs))

	if _, err := os.Stat(abs); err != nil {
		pErr.Printf("  File not found: %v\n", err)
		return
	}

	streams, err := probeStreams(ctx, abs)
	if err != nil {
		pErr.Printf("  Probe failed: %v\n", err)
		return
	}

	printLosslessTrackInfo(streams)
	var audioSel, subSel map[int]bool
	if askTracks {
		audioSel, subSel, _ = promptTrackSelection(streams, false)
	}

	// Ctrl+C means STOP: the failing step removes its own unfinished output.
	vidErr := writeNoSoundVideoLossless(ctx, abs, streams)
	if errors.Is(vidErr, context.Canceled) {
		fmt.Println(pterm.Gray("  Aborted — unfinished video file removed."))
		return
	}
	if vidErr != nil {
		pErr.Printf("  Silent video creation failed: %v\n", vidErr)
	}
	if ctx.Err() != nil {
		fmt.Println(pterm.Gray("  Aborted."))
		return
	}

	extractAudiosLossless(ctx, abs, streams, audioSel)
	extractSubsLossless(ctx, abs, streams, subSel)
}

// printLosslessTrackInfo lists the audio tracks neutrally (no DaVinci wording),
// so the user sees what will be copied 1:1.
func printLosslessTrackInfo(streams *ffprobeOutput) {
	idx := 0
	for _, s := range streams.Streams {
		if s.CodecType != "audio" {
			continue
		}
		idx++
		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}
		layout := s.ChannelLayout
		if layout == "" {
			layout = fmt.Sprintf("%dch", s.Channels)
		}
		fmt.Printf("  %s %s %s\n",
			pterm.Gray(fmt.Sprintf("Audio %d", idx)),
			pterm.LightWhite(fmt.Sprintf("(%s %s)", lang, strings.ToUpper(s.CodecName))),
			pterm.LightBlue(fmt.Sprintf("→ copy 1:1 (%s)", layout)),
		)
	}
}

// writeNoSoundVideoLossless stream-copies the primary video track into a
// ".NoSound" file (no audio/subs). MP4 targets get +faststart and the hvc1 tag
// for HEVC; MKV targets hold any codec as-is.
func writeNoSoundVideoLossless(ctx context.Context, srcPath string, streams *ffprobeOutput) error {
	hasVideo := false
	for _, s := range streams.Streams {
		if s.CodecType == "video" {
			hasVideo = true
			break
		}
	}
	if !hasVideo {
		fmt.Println(pterm.Gray("  → No video track found — skipping silent picture."))
		return nil
	}

	out := splitNoSoundOutPath(srcPath)
	target, err := uniquePath(out)
	if err != nil {
		return fmt.Errorf("Streams.go: writeNoSoundVideoLossless: %w", err)
	}
	if target != out {
		pInfo.Printf("  Output name already taken — writing as %s\n", filepath.Base(target))
		out = target
	}

	isMP4 := strings.EqualFold(filepath.Ext(out), ".mp4")
	args := []string{
		"-y", "-i", srcPath,
		"-map", "0:V:0",
		"-c:v", "copy",
		"-an", "-sn",
		// Many streams (HEVC with B-frames, DTS-X MKVs, transport streams) start
		// with a small negative/non-monotonic timestamp. Copied 1:1 into the
		// silent file as-is, that file can no longer be re-muxed — the later
		// -join then dies with "Can't write packet with unknown timestamp".
		// make_zero shifts the timeline so the first packet is 0; the picture
		// itself is untouched (still a true 1:1 copy).
		"-avoid_negative_ts", "make_zero",
	}
	if isMP4 {
		args = append(args, "-movflags", "+faststart")
		for _, s := range streams.Streams {
			if s.CodecType == "video" && s.CodecName == "hevc" {
				args = append(args, "-tag:v", "hvc1")
				break
			}
		}
	}
	args = append(args, out)

	fmt.Println(pterm.Gray("  → Extracting video (stream copy, no re-encode)..."))
	durationSec, _ := strconv.ParseFloat(streams.Format.Duration, 64)
	if err := runFFmpeg(ctx, args, durationSec, 1, 1, getFileSizeMB(srcPath)); err != nil {
		_ = os.Remove(out)
		return fmt.Errorf("Streams.go: writeNoSoundVideoLossless: %w", err)
	}
	pOK.Printf("    ✓ %s\n", filepath.Base(out))
	return nil
}

// extractAudiosLossless copies every selected audio track 1:1 into its native
// container. sel == nil means all. No re-encode ever.
func extractAudiosLossless(ctx context.Context, srcPath string, streams *ffprobeOutput, sel map[int]bool) {
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	dir := filepath.Dir(srcPath)

	var audioStreams []ffprobeStream
	for _, s := range streams.Streams {
		if s.CodecType == "audio" {
			audioStreams = append(audioStreams, s)
		}
	}
	if len(audioStreams) == 0 {
		fmt.Println(pterm.Gray("  → No audio tracks found."))
		return
	}
	want := 0
	for i := range audioStreams {
		if sel == nil || sel[i] {
			want++
		}
	}
	if want == 0 {
		fmt.Println(pterm.Gray("  → Audio skipped (not selected)."))
		return
	}
	fmt.Println(pterm.Gray(fmt.Sprintf("  → Extracting %d audio track(s) (stream copy, no re-encode)...", want)))

	suffixCounter := map[string]int{}
	for i, s := range audioStreams {
		if ctx.Err() != nil {
			break
		}
		if sel != nil && !sel[i] {
			continue
		}

		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}
		suffix := lang
		seen := suffixCounter[suffix]
		suffixCounter[suffix]++
		if seen > 0 {
			suffix = fmt.Sprintf("%s.%d", suffix, seen+1)
		}

		ext := losslessAudioExt(s.CodecName)
		outPath := filepath.Join(dir, fmt.Sprintf("%s.%s.%s", base, suffix, ext))
		if p, uerr := uniquePath(outPath); uerr == nil {
			outPath = p
		} else {
			pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
			continue
		}

		ffargs := []string{
			"-y", "-i", srcPath,
			"-map", fmt.Sprintf("0:a:%d", i),
			"-vn", "-sn",
			"-c:a", "copy",
		}
		if ext == "wav" {
			ffargs = append(ffargs, "-rf64", "auto")
		}
		ffargs = append(ffargs, outPath)

		layout := s.ChannelLayout
		if layout == "" {
			layout = fmt.Sprintf("%dch", s.Channels)
		}
		fmt.Printf("    %s %s %s\n",
			pterm.Gray(fmt.Sprintf("Audio %d:", i+1)),
			pterm.LightWhite(filepath.Base(outPath)),
			pterm.LightBlue(fmt.Sprintf("(%s %s, 1:1)", strings.ToUpper(s.CodecName), layout)),
		)

		if err := runFFmpegSub(ctx, ffargs); err != nil {
			if !errors.Is(err, context.Canceled) {
				pErr.Printf("    × %s: %v\n", filepath.Base(outPath), err)
			}
			_ = os.Remove(outPath)
			continue
		}
		pOK.Printf("    ✓ %s\n", filepath.Base(outPath))
	}
}

// extractSubsLossless copies every selected subtitle track 1:1, keeping its
// native format (ASS stays ASS, PGS stays SUP, VobSub stays IDX/SUB). The only
// unavoidable change is mov_text, which has no standalone container and is
// written as SRT (text 1:1). The SRT cleaner is NOT run here.
func extractSubsLossless(ctx context.Context, srcPath string, streams *ffprobeOutput, sel map[int]bool) {
	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	dir := filepath.Dir(srcPath)

	var subStreams []ffprobeStream
	for _, s := range streams.Streams {
		if s.CodecType == "subtitle" {
			subStreams = append(subStreams, s)
		}
	}
	if len(subStreams) == 0 {
		fmt.Println(pterm.Gray("  → No subtitle tracks found."))
		return
	}
	want := len(subStreams)
	if sel != nil {
		want = len(sel)
		if want == 0 {
			fmt.Println(pterm.Gray("  → Subtitles skipped (not selected)."))
			return
		}
	}
	fmt.Println(pterm.Gray(fmt.Sprintf("  → Extracting %d subtitle track(s) (1:1, no cleaning)...", want)))

	suffixCounter := map[string]int{}
	for i, s := range subStreams {
		if ctx.Err() != nil {
			break
		}
		if sel != nil && !sel[i] {
			continue
		}

		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}
		suffix := lang
		if s.Disposition.Forced == 1 {
			suffix += ".forced"
		}
		if s.Disposition.HearingImpaired == 1 {
			suffix += ".sdh"
		}
		seen := suffixCounter[suffix]
		suffixCounter[suffix]++
		if seen > 0 {
			suffix = fmt.Sprintf("%s.%d", suffix, seen+1)
		}

		codec := strings.ToLower(s.CodecName)
		var outPath string
		var ffargs []string
		note := ""

		switch codec {
		case "ass", "ssa":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.ass", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{"-y", "-i", srcPath, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "copy", outPath}
		case "webvtt":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.vtt", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{"-y", "-i", srcPath, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "copy", outPath}
		case "mov_text":
			// No standalone mov_text container exists — SRT is the text-lossless target.
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.srt", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{"-y", "-i", srcPath, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "srt", outPath}
			note = "(mov_text → SRT, text 1:1)"
		case "subrip", "srt":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.srt", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{"-y", "-i", srcPath, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "copy", outPath}
		case "hdmv_pgs_subtitle":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.sup", base, suffix))
			if p, uerr := uniquePath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{"-y", "-i", srcPath, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "copy", outPath}
		case "dvd_subtitle":
			outPath = filepath.Join(dir, fmt.Sprintf("%s.%s.idx", base, suffix))
			if p, uerr := uniqueVobSubPath(outPath); uerr == nil {
				outPath = p
			} else {
				pWarn.Printf("    × Track %d skipped: %v\n", i+1, uerr)
				continue
			}
			ffargs = []string{"-y", "-i", srcPath, "-map", fmt.Sprintf("0:s:%d", i), "-c:s", "copy", outPath}
		default:
			pWarn.Printf("    × Track %d: unknown sub codec '%s' (skipped)\n", i+1, codec)
			continue
		}

		if note != "" {
			fmt.Printf("    %s %s %s\n",
				pterm.Gray(fmt.Sprintf("Sub %d:", i+1)),
				pterm.LightWhite(filepath.Base(outPath)),
				pterm.LightYellow(note))
		}

		if err := runFFmpegSub(ctx, ffargs); err != nil {
			if !errors.Is(err, context.Canceled) {
				pErr.Printf("    × %s: %v\n", filepath.Base(outPath), err)
			}
			_ = os.Remove(outPath)
			if strings.HasSuffix(outPath, ".idx") {
				_ = os.Remove(strings.TrimSuffix(outPath, ".idx") + ".sub")
			}
			continue
		}
		pOK.Printf("    ✓ %s\n", filepath.Base(outPath))
	}
}

// ----------------------------------------------------------------------------
// -join entry point
// ----------------------------------------------------------------------------

func runJoinMode(ctx context.Context, args []string) {
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("Join  (lossless 1:1 mux → MKV)")
	fmt.Println()

	if len(args) == 0 {
		showUsageSplitJoin()
		return
	}

	mkvs, mp4s, audios, srts, others := categorizeArgs(args)
	if len(others) > 0 {
		pErr.Printf("Unknown file types: %v\n", others)
		fmt.Println()
		showUsageSplitJoin()
		return
	}
	videos := append(append([]string{}, mkvs...), mp4s...)

	switch {
	case len(videos) == 0:
		pErr.Println("Please drop exactly ONE video (the .NoSound picture) plus audio/subtitle files.")
		fmt.Println()
		showUsageSplitJoin()
	case len(videos) > 1:
		pErr.Println("Please drop only ONE video file as the base (plus audio/subtitle files).")
		fmt.Println()
		showUsageSplitJoin()
	case len(audios) == 0 && len(srts) == 0:
		pErr.Println("Please drop at least one audio or subtitle file in addition to the video.")
		fmt.Println()
		showUsageSplitJoin()
	default:
		runJoinLossless(ctx, videos[0], audios, srts)
	}
}

// runJoinLossless muxes a (silent) picture track plus external audio/subtitle
// files into a single MKV, copying every stream 1:1. Only the picture of the
// base is ever used (any audio/subtitles it carries are ignored), and the
// result is always a fresh ".joined.mkv", so the source is never touched.
func runJoinLossless(ctx context.Context, videoPath string, audioPaths, srtPaths []string) {
	abs, _ := filepath.Abs(videoPath)
	pInfo.Printf("» %s + %d Audio + %d Sub\n",
		filepath.Base(abs), len(audioPaths), len(srtPaths))

	if _, err := os.Stat(abs); err != nil {
		pErr.Printf("  File not found: %v\n", err)
		return
	}
	for _, a := range audioPaths {
		if _, err := os.Stat(a); err != nil {
			pErr.Printf("  Audio file not found: %v\n", err)
			return
		}
	}
	for _, srt := range srtPaths {
		if _, err := os.Stat(srt); err != nil {
			pErr.Printf("  Subtitle file not found: %v\n", err)
			return
		}
	}

	streams, err := probeStreams(ctx, abs)
	if err != nil {
		pErr.Printf("  Probe failed: %v\n", err)
		return
	}

	// The base contributes its picture only; any audio or subtitles it might
	// carry are simply not used. We only confirm there IS a video track to
	// build from (a fresh ".joined.mkv" is always written, the source is never
	// touched, so there is nothing to warn about).
	hasVideo := false
	for _, s := range streams.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			hasVideo = true
			break
		}
	}
	if !hasVideo {
		pErr.Println("  The base file has no video track — nothing to build a picture from.")
		return
	}

	base := trimToolSuffixes(strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)))
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	dir := filepath.Dir(abs)
	outPath := filepath.Join(dir, base+".joined.mkv")

	// Never overwrite an existing file (incl. the base itself): pick a numbered
	// name (.joined2.mkv, …) instead.
	if _, outErr := os.Stat(outPath); outPath == abs || outErr == nil {
		const maxCandidates = 100
		found := false
		for n := 2; n <= maxCandidates; n++ {
			candidate := filepath.Join(dir, fmt.Sprintf("%s.joined%d.mkv", base, n))
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				outPath = candidate
				found = true
				break
			}
		}
		if !found {
			pErr.Printf("  No free output file name found (checked up to .joined%d.mkv)\n", maxCandidates)
			return
		}
		pInfo.Printf("  Output name already taken — writing as %s\n", filepath.Base(outPath))
	}

	useExternalAudio := len(audioPaths) > 0
	srtInputStart := 1 + len(audioPaths)

	buildArgs := func(withSubs bool) []string {
		a := []string{"-y", "-i", abs}
		for _, au := range audioPaths {
			audAbs, _ := filepath.Abs(au)
			a = append(a, "-i", audAbs)
		}
		if withSubs {
			for _, srt := range srtPaths {
				srtAbs, _ := filepath.Abs(srt)
				a = append(a, "-i", srtAbs)
			}
		}
		// Picture of the base only (no cover art).
		a = append(a, "-map", "0:V:0")
		if useExternalAudio {
			for i := range audioPaths {
				a = append(a, "-map", fmt.Sprintf("%d:a:0", i+1))
			}
		}
		if withSubs {
			for i := range srtPaths {
				a = append(a, "-map", fmt.Sprintf("%d:s:0", srtInputStart+i))
			}
		}
		// Everything copied 1:1.
		a = append(a, "-c:v", "copy")
		if useExternalAudio {
			a = append(a, "-c:a", "copy")
		} else {
			a = append(a, "-an")
		}
		if withSubs && len(srtPaths) > 0 {
			a = append(a, "-c:s", "copy")
		}
		// Guard against negative/non-monotonic source timestamps (see
		// writeNoSoundVideoLossless): without this the muxer can reject the
		// first packets. Harmless for clean inputs (nothing to shift).
		a = append(a, "-avoid_negative_ts", "make_zero")

		// Default audio: German preferred, else first track.
		defaultAudioIdx := 0
		if useExternalAudio {
			for i, au := range audioPaths {
				if langFromFilename(au) == "ger" {
					defaultAudioIdx = i
					break
				}
			}
		}
		for i := range audioPaths {
			lang := langFromFilename(audioPaths[i])
			a = append(a, fmt.Sprintf("-metadata:s:a:%d", i), fmt.Sprintf("language=%s", lang))
			if i == defaultAudioIdx {
				a = append(a, fmt.Sprintf("-disposition:a:%d", i), "default")
			} else {
				a = append(a, fmt.Sprintf("-disposition:a:%d", i), "0")
			}
		}
		if withSubs {
			for i, srt := range srtPaths {
				lang, forced, sdh := parseSubTags(srt)
				a = append(a, fmt.Sprintf("-metadata:s:s:%d", i), fmt.Sprintf("language=%s", lang))
				switch {
				case forced:
					a = append(a, fmt.Sprintf("-disposition:s:%d", i), "forced")
				case sdh:
					a = append(a, fmt.Sprintf("-disposition:s:%d", i), "hearing_impaired")
				default:
					a = append(a, fmt.Sprintf("-disposition:s:%d", i), "0")
				}
			}
		}
		a = append(a, outPath)
		return a
	}

	// Show the plan.
	for _, au := range audioPaths {
		lang := langFromFilename(au)
		fmt.Printf("  %s %s → %s\n", pterm.Gray("• Audio"),
			pterm.LightWhite(filepath.Base(au)), pterm.LightCyan(lang))
	}
	for _, srt := range srtPaths {
		lang, forced, sdh := parseSubTags(srt)
		tag := ""
		if forced {
			tag = pterm.LightRed(" [Forced]")
		} else if sdh {
			tag = pterm.LightYellow(" [SDH]")
		}
		fmt.Printf("  %s %s → %s%s\n", pterm.Gray("• Sub  "),
			pterm.LightWhite(filepath.Base(srt)), pterm.LightCyan(lang), tag)
	}

	fmt.Println(pterm.Gray("  → Building MKV (1:1, no re-encode)..."))
	durationSec, _ := strconv.ParseFloat(streams.Format.Duration, 64)

	err = runFFmpeg(ctx, buildArgs(true), durationSec, 1, 1, 0)
	if err == nil {
		pOK.Printf("    ✓ %s\n", filepath.Base(outPath))
		return
	}
	if errors.Is(err, context.Canceled) {
		_ = os.Remove(outPath)
		return
	}
	_ = os.Remove(outPath)

	// Subtitle copy can fail on a malformed file — retry without subtitles.
	if len(srtPaths) > 0 {
		pWarn.Println("  Join with subtitles failed, trying without subtitles...")
		if err2 := runFFmpeg(ctx, buildArgs(false), durationSec, 1, 1, 0); err2 != nil {
			_ = os.Remove(outPath)
			pErr.Printf("  Join failed even without subtitles: %v\n", err2)
			return
		}
		pOK.Printf("    ✓ %s %s\n", filepath.Base(outPath),
			pterm.LightYellow("(without subtitles — check the subtitle files)"))
		return
	}

	pErr.Printf("  Join failed: %v\n", err)
}

// showUsageSplitJoin explains the lossless -split / -join modes.
func showUsageSplitJoin() {
	pInfo.Println("Lossless usage (1:1 copy, no re-encode, no cleaning):")
	fmt.Println()
	fmt.Printf("  %s\n", pterm.LightWhite("-split  <video files / folders>"))
	fmt.Printf("     %s\n", pterm.Gray("→ .NoSound.<mp4|mkv> (silent picture, stream copy)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.<ac3|dts|eac3|m4a|flac|...> (each audio track, native, 1:1)"))
	fmt.Printf("     %s\n", pterm.Gray("→ .<lang>.<srt|ass|sup|idx> (each subtitle, untouched)"))
	fmt.Printf("     %s\n", pterm.Gray("→ a single file asks which tracks to extract; a folder takes all"))
	fmt.Println()
	fmt.Printf("  %s\n", pterm.LightWhite("-join   <one .NoSound video> + <audio/subtitle files>"))
	fmt.Printf("     %s\n", pterm.Gray("→ rebuilds a single \".joined.mkv\" with everything copied 1:1"))
	fmt.Printf("     %s\n", pterm.Gray("→ only the picture of the base is used; the source is never changed"))
	fmt.Println()
	fmt.Printf("  %s\n", pterm.Gray("Tip: -split then -join is a true lossless round-trip. For DaVinci-ready"))
	fmt.Printf("  %s\n", pterm.Gray("     output (AAC audio, cleaned subtitles) use -davinci instead."))
	fmt.Println()
}

// ════════════════════════════════════════════════════════════════════════════
// SRT-Cleaner (integrated from SRTCleaner.go)
//
//   - State-machine parser (robust against blank lines in text, missing numbers)
//   - HTML tags (<i>, <font...>) + ASS tags ({\an8}) stripped
//   - HTML entities unescaped (&amp; → &)
//   - Invisible Unicode characters discarded (soft-hyphen, ZWSP …)
//   - Empty blocks + config phrases filtered, blocks re-numbered
//   - 1ms micro-gap correction, CRLF output
//   - Atomic in-place write (temp + rename) → no SRT loss on abort
//
// ════════════════════════════════════════════════════════════════════════════

type srtBlock struct {
	Number    int
	Timestamp string
	StartMS   int
	EndMS     int
	Text      string
}

type srtState int

const (
	srtExpectNumber srtState = iota
	srtExpectTimestamp
	srtExpectText
)

var (
	srtTSRegex = regexp.MustCompile(
		`^(\d{2}:\d{2}:\d{2}[.,]\d{3})\s+-->\s+(\d{2}:\d{2}:\d{2}[.,]\d{3})`)
	srtHTMLTagRegex = regexp.MustCompile(`<[^>]+>`)
	srtASSTagRegex  = regexp.MustCompile(`\{[^}]+\}`)

	srtPhrasesOnce  sync.Once
	srtPhrasesCache []string
)

// cp1252High maps the bytes 0x80–0x9F to their Windows-1252 characters
// (typographic quotes, dashes, €, …). All other bytes match ISO-8859-1, so
// rune(b) is already correct for them.
var cp1252High = [32]rune{
	'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8D, 'Ž', 0x8F,
	0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9D, 'ž', 'Ÿ',
}

// cleanSRT cleans a SRT file in-place (atomic write).
func cleanSRT(srtPath string) {
	// Safety cap: real subtitle files are a few hundred KB at most. Anything
	// bigger is almost certainly a mislabeled binary — decoding it would cost
	// up to 4× its size in RAM, so leave such files untouched.
	const maxSRTBytes = 20 * 1024 * 1024
	if info, err := os.Stat(srtPath); err != nil || info.Size() > maxSRTBytes {
		return
	}
	raw, err := os.ReadFile(srtPath)
	if err != nil {
		return
	}
	var decoded string
	if utf8.Valid(raw) {
		decoded = string(raw)
	} else {
		// ANSI fallback: Windows files are practically always Windows-1252,
		// which equals ISO-8859-1 except for the 0x80–0x9F block.
		runes := make([]rune, len(raw))
		for i, b := range raw {
			if b >= 0x80 && b <= 0x9F {
				runes[i] = cp1252High[b-0x80]
			} else {
				runes[i] = rune(b)
			}
		}
		decoded = string(runes)
	}
	content := srtNormalize(decoded)
	blocks, _ := srtParse(content)
	if len(blocks) == 0 {
		return // No parseable content → leave original untouched.
	}
	clean, _ := srtFilter(blocks, srtCleanerPhrases())
	if len(clean) == 0 {
		pWarn.Printf("    SRT cleaner: all blocks filtered – keeping original: %s\n",
			filepath.Base(srtPath))
		return
	}
	srtApplyMicroGaps(clean)

	var sb strings.Builder
	for i, b := range clean {
		sb.WriteString(strconv.Itoa(b.Number))
		sb.WriteString("\r\n")
		sb.WriteString(b.Timestamp)
		sb.WriteString("\r\n")
		sb.WriteString(strings.ReplaceAll(b.Text, "\n", "\r\n"))
		sb.WriteString("\r\n")
		if i < len(clean)-1 {
			sb.WriteString("\r\n")
		}
	}
	sb.WriteString("\r\n")

	// Unique temp name in the same directory (same volume keeps the final
	// rename atomic); a static name would collide when two instances clean
	// the same file concurrently.
	tmpFile, err := os.CreateTemp(filepath.Dir(srtPath), filepath.Base(srtPath)+".*.tmp")
	if err != nil {
		return
	}
	tmp := tmpFile.Name()
	_, werr := tmpFile.WriteString(sb.String())
	cerr := tmpFile.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, srtPath); err != nil {
		_ = os.Remove(tmp)
	}
}

// srtNormalize removes BOM and normalises line endings to \n.
func srtNormalize(s string) string {
	s = strings.TrimPrefix(s, "\xEF\xBB\xBF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// srtParse is a state machine with lookahead buffer for blank lines in text.
func srtParse(content string) ([]srtBlock, int) {
	var blocks []srtBlock
	malformed := 0

	var (
		state         = srtExpectNumber
		curNum        int
		curTS         string
		curStartMS    int
		curEndMS      int
		curLines      []string
		pendingBlanks int
	)

	commitBlock := func() {
		if curTS == "" {
			return
		}
		blocks = append(blocks, srtBlock{
			Number:    curNum,
			Timestamp: curTS,
			StartMS:   curStartMS,
			EndMS:     curEndMS,
			Text:      strings.Join(curLines, "\n"),
		})
		curTS = ""
		curLines = nil
		pendingBlanks = 0
	}

	isBlockStart := func(trimmed string) bool {
		if _, err := strconv.Atoi(trimmed); err == nil {
			return true
		}
		return srtTSRegex.MatchString(trimmed)
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)

		switch state {
		case srtExpectNumber:
			if trimmed == "" {
				continue
			}
			n, err := strconv.Atoi(trimmed)
			if err != nil {
				if m := srtTSRegex.FindStringSubmatch(trimmed); m != nil {
					malformed++
					curNum = len(blocks) + 1
					curTS = m[1] + " --> " + m[2]
					curStartMS = srtParseTimeMS(m[1])
					curEndMS = srtParseTimeMS(m[2])
					state = srtExpectText
					continue
				}
				malformed++
				continue
			}
			curNum = n
			state = srtExpectTimestamp

		case srtExpectTimestamp:
			if trimmed == "" {
				malformed++
				state = srtExpectNumber
				continue
			}
			m := srtTSRegex.FindStringSubmatch(trimmed)
			if m == nil {
				malformed++
				state = srtExpectNumber
				continue
			}
			curTS = m[1] + " --> " + m[2]
			curStartMS = srtParseTimeMS(m[1])
			curEndMS = srtParseTimeMS(m[2])
			state = srtExpectText

		case srtExpectText:
			if trimmed == "" {
				pendingBlanks++
				continue
			}
			if pendingBlanks > 0 {
				if isBlockStart(trimmed) {
					commitBlock()
					state = srtExpectNumber
					n, err := strconv.Atoi(trimmed)
					if err != nil {
						if m := srtTSRegex.FindStringSubmatch(trimmed); m != nil {
							malformed++
							curNum = len(blocks) + 1
							curTS = m[1] + " --> " + m[2]
							curStartMS = srtParseTimeMS(m[1])
							curEndMS = srtParseTimeMS(m[2])
							state = srtExpectText
						} else {
							malformed++
						}
					} else {
						curNum = n
						state = srtExpectTimestamp
					}
				} else {
					for i := 0; i < pendingBlanks; i++ {
						curLines = append(curLines, "")
					}
					pendingBlanks = 0
					curLines = append(curLines, line)
				}
			} else {
				curLines = append(curLines, line)
			}
		}
	}

	if state == srtExpectText {
		commitBlock()
	}
	return blocks, malformed
}

// srtFilter strips tags/entities/invisible chars and discards empty + config blocks.
func srtFilter(blocks []srtBlock, badPhrases []string) ([]srtBlock, int) {
	var clean []srtBlock
	removed := 0
	counter := 1

	for _, b := range blocks {
		b.Text = srtHTMLTagRegex.ReplaceAllString(b.Text, "")
		b.Text = srtASSTagRegex.ReplaceAllString(b.Text, "")
		b.Text = strings.TrimSpace(html.UnescapeString(b.Text))

		var cleanedLines []string
		for _, l := range strings.Split(b.Text, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				cleanedLines = append(cleanedLines, l)
			}
		}
		b.Text = strings.Join(cleanedLines, "\n")

		// Discard invisible Unicode characters (soft-hyphen, ZWSP, BOM …) —
		// from the output text itself, not just for the checks below.
		b.Text = strings.Map(func(r rune) rune {
			switch r {
			case '\u00AD', '\u200B', '\u200C', '\u200D', '\uFEFF':
				return -1
			}
			return r
		}, b.Text)

		if strings.TrimSpace(b.Text) == "" {
			removed++
			continue
		}

		// All filter phrases come from SRTCleaner_config.txt — including the
		// former hard-coded "untertitel", which is now a config entry the user
		// can remove or keep.
		textLower := strings.ToLower(b.Text)
		isBad := false
		for _, phrase := range badPhrases {
			if strings.HasPrefix(phrase, "=") {
				if textLower == strings.TrimPrefix(phrase, "=") {
					isBad = true
					break
				}
			} else if strings.Contains(textLower, phrase) {
				isBad = true
				break
			}
		}
		if isBad {
			removed++
			continue
		}

		b.Number = counter
		counter++
		clean = append(clean, b)
	}
	return clean, removed
}

// srtApplyMicroGaps inserts a 1ms gap when block B starts exactly where A ends.
func srtApplyMicroGaps(blocks []srtBlock) {
	for i := 1; i < len(blocks); i++ {
		if blocks[i].StartMS == blocks[i-1].EndMS {
			blocks[i].StartMS++
			blocks[i].Timestamp = srtFormatTimeMS(blocks[i].StartMS) +
				" --> " + srtFormatTimeMS(blocks[i].EndMS)
		}
	}
}

// srtCleanerPhrases loads the phrase list once (lazy) from the exe directory.
func srtCleanerPhrases() []string {
	srtPhrasesOnce.Do(func() {
		exePath, err := os.Executable()
		if err != nil {
			return
		}
		srtPhrasesCache = loadOrCreateSRTConfig(
			filepath.Join(filepath.Dir(exePath), "SRTCleaner_config.txt"))
	})
	return srtPhrasesCache
}

func loadOrCreateSRTConfig(configPath string) []string {
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultConfig := "# SRTCleaner Configuration\n" +
			"# ------------------------\n" +
			"# Enter phrases here (case is ignored).\n" +
			"#\n" +
			"# STANDARD MODE (contained anywhere):\n" +
			"#   A phrase without prefix deletes every block in which it appears.\n" +
			"#   Example: \"2017\" also deletes \"In the year 2017\".\n" +
			"#\n" +
			"# EXACT MODE (strict equality):\n" +
			"#   A line prefixed with \"=\" deletes a block only if the text\n" +
			"#   matches EXACTLY. Example: \"=2017\".\n" +
			"#\n" +
			"untertitel\n" +
			"funk, 2017\n" +
			"©\n" +
			"whisper\n" +
			"copyright\n" +
			"uebersetzung von\n"
		if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err == nil {
			fmt.Println(pterm.Gray("  · SRTCleaner config created: " +
				filepath.Base(configPath)))
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var phrases []string
	for _, line := range strings.Split(srtNormalize(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		phrases = append(phrases, strings.ToLower(line))
	}
	return phrases
}

func srtParseTimeMS(ts string) int {
	ts = strings.ReplaceAll(ts, ".", ",")
	var h, m, s, ms int
	_, _ = fmt.Sscanf(ts, "%d:%d:%d,%d", &h, &m, &s, &ms)
	return ((h*60+m)*60+s)*1000 + ms
}

func srtFormatTimeMS(total int) string {
	ms := total % 1000
	total /= 1000
	s := total % 60
	total /= 60
	m := total % 60
	h := total / 60
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

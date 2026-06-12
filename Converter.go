//go:build windows && amd64

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pterm/pterm"
)

// ----------------------------------------------------------------------------
// convJob: FFmpeg argument builder
// ----------------------------------------------------------------------------

type convJob struct {
	inputPath     string
	outputPath    string
	nvencOpts     []string
	vfOpts        []string
	withSubs      bool
	audioCopy     bool
	isTS          bool
	isMKV         bool
	noAudio       bool
	pureAudioCopy bool
	audioStreams  []AudioStreamInfo
	subCodecs     []string
}

// staleStatsTagArgs deletes the per-track statistics tags (BPS,
// NUMBER_OF_BYTES, …) that mkvmerge wrote into the SOURCE file. Copied
// unchanged they describe the old streams and show absurd bitrates in
// MediaInfo. The muxer writes a fresh DURATION on its own.
func staleStatsTagArgs() []string {
	tags := []string{
		"BPS", "DURATION", "NUMBER_OF_FRAMES", "NUMBER_OF_BYTES",
		"_STATISTICS_WRITING_APP", "_STATISTICS_WRITING_DATE_UTC", "_STATISTICS_TAGS",
	}
	args := make([]string, 0, len(tags)*4)
	for _, t := range tags {
		args = append(args, "-metadata:s", t+"=", "-metadata:s", t+"-eng=")
	}
	return args
}

func (j convJob) buildConvertArgs() []string {
	a := make([]string, 0, 32)
	a = append(a, "-y")
	// Decode strictly on the CPU: NVDEC (-hwaccel cuda / av1_cuvid) is removed
	// because it TDR-crashes the GPU driver on extreme-bitrate HEVC. Encoding
	// stays on the GPU via hevc_nvenc (an output option, set in nvencOpts).
	if j.isTS {
		a = append(a, "-err_detect", "ignore_err", "-fflags", "+genpts+discardcorrupt")
	}
	a = append(a, "-i", j.inputPath, "-map", "0:V:0")
	if !j.noAudio {
		a = append(a, "-map", "0:a?")
	}
	if j.withSubs && !j.noAudio {
		a = append(a, "-map", "0:s?")
	}
	// Attachments (fonts, cover art) ride along independently of subtitles.
	if j.isMKV && !j.noAudio {
		a = append(a, "-map", "0:t?")
	}
	if j.isTS {
		a = append(a, "-map", "-0:d?", "-avoid_negative_ts", "make_zero")
	}
	a = append(a, j.vfOpts...)
	a = append(a, j.nvencOpts...)
	if j.noAudio {
		a = append(a, "-an")
	} else if j.pureAudioCopy {
		a = append(a, "-c:a", "copy")
	} else {
		a = append(a, buildPerStreamAudioArgs(j.audioStreams, !j.audioCopy, j.isTS)...)
	}
	if j.withSubs && !j.noAudio {
		a = append(a, subtitleCodecArgs(j.subCodecs)...)
	} else {
		a = append(a, "-sn")
	}
	if j.isMKV && !j.noAudio {
		a = append(a, "-c:t", "copy")
	}
	a = append(a, staleStatsTagArgs()...)
	return append(a, j.outputPath)
}

func (j convJob) buildRemuxArgs() []string {
	a := make([]string, 0, 32)
	a = append(a, "-y")
	if j.isTS {
		a = append(a, "-err_detect", "ignore_err", "-fflags", "+genpts+discardcorrupt")
	}
	a = append(a, "-i", j.inputPath, "-map", "0:V:0")
	if !j.noAudio {
		a = append(a, "-map", "0:a?")
	}
	if j.withSubs && !j.noAudio {
		a = append(a, "-map", "0:s?")
	}
	if j.isMKV && !j.noAudio {
		a = append(a, "-map", "0:t?")
	}
	if j.isTS {
		a = append(a, "-map", "-0:d?", "-avoid_negative_ts", "make_zero")
	}
	a = append(a, "-c:v", "copy")
	if j.noAudio {
		a = append(a, "-an", "-sn")
	} else {
		if j.pureAudioCopy {
			a = append(a, "-c:a", "copy")
		} else {
			a = append(a, buildPerStreamAudioArgs(j.audioStreams, !j.audioCopy, j.isTS)...)
		}
		if j.withSubs {
			a = append(a, subtitleCodecArgs(j.subCodecs)...)
		} else {
			a = append(a, "-sn")
		}
		if j.isMKV {
			a = append(a, "-c:t", "copy")
		}
	}
	a = append(a, staleStatsTagArgs()...)
	return append(a, j.outputPath)
}

// ----------------------------------------------------------------------------
// Subtitle codec logic
// ----------------------------------------------------------------------------

func subtitleCodecArgs(subCodecs []string) []string {
	if len(subCodecs) == 0 {
		return []string{"-sn"}
	}
	args := make([]string, 0, len(subCodecs)*2)
	for i, c := range subCodecs {
		sel := fmt.Sprintf("-c:s:%d", i)
		if subTextConvertibleToSRT(c) {
			args = append(args, sel, "srt")
		} else {
			args = append(args, sel, "copy")
		}
	}
	return args
}

// subTextConvertibleToSRT reports whether a subtitle codec is text-based and can
// be remapped to SRT. Bitmap formats (PGS, VobSub, DVB, XSUB) cannot be turned
// into SRT and are copied through unchanged instead.
func subTextConvertibleToSRT(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "text":
		return true
	}
	return false
}

// ----------------------------------------------------------------------------
// Audio logic (DaVinci Resolve safe)
// ----------------------------------------------------------------------------

// DaVinci Resolve decodes AAC only with the classic MPEG-4 channel
// configurations 1-7 and max. 48 kHz. FFmpeg signals 7.1 AAC as
// channelConfiguration 12, which Resolve cannot read (silent track), so 8ch
// material is downmixed to 5.1 and everything is resampled to 48 kHz.
const davinciSafeChannelLayoutsFilter = "aformat=sample_rates=48000:channel_layouts=mono|stereo|5.1"

const davinciMaxAudioChannels = 6
const davinciMaxSampleRate = 48000

func isDavinciIncompatibleAudio(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "ac3", "eac3", "dts", "truehd", "mlp", "opus", "vorbis", "flac":
		return true
	}
	return false
}

var davinciSafeLayouts = map[string]bool{
	"mono":      true,
	"stereo":    true,
	"5.1":       true,
	"5.1(back)": true,
}

func isDavinciSafeLayout(layout string, channels int) bool {
	l := strings.ToLower(strings.TrimSpace(layout))
	if davinciSafeLayouts[l] {
		return true
	}
	if l == "" || l == "unknown" {
		return channels == 1 || channels == 2
	}
	return false
}

func needsAudioReencode(codec, layout string, channels, sampleRate int) bool {
	return isDavinciIncompatibleAudio(codec) ||
		!isDavinciSafeLayout(layout, channels) ||
		sampleRate > davinciMaxSampleRate
}

// aacEncodeParams returns the effective output channel count (capped at 5.1 by
// the downmix filter) and the AAC target bitrate in kbps for a re-encode.
func aacEncodeParams(channels int) (effCh, brKbps int) {
	effCh = channels
	if effCh <= 0 {
		effCh = 2
	}
	if effCh > davinciMaxAudioChannels {
		effCh = davinciMaxAudioChannels
	}
	brKbps = effCh * appSettings.audioKbpsPerChannel
	if brKbps < appSettings.fallbackAudioBitrate {
		brKbps = appSettings.fallbackAudioBitrate
	}
	if brKbps > 640 {
		brKbps = 640
	}
	return effCh, brKbps
}

func buildPerStreamAudioArgs(streams []AudioStreamInfo, forceAACAll bool, isTS bool) []string {
	var args []string
	af := davinciSafeChannelLayoutsFilter
	if isTS {
		af = "aresample=async=1:first_pts=0," + af
	}
	for i, s := range streams {
		sel := fmt.Sprintf(":a:%d", i)

		// Source language tags are carried over by FFmpeg automatically; untagged
		// tracks stay untagged ("und") instead of being stamped with a guess.

		if forceAACAll || needsAudioReencode(s.Codec, s.Layout, s.Channels, s.SampleRate) {
			ch, br := aacEncodeParams(s.Channels)
			args = append(args,
				"-c"+sel, "aac",
				"-b"+sel, fmt.Sprintf("%dk", br),
				"-filter"+sel, af,
			)
			title := fmt.Sprintf("AAC %dch (orig: %s)", ch, strings.ToUpper(s.Codec))
			args = append(args, fmt.Sprintf("-metadata:s%s", sel), fmt.Sprintf("title=%s", title))
		} else {
			args = append(args, "-c"+sel, "copy")
			if s.Title == "" {
				title := fmt.Sprintf("%s %dch (Original)", strings.ToUpper(s.Codec), s.Channels)
				args = append(args, fmt.Sprintf("-metadata:s%s", sel), fmt.Sprintf("title=%s", title))
			}
		}
	}
	return args
}

// ----------------------------------------------------------------------------
// Cascade: dynamic attempt list + FFmpeg error classification
// ----------------------------------------------------------------------------

type cascadeAttempt struct {
	label                        string
	audioCopy, withSubs, noAudio bool
}

// buildCascadeAttempts assembles only the rungs that can actually differ for
// this source: SUBS rungs need subtitles, AAC rungs need audio that is not in
// -copyaudio mode, the VIDEO-ONLY rung needs audio it could drop. This avoids
// re-running byte-identical FFmpeg calls after a failure.
func buildCascadeAttempts(hasSubs, hasAudio, pureCopy bool) []cascadeAttempt {
	var at []cascadeAttempt
	if hasSubs {
		at = append(at, cascadeAttempt{"SUBS+ACOPY", true, true, false})
		if hasAudio && !pureCopy {
			at = append(at, cascadeAttempt{"SUBS+AAC", false, true, false})
		}
	}
	at = append(at, cascadeAttempt{"NO-SUBS+ACOPY", true, false, false})
	if hasAudio && !pureCopy {
		at = append(at, cascadeAttempt{"NO-SUBS+AAC", false, false, false})
	}
	if hasAudio {
		at = append(at, cascadeAttempt{"VIDEO-ONLY (fallback)", false, false, true})
	}
	for i := range at {
		at[i].label = fmt.Sprintf("%s %d/%d", at[i].label, i+1, len(at))
	}
	return at
}

type ffmpegFailKind int

const (
	failUnknown ffmpegFailKind = iota
	failSubtitle
	failAudio
	failVideo
)

// classifyFFmpegError groups a failure by its last FFmpeg stderr line so the
// cascade can skip rungs that cannot fix it. Heuristic by design: unknown
// messages simply fall through to the regular next rung.
func classifyFFmpegError(msg string) ffmpegFailKind {
	m := strings.ToLower(msg)
	contains := func(keys ...string) bool {
		for _, k := range keys {
			if strings.Contains(m, k) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("nvenc", "cuda", "cuvid", "no capable devices"):
		return failVideo
	case contains("subtitle", "subrip", "mov_text", "hdmv_pgs", "dvb_sub",
		"dvd_sub", "webvtt", "vobsub"):
		return failSubtitle
	case contains("audio", "aac", "ac-3", "eac3", "e-ac-3", "dts", "dca",
		"truehd", "mlp", "opus", "vorbis", "flac", "pcm_",
		"channel layout", "sample rate", "aformat", "aresample"):
		return failAudio
	}
	return failUnknown
}

func allAudioSafeAAC(streams []AudioStreamInfo) bool {
	for _, s := range streams {
		if !strings.EqualFold(s.Codec, "aac") {
			return false
		}
		if !isDavinciSafeLayout(s.Layout, s.Channels) {
			return false
		}
		if s.SampleRate > davinciMaxSampleRate {
			return false
		}
	}
	return true
}

// durationsClose reports whether two probed durations differ by at most 5% —
// used to tell a resumable previous output apart from a name collision.
// Unusable durations (0/N/A) count as close, preserving the old skip behavior.
func durationsClose(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	return a >= b*0.95 && a <= b*1.05
}

// ----------------------------------------------------------------------------
// removeOrRename: robust deletion with fallback rename
// ----------------------------------------------------------------------------

func removeOrRename(path string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}
	for attempt := 0; attempt < 4; attempt++ {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return
		}
		if attempt < 3 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	brokenPath := path + ".broken"
	_ = os.Remove(brokenPath)
	for attempt := 0; attempt < 3; attempt++ {
		if err := os.Rename(path, brokenPath); err == nil {
			pWarn.Printf("Corrupt output not deletable → renamed: %s\n",
				filepath.Base(brokenPath))
			return
		}
		if attempt < 2 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	marker := path + ".invalid"
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0644); err != nil {
		pWarn.Printf("Corrupt output neither deletable nor markable: %s\n", filepath.Base(path))
		return
	}
	pWarn.Printf("Corrupt output blocked → marker set: %s\n", filepath.Base(marker))
}

// ----------------------------------------------------------------------------
// processFile: main per-file processing logic
// ----------------------------------------------------------------------------

func processFile(ctx context.Context, cfg *AppConfig, filePath string, idx, total int) ProcessResult {
	ext := strings.ToLower(filepath.Ext(filePath))
	base := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	dir := filepath.Dir(filePath)

	fmt.Println()
	pterm.NewStyle(pterm.FgLightCyan, pterm.Bold).Printf("[%d/%d] ", idx, total)
	pterm.NewStyle(pterm.FgLightWhite).Printf("%s%s\n", base, ext)
	result := ProcessResult{InputFile: filePath}

	if ext == ".mkv" {
		for _, suf := range skipInputSuffixes {
			if strings.HasSuffix(strings.ToLower(base), suf) {
				fmt.Println(pterm.Gray("  Skipped: already converted."))
				fmt.Println()
				result.Skipped = true
				return result
			}
		}
	}

	outputDir := filepath.Join(dir, "output")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		result.ErrMsg = fmt.Sprintf("Converter.go: processFile: cannot create output folder: %v", err)
		result.FailedAt = time.Now()
		return result
	}
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	basenameFull := base

	// srcStats is probed lazily: only when an existing output has to be told
	// apart from a name collision (different source, same cleaned name).
	var srcStats *VideoStats
	probeSource := func() bool {
		if srcStats != nil {
			return true
		}
		st, err := getVideoStats(ctx, filePath)
		if err != nil {
			result.ErrMsg = fmt.Sprintf("Converter.go: processFile: FFprobe error: %v", err)
			result.FailedAt = time.Now()
			return false
		}
		srcStats = st
		return true
	}

	collision := false
	for _, suf := range skipSuffixes {
		candidate := filepath.Join(outputDir, basenameFull+suf+".mkv")
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		candStats, probeErr := getVideoStats(ctx, candidate)
		if probeErr != nil {
			if rmErr := os.Remove(candidate); rmErr != nil {
				fmt.Println(pterm.Gray(fmt.Sprintf(
					"  Skipped: previous file not readable and not deletable (%s — likely another instance active).",
					filepath.Base(candidate))))
				fmt.Println()
				result.Skipped = true
				return result
			}
			fmt.Println(pterm.Gray("  · Broken previous file (crash ghost) detected and deleted."))
			continue
		}
		marker := candidate + ".invalid"
		if _, mErr := os.Stat(marker); mErr == nil {
			if rmErr := os.Remove(candidate); rmErr == nil {
				_ = os.Remove(marker)
				fmt.Println(pterm.Gray("  · Corrupt previous file removed: " + filepath.Base(candidate)))
				continue
			}
			fmt.Println(pterm.Gray(fmt.Sprintf(
				"  Skipped: corrupt previous file still blocked (%s).",
				filepath.Base(candidate))))
			fmt.Println()
			result.Skipped = true
			result.ErrMsg = "Corrupt output file blocked"
			return result
		}
		if !probeSource() {
			return result
		}
		if !durationsClose(candStats.DurationSec, srcStats.DurationSec) {
			// Existing output stems from a DIFFERENT source whose cleaned name
			// collides with ours → pick a numbered output name instead of skipping.
			collision = true
			continue
		}
		fmt.Println(pterm.Gray("  Skipped: output file already exists."))
		fmt.Println()
		result.Skipped = true
		return result
	}

	if collision {
		resolved := false
		for n := 2; n <= 99 && !resolved; n++ {
			cand := fmt.Sprintf("%s.%d", basenameFull, n)
			occupied := false
			for _, suf := range skipSuffixes {
				p := filepath.Join(outputDir, cand+suf+".mkv")
				if _, err := os.Stat(p); err != nil {
					continue
				}
				occupied = true
				// A numbered output may already belong to THIS source (resume).
				if cs, e := getVideoStats(ctx, p); e == nil &&
					durationsClose(cs.DurationSec, srcStats.DurationSec) {
					fmt.Println(pterm.Gray("  Skipped: output file already exists (" +
						filepath.Base(p) + ")."))
					fmt.Println()
					result.Skipped = true
					return result
				}
				break
			}
			if !occupied {
				basenameFull = cand
				resolved = true
			}
		}
		if !resolved {
			fmt.Println(pterm.Gray("  Skipped: no free output name found (name collision)."))
			fmt.Println()
			result.Skipped = true
			result.ErrMsg = "Output name collision: no free numbered name"
			return result
		}
		pInfo.Printf("Output name collision — writing as %s.\n",
			pterm.LightCyan(basenameFull))
	}

	fileSizeMB := getFileSizeMB(filePath)
	lockPath := filepath.Join(outputDir, basenameFull+".lock")

	stats := srcStats
	if stats == nil {
		st, err := getVideoStats(ctx, filePath)
		if err != nil {
			result.ErrMsg = fmt.Sprintf("Converter.go: processFile: FFprobe error: %v", err)
			result.FailedAt = time.Now()
			return result
		}
		stats = st
	}
	stats.FileSizeMB = fileSizeMB

	unlock, lockErr := acquireProcessingLock(lockPath, fileSizeMB, filePath)
	if lockErr != nil {
		fmt.Println(pterm.Gray("  Skipped: Another instance is currently processing this file."))
		fmt.Println()
		result.Skipped = true
		return result
	}
	defer unlock()

	for _, suf := range skipSuffixes {
		candidate := filepath.Join(outputDir, basenameFull+suf+".mkv")
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		fmt.Println(pterm.Gray("  Skipped: output file found after acquiring lock (another instance was faster)."))
		fmt.Println()
		result.Skipped = true
		return result
	}

	preview := filepath.Join(outputDir, basenameFull+".preview.mkv")
	if _, err := os.Stat(preview); err == nil {
		if err := os.Remove(preview); err == nil {
			fmt.Println(pterm.Gray("  · Preview removed: " + filepath.Base(preview)))
		}
	}

	if stats.FPSDen == 0 {
		stats.FPSDen = 1
	}
	fps := float64(stats.FPSNum) / float64(stats.FPSDen)
	fmt.Printf("  %s · %s · %s · %s %s · %s\n",
		pterm.LightGreen(strings.ToUpper(stats.VideoCodec)),
		pterm.Cyan(fmt.Sprintf("%dx%d", stats.Width, stats.Height)),
		pterm.LightWhite(fmt.Sprintf("%.2ffps", fps)),
		pterm.LightBlue(strings.ToUpper(stats.AudioCodec)),
		pterm.LightWhite(fmt.Sprintf("%dch", stats.Channels)),
		pterm.Yellow(formatDuration(stats.DurationSec)))

	// HDR-Policy (per Datei, leckt nie in SDR-Dateien): nur noch Erkennung +
	// Hinweis. Der Bitraten-Deckel wird durch HDR NICHT mehr angehoben — er
	// richtet sich allein nach dem Modus: 1080p (Standard) → maxBitrate1080p
	// (8000), behaltenes 4K (-original) → maxBitrateOriginal (18000). Die
	// HDR-Signalisierung (PQ/BT.2020-Tags) trägt buildColorOpts unabhängig.
	effCfg := *cfg
	hdrKind := videoHDRKind(stats)
	if hdrKind != "" {
		pInfo.Printf("%s HDR detected (%s) — bitrate cap %sk (by resolution mode).\n",
			pterm.LightMagenta("›"),
			strings.ToUpper(hdrKind),
			pterm.LightCyan(fmt.Sprintf("%d", effCfg.maxBitrateKbps)))
	}

	bitrateKbps := determineBitrateKbps(stats)
	var calcKbps int64
	switch {
	case bitrateKbps <= 3500:
		calcKbps = bitrateKbps * 95 / 100
	case bitrateKbps >= 9000:
		calcKbps = bitrateKbps * 65 / 100
	default:
		pct := 95 - ((bitrateKbps-3500)*30)/(9000-3500)
		calcKbps = bitrateKbps * pct / 100
	}
	if calcKbps < 1500 {
		calcKbps = 1500
	}
	if calcKbps > effCfg.maxBitrateKbps {
		calcKbps = effCfg.maxBitrateKbps
	}

	// -av1 swaps the target codec: encoder opts, output suffix, "already
	// converted" detection and validation all follow targetCodec.
	targetCodec, outSuffix, codecLabel := "hevc", ".h265", "H.265"
	if cfg.av1 {
		targetCodec, outSuffix, codecLabel = "av1", ".av1", "AV1"
	}

	maxBR := fmt.Sprintf("%dk", calcKbps)
	bufBR := fmt.Sprintf("%dk", calcKbps*2)
	gopSize := calcGOP(stats.FPSNum, stats.FPSDen)
	var nvencOpts []string
	if cfg.av1 {
		nvencOpts = buildAV1Opts(maxBR, bufBR, gopSize)
	} else {
		nvencOpts = buildNVENCOpts(maxBR, bufBR, gopSize)
	}
	// HDR signalling is carried by the color tags copied 1:1 from the source in
	// buildColorOpts (primaries/transfer/colorspace/range — only when present, so
	// nothing is fabricated). Mastering-display / MaxCLL static metadata rides
	// through automatically on stream-copy and on re-encodes without rescaling. We
	// deliberately do NOT reconstruct a -master_display string: a synthesized value
	// is exactly what has aborted HDR conversions in the past.
	nvencOpts = append(nvencOpts, buildColorOpts(stats)...)
	doScale := needsScaling(cfg, stats.Width, stats.Height)

	doConvert, doRemux := false, false
	switch {
	case strings.EqualFold(stats.VideoCodec, targetCodec) && ext == ".mkv" &&
		!doScale && bitrateKbps <= effCfg.maxBitrateKbps && allAudioSafeAAC(stats.AudioStreams):
		newPath := filepath.Join(dir, base+outSuffix+ext)
		if _, statErr := os.Stat(newPath); statErr == nil {
			fmt.Println(pterm.Gray(fmt.Sprintf(
				"  Already %s-MKV (%d kbps) – skipped (target name exists: %s).",
				codecLabel, bitrateKbps, filepath.Base(newPath))))
		} else if err := os.Rename(filePath, newPath); err == nil {
			pOK.Printf("Already %s-MKV (%d kbps) – renamed to %s.\n",
				codecLabel, bitrateKbps, filepath.Base(newPath))
		} else {
			pWarn.Printf("Already %s-MKV (%d kbps) – skipped (rename: %v).\n",
				codecLabel, bitrateKbps, err)
		}
		fmt.Println()
		result.Skipped = true
		return result
	case strings.EqualFold(stats.VideoCodec, targetCodec) && !doScale &&
		bitrateKbps <= effCfg.maxBitrateKbps:
		doRemux = true
	default:
		doConvert = true
	}

	printDavinciAudioInfo(stats.AudioStreams, cfg.copyAudio)

	var vfOpts []string
	if doConvert {
		doDeint := videoIsInterlaced(stats)
		if doDeint {
			pInfo.Printf("%s Interlaced source (%s) — deinterlacing with bwdif.\n",
				pterm.LightMagenta("›"), stats.FieldOrder)
		}
		vfOpts = []string{"-vf", buildVideoFilter(doScale, doDeint)}
	}

	baseJob := convJob{
		inputPath:    filePath,
		nvencOpts:    nvencOpts,
		vfOpts:       vfOpts,
		isTS:         ext == ".ts",
		isMKV:        ext == ".mkv",
		audioStreams: stats.AudioStreams,
		subCodecs:    stats.SubCodecs,
	}

	var outputFile string
	encodingOK := false
	noAudioUsed := false
	var firstConvertErr, lastConvertErr error

	// runCascade walks the dynamic attempt ladder. The FFmpeg error text steers
	// it: subtitle errors disable all SUBS rungs, audio errors disable rungs
	// with the same audio handling, encoder errors abort outright (no rung can
	// fix a broken video encode). A rung that exits 0 but fails validation
	// counts as failed, so the next rung still gets its chance.
	runCascade := func(buildArgs func(convJob) []string, labelPrefix string) {
		attempts := buildCascadeAttempts(
			len(stats.SubCodecs) > 0, len(stats.AudioStreams) > 0, cfg.copyAudio)
		subsFailed := false
		videoFailed := false
		audioModeFailed := map[bool]bool{}
		firstRun := true
		for _, att := range attempts {
			if ctx.Err() != nil || videoFailed {
				break
			}
			if subsFailed && att.withSubs {
				fmt.Println(pterm.Gray("  · " + labelPrefix + att.label + " skipped (subtitle error)"))
				continue
			}
			if !att.noAudio && audioModeFailed[att.audioCopy] {
				fmt.Println(pterm.Gray("  · " + labelPrefix + att.label + " skipped (audio error)"))
				continue
			}
			if !firstRun {
				_ = os.Remove(outputFile)
			}
			firstRun = false
			pterm.NewStyle(pterm.FgLightMagenta, pterm.Bold).Printf("  >> %s%s\n", labelPrefix, att.label)
			job := baseJob
			job.outputPath = outputFile
			job.audioCopy = att.audioCopy
			job.withSubs = att.withSubs
			job.noAudio = att.noAudio
			job.pureAudioCopy = cfg.copyAudio
			err := runFFmpeg(ctx, buildArgs(job), stats.DurationSec, idx, total, stats.FileSizeMB)
			if errors.Is(err, context.Canceled) {
				encodingOK = true
				noAudioUsed = att.noAudio
				break
			}
			if err == nil {
				vErr := validateOutput(ctx, outputFile, stats, doConvert, baseJob.isTS, att.noAudio, targetCodec)
				if vErr == nil || ctx.Err() != nil {
					encodingOK = true
					noAudioUsed = att.noAudio
					break
				}
				pWarn.Printf("Attempt invalid (%v) — trying next stage.\n", vErr)
				err = vErr
			}
			if firstConvertErr == nil {
				firstConvertErr = err
			}
			lastConvertErr = err
			switch classifyFFmpegError(err.Error()) {
			case failSubtitle:
				if att.withSubs {
					subsFailed = true
				}
			case failAudio:
				if !att.noAudio {
					audioModeFailed[att.audioCopy] = true
				}
			case failVideo:
				videoFailed = true
				pWarn.Println("Video encoder error — remaining attempts skipped.")
			}
		}
	}

	if doConvert {
		outputFile = filepath.Join(outputDir, basenameFull+outSuffix+".part.mkv")
		runCascade(func(j convJob) []string { return j.buildConvertArgs() }, "")
	} else if doRemux {
		outputFile = filepath.Join(outputDir, basenameFull+".remux.part.mkv")
		runCascade(func(j convJob) []string { return j.buildRemuxArgs() }, "REMUX ")
	}

	if ctx.Err() != nil {
		previewFile := filepath.Join(outputDir, basenameFull+".preview.mkv")
		if outputFile != "" {
			if _, err := os.Stat(outputFile); err == nil {
				if err := os.Rename(outputFile, previewFile); err != nil {
					removeOrRename(outputFile)
					result.Skipped = true
					result.ErrMsg = fmt.Sprintf("Converter.go: processFile: preview rename failed: %v", err)
					pWarn.Printf("Skipped: aborted (preview rename failed: %v).\n\n", err)
					return result
				}
				result.OutputFile = previewFile
				result.IsPreview = true
				pOK.Printf("Preview saved: %s\n\n", filepath.Base(previewFile))
				return result
			}
		}
		result.Skipped = true
		fmt.Println(pterm.Gray("  Skipped: aborted."))
		fmt.Println()
		return result
	}

	if !encodingOK || outputFile == "" {
		if outputFile != "" {
			removeOrRename(outputFile)
		}
		switch {
		case lastConvertErr != nil:
			msg := fmt.Sprintf("Converter.go: processFile: all FFmpeg attempts failed: %v", lastConvertErr)
			if firstConvertErr != nil && firstConvertErr.Error() != lastConvertErr.Error() {
				msg += fmt.Sprintf(" | first error: %v", firstConvertErr)
			}
			result.ErrMsg = msg
		default:
			result.ErrMsg = "Converter.go: processFile: all FFmpeg attempts (encoding/remux) failed"
		}
		fmt.Println()
		pErr.Println("Conversion failed.")
		result.FailedAt = time.Now()
		return result
	}

	if err := copyTimestamps(filePath, outputFile); err != nil {
		pWarn.Printf("Could not transfer file timestamps: %v\n", err)
	}

	outSizeMB := getFileSizeMB(outputFile)
	savedMB := stats.FileSizeMB - outSizeMB

	if savedMB <= 0 && doConvert {
		pOK.Printf("%.0f MB  →  %.0f MB   %s — %s discarded\n",
			stats.FileSizeMB, outSizeMB,
			pterm.LightRed(fmt.Sprintf("(+%.0f MB larger)", -savedMB)),
			codecLabel)
		_ = os.Remove(outputFile)

		if ext == ".mkv" {
			markPath := filepath.Join(dir, base+".remux.mkv")
			if _, statErr := os.Stat(markPath); statErr == nil {
				fmt.Println(pterm.Gray(fmt.Sprintf(
					"  >> Keeping original as %s (target name %s already exists)",
					filepath.Base(filePath), filepath.Base(markPath))))
				result.OutputFile = filePath
			} else if renameErr := os.Rename(filePath, markPath); renameErr == nil {
				fmt.Println(pterm.Gray(fmt.Sprintf(
					"  >> Original renamed to %s (protection against re-conversion)",
					filepath.Base(markPath))))
				result.OutputFile = markPath
			} else {
				pWarn.Printf("Keeping original as %s (rename: %v)\n",
					filepath.Base(filePath), renameErr)
				result.OutputFile = filePath
			}
			result.SavedMB = 0
			result.Success = true
			fmt.Println()
			return result
		}

		mkvFile := filepath.Join(outputDir, basenameFull+".remux.mkv")
		pterm.NewStyle(pterm.FgLightMagenta, pterm.Bold).
			Println("  >> REMUX to MKV (stream copy, lossless)")

		audioArgs := buildPerStreamAudioArgs(stats.AudioStreams, false, ext == ".ts")
		if cfg.copyAudio {
			audioArgs = []string{"-c:a", "copy"}
		}
		subArgs := subtitleCodecArgs(stats.SubCodecs)

		remuxArgs := []string{"-y", "-err_detect", "ignore_err"}
		if ext == ".ts" {
			remuxArgs = append(remuxArgs, "-fflags", "+genpts+discardcorrupt")
		}
		remuxArgs = append(remuxArgs,
			"-i", filePath,
			"-map", "0:V:0",
			"-map", "0:a?",
			"-map", "0:s?",
		)
		if ext == ".ts" {
			remuxArgs = append(remuxArgs, "-map", "-0:d?", "-avoid_negative_ts", "make_zero")
		}
		remuxArgs = append(remuxArgs, "-c:v", "copy")
		remuxArgs = append(remuxArgs, audioArgs...)
		remuxArgs = append(remuxArgs, subArgs...)
		remuxArgs = append(remuxArgs, staleStatsTagArgs()...)
		remuxArgs = append(remuxArgs, mkvFile)

		if err := runFFmpeg(ctx, remuxArgs, stats.DurationSec, idx, total, stats.FileSizeMB); err != nil {
			_ = os.Remove(mkvFile)
			if errors.Is(err, context.Canceled) {
				result.Skipped = true
				return result
			}
			pWarn.Println("MKV remux with subtitles failed, trying without...")

			remuxNoSubs := []string{"-y", "-err_detect", "ignore_err"}
			if ext == ".ts" {
				remuxNoSubs = append(remuxNoSubs, "-fflags", "+genpts+discardcorrupt")
			}
			remuxNoSubs = append(remuxNoSubs,
				"-i", filePath,
				"-map", "0:V:0",
				"-map", "0:a?",
			)
			if ext == ".ts" {
				remuxNoSubs = append(remuxNoSubs, "-map", "-0:d?", "-avoid_negative_ts", "make_zero")
			}
			remuxNoSubs = append(remuxNoSubs, "-c:v", "copy")
			remuxNoSubs = append(remuxNoSubs, audioArgs...)
			remuxNoSubs = append(remuxNoSubs, staleStatsTagArgs()...)
			remuxNoSubs = append(remuxNoSubs, "-sn", mkvFile)

			if err2 := runFFmpeg(ctx, remuxNoSubs, stats.DurationSec, idx, total, stats.FileSizeMB); err2 != nil {
				_ = os.Remove(mkvFile)
				if errors.Is(err2, context.Canceled) {
					result.Skipped = true
					return result
				}
				pWarn.Printf("Final MKV remux failed, original is kept: %v\n", err2)
				result.ErrMsg = fmt.Sprintf("Converter.go: processFile: MKV remux after H.265 discard failed: %v", err2)
				result.FailedAt = time.Now()
				fmt.Println()
				return result
			}
		}

		if valErr := validateOutput(ctx, mkvFile, stats, false, ext == ".ts", false, targetCodec); valErr != nil {
			removeOrRename(mkvFile)
			result.ErrMsg = fmt.Sprintf("Converter.go: processFile: final remux invalid: %v", valErr)
			result.FailedAt = time.Now()
			return result
		}
		if err := copyTimestamps(filePath, mkvFile); err != nil {
			pWarn.Printf("Could not transfer file timestamps: %v\n", err)
		}
		if err := sendToRecycleBin(filePath); err != nil {
			pWarn.Printf("Original is kept (recycle bin): %s → %v\n", filePath, err)
		}
		result.OutputFile = mkvFile
		result.SavedMB = 0
		result.Success = true
		fmt.Println()
		return result
	}

	// Rename .part.mkv → final name only after successful validation.
	if strings.Contains(outputFile, ".part.mkv") {
		finalOutput := strings.Replace(outputFile, ".part.mkv", ".mkv", 1)
		_ = os.Remove(finalOutput)
		if err := os.Rename(outputFile, finalOutput); err != nil {
			removeOrRename(outputFile)
			result.ErrMsg = fmt.Sprintf("Converter.go: processFile: final rename failed: %v", err)
			result.FailedAt = time.Now()
			fmt.Println()
			return result
		}
		outputFile = finalOutput
	}

	sizeNote := pterm.LightGreen(fmt.Sprintf("(–%.0f MB)", savedMB))
	if savedMB < 0 {
		// Lossless remux can come out marginally larger (container overhead).
		sizeNote = pterm.LightYellow(fmt.Sprintf("(+%.0f MB larger, remux kept)", -savedMB))
	}
	pOK.Printf("%.0f MB  →  %.0f MB   %s\n",
		stats.FileSizeMB, outSizeMB, sizeNote)

	if noAudioUsed && len(stats.AudioStreams) > 0 {
		// Video-only fallback: the output has no sound, so the original (and
		// with it the only copy of the audio) must never go to the recycle bin.
		result.NoAudio = true
		pWarn.Printf("Converted WITHOUT audio (fallback) — original kept: %s\n",
			filepath.Base(filePath))
	} else if err := sendToRecycleBin(filePath); err != nil {
		pWarn.Printf("Original is kept (recycle bin): %s → %v\n", filePath, err)
	}
	result.OutputFile = outputFile
	result.SavedMB = savedMB
	result.Success = true
	fmt.Println()
	return result
}

// ----------------------------------------------------------------------------
// getVideoStats: FFprobe wrapper
// ----------------------------------------------------------------------------

func getVideoStats(ctx context.Context, filePath string) (*VideoStats, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", "-show_streams", "-show_format", "-of", "json", filePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("Converter.go: getVideoStats: FFprobe timeout (file may be corrupted): %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("Converter.go: getVideoStats: FFprobe: %w | %s",
				err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("Converter.go: getVideoStats: %w", err)
	}

	var p ffprobeOutput
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("Converter.go: getVideoStats: JSON parse error: %w", err)
	}

	s := &VideoStats{
		VideoCodec: "unknown", AudioCodec: "unknown",
		Channels: 0, FPSNum: 30, FPSDen: 1,
	}
	for _, st := range p.Streams {
		switch strings.ToLower(st.CodecType) {
		case "video":
			if s.VideoCodec != "unknown" || st.Disposition.AttachedPic == 1 {
				continue
			}
			s.VideoCodec = st.CodecName
			s.Width = st.Width
			s.Height = st.Height
			s.FieldOrder = st.FieldOrder
			s.ColorSpace = st.ColorSpace
			s.ColorTransfer = st.ColorTransfer
			s.ColorPrimaries = st.ColorPrimaries
			s.ColorRange = st.ColorRange
			if st.RFrameRate != "" {
				parts := strings.SplitN(st.RFrameRate, "/", 2)
				if len(parts) == 2 {
					s.FPSNum, _ = strconv.Atoi(parts[0])
					s.FPSDen, _ = strconv.Atoi(parts[1])
				}
			}
			if bps, e := strconv.ParseInt(st.BitRate, 10, 64); e == nil {
				s.BitrateBps = bps
			}
		case "audio":
			if st.Channels > s.Channels {
				s.AudioCodec = st.CodecName
				s.Channels = st.Channels
			} else if s.AudioCodec == "unknown" {
				s.AudioCodec = st.CodecName
			}
			lang, title := "", ""
			if st.Tags != nil {
				lang = st.Tags["language"]
				title = st.Tags["title"]
			}
			sr, _ := strconv.Atoi(st.SampleRate)
			s.AudioStreams = append(s.AudioStreams, AudioStreamInfo{
				Codec:      st.CodecName,
				Channels:   st.Channels,
				Layout:     st.ChannelLayout,
				Language:   lang,
				Title:      title,
				SampleRate: sr,
			})
		case "subtitle":
			s.SubCodecs = append(s.SubCodecs, st.CodecName)
		}
	}
	if s.BitrateBps == 0 {
		s.BitrateBps, _ = strconv.ParseInt(p.Format.BitRate, 10, 64)
	}
	s.DurationSec, _ = strconv.ParseFloat(p.Format.Duration, 64)
	return s, nil
}

// ----------------------------------------------------------------------------
// validateOutput: audio stream count check
// ----------------------------------------------------------------------------

func validateOutput(ctx context.Context, outputFile string, src *VideoStats, isConversion bool, isTS bool, noAudioOK bool, targetCodec string) error {
	out, err := getVideoStats(ctx, outputFile)
	if err != nil {
		return fmt.Errorf("Converter.go: validateOutput: probe failed: %w", err)
	}
	if out.VideoCodec == "" || out.VideoCodec == "unknown" {
		return errors.New("Converter.go: validateOutput: no video codec")
	}
	if isConversion && !strings.EqualFold(out.VideoCodec, targetCodec) {
		return fmt.Errorf("Converter.go: validateOutput: not %s", strings.ToUpper(targetCodec))
	}
	if !isConversion && !strings.EqualFold(out.VideoCodec, src.VideoCodec) {
		return errors.New("Converter.go: validateOutput: codec changed")
	}
	if !noAudioOK && !strings.EqualFold(src.AudioCodec, "unknown") &&
		strings.EqualFold(out.AudioCodec, "unknown") {
		return errors.New("Converter.go: validateOutput: audio missing")
	}
	if !noAudioOK && len(src.AudioStreams) > 0 &&
		len(out.AudioStreams) < len(src.AudioStreams) {
		return fmt.Errorf("Converter.go: validateOutput: audio stream loss (%d source → %d output)",
			len(src.AudioStreams), len(out.AudioStreams))
	}
	durationTolerance := 0.98
	if isTS {
		durationTolerance = 0.95
	}
	if src.DurationSec > 0 &&
		(out.DurationSec == 0 || out.DurationSec < src.DurationSec*durationTolerance) {
		return errors.New("Converter.go: validateOutput: video too short")
	}
	if src.FileSizeMB > 0 {
		outInfo, statErr := os.Stat(outputFile)
		if statErr != nil {
			return fmt.Errorf("Converter.go: validateOutput: output file not readable: %w", statErr)
		}
		minBytes := int64(src.FileSizeMB * 1024 * 1024 * 0.01)
		if minBytes < 1024*1024 {
			minBytes = 1024 * 1024
		}
		if outInfo.Size() < minBytes {
			return fmt.Errorf("Converter.go: validateOutput: output file too small (%.1f KB)",
				float64(outInfo.Size())/1024)
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// runFFmpeg: progress bar with ETA
// ----------------------------------------------------------------------------

func extractTimeSec(timeStr string) float64 {
	isNegative := strings.HasPrefix(timeStr, "-")
	cleanStr := strings.TrimPrefix(timeStr, "-")
	parts := strings.Split(cleanStr, ":")
	if len(parts) == 3 {
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		s, _ := strconv.ParseFloat(parts[2], 64)
		total := h*3600 + m*60 + s
		if isNegative {
			return -total
		}
		return total
	}
	return -1
}

func runFFmpeg(ctx context.Context, args []string, durationSec float64, fileIdx, fileTotal int, inputSizeMB float64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	outputPath := ""
	if len(args) > 0 {
		if last := args[len(args)-1]; !strings.HasPrefix(last, "-") {
			outputPath = last
		}
	}

	args = append([]string{"-v", "warning", "-nostats", "-progress", "pipe:1"}, args...)

	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	cmd := exec.CommandContext(runCtx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP |
			winCREATE_NO_WINDOW | winIDLE_PRIORITY_CLASS,
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("Converter.go: runFFmpeg (StdinPipe): %w", err)
	}
	defer stdin.Close()

	cmd.Cancel = func() error {
		_, werr := io.WriteString(stdin, "q\n")
		return werr
	}
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("Converter.go: runFFmpeg (StdoutPipe): %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("Converter.go: runFFmpeg (StderrPipe): %w", err)
	}

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Converter.go: runFFmpeg (Start): %w", err)
	}

	const stallLimit = 5 * time.Minute
	watchdog := time.AfterFunc(stallLimit, func() {
		cancelRun(errFFmpegStall)
	})
	defer watchdog.Stop()

	var wg sync.WaitGroup
	var lastErrLine string
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if t := strings.TrimSpace(sc.Text()); t != "" {
				lastErrLine = t
			}
		}
		if err := sc.Err(); err != nil && lastErrLine == "" {
			lastErrLine = err.Error()
		}
	}()

	const (
		barLen  = 48
		oBarLen = 30
	)

	startTime := time.Now()
	progressStarted := false
	var lastStatTime time.Time
	var smoothedEstMB float64
	var lastRender time.Time
	const renderInterval = 100 * time.Millisecond
	var lastL2, lastL3, lastL4 string

	fmt.Print("\033[?25l\033[?7l")
	defer fmt.Print("\033[?25h\033[?7h")

	progressArea, _ := pterm.DefaultArea.WithRemoveWhenDone(false).Start()

	cyanLabel := func(s string, width int) string {
		return pterm.Cyan(fmt.Sprintf("%-*s", width, s))
	}
	magentaLbl := func(s string, width int) string {
		return pterm.NewStyle(pterm.FgLightMagenta, pterm.Bold).
			Sprint(fmt.Sprintf("%-*s", width, s))
	}

	fields := make(map[string]string, 16)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		watchdog.Reset(stallLimit)

		key, val, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok {
			continue
		}
		fields[key] = val
		if key != "progress" {
			continue
		}

		sec := -1.0
		if v := fields["out_time_us"]; v != "" && v != "N/A" {
			if us, e := strconv.ParseFloat(v, 64); e == nil {
				sec = us / 1_000_000
			}
		}
		if sec < 0 {
			if v := fields["out_time"]; v != "" && v != "N/A" {
				sec = extractTimeSec(v)
			}
		}
		if sec < 0 || durationSec <= 0 {
			continue
		}

		pct := sec / durationSec * 100
		if pct > 100 {
			pct = 100
		}
		if pct < 0.1 {
			pct = 0.1
		}
		filled := int(pct / 100 * float64(barLen))
		if filled > barLen {
			filled = barLen
		}
		barFilled := strings.Repeat("█", filled)
		barEmpty := strings.Repeat("░", barLen-filled)

		elapsed := time.Since(startTime).Seconds()
		etaStr := "-:--"
		if pct > 0 {
			etaStr = formatDuration(elapsed/(pct/100) - elapsed)
		}
		laufzeit := formatDuration(elapsed)

		fps := "-"
		if fv, e := strconv.ParseFloat(fields["fps"], 64); e == nil && fv > 0 {
			fps = fmt.Sprintf("%d", int(fv))
		}
		bitrate := "-"
		if b := strings.TrimSpace(strings.TrimSuffix(fields["bitrate"], "kbits/s")); b != "" {
			if bv, e := strconv.ParseFloat(b, 64); e == nil && bv > 0 {
				bitrate = fmt.Sprintf("%dk", int(bv))
			}
		}
		speed := "-"
		if s := strings.TrimSpace(strings.TrimSuffix(fields["speed"], "x")); s != "" {
			if sv, e := strconv.ParseFloat(s, 64); e == nil && sv > 0 {
				speed = fmt.Sprintf("%.1fx", sv)
			}
		}
		frame := "0"
		if f := fields["frame"]; f != "" && f != "N/A" {
			frame = f
		}

		var estMBVal float64
		if v := fields["total_size"]; v != "" && v != "N/A" {
			if bytesVal, e := strconv.ParseFloat(v, 64); e == nil && bytesVal > 0 && pct > 5 {
				estMBVal = (bytesVal / 1024 / 1024) / (pct / 100)
			}
		}
		if estMBVal == 0 && pct > 5 && outputPath != "" &&
			time.Since(lastStatTime) >= time.Second {
			lastStatTime = time.Now()
			if info, statErr := os.Stat(outputPath); statErr == nil {
				if curMB := float64(info.Size()) / 1024 / 1024; curMB > 0 {
					estMBVal = curMB / (pct / 100)
				}
			}
		}
		if estMBVal > 0 {
			if smoothedEstMB == 0 {
				smoothedEstMB = estMBVal
			} else {
				smoothedEstMB = smoothedEstMB*0.85 + estMBVal*0.15
			}
		}

		if progressStarted && time.Since(lastRender) < renderInterval {
			continue
		}
		lastRender = time.Now()

		overallPct := (float64(fileIdx-1) + pct/100) /
			float64(max(fileTotal, 1)) * 100
		oFilled := int(overallPct / 100 * float64(oBarLen))
		if oFilled > oBarLen {
			oFilled = oBarLen
		}
		oBarFilled := strings.Repeat("█", oFilled)
		oBarEmpty := strings.Repeat("░", oBarLen-oFilled)

		l1 := fmt.Sprintf("  [%s%s]  %s",
			pterm.LightGreen(barFilled), pterm.Gray(barEmpty),
			pterm.NewStyle(pterm.FgLightWhite, pterm.Bold).
				Sprint(fmt.Sprintf("%5.1f%%", pct)))

		l2 := fmt.Sprintf("  %s %-8s   %s %-8s   %s %s",
			cyanLabel("Position", 10), formatDuration(sec),
			cyanLabel("Elapsed", 10), laufzeit,
			cyanLabel("ETA", 8), pterm.LightYellow(etaStr))

		l3 := fmt.Sprintf("  %s %-8s   %s %-8s   %s %s",
			cyanLabel("Frames/s", 10), fps,
			cyanLabel("Bitrate", 10), bitrate,
			cyanLabel("Speed", 8), pterm.LightGreen(speed))

		var l4 string
		switch {
		case smoothedEstMB > 0 && inputSizeMB > 0:
			savingsMB := inputSizeMB - smoothedEstMB
			savingsPct := savingsMB / inputSizeMB * 100
			if savingsMB < 0 {
				l4 = fmt.Sprintf("  %s %-8s   %.0f MB  →  ~%.0f MB   %s",
					cyanLabel("Frame", 10), frame,
					inputSizeMB, smoothedEstMB,
					pterm.LightRed(fmt.Sprintf("(+%.0f MB larger)", -savingsMB)))
			} else {
				l4 = fmt.Sprintf("  %s %-8s   %.0f MB  →  ~%.0f MB   %s",
					cyanLabel("Frame", 10), frame,
					inputSizeMB, smoothedEstMB,
					pterm.LightGreen(fmt.Sprintf("(–%.0f MB / %.0f%% smaller)",
						savingsMB, savingsPct)))
			}
		case smoothedEstMB > 0:
			// No meaningful input size (e.g. merge, where growing is expected):
			// show the estimated output size without any smaller/larger verdict.
			l4 = fmt.Sprintf("  %s %-8s   %s  ~%.0f MB",
				cyanLabel("Frame", 10), frame, cyanLabel("Output", 8), smoothedEstMB)
		case inputSizeMB > 0:
			l4 = fmt.Sprintf("  %s %-8s   %.0f MB  →  %s",
				cyanLabel("Frame", 10), frame,
				inputSizeMB, pterm.Gray("..."))
		default:
			l4 = fmt.Sprintf("  %s %-8s   %s  %s",
				cyanLabel("Frame", 10), frame, cyanLabel("Output", 8), pterm.Gray("..."))
		}

		var l5 string
		if fileTotal > 1 {
			l5 = fmt.Sprintf("  %s [%s%s]  %s  %s",
				magentaLbl("Overall", 10),
				pterm.LightMagenta(oBarFilled), pterm.Gray(oBarEmpty),
				pterm.NewStyle(pterm.FgLightWhite, pterm.Bold).
					Sprint(fmt.Sprintf("%5.1f%%", overallPct)),
				pterm.Gray(fmt.Sprintf("(%d/%d)", fileIdx, fileTotal)))
		}

		lastL2, lastL3, lastL4 = l2, l3, l4

		var content string
		if l5 != "" {
			content = strings.Join([]string{l1, l2, l3, l4, l5}, "\n")
		} else {
			content = strings.Join([]string{l1, l2, l3, l4}, "\n")
		}
		progressArea.Update(content)
		progressStarted = true
	}
	scanErr := scanner.Err()
	wg.Wait() // stderr goroutine done — lastErrLine is safe to touch now
	if scanErr != nil && lastErrLine == "" {
		lastErrLine = scanErr.Error()
	}
	err = cmd.Wait()

	if ctx.Err() != nil {
		if progressStarted {
			progressArea.Update("")
		}
		_ = progressArea.Stop()
		return ctx.Err()
	}
	if errors.Is(context.Cause(runCtx), errFFmpegStall) {
		if progressStarted {
			progressArea.Update("")
		}
		_ = progressArea.Stop()
		return fmt.Errorf("Converter.go: runFFmpeg: FFmpeg stopped responding for %s (stall timeout) | Last output: %s",
			stallLimit, lastErrLine)
	}
	if err != nil {
		if progressStarted {
			progressArea.Update("")
		}
		_ = progressArea.Stop()
		return fmt.Errorf("Converter.go: runFFmpeg: %w | Last output: %s", err, lastErrLine)
	}

	if !progressStarted {
		fmt.Println()
	} else {
		finalBar := pterm.LightGreen(strings.Repeat("█", barLen))
		finalL1 := fmt.Sprintf("  [%s]  %s",
			finalBar,
			pterm.NewStyle(pterm.FgLightGreen, pterm.Bold).Sprint("100.0%"))

		parts := []string{finalL1, lastL2, lastL3, lastL4}
		if fileTotal > 1 {
			overallPct := float64(fileIdx) / float64(fileTotal) * 100
			oFilled := int(overallPct / 100 * float64(oBarLen))
			if oFilled > oBarLen {
				oFilled = oBarLen
			}
			oBarFilled := strings.Repeat("█", oFilled)
			oBarEmpty := strings.Repeat("░", oBarLen-oFilled)
			finalL5 := fmt.Sprintf("  %s [%s%s]  %s  %s",
				magentaLbl("Overall", 10),
				pterm.LightMagenta(oBarFilled), pterm.Gray(oBarEmpty),
				pterm.NewStyle(pterm.FgLightWhite, pterm.Bold).
					Sprint(fmt.Sprintf("%5.1f%%", overallPct)),
				pterm.Gray(fmt.Sprintf("(%d/%d)", fileIdx, fileTotal)))
			parts = append(parts, finalL5)
		}
		progressArea.Update(strings.Join(parts, "\n"))
	}
	_ = progressArea.Stop()
	if progressStarted {
		fmt.Println()
	}
	return nil
}

// ----------------------------------------------------------------------------
// Bitrate / scaling / filter / GOP helpers
// ----------------------------------------------------------------------------

func determineBitrateKbps(s *VideoStats) int64 {
	if s.DurationSec < 1 || s.FileSizeMB <= 0 {
		if s.Height >= 2160 {
			return 12000
		}
		if s.Height >= 1080 {
			return 6000
		}
		return 2000
	}

	totalKbps := int64(s.FileSizeMB * 8000 / s.DurationSec)

	var audioKbps int64
	for _, audio := range s.AudioStreams {
		ch := audio.Channels
		if ch <= 0 {
			ch = 2
		}
		switch strings.ToLower(audio.Codec) {
		case "truehd":
			audioKbps += 3500
		case "dca", "dts":
			audioKbps += 1536
		case "flac":
			audioKbps += int64(ch * 400)
		case "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_f32le", "pcm_f64le", "pcm_u8":
			audioKbps += int64(ch * 768)
		case "ac3":
			audioKbps += 384
		case "eac3":
			audioKbps += 640
		default:
			audioKbps += int64(ch * 96)
		}
	}

	estVideoKbps := totalKbps - audioKbps

	if s.BitrateBps > 0 {
		probeVideoKbps := s.BitrateBps / 1000
		if probeVideoKbps < totalKbps-150 {
			return probeVideoKbps
		}
	}

	if estVideoKbps < 500 {
		estVideoKbps = 500
	}
	return estVideoKbps
}

func needsScaling(cfg *AppConfig, w, h int) bool {
	if cfg.keepOriginal {
		return false
	}
	if w <= 0 || h <= 0 {
		return false
	}
	short := appSettings.maxResolution
	long := short * 16 / 9
	return max(w, h) > long || min(w, h) > short
}

func buildVideoFilter(doScale, deinterlace bool) string {
	// bwdif before any scaling: deinterlacing needs the original field
	// structure. send_frame keeps the source frame rate (25i → 25p).
	pre := ""
	if deinterlace {
		pre = "bwdif=mode=send_frame,"
	}
	if doScale {
		short := appSettings.maxResolution
		long := short * 16 / 9
		cas := strconv.FormatFloat(appSettings.casStrength, 'f', -1, 64)
		return pre + fmt.Sprintf(
			"scale='if(gte(iw,ih),%d,%d)':'if(gte(iw,ih),%d,%d)'"+
				":force_original_aspect_ratio=decrease:force_divisible_by=2,"+
				"cas=strength=%s,format=p010le",
			long, short, short, long, cas)
	}
	return pre + "crop=trunc(iw/2)*2:trunc(ih/2)*2,format=p010le"
}

// videoIsInterlaced reports whether the probed field order marks real
// interlaced material (TV recordings etc.). "progressive"/"unknown" → false.
func videoIsInterlaced(s *VideoStats) bool {
	switch strings.ToLower(strings.TrimSpace(s.FieldOrder)) {
	case "tt", "bb", "tb", "bt":
		return true
	}
	return false
}

func buildNVENCOpts(maxBitrate, bufsize string, gop int) []string {
	opts := []string{
		"-c:v", "hevc_nvenc", "-rc", "vbr", "-cq", strconv.Itoa(appSettings.targetCQ),
		"-b:v", "0", "-maxrate", maxBitrate, "-bufsize", bufsize,
		"-profile:v", "main10", "-pix_fmt", "p010le",
		"-preset", appSettings.nvencPreset, "-tune", "hq",
		"-multipass", "qres", "-rc-lookahead", strconv.Itoa(appSettings.nvencLookahead), "-fps_mode", "cfr",
		"-g", strconv.Itoa(gop), "-spatial_aq", "1", "-temporal_aq", "1",
		"-aq-strength", "8", "-bf", strconv.Itoa(appSettings.bFrames),
	}
	// b_ref_mode needs B-frames; older GPUs (no B-frame support) reject it.
	if appSettings.bFrames > 0 {
		opts = append(opts, "-b_ref_mode", "2")
	}
	return opts
}

// buildAV1Opts mirrors buildNVENCOpts for av1_nvenc. Differences: own CQ
// scale (0-63, av1TargetCQ), no -profile (Main covers 8/10-bit), no
// B-frame options (not exposed by av1_nvenc), AQ flags use hyphens.
func buildAV1Opts(maxBitrate, bufsize string, gop int) []string {
	return []string{
		"-c:v", "av1_nvenc", "-rc", "vbr", "-cq", strconv.Itoa(appSettings.av1TargetCQ),
		"-b:v", "0", "-maxrate", maxBitrate, "-bufsize", bufsize,
		"-pix_fmt", "p010le",
		"-preset", appSettings.nvencPreset, "-tune", "hq",
		"-multipass", "qres", "-rc-lookahead", strconv.Itoa(appSettings.nvencLookahead), "-fps_mode", "cfr",
		"-g", strconv.Itoa(gop), "-spatial-aq", "1", "-temporal-aq", "1",
		"-aq-strength", "8",
	}
}

func buildColorOpts(s *VideoStats) []string {
	var a []string
	isUsable := func(v string) bool {
		v = strings.ToLower(strings.TrimSpace(v))
		return v != "" && v != "unknown" && v != "reserved"
	}
	isSane := func(v string) bool {
		v = strings.ToLower(strings.TrimSpace(v))
		return v != "bt470m" && v != "bt470bg"
	}
	if isUsable(s.ColorPrimaries) {
		a = append(a, "-color_primaries", s.ColorPrimaries)
	}
	if isUsable(s.ColorTransfer) && isSane(s.ColorTransfer) {
		a = append(a, "-color_trc", s.ColorTransfer)
	}
	if isUsable(s.ColorSpace) {
		a = append(a, "-colorspace", s.ColorSpace)
	}
	if isUsable(s.ColorRange) {
		a = append(a, "-color_range", s.ColorRange)
	}
	return a
}

// ----------------------------------------------------------------------------
// HDR detection
// ----------------------------------------------------------------------------

// videoHDRKind classifies the primary video stream by its transfer function:
// "pq" (HDR10, SMPTE ST 2084), "hlg" (Hybrid Log-Gamma) or "" (SDR). Real HDR
// streams always carry the transfer tag, so keying on it avoids false positives
// on plain BT.2020-primaries SDR material. Used only to raise the bitrate cap;
// the HDR tags themselves are copied from the source by buildColorOpts.
func videoHDRKind(s *VideoStats) string {
	switch strings.ToLower(strings.TrimSpace(s.ColorTransfer)) {
	case "smpte2084", "smptest2084":
		return "pq"
	case "arib-std-b67":
		return "hlg"
	}
	return ""
}

func calcGOP(n, d int) int {
	if n <= 0 {
		n = 30
	}
	if d <= 0 {
		d = 1
	}
	g := n * 4 / d
	if g < 48 {
		return 48
	}
	if g > 600 {
		return 600
	}
	return g
}

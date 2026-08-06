//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

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
	vfOptsCPU     []string // Filterkette ohne Grafikkarte; nur gesetzt, wenn vfOpts eine ist
	encodeOnCard  bool     // Bilder bleiben im Grafikspeicher → Encoder ohne -pix_fmt
	hwaccelOpts   []string // Entpacken auf der Grafikkarte; leer = Prozessor
	withSubs      bool
	audioCopy     bool
	isTS          bool
	isMKV         bool
	noAudio       bool
	pureAudioCopy bool
	audioStreams  []AudioStreamInfo
	subCodecs     []string
	sourceTag     string // value for the sourceTagKey metadata (origin file name)
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

// sourceTagArgs stamps the origin file name into the output as a global
// container tag (sourceTagKey). The "already converted" check reads it back to
// tell a genuine resume apart from a name collision: two different sources whose
// cleaned names AND durations happen to match no longer overwrite or shadow each
// other. Empty tag → no args (older outputs simply have none; handled there).
func sourceTagArgs(sourceTag string) []string {
	if sourceTag == "" {
		return nil
	}
	return []string{"-metadata", sourceTagKey + "=" + sourceTag}
}

// gpuDecodeDisabled schaltet NVDEC für den REST des Laufs ab, sobald ein
// Entpack-Versuch auf der Grafikkarte schiefgegangen ist. Ohne dieses Merken
// würde jede weitere Datei denselben Fehlversuch samt Wiederholung bezahlen.
// Der Ablauf ist sequenziell (eine Datei nach der anderen), daher genügt eine
// einfache Variable ohne Sperre.
var gpuDecodeDisabled bool

// gpuFramesStayOnCard gilt für die gerade laufende Datei: true, wenn die
// Filterkette die Bilder im Grafikspeicher lässt (verkleinern auf der Karte,
// kein Nachschärfen). Dann darf der Encoder KEIN -pix_fmt bekommen, sonst
// bricht FFmpeg mit "Impossible to convert between the formats" ab — das
// Bildformat bestimmt in diesem Fall scale_cuda selbst.
// Gesetzt wird die Variable einmal je Datei in processFile, gelesen von den
// Options-Bauern; wie bei cpuModeActive spart das eine Fallunterscheidung an
// jeder Aufrufstelle. Der Ablauf ist sequenziell, daher genügt eine einfache
// Variable ohne Sperre.
var gpuFramesStayOnCard bool

// gpuDecodeSafeCodecs listet die Codecs, die NVDEC auf allen unterstützten
// Karten sicher beherrscht. Alles andere (exotische oder sehr alte Formate)
// läuft weiter über den Prozessor — dort kostet der Entpack-Vorgang ohnehin
// wenig, weil solche Dateien klein sind.
var gpuDecodeSafeCodecs = map[string]bool{
	"h264": true, "hevc": true, "av1": true, "vp9": true,
}

// gpuDecodeArgs liefert die FFmpeg-Argumente zum Entpacken auf der Grafikkarte
// — oder nil, wenn dafür auch nur ein Zweifel besteht.
//
// Warum das Bild dabei gleich bleibt: H.264/HEVC/AV1 schreiben den
// Dekodier-Vorgang bitgenau vor. NVDEC MUSS also dieselben Pixel liefern wie
// der Software-Dekodierer; nachgewiesen am 2026-07-29 per framemd5 (identischer
// Hash über den ganzen Videostrom). Beschleunigt wird nur der Weg dorthin.
//
// Die Bitratengrenze ist KEIN Tempo-, sondern ein Sicherheitsventil: an einer
// HEVC-Datei mit ~400 Mbit/s riss NVDEC 2026-06 den Grafiktreiber mit (TDR).
// So etwas kann kein Rückfall auffangen — wenn der Treiber fällt, läuft kein
// Code mehr. Deshalb greift die Grenze VOR dem Versuch, und im Zweifel
// (unbekannte Bitrate, fremder Codec) gewinnt immer der Prozessor-Weg.
func gpuDecodeArgs(stats *VideoStats) []string {
	if !appSettings.gpuDecode || gpuDecodeDisabled || cpuModeActive || stats == nil {
		return nil
	}
	if !gpuDecodeSafeCodecs[strings.ToLower(strings.TrimSpace(stats.VideoCodec))] {
		return nil
	}
	// Unbekannte Bitrate heißt: die Schutzgrenze lässt sich nicht prüfen.
	if stats.BitrateBps <= 0 {
		return nil
	}
	limitBps := int64(appSettings.gpuDecodeMaxMbit) * 1_000_000
	if stats.BitrateBps > limitBps {
		return nil
	}
	return []string{"-hwaccel", "cuda"}
}

// fallBackToCPUDecode stellt einen Auftrag vollständig auf den Prozessor-Weg
// um. Beim reinen Entpacken auf der Karte genügte es, "-hwaccel" wegzulassen;
// wird auch VERKLEINERT, hängen zwei weitere Dinge daran: eine Kette mit
// scale_cuda kann ohne Bilder im Grafikspeicher gar nicht laufen, und der
// Encoder braucht sein Eingangsformat zurück, das bei der reinen
// Grafikkarten-Kette bewusst fehlt.
func (j convJob) fallBackToCPUDecode() convJob {
	j.hwaccelOpts = nil
	if len(j.vfOptsCPU) > 0 {
		j.vfOpts = j.vfOptsCPU
	}
	if j.encodeOnCard {
		j.nvencOpts = append(append([]string{}, j.nvencOpts...), "-pix_fmt", encodePixFmt())
		j.encodeOnCard = false
	}
	return j
}

func (j convJob) buildConvertArgs() []string {
	a := make([]string, 0, 32)
	a = append(a, "-y")
	// Entpacken auf der Grafikkarte (NVDEC), sofern gpuDecodeArgs es für diese
	// Datei freigegeben hat — bildgleich, aber schneller. Bleibt die Liste leer,
	// entpackt wie bisher der Prozessor. "-hwaccel" ist eine EINGABE-Option und
	// muss deshalb vor "-i" stehen. Das Kodieren läuft unverändert auf der GPU
	// (hevc_nvenc, steckt in nvencOpts).
	a = append(a, j.hwaccelOpts...)
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
	a = append(a, sourceTagArgs(j.sourceTag)...)
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
	a = append(a, sourceTagArgs(j.sourceTag)...)
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
	// Video-Encoder-Fehler: beide Backends. Keine Kaskaden-Sprosse kann einen
	// kaputten Video-Encode reparieren, deshalb wird sofort abgebrochen —
	// egal ob die GPU (nvenc/cuda) oder der CPU-Encoder (x265/svt) klemmt.
	case contains("nvenc", "cuda", "cuvid", "no capable devices",
		"x265", "libx265", "svt", "libsvtav1"):
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

// retireOriginal disposes of a successfully converted source file. The output
// already lives in its own folder, so keeping the source can never overwrite
// anything. Three ways, in order of precedence:
//
//	-keep                     leave the original exactly where it is
//	retireMode=folder         move it into the "originals" subfolder (default)
//	retireMode=recyclebin     hand it to the Windows recycle bin
//
// Whatever goes wrong, the original is never destroyed as a side effect: on
// any error it simply stays where it is and the reason is printed.
func retireOriginal(cfg *AppConfig, filePath string) {
	if cfg.keepSource {
		pInfo.Printf("Original kept (-keep): %s\n", filepath.Base(filePath))
		return
	}
	if appSettings.retireMode == retireModeFolder {
		moved, err := moveOriginalToFolder(filePath)
		if err != nil {
			pWarn.Printf("Original is kept: %s → %v\n", filePath, err)
			return
		}
		pInfo.Printf("Original moved to \"%s\": %s\n", originalsFolderName, filepath.Base(moved))
		return
	}

	err := sendToRecycleBin(filePath)
	switch {
	case err == nil:
		return
	case errors.Is(err, errRecycleNotVerified):
		// Windows reported success, but the file is not in the bin — it is
		// gone for good. Say so plainly instead of pretending it was kept.
		pWarn.Printf("Original was deleted PERMANENTLY — Windows did not put it into the recycle bin: %s\n",
			filePath)
	default:
		pWarn.Printf("Original is kept (recycle bin): %s → %v\n", filePath, err)
	}
}

// maxOriginalsNameTries begrenzt die Suche nach einem freien Namen im
// Originale-Ordner. Wer 99 gleichnamige Dateien dorthin schiebt, hat ein
// anderes Problem als eine fehlende Nummer 100.
const maxOriginalsNameTries = 99

// moveOriginalToFolder verschiebt die Quelldatei in den Unterordner
// "originals" NEBEN ihr — also auf dasselbe Laufwerk, wodurch das Verschieben
// ein reines Umbenennen bleibt und auch bei 60-GB-Dateien sofort fertig ist.
// Zurückgegeben wird der neue Pfad.
func moveOriginalToFolder(filePath string) (string, error) {
	targetDir := filepath.Join(filepath.Dir(filePath), originalsFolderName)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create \"%s\" folder: %w", originalsFolderName, err)
	}
	target, err := freeOriginalsPath(targetDir, filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	if err := os.Rename(filePath, target); err != nil {
		return "", fmt.Errorf("cannot move it into \"%s\": %w", originalsFolderName, err)
	}
	return target, nil
}

// freeOriginalsPath findet einen freien Namen im Zielordner. Gleichnamige
// Quellen aus verschiedenen Ordnern dürfen sich nicht gegenseitig
// überschreiben, deshalb wird bei Bedarf " (2)", " (3)" … angehängt.
func freeOriginalsPath(targetDir, name string) (string, error) {
	candidate := filepath.Join(targetDir, name)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate, nil
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; i <= maxOriginalsNameTries; i++ {
		candidate = filepath.Join(targetDir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name in \"%s\" for %s", originalsFolderName, name)
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
				// -mp4 repackages an already-converted file into a compatible
				// MP4 instead of skipping it (lossless for H.265/H.264; an AV1
				// file gets a hint inside convertedMKVToMP4).
				if cfg.mp4Mode {
					return convertedMKVToMP4(ctx, cfg, filePath)
				}
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

	// The source is probed once, up front: the skip/collision check, the
	// bitrate/scaling decision and the encode itself all need these stats. The
	// lazy probe of earlier versions saved nothing measurable (a skipped file
	// still probes its existing output), and the skip check now needs to know
	// the result codec, so a single probe keeps the logic correct and simpler.
	stats, statErr := getVideoStats(ctx, filePath)
	if statErr != nil {
		result.ErrMsg = fmt.Sprintf("Converter.go: processFile: FFprobe error: %v", statErr)
		result.FailedAt = time.Now()
		return result
	}
	stats.FileSizeMB = getFileSizeMB(filePath)

	// resultCodec is the codec the OUTPUT will actually carry: the target codec
	// for a real re-encode, but the unchanged source codec when the source is
	// lean enough to be only remuxed (mirrors the doConvert/doRemux switch
	// below). The skip check compares against it, so a finished .h265 never
	// blocks a -av1 run and vice versa — different codec mode, different output.
	targetCodec := "hevc"
	if cfg.av1 {
		targetCodec = "av1"
	}
	resultCodec := targetCodec
	{
		doScale := needsScaling(cfg, stats.Width, stats.Height)
		srcKbps := determineBitrateKbps(stats)
		calc := cappedTargetKbps(srcKbps, outputHeightFor(stats, doScale), cfg.maxBitrateKbps)
		if !(doScale || calc < srcKbps) {
			resultCodec = stats.VideoCodec
		}
	}

	// srcID identifies the exact source file (name incl. extension); it is
	// stamped into every output as the sourceTagKey metadata and read back here.
	// isOurFinishedOutput decides whether an existing output is THIS run's result
	// (→ skip) or only looks alike: a different codec mode, or a different source
	// whose cleaned name + duration collide with ours (→ write a numbered output).
	srcID := filepath.Base(filePath)
	isOurFinishedOutput := func(cs *VideoStats) bool {
		if !durationsClose(cs.DurationSec, stats.DurationSec) {
			return false // different length → different source
		}
		if !strings.EqualFold(cs.VideoCodec, resultCodec) {
			return false // other codec mode's output → not this run's result
		}
		// Same codec & length: ours only if the source tag matches (or is absent
		// on a legacy output we cannot disambiguate — keep the old skip behaviour).
		return cs.SourceTag == "" || strings.EqualFold(cs.SourceTag, srcID)
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
		if isOurFinishedOutput(candStats) {
			fmt.Println(pterm.Gray("  Skipped: output file already exists."))
			fmt.Println()
			result.Skipped = true
			return result
		}
		if !strings.EqualFold(candStats.VideoCodec, resultCodec) {
			// Same source in the OTHER codec mode (e.g. a .h265 while we make
			// .av1): leave it untouched and write our own differently-named output.
			continue
		}
		// Same cleaned name but a different source (different length, or same
		// length with a non-matching source tag) → write a numbered output.
		collision = true
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
				if cs, e := getVideoStats(ctx, p); e == nil && isOurFinishedOutput(cs) {
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

	fileSizeMB := stats.FileSizeMB
	lockPath := filepath.Join(outputDir, basenameFull+".lock")

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
		// Only OUR finished output aborts here; another codec mode's file or a
		// different source must not make us skip (mirrors the pre-lock check).
		if cs, e := getVideoStats(ctx, candidate); e == nil && !isOurFinishedOutput(cs) {
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
	// (8000), behaltenes 4K (-original) → maxBitrateOriginal (22000). Die
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
	doScale := needsScaling(cfg, stats.Width, stats.Height)

	// Einmal je Datei entscheiden, wo verkleinert wird — und zwar HIER, vor den
	// Encoder-Optionen: bleibt das Bild im Grafikspeicher, darf der Encoder kein
	// -pix_fmt bekommen (siehe gpuFramesStayOnCard).
	doDeint := videoIsInterlaced(stats)
	hwaccelOpts := gpuDecodeArgs(stats)
	gpuScale := gpuScaleUsable(hwaccelOpts, doScale, doDeint)
	gpuFramesStayOnCard = gpuScale && appSettings.casStrength <= 0
	if gpuScale {
		// Ohne dieses Ausgabeformat landen die entpackten Bilder sofort wieder
		// im Arbeitsspeicher — scale_cuda bekäme dann gar nichts zu sehen.
		hwaccelOpts = append(hwaccelOpts, "-hwaccel_output_format", "cuda")
	}

	// Source-derived bitrate cap (restored from 1.1.2, refined with the video
	// expert's matrix): aim for 80% of the source video bitrate so the re-encode is
	// guaranteed to shrink the file, never below the per-resolution floor, never
	// above the per-mode ceiling. -cq (unchanged) still drives the picture; calcKbps
	// only feeds -maxrate/-bufsize. The fixed per-mode ceiling used in 1.1.3 never
	// bit for low/mid-bitrate sources, so -cq alone let those files grow — the
	// post-encode safety net then discarded the encode and remuxed, so the tool
	// stopped actually compressing. The cap fixes that at the source.
	calcKbps := cappedTargetKbps(bitrateKbps, outputHeightFor(stats, doScale), effCfg.maxBitrateKbps)
	// reEncodeWorthwhile: if we are NOT downscaling and even the floor cannot
	// undercut the source (calcKbps ≥ source), a re-encode can only lose quality
	// (and risk growing), so we remux instead. Codec-agnostic — this catches the
	// highly-compressed H.264 case the user hit as well as already-lean HEVC/AV1.
	reEncodeWorthwhile := doScale || calcKbps < bitrateKbps

	// -av1 swaps the target codec: encoder opts, output suffix, "already
	// converted" detection and validation all follow targetCodec.
	targetCodec, outSuffix, codecLabel := "hevc", ".h265", "H.265"
	if cfg.av1 {
		targetCodec, outSuffix, codecLabel = "av1", ".av1", "AV1"
	}

	maxBR := fmt.Sprintf("%dk", calcKbps)
	bufBR := fmt.Sprintf("%dk", calcKbps*2)
	gopSize := calcGOP(stats.FPSNum, stats.FPSDen)
	// Der Encoder ergibt sich aus Zielcodec (-av1) und Backend (-cpu); die
	// Aufrufstelle unterscheidet nur noch, WELCHER Qualitätswert gilt:
	// -cq schlägt den fest eingestellten Wert der jeweiligen Skala, und
	// Auto-CQ ersetzt ihn weiter unten gegebenenfalls durch einen gemessenen.
	buildVideoOpts := activeVideoOptsBuilder(cfg.av1)
	cqValue := activeManualCQ(cfg.av1)
	if cfg.forcedCQ > 0 {
		// -cq: fester Wert nur für diesen Lauf; Auto-CQ wurde dafür schon
		// beim Argument-Einlesen abgeschaltet.
		cqValue = cfg.forcedCQ
	}
	nvencOpts := buildVideoOpts(cqValue, maxBR, bufBR, gopSize)
	// HDR signalling is carried by the color tags copied 1:1 from the source in
	// buildColorOpts (primaries/transfer/colorspace/range — only when present, so
	// nothing is fabricated). Mastering-display / MaxCLL static metadata rides
	// through automatically on stream-copy and on re-encodes without rescaling. We
	// deliberately do NOT reconstruct a -master_display string: a synthesized value
	// is exactly what has aborted HDR conversions in the past.
	nvencOpts = append(nvencOpts, buildColorOpts(stats)...)

	doConvert, doRemux := false, false
	switch {
	case strings.EqualFold(stats.VideoCodec, targetCodec) && ext == ".mkv" && !cfg.mp4Mode &&
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
	case !reEncodeWorthwhile:
		// Source already lean: even the resolution floor cannot undercut it, so a
		// re-encode would only burn GPU time and lose quality (the post-encode
		// safety net would discard it anyway). Remux instead — codec-agnostic, so
		// this also covers highly-compressed H.264, exactly the case reported.
		pInfo.Printf("%s Source already lean (%d kbps, target floor would be %d kbps) — remuxing instead of re-encoding.\n",
			pterm.LightMagenta("›"), bitrateKbps, calcKbps)
		doRemux = true
	default:
		doConvert = true
	}

	printDavinciAudioInfo(stats.AudioStreams, cfg.copyAudio)

	var vfOpts, vfOptsCPU []string
	if doConvert {
		if doDeint {
			pInfo.Printf("%s Interlaced source (%s) — deinterlacing with bwdif.\n",
				pterm.LightMagenta("›"), stats.FieldOrder)
		}
		filterChain := buildVideoFilter(doScale, doDeint, gpuScale)
		vfOpts = []string{"-vf", filterChain}
		if gpuScale {
			// Rückweg für den Fall, dass die Grafikkarte den Lauf abweist: eine
			// Kette mit scale_cuda kann ohne Bilder im Grafikspeicher nicht
			// laufen, der Rückfall muss sie also mit austauschen.
			vfOptsCPU = []string{"-vf", buildVideoFilter(doScale, doDeint, false)}
			pInfo.Printf("%s Scaling on the graphics card (Lanczos).\n",
				pterm.LightMagenta("›"))
		}

		// -autocq: pick the CQ for THIS file via sampled VMAF measurements.
		// Only real re-encodes get here with the flag set (remuxes skip encoding
		// entirely). H.265 and AV1 each search on their own CQ-scale profile —
		// same mechanism, different anchors/clamps. On analysis failure
		// autoDetectCQ already warned and the codec's configured CQ in nvencOpts
		// simply stays in effect.
		if cfg.autoCQ {
			scale := activeAutoCQScale(cfg.av1)
			if cq, ok := autoDetectCQ(ctx, filePath, stats, filterChain, maxBR, bufBR, gopSize, doScale, scale); ok {
				nvencOpts = scale.buildOpts(cq, maxBR, bufBR, gopSize)
				nvencOpts = append(nvencOpts, buildColorOpts(stats)...)
			}
		}
	}

	baseJob := convJob{
		inputPath:    filePath,
		nvencOpts:    nvencOpts,
		vfOpts:       vfOpts,
		vfOptsCPU:    vfOptsCPU,
		encodeOnCard: doConvert && gpuFramesStayOnCard,
		isTS:         ext == ".ts",
		isMKV:        ext == ".mkv",
		audioStreams: stats.AudioStreams,
		subCodecs:    stats.SubCodecs,
		sourceTag:    srcID,
	}
	// Nur ein echtes Umwandeln entpackt überhaupt Bilder; beim Remux werden die
	// Spuren nur umkopiert, da gibt es nichts zu beschleunigen.
	if doConvert {
		baseJob.hwaccelOpts = hwaccelOpts
	} else {
		// Ein Remux hat keine Filterkette — die Encoder-Optionen bleiben zwar
		// gebaut, werden aber nie benutzt. Die Merkvariable trotzdem
		// zurücksetzen, damit sie nicht in die nächste Datei hineinwirkt.
		gpuFramesStayOnCard = false
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
			// Ist das Entpacken auf der Grafikkarte in diesem Lauf schon einmal
			// gescheitert, nehmen auch alle folgenden Stufen gleich den
			// Prozessor-Weg statt denselben Fehler noch einmal zu bezahlen.
			if gpuDecodeDisabled {
				job = job.fallBackToCPUDecode()
			}
			job.outputPath = outputFile
			job.audioCopy = att.audioCopy
			job.withSubs = att.withSubs
			job.noAudio = att.noAudio
			job.pureAudioCopy = cfg.copyAudio
			err := runFFmpegWithCPUDecodeFallback(ctx, job, buildArgs,
				stats.DurationSec, idx, total, stats.FileSizeMB)
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
		outputFile = filepath.Join(outputDir, basenameFull+remuxSuffix(stats.VideoCodec)+".part.mkv")
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
			markPath := filepath.Join(dir, base+remuxSuffix(stats.VideoCodec)+".mkv")
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

		mkvFile := filepath.Join(outputDir, basenameFull+remuxSuffix(stats.VideoCodec)+".mkv")
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
		remuxArgs = append(remuxArgs, sourceTagArgs(srcID)...)
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
			remuxNoSubs = append(remuxNoSubs, sourceTagArgs(srcID)...)
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
		retireOriginal(cfg, filePath)
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
	} else {
		retireOriginal(cfg, filePath)
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
	// Origin tag NVENCForge stamps into its own outputs (case-insensitive: the
	// Matroska muxer may upper-case the key). Used by the skip check to separate
	// a real resume from a name collision.
	for k, v := range p.Format.Tags {
		if strings.EqualFold(k, sourceTagKey) {
			s.SourceTag = v
			break
		}
	}
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

// runFFmpegWithCPUDecodeFallback führt einen Umwandel-Lauf aus und wiederholt
// ihn EINMAL mit Entpacken auf dem Prozessor, falls der Versuch mit der
// Grafikkarte scheitert. Danach bleibt NVDEC für den Rest des Laufs aus.
//
// Was das auffängt: saubere Absagen der Grafikkarte — ein Format, das ihr
// Dekodierer nicht kennt, belegter Grafikspeicher, ein abweisender Treiber.
// In diesen Fällen beendet sich FFmpeg mit einer Fehlermeldung, und die Datei
// wird auf dem bewährten Weg fertig umgewandelt.
//
// Was das NICHT auffangen kann: einen echten Treiberabsturz (TDR), der das
// System mitreißt — dann läuft hier kein Code mehr. Davor schützt allein die
// Bitratengrenze in gpuDecodeArgs, die schon vor dem Versuch greift.
//
// Ein Abbruch durch den Nutzer (Strg+C) ist kein Grafikkarten-Fehler und wird
// deshalb unverändert durchgereicht.
func runFFmpegWithCPUDecodeFallback(ctx context.Context, job convJob,
	buildArgs func(convJob) []string, durationSec float64,
	fileIdx, fileTotal int, inputSizeMB float64) error {

	err := runFFmpeg(ctx, buildArgs(job), durationSec, fileIdx, fileTotal, inputSizeMB)
	if err == nil || len(job.hwaccelOpts) == 0 || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return err
	}
	pWarn.Println("GPU decoding failed — repeating this file with CPU decoding.")
	gpuDecodeDisabled = true
	gpuFramesStayOnCard = false
	return runFFmpeg(ctx, buildArgs(job.fallBackToCPUDecode()), durationSec, fileIdx, fileTotal, inputSizeMB)
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

// outputHeightFor returns the height the encoder will actually write: the source
// height, or the downscale short side (maxResolution) when scaling. The bitrate
// floor keys off the OUTPUT resolution, not the possibly-larger source.
func outputHeightFor(s *VideoStats, doScale bool) int {
	if doScale {
		return appSettings.maxResolution
	}
	return s.Height
}

// bitrateFloorKbps is the absolute minimum target bitrate per output resolution —
// a safety net so very low-bitrate sources don't turn to mush. Buckets agreed
// with the video expert (output height in px → kbps).
func bitrateFloorKbps(height int) int64 {
	switch {
	case height <= 720:
		return 800
	case height <= 1080:
		return 1500
	case height <= 1440:
		return 3000
	default:
		return 6000
	}
}

// bitrateTargetPercent is the share of the source video bitrate the encode aims
// at before the per-mode ceiling applies, so a re-encode shrinks the file even
// when the ceiling is generous. autoCQCapLimitsQuality repeats this arithmetic
// to tell a ceiling-limited Auto-CQ analysis from a genuinely exhausted source.
const bitrateTargetPercent = 80

// cappedTargetKbps restores the source-derived ceiling dropped in 1.1.3: target
// bitrateTargetPercent of the source video bitrate (so the re-encode shrinks),
// clamped UP to the resolution floor and DOWN to the per-mode ceiling. It only
// sets -maxrate/-bufsize; -cq still governs the picture. The ceiling is applied
// last so an explicit -NNNN override always wins, even over the floor.
func cappedTargetKbps(sourceKbps int64, outHeight int, ceiling int64) int64 {
	target := sourceKbps * bitrateTargetPercent / 100
	if floor := bitrateFloorKbps(outHeight); target < floor {
		target = floor
	}
	if ceiling > 0 && target > ceiling {
		target = ceiling
	}
	return target
}

// remuxSuffix maps a (passed-through) source video codec to the output-name
// suffix used for remuxes, mirroring the convert path's .h265/.av1 scheme so a
// remux is labelled by what it actually contains (e.g. .h264.mkv) instead of a
// generic ".remux". Every value returned here MUST also appear in skipSuffixes
// and skipInputSuffixes, otherwise re-running would not recognise the file as
// already processed. Exotic codecs fall back to ".remux" (still recognised).
func remuxSuffix(videoCodec string) string {
	switch strings.ToLower(strings.TrimSpace(videoCodec)) {
	case "hevc", "h265":
		return ".h265"
	case "h264", "avc":
		return ".h264"
	case "av1":
		return ".av1"
	default:
		return ".remux"
	}
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

// ----------------------------------------------------------------------------
// Verkleinern auf der Grafikkarte (scale_cuda)
// ----------------------------------------------------------------------------
//
// Warum das schneller ist: Wird auf der Karte entpackt (NVDEC) und dort auch
// verkleinert, verlässt das Bild den Grafikspeicher nie — der Umweg über den
// Arbeitsspeicher entfällt komplett. Gemessen am 2026-08-06 (4K→1080p, 90 s):
//
//	CPU-Bicubic + CAS 0.4 (der bisherige Weg) ....... 33,5 s   VMAF 97,32
//	GPU-Lanczos + CAS (Bild kommt für CAS zurück) ... 28,1 s   VMAF 97,62
//	GPU-Lanczos ohne CAS (Bild bleibt oben) ......... 13,4 s   VMAF 94,66
//
// (VMAF gegen das 4K-ORIGINAL gemessen, beide Seiten gleich hochgerechnet —
// die Zahl bewertet also den Skalierer mit.) Lanczos statt des bisherigen
// Bicubic ist dabei kein Zufall: Bicubic auf der Karte kam nur auf 90,40.
//
// Es gibt KEINEN CAS-Filter für die Grafikkarte (Filterliste geprüft), deshalb
// entscheidet casStrength den Weg: mit Nachschärfen muss das Bild einmal
// zurück in den Arbeitsspeicher, ohne bleibt es oben. Beide Wege sind
// schneller als vorher.

// gpuScaleUsable meldet, ob das Verkleinern dieser Datei auf der Grafikkarte
// laufen kann. Voraussetzung ist, dass die Bilder überhaupt dort liegen (also
// NVDEC das Entpacken übernimmt) und dass keine CPU-Filter davor müssen:
// bwdif (Deinterlacing) rechnet auf dem Prozessor und bekäme keine Bilder.
func gpuScaleUsable(hwaccelOpts []string, doScale, deinterlace bool) bool {
	return len(hwaccelOpts) > 0 && doScale && !deinterlace
}

// gpuPixFmt ist das Bildformat IM Grafikspeicher. Dort gibt es nur die
// verschränkten Formate: nv12 für 8 Bit, p010le für 10 Bit — ein planares
// yuv420p kennt die Karte nicht.
func gpuPixFmt() string {
	if eightBitActive {
		return "nv12"
	}
	return "p010le"
}

// buildGPUScaleChain baut den Teil der Filterkette, der auf der Karte läuft.
// Endet die Kette hier, gehen die Bilder direkt in den Encoder; ist
// Nachschärfen eingestellt, holt hwdownload sie in den Arbeitsspeicher zurück,
// wo cas rechnen kann.
func buildGPUScaleChain() string {
	short := appSettings.maxResolution
	long := short * 16 / 9
	chain := fmt.Sprintf(
		"scale_cuda='if(gte(iw,ih),%d,%d)':'if(gte(iw,ih),%d,%d)'"+
			":force_original_aspect_ratio=decrease:force_divisible_by=2"+
			":interp_algo=lanczos:format=%s",
		long, short, short, long, gpuPixFmt())
	if appSettings.casStrength > 0 {
		chain += ",hwdownload,format=" + gpuPixFmt() +
			",cas=strength=" + strconv.FormatFloat(appSettings.casStrength, 'f', -1, 64) +
			",format=" + encodePixFmt()
	}
	return chain
}

// chainUsesGPU meldet, ob eine Filterkette auf der Grafikkarte rechnet. Nötig
// an den Stellen, die eine solche Kette anders behandeln müssen — etwa beim
// Rückfall auf den Prozessor, wo sie durch die CPU-Kette ersetzt werden muss.
func chainUsesGPU(chain string) bool {
	return strings.Contains(chain, "scale_cuda")
}

// chainEndsOnCard meldet, ob die Bilder am Ende der Kette noch im
// Grafikspeicher liegen. Zwei Dinge hängen daran: der Encoder darf dann KEIN
// -pix_fmt bekommen (FFmpeg würde einen CPU-Formatwandler einhängen, den es
// für Bilder im Grafikspeicher nicht gibt — Fehlermeldung "Impossible to
// convert between the formats"), und jeder nachfolgende CPU-Filter braucht
// vorher ein hwdownload.
func chainEndsOnCard(chain string) bool {
	return chainUsesGPU(chain) && !strings.Contains(chain, "hwdownload")
}

// filterChainToCPU ergänzt eine auf der Karte endende Kette um den Rückweg in
// den Arbeitsspeicher. Für alles, was danach ein CPU-Filter anfassen muss:
// der VMAF-Vergleich und der verlustfreie Zwischenspeicher der Auto-CQ-Suche.
func filterChainToCPU(chain string) string {
	if !chainEndsOnCard(chain) {
		return chain
	}
	return chain + ",hwdownload,format=" + gpuPixFmt()
}

// ----------------------------------------------------------------------------
// Bittiefe: 10 Bit ist der Auslieferungszustand, -8bit die Ausnahme
// ----------------------------------------------------------------------------
//
// Warum 10 Bit die Vorgabe bleibt: H.265 kostet dafür praktisch nichts und
// vermeidet die Streifen, die in dunklen Verläufen sonst sichtbar werden (der
// Encoder rechnet intern feiner). 8 Bit gibt es nur für Geräte, die das Profil
// "Main 10" gar nicht dekodieren können — ältere Fernseher, Beamer, Handys.
// Bei 8 Bit ist das Format für beide Encoder-Familien dasselbe.

// encodePixFmt ist das Pixelformat für NVENC und für das Ende der Filterkette.
func encodePixFmt() string {
	if eightBitActive {
		return "yuv420p"
	}
	return "p010le"
}

// cpuEncodePixFmt ist das Gegenstück für libx265/libsvtav1, die die
// planare Schreibweise erwarten.
func cpuEncodePixFmt() string {
	if eightBitActive {
		return "yuv420p"
	}
	return "yuv420p10le"
}

// hevcProfileName: "main10" trägt 10 Bit, "main" ist das 8-Bit-Profil, das
// auch alte Geräte kennen. Gilt für hevc_nvenc und libx265 gleichermaßen.
func hevcProfileName() string {
	if eightBitActive {
		return "main"
	}
	return "main10"
}

// encoderInputFormatArgs liefert das Eingangsformat für die NVENC-Encoder —
// außer wenn die Bilder im Grafikspeicher bleiben: dann legt scale_cuda das
// Format fest und ein zusätzliches -pix_fmt würde den Lauf abbrechen
// (siehe gpuFramesStayOnCard).
func encoderInputFormatArgs() []string {
	if gpuFramesStayOnCard {
		return nil
	}
	return []string{"-pix_fmt", encodePixFmt()}
}

func buildVideoFilter(doScale, deinterlace, gpuScale bool) string {
	// bwdif before any scaling: deinterlacing needs the original field
	// structure. send_frame keeps the source frame rate (25i → 25p).
	pre := ""
	if deinterlace {
		pre = "bwdif=mode=send_frame,"
	}
	if doScale && gpuScale {
		return pre + buildGPUScaleChain()
	}
	if doScale {
		short := appSettings.maxResolution
		long := short * 16 / 9
		chain := fmt.Sprintf(
			"scale='if(gte(iw,ih),%d,%d)':'if(gte(iw,ih),%d,%d)'"+
				":force_original_aspect_ratio=decrease:force_divisible_by=2",
			long, short, short, long)
		// casStrength=0 lässt das Nachschärfen ganz weg statt es mit Stärke 0
		// mitlaufen zu lassen: der Filter würde sonst jedes Bild anfassen, ohne
		// etwas zu ändern. Gemessen 2026-07-29 kostet er rund 8 s je 90 s Video
		// (4K→1080p) — der zweitteuerste Posten der ganzen Kette.
		if appSettings.casStrength > 0 {
			chain += ",cas=strength=" +
				strconv.FormatFloat(appSettings.casStrength, 'f', -1, 64)
		}
		return pre + chain + ",format=" + encodePixFmt()
	}
	return pre + "crop=trunc(iw/2)*2:trunc(ih/2)*2,format=" + encodePixFmt()
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

// buildNVENCOptsWithCQ assembles the H.265 encoder options at the given CQ.
// -cq and Auto-CQ swap only that value; every other parameter stays identical
// — which also guarantees the Auto-CQ sample encodes run with exactly the
// settings of the real encode. The fixed value for manual mode comes from
// activeManualCQ (targetCQ here), so all four encoder backends are chosen in
// one place.
// AQ options use the dash spellings (-spatial-aq/-temporal-aq): FFmpeg
// master removed the old underscore aliases in 2026, and the dash form
// exists in every supported build.
func buildNVENCOptsWithCQ(cq int, maxBitrate, bufsize string, gop int) []string {
	opts := []string{
		"-c:v", "hevc_nvenc", "-rc", "vbr", "-cq", strconv.Itoa(cq),
		"-b:v", "0", "-maxrate", maxBitrate, "-bufsize", bufsize,
		"-profile:v", hevcProfileName(),
		"-preset", appSettings.nvencPreset, "-tune", "hq",
		"-rc-lookahead", strconv.Itoa(appSettings.nvencLookahead), "-fps_mode", "cfr",
		"-g", strconv.Itoa(gop), "-spatial-aq", "1",
		"-aq-strength", "8", "-bf", strconv.Itoa(appSettings.bFrames),
	}
	opts = append(opts, encoderInputFormatArgs()...)
	// Temporal AQ + multipass need Turing (RTX 20) or newer. checkHardwareCapabilities
	// clears nvencAdvancedAQ on older cards so the encode drops them instead of
	// failing on every file.
	if nvencAdvancedAQ {
		opts = append(opts, "-multipass", "qres", "-temporal-aq", "1")
	}
	// b_ref_mode needs B-frames; older GPUs (no B-frame support) reject it.
	if appSettings.bFrames > 0 {
		opts = append(opts, "-b_ref_mode", "2")
	}
	return opts
}

// buildAV1OptsWithCQ mirrors buildNVENCOptsWithCQ for av1_nvenc. Differences:
// own CQ scale (1-63, av1TargetCQ), no -profile (Main covers 8/10-bit), no
// B-frame options (not exposed by av1_nvenc), AQ flags use hyphens.
func buildAV1OptsWithCQ(cq int, maxBitrate, bufsize string, gop int) []string {
	opts := []string{
		"-c:v", "av1_nvenc", "-rc", "vbr", "-cq", strconv.Itoa(cq),
		"-b:v", "0", "-maxrate", maxBitrate, "-bufsize", bufsize,
		"-preset", appSettings.nvencPreset, "-tune", "hq",
		"-multipass", "qres", "-rc-lookahead", strconv.Itoa(appSettings.nvencLookahead), "-fps_mode", "cfr",
		"-g", strconv.Itoa(gop), "-spatial-aq", "1", "-temporal-aq", "1",
		"-aq-strength", "8",
	}
	return append(opts, encoderInputFormatArgs()...)
}

// ----------------------------------------------------------------------------
// CPU-Encoder (-cpu): dieselben Bilder ohne Nvidia-Karte
// ----------------------------------------------------------------------------

// buildX265OptsWithCQ ist das CPU-Gegenstück zu buildNVENCOptsWithCQ. Alles,
// was das Bild bestimmt, bleibt gleich (10 Bit, Bitraten-Deckel, GOP) — nur
// der Encoder wechselt auf libx265. Die NVENC-eigenen Regler (Lookahead,
// B-Frames, spatial/temporal AQ, multipass) haben hier bewusst KEIN
// Gegenstück: x265 steuert das über sein Preset selbst und besser, als
// einzeln durchgereichte Werte es könnten.
//
// -crf ist NICHT dieselbe Skala wie NVENCs -cq: aus der Messreihe vom
// 2026-07-25 liegt gleiche Qualität bei rund CQ-7 (NVENC CQ 26 entspricht
// x265 CRF ~19). Deshalb hat der CPU-Modus mit cpuTargetCRF einen eigenen
// INI-Schlüssel und mit x265AutoCQScale ein eigenes Auto-CQ-Profil.
//
// log-level=error unterdrückt x265' gesprächige Statuszeilen, die sonst die
// Fortschrittsanzeige in runFFmpeg überschreiben würden.
func buildX265OptsWithCQ(crf int, maxBitrate, bufsize string, gop int) []string {
	opts := []string{
		"-c:v", "libx265", "-crf", strconv.Itoa(crf),
		"-maxrate", maxBitrate, "-bufsize", bufsize,
		"-profile:v", hevcProfileName(), "-pix_fmt", cpuEncodePixFmt(),
		"-preset", appSettings.cpuPreset, "-fps_mode", "cfr",
		"-g", strconv.Itoa(gop),
		"-x265-params", "log-level=error",
	}
	// 0 = alle Kerne (Standard). Ein Deckel hält den Rechner bedienbar,
	// während im Hintergrund encodiert wird.
	if appSettings.cpuThreads > 0 {
		opts = append(opts, "-threads", strconv.Itoa(appSettings.cpuThreads))
	}
	return opts
}

// buildSVTAV1OptsWithCQ spiegelt buildX265OptsWithCQ für AV1 auf dem
// Prozessor. SVT-AV1 kennt keine Profil-Angabe (Main deckt 8 und 10 Bit ab)
// und zählt sein Preset numerisch (0 = langsamst/bester, 13 = schnellst).
// Die Thread-Begrenzung heißt hier lp (logical processors) — libsvtav1
// ignoriert das allgemeine -threads.
func buildSVTAV1OptsWithCQ(crf int, maxBitrate, bufsize string, gop int) []string {
	opts := []string{
		"-c:v", "libsvtav1", "-crf", strconv.Itoa(crf),
		"-maxrate", maxBitrate, "-bufsize", bufsize,
		"-pix_fmt", cpuEncodePixFmt(),
		"-preset", strconv.Itoa(appSettings.cpuAV1Preset), "-fps_mode", "cfr",
		"-g", strconv.Itoa(gop),
	}
	if appSettings.cpuThreads > 0 {
		opts = append(opts, "-svtav1-params", "lp="+strconv.Itoa(appSettings.cpuThreads))
	}
	return opts
}

// activeVideoOptsBuilder liefert den Options-Bauer des aktiven Backends für
// den Zielcodec. Alle vier Bauer haben dieselbe Signatur, damit -cq und
// Auto-CQ nur den Qualitätswert austauschen müssen und die Aufrufstellen
// nichts über GPU oder CPU wissen.
func activeVideoOptsBuilder(av1 bool) func(cq int, maxBitrate, bufsize string, gop int) []string {
	switch {
	case av1 && cpuModeActive:
		return buildSVTAV1OptsWithCQ
	case av1:
		return buildAV1OptsWithCQ
	case cpuModeActive:
		return buildX265OptsWithCQ
	default:
		return buildNVENCOptsWithCQ
	}
}

// activeManualCQ liefert den fest eingestellten Qualitätswert des aktiven
// Backends — den Wert, der ohne Auto-CQ und ohne -cq gilt. Jede Kombination
// hat einen eigenen INI-Schlüssel, weil dieselbe Zahl je Encoder etwas
// völlig anderes bedeutet.
func activeManualCQ(av1 bool) int {
	switch {
	case av1 && cpuModeActive:
		return appSettings.cpuAV1TargetCRF
	case av1:
		return appSettings.av1TargetCQ
	case cpuModeActive:
		return appSettings.cpuTargetCRF
	default:
		return appSettings.targetCQ
	}
}

// activeAutoCQScale liefert das Auto-CQ-Profil des aktiven Backends. Die
// Suchmechanik ist für alle vier identisch — nur Anker, Klemmen und
// Schrittbreiten unterscheiden sich, weil jede CQ/CRF-Skala anders tickt.
func activeAutoCQScale(av1 bool) autoCQScale {
	switch {
	case av1 && cpuModeActive:
		return svtav1AutoCQScale
	case av1:
		return av1AutoCQScale
	case cpuModeActive:
		return x265AutoCQScale
	default:
		return hevcAutoCQScale
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

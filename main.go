//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

// NVENCForge — H265 Batch-Konverter + DaVinci Resolve Workflow + Split/Join
//
// Version 2: HDR10-bewusstes Encoding (behält bei PQ/HLG-Material die
// Originalauflösung, hebt das Bitraten-Limit an und übernimmt Mastering-Display
// + MaxCLL), Unicode-fähige Dateinamen-Bereinigung (jedes Schriftsystem
// weltweit) und ein gehärteter FFmpeg-Auto-Downloader (Verbindungs-/Antwort-
// Timeouts). Basiert auf der 6-Datei-Architektur mit fmt.Errorf/%w-Wrapping.
//
// Kompilieren:
//
//	go mod init NVENCForge
//	go mod tidy
//	go build -ldflags="-s -w" -o NVENCForge.exe
//
// Lange Pfade (>260 Zeichen) aktivieren (Admin-CMD):
//
//	reg add "HKLM\SYSTEM\CurrentControlSet\Control\FileSystem" /v LongPathsEnabled /t REG_DWORD /d 1 /f

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/pterm/pterm"
)

// appVersion is shown in the startup header so the running build is obvious.
// Keep it in sync with the git tag / GitHub release on every release.
const appVersion = "1.1.7"

// ----------------------------------------------------------------------------
// Package-level sentinels and tool paths (set once in initTools, read-only after)
// ----------------------------------------------------------------------------

var (
	ffmpegPath  string
	ffprobePath string
)

// errFFmpegStall is the sentinel reported by the stall-watchdog via
// context.WithCancelCause. After the run it is checked with errors.Is against
// context.Cause(runCtx).
var errFFmpegStall = errors.New("ffmpeg stall timeout")

// ----------------------------------------------------------------------------
// Video file classification
// ----------------------------------------------------------------------------

var videoExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".ts": true, ".avi": true,
	".mov": true, ".flv": true, ".wmv": true, ".webm": true,
	".m4v": true, ".mts": true, ".m2ts": true,
}

// skipSuffixes / skipInputSuffixes recognise NVENCForge's own outputs so a
// re-run never re-encodes an already-processed file. They MUST list every suffix
// remuxSuffix() can emit (.h265/.h264/.av1) plus the legacy ".remux" fallback.
var skipSuffixes = []string{".h265", ".h264", ".remux", ".av1"}
var skipInputSuffixes = []string{".h265", ".h264", ".remux", ".preview", ".av1"}

// sourceTagKey is the global container metadata key NVENCForge writes into every
// output it produces (see sourceTagArgs). It records the exact source file name
// so the "already converted" skip check can tell a genuine resume apart from a
// name collision — two different sources whose cleaned names (and durations)
// happen to match. Matroska may store the key upper-cased, so read it back with
// a case-insensitive compare.
const sourceTagKey = "NVENCFORGE_SOURCE"

// ----------------------------------------------------------------------------
// Filename normalisation (integrated from CleanVideoNames)
// Applied ONLY to video basenames, NEVER to directory names.
// ----------------------------------------------------------------------------

var markersDrop = map[string]bool{
	"ts": true, "m2ts": true,
	"web": true, "webrip": true, "webdl": true, "dl": true,
	"bluray": true, "bdrip": true, "remux": true, "bdremux": true,
	"hdtv": true, "dvdrip": true, "dvd": true, "p2p": true,
	"mp4": true, "mkv": true, "avi": true, "mov": true,
	"m4v": true, "wmv": true, "flv": true,
	"mpg": true, "mpeg": true, "webm": true,
	"xvid": true, "divx": true,
	"proper": true, "repack": true, "internal": true,
}

var markersKeep = map[string]string{
	"h264": "h264", "h265": "h265",
	"x264": "x264", "x265": "x265",
	"hevc": "hevc", "av1": "av1", "vp9": "vp9",
	"720p": "720p", "1080p": "1080p", "1440p": "1440p", "2160p": "2160p", "4k": "4k",
	"hdr": "hdr", "hdr10": "hdr10", "sdr": "sdr",
	"10bit": "10bit", "8bit": "8bit",
	"aac": "aac", "ac3": "ac3", "eac3": "eac3", "dts": "dts", "opus": "opus",
}

var (
	reHashDigit = regexp.MustCompile(`#(\d)`)
	reMultiDot  = regexp.MustCompile(`\.{2,}`)
)

// normalizeName cleans a base name: any Unicode letter or digit survives — so
// non-Latin scripts (CJK, Cyrillic, Greek, Arabic, …) are preserved for users
// worldwide — plus the dot separator and any user-approved characters from
// extraFilenameChars (NVENCForge_Config.ini). Whitespace ALWAYS becomes a dot
// (not overridable), everything else (punctuation, symbols, emoji, control
// chars) becomes a dot too and is then collapsed by reMultiDot.
func normalizeName(s string) string {
	s = reHashDigit.ReplaceAllString(s, "Nr$1")
	s = strings.ReplaceAll(s, "#", "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '.':
			return r
		case unicode.IsSpace(r):
			return '.'
		case strings.ContainsRune(appSettings.extraFilenameChars, r):
			return r
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return r
		}
		return '.'
	}, s)
	s = reMultiDot.ReplaceAllString(s, ".")
	return strings.Trim(s, ".")
}

func extractTrailingMarkers(name string) (string, []string) {
	var keep []string
	for {
		idx := strings.LastIndex(name, ".")
		if idx == -1 {
			break
		}
		tok := strings.ToLower(name[idx+1:])
		if markersDrop[tok] {
			name = name[:idx]
			continue
		}
		if c, ok := markersKeep[tok]; ok {
			keep = append([]string{c}, keep...)
			name = name[:idx]
			continue
		}
		break
	}
	if len(keep) > 0 && strings.Contains(name, ".") {
		var f []string
		for _, t := range strings.Split(name, ".") {
			if t != "" && !markersDrop[strings.ToLower(t)] {
				f = append(f, t)
			}
		}
		name = strings.Join(f, ".")
	}
	if name != "" {
		nl := strings.ToLower(name)
		if markersDrop[nl] {
			name = ""
		} else if c, ok := markersKeep[nl]; ok {
			keep = append([]string{c}, keep...)
			name = ""
		}
	}
	return name, keep
}

// cleanFileBaseName returns "" when nothing usable remains; callers keep the
// original name in that case.
func cleanFileBaseName(baseNoExt string) string {
	main, keep := extractTrailingMarkers(normalizeName(baseNoExt))
	if len(keep) == 0 {
		return main
	}
	if main == "" {
		return strings.Join(keep, ".")
	}
	return main + "." + strings.Join(keep, ".")
}

// ----------------------------------------------------------------------------
// Data structures
// ----------------------------------------------------------------------------

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	CodecType      string             `json:"codec_type"`
	CodecName      string             `json:"codec_name"`
	Width          int                `json:"width"`
	Height         int                `json:"height"`
	RFrameRate     string             `json:"r_frame_rate"`
	BitRate        string             `json:"bit_rate"`
	Channels       int                `json:"channels"`
	ChannelLayout  string             `json:"channel_layout"`
	SampleRate     string             `json:"sample_rate"`
	FieldOrder     string             `json:"field_order"`
	ColorSpace     string             `json:"color_space"`
	ColorTransfer  string             `json:"color_transfer"`
	ColorPrimaries string             `json:"color_primaries"`
	ColorRange     string             `json:"color_range"`
	Tags           map[string]string  `json:"tags"`
	Disposition    ffprobeDisposition `json:"disposition"`
}

type ffprobeDisposition struct {
	AttachedPic     int `json:"attached_pic"`
	Forced          int `json:"forced"`
	HearingImpaired int `json:"hearing_impaired"`
}

type ffprobeFormat struct {
	Duration string            `json:"duration"`
	BitRate  string            `json:"bit_rate"`
	Tags     map[string]string `json:"tags"`
}

type AudioStreamInfo struct {
	Codec      string
	Channels   int
	Layout     string
	Language   string
	Title      string
	SampleRate int
}

type VideoStats struct {
	VideoCodec     string
	AudioCodec     string
	Channels       int
	AudioStreams   []AudioStreamInfo
	SubCodecs      []string
	Width          int
	Height         int
	FPSNum         int
	FPSDen         int
	DurationSec    float64
	BitrateBps     int64
	FileSizeMB     float64
	FieldOrder     string
	ColorSpace     string
	ColorTransfer  string
	ColorPrimaries string
	ColorRange     string
	SourceTag      string // sourceTagKey value found in the container (origin file name), "" if none
}

type ProcessResult struct {
	InputFile  string
	OutputFile string
	SavedMB    float64
	Success    bool
	Skipped    bool
	IsPreview  bool
	NoAudio    bool // video-only fallback used: output has no sound, original kept
	ErrMsg     string
	FailedAt   time.Time
}

type lockInfo struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	Source     string    `json:"source"`
	SizeMB     float64   `json:"size_mb"`
	OwnerImage string    `json:"owner_image,omitempty"`
	Hostname   string    `json:"hostname,omitempty"`
}

// ----------------------------------------------------------------------------
// pterm printers with custom prefixes
// ----------------------------------------------------------------------------

var (
	pWarn = pterm.Warning.WithPrefix(pterm.Prefix{
		Text:  " WARNING ",
		Style: pterm.NewStyle(pterm.BgYellow, pterm.FgBlack, pterm.Bold),
	})
	pErr = pterm.Error.WithPrefix(pterm.Prefix{
		Text:  " ERROR ",
		Style: pterm.NewStyle(pterm.BgRed, pterm.FgWhite, pterm.Bold),
	})
	pOK = pterm.Success.WithPrefix(pterm.Prefix{
		Text:  "   OK   ",
		Style: pterm.NewStyle(pterm.BgGreen, pterm.FgBlack, pterm.Bold),
	})
	pInfo = pterm.Info.WithPrefix(pterm.Prefix{
		Text:  "  INFO  ",
		Style: pterm.NewStyle(pterm.BgCyan, pterm.FgBlack, pterm.Bold),
	})
	pAbort = pterm.Error.WithPrefix(pterm.Prefix{
		Text:  " ABORT ",
		Style: pterm.NewStyle(pterm.BgRed, pterm.FgLightWhite, pterm.Bold),
	})
	// pFatal is never silenced by -debug. Reserved for run-blocking startup
	// errors the user must see (missing GPU, FFmpeg setup) — unlike pErr, which
	// reports per-operation failures and is suppressed without -debug.
	pFatal = pterm.Error.WithPrefix(pterm.Prefix{
		Text:  " ERROR ",
		Style: pterm.NewStyle(pterm.BgRed, pterm.FgWhite, pterm.Bold),
	})
)

// ----------------------------------------------------------------------------
// Debug switch (hidden, developer-only)
// ----------------------------------------------------------------------------

// debugMode is set once at the start of main() and read-only afterwards. When
// false, all pErr output is routed to io.Discard so end users never see internal
// failure reasons. Intentionally undocumented (absent from help and tips).
var debugMode bool

// davinciMode is true when the process runs in -davinci mode (the DaVinci
// Resolve workflow). Set once at the start of main(); read by the abort
// handlers to pick the right message (there is no preview file in this mode).
var davinciMode bool

// splitMode / joinMode are true for the lossless -split / -join modes. Like
// -davinci they produce no preview file, so the abort handler treats them the
// same way (unfinished outputs are removed, nothing is salvaged).
var (
	splitMode bool
	joinMode  bool
)

// consumeDebugFlag scans os.Args for a "-debug" token (case-insensitive),
// removes it so it is never treated as input, and reports whether it was present.
func consumeDebugFlag() bool {
	found := false
	out := os.Args[:0]
	for _, a := range os.Args {
		if strings.EqualFold(a, "-debug") {
			found = true
			continue
		}
		out = append(out, a)
	}
	os.Args = out
	return found
}

// ----------------------------------------------------------------------------
// Signal handling
// ----------------------------------------------------------------------------

// setupSignalContext returns the root context and its cancel function.
//   - First Ctrl+C: ctx is cancelled → runFFmpeg sends FFmpeg "q" (preview is
//     cleanly finalized).
//   - Second Ctrl+C: immediate hard exit.
func setupSignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
		fmt.Println()
		if davinciMode || splitMode || joinMode {
			pAbort.Println("Ctrl+C detected. Aborting — unfinished files will be removed...")
		} else {
			pAbort.Println("Ctrl+C detected. Finishing current task cleanly (preview will be saved)...")
		}

		<-sigChan
		fmt.Print("\033[?25h\033[?7h")
		fmt.Println()
		pAbort.WithPrefix(pterm.Prefix{
			Text:  " FORCE QUIT ",
			Style: pterm.NewStyle(pterm.BgRed, pterm.FgLightWhite, pterm.Bold),
		}).Println("Second Ctrl+C detected. Exiting immediately!")
		os.Exit(1)
	}()

	return ctx, cancel
}

// ----------------------------------------------------------------------------
// Tool detection: local folder first, then system PATH
// ----------------------------------------------------------------------------

// initTools resolves ffmpeg.exe and ffprobe.exe. If neither is found locally
// nor in PATH, it calls downloadFFmpeg to fetch them automatically.
func initTools() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("main.go: initTools (os.Executable): %w", err)
	}
	exeDir := filepath.Dir(exePath)

	resolve := func(name string) (string, bool) {
		local := filepath.Join(exeDir, name)
		if info, statErr := os.Stat(local); statErr == nil && !info.IsDir() {
			return local, true
		}
		if p, lookErr := exec.LookPath(name); lookErr == nil {
			return p, true
		}
		return "", false
	}

	fp, okF := resolve("ffmpeg.exe")
	pp, okP := resolve("ffprobe.exe")

	if !okF || !okP {
		pInfo.Println("FFmpeg not found locally or in PATH. Attempting auto-download...")
		if dlErr := downloadFFmpeg(exeDir); dlErr != nil {
			return fmt.Errorf("main.go: initTools: auto-download failed: %w", dlErr)
		}
		// Re-resolve after download.
		fp, okF = resolve("ffmpeg.exe")
		pp, okP = resolve("ffprobe.exe")
		if !okF || !okP {
			return errors.New("main.go: initTools: ffmpeg.exe / ffprobe.exe still missing after download")
		}
	}

	ffmpegPath = fp
	ffprobePath = pp
	return nil
}

// ----------------------------------------------------------------------------
// Hardware check: NVENC HEVC 10-bit dummy encode + CAS filter probe
// ----------------------------------------------------------------------------

// nvencAdvancedAQ is true while the GPU supports Temporal AQ + multipass (Turing
// / RTX 20 series or newer). checkHardwareCapabilities clears it for older cards
// (Pascal/Volta) so the real encode drops -temporal_aq/-multipass instead of
// failing on every single file.
var nvencAdvancedAQ = true

// checkHardwareCapabilities probes with the SAME flags the real encode uses, so a
// card that passes here cannot fail later on every file. HEVC B-frames AND Temporal
// AQ/multipass share the Turing+ gate; older cards (Maxwell-2/Pascal/Volta) are
// retried once fully degraded and then run without those features instead of
// refusing to start. Maxwell-1 / no-NVENC cards fail the 10-bit probe outright.
func checkHardwareCapabilities() error {
	pInfo.Println("Checking GPU capabilities (NVENC HEVC 10-bit)...")

	tryEncode := func(bf int, advancedAQ bool) (string, error) {
		args := []string{
			"-v", "error", "-f", "lavfi",
			"-i", "color=c=black:s=1920x1080:d=1",
			"-c:v", "hevc_nvenc", "-profile:v", "main10", "-pix_fmt", "p010le",
			"-preset", appSettings.nvencPreset, "-tune", "hq",
			"-rc-lookahead", strconv.Itoa(appSettings.nvencLookahead),
			"-spatial_aq", "1",
		}
		if advancedAQ {
			args = append(args, "-multipass", "qres", "-temporal_aq", "1")
		}
		if bf > 0 {
			args = append(args, "-bf", strconv.Itoa(bf), "-b_ref_mode", "2")
		}
		args = append(args, "-f", "null", "-")
		dummy := exec.Command(ffmpegPath, args...)
		dummy.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
		out, err := dummy.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if out, err := tryEncode(appSettings.bFrames, true); err != nil {
		// HEVC B-frames and Temporal AQ share the same Turing+ gate, so a single
		// fully-degraded retry (no B-frames, no Temporal AQ/multipass) decides it:
		// succeeds → pre-Turing card (Pascal/Volta), keep encoding without those
		// features; fails → genuine rejection (Maxwell-1, no 10-bit, no NVENC).
		if _, retryErr := tryEncode(0, false); retryErr == nil {
			if appSettings.bFrames > 0 {
				appSettings.bFrames = 0
				pWarn.Println("GPU does not support HEVC B-frames — continuing without B-frames.")
				pWarn.Println("Set 'bFrames=0' in NVENCForge_Config.ini to make this permanent.")
			}
			nvencAdvancedAQ = false
			pWarn.Println("GPU does not support Temporal AQ / multipass (needs RTX 20 series or newer) — continuing without them.")
		} else {
			return fmt.Errorf("main.go: checkHardwareCapabilities: NVENC dummy encode failed: %v | %s",
				err, out)
		}
	}

	filters := exec.Command(ffmpegPath, "-v", "error", "-filters")
	filters.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
	out, err := filters.Output()
	if err != nil {
		return fmt.Errorf("main.go: checkHardwareCapabilities: cannot read FFmpeg filter list: %w", err)
	}
	hasCAS := false
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[1] == "cas" {
			hasCAS = true
			break
		}
	}
	if !hasCAS {
		return errors.New("main.go: checkHardwareCapabilities: CAS filter missing in FFmpeg build")
	}
	return nil
}

// checkAV1Capability probes a 10-bit av1_nvenc dummy encode. AV1 encoding
// needs an RTX 40 series GPU (Ada) or newer; older cards fail here cleanly.
func checkAV1Capability() error {
	pInfo.Println("Checking GPU capabilities (NVENC AV1 10-bit)...")
	args := []string{
		"-v", "error", "-f", "lavfi",
		"-i", "color=c=black:s=1920x1080:d=1",
		"-c:v", "av1_nvenc", "-pix_fmt", "p010le",
		"-f", "null", "-",
	}
	dummy := exec.Command(ffmpegPath, args...)
	dummy.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
	out, err := dummy.CombinedOutput()
	if err != nil {
		return fmt.Errorf("main.go: checkAV1Capability: AV1 dummy encode failed: %v | %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ----------------------------------------------------------------------------
// Lock management
// ----------------------------------------------------------------------------

func readLockInfo(lockPath string) (lockInfo, error) {
	var info lockInfo
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return info, fmt.Errorf("main.go: readLockInfo (ReadFile): %w", err)
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, fmt.Errorf("main.go: readLockInfo (Unmarshal): %w", err)
	}
	if info.StartedAt.IsZero() {
		if st, statErr := os.Stat(lockPath); statErr == nil {
			info.StartedAt = st.ModTime()
		}
	}
	return info, nil
}

// removeStaleLock returns (removed bool, foreignHost string).
// If foreignHost != "" the lock belongs to another machine and is never removed.
func removeStaleLock(lockPath string) (bool, string) {
	info, err := readLockInfo(lockPath)
	if err != nil {
		// errors.Is unwraps the fmt.Errorf chain from readLockInfo
		// (os.IsNotExist would never match the wrapped error).
		if errors.Is(err, fs.ErrNotExist) {
			return true, ""
		}
		data, readErr := os.ReadFile(lockPath)
		if readErr == nil && len(bytes.TrimSpace(data)) == 0 {
			_ = os.Remove(lockPath)
			return true, ""
		}
		st, statErr := os.Stat(lockPath)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return true, ""
			}
			return false, ""
		}
		if time.Since(st.ModTime()) > 5*time.Minute {
			_ = os.Remove(lockPath)
			return true, ""
		}
		return false, ""
	}
	localHost, _ := os.Hostname()
	if info.Hostname != "" && localHost != "" &&
		!strings.EqualFold(info.Hostname, localHost) {
		return false, info.Hostname
	}
	if isLockOwnerAlive(info) {
		return false, ""
	}
	_ = os.Remove(lockPath)
	return true, ""
}

// FIX LOCK-02: Sync() vor Close() stellt sicher, dass der JSON-Inhalt auf Disk
// landet bevor andere Instanzen das Lockfile lesen können.
func acquireProcessingLock(lockPath string, sizeMB float64, sourceFile string) (func(), error) {
	exePath, _ := os.Executable()
	hostname, _ := os.Hostname()
	payload := lockInfo{
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC(),
		Source:     filepath.Base(sourceFile),
		SizeMB:     sizeMB,
		OwnerImage: exePath,
		Hostname:   hostname,
	}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	if len(encoded) == 0 {
		encoded = []byte(fmt.Sprintf(
			`{"pid":%d,"started_at":%q,"source":%q,"size_mb":%.2f,"owner_image":%q,"hostname":%q}`,
			payload.PID, payload.StartedAt.Format(time.RFC3339),
			payload.Source, payload.SizeMB, payload.OwnerImage, payload.Hostname))
	}

	for tries := 0; tries < 3; tries++ {
		lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			if _, wErr := lf.Write(encoded); wErr != nil {
				_ = lf.Close()
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("main.go: acquireProcessingLock (write): %w", wErr)
			}
			_ = lf.Sync()
			_ = lf.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("main.go: acquireProcessingLock (OpenFile): %w", err)
		}
		removed, foreignHost := removeStaleLock(lockPath)
		if foreignHost != "" {
			return nil, fmt.Errorf("main.go: acquireProcessingLock: already being processed on PC %q", foreignHost)
		}
		if removed {
			continue
		}
		return nil, fmt.Errorf("main.go: acquireProcessingLock: lock active: %s", filepath.Base(lockPath))
	}
	return nil, fmt.Errorf("main.go: acquireProcessingLock: could not acquire lock: %s", filepath.Base(lockPath))
}

// ----------------------------------------------------------------------------
// Argument parsing
// ----------------------------------------------------------------------------

func (cfg *AppConfig) parseArgs(args []string) []string {
	var rest []string
	explicitBitrate := false
	for _, arg := range args {
		if strings.EqualFold(arg, "-shutdown") {
			cfg.autoShutdown = true
			pInfo.Println("Auto-shutdown after completion enabled.")
			continue
		}
		if strings.EqualFold(arg, "-orig") || strings.EqualFold(arg, "-original") {
			cfg.keepOriginal = true
			pInfo.Println("Original resolution mode enabled: no downscaling.")
			continue
		}
		if strings.EqualFold(arg, "-copyaudio") || strings.EqualFold(arg, "-ca") {
			cfg.copyAudio = true
			pInfo.Println("Audio copy mode enabled: streams copied 1:1 (no AAC re-encode).")
			continue
		}
		if strings.EqualFold(arg, "-av1") {
			cfg.av1 = true
			pInfo.Println("AV1 mode enabled: encoding with av1_nvenc instead of H.265.")
			continue
		}
		if strings.EqualFold(arg, "-keep") {
			cfg.keepSource = true
			pInfo.Println("Keep-source mode enabled: originals are NOT moved to the recycle bin.")
			continue
		}
		if len(arg) > 1 && arg[0] == '-' {
			if _, errStat := os.Stat(arg); os.IsNotExist(errStat) {
				if n, err := strconv.ParseInt(arg[1:], 10, 64); err == nil && n > 0 {
					cfg.maxBitrateKbps = n
					explicitBitrate = true
					pInfo.Printf("Max bitrate set manually: %sk\n",
						pterm.LightCyan(fmt.Sprintf("%d", cfg.maxBitrateKbps)))
					continue
				}
				// Looks like an option but matches nothing known and no file on
				// disk: warn instead of silently ignoring (typos like -shutdwon).
				if strings.EqualFold(arg, "-davinci") || strings.EqualFold(arg, "-streams") ||
					strings.EqualFold(arg, "-split") || strings.EqualFold(arg, "-join") {
					pWarn.Printf("'%s' must be the FIRST argument — ignored here.\n", strings.ToLower(arg))
					pWarn.Printf("Example: NVENCForge.exe %s file.mkv\n", strings.ToLower(arg))
				} else {
					pWarn.Printf("Unknown option %q ignored. Available: -NNNN, -orig/-original, -copyaudio/-ca, -av1, -keep, -shutdown, -davinci, -split, -join\n", arg)
				}
				continue
			}
		}
		rest = append(rest, arg)
	}
	// AV1 reaches H.265 quality at ~25-30% less bitrate, so the AV1 mode has
	// its own (lower) caps. An explicit -NNNN always wins.
	if !explicitBitrate {
		switch {
		case cfg.av1 && cfg.keepOriginal:
			cfg.maxBitrateKbps = appSettings.av1MaxBitrateOriginal
			pInfo.Printf("Max bitrate (AV1 Original mode): %sk\n",
				pterm.LightCyan(fmt.Sprintf("%d", cfg.maxBitrateKbps)))
		case cfg.av1:
			cfg.maxBitrateKbps = appSettings.av1MaxBitrate1080p
			pInfo.Printf("Max bitrate (AV1 mode): %sk\n",
				pterm.LightCyan(fmt.Sprintf("%d", cfg.maxBitrateKbps)))
		case cfg.keepOriginal:
			cfg.maxBitrateKbps = appSettings.maxBitrateOriginal
			pInfo.Printf("Max bitrate (Original mode): %sk\n",
				pterm.LightCyan(fmt.Sprintf("%d", appSettings.maxBitrateOriginal)))
		}
	}
	cfg.inputArgs = rest
	return rest
}

func collectInputFiles(cfg *AppConfig, args []string) []string {
	if len(args) > 0 {
		var out []string
		for _, a := range args {
			abs, err := filepath.Abs(a)
			if err != nil {
				continue
			}
			abs = getLongPathName(abs)
			info, err := os.Stat(abs)
			if err != nil {
				continue
			}
			if !info.IsDir() {
				if videoExtensions[strings.ToLower(filepath.Ext(abs))] {
					out = append(out, abs)
				}
				continue
			}
			_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if strings.EqualFold(d.Name(), "output") {
						return filepath.SkipDir
					}
					return nil
				}
				if videoExtensions[strings.ToLower(filepath.Ext(path))] {
					out = append(out, path)
				}
				return nil
			})
		}
		return out
	}

	workDir := getWorkDir(cfg)
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && videoExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(workDir, e.Name()))
		}
	}
	return out
}

func getWorkDir(cfg *AppConfig) string {
	if len(cfg.inputArgs) > 0 {
		if info, err := os.Stat(cfg.inputArgs[0]); err == nil && info.IsDir() {
			return cfg.inputArgs[0]
		}
		return filepath.Dir(cfg.inputArgs[0])
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}

// ----------------------------------------------------------------------------
// Utility helpers
// ----------------------------------------------------------------------------

func waitForEnter() {
	fmt.Print("\nPress Enter to exit...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func formatDuration(seconds float64) string {
	if seconds < 0 || seconds > 360000 {
		return "-:--"
	}
	t := int(seconds)
	h, m, s := t/3600, (t%3600)/60, t%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func getFileSizeMB(path string) float64 {
	i, e := os.Stat(path)
	if e != nil {
		return 0
	}
	return float64(i.Size()) / 1048576
}

// FIX LEAK-04: explicit f.Close() per iteration (no defer inside loop).
func writeErrorLog(cfg *AppConfig, results []ProcessResult) {
	groups := make(map[string][]string)
	for _, r := range results {
		if !r.Success && !r.Skipped && !r.IsPreview {
			ts := r.FailedAt
			if ts.IsZero() {
				ts = time.Now()
			}
			dir := filepath.Dir(r.InputFile)
			if dir == "" {
				dir = getWorkDir(cfg)
			}
			line := fmt.Sprintf("[%s] %s: %s",
				ts.Format("2006-01-02 15:04:05"),
				filepath.Base(r.InputFile), r.ErrMsg)
			groups[dir] = append(groups[dir], line)
		}
	}
	if len(groups) == 0 {
		return
	}
	header := fmt.Sprintf("=== %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
	for dir, lines := range groups {
		logPath := filepath.Join(dir, "error_report.txt")
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			pWarn.Printf("Could not write error log: %v\n", err)
			continue
		}
		if _, err := f.WriteString(header); err != nil {
			pWarn.Printf("Error log: writing header failed: %v\n", err)
			_ = f.Close()
			continue
		}
		if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
			pWarn.Printf("Error log: writing entries failed: %v\n", err)
		}
		_ = f.Close()
	}
}

// ----------------------------------------------------------------------------
// printActiveSettings
// ----------------------------------------------------------------------------

func printActiveSettings(cfg *AppConfig) {
	s := appSettings

	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("Active Settings  (NVENCForge_Config.ini)")
	fmt.Println()

	bitrate := s.maxBitrate1080p
	bitrateActive := false
	resValue := fmt.Sprintf("%d p", s.maxResolution)
	resActive := false
	autoShutdown := s.autoShutdown
	if cfg != nil {
		if cfg.maxBitrateKbps != s.maxBitrate1080p {
			bitrate = cfg.maxBitrateKbps
			bitrateActive = true
		}
		if cfg.keepOriginal {
			resValue = "original"
			resActive = true
		}
		autoShutdown = cfg.autoShutdown
	}

	shutdownVal := "off"
	shutdownColor := "gray"
	if autoShutdown {
		shutdownVal = "on"
		shutdownColor = "yellow"
	}

	// -copyaudio switches the whole audio pipeline to 1:1 copy; the AAC
	// bitrate settings shown above it are not applied then.
	audioMode := "AAC if needed"
	audioModeActive := false
	if cfg != nil && cfg.copyAudio {
		audioMode = "copy 1:1"
		audioModeActive = true
	}

	// -av1 switches encoder, CQ scale (av1TargetCQ) and bitrate caps; the
	// B-frame setting is not used by av1_nvenc.
	videoCodec := "H.265"
	codecActive := false
	cqVal := s.targetCQ
	bfVal := fmt.Sprintf("%d", s.bFrames)
	bfColor := "cyan"
	if cfg != nil && cfg.av1 {
		videoCodec = "AV1"
		codecActive = true
		cqVal = s.av1TargetCQ
		bfVal = "n/a (AV1)"
		bfColor = "gray"
	}

	type entry struct {
		label  string
		value  string
		color  string
		active bool
	}
	entries := []entry{
		{"Video codec", videoCodec, "cyan", codecActive},
		{"Target CQ", fmt.Sprintf("%d", cqVal), "cyan", codecActive},
		{"Max bitrate", fmt.Sprintf("%d k", bitrate), "cyan", bitrateActive},
		{"Resolution", resValue, "cyan", resActive},
		{"NVENC preset", s.nvencPreset, "cyan", false},
		{"NVENC lookahead", fmt.Sprintf("%d fr", s.nvencLookahead), "cyan", false},
		{"B-frames", bfVal, bfColor, false},
		{"CAS sharpening", fmt.Sprintf("%.2f", s.casStrength), "cyan", false},
		{"Audio/channel", fmt.Sprintf("%d k", s.audioKbpsPerChannel), "cyan", false},
		{"Audio fallback", fmt.Sprintf("%d k", s.fallbackAudioBitrate), "cyan", false},
		{"Audio mode", audioMode, "cyan", audioModeActive},
		{"Auto-shutdown", shutdownVal, shutdownColor, false},
	}

	colorize := func(val, color string) string {
		switch color {
		case "yellow":
			return pterm.LightYellow(val)
		case "gray":
			return pterm.Gray(val)
		default:
			return pterm.LightCyan(val)
		}
	}

	const cols = 3

	// Column widths derive from the actual cell contents (longest cell per
	// column + fixed gap), so the grid stays aligned for every flag
	// combination — "(active)" suffixes previously pushed columns sideways.
	valTexts := make([]string, len(entries))
	visLens := make([]int, len(entries))
	var colWidth [cols]int
	for i, e := range entries {
		valTexts[i] = e.value
		if e.active {
			valTexts[i] = e.value + " (active)"
		}
		visLens[i] = len(e.label) + 2 + len(valTexts[i])
		if c := i % cols; visLens[i] > colWidth[c] {
			colWidth[c] = visLens[i]
		}
	}
	for i, e := range entries {
		valColor := e.color
		if e.active {
			valColor = "yellow"
		}
		cell := fmt.Sprintf("%s: %s", pterm.LightWhite(e.label), colorize(valTexts[i], valColor))
		if i == len(entries)-1 || (i+1)%cols == 0 {
			fmt.Printf("  %s\n", cell)
		} else {
			fmt.Printf("  %s%*s", cell, colWidth[i%cols]-visLens[i]+3, "")
		}
	}

	fmt.Println()
	fmt.Println(pterm.Gray("  Hint: To change any of these parameters, please edit 'NVENCForge_Config.ini'."))
	fmt.Println()
}

// printStreamSettings shows only the settings the DaVinci Resolve workflow
// (AAC re-encode bitrates) instead of the full encoder panel.
func printStreamSettings() {
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("Active Settings  (NVENCForge_Config.ini)")
	fmt.Println()
	fmt.Printf("  %s: %s        %s: %s\n",
		pterm.LightWhite("Audio/channel"),
		pterm.LightCyan(fmt.Sprintf("%d k", appSettings.audioKbpsPerChannel)),
		pterm.LightWhite("Audio fallback"),
		pterm.LightCyan(fmt.Sprintf("%d k", appSettings.fallbackAudioBitrate)))
	fmt.Println()
	fmt.Println(pterm.Gray("  Hint: To change any of these parameters, please edit 'NVENCForge_Config.ini'."))
	fmt.Println()
}

func printSummary(ctx context.Context, cfg *AppConfig, results []ProcessResult, elapsed time.Duration) {
	ok, fail, skip, preview, noAudio := 0, 0, 0, 0, 0
	var saved float64
	for _, r := range results {
		if r.Skipped {
			skip++
		} else if r.IsPreview {
			preview++
		} else if r.Success {
			ok++
			saved += r.SavedMB
			if r.NoAudio {
				noAudio++
			}
		} else {
			fail++
		}
	}
	if fail > 0 {
		writeErrorLog(cfg, results)
	}

	abortNote := ""
	if ctx.Err() != nil {
		abortNote = pterm.LightRed("  (aborted)")
	}

	fmt.Println()
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("Summary")
	fmt.Println()

	line := func(label, value, color string) {
		styled := value
		switch color {
		case "green":
			styled = pterm.LightGreen(value)
		case "yellow":
			styled = pterm.LightYellow(value)
		case "red":
			styled = pterm.LightRed(value)
		case "cyan":
			styled = pterm.LightCyan(value)
		case "gray":
			styled = pterm.Gray(value)
		}
		fmt.Printf("  %-18s %s\n", pterm.LightWhite(label), styled)
	}

	okColor := "green"
	if ok == 0 {
		okColor = "gray"
	}
	failColor := "gray"
	if fail > 0 {
		failColor = "red"
	}

	line("Successful:", fmt.Sprintf("%d", ok), okColor)
	if noAudio > 0 {
		line("Without audio:", fmt.Sprintf("%d  (video-only fallback, original kept)", noAudio), "yellow")
	}
	if preview > 0 {
		line("Preview:", fmt.Sprintf("%d  (aborted)", preview), "yellow")
	}
	line("Failed:", fmt.Sprintf("%d", fail), failColor)
	fmt.Printf("  %-18s %s%s\n",
		pterm.LightWhite("Skipped:"),
		pterm.Gray(fmt.Sprintf("%d", skip)),
		abortNote)
	if saved > 0 {
		line("Saved:", fmt.Sprintf("%.0f MB", saved), "cyan")
	}
	if fail > 0 {
		fmt.Println()
		pWarn.Println("Errors in: " + pterm.LightYellow("error_report.txt"))
	}
	fmt.Println()
	line("Elapsed time:", formatDuration(elapsed.Seconds()), "cyan")
	fmt.Println()
}

// ----------------------------------------------------------------------------
// main
// ----------------------------------------------------------------------------

func main() {
	// Crash-catcher: keeps the window open and shows a clean message if a panic
	// propagates. MUST be the first defer.
	defer func() {
		if r := recover(); r != nil {
			fmt.Println()
			pErr.Printf("Unexpected error (crash): %v\n", r)
			pErr.Println("Please contact support with a photo/screenshot of this message.")
			waitForEnter()
			os.Exit(1)
		}
	}()

	batchStart := time.Now()

	// Hidden developer switch: without -debug, suppress all error output so end
	// users never see internal failure reasons. Must run before any pErr use.
	debugMode = consumeDebugFlag()
	// -davinci is the DaVinci Resolve workflow mode. "-streams" is kept as a
	// silent backward-compatible alias so older "Send to" shortcuts keep working.
	davinciMode = len(os.Args) > 1 &&
		(strings.EqualFold(os.Args[1], "-davinci") || strings.EqualFold(os.Args[1], "-streams"))
	splitMode = len(os.Args) > 1 && strings.EqualFold(os.Args[1], "-split")
	joinMode = len(os.Args) > 1 && strings.EqualFold(os.Args[1], "-join")
	if !debugMode {
		pErr = pErr.WithWriter(io.Discard)
	}

	// Self-extract the embedded build sources into ./sourcecode (only if absent)
	// and lay down the user help file. Both are non-fatal best-effort steps.
	_ = extractEmbeddedSource()
	_ = writeHelpFileIfMissing()

	ctx, cancel := setupSignalContext()
	defer cancel()
	setupConsoleCtrlHandler(cancel)
	enableAnsiConsole()

	fmt.Println()
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgBlue)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("NVENCForge v" + appVersion + " — H265 NVENC Converter")
	fmt.Println()

	tipps := pterm.LightYellow("• NVENCForge.exe -10000 video.mp4 ") +
		pterm.Gray("   >>  ") + "Set max bitrate to 10000k\n" +
		pterm.LightYellow("• NVENCForge.exe -original <files>") +
		pterm.Gray("   >>  ") + "Keep original resolution (no downscale)\n" +
		pterm.LightYellow("• NVENCForge.exe -copyaudio <files>") +
		pterm.Gray("  >>  ") + "Copy audio 1:1 (no AAC re-encode)\n" +
		pterm.LightYellow("• NVENCForge.exe -av1 <files>      ") +
		pterm.Gray("  >>  ") + "Encode AV1 instead of H.265 (RTX 40+)\n" +
		pterm.LightYellow("• NVENCForge.exe -keep <files>    ") +
		pterm.Gray("   >>  ") + "Keep originals (don't move to recycle bin)\n" +
		pterm.LightYellow("• NVENCForge.exe -shutdown        ") +
		pterm.Gray("   >>  ") + "Shut down PC when finished\n" +
		pterm.LightYellow("• NVENCForge.exe -davinci <files> ") +
		pterm.Gray("   >>  ") + "DaVinci Resolve workflow (Audio/Subs/Split/Merge)\n" +
		pterm.LightYellow("• NVENCForge.exe -split <files>   ") +
		pterm.Gray("   >>  ") + "Lossless split (1:1, no re-encode)\n" +
		pterm.LightYellow("• NVENCForge.exe -join <files>    ") +
		pterm.Gray("   >>  ") + "Lossless join back into one MKV\n\n" +
		pterm.Gray("  (tips can be combined: -original -copyaudio -shutdown)")

	pterm.DefaultBox.
		WithTitle(pterm.LightCyan("  Quick-Start Tips  ")).
		WithTitleTopCenter().
		WithBoxStyle(pterm.NewStyle(pterm.FgGray)).
		Println(tipps)
	fmt.Println()

	if err := initTools(); err != nil {
		fmt.Println()
		pFatal.Println("FFmpeg/FFprobe setup failed.")
		if debugMode {
			pterm.Println(pterm.Gray("  Detail: " + err.Error()))
		}
		pterm.Println(pterm.Gray("  Place ffmpeg.exe and ffprobe.exe in the same folder as NVENCForge.exe,"))
		pterm.Println(pterm.Gray("  or ensure an internet connection for the auto-download to succeed."))
		waitForEnter()
		os.Exit(1)
	}

	// Config first: the GPU probe honors the configured B-frame count.
	loadOrCreateAppConfig()
	srtCleanerPhrases()

	// DaVinci Resolve workflow: pure remux/AAC work, no NVENC involved — the GPU
	// probe is skipped (faster start; it even works without an Nvidia card).
	if davinciMode {
		printStreamSettings()
		runDavinciMode(ctx, os.Args[2:])
		waitForEnter()
		return
	}

	// Lossless split/join: -split / -join copy every stream 1:1. No NVENC, no GPU
	// probe, works without an Nvidia card.
	if splitMode {
		runSplitMode(ctx, os.Args[2:])
		waitForEnter()
		return
	}
	if joinMode {
		runJoinMode(ctx, os.Args[2:])
		waitForEnter()
		return
	}

	// parseArgs runs before the GPU probe so the AV1 flag can steer it.
	cfg := &AppConfig{
		maxBitrateKbps: appSettings.maxBitrate1080p,
		autoShutdown:   appSettings.autoShutdown,
	}
	if cfg.autoShutdown {
		pInfo.Println("Auto-shutdown enabled via configuration.")
	}
	cfg.parseArgs(os.Args[1:])

	if err := checkHardwareCapabilities(); err != nil {
		fmt.Println()
		pFatal.Println("No compatible Nvidia GPU found (NVENC unavailable).")
		pFatal.Println("NVENCForge requires an Nvidia graphics card.")
		if debugMode {
			pterm.Println(pterm.Gray("  Detail: " + err.Error()))
		}
		waitForEnter()
		os.Exit(1)
	}
	if cfg.av1 {
		if err := checkAV1Capability(); err != nil {
			fmt.Println()
			pFatal.Println("AV1 encoding not available on this GPU (requires RTX 40 series or newer).")
			pFatal.Println("Run without -av1 to encode H.265 instead.")
			if debugMode {
				pterm.Println(pterm.Gray("  Detail: " + err.Error()))
			}
			waitForEnter()
			os.Exit(1)
		}
	}
	printActiveSettings(cfg)
	files := collectInputFiles(cfg, cfg.inputArgs)
	if len(files) == 0 {
		pInfo.Println("No video files found.")
		waitForEnter()
		return
	}

	dateiStr := "files"
	if len(files) == 1 {
		dateiStr = "file"
	}
	pInfo.Printf("Processing %s %s...\n",
		pterm.LightCyan(fmt.Sprintf("%d", len(files))), dateiStr)

	results := make([]ProcessResult, 0, len(files))
	for i, f := range files {
		if ctx.Err() != nil {
			results = append(results, ProcessResult{InputFile: f, Skipped: true})
			continue
		}
		results = append(results, processFile(ctx, cfg, f, i+1, len(files)))
	}

	printSummary(ctx, cfg, results, time.Since(batchStart))

	if cfg.autoShutdown && ctx.Err() == nil {
		fmt.Println()
		pterm.Warning.WithPrefix(pterm.Prefix{
			Text:  " SHUTDOWN ",
			Style: pterm.NewStyle(pterm.BgLightRed, pterm.FgBlack, pterm.Bold),
		}).Println("The PC will shut down in 30 seconds...")
		fmt.Println(pterm.Gray("Tip: 'shutdown /a' cancels the shutdown."))

		if err := exec.Command("shutdown", "/s", "/t", "30").Run(); err != nil {
			pErr.Printf("Could not schedule shutdown: %v\n", err)
			waitForEnter()
			return
		}
		time.Sleep(5 * time.Second)
		return
	}

	waitForEnter()
}

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
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/pterm/pterm"
)

// appVersion is shown in the startup header so the running build is obvious.
// Keep it in sync with the git tag / GitHub release on every release.
const appVersion = "1.23.0"

// ----------------------------------------------------------------------------
// Package-level sentinels and tool paths (set once in initTools, read-only after)
// ----------------------------------------------------------------------------

var (
	ffmpegPath  string
	ffprobePath string
	// ffmpegSource hält fest, WOHER die benutzten FFmpeg-Programme stammen, und
	// wird in der Einstellungs-Box mit angezeigt. Ohne diese Angabe ist von
	// außen nicht erkennbar, ob gerade der geprüfte Build oder ein fremder aus
	// dem Suchpfad rechnet — ein Unterschied, der die Qualitätsmessung betrifft.
	ffmpegSource string
)

const (
	ffmpegSourceLocal = "own copy"
	ffmpegSourcePath  = "from PATH"
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

// partSuffix marks the still-growing output: every encode and remux writes to
// "<name><codec>.part.mkv" and is renamed to the final name only after
// validateOutput passed. A clean Ctrl+C turns the part file into ".preview.mkv",
// so anything still carrying this marker survived a HARD kill (power loss, task
// manager, crash) and is a torn fragment — never a source file.
const partSuffix = ".part"

// isInterruptedFragment reports whether a collected file is one of our own torn
// part files. Without this check the fragment would pass as an ordinary .mkv
// input and be re-encoded in full: pure wasted runtime plus a nonsense output
// named "<name>.h265.part.h265.mkv".
func isInterruptedFragment(path string) bool {
	if !strings.EqualFold(filepath.Ext(path), ".mkv") {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return strings.HasSuffix(strings.ToLower(base), partSuffix)
}

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
	AvgFrameRate   string             `json:"avg_frame_rate"`
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
	FPSNote        string // Hinweis zur Bildrate (leer = unauffällig), siehe pickFrameRate
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
	InputMB    float64 // Größe der Quelldatei, Bezugswert für die Ersparnis in Prozent
	SavedMB    float64
	Success    bool
	Skipped    bool
	IsPreview  bool
	PreviewPct float64 // bei IsPreview: wie weit der Encode gekommen war (0-100)
	NoAudio    bool    // video-only fallback used: output has no sound, original kept
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
	// errors the user must see (missing GPU, FFmpeg setup). Kept as its own name
	// because the call sites read differently: pFatal ends the run, pErr does not.
	pFatal = pterm.Error.WithPrefix(pterm.Prefix{
		Text:  " ERROR ",
		Style: pterm.NewStyle(pterm.BgRed, pterm.FgWhite, pterm.Bold),
	})
	// pDetail carries the internal reason BEHIND an error (FFmpeg exit codes,
	// probe output, Go error chains) and is the only printer -debug silences.
	// Everything a user can act on — "conversion failed", "please drop only one
	// video file" — belongs on pErr and must stay visible: a failure that shows
	// nothing at all is indistinguishable from a file that was silently skipped.
	pDetail = pterm.Error.WithPrefix(pterm.Prefix{
		Text:  " DETAIL ",
		Style: pterm.NewStyle(pterm.BgDarkGray, pterm.FgLightWhite),
	})
)

// ----------------------------------------------------------------------------
// Debug switch (hidden, developer-only)
// ----------------------------------------------------------------------------

// debugMode is set once at the start of main() and read-only afterwards. When
// false, all pDetail output is routed to io.Discard so end users never see
// internal failure reasons. Intentionally undocumented (absent from help and
// tips).
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

// progressAreaActive meldet, dass gerade eine mehrzeilige Fortschrittsanzeige
// gezeichnet wird. Der Abbruch-Handler läuft in einer eigenen Goroutine, deshalb
// atomar.
var progressAreaActive atomic.Bool

// abortNoticePending merkt sich, dass die Abbruch-Meldung noch aussteht, weil
// sie beim Drücken von Strg+C die laufende Anzeige zerschossen hätte.
var abortNoticePending atomic.Bool

// printAbortNotice gibt die Meldung zum ersten Strg+C aus. Der Wortlaut hängt
// vom Modus ab: die Werkzeug-Modi werfen unfertige Dateien weg, die
// Konvertierung rettet den bisherigen Teil als Vorschau.
func printAbortNotice() {
	if davinciMode || splitMode || joinMode {
		pAbort.Println("Ctrl+C detected. Aborting — unfinished files will be removed...")
		return
	}
	pAbort.Println("Ctrl+C detected. Finishing current task cleanly (partial file will be kept)...")
}

// flushAbortNotice holt eine zurückgestellte Abbruch-Meldung nach. Aufzurufen,
// sobald die Fortschrittsanzeige geschlossen ist.
func flushAbortNotice() {
	if abortNoticePending.Swap(false) {
		printAbortNotice()
	}
}

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
		// Solange die mehrzeilige Fortschrittsanzeige läuft, darf hier NICHTS
		// gedruckt werden: sie zeichnet sich, indem sie den Cursor um die Höhe
		// ihres letzten Inhalts zurücksetzt, und jede Fremdausgabe verschiebt
		// diese Rechnung. Genau daran blieb nach Strg+C ein halber Balkenblock
		// stehen — und diese Meldung selbst wurde vom nächsten Neuzeichnen
		// wieder überschrieben. runFFmpeg holt sie nach, sobald die Anzeige zu ist.
		if progressAreaActive.Load() {
			abortNoticePending.Store(true)
		} else {
			fmt.Println()
			printAbortNotice()
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

	// Nur direkt neben der exe suchen — das ist die Kopie, die NVENCForge
	// mitbringt oder selbst geladen hat, und die einzige, deren Fähigkeiten
	// bekannt sind.
	resolveLocal := func(name string) (string, bool) {
		local := filepath.Join(exeDir, name)
		if info, statErr := os.Stat(local); statErr == nil && !info.IsDir() {
			return local, true
		}
		return "", false
	}

	if fp, okF := resolveLocal("ffmpeg.exe"); okF {
		if pp, okP := resolveLocal("ffprobe.exe"); okP {
			ffmpegPath, ffprobePath, ffmpegSource = fp, pp, ffmpegSourceLocal
			return nil
		}
	}

	// Kein eigenes FFmpeg da: den geprüften Build holen, statt einfach das
	// nächstbeste aus dem Suchpfad zu nehmen. Bis 1.9.0 gewann der Suchpfad
	// vor dem Download — dann lief NVENCForge mit einem beliebigen fremden
	// Build, und da sämtliche CQ- und VMAF-Werte des Programms an einem
	// bekannten Encoder ausgemessen wurden, sind sie mit einem fremden nicht
	// mehr belastbar.
	pInfo.Println("No FFmpeg next to NVENCForge.exe — downloading the tested build...")
	dlErr := downloadFFmpeg(exeDir)
	if dlErr == nil {
		fp, okF := resolveLocal("ffmpeg.exe")
		pp, okP := resolveLocal("ffprobe.exe")
		if okF && okP {
			ffmpegPath, ffprobePath, ffmpegSource = fp, pp, ffmpegSourceLocal
			return nil
		}
		dlErr = errors.New("ffmpeg.exe / ffprobe.exe still missing after download")
	}

	// Notnagel: ohne Internet (oder wenn GitHub gerade nicht erreichbar ist)
	// ist ein fremdes FFmpeg immer noch besser als ein Programm, das gar nicht
	// startet. Der Nutzer erfährt aber ausdrücklich, was gerade passiert.
	fp, errF := exec.LookPath("ffmpeg.exe")
	pp, errP := exec.LookPath("ffprobe.exe")
	if errF == nil && errP == nil {
		pWarn.Printf("Download failed: %s\n", plainError(dlErr))
		pWarn.Println("Using the FFmpeg found in your PATH instead. It works, but its build is")
		pWarn.Println("unknown — quality measurements can differ from the tested one. To fix")
		pWarn.Println("this, put ffmpeg.exe and ffprobe.exe next to NVENCForge.exe.")
		ffmpegPath, ffprobePath, ffmpegSource = fp, pp, ffmpegSourcePath
		return nil
	}

	return fmt.Errorf("main.go: initTools: auto-download failed: %w", dlErr)
}

// ----------------------------------------------------------------------------
// Hardware check: NVENC HEVC 10-bit dummy encode + CAS filter probe
// ----------------------------------------------------------------------------

// nvencAdvancedAQ is true while the GPU supports Temporal AQ + multipass (Turing
// / RTX 20 series or newer). checkHardwareCapabilities clears it for older cards
// (Pascal/Volta) so the real encode drops -temporal-aq/-multipass instead of
// failing on every single file.
var nvencAdvancedAQ = true

// nvencBFrameRefMode is true while the GPU accepts -b_ref_mode (using B-frames
// as reference frames). This is a capability of its own: a card can encode
// B-frames and still reject it, so it is probed separately instead of riding
// along with bFrames — otherwise one missing feature would cost us both.
var nvencBFrameRefMode = true

// cpuFallbackPromptTimeout ist die Bedenkzeit bei der Rückfrage "ohne
// Nvidia-Karte auf dem Prozessor weitermachen?". Läuft sie ab, wird
// weitergemacht — ein unbeaufsichtigter Stapellauf soll nicht an einer
// Frage hängen bleiben, vor der niemand sitzt.
const cpuFallbackPromptTimeout = 15 * time.Second

// nvencProbe is one trial encode's worth of settings. It exists so the probe
// calls stay readable — the alternative is four loose arguments at every call
// site, where nobody can tell "true, false" apart.
type nvencProbe struct {
	bFrames    int
	advancedAQ bool
	bRefMode   bool
	lookahead  int
}

// checkHardwareCapabilities probes with the SAME flags the real encode uses, so a
// card that passes here cannot fail later on every file.
//
// The normal case costs exactly ONE probe: if the configured settings run, we are
// done and nothing below executes. Only a rejection sends us into
// probeNVENCFeatures, which asks the card what it really supports.
//
// Deliberately measured instead of read off a GPU model list: a list would be
// guesswork for every card that cannot be tested here, and it would age with
// each new generation. The card itself always knows the truth.
func checkHardwareCapabilities() error {
	pInfo.Println("Checking GPU capabilities (NVENC HEVC 10-bit)...")

	tryEncode := func(p nvencProbe) (string, error) {
		args := []string{
			"-v", "error", "-f", "lavfi",
			"-i", "color=c=black:s=1920x1080:d=1",
			"-c:v", "hevc_nvenc", "-profile:v", "main10", "-pix_fmt", "p010le",
			"-preset", appSettings.nvencPreset, "-tune", "hq",
			"-rc-lookahead", strconv.Itoa(p.lookahead),
			"-spatial-aq", "1",
		}
		if p.advancedAQ {
			args = append(args, "-multipass", "qres", "-temporal-aq", "1")
		}
		if p.bFrames > 0 {
			args = append(args, "-bf", strconv.Itoa(p.bFrames))
			if p.bRefMode {
				args = append(args, "-b_ref_mode", "2")
			}
		}
		args = append(args, "-f", "null", "-")
		dummy := exec.Command(ffmpegPath, args...)
		dummy.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
		out, err := dummy.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	configured := nvencProbe{
		bFrames:    appSettings.bFrames,
		advancedAQ: true,
		bRefMode:   appSettings.bFrames > 0,
		lookahead:  appSettings.nvencLookahead,
	}
	if _, err := tryEncode(configured); err != nil {
		if err := probeNVENCFeatures(tryEncode); err != nil {
			return err
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

	// Antwort auf die eigene Frage: bisher stand hier nur "Checking GPU
	// capabilities..." und danach nie ein Ergebnis. Welche Karte rechnet, ist
	// außerdem die erste Rückfrage, wenn ein Lauf ungewohnt langsam war.
	if name := nvidiaCardName(); name != "" {
		pOK.Printf("GPU ready: %s\n", pterm.LightCyan(name))
	} else {
		pOK.Println("GPU ready (NVENC HEVC 10-bit).")
	}
	return nil
}

// probeNVENCFeatures asks the card what it actually supports, one feature at a
// time. It runs ONLY after the configured settings were rejected, so a healthy
// card never pays for it.
//
// The old code jumped straight from "everything on" to "no B-frames, no Temporal
// AQ". That was too coarse: a card that merely allows FEWER B-frames lost both
// features for nothing — and B-frames are one of the biggest size savers there is.
//
// Order matters. Every step builds on what already worked, so no probe can fail
// for a reason that was ruled out earlier.
func probeNVENCFeatures(tryEncode func(nvencProbe) (string, error)) error {
	pWarn.Println("This GPU rejected the configured settings — probing what it supports...")

	// 1. Bare baseline: no B-frames, no advanced AQ. The lookahead window stays
	// in because it costs graphics memory, so a small card can fail on that
	// alone. Shrink it before declaring the card unusable.
	base := nvencProbe{lookahead: appSettings.nvencLookahead}
	out, err := tryEncode(base)
	if err != nil {
		for _, smaller := range []int{16, 8, 0} {
			if smaller >= appSettings.nvencLookahead {
				continue
			}
			base.lookahead = smaller
			if out, err = tryEncode(base); err == nil {
				pWarn.Printf("GPU could not handle a %d-frame lookahead — continuing with %d.\n",
					appSettings.nvencLookahead, smaller)
				pWarn.Printf("Set 'nvencLookahead=%d' in NVENCForge_Config.ini to make this permanent.\n", smaller)
				appSettings.nvencLookahead = smaller
				break
			}
		}
	}
	if err != nil {
		// Nothing left to degrade: Maxwell-1, no NVENC, or no 10-bit support.
		return fmt.Errorf("main.go: probeNVENCFeatures: NVENC dummy encode failed: %v | %s",
			err, out)
	}

	// 2. Temporal AQ + multipass (Turing / RTX 20 series and newer).
	withAQ := base
	withAQ.advancedAQ = true
	if _, aqErr := tryEncode(withAQ); aqErr == nil {
		nvencAdvancedAQ = true
	} else {
		nvencAdvancedAQ = false
		pWarn.Println("GPU does not support Temporal AQ / multipass (needs RTX 20 series or newer) — continuing without them.")
	}

	// 3. Highest B-frame count this card accepts, counting down from the INI
	// value. First success wins, so every card ends up with its own maximum
	// instead of falling all the way to zero.
	wanted := appSettings.bFrames
	supported := 0
	probe := base
	probe.advancedAQ = nvencAdvancedAQ
	for n := wanted; n >= 1; n-- {
		probe.bFrames = n
		if _, bfErr := tryEncode(probe); bfErr == nil {
			supported = n
			break
		}
	}
	appSettings.bFrames = supported

	// 4. B-frames as reference frames — only meaningful once B-frames work.
	nvencBFrameRefMode = false
	if supported > 0 {
		probe.bFrames = supported
		probe.bRefMode = true
		if _, refErr := tryEncode(probe); refErr == nil {
			nvencBFrameRefMode = true
		} else {
			pWarn.Println("GPU does not support B-frame reference mode — continuing without it.")
		}
	}

	// The B-frame verdict comes last so it is not buried between the probes.
	switch {
	case wanted == 0:
		// The user switched B-frames off themselves — nothing to report.
	case supported == 0:
		pWarn.Println("GPU does not support HEVC B-frames — continuing without B-frames.")
		pWarn.Println("Set 'bFrames=0' in NVENCForge_Config.ini to make this permanent.")
	case supported < wanted:
		pWarn.Printf("GPU supports at most %d B-frames, not %d — continuing with %d.\n",
			supported, wanted, supported)
		pWarn.Printf("Set 'bFrames=%d' in NVENCForge_Config.ini to make this permanent.\n", supported)
	}
	return nil
}

// nvidiaCardName fragt den Kartennamen beim Treiber ab. Rein kosmetisch: fehlt
// nvidia-smi oder antwortet es nicht rechtzeitig, bleibt die Angabe leer und
// der Lauf geht unverändert weiter. Das Zeitlimit ist bewusst knapp — für eine
// Namenszeile darf der Start nicht spürbar warten.
func nvidiaCardName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name", "--format=csv,noheader")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(name)
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

// checkCPUEncoderCapability is the CPU-mode counterpart of
// checkHardwareCapabilities: it probes the encoder the run will actually use,
// with the configured preset, so a missing library or a bad setting fails once
// here instead of on every single file. Slim FFmpeg builds without libx265 or
// libsvtav1 exist, so this is a real case, not a formality.
func checkCPUEncoderCapability(av1 bool) error {
	label, encoderName := "libx265", "H.265"
	if av1 {
		label, encoderName = "libsvtav1", "AV1"
	}
	pInfo.Printf("Checking CPU encoder (%s %s 10-bit)...\n", label, encoderName)

	args := []string{
		"-v", "error", "-f", "lavfi",
		"-i", "color=c=black:s=1920x1080:d=1",
	}
	// Dieselben Optionen wie im echten Lauf, nur ohne Bitraten-Deckel: was
	// hier läuft, läuft auch beim Encodieren.
	if av1 {
		args = append(args, buildSVTAV1OptsWithCQ(appSettings.cpuAV1TargetCRF, "20000k", "40000k", 120)...)
	} else {
		args = append(args, buildX265OptsWithCQ(appSettings.cpuTargetCRF, "20000k", "40000k", 120)...)
	}
	args = append(args, "-f", "null", "-")

	dummy := exec.Command(ffmpegPath, args...)
	dummy.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
	out, err := dummy.CombinedOutput()
	if err != nil {
		return fmt.Errorf("main.go: checkCPUEncoderCapability: %s dummy encode failed: %v | %s",
			label, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// askYesNoTimeout asks a yes/no question and falls back to defaultYes when the
// user does not answer within timeout. The timeout exists so unattended runs
// (Send-to on a batch of files, an overnight queue) keep going instead of
// waiting forever at a prompt nobody is sitting in front of.
func askYesNoTimeout(question string, timeout time.Duration, defaultYes bool) bool {
	suffix := "(y/n)"
	if defaultYes {
		suffix = "(Y/n)"
	}
	fmt.Printf("  %s %s, continuing in %.0f s: ", question, suffix, timeout.Seconds())

	answer := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			// Kein lesbares stdin (umgeleitet/geschlossen): wie Zeitablauf
			// behandeln, damit der Lauf nicht stumm hängen bleibt.
			return
		}
		answer <- strings.TrimSpace(strings.ToLower(line))
	}()

	select {
	case a := <-answer:
		switch a {
		case "y", "yes", "j", "ja":
			return true
		case "n", "no", "nein":
			return false
		default:
			return defaultYes // Enter oder Unsinn: Vorgabe gilt
		}
	case <-time.After(timeout):
		fmt.Println()
		return defaultYes
	}
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
	sawAutoCQFlag, sawNoAutoCQ := false, false
	sawCropFlag, sawNoCropFlag := false, false
	sawKeepFlag, sawNoKeepFlag := false, false
	sawCPUFlag := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.EqualFold(arg, "-shutdown") {
			cfg.autoShutdown = true
			pInfo.Println("Auto-shutdown after completion enabled.")
			continue
		}
		// Gegenstück zu -shutdown, für den Fall autoShutdown=true in der INI.
		// Ohne diesen Schalter könnte eine Oberfläche das Abschalten nicht
		// abwählen: sie kann nur Argumente MITGEBEN, keine wegnehmen.
		if strings.EqualFold(arg, "-noshutdown") {
			cfg.autoShutdown = false
			continue
		}
		if strings.EqualFold(arg, "-orig") || strings.EqualFold(arg, "-original") {
			cfg.keepOriginal = true
			pInfo.Println("Original resolution mode enabled: no downscaling.")
			continue
		}
		// Gegenstück zu -orig, für den Fall keepResolution=true in der
		// Konfigurationsdatei: verkleinern wie in maxResolution eingestellt.
		if strings.EqualFold(arg, "-downscale") {
			cfg.keepOriginal = false
			continue
		}
		if strings.EqualFold(arg, "-copyaudio") || strings.EqualFold(arg, "-ca") {
			cfg.copyAudio = true
			pInfo.Println("Audio copy mode enabled: streams copied 1:1 (no AAC re-encode).")
			continue
		}
		// Gegenstück zu -copyaudio, für den Fall audioMode=copy in der
		// Konfigurationsdatei.
		if strings.EqualFold(arg, "-aac") {
			cfg.copyAudio = false
			continue
		}
		if strings.EqualFold(arg, "-av1") {
			// Gemeldet wird erst nach der Schleife: welcher AV1-Encoder läuft,
			// hängt von -cpu ab, und das darf hinter -av1 stehen.
			cfg.av1 = true
			continue
		}
		// Gegenstück zu -av1: der spätere Schalter gewinnt. Gedacht für eine
		// Befehlszeile, in der -av1 schon steht — etwa eine "Senden an"-
		// Verknüpfung, die man für einen Lauf überstimmen will, ohne sie zu
		// ändern.
		//
		// Ausdrücklich KEIN Gegenstück zur Konfigurationsdatei: die hat gar
		// keinen Schlüssel, der AV1 einschaltet (encoder kennt nur "nvidia"
		// und "cpu"). Diese Notiz steht hier, weil die erste Fassung dieses
		// Schalters genau das behauptet hat.
		if strings.EqualFold(arg, "-h265") || strings.EqualFold(arg, "-hevc") {
			cfg.av1 = false
			continue
		}
		if strings.EqualFold(arg, "-cpu") {
			cfg.cpu = true
			sawCPUFlag = true
			continue
		}
		// Gegenstück zu -cpu, für den Fall encoder=cpu in der
		// Konfigurationsdatei: auf der Grafikkarte rechnen.
		if strings.EqualFold(arg, "-gpu") || strings.EqualFold(arg, "-nvidia") {
			cfg.cpu = false
			continue
		}
		// -apple ist der alte Name aus 1.4.0 und bleibt gültig: er steckt in
		// bestehenden Send-to-Verknüpfungen, die ein Wegfall stillschweigend
		// unbrauchbar machen würde.
		if strings.EqualFold(arg, "-mp4") || strings.EqualFold(arg, "-apple") {
			cfg.mp4Mode = true
			pInfo.Println("MP4 mode enabled: output as a widely playable MP4 (H.265/hvc1 + AAC + faststart).")
			continue
		}
		// Gegenstück zu -mp4, für den Fall container=mp4 in der
		// Konfigurationsdatei.
		if strings.EqualFold(arg, "-mkv") {
			cfg.mp4Mode = false
			continue
		}
		if strings.EqualFold(arg, "-8bit") {
			cfg.eightBit = true
			pInfo.Println("8-bit mode enabled: encoding in 8 bit instead of 10 bit (for older devices).")
			continue
		}
		// Gegenstück zu -8bit, für den Fall bitDepth=8 in der
		// Konfigurationsdatei.
		if strings.EqualFold(arg, "-10bit") {
			cfg.eightBit = false
			continue
		}
		if strings.EqualFold(arg, "-keep") {
			cfg.keepSource = true
			sawKeepFlag = true
			// Seit 1.8.0 wandern Originale in den Unterordner "originals", nicht
			// mehr in den Papierkorb — die Meldung sprach bis 1.9.0 noch vom
			// Papierkorb und beschrieb damit einen Zustand, den es nicht mehr gibt.
			pInfo.Println("Keep-source mode enabled: originals stay exactly where they are.")
			continue
		}
		// Gegenstück zu -keep, für den Fall keepSource=true in der
		// Konfigurationsdatei: Originale wieder wegräumen lassen.
		if strings.EqualFold(arg, "-nokeep") {
			cfg.keepSource = false
			sawNoKeepFlag = true
			continue
		}
		if strings.EqualFold(arg, "-autocq") {
			cfg.autoCQ = true
			sawAutoCQFlag = true
			continue
		}
		if strings.EqualFold(arg, "-noautocq") {
			cfg.autoCQ = false
			sawNoAutoCQ = true
			continue
		}
		// Auto-Crop: wie bei Auto-CQ gewinnt das zuletzt genannte Flag, deshalb
		// wird auch hier erst nach der Schleife gemeldet.
		if strings.EqualFold(arg, "-crop") {
			cfg.autoCrop = true
			sawCropFlag = true
			continue
		}
		if strings.EqualFold(arg, "-nocrop") {
			cfg.autoCrop = false
			cfg.cropCheckOnly = false
			sawNoCropFlag = true
			continue
		}
		// -cropcheck schaut nur nach und schreibt das Kontrollbild. Es setzt
		// die Erkennung mit an, weil ohne sie nichts zu zeigen wäre.
		if strings.EqualFold(arg, "-cropcheck") {
			cfg.cropCheckOnly = true
			cfg.autoCrop = true
			sawCropFlag = true
			continue
		}
		if strings.EqualFold(arg, "-cq") {
			// Two-token flag: the CQ value follows as its own argument. The
			// valid upper bound depends on the codec (H.265 1-51, AV1 1-63),
			// which is only known after the whole loop (-av1 may follow -cq),
			// so the range is checked there.
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					i++ // the number belongs to -cq, even when out of range
					if n >= 1 {
						cfg.forcedCQ = n
					} else {
						pWarn.Printf("-cq %d is not a valid CQ (must be positive) — ignored.\n", n)
					}
					continue
				}
			}
			pWarn.Println("-cq needs a CQ number (e.g. \"-cq 28\") — ignored.")
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
					// Pointing at -help beats reciting fourteen flags: the list
					// was too long to scan for the one that was meant, and it
					// dated silently every time an option was added.
					pWarn.Printf("Unknown option %q — ignored.\n", arg)
					pWarn.Println("Run \"NVENCForge.exe -help\" to see every available option.")
				}
				continue
			}
		}
		rest = append(rest, arg)
	}
	// -mp4 exists for maximum reach, and AV1 works against exactly that: the iOS
	// Photos app cannot decode it at all and many TVs and older phones can't
	// either. So the MP4 output is always H.265 (hvc1). -av1 is overridden here,
	// before the codec-dependent CQ range and bitrate caps below are resolved.
	if cfg.mp4Mode && cfg.av1 {
		cfg.av1 = false
		pWarn.Println("-mp4 forces H.265: AV1 does not play on iPhones and many TVs — AV1 is switched off for this run.")
	}
	// 8 Bit gilt für den ganzen Lauf. Gesetzt wird die Variable erst hier, weil
	// alle Options-Bauer sie lesen und die Flags in beliebiger Reihenfolge
	// stehen dürfen.
	eightBitActive = cfg.eightBit
	// Backend festlegen: -cpu schlägt die INI (encoder=cpu). Beides kann hinter
	// -av1 stehen, deshalb erst hier — und deshalb wird der tatsächlich
	// laufende Encoder auch erst jetzt gemeldet, genau einmal. Findet der
	// GPU-Test später keine Karte, schaltet main() zusätzlich auf CPU um.
	cpuModeActive = cfg.cpu
	origin := ""
	if cpuModeActive && !sawCPUFlag {
		origin = " (from configuration: encoder=cpu)"
	}
	switch {
	case cfg.av1 && cpuModeActive:
		pInfo.Printf("AV1 on the processor%s: encoding with libsvtav1 — no Nvidia card needed, but much slower than a GPU.\n", origin)
	case cfg.av1:
		pInfo.Println("AV1 mode enabled: encoding with av1_nvenc instead of H.265.")
	case cpuModeActive:
		pInfo.Printf("CPU mode%s: encoding H.265 with libx265 — no Nvidia card needed, but much slower than a GPU.\n", origin)
	}
	// -cq uses the active codec's CQ scale: H.265 1-51, AV1 1-63. The same
	// number means a different quality per codec, so the upper bound depends
	// on the -av1 flag, which is only fully known now (it may follow -cq).
	if cfg.forcedCQ > 0 {
		maxCQ, scaleLabel := 51, "H.265 scale 1-51"
		if cfg.av1 {
			maxCQ, scaleLabel = 63, "AV1 scale 1-63"
		}
		if cfg.forcedCQ > maxCQ {
			pWarn.Printf("-cq %d is out of range (%s) — ignored.\n", cfg.forcedCQ, scaleLabel)
			cfg.forcedCQ = 0
		}
	}
	// A manual -cq exists exactly for the runs where the automatic pick is
	// unwanted, so it wins over every Auto-CQ source (flag or config).
	if cfg.forcedCQ > 0 {
		cfg.autoCQ = false
		pInfo.Printf("Manual CQ %d forced for this run — Auto-CQ disabled.\n", cfg.forcedCQ)
	}
	// Auto-CQ can come from the -autocq flag or the config (autoCQ=true);
	// the last flag on the command line wins, so the status is only known —
	// and reported once — after the whole loop. It works for both codecs, each
	// on its own CQ-scale profile (H.265 anchors 26/30, AV1 anchors 24/32).
	autoCQCodec := "H.265"
	if cfg.av1 {
		autoCQCodec = "AV1"
	}
	switch {
	case cfg.autoCQ && sawAutoCQFlag:
		pInfo.Printf("Auto-CQ mode enabled: CQ per file via sampled VMAF measurement (%s).\n", autoCQCodec)
	case cfg.autoCQ:
		// Der Konfigurations-Fall bekommt KEINE eigene Zeile mehr: die
		// Einstellungs-Box direkt darunter sagt dasselbe ("Quality: measured per
		// file" samt Zielwert). Nur die per Flag erzwungene Abweichung vom
		// Konfigurierten ist eine Meldung wert.
	case sawNoAutoCQ && (sawAutoCQFlag || appSettings.autoCQ):
		pInfo.Println("Auto-CQ disabled for this run (-noautocq) — using the configured CQ.")
	}
	// Auto-Crop. Der Prüfmodus bekommt eine eigene, deutliche Zeile: er sieht
	// aus wie ein normaler Lauf, konvertiert aber absichtlich nichts — das
	// muss vor dem ersten Nichts-passiert-Moment gesagt sein.
	switch {
	case cfg.cropCheckOnly:
		pInfo.Println("Crop check only: writing a picture of the proposed cut — no file is converted.")
	case cfg.autoCrop && sawCropFlag:
		pInfo.Println("Auto-crop enabled: black bars are detected and cut off.")
	case cfg.autoCrop:
		pInfo.Println("Auto-crop enabled via configuration: black bars are detected and cut off.")
	case sawNoCropFlag && appSettings.autoCrop:
		pInfo.Println("Auto-crop disabled for this run (-nocrop) — black bars are kept.")
	}
	// Keep-source. Die Meldung im Argument-Durchlauf oben läuft nur, wenn
	// -keep wirklich genannt wurde. Kommt die Einstellung aus der
	// Konfigurationsdatei, muss sie hier gesagt werden: Dass nichts
	// weggeräumt wird, merkt man sonst erst, wenn die Platte voll ist.
	switch {
	case cfg.keepSource && !sawKeepFlag:
		pInfo.Println("Keep-source enabled via configuration: originals stay exactly where they are.")
	case sawNoKeepFlag && appSettings.keepSource:
		pInfo.Println("Keep-source disabled for this run (-nokeep) — originals are moved away as configured.")
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
					// Weder unsere Ausgabe noch die beiseitegelegten
					// Originale dürfen erneut eingesammelt werden.
					if strings.EqualFold(d.Name(), "output") ||
						strings.EqualFold(d.Name(), originalsFolderName) {
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
	// In -json mode there is nobody at a keyboard: a graphical front end starts
	// this process without a console, so waiting here would hang the run forever
	// (or until the front end pushes a fake keystroke, which is exactly the kind
	// of guesswork -json exists to remove).
	if jsonMode {
		return
	}
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

func fileSizeBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func getFileSizeMB(path string) float64 {
	return float64(fileSizeBytes(path)) / 1048576
}

// ----------------------------------------------------------------------------
// Batch progress: overall bar and remaining time for the WHOLE run
// ----------------------------------------------------------------------------

// batchETAMinShare is the fraction of the batch that must be done before a
// remaining time is shown. Below that the extrapolation is guesswork: the very
// first percent of the first file would predict the whole night.
const batchETAMinShare = 0.02

// batchTracker weighs the overall progress by INPUT BYTES instead of file
// count. A two-hour film and an eight-minute episode are one file each, but not
// one unit of work — counting files made the overall bar jump. Bytes are not a
// perfect measure of encoding effort either (a remux is far cheaper than a
// re-encode of the same size), but they are known up front for every file and
// far closer to the truth than the file count.
//
// Because the estimate divides the elapsed WALL CLOCK by the finished share,
// everything that happens between the encodes is included automatically — above
// all the Auto-CQ analysis, which takes minutes and produces no encoder
// progress at all.
type batchTracker struct {
	start      time.Time
	totalBytes int64 // sum over every input file of the run
	doneBytes  int64 // files completely dealt with
	curBytes   int64 // the file currently being worked on
}

var batch batchTracker

// share returns the finished fraction of the batch (0..1). filePct is the
// progress inside the current file in percent. It returns 0 when no sizes are
// known, which tells the caller to fall back to counting files.
func (b *batchTracker) share(filePct float64) float64 {
	if b.totalBytes <= 0 {
		return 0
	}
	if filePct < 0 {
		filePct = 0
	}
	if filePct > 100 {
		filePct = 100
	}
	done := float64(b.doneBytes) + float64(b.curBytes)*filePct/100
	share := done / float64(b.totalBytes)
	if share < 0 {
		return 0
	}
	if share > 1 {
		return 1
	}
	return share
}

// remaining estimates the seconds left for the whole batch. The second return
// value is false while the estimate would still be meaningless.
func (b *batchTracker) remaining(filePct float64) (float64, bool) {
	share := b.share(filePct)
	if share < batchETAMinShare {
		return 0, false
	}
	elapsed := time.Since(b.start).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return elapsed/share - elapsed, true
}

// totalInputBytes sums the sizes of all collected files. Unreadable entries
// count as 0 — they simply do not contribute to the weighting.
func totalInputBytes(files []string) int64 {
	var sum int64
	for _, f := range files {
		sum += fileSizeBytes(f)
	}
	return sum
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
			pWarn.Printf("Could not write error log: %s\n", plainError(err))
			continue
		}
		if _, err := f.WriteString(header); err != nil {
			pWarn.Printf("Error log: writing header failed: %s\n", plainError(err))
			_ = f.Close()
			continue
		}
		if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
			pWarn.Printf("Error log: writing entries failed: %s\n", plainError(err))
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
	resValue := fmt.Sprintf("max %dp", s.maxResolution)
	resActive := false
	autoShutdown := s.autoShutdown
	if cfg != nil {
		if cfg.maxBitrateKbps != s.maxBitrate1080p {
			bitrate = cfg.maxBitrateKbps
			bitrateActive = true
		}
		if cfg.keepOriginal {
			resValue = "original (no downscale)"
			resActive = true
		}
		autoShutdown = cfg.autoShutdown
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
	if cfg != nil && cfg.av1 {
		videoCodec = "AV1"
		codecActive = true
		cqVal = s.av1TargetCQ
		bfVal = "n/a (AV1)"
	}
	// -mp4 keeps H.265 but repackages the result as a widely playable MP4 (hvc1).
	if cfg != nil && cfg.mp4Mode {
		videoCodec = "H.265 (MP4)"
		codecActive = true
	}
	// 8 Bit ist die Ausnahme und muss deshalb sichtbar sein — sonst wundert man
	// sich später über Streifen in dunklen Verläufen.
	if cfg != nil && cfg.eightBit {
		videoCodec += ", 8-bit"
		codecActive = true
	}
	// -cpu swaps the encoder itself: same codec, but libx265/libsvtav1 on their
	// own CRF scales, so the fixed quality value comes from a different key.
	if cpuModeActive {
		videoCodec += " on CPU"
		codecActive = true
		cqVal = activeManualCQ(cfg != nil && cfg.av1)
	}

	// -autocq replaces the fixed CQ with a per-file VMAF search; showing the
	// static number would be misleading then. A manual -cq wins over both.
	// Auto-CQ and -cq now apply to AV1 too, so this display simply layers over
	// the AV1 branch above. The wording avoids "CQ" wherever it can: the number
	// means nothing to most users, while "measured per file" states exactly
	// what the tool is about to do. The numeric target moved to the detail line.
	qualityText := fmt.Sprintf("fixed CQ %d", cqVal)
	qualityColor := "cyan"
	switch {
	case cfg != nil && cfg.forcedCQ > 0:
		qualityText = fmt.Sprintf("fixed CQ %d (-cq)", cfg.forcedCQ)
		qualityColor = "yellow"
	case cfg != nil && cfg.autoCQ:
		qualityText = "measured per file"
		qualityColor = "yellow"
	}

	// Where the original ends up is the question users care about most ("where
	// did my file go?") and it was missing from this panel entirely until 1.9.0.
	// -keep beats the configured mode, so it is checked first.
	originalsText, originalsActive := "moved to \""+originalsFolderName+"\" folder", false
	switch {
	case cfg != nil && cfg.keepSource:
		originalsText, originalsActive = "left exactly where they are", true
	case s.retireMode == retireModeRecycleBin:
		originalsText = "moved to the recycle bin"
	}

	// Entpacken: zeigt an, ob die Grafikkarte mithilft und ab welcher Bitrate
	// sicherheitshalber wieder der Prozessor übernimmt. Im CPU-Modus gibt es
	// keine Grafikkarte im Spiel, dann ist die Zeile schlicht "CPU".
	decodeVal := "CPU"
	if s.gpuDecode && !cpuModeActive {
		decodeVal = fmt.Sprintf("GPU (< %d Mbit)", s.gpuDecodeMaxMbit)
	}
	casVal := fmt.Sprintf("%.2f", s.casStrength)
	if s.casStrength <= 0 {
		casVal = "off"
	}

	type entry struct {
		label  string
		value  string
		color  string
		active bool
	}

	// Zwei Ebenen statt eines gleichrangigen 13-Felder-Rasters: oben steht, was
	// mit DIESEM Video passiert, darunter gedämpft die Regler dahinter. Die
	// alte Tabelle stellte "NVENC lookahead" gleichberechtigt neben "Codec" —
	// technisch vollständig, aber für Einsteiger eine Wand aus Fachbegriffen.
	primary := []entry{
		{"Codec", videoCodec, "cyan", codecActive},
		{"Quality", qualityText, qualityColor, false},
		{"Resolution", resValue, "cyan", resActive},
		{"Audio", audioMode, "cyan", audioModeActive},
		{"Originals", originalsText, "cyan", originalsActive},
	}
	// Ein von Hand gesetzter Deckel (-NNNN) gehört nach oben: der Nutzer hat ihn
	// bewusst mitgegeben und will ihn bestätigt sehen. Der Standardwert bleibt
	// unten bei den Details stehen.
	if bitrateActive {
		primary = append(primary,
			entry{"Max bitrate", fmt.Sprintf("%d k", bitrate), "cyan", true})
	}
	if autoShutdown {
		// Nur wenn eingeschaltet, dafür dann prominent: dass sich der PC gleich
		// von selbst abschaltet, darf niemanden überraschen. Ein "off" in der
		// Liste wäre dagegen reines Rauschen.
		primary = append(primary,
			entry{"Auto-shutdown", "PC turns off when finished", "yellow", true})
	}

	// Die Regler dahinter — als ausgerichtete Tabelle statt als eine mit "·"
	// verkettete Fließzeile. Grund ist die Fehlersuche: man muss einen EINZELNEN
	// Wert wiederfinden können, und einen Fließtext liest man dafür Wort für
	// Wort ab. Die Zweiteilung bleibt bestehen — oben steht, was mit dem Video
	// passiert, hier unten, womit. Die Beschriftungen sind bewusst nah an den
	// INI-Schlüsseln gehalten ("AQ strength" → aqStrength), damit der Weg von
	// der Anzeige zur Stellschraube kurz ist.
	// Zwei thematische Spalten: links alles, was über die Bildqualität und damit
	// über die Dateigröße entscheidet, rechts der Weg durch das Programm. Die
	// Spalten werden NICHT zeilenweise durchgezählt, sonst stünden zufällige
	// Paare wie "Plateau tolerance | NVENC preset" nebeneinander.
	type detail struct{ label, value string }
	var quality, pipeline []detail
	addQuality := func(label, value string) { quality = append(quality, detail{label, value}) }
	addPipeline := func(label, value string) { pipeline = append(pipeline, detail{label, value}) }

	// Ein von Hand gesetzter Deckel steht schon oben in der Hauptliste — hier
	// noch einmal wäre er die einzige doppelte Angabe der ganzen Anzeige.
	if !bitrateActive {
		addQuality("Max bitrate", fmt.Sprintf("%d k", bitrate))
	}
	if cfg != nil && cfg.autoCQ {
		target := fmt.Sprintf("VMAF %.4g", s.autoCQTargetVMAF)
		if s.autoCQTolerance > 0 {
			target = fmt.Sprintf("VMAF %.4g (accepts %.4g)",
				s.autoCQTargetVMAF, s.autoCQTargetVMAF-s.autoCQTolerance)
		}
		addQuality("Quality target", target)
		addQuality("Plateau tolerance", fmt.Sprintf("%.4g VMAF", s.autoCQPlateauTolerance))
	}

	// Die encoder-eigenen Regler unterscheiden sich je Backend: NVENC-Preset,
	// AQ-Stärke, Lookahead und B-Frames haben im CPU-Modus keinerlei Wirkung,
	// dort zählen stattdessen das x265-/SVT-Preset und die Thread-Grenze.
	if cpuModeActive {
		presetVal := s.cpuPreset + " (libx265)"
		if cfg != nil && cfg.av1 {
			presetVal = fmt.Sprintf("%d (SVT-AV1)", s.cpuAV1Preset)
		}
		threadsVal := "all cores"
		if s.cpuThreads > 0 {
			threadsVal = fmt.Sprintf("%d threads", s.cpuThreads)
		}
		addQuality("CPU preset", presetVal)
		addQuality("Threads", threadsVal)
	} else {
		addQuality("NVENC preset", s.nvencPreset)
		addQuality("AQ strength", fmt.Sprintf("%d", s.aqStrength))
		addQuality("Lookahead", fmt.Sprintf("%d frames", s.nvencLookahead))
		addQuality("B-frames", bfVal)
	}

	addPipeline("Decoding", decodeVal)
	addPipeline("Sharpening", casVal)
	addPipeline("Audio", fmt.Sprintf("%d k per channel", s.audioKbpsPerChannel))
	addPipeline("Audio fallback", fmt.Sprintf("%d k", s.fallbackAudioBitrate))
	if ffmpegSource != "" {
		addPipeline("FFmpeg", ffmpegSource)
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

	// Die sichtbare Länge muss getrennt mitgeführt werden: pterm packt die
	// Farben in ANSI-Steuerzeichen, und die zählt jede Breitenrechnung sonst
	// mit — genau daran verrutschte die alte Tabelle bei "(active)".
	renderCell := func(e entry) (text string, visibleLen int) {
		val := e.value
		color := e.color
		if e.active {
			val += " (active)"
			color = "yellow"
		}
		return pterm.LightWhite(e.label+": ") + colorize(val, color),
			len(e.label) + 2 + len(val)
	}

	// Spaltenbreite aus dem längsten Eintrag der linken Spalte statt fest
	// vorgegeben: Werte wie "Originals: left exactly where they are (active)"
	// sprengen jede feste Breite, und die rechte Spalte stünde dann je Zeile
	// woanders.
	const columnGap = 3
	columnWidth := 0
	for i := 0; i < len(primary); i += 2 {
		if _, width := renderCell(primary[i]); width > columnWidth {
			columnWidth = width
		}
	}
	for i := 0; i < len(primary); i += 2 {
		left, leftLen := renderCell(primary[i])
		if i+1 >= len(primary) {
			fmt.Printf("  %s\n", left)
			break
		}
		right, _ := renderCell(primary[i+1])
		fmt.Printf("  %s%*s%s\n", left, columnWidth-leftLen+columnGap, "", right)
	}

	// Spaltenbreiten aus den SICHTBAREN Texten rechnen und erst danach färben:
	// pterm packt die Farben in ANSI-Steuerzeichen, die jede Breitenrechnung
	// mitzählen würde. Genau daran verrutschte die frühere Tabelle.
	widest := func(rows []detail, pick func(detail) string) int {
		w := 0
		for _, r := range rows {
			if n := len(pick(r)); n > w {
				w = n
			}
		}
		return w
	}
	leftLabel := widest(quality, func(d detail) string { return d.label })
	leftValue := widest(quality, func(d detail) string { return d.value })
	rightLabel := widest(pipeline, func(d detail) string { return d.label })

	// Die rechte Spalte wird bewusst nicht aufgefüllt — sonst hingen unsichtbare
	// Leerzeichen am Zeilenende, die beim Kopieren des Fensters mitwandern.
	cell := func(d detail, labelW, valueW int) string {
		return pterm.Gray(fmt.Sprintf("%-*s", labelW, d.label)) + "  " +
			pterm.LightCyan(fmt.Sprintf("%-*s", valueW, d.value))
	}

	fmt.Println()
	fmt.Println("  " + pterm.Gray("Details"))
	for i := 0; i < len(quality) || i < len(pipeline); i++ {
		row := "    "
		switch {
		case i < len(quality) && i < len(pipeline):
			row += cell(quality[i], leftLabel, leftValue) + "    " +
				cell(pipeline[i], rightLabel, 0)
		case i < len(quality):
			row += cell(quality[i], leftLabel, 0)
		default:
			row += strings.Repeat(" ", leftLabel+2+leftValue+4) +
				cell(pipeline[i], rightLabel, 0)
		}
		fmt.Println(row)
	}

	fmt.Println()
	fmt.Println(pterm.Gray("  Every setting and what it does:  NVENCForge_Config.ini  ·  Options:  -help"))
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
	var saved, sourceTotal, previewPct float64
	for _, r := range results {
		if r.Skipped {
			skip++
		} else if r.IsPreview {
			preview++
			if r.PreviewPct > previewPct {
				previewPct = r.PreviewPct
			}
		} else if r.Success {
			ok++
			saved += r.SavedMB
			sourceTotal += r.InputMB
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

	fmt.Println()
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("Summary")
	fmt.Println()

	// Erst auf Breite bringen, DANN einfärben: pterm packt die Farbe in
	// ANSI-Steuerzeichen, die %-18s mitgezählt hätte. Dadurch stand hinter dem
	// kurzen "Failed:" mehr Luft als hinter "Successful:", und die Zahlen
	// standen nicht untereinander.
	const summaryLabelWidth = 14
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
		fmt.Printf("  %s %s\n",
			pterm.LightWhite(fmt.Sprintf("%-*s", summaryLabelWidth, label)), styled)
	}

	// Der Abbruch gehört als eigener Satz nach oben. Vorher hing ein "(aborted)"
	// hinter dem Skipped-Zähler — also ausgerechnet an einer 0, wenn gar nichts
	// übersprungen wurde. Und die Frage, die nach Strg+C wirklich offen ist,
	// lautet nicht "wie viele", sondern "sind meine Originale noch da?".
	if ctx.Err() != nil {
		pterm.NewStyle(pterm.FgLightYellow, pterm.Bold).
			Println("  Stopped by you (Ctrl+C) — no original was touched.")
		fmt.Println()
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
		note := fmt.Sprintf("%d  (partial file kept as .preview.mkv", preview)
		if previewPct > 0 {
			note += fmt.Sprintf(", %.0f %% done", previewPct)
		}
		line("Unfinished:", note+")", "yellow")
	}
	line("Failed:", fmt.Sprintf("%d", fail), failColor)
	line("Skipped:", fmt.Sprintf("%d", skip), "gray")
	// Die Ersparnis ist die Währung des Programms — deshalb steht sie auch dann
	// da, wenn nichts zusammenkam, statt einfach zu fehlen.
	switch {
	case saved > 0 && sourceTotal > 0:
		line("Saved:", fmt.Sprintf("%.0f MB  (%.0f %% of %.0f MB)",
			saved, saved/sourceTotal*100, sourceTotal), "cyan")
	case saved > 0:
		line("Saved:", fmt.Sprintf("%.0f MB", saved), "cyan")
	default:
		line("Saved:", "nothing yet", "gray")
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

	// -json splits the two channels: machine-readable events on stdout, the
	// entire human-facing display on stderr. Must run before ANY output, or the
	// first lines would already be in the wrong channel. Without the flag
	// nothing is redirected and nothing is emitted.
	jsonMode = consumeJSONFlag()
	if jsonMode {
		enableJSONMode()
	}

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
		pDetail = pDetail.WithWriter(io.Discard)
	}

	// Lay down the user help file next to the exe. Non-fatal best-effort step.
	_ = syncHelpFile()

	ctx, cancel := setupSignalContext()
	defer cancel()
	setupConsoleCtrlHandler(cancel)
	enableAnsiConsole()

	// -help is answered here and nowhere later: initTools() below would first
	// download ~80 MB of FFmpeg, and nobody asking for an option list expects a
	// download. The GPU probe is skipped for the same reason — the option list
	// is identical with or without a graphics card. syncHelpFile() has already
	// run above, so the manual this text points to really is on disk.
	if wantsHelp(os.Args[1:]) {
		printConsoleHelp()
		waitForEnter()
		return
	}

	fmt.Println()
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgBlue)).
		WithTextStyle(pterm.NewStyle(pterm.FgLightWhite, pterm.Bold)).
		Println("NVENCForge v" + appVersion + " — H265 NVENC Converter")
	fmt.Println()

	// The startup box answers exactly one question: "what do I have to do?".
	// For this tool the honest answer is "nothing at all", so that sentence
	// comes first and stands alone. Listing all fourteen options here (as every
	// build up to 1.8.0 did) made a program that needs no options whatsoever
	// look like it demands studying a manual first — the single most common
	// reason newcomers bounce off a command-line tool. The full list is one
	// keystroke away via -help.
	//
	// Sie erscheint aber nur noch, wenn wirklich jemand ratlos davorsitzt (siehe
	// droppedSomething weiter unten) — bei jedem Drag-and-drop-Lauf stand hier
	// vierzehn Zeilen lang die Anleitung für etwas, das der Anwender gerade
	// getan hat.
	quickStart := pterm.LightWhite("Just drag video files or a folder onto NVENCForge.exe.") + "\n" +
		pterm.Gray("Nothing else is needed — the best quality setting is measured") + "\n" +
		pterm.Gray("automatically, separately for every single file.") + "\n\n" +
		pterm.LightCyan("Most used options:") + "\n" +
		pterm.LightYellow("  -original   ") + "keep the resolution (no downscale to 1080p)\n" +
		pterm.LightYellow("  -copyaudio  ") + "leave the audio untouched\n" +
		pterm.LightYellow("  -av1        ") + "AV1 instead of H.265 — smaller, needs RTX 40+\n\n" +
		pterm.LightCyan("Other tools:  ") +
		pterm.LightYellow("-davinci   -split   -join") + "\n\n" +
		pterm.Gray("Every option explained:  ") + pterm.LightYellow("NVENCForge.exe -help")

	// Die Anleitung richtet sich an den, der die EXE einfach doppelklickt und
	// nicht weiß, was zu tun ist. Wer schon Dateien draufgezogen hat, weiß es —
	// dort kostet die Box nur vierzehn Zeilen und schiebt die eigentliche Arbeit
	// aus dem Bild. Geprüft wird auf ein Argument, das kein Schalter ist; die
	// genaue Auswertung übernimmt später parseArgs.
	droppedSomething := false
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-") {
			droppedSomething = true
			break
		}
	}
	if !droppedSomething {
		pterm.DefaultBox.
			WithTitle(pterm.LightCyan("  Quick Start  ")).
			WithTitleTopCenter().
			WithBoxStyle(pterm.NewStyle(pterm.FgGray)).
			Println(quickStart)
		fmt.Println()
	}

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

	// Die Werkzeug-Modi laufen nicht über die Stapel-Verwaltung, brauchen aber
	// denselben Startzeitpunkt für ihre Schlussübersicht.
	batch = batchTracker{start: batchStart}

	// DaVinci Resolve workflow: pure remux/AAC work, no NVENC involved — the GPU
	// probe is skipped (faster start; it even works without an Nvidia card).
	if davinciMode {
		emitToolRunStart()
		printStreamSettings()
		runDavinciMode(ctx, os.Args[2:])
		printToolSummary(ctx)
		waitForEnter()
		return
	}

	// Lossless split/join: -split / -join copy every stream 1:1. No NVENC, no GPU
	// probe, works without an Nvidia card.
	if splitMode {
		emitToolRunStart()
		runSplitMode(ctx, os.Args[2:])
		printToolSummary(ctx)
		waitForEnter()
		return
	}
	if joinMode {
		emitToolRunStart()
		runJoinMode(ctx, os.Args[2:])
		printToolSummary(ctx)
		waitForEnter()
		return
	}

	// parseArgs runs before the GPU probe so the AV1 flag can steer it.
	cfg := newAppConfig(appSettings)
	if cfg.autoShutdown {
		pInfo.Println("Auto-shutdown enabled via configuration.")
	}
	cfg.parseArgs(os.Args[1:])

	// GPU-Test nur, wenn auch auf der GPU encodiert werden soll. Schlägt er
	// fehl, ist das kein Abbruchgrund mehr: seit dem CPU-Modus kann derselbe
	// Auftrag ohne Nvidia-Karte laufen, nur langsamer. Deshalb wird gefragt
	// statt aufgegeben — mit Zeitlimit, damit unbeaufsichtigte Stapelläufe
	// (Send-to über Nacht) nicht ewig an der Rückfrage stehen bleiben.
	if !cpuModeActive {
		if err := checkHardwareCapabilities(); err != nil {
			fmt.Println()
			pWarn.Println("No compatible Nvidia GPU found (NVENC unavailable).")
			// Always show the underlying FFmpeg error: a bad ffmpeg build (e.g.
			// renamed encoder options) fails this probe too, and without the
			// detail that is indistinguishable from a genuinely missing GPU.
			pterm.Println(pterm.Gray("  Detail: " + err.Error()))
			fmt.Println()
			pInfo.Println("NVENCForge can encode on the processor instead (libx265 / libsvtav1).")
			pInfo.Println("That works on any machine, but takes considerably longer than a GPU.")
			if !askYesNoTimeout("Continue on the processor?", cpuFallbackPromptTimeout, true) {
				fmt.Println()
				pFatal.Println("Stopped — no Nvidia card and CPU mode declined.")
				pInfo.Println("Tip: encoder=cpu in NVENCForge_Config.ini makes CPU mode permanent, -cpu enables it per run.")
				waitForEnter()
				os.Exit(1)
			}
			cpuModeActive = true
			pOK.Println("Switched to CPU mode for this run.")
		}
	}
	switch {
	case cpuModeActive:
		// Im CPU-Modus wird der Encoder geprüft, der wirklich läuft — eine
		// fehlende Bibliothek soll einmal hier auffallen, nicht bei jeder Datei.
		if err := checkCPUEncoderCapability(cfg.av1); err != nil {
			fmt.Println()
			pFatal.Println("The CPU encoder is not available in this FFmpeg build.")
			pFatal.Println("Delete ffmpeg.exe next to NVENCForge.exe and restart to download a complete build.")
			pterm.Println(pterm.Gray("  Detail: " + err.Error()))
			waitForEnter()
			os.Exit(1)
		}
	case cfg.av1:
		if err := checkAV1Capability(); err != nil {
			fmt.Println()
			pFatal.Println("AV1 encoding not available on this GPU (requires RTX 40 series or newer).")
			pFatal.Println("Run without -av1 to encode H.265, or add -cpu to encode AV1 on the processor.")
			if debugMode {
				pterm.Println(pterm.Gray("  Detail: " + err.Error()))
			}
			waitForEnter()
			os.Exit(1)
		}
	}
	// -autocq needs the libvmaf filter; slim FFmpeg builds may lack it.
	// Checked once up front so the whole batch degrades with a single clear
	// warning instead of one failed analysis per file.
	if cfg.autoCQ {
		if err := checkLibVMAF(); err != nil {
			pWarn.Println("Auto-CQ disabled: this FFmpeg build has no libvmaf filter — using the fixed CQ/CRF from the config.")
			if debugMode {
				pterm.Println(pterm.Gray("  Detail: " + err.Error()))
			}
			cfg.autoCQ = false
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

	batch = batchTracker{start: batchStart, totalBytes: totalInputBytes(files)}

	emitRunStart(cfg, len(files))

	results := make([]ProcessResult, 0, len(files))
	for i, f := range files {
		if ctx.Err() != nil {
			results = append(results, ProcessResult{InputFile: f, Skipped: true})
			continue
		}
		batch.curBytes = fileSizeBytes(f)
		emitFileStart(i+1, len(files), f)
		res := processFile(ctx, cfg, f, i+1, len(files))
		if cfg.mp4Mode {
			// -mp4: repackage the finished MKV output into a compatible MP4.
			remuxResultToMP4(ctx, cfg, &res)
		}
		// Book the file as done AFTER the optional MP4 step, so its bytes stay
		// in curBytes while that repackaging still reports progress.
		batch.doneBytes += batch.curBytes
		batch.curBytes = 0
		emitFileResult(i+1, res)
		results = append(results, res)
	}

	printSummary(ctx, cfg, results, time.Since(batchStart))
	emitRunSummary(results, time.Since(batchStart))

	if cfg.autoShutdown && ctx.Err() == nil {
		fmt.Println()
		pterm.Warning.WithPrefix(pterm.Prefix{
			Text:  " SHUTDOWN ",
			Style: pterm.NewStyle(pterm.BgLightRed, pterm.FgBlack, pterm.Bold),
		}).Println("The PC will shut down in 30 seconds...")
		fmt.Println(pterm.Gray("Tip: 'shutdown /a' cancels the shutdown."))

		if err := exec.Command("shutdown", "/s", "/t", "30").Run(); err != nil {
			pErr.Printf("Could not schedule shutdown: %s\n", plainError(err))
			waitForEnter()
			return
		}
		time.Sleep(5 * time.Second)
		return
	}

	waitForEnter()
}

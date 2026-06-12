//go:build windows && amd64

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/pterm/pterm"
)

// ----------------------------------------------------------------------------
// AppConfig kapselt den veränderlichen Programmfluss-Zustand.
// Wird in main() einmalig befüllt und danach ausschließlich als Parameter
// durchgereicht — kein globaler Schreibzugriff.
// ----------------------------------------------------------------------------

type AppConfig struct {
	maxBitrateKbps int64    // Obergrenze der Ziel-Bitrate in kbps
	autoShutdown   bool     // PC nach Abschluss herunterfahren
	keepOriginal   bool     // -original (alias -orig): Originalauflösung behalten, Bitrate-Cap auf 18000k
	copyAudio      bool     // -copyaudio: Ton 1:1 kopieren (kein DaVinci-AAC-Re-Encode)
	av1            bool     // -av1: opt-in AV1-Encoding (av1_nvenc) statt H.265
	inputArgs      []string // verbleibende Nicht-Flag-Argumente (Dateien/Ordner)
}

// ----------------------------------------------------------------------------
// AppSettings hält die aus NVENCForge_Config.ini geladenen Encoder-Parameter.
// Wird in main() via loadOrCreateAppConfig() EINMAL befüllt, danach nur lesend.
// ----------------------------------------------------------------------------

type AppSettings struct {
	targetCQ             int
	maxBitrate1080p      int64
	maxBitrateOriginal   int64
	maxResolution        int
	nvencPreset          string
	nvencLookahead       int
	bFrames              int
	casStrength          float64
	audioKbpsPerChannel  int
	fallbackAudioBitrate int
	autoShutdown         bool
	extraFilenameChars   string
	av1TargetCQ          int
	av1MaxBitrate1080p   int64
	av1MaxBitrateOriginal int64
}

var appSettings = defaultAppSettings()

func defaultAppSettings() AppSettings {
	return AppSettings{
		targetCQ:             26,
		maxBitrate1080p:      8000,
		maxBitrateOriginal:   18000,
		maxResolution:        1080,
		nvencPreset:          "p5",
		nvencLookahead:       32,
		bFrames:              4,
		casStrength:          0.4,
		audioKbpsPerChannel:  96,
		fallbackAudioBitrate: 128,
		autoShutdown:         false,
		extraFilenameChars:   "",
		av1TargetCQ:          32,
		av1MaxBitrate1080p:   6000,
		av1MaxBitrateOriginal: 13000,
	}
}

// loadOrCreateAppConfig legt die INI bei Fehlen an. Ungültige Werte fallen
// einzeln auf ihren Default zurück (mit Warnung); die Datei selbst wird nie
// überschrieben, damit die übrigen Nutzer-Einstellungen erhalten bleiben.
func loadOrCreateAppConfig() {
	appSettings = defaultAppSettings()
	exePath, err := os.Executable()
	if err != nil {
		return // Defaults bleiben aktiv
	}
	path := filepath.Join(filepath.Dir(exePath), "NVENCForge_Config.ini")

	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if werr := writeDefaultAppConfig(path); werr == nil {
			fmt.Println(pterm.Gray("  · Configuration created: " + filepath.Base(path)))
		}
		return
	}

	parsed, warns := parseAppConfig(path)
	for _, w := range warns {
		pWarn.Println("Config: " + w)
	}
	appSettings = parsed
}

// parseAppConfig liest mit Bounds-Checking. Jeder ungültige Wert wird einzeln
// auf seinen Default zurückgesetzt und als Warnung gemeldet; gültige Werte
// bleiben erhalten. Unbekannte Keys werden ignoriert (vorwärtskompatibel).
func parseAppConfig(path string) (AppSettings, []string) {
	s := defaultAppSettings()
	var warns []string
	f, err := os.Open(path)
	if err != nil {
		return s, []string{"configuration not readable - using defaults"}
	}
	defer f.Close()

	validPresets := map[string]bool{
		"p1": true, "p2": true, "p3": true, "p4": true,
		"p5": true, "p6": true, "p7": true,
	}
	validRes := map[int]bool{720: true, 1080: true, 1440: true, 2160: true}

	bad := func(key, val string) {
		warns = append(warns, fmt.Sprintf("%s=%q invalid - default value kept", key, val))
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			warns = append(warns, fmt.Sprintf("line %q ignored (missing '=')", line))
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch key {
		case "targetCQ":
			if n, e := strconv.Atoi(val); e == nil && n >= 1 && n <= 51 {
				s.targetCQ = n
			} else {
				bad(key, val)
			}
		case "maxBitrate1080p":
			if n, e := strconv.Atoi(val); e == nil && n > 1000 {
				s.maxBitrate1080p = int64(n)
			} else {
				bad(key, val)
			}
		case "maxBitrateOriginal":
			if n, e := strconv.Atoi(val); e == nil && n > 1000 {
				s.maxBitrateOriginal = int64(n)
			} else {
				bad(key, val)
			}
		case "maxResolution":
			if n, e := strconv.Atoi(val); e == nil && validRes[n] {
				s.maxResolution = n
			} else {
				bad(key, val)
			}
		case "nvencPreset":
			if p := strings.ToLower(val); validPresets[p] {
				s.nvencPreset = p
			} else {
				bad(key, val)
			}
		case "nvencLookahead":
			if n, e := strconv.Atoi(val); e == nil && n >= 0 && n <= 32 {
				s.nvencLookahead = n
			} else {
				bad(key, val)
			}
		case "bFrames":
			if n, e := strconv.Atoi(val); e == nil && n >= 0 && n <= 4 {
				s.bFrames = n
			} else {
				bad(key, val)
			}
		case "casStrength":
			if fv, e := strconv.ParseFloat(val, 64); e == nil && fv >= 0.0 && fv <= 1.0 {
				s.casStrength = fv
			} else {
				bad(key, val)
			}
		case "audioKbpsPerChannel":
			if n, e := strconv.Atoi(val); e == nil && n > 32 {
				s.audioKbpsPerChannel = n
			} else {
				bad(key, val)
			}
		case "fallbackAudioBitrate":
			if n, e := strconv.Atoi(val); e == nil && n >= 128 && n <= 640 {
				s.fallbackAudioBitrate = n
			} else {
				bad(key, val)
			}
		case "autoShutdown":
			if b, e := strconv.ParseBool(val); e == nil {
				s.autoShutdown = b
			} else {
				bad(key, val)
			}
		case "av1TargetCQ":
			if n, e := strconv.Atoi(val); e == nil && n >= 1 && n <= 63 {
				s.av1TargetCQ = n
			} else {
				bad(key, val)
			}
		case "av1MaxBitrate1080p":
			if n, e := strconv.Atoi(val); e == nil && n > 1000 {
				s.av1MaxBitrate1080p = int64(n)
			} else {
				bad(key, val)
			}
		case "av1MaxBitrateOriginal":
			if n, e := strconv.Atoi(val); e == nil && n > 1000 {
				s.av1MaxBitrateOriginal = int64(n)
			} else {
				bad(key, val)
			}
		case "extraFilenameChars":
			// Windows-forbidden path characters and whitespace can never be
			// allowed; they are dropped individually with a warning.
			var kept []rune
			dropped := false
			for _, r := range val {
				if strings.ContainsRune(`\/:*?"<>|`, r) ||
					unicode.IsSpace(r) || unicode.IsControl(r) {
					dropped = true
					continue
				}
				kept = append(kept, r)
			}
			if dropped {
				warns = append(warns,
					"extraFilenameChars: characters not allowed in Windows file names were ignored")
			}
			s.extraFilenameChars = string(kept)
		default:
			// Unbekannter Schlüssel (z. B. defaultAudioLang aus älteren
			// Versionen): ignorieren (vorwärtskompatibel).
		}
	}
	if err := sc.Err(); err != nil {
		warns = append(warns, "configuration only partially readable: "+err.Error())
	}
	return s, warns
}

// writeDefaultAppConfig schreibt die komplette, kommentierte Standard-INI.
func writeDefaultAppConfig(path string) error {
	d := defaultAppSettings()
	content := fmt.Sprintf(`# NVENCForge Configuration
# =====================================================================
# This file controls the encoder parameters. Invalid values are reported
# at startup and fall back to their defaults individually - all other
# settings stay in effect and the file is never modified.
# Lines starting with # are comments. Format:  key=value

# Constant Quality (CQ). Lower = better quality, larger file.
# Allowed: 1 to 51.  Default: 26
targetCQ=%d

# Maximum target bitrate (kbit/s) in standard mode.
# Allowed: greater than 1000.  Default: 8000
maxBitrate1080p=%d

# Maximum target bitrate (kbit/s) in -original mode.
# Allowed: greater than 1000.  Default: 18000
maxBitrateOriginal=%d

# Target resolution (short edge) in standard mode. Larger material is
# downscaled. Allowed: 720, 1080, 1440, 2160.  Default: 1080
maxResolution=%d

# NVENC encoder preset. p1=fastest, p7=best quality (slower).
# Allowed: p1, p2, p3, p4, p5, p6, p7.  Default: p5
nvencPreset=%s

# Lookahead frames. On VRAM errors with older GPUs lower to 16 or 8.
# Allowed: 0 to 32.  Default: 32
nvencLookahead=%d

# Number of B-frames. Older GPUs may support fewer.
# Allowed: 0 to 4.  Default: 4
bFrames=%d

# Sharpening after downscale (CAS). 0.0=off, 1.0=maximum.
# Allowed: 0.0 to 1.0.  Default: 0.4
casStrength=%s

# AAC bitrate per audio channel (kbit/s) on re-encoding.
# Allowed: greater than 32.  Default: 96
audioKbpsPerChannel=%d

# Minimum/fallback audio bitrate (kbit/s) for AAC.
# Allowed: 128 to 640.  Default: 128
fallbackAudioBitrate=%d

# Shut down the PC automatically when finished.
# Allowed: true, false.  Default: false
autoShutdown=%t

# Extra characters that survive file name cleaning (besides letters,
# digits and dots). Spaces always become dots; multiple dots are always
# collapsed. Windows-forbidden characters (\ / : * ? " < > |) are ignored.
# Example: extraFilenameChars=-_'    Default: (empty)
extraFilenameChars=%s

# --- AV1 mode (-av1, opt-in; requires RTX 40 series or newer) ---

# Constant Quality for AV1 (scale 1-63, NOT comparable to targetCQ!).
# The default was calibrated via VMAF to match the H.265 quality of
# targetCQ=26. Lower = better quality, larger file.  Default: %d
av1TargetCQ=%d

# Maximum AV1 target bitrate (kbit/s) in standard mode.
# AV1 needs ~25-30%% less bitrate than H.265 for equal quality.
# Allowed: greater than 1000.  Default: %d
av1MaxBitrate1080p=%d

# Maximum AV1 target bitrate (kbit/s) in -original mode.
# Allowed: greater than 1000.  Default: %d
av1MaxBitrateOriginal=%d
`,
		d.targetCQ, d.maxBitrate1080p, d.maxBitrateOriginal, d.maxResolution,
		d.nvencPreset, d.nvencLookahead, d.bFrames,
		strconv.FormatFloat(d.casStrength, 'f', -1, 64),
		d.audioKbpsPerChannel, d.fallbackAudioBitrate,
		d.autoShutdown, d.extraFilenameChars,
		d.av1TargetCQ, d.av1TargetCQ,
		d.av1MaxBitrate1080p, d.av1MaxBitrate1080p,
		d.av1MaxBitrateOriginal, d.av1MaxBitrateOriginal)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("Config.go: writeDefaultAppConfig: %w", err)
	}
	return nil
}

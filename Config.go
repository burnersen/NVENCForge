//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

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
	keepOriginal   bool     // -original (alias -orig): Originalauflösung behalten, Bitrate-Cap auf 22000k
	copyAudio      bool     // -copyaudio: Ton 1:1 kopieren (kein DaVinci-AAC-Re-Encode)
	av1            bool     // -av1: opt-in AV1-Encoding (av1_nvenc) statt H.265
	apple          bool     // -apple: Ausgabe als iOS-taugliche MP4 (H.265/hvc1 + AAC + faststart) statt MKV
	keepSource     bool     // -keep: Originaldatei NICHT in den Papierkorb verschieben (bleibt unangetastet)
	autoCQ         bool     // -autocq: CQ pro Datei per Stichproben-VMAF-Suche bestimmen (nur H.265)
	forcedCQ       int      // -cq N: fester CQ nur für diesen Lauf (0 = aus); schlägt Auto-CQ und INI-Ziel-CQ (H.265 1-51, AV1 1-63)
	inputArgs      []string // verbleibende Nicht-Flag-Argumente (Dateien/Ordner)
}

// ----------------------------------------------------------------------------
// AppSettings hält die aus NVENCForge_Config.ini geladenen Encoder-Parameter.
// Wird in main() via loadOrCreateAppConfig() EINMAL befüllt, danach nur lesend.
// ----------------------------------------------------------------------------

type AppSettings struct {
	targetCQ              int
	maxBitrate1080p       int64
	maxBitrateOriginal    int64
	maxResolution         int
	nvencPreset           string
	nvencLookahead        int
	bFrames               int
	casStrength           float64
	audioKbpsPerChannel   int
	fallbackAudioBitrate  int
	autoShutdown          bool
	extraFilenameChars    string
	av1TargetCQ           int
	av1MaxBitrate1080p    int64
	av1MaxBitrateOriginal int64
	autoCQ                bool
	autoCQTargetVMAF      float64
	autoCQTolerance       float64
}

var appSettings = defaultAppSettings()

func defaultAppSettings() AppSettings {
	return AppSettings{
		targetCQ:              26,
		maxBitrate1080p:       8000,
		maxBitrateOriginal:    22000,
		maxResolution:         1080,
		nvencPreset:           "p5",
		nvencLookahead:        32,
		bFrames:               4,
		casStrength:           0.4,
		audioKbpsPerChannel:   96,
		fallbackAudioBitrate:  128,
		autoShutdown:          false,
		extraFilenameChars:    "",
		av1TargetCQ:           32,
		av1MaxBitrate1080p:    6000,
		av1MaxBitrateOriginal: 13000,
		autoCQ:                true,
		autoCQTargetVMAF:      97,
		autoCQTolerance:       0.5,
	}
}

// loadOrCreateAppConfig legt die INI bei Fehlen an. Ungültige Werte werden
// einzeln auf ihren Default zurückgesetzt – mit Warnung UND direkt in der INI
// korrigiert (nur die betroffenen Zeilen); gültige Werte, Kommentare und
// unbekannte Keys bleiben unangetastet. Geschrieben wird nur, wenn überhaupt
// ein ungültiger Wert gefunden wurde.
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

	parsed, invalids, warns := parseAppConfig(path)
	for _, w := range warns {
		pWarn.Println("Config: " + w)
	}
	if len(invalids) > 0 {
		defaults := defaultConfigStrings()
		writeErr := resetInvalidConfigLines(path, invalids)
		for _, iv := range invalids {
			switch {
			case writeErr != nil:
				pWarn.Printf("Config: %s=%q invalid - default value kept (config not writable: %v)\n",
					iv.key, iv.val, writeErr)
			default:
				pWarn.Printf("Config: %s=%q invalid - reset to default (%s) in config file\n",
					iv.key, iv.val, defaults[iv.key])
			}
		}
	}
	appSettings = parsed
}

// invalidSetting hält einen Key, dessen Wert die Validierung nicht bestanden
// hat, samt dem Originalwert – für Warnung und Zurückschreiben in die INI.
type invalidSetting struct{ key, val string }

// defaultConfigStrings liefert für jeden geprüften Key die kanonische
// INI-Schreibweise seines Defaults. Maßgeblich dafür, welche Keys überhaupt
// zurückgeschrieben werden (extraFilenameChars ist bewusst NICHT enthalten).
func defaultConfigStrings() map[string]string {
	d := defaultAppSettings()
	return map[string]string{
		"targetCQ":              strconv.Itoa(d.targetCQ),
		"maxBitrate1080p":       strconv.FormatInt(d.maxBitrate1080p, 10),
		"maxBitrateOriginal":    strconv.FormatInt(d.maxBitrateOriginal, 10),
		"maxResolution":         strconv.Itoa(d.maxResolution),
		"nvencPreset":           d.nvencPreset,
		"nvencLookahead":        strconv.Itoa(d.nvencLookahead),
		"bFrames":               strconv.Itoa(d.bFrames),
		"casStrength":           strconv.FormatFloat(d.casStrength, 'g', -1, 64),
		"audioKbpsPerChannel":   strconv.Itoa(d.audioKbpsPerChannel),
		"fallbackAudioBitrate":  strconv.Itoa(d.fallbackAudioBitrate),
		"autoShutdown":          strconv.FormatBool(d.autoShutdown),
		"av1TargetCQ":           strconv.Itoa(d.av1TargetCQ),
		"av1MaxBitrate1080p":    strconv.FormatInt(d.av1MaxBitrate1080p, 10),
		"av1MaxBitrateOriginal": strconv.FormatInt(d.av1MaxBitrateOriginal, 10),
		"autoCQ":                strconv.FormatBool(d.autoCQ),
		"autoCQTargetVMAF":      strconv.FormatFloat(d.autoCQTargetVMAF, 'f', -1, 64),
		"autoCQTolerance":       strconv.FormatFloat(d.autoCQTolerance, 'f', -1, 64),
	}
}

// resetInvalidConfigLines setzt in der INI ausschließlich den Wert jeder
// ungültigen Zeile auf ihren Default zurück. Kommentare, gültige Werte und
// unbekannte Keys bleiben unangetastet; die linke Seite (Key inkl. Formatierung)
// und die ursprünglichen Zeilenenden bleiben erhalten. No-op, wenn nichts
// zurückzusetzen ist.
func resetInvalidConfigLines(path string, invalids []invalidSetting) error {
	if len(invalids) == 0 {
		return nil
	}
	defaults := defaultConfigStrings()
	resetKey := make(map[string]bool, len(invalids))
	for _, iv := range invalids {
		if _, ok := defaults[iv.key]; ok {
			resetKey[iv.key] = true
		}
	}
	if len(resetKey) == 0 {
		return nil // nur Keys ohne Default-Rückschreibung (z. B. extraFilenameChars)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	crlf := strings.Contains(text, "\r\n")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		left, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if resetKey[strings.TrimSpace(left)] {
			lines[i] = left + "=" + defaults[strings.TrimSpace(left)]
		}
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return os.WriteFile(path, []byte(out), 0644)
}

// parseAppConfig liest mit Bounds-Checking. Jeder ungültige Wert wird als
// invalidSetting zurückgegeben – der Caller meldet ihn UND schreibt den Default
// in die INI zurück. Gültige Werte bleiben erhalten, unbekannte Keys werden
// ignoriert (vorwärtskompatibel). warns sind sonstige Hinweise (z. B. Zeilen
// ohne '='), die nichts zurückschreiben.
func parseAppConfig(path string) (AppSettings, []invalidSetting, []string) {
	s := defaultAppSettings()
	var warns []string
	var invalids []invalidSetting
	f, err := os.Open(path)
	if err != nil {
		return s, invalids, []string{"configuration not readable - using defaults"}
	}
	defer f.Close()

	validPresets := map[string]bool{
		"p1": true, "p2": true, "p3": true, "p4": true,
		"p5": true, "p6": true, "p7": true,
	}
	validRes := map[int]bool{720: true, 1080: true, 1440: true, 2160: true}

	bad := func(key, val string) {
		invalids = append(invalids, invalidSetting{key: key, val: val})
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
		case "autoCQ":
			if b, e := strconv.ParseBool(val); e == nil {
				s.autoCQ = b
			} else {
				bad(key, val)
			}
		case "autoCQTargetVMAF":
			if fv, e := strconv.ParseFloat(val, 64); e == nil && fv >= 70 && fv <= 99 {
				s.autoCQTargetVMAF = fv
			} else {
				bad(key, val)
			}
		case "autoCQTolerance":
			if fv, e := strconv.ParseFloat(val, 64); e == nil && fv >= 0 && fv <= 5 {
				s.autoCQTolerance = fv
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
	return s, invalids, warns
}

// writeDefaultAppConfig schreibt die komplette, kommentierte Standard-INI.
func writeDefaultAppConfig(path string) error {
	d := defaultAppSettings()
	content := fmt.Sprintf(`# NVENCForge Configuration
# =====================================================================
# This file controls the encoder parameters. Invalid values are reported
# at startup and reset to their default right here in the file (only the
# affected lines); all valid settings and comments stay untouched.
# Lines starting with # are comments. Format:  key=value

# Constant Quality (CQ). Lower = better quality, larger file.
# Allowed: 1 to 51.  Default: 26
targetCQ=%d

# Maximum target bitrate (kbit/s) in standard mode.
# Allowed: greater than 1000.  Default: 8000
maxBitrate1080p=%d

# Maximum target bitrate (kbit/s) in -original mode.
# Allowed: greater than 1000.  Default: 22000
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
# Fixed CQ for manual AV1 mode (Auto-CQ off / -noautocq). Measured 2026-07-06:
# CQ 32 is about VMAF 94 - a lean setting, NOT equal to H.265 CQ 26 (which sits
# ~2-3 VMAF points higher). With Auto-CQ on (the default) a per-file value is
# measured, and an unmeasurable clip falls back to a built-in CQ near the target,
# not to this value. Lower = better quality, larger file.  Default: %d
av1TargetCQ=%d

# Maximum AV1 target bitrate (kbit/s) in standard mode.
# AV1 needs ~25-30%% less bitrate than H.265 for equal quality.
# Allowed: greater than 1000.  Default: %d
av1MaxBitrate1080p=%d

# Maximum AV1 target bitrate (kbit/s) in -original mode.
# Allowed: greater than 1000.  Default: %d
av1MaxBitrateOriginal=%d

# --- Auto-CQ mode (-autocq, on by default; H.265 and AV1) ---

# Run Auto-CQ by default, as if -autocq were passed on every start.
# -noautocq disables it for a single run. Works for H.265 and AV1 alike, each
# on its own CQ scale. If Auto-CQ cannot run, H.265 falls back to targetCQ; AV1
# falls back to a built-in CQ near the target (NOT av1TargetCQ, which is lean).
# Allowed: true, false.  Default: %t
autoCQ=%t

# VMAF quality target for -autocq. Sample windows of each file (placed on
# the source's bitrate profile, hardest scene always included) are encoded
# at two anchor CQ values (per codec), measured with VMAF against the source,
# and the CQ expected to hit this target is verified by one extra measurement
# before the real encode. 97 stays visually transparent even in direct
# comparison; lower = smaller files.
# Allowed: 70 to 99.  Default: %s
autoCQTargetVMAF=%s

# How far below autoCQTargetVMAF the Auto-CQ pick may land when that saves
# CQ steps (smaller files). The search aims at (target - tolerance) and
# accepts it as a hit; on pre-compressed sources whose quality tops out
# below the target, the same margin applies under the reachable maximum,
# and flat plateaus are additionally probed above CQ 30 (up to 34) for
# extra savings backed by real measurements.
# Differences up to ~0.5 VMAF are invisible; 0 always chases the target.
# Allowed: 0 to 5.  Default: %s
autoCQTolerance=%s
`,
		d.targetCQ, d.maxBitrate1080p, d.maxBitrateOriginal, d.maxResolution,
		d.nvencPreset, d.nvencLookahead, d.bFrames,
		strconv.FormatFloat(d.casStrength, 'f', -1, 64),
		d.audioKbpsPerChannel, d.fallbackAudioBitrate,
		d.autoShutdown, d.extraFilenameChars,
		d.av1TargetCQ, d.av1TargetCQ,
		d.av1MaxBitrate1080p, d.av1MaxBitrate1080p,
		d.av1MaxBitrateOriginal, d.av1MaxBitrateOriginal,
		d.autoCQ, d.autoCQ,
		strconv.FormatFloat(d.autoCQTargetVMAF, 'f', -1, 64),
		strconv.FormatFloat(d.autoCQTargetVMAF, 'f', -1, 64),
		strconv.FormatFloat(d.autoCQTolerance, 'f', -1, 64),
		strconv.FormatFloat(d.autoCQTolerance, 'f', -1, 64))
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("Config.go: writeDefaultAppConfig: %w", err)
	}
	return nil
}

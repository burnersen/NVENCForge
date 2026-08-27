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
	av1            bool     // -av1: opt-in AV1-Encoding (av1_nvenc bzw. libsvtav1) statt H.265
	cpu            bool     // -cpu: auf dem Prozessor encodieren (libx265/libsvtav1) statt auf der GPU
	mp4Mode        bool     // -mp4 (alt: -apple): Ausgabe als überall abspielbare MP4 (H.265/hvc1 + AAC + faststart) statt MKV
	eightBit       bool     // -8bit: in 8 Bit encodieren statt in 10 Bit (für alte Geräte, die kein Main10 können)
	keepSource     bool     // -keep: Originaldatei NICHT in den Papierkorb verschieben (bleibt unangetastet)
	autoCQ         bool     // -autocq: CQ pro Datei per Stichproben-VMAF-Suche bestimmen (nur H.265)
	forcedCQ       int      // -cq N: fester CQ nur für diesen Lauf (0 = aus); schlägt Auto-CQ und INI-Ziel-CQ (H.265 1-51, AV1 1-63)
	autoCrop       bool     // -crop: schwarze Balken erkennen und wegschneiden (Voreinstellung aus)
	cropCheckOnly  bool     // -cropcheck: nur das Kontrollbild schreiben, NICHT konvertieren
	inputArgs      []string // verbleibende Nicht-Flag-Argumente (Dateien/Ordner)
}

// newAppConfig baut den Startzustand eines Laufs aus der Konfigurationsdatei.
//
// Das ist die eine Stelle, an der die Datei auf den Lauf trifft. Sie steht
// bewusst VOR dem Auswerten der Befehlszeile: Ein Flag überstimmt die Datei,
// niemals umgekehrt. Seit 1.23.0 gilt das auch für die Grundentscheidungen
// (Codec, Container, Bittiefe, Ton, Auflösung, Umgang mit dem Original) —
// vorher waren die nur als Flag zu haben und ließen sich nicht festlegen.
//
// Eigene Funktion, damit eine Prüfung diesen Zustand ohne main() ansehen kann.
func newAppConfig(s AppSettings) *AppConfig {
	return &AppConfig{
		maxBitrateKbps: s.maxBitrate1080p,
		autoShutdown:   s.autoShutdown,
		autoCQ:         s.autoCQ,
		autoCrop:       s.autoCrop,
		av1:            s.codec == codecAV1,
		cpu:            s.encoder == encoderCPU,
		mp4Mode:        s.container == containerMP4,
		copyAudio:      s.audioMode == audioModeCopy,
		eightBit:       s.bitDepth == bitDepth8,
		keepSource:     s.keepSource,
		keepOriginal:   s.keepResolution,
	}
}

// ----------------------------------------------------------------------------
// AppSettings hält die aus NVENCForge_Config.ini geladenen Encoder-Parameter.
// Wird in main() via loadOrCreateAppConfig() EINMAL befüllt, danach nur lesend.
// ----------------------------------------------------------------------------

type AppSettings struct {
	targetCQ               int
	maxBitrate1080p        int64
	maxBitrateOriginal     int64
	maxResolution          int
	nvencPreset            string
	nvencLookahead         int
	bFrames                int
	aqStrength             int
	casStrength            float64
	audioKbpsPerChannel    int
	fallbackAudioBitrate   int
	autoShutdown           bool
	extraFilenameChars     string
	av1TargetCQ            int
	av1MaxBitrate1080p     int64
	av1MaxBitrateOriginal  int64
	autoCQ                 bool
	autoCQTargetVMAF       float64
	autoCQTolerance        float64
	autoCQPlateauTolerance float64
	encoder                string
	cpuPreset              string
	cpuAV1Preset           int
	cpuTargetCRF           int
	cpuAV1TargetCRF        int
	cpuThreads             int
	gpuDecode              bool
	gpuDecodeMaxMbit       int
	retireMode             string
	autoCrop               bool
	codec                  string
	container              string
	audioMode              string
	bitDepth               int
	keepSource             bool
	keepResolution         bool
}

var appSettings = defaultAppSettings()

func defaultAppSettings() AppSettings {
	return AppSettings{
		targetCQ:               26,
		maxBitrate1080p:        8000,
		maxBitrateOriginal:     22000,
		maxResolution:          1080,
		nvencPreset:            "p5",
		nvencLookahead:         32,
		bFrames:                5,
		aqStrength:             2,
		casStrength:            0.4,
		audioKbpsPerChannel:    96,
		fallbackAudioBitrate:   128,
		autoShutdown:           false,
		extraFilenameChars:     "",
		av1TargetCQ:            32,
		av1MaxBitrate1080p:     6000,
		av1MaxBitrateOriginal:  13000,
		autoCQ:                 true,
		autoCQTargetVMAF:       96,
		autoCQTolerance:        0.5,
		autoCQPlateauTolerance: 2.5,
		encoder:                encoderNvidia,
		cpuPreset:              "fast",
		cpuAV1Preset:           6,
		cpuTargetCRF:           18,
		cpuAV1TargetCRF:        32,
		cpuThreads:             0,
		gpuDecode:              true,
		gpuDecodeMaxMbit:       gpuDecodeDefaultMaxMbit,
		retireMode:             retireModeFolder,
		autoCrop:               false,
		codec:                  codecH265,
		container:              containerMKV,
		audioMode:              audioModeAAC,
		bitDepth:               bitDepth10,
		keepSource:             false,
		keepResolution:         false,
	}
}

// Schutzgrenze für das Entpacken auf der Grafikkarte (NVDEC). Der Treiber
// stürzte 2026-06 an einer HEVC-Datei mit rund 400 Mbit/s ab (TDR, Windows
// riss den Grafiktreiber weg) — deshalb bleibt NVDEC oberhalb dieser Grenze
// abgeschaltet und solche Dateien laufen weiter über den bewährten
// Prozessor-Weg.
//
// 50 Mbit/s ist bewusst vorsichtig gewählt: acht Mal unter dem bekannten
// Absturzfall und trotzdem weit über allem, was übliche Quellen liefern
// (4K-Material aus dem Netz liegt bei 10–30 Mbit/s). Wer nachweislich
// höherbitratiges Material fährt, hebt den Wert in der INI an.
//
// Die Grenze wurde NICHT durch Ausprobieren ermittelt — der Test dafür wäre
// genau der Absturz, den sie verhindern soll. Eine Messreihe bis 394 Mbit/s
// (2026-07-29, 15-Sekunden-Ausschnitte) blieb zwar folgenlos, beweist aber
// nur, dass DIESE Ausschnitte liefen: kurze Proben treffen seltene
// Problemstellen im Datenstrom nicht.
const gpuDecodeDefaultMaxMbit = 50

// Encoder-Backends. encoderNvidia ist der Auslieferungszustand (NVENC);
// encoderCPU rechnet auf dem Prozessor (libx265 / libsvtav1) und braucht
// keine Nvidia-Karte.
const (
	encoderNvidia = "nvidia"
	encoderCPU    = "cpu"
)

// Wohin das Original nach erfolgreicher Konvertierung geht.
//
// retireModeFolder ist seit 1.8.0 der Auslieferungszustand: der Unterordner
// "originals" liegt neben der Quelldatei (also auf demselben Laufwerk, das
// Verschieben ist dadurch sofort fertig) und wird von Windows NICHT von
// selbst geleert. Der Papierkorb dagegen wird geleert — durch die
// Speicheroptimierung und durch die Aufgabe SilentCleanup, sobald ein
// Laufwerk knapp wird. Genau dadurch verschwanden Originale unbemerkt.
// Preis der neuen Voreinstellung: der Platz wird erst frei, wenn der
// Anwender den Ordner selbst leert.
const (
	retireModeFolder     = "folder"
	retireModeRecycleBin = "recyclebin"

	// originalsFolderName ist das Gegenstück zum Ausgabeordner "output".
	originalsFolderName = "originals"
)

// Die Grundentscheidungen eines Laufs — was hinten herauskommt.
//
// Sie waren bis 1.23.0 nur als Flags zu haben (-av1, -mp4, -8bit, -copyaudio,
// -orig, -keep) und ließen sich deshalb nicht dauerhaft festlegen. Wer immer
// dasselbe wollte, musste es bei jedem Aufruf mitgeben — und eine Oberfläche
// konnte nicht anzeigen, was denn nun gilt, weil es nirgends stand.
//
// Jede dieser Einstellungen hat ein Flag UND ein Gegen-Flag: Ein Aufruf muss
// sie in beide Richtungen überstimmen können, sonst gewinnt die Datei immer
// gegen das, was im Fenster eingestellt ist.
const (
	codecH265 = "h265"
	codecAV1  = "av1"

	containerMKV = "mkv"
	containerMP4 = "mp4"

	audioModeAAC  = "aac"
	audioModeCopy = "copy"

	// Bittiefe der Ausgabe. 10 Bit ist der Auslieferungszustand: HEVC Main10
	// zeigt in dunklen Verläufen keine Streifen. 8 Bit ist für alte Geräte da.
	bitDepth10 = 10
	bitDepth8  = 8
)

// cpuModeActive gilt für den ganzen Lauf: gesetzt durch das Flag -cpu, den
// INI-Schlüssel encoder=cpu oder den Rückfall, wenn keine NVENC-Karte
// gefunden wurde. Die Options-Bauer lesen es, damit an den Aufrufstellen
// keine zusätzliche Fallunterscheidung nötig ist.
var cpuModeActive = false

// eightBitActive gilt für den ganzen Lauf: gesetzt durch das Flag -8bit.
// Ausgeliefert wird weiterhin 10 Bit — das vermeidet Streifen in dunklen
// Verläufen und kostet bei H.265 nichts. 8 Bit ist ausschließlich eine
// Kompatibilitätskrücke für Geräte, die das Profil "Main 10" nicht
// dekodieren können (ältere Fernseher, Beamer, Android-Handys). Die
// Options-Bauer lesen die Variable, damit an den Aufrufstellen keine
// zusätzliche Fallunterscheidung nötig ist — genau wie bei cpuModeActive.
var eightBitActive = false

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
			// A missing config means this is the very first start: the moment to
			// explain the two files that just appeared out of nowhere, and to
			// say plainly that nothing has to be configured. The bare
			// "Configuration created" line this replaces left newcomers
			// wondering what they were now supposed to fill in.
			printFirstRunWelcome()
		}
		return
	}

	// Die Datei ist da, aber vielleicht älter als das Programm. Fehlende
	// Einstellungen werden ergänzt, BEVOR gelesen wird — nicht wegen der Werte
	// (dafür gäbe es Standardwerte), sondern wegen der Oberfläche: das Fenster
	// baut seine Einstellungsseite aus dieser Datei und kann nur anbieten, was
	// darin steht.
	added, addErr := addMissingConfigEntries(path)
	switch {
	case addErr != nil:
		pWarn.Printf("Config: could not add new settings to the config file: %v\n",
			plainError(addErr))
	case len(added) > 0:
		pInfo.Printf("Config: %d new setting(s) added to NVENCForge_Config.ini: %s\n",
			len(added), strings.Join(added, ", "))
		pInfo.Printf("        Your previous file was kept as %s\n",
			filepath.Base(path)+configBackupSuffix)
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
		"targetCQ":               strconv.Itoa(d.targetCQ),
		"maxBitrate1080p":        strconv.FormatInt(d.maxBitrate1080p, 10),
		"maxBitrateOriginal":     strconv.FormatInt(d.maxBitrateOriginal, 10),
		"maxResolution":          strconv.Itoa(d.maxResolution),
		"nvencPreset":            d.nvencPreset,
		"nvencLookahead":         strconv.Itoa(d.nvencLookahead),
		"bFrames":                strconv.Itoa(d.bFrames),
		"aqStrength":             strconv.Itoa(d.aqStrength),
		"casStrength":            strconv.FormatFloat(d.casStrength, 'g', -1, 64),
		"audioKbpsPerChannel":    strconv.Itoa(d.audioKbpsPerChannel),
		"fallbackAudioBitrate":   strconv.Itoa(d.fallbackAudioBitrate),
		"autoShutdown":           strconv.FormatBool(d.autoShutdown),
		"av1TargetCQ":            strconv.Itoa(d.av1TargetCQ),
		"av1MaxBitrate1080p":     strconv.FormatInt(d.av1MaxBitrate1080p, 10),
		"av1MaxBitrateOriginal":  strconv.FormatInt(d.av1MaxBitrateOriginal, 10),
		"autoCQ":                 strconv.FormatBool(d.autoCQ),
		"autoCQTargetVMAF":       strconv.FormatFloat(d.autoCQTargetVMAF, 'f', -1, 64),
		"autoCQTolerance":        strconv.FormatFloat(d.autoCQTolerance, 'f', -1, 64),
		"autoCQPlateauTolerance": strconv.FormatFloat(d.autoCQPlateauTolerance, 'f', -1, 64),
		"encoder":                d.encoder,
		"cpuPreset":              d.cpuPreset,
		"cpuAV1Preset":           strconv.Itoa(d.cpuAV1Preset),
		"cpuTargetCRF":           strconv.Itoa(d.cpuTargetCRF),
		"cpuAV1TargetCRF":        strconv.Itoa(d.cpuAV1TargetCRF),
		"cpuThreads":             strconv.Itoa(d.cpuThreads),
		"gpuDecode":              strconv.FormatBool(d.gpuDecode),
		"gpuDecodeMaxMbit":       strconv.Itoa(d.gpuDecodeMaxMbit),
		"retireMode":             d.retireMode,
		"autoCrop":               strconv.FormatBool(d.autoCrop),
		"codec":                  d.codec,
		"container":              d.container,
		"audioMode":              d.audioMode,
		"bitDepth":               strconv.Itoa(d.bitDepth),
		"keepSource":             strconv.FormatBool(d.keepSource),
		"keepResolution":         strconv.FormatBool(d.keepResolution),
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
	// x265-Presetnamen (nur diese kennt libx265; "placebo" ist bewusst dabei,
	// auch wenn es praktisch niemand nutzt).
	validCPUPresets := map[string]bool{
		"ultrafast": true, "superfast": true, "veryfast": true, "faster": true,
		"fast": true, "medium": true, "slow": true, "slower": true,
		"veryslow": true, "placebo": true,
	}

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
			if n, e := strconv.Atoi(val); e == nil && n >= 0 && n <= 5 {
				s.bFrames = n
			} else {
				bad(key, val)
			}
		case "aqStrength":
			if n, e := strconv.Atoi(val); e == nil && n >= 1 && n <= 15 {
				s.aqStrength = n
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
		case "autoCQPlateauTolerance":
			if fv, e := strconv.ParseFloat(val, 64); e == nil && fv >= 0 && fv <= 10 {
				s.autoCQPlateauTolerance = fv
			} else {
				bad(key, val)
			}
		case "encoder":
			if e := strings.ToLower(val); e == encoderNvidia || e == encoderCPU {
				s.encoder = e
			} else {
				bad(key, val)
			}
		case "retireMode":
			if m := strings.ToLower(val); m == retireModeFolder || m == retireModeRecycleBin {
				s.retireMode = m
			} else {
				bad(key, val)
			}
		case "cpuPreset":
			if p := strings.ToLower(val); validCPUPresets[p] {
				s.cpuPreset = p
			} else {
				bad(key, val)
			}
		case "cpuAV1Preset":
			// SVT-AV1 kennt 0-13; ab 11 ist es ausdrücklich nur noch für
			// Automatisierung gedacht (der Encoder warnt selbst davor).
			if n, e := strconv.Atoi(val); e == nil && n >= 0 && n <= 13 {
				s.cpuAV1Preset = n
			} else {
				bad(key, val)
			}
		case "cpuTargetCRF":
			if n, e := strconv.Atoi(val); e == nil && n >= 1 && n <= 51 {
				s.cpuTargetCRF = n
			} else {
				bad(key, val)
			}
		case "cpuAV1TargetCRF":
			if n, e := strconv.Atoi(val); e == nil && n >= 1 && n <= 63 {
				s.cpuAV1TargetCRF = n
			} else {
				bad(key, val)
			}
		case "cpuThreads":
			// 0 = alle Kerne. Die Obergrenze ist absichtlich großzügig: mehr
			// Threads als Kerne schadet nicht, der Encoder deckelt selbst.
			if n, e := strconv.Atoi(val); e == nil && n >= 0 && n <= 256 {
				s.cpuThreads = n
			} else {
				bad(key, val)
			}
		case "gpuDecode":
			if b, e := strconv.ParseBool(val); e == nil {
				s.gpuDecode = b
			} else {
				bad(key, val)
			}
		case "autoCrop":
			if b, e := strconv.ParseBool(val); e == nil {
				s.autoCrop = b
			} else {
				bad(key, val)
			}
		case "codec":
			// AV1 gab es bis 1.23.0 nur als Flag -av1. Ein Schlüssel dafür
			// fehlte, und wer immer AV1 wollte, musste es jedes Mal mitgeben.
			if c := strings.ToLower(val); c == codecH265 || c == codecAV1 {
				s.codec = c
			} else {
				bad(key, val)
			}
		case "container":
			if c := strings.ToLower(val); c == containerMKV || c == containerMP4 {
				s.container = c
			} else {
				bad(key, val)
			}
		case "audioMode":
			if m := strings.ToLower(val); m == audioModeAAC || m == audioModeCopy {
				s.audioMode = m
			} else {
				bad(key, val)
			}
		case "bitDepth":
			// Nur die beiden Werte, die der Encoder wirklich kennt. Eine freie
			// Zahl anzunehmen hieße, einen Tippfehler bis zum FFmpeg-Aufruf
			// durchzureichen, wo er als unverständlicher Fehler ankommt.
			if n, e := strconv.Atoi(val); e == nil && (n == bitDepth10 || n == bitDepth8) {
				s.bitDepth = n
			} else {
				bad(key, val)
			}
		case "keepSource":
			if b, e := strconv.ParseBool(val); e == nil {
				s.keepSource = b
			} else {
				bad(key, val)
			}
		case "keepResolution":
			if b, e := strconv.ParseBool(val); e == nil {
				s.keepResolution = b
			} else {
				bad(key, val)
			}
		case "gpuDecodeMaxMbit":
			// Obergrenze 500 statt "beliebig": ein Tippfehler darf nicht dazu
			// führen, dass die Schutzgrenze faktisch wegfällt. 1 = praktisch aus.
			if n, e := strconv.Atoi(val); e == nil && n >= 1 && n <= 500 {
				s.gpuDecodeMaxMbit = n
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
// configLineEnding: die INI wird von Windows-Nutzern mit Notepad geöffnet, das
// bei reinen LF-Umbrüchen früher alles in eine Zeile quetschte. CRLF ist hier
// also kein Formalismus, sondern Lesbarkeit auf dem Zielsystem.
const configLineEnding = "\r\n"

// buildDefaultConfigText erzeugt die Konfigurationsdatei im Auslieferungs-
// zustand als Text.
//
// Aufbau in zwei Teilen: oben die Handvoll Werte, die Nutzer wirklich ändern
// wollen, darunter die Experten-Regler. Vorher standen alle Schlüssel
// gleichrangig untereinander, was die Datei wie eine Aufgabenliste wirken ließ,
// obwohl kein einziger Wert angefasst werden muss.
//
// Jeder Eintrag entsteht über configEntry, damit Schlüssel, Wert, erlaubter
// Bereich und Erklärung im Quelltext beieinanderstehen. Die frühere Fassung war
// eine einzige fmt.Sprintf-Vorlage mit 40 positionsgebundenen Platzhaltern und
// einer separaten Argumentliste — ein eingeschobener Eintrag hätte dort alle
// folgenden Werte lautlos in die falschen Schlüssel geschrieben, und weil die
// Typen gleich sind, hätte weder der Compiler noch go vet etwas gemerkt.
//
// Der Text hat zwei Abnehmer: writeDefaultAppConfig legt damit die Datei beim
// allerersten Start an, und addMissingConfigEntries bedient sich daraus, wenn
// einer vorhandenen Datei später hinzugekommene Einstellungen fehlen.
func buildDefaultConfigText() string {
	d := defaultAppSettings()
	var b strings.Builder

	line := func(text string) {
		b.WriteString(text + configLineEnding)
	}
	heading := func(title string) {
		line("")
		line("# =====================================================================")
		for _, part := range strings.Split(title, "\n") {
			line("#  " + part)
		}
		line("# =====================================================================")
		line("")
	}
	group := func(title string) {
		line("# --- " + title + " ---")
		line("")
	}
	// configEntry schreibt Erklärung, erlaubten Bereich und den Schlüssel selbst.
	// Der erlaubte Bereich ist Pflichtangabe, damit keine Einstellung ohne die
	// Frage "was darf ich hier eintragen?" beantwortet zurückbleibt.
	configEntry := func(key string, value any, allowed, comment string) {
		for _, part := range strings.Split(strings.TrimSpace(comment), "\n") {
			line("# " + part)
		}
		text := fmt.Sprint(value)
		line(fmt.Sprintf("# Allowed: %s   |   Default: %s", allowed, text))
		line(key + "=" + text)
		line("")
	}

	line("# NVENCForge Configuration")
	line("# =====================================================================")
	line("# You do NOT have to change anything in here. Every value below is a")
	line("# tested default - NVENCForge works out of the box exactly as it is.")
	line("#")
	line("# Format:  key=value      Lines starting with # are comments.")
	line("# If a value is invalid, NVENCForge says so at startup and resets THAT")
	line("# line to its default. Your other settings and comments stay untouched.")

	heading("PART 1  -  the handful of settings people actually change")

	configEntry("maxResolution", d.maxResolution, "720, 1080, 1440, 2160",
		`Videos larger than this are scaled down (short edge, in pixels).
1080 is Full HD. Set 2160 to keep 4K material at 4K.
The -original option ignores this for a single run.`)

	configEntry("keepResolution", d.keepResolution, "true, false",
		`Keep the resolution of the source instead of scaling it down to
maxResolution. A 4K film then stays 4K, whatever maxResolution says.
Expect much bigger files and longer encodes - and check that the
device you play on can handle 4K at all.
The -original option does this for a single run, -downscale undoes it.`)

	configEntry("codec", d.codec, "h265, av1",
		`Which video codec the result is encoded in.
"h265" is the safe choice - every current phone, TV and player handles
it, and it is what this program is tuned for.
"av1" reaches the same picture at roughly 25-30 % less bitrate, but it
encodes slower, plays back on fewer devices, and on the graphics card
it needs an RTX 40 series or newer. Its quality is steered by
av1TargetCQ, not targetCQ.
The -av1 option switches a single run over, -h265 back.`)

	configEntry("container", d.container, "mkv, mp4",
		`Which kind of file the result is written into.
"mkv" holds everything: several audio tracks, subtitles of any kind,
chapters.
"mp4" is what phones, TVs and Apple devices play without complaint -
but it cannot store picture subtitles (those tracks are dropped), and
it takes only ONE audio and one subtitle track, so a source with
several opens a chooser.
The -mp4 option switches a single run over, -mkv back.`)

	configEntry("bitDepth", d.bitDepth, "10, 8",
		`How finely colour is graded in the output.
10 keeps dark gradients smooth (HEVC Main10) and is what current
devices expect.
8 is for older hardware that cannot play Main10 - a night sky can show
visible bands in 8 bit.
The -8bit option switches a single run over, -10bit back.`)

	configEntry("audioMode", d.audioMode, "aac, copy",
		`What happens to the sound.
"aac" re-encodes to AAC where the target file needs it and copies the
track 1:1 where it does not.
"copy" never re-encodes: the original sound goes in untouched. That
keeps it bit-perfect - but not every audio format fits into an MP4.
The -copyaudio option switches a single run over, -aac back.`)

	configEntry("autoCrop", d.autoCrop, "true, false",
		`Cut off the black bars of letterboxed video (a 21:9 film inside a
16:9 frame, for example).
What it does for you depends on how quality is decided:
  With a fixed CQ, files get about 6 % smaller for a quarter of the
  frame in bars, and the encode runs 19 % faster.
  With Auto-CQ on (the default), files get LARGER, not smaller - and
  that is the feature working, not failing. Auto-CQ measures quality
  on the whole picture, and black bars flatter that measurement, so
  letterboxed films have been getting less quality than you asked
  for. Cutting the bars makes the measurement honest again.
Off by default on purpose. The bars are detected from nine samples
across the film and the MAJORITY decides, so a logo flashing up
inside a bar cannot hold the cut back. The cut is always symmetric,
because letterbox and pillarbox sit centred. Where the samples
genuinely disagree - a film that changes aspect ratio part-way,
like the IMAX scenes in some blockbusters - nothing is cut at all.
Bars on the sides or on all four edges work the same way.
Subtitles are an exception. A subtitle that sits in the black bar
below the picture would be cut away with it, and for picture
subtitles (Blu-ray PGS, DVD VobSub) that placement is how they are
built - so a file carrying such a track is never cut. Burned-in
subtitles are searched for inside the bars: a wide, centred line
that shows up at a second point in the film calls the cut off, while
a copyright notice appearing once does not. Text subtitle tracks
(SRT, ASS, mov_text) are not affected - the player places those
itself.
No automatic detection is perfect, and a wrong cut is not visible
in the result, only in what is missing from it.
Use -cropcheck first: it writes a picture showing where the cut would
go, without converting anything. The -crop option turns cutting on
for a single run, -nocrop off.`)

	configEntry("autoCQTargetVMAF", d.autoCQTargetVMAF, "70 to 99",
		`How much visible quality the automatic search aims for, on a scale
where 100 is identical to the source. 96 is indistinguishable in
normal viewing; 97 holds up even in a direct side-by-side
comparison; below 94 you start to see it. Higher = bigger files.`)

	configEntry("audioKbpsPerChannel", d.audioKbpsPerChannel, "more than 32",
		`Audio quality when a track has to be re-encoded to AAC, per channel.
96 is roughly CD quality for stereo (2 x 96 = 192 kbit/s).
Tracks that are already fine are copied untouched either way.`)

	configEntry("retireMode", d.retireMode, "folder, recyclebin",
		`What happens to the original after a successful conversion.
"folder" moves it into an "originals" subfolder next to the source.
That is instant even for a 60 GB file, and Windows never touches it -
your originals wait there until YOU delete them.
"recyclebin" is the old behaviour and is NOT safer: Windows empties
the recycle bin on its own when a drive runs low on space, which is
how originals used to vanish unnoticed.
The -keep option always wins and leaves the original where it is.`)

	configEntry("keepSource", d.keepSource, "true, false",
		`Leave the original exactly where it is after a successful conversion,
instead of moving it away as retireMode says.
Both files then exist side by side - the result lives in its own
"output" folder, so nothing is ever overwritten. Keep an eye on your
free space with this switched on.
The -keep option does this for a single run, -nokeep undoes it.`)

	configEntry("encoder", d.encoder, "nvidia, cpu",
		`Which encoder to use. "nvidia" uses the graphics card (fast).
"cpu" uses the processor instead - runs on any machine, but takes
roughly 40 minutes per hour of 1080p video. Without an Nvidia card,
NVENCForge offers CPU mode by itself at startup.
The -cpu option switches a single run over, -gpu back.`)

	heading("PART 2  -  expert settings\nThese are measured, well-tested values. You can safely ignore\nthis entire section - it is here for people who want to tinker.")

	group("Quality and bitrate")

	configEntry("targetCQ", d.targetCQ, "1 to 51",
		`Fixed quality value for H.265, used only when the automatic search
is off (-noautocq). Lower = better quality and bigger files.`)

	configEntry("maxBitrate1080p", d.maxBitrate1080p, "more than 1000",
		`Upper bitrate limit in kbit/s for normal (downscaled) mode.
A ceiling, not a target: most files stay well below it.`)

	configEntry("maxBitrateOriginal", d.maxBitrateOriginal, "more than 1000",
		`Upper bitrate limit in kbit/s when -original is used. Higher,
because 4K material needs more bitrate than 1080p.`)

	configEntry("casStrength", d.casStrength, "0.0 to 1.0",
		`Sharpening applied after downscaling. 0.4 is a light touch,
1.0 is the maximum, 0.0 switches it off entirely.
Switching it off also makes 4K conversions noticeably faster -
it is the single most expensive filter step. The picture just
gets a little softer.`)

	configEntry("fallbackAudioBitrate", d.fallbackAudioBitrate, "128 to 640",
		`Lower limit in kbit/s for re-encoded AAC audio. Without this floor a
mono or low-channel track would end up with far too little bitrate.`)

	group("Automatic quality search (Auto-CQ)")

	configEntry("autoCQ", d.autoCQ, "true, false",
		`Measure the best quality setting for every file. This is what
makes NVENCForge more than a preset - leave it on.
-noautocq switches it off for a single run.`)

	configEntry("autoCQTolerance", d.autoCQTolerance, "0 to 5",
		`How far below the quality target the search may land when that
saves a real amount of file size. Differences up to about 0.5 are
invisible. 0 chases the target exactly and produces bigger files.`)

	configEntry("autoCQPlateauTolerance", d.autoCQPlateauTolerance, "0 to 10",
		`Extra savings allowance for sources that were already heavily
compressed (streaming rips, for example). Their quality tops out
below the target no matter what, so chasing it only wastes space.
Every candidate is verified by a real measurement, never estimated.
0 restores the old, more cautious behaviour.`)

	group("AV1 mode (-av1, needs an RTX 40 series card or newer)")

	configEntry("av1TargetCQ", d.av1TargetCQ, "1 to 63",
		`Fixed quality value for AV1 when the automatic search is off.
This is a DIFFERENT scale than targetCQ - the numbers are not
comparable. 32 here is a lean setting, roughly VMAF 94.`)

	configEntry("av1MaxBitrate1080p", d.av1MaxBitrate1080p, "more than 1000",
		`Bitrate ceiling for AV1 in normal mode. Lower than the H.265
value on purpose: AV1 needs 25-30% less for the same quality.`)

	configEntry("av1MaxBitrateOriginal", d.av1MaxBitrateOriginal, "more than 1000",
		`Bitrate ceiling for AV1 together with -original.`)

	group("CPU mode (-cpu)")

	configEntry("cpuPreset", d.cpuPreset, "ultrafast ... fast, medium, slow ... placebo",
		`Speed/quality trade-off for H.265 on the processor. Measured:
"medium" gains almost nothing over "fast", while "slow" gains a
little quality for three to four times the encoding time.`)

	configEntry("cpuAV1Preset", d.cpuAV1Preset, "0 to 13",
		`Same idea for AV1 on the processor. 0 is slowest/best,
13 is fastest. 6 is the sweet spot; above 8 quality drops off.`)

	configEntry("cpuTargetCRF", d.cpuTargetCRF, "1 to 51",
		`Fixed quality for H.265 on the processor when the automatic
search is off. NOT the same number as targetCQ: the processor
encoder needs roughly 7 steps lower for the same result.`)

	configEntry("cpuAV1TargetCRF", d.cpuAV1TargetCRF, "1 to 63",
		`Fixed quality for AV1 on the processor when the search is off.`)

	configEntry("cpuThreads", d.cpuThreads, "0 to 256",
		`How many processor cores the encoder may use. 0 = all of them
(fastest, but the machine is barely usable meanwhile). Set this to
about half your cores if you want to keep working alongside it.`)

	group("Encoder internals (rarely worth touching)")

	configEntry("nvencPreset", d.nvencPreset, "p1 to p7",
		`Graphics card encoder preset. p1 is fastest, p7 is best quality
and slower. p5 is the balanced middle ground.`)

	configEntry("nvencLookahead", d.nvencLookahead, "0 to 32",
		`How many frames ahead the encoder plans. More is better but
needs graphics memory. You do not have to tune this: if your card
cannot hold the window, the startup check retries with 16, then 8,
and tells you which value it settled on.`)

	configEntry("bFrames", d.bFrames, "0 to 5",
		`Number of B-frames (frames stored as the difference between
their neighbours - they save a lot of space). 5 is the maximum any
current NVIDIA card accepts. Older cards may support fewer or none:
the startup check counts down and keeps the highest number your card
takes, so you do not have to know its limit. Not used by AV1.`)

	configEntry("aqStrength", d.aqStrength, "1 to 15",
		`How strongly the encoder shifts bits towards busy parts of the
picture. Low values spend fewer bits overall. Measured across four
real files: dropping this from 8 to 2 made every one of them 8-28%
smaller at the same quality and cost no extra time, so 2 is the
default. Raise it only if you see blocky patches in dark, flat
areas.`)

	group("Speed")

	configEntry("gpuDecode", d.gpuDecode, "true, false",
		`Unpack the source video on the graphics card instead of the
processor. About 20% faster on 4K sources, and the picture is
bit-for-bit identical - unpacking is exactly defined by the codec
standard, so there is nothing to lose here. Any decoder error
falls back to the processor automatically.`)

	configEntry("gpuDecodeMaxMbit", d.gpuDecodeMaxMbit, "1 to 500",
		`Safety limit for the option above: sources above this bitrate
are always unpacked on the processor. Extreme-bitrate video has
been known to crash display drivers, and no fallback can catch
that - it has to be avoided beforehand. Typical 4K sources run at
10-30 Mbit/s, so the default costs you nothing.`)

	group("Everything else")

	configEntry("autoShutdown", d.autoShutdown, "true, false",
		`Shut the PC down automatically when the whole batch is finished.
The -shutdown option does the same for a single run.`)

	configEntry("extraFilenameChars", d.extraFilenameChars, "any characters, or empty",
		`Characters that survive file name cleaning besides letters, digits
and dots. Spaces always become dots, multiple dots are collapsed,
and characters Windows forbids are always removed.
Example:  extraFilenameChars=-_'`)

	return b.String()
}

// writeDefaultAppConfig legt die INI neu an — nur beim allerersten Start.
func writeDefaultAppConfig(path string) error {
	if err := os.WriteFile(path, []byte(buildDefaultConfigText()), 0644); err != nil {
		return fmt.Errorf("Config.go: writeDefaultAppConfig: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Fehlende Einstellungen nachrüsten
// ----------------------------------------------------------------------------
//
// Eine INI, die schon existierte, wurde bis 1.21.2 nie wieder ergänzt: neue
// Einstellungen kamen nur in Dateien, die frisch angelegt wurden. Wer
// NVENCForge länger benutzt, hatte deshalb eine INI, in der alles fehlte, was
// seit ihrer Entstehung dazugekommen war.
//
// Für den Konverter selbst war das harmlos — für fehlende Schlüssel gelten
// seine eingebauten Standardwerte. Für die Oberfläche war es das nicht: das
// Fenster baut seine Einstellungsseite AUS DER INI. Was dort nicht steht, kann
// es nicht anbieten, und niemand sieht, dass etwas fehlt.

// configBackupSuffix hängt an die Sicherungskopie. Es gibt bewusst nur eine:
// eine wachsende Sammlung von .bak-Dateien will niemand im Programmordner.
const configBackupSuffix = ".bak"

// configBlock ist ein Eintrag der Vorlage: der Schlüssel und die Zeilen, die zu
// ihm gehören (Erklärung, erlaubter Bereich und die Wertzeile selbst).
type configBlock struct {
	key   string
	lines []string
}

// configBlocksFromTemplate zerlegt die Vorlage in ihre Einträge.
//
// Ein Block sind die Kommentarzeilen unmittelbar über einer Wertzeile plus
// diese Zeile. Eine Leerzeile beendet ihn — dieselbe Regel, nach der configEntry
// die Datei schreibt und nach der die Oberfläche sie liest. Überschriften
// bleiben dadurch von selbst außen vor: hinter ihnen steht eine Leerzeile.
func configBlocksFromTemplate() []configBlock {
	var blocks []configBlock
	var pending []string

	for _, raw := range strings.Split(buildDefaultConfigText(), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			pending = nil
		case strings.HasPrefix(trimmed, "#"):
			pending = append(pending, line)
		default:
			key, _, found := strings.Cut(trimmed, "=")
			if !found {
				pending = nil
				continue
			}
			blocks = append(blocks, configBlock{
				key:   strings.TrimSpace(key),
				lines: append(append([]string(nil), pending...), line),
			})
			pending = nil
		}
	}
	return blocks
}

// configKeyOfLine liefert den Schlüssel einer Wertzeile. Kommentare, Leerzeilen
// und Zeilen ohne "=" haben keinen.
func configKeyOfLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	key, _, found := strings.Cut(trimmed, "=")
	if !found {
		return "", false
	}
	return strings.TrimSpace(key), true
}

// configKeysInFile liefert die Schlüssel, die eine INI bereits enthält.
func configKeysInFile(content string) map[string]bool {
	keys := make(map[string]bool)
	for _, raw := range strings.Split(content, "\n") {
		if key, ok := configKeyOfLine(raw); ok {
			keys[key] = true
		}
	}
	return keys
}

// detectLineEnding liest das Zeilenende der vorhandenen Datei ab, statt das
// eigene aufzudrängen: wer seine INI einmal durch ein Werkzeug geschickt hat,
// das auf LF umstellt, bekäme sonst eine Datei mit zwei verschiedenen Enden.
func detectLineEnding(content string) string {
	if strings.Contains(content, "\r\n") {
		return "\r\n"
	}
	if strings.Contains(content, "\n") {
		return "\n"
	}
	return configLineEnding
}

// pendingEntry ist ein noch einzufügender Eintrag samt der Stelle, an die er
// gehört: hinter "anchor", dem letzten Schlüssel vor ihm, den die Datei kennt.
type pendingEntry struct {
	anchor string
	block  configBlock
	placed bool
}

// insertMissingEntries baut den neuen Dateiinhalt.
//
// Getrennt vom Schreiben, damit die Regel selbst ohne Datei auf der Platte
// prüfbar ist — genau hier steckt die Arbeit, nicht im os.WriteFile.
//
// Jeder Eintrag landet an SEINER Stelle: direkt hinter dem Eintrag, der in der
// Vorlage vor ihm steht und in der Datei schon vorhanden ist. Ans Ende hängen
// wäre einfacher, risse die Einstellung aber aus ihrem Abschnitt — die
// Oberfläche gruppiert nach genau dieser Reihenfolge, und ein alltäglicher
// Schalter wie autoCrop gehört nicht hinter die Experten-Regler.
//
// Alles Vorhandene bleibt Zeichen für Zeichen stehen: Werte, Kommentare,
// Einrückung, eigene Notizen.
func insertMissingEntries(content string, blocks []configBlock) (string, []string) {
	keys := configKeysInFile(content)

	var pending []*pendingEntry
	anchor := ""
	for _, block := range blocks {
		if keys[block.key] {
			anchor = block.key
			continue
		}
		pending = append(pending, &pendingEntry{anchor: anchor, block: block})
	}
	if len(pending) == 0 {
		return content, nil
	}

	var out []string
	emit := func(entry *pendingEntry) {
		// Leerzeile zuerst: die Zeile hinter dem Anker gehört noch zu ihm, der
		// neue Eintrag braucht Luft davor. Die Leerzeile, die in der Datei
		// ohnehin folgt, schließt den neuen Block dann ab.
		out = append(out, "")
		out = append(out, entry.block.lines...)
		entry.placed = true
	}

	for _, raw := range strings.Split(content, "\n") {
		out = append(out, strings.TrimSuffix(raw, "\r"))
		key, ok := configKeyOfLine(raw)
		if !ok {
			continue
		}
		for _, entry := range pending {
			if !entry.placed && entry.anchor == key {
				emit(entry)
			}
		}
	}

	// Übrig bleibt, wessen Anker die Datei doch nicht enthielt — etwa weil die
	// Zeile von Hand gelöscht wurde. Hinten anhängen ist unschön, aber besser
	// als die Einstellung stillschweigend fallen zu lassen.
	for _, entry := range pending {
		if !entry.placed {
			emit(entry)
		}
	}

	added := make([]string, 0, len(pending))
	for _, entry := range pending {
		added = append(added, entry.block.key)
	}
	return strings.Join(out, detectLineEnding(content)), added
}

// addMissingConfigEntries trägt fehlende Einstellungen in eine vorhandene INI
// nach und meldet, welche das waren. Angefasst wird die Datei nur, wenn
// wirklich etwas fehlt; vorher entsteht eine Sicherungskopie.
func addMissingConfigEntries(path string) ([]string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("Config.go: addMissingConfigEntries (read): %w", err)
	}

	updated, added := insertMissingEntries(string(content), configBlocksFromTemplate())
	if len(added) == 0 {
		return nil, nil
	}

	if err := os.WriteFile(path+configBackupSuffix, content, 0644); err != nil {
		return nil, fmt.Errorf("Config.go: addMissingConfigEntries (backup): %w", err)
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return nil, fmt.Errorf("Config.go: addMissingConfigEntries (write): %w", err)
	}
	return added, nil
}

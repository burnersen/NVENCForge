//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import "testing"

// Diese Datei prüft die VERDRAHTUNG der Grundeinstellungen, nicht ihre Wirkung:
// Kommt an, was in der Konfigurationsdatei steht, und lässt es sich von der
// Befehlszeile in BEIDE Richtungen überstimmen?
//
// Der Anlass ist ein echter Fehler: autoCrop war im Konverter fertig, kam aber
// nie dort an, wo es hätte gesehen werden müssen. Eine Einstellung, die niemand
// erreicht, ist keine Einstellung.

// withSettings tauscht die globalen Einstellungen für einen Test aus und stellt
// sie danach wieder her. parseArgs liest sie für seine Meldungen mit.
func withSettings(t *testing.T, s AppSettings) {
	t.Helper()
	previous := appSettings
	appSettings = s
	t.Cleanup(func() { appSettings = previous })
}

// TestConfigFileDrivesTheRunDefaults: Was in der Datei steht, gilt auch ohne
// jedes Flag.
func TestConfigFileDrivesTheRunDefaults(t *testing.T) {
	s := defaultAppSettings()
	s.codec = codecAV1
	s.container = containerMP4
	s.audioMode = audioModeCopy
	s.bitDepth = bitDepth8
	s.keepSource = true
	s.keepResolution = true

	cfg := newAppConfig(s)
	if !cfg.av1 {
		t.Error("codec=av1 kommt nicht an")
	}
	if !cfg.mp4Mode {
		t.Error("container=mp4 kommt nicht an")
	}
	if !cfg.copyAudio {
		t.Error("audioMode=copy kommt nicht an")
	}
	if !cfg.eightBit {
		t.Error("bitDepth=8 kommt nicht an")
	}
	if !cfg.keepSource {
		t.Error("keepSource=true kommt nicht an")
	}
	if !cfg.keepOriginal {
		t.Error("keepResolution=true kommt nicht an")
	}
}

// TestDefaultConfigLeavesEverythingOff hält den Auslieferungszustand fest: Ohne
// Zutun bleibt es bei H.265 in MKV, 10 Bit, AAC wo nötig, verkleinern, Original
// wegräumen. Eine Voreinstellung, die sich unbemerkt verschiebt, ändert das
// Ergebnis für jeden, der nichts eingestellt hat.
func TestDefaultConfigLeavesEverythingOff(t *testing.T) {
	cfg := newAppConfig(defaultAppSettings())
	if cfg.av1 || cfg.mp4Mode || cfg.copyAudio || cfg.eightBit || cfg.keepSource || cfg.keepOriginal {
		t.Errorf("Auslieferungszustand ist nicht mehr neutral: %+v", *cfg)
	}
}

// TestFlagsOverrideTheConfigFile: Ein Flag schaltet ein, was die Datei nicht
// vorsieht.
func TestFlagsOverrideTheConfigFile(t *testing.T) {
	s := defaultAppSettings()
	withSettings(t, s)

	cfg := newAppConfig(s)
	cfg.parseArgs([]string{"-av1", "-copyaudio", "-8bit", "-keep", "-orig"})

	if !cfg.av1 || !cfg.copyAudio || !cfg.eightBit || !cfg.keepSource || !cfg.keepOriginal {
		t.Errorf("Flags greifen nicht durch: %+v", *cfg)
	}
}

// TestCounterFlagsOverrideTheConfigFile ist der wichtigere Fall — und der
// Grund, warum es die Gegen-Flags überhaupt gibt: Eine Oberfläche kann
// Argumente nur MITGEBEN, nie wegnehmen. Ohne -mkv, -aac, -10bit, -nokeep und
// -downscale gewänne die Datei immer gegen das, was im Fenster eingestellt ist.
func TestCounterFlagsOverrideTheConfigFile(t *testing.T) {
	s := defaultAppSettings()
	s.codec = codecAV1
	s.container = containerMP4
	s.audioMode = audioModeCopy
	s.bitDepth = bitDepth8
	s.keepSource = true
	s.keepResolution = true
	withSettings(t, s)

	cfg := newAppConfig(s)
	cfg.parseArgs([]string{"-h265", "-mkv", "-aac", "-10bit", "-nokeep", "-downscale"})

	if cfg.av1 {
		t.Error("-h265 hebt codec=av1 nicht auf")
	}
	if cfg.mp4Mode {
		t.Error("-mkv hebt container=mp4 nicht auf")
	}
	if cfg.copyAudio {
		t.Error("-aac hebt audioMode=copy nicht auf")
	}
	if cfg.eightBit {
		t.Error("-10bit hebt bitDepth=8 nicht auf")
	}
	if cfg.keepSource {
		t.Error("-nokeep hebt keepSource=true nicht auf")
	}
	if cfg.keepOriginal {
		t.Error("-downscale hebt keepResolution=true nicht auf")
	}
}

// TestConfiguredCodecPicksItsBitrateCap prüft die Verdrahtung eine Ebene
// weiter: Der Codec aus der Datei muss dieselben Folgen haben wie das Flag —
// AV1 hat eigene Bitraten-Deckel. Käme nur das Flag dort an, liefe ein
// AV1-Lauf aus der Konfigurationsdatei mit den H.265-Werten.
func TestConfiguredCodecPicksItsBitrateCap(t *testing.T) {
	s := defaultAppSettings()
	s.codec = codecAV1
	withSettings(t, s)

	cfg := newAppConfig(s)
	cfg.parseArgs(nil)

	if cfg.maxBitrateKbps != s.av1MaxBitrate1080p {
		t.Errorf("Deckel %d, erwartet den AV1-Wert %d", cfg.maxBitrateKbps, s.av1MaxBitrate1080p)
	}
}

// TestMP4FlagStillReaches prüft -mp4 getrennt: Zusammen mit -av1 ginge es
// unter, weil MP4 den Codec bewusst auf H.265 zwingt (siehe unten).
func TestMP4FlagStillReaches(t *testing.T) {
	s := defaultAppSettings()
	withSettings(t, s)

	cfg := newAppConfig(s)
	cfg.parseArgs([]string{"-mp4"})

	if !cfg.mp4Mode {
		t.Error("-mp4 kommt nicht an")
	}
}

// TestMP4AndAV1SurviveTogether hält den Fall fest, der bis 1.30.0 falsch lief:
// Wer AV1 UND MP4 wählte, bekam H.265 — das Programm schaltete AV1 ab und
// begründete das nur mit einer Zeile im Protokoll. Gemeldet wurde es von einem
// Nutzer, dem genau das an seinen Dateien auffiel.
//
// Beides gehört zusammen: AV1 in MP4 ist ein regulärer Fall (Kennzeichen
// "av01"), und die Entscheidung, ob das Zielgerät ihn abspielt, gehört dem
// Nutzer. Geprüft wird über BEIDE Wege — aus der Konfigurationsdatei und über
// die Schalter —, denn aus derselben Einstellung darf nicht je nach Herkunft
// etwas anderes herauskommen.
func TestMP4AndAV1SurviveTogether(t *testing.T) {
	s := defaultAppSettings()
	s.codec = codecAV1
	s.container = containerMP4
	withSettings(t, s)

	fromFile := newAppConfig(s)
	fromFile.parseArgs(nil)
	if !fromFile.av1 {
		t.Error("codec=av1 zusammen mit container=mp4 verliert AV1")
	}
	if !fromFile.mp4Mode {
		t.Error("container=mp4 ging dabei verloren")
	}

	plain := defaultAppSettings()
	withSettings(t, plain)
	fromFlags := newAppConfig(plain)
	fromFlags.parseArgs([]string{"-av1", "-mp4"})
	if !fromFlags.av1 {
		t.Error("-av1 zusammen mit -mp4 verliert AV1")
	}
	if !fromFlags.mp4Mode {
		t.Error("-mp4 ging dabei verloren")
	}

	// Der Ausweg muss weiter offen stehen: Wer eine überall abspielbare Datei
	// braucht, hebt AV1 mit -h265 auf.
	withSettings(t, s)
	overridden := newAppConfig(s)
	overridden.parseArgs([]string{"-h265"})
	if overridden.av1 {
		t.Error("-h265 hebt codec=av1 im MP4-Modus nicht auf")
	}
}

// TestGPUFlagOverridesCPUFromTheConfigFile: Ohne dieses Gegenstück könnte ein
// Fenster encoder=cpu nicht abwählen — es kann Argumente nur mitgeben.
func TestGPUFlagOverridesCPUFromTheConfigFile(t *testing.T) {
	s := defaultAppSettings()
	s.encoder = encoderCPU
	withSettings(t, s)

	fromFile := newAppConfig(s)
	if !fromFile.cpu {
		t.Fatal("encoder=cpu kommt gar nicht erst an")
	}

	cfg := newAppConfig(s)
	cfg.parseArgs([]string{"-gpu"})
	if cfg.cpu {
		t.Error("-gpu hebt encoder=cpu nicht auf")
	}
}

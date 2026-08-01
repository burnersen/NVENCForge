//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfigRoundTrip ist die Absicherung gegen den einen Fehler, den
// eine Konfigurationsvorlage still begehen kann: einen Wert beim falschen
// Schlüssel abzulegen. Geschrieben, gelesen und verglichen — weicht ein
// einziges Feld ab, steht die Zuordnung in writeDefaultAppConfig falsch.
func TestDefaultConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
	if err := writeDefaultAppConfig(path); err != nil {
		t.Fatalf("writeDefaultAppConfig: %v", err)
	}

	parsed, invalids, warns := parseAppConfig(path)
	for _, iv := range invalids {
		t.Errorf("frisch geschriebene Vorlage enthält ungültigen Wert: %s=%q", iv.key, iv.val)
	}
	for _, w := range warns {
		t.Errorf("frisch geschriebene Vorlage erzeugt Warnung: %s", w)
	}
	if parsed != defaultAppSettings() {
		t.Errorf("gelesene Einstellungen weichen von den Vorgaben ab\n gelesen:  %+v\n erwartet: %+v",
			parsed, defaultAppSettings())
	}
}

// TestDefaultConfigContainsEveryKey stellt sicher, dass beim Umsortieren der
// Vorlage kein Schlüssel verlorengeht oder doppelt auftaucht. Ein fehlender
// Schlüssel fiele sonst nicht auf: die Voreinstellung greift ja trotzdem, nur
// findet der Anwender den Wert nie und kann ihn nicht ändern.
func TestDefaultConfigContainsEveryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
	if err := writeDefaultAppConfig(path); err != nil {
		t.Fatalf("writeDefaultAppConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Nur echte Einstellungszeilen zählen; Kommentare nennen Schlüsselnamen im
	// Fließtext und würden das Ergebnis sonst verfälschen.
	found := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, _, ok := strings.Cut(line, "="); ok {
			found[strings.TrimSpace(key)]++
		}
	}

	// defaultConfigStrings kennt jeden validierten Schlüssel; extraFilenameChars
	// steht dort bewusst nicht drin (leerer Wert lässt sich nicht validieren)
	// und wird deshalb einzeln geprüft.
	expected := []string{"extraFilenameChars"}
	for key := range defaultConfigStrings() {
		expected = append(expected, key)
	}
	for _, key := range expected {
		switch found[key] {
		case 1: // genau richtig
		case 0:
			t.Errorf("Schlüssel %q fehlt in der Konfigurationsvorlage", key)
		default:
			t.Errorf("Schlüssel %q steht %dx in der Vorlage", key, found[key])
		}
	}
	for key := range found {
		if _, known := defaultConfigStrings()[key]; !known && key != "extraFilenameChars" {
			t.Errorf("Vorlage enthält unbekannten Schlüssel %q", key)
		}
	}
}

// TestDefaultConfigDocumentsEveryValue hält die Zusage ein, dass zu jeder
// Einstellung erkennbar ist, was man eintragen darf. Ohne diese Prüfung
// rutscht beim nächsten neuen Schlüssel leicht ein undokumentierter Eintrag
// in die Datei.
func TestDefaultConfigDocumentsEveryValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
	if err := writeDefaultAppConfig(path); err != nil {
		t.Fatalf("writeDefaultAppConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "=") {
			continue
		}
		key, _, _ := strings.Cut(trimmed, "=")
		if i == 0 || !strings.HasPrefix(strings.TrimSpace(lines[i-1]), "# Allowed:") {
			t.Errorf("Schlüssel %q hat keine Zeile mit dem erlaubten Wertebereich über sich",
				strings.TrimSpace(key))
		}
	}
}

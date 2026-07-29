//go:build windows && amd64

// Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withRetireMode setzt den paketweiten Schalter und stellt ihn danach wieder her.
func withRetireMode(t *testing.T, mode string) {
	t.Helper()
	old := appSettings
	t.Cleanup(func() { appSettings = old })
	appSettings = defaultAppSettings()
	appSettings.retireMode = mode
}

// Der Auslieferungszustand muss der sichere Weg sein: Ordner, nicht Papierkorb.
func TestRetireModeDefaultIsFolder(t *testing.T) {
	if got := defaultAppSettings().retireMode; got != retireModeFolder {
		t.Errorf("Voreinstellung ist %q, erwartet %q", got, retireModeFolder)
	}
}

// Das Original landet im Unterordner NEBEN der Quelle — gleiches Laufwerk,
// damit das Verschieben ein Umbenennen bleibt.
func TestMoveOriginalToFolder(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mkv")
	if err := os.WriteFile(src, []byte("video"), 0644); err != nil {
		t.Fatalf("Testdatei: %v", err)
	}

	moved, err := moveOriginalToFolder(src)
	if err != nil {
		t.Fatalf("Verschieben fehlgeschlagen: %v", err)
	}
	if want := filepath.Join(dir, originalsFolderName, "clip.mkv"); moved != want {
		t.Errorf("Ziel ist %q, erwartet %q", moved, want)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("die Quelldatei liegt noch am alten Platz")
	}
	if data, err := os.ReadFile(moved); err != nil || string(data) != "video" {
		t.Errorf("Inhalt am Ziel stimmt nicht: %v / %q", err, data)
	}
}

// Gleichnamige Quellen aus verschiedenen Ordnern dürfen sich im Zielordner
// nicht gegenseitig überschreiben.
func TestMoveOriginalKeepsBothOnNameClash(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "clip.mkv")
	if err := os.WriteFile(first, []byte("erste"), 0644); err != nil {
		t.Fatalf("Testdatei: %v", err)
	}
	if _, err := moveOriginalToFolder(first); err != nil {
		t.Fatalf("erstes Verschieben: %v", err)
	}
	if err := os.WriteFile(first, []byte("zweite"), 0644); err != nil {
		t.Fatalf("zweite Testdatei: %v", err)
	}

	moved, err := moveOriginalToFolder(first)
	if err != nil {
		t.Fatalf("zweites Verschieben: %v", err)
	}
	if filepath.Base(moved) != "clip (2).mkv" {
		t.Errorf("zweite Datei heißt %q, erwartet \"clip (2).mkv\"", filepath.Base(moved))
	}
	entries, err := os.ReadDir(filepath.Join(dir, originalsFolderName))
	if err != nil {
		t.Fatalf("Zielordner: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("%d Dateien im Zielordner, erwartet 2", len(entries))
	}
}

// Eine fehlende Quelldatei darf einen Fehler geben, aber nichts anrichten.
func TestMoveOriginalMissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := moveOriginalToFolder(filepath.Join(dir, "gibtesnicht.mkv")); err == nil {
		t.Error("fehlende Datei wurde als erfolgreich verschoben gemeldet")
	}
}

// freeOriginalsPath muss die Endung erhalten und darf nie einen belegten
// Namen zurückgeben.
func TestFreeOriginalsPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.mkv", "a (2).mkv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatalf("Testdatei %s: %v", name, err)
		}
	}
	got, err := freeOriginalsPath(dir, "a.mkv")
	if err != nil {
		t.Fatalf("kein freier Name gefunden: %v", err)
	}
	if filepath.Base(got) != "a (3).mkv" {
		t.Errorf("freier Name ist %q, erwartet \"a (3).mkv\"", filepath.Base(got))
	}
	if !strings.HasSuffix(got, ".mkv") {
		t.Errorf("Endung verloren: %q", got)
	}
}

// Der Ordner der beiseitegelegten Originale darf beim Einsammeln von
// Videodateien nicht wieder mitgenommen werden — sonst würde das Programm
// seine eigenen Quellen erneut konvertieren.
func TestOriginalsFolderIsSkippedWhenCollecting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "neu.mkv"), []byte("x"), 0644); err != nil {
		t.Fatalf("Testdatei: %v", err)
	}
	for _, sub := range []string{"output", originalsFolderName} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatalf("Unterordner %s: %v", sub, err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "alt.mkv"), []byte("x"), 0644); err != nil {
			t.Fatalf("Datei in %s: %v", sub, err)
		}
	}

	var found []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.EqualFold(d.Name(), "output") ||
				strings.EqualFold(d.Name(), originalsFolderName) {
				return filepath.SkipDir
			}
			return nil
		}
		if videoExtensions[strings.ToLower(filepath.Ext(path))] {
			found = append(found, filepath.Base(path))
		}
		return nil
	})
	if len(found) != 1 || found[0] != "neu.mkv" {
		t.Errorf("eingesammelt wurde %v, erwartet nur [neu.mkv]", found)
	}
}

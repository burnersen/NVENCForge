//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

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

// -keep heißt: die Quelle bleibt, wo und wie sie ist — auch dann, wenn der
// eingestellte Weg sie sonst wegräumen würde.
func TestRetireOriginalKeepLeavesFileUntouched(t *testing.T) {
	withRetireMode(t, retireModeFolder)
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mkv")
	if err := os.WriteFile(src, []byte("video"), 0644); err != nil {
		t.Fatalf("Testdatei: %v", err)
	}

	retireOriginal(&AppConfig{keepSource: true}, src)

	if _, err := os.Stat(src); err != nil {
		t.Errorf("Quelle ist trotz -keep nicht mehr an ihrem Platz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, originalsFolderName)); !os.IsNotExist(err) {
		t.Errorf("Ordner %q wurde trotz -keep angelegt", originalsFolderName)
	}
}

// Die Abkürzung "nur umbenennen" hängt an canRenameInPlace. -mp4 schließt sie
// dort aus; -keep wird an der Aufrufstelle ausgeschlossen — beides zusammen
// stellt sicher, dass eine behaltene Quelle ihren Namen nie wechselt.
func TestRenameInPlaceIsRuledOutForMP4(t *testing.T) {
	stats := &VideoStats{VideoCodec: "hevc"}
	if !canRenameInPlace(stats, "hevc", ".mkv", false, false) {
		t.Fatal("Ausgangslage stimmt nicht: eine fertige H.265-MKV müsste umbenannt werden dürfen")
	}
	if canRenameInPlace(stats, "hevc", ".mkv", true, false) {
		t.Error("-mp4 darf die Quelle nicht nur umbenennen")
	}
}

// Ein -mp4-Lauf hinterlässt keine .mkv. Die Suche nach einem fertigen Ergebnis
// muss deshalb auch die .mp4-Namen kennen — aber nur in einem -mp4-Lauf.
func TestFinishedOutputCandidates(t *testing.T) {
	dir := filepath.Join("C:", "filme", "output")

	mkvOnly := finishedOutputCandidates(dir, "clip", false)
	if len(mkvOnly) != len(skipSuffixes) {
		t.Fatalf("ohne -mp4: %d Namen, erwartet %d", len(mkvOnly), len(skipSuffixes))
	}
	for _, name := range mkvOnly {
		if isMP4Path(name) {
			t.Errorf("ohne -mp4 darf keine MP4 als fertiges Ergebnis gelten: %s", name)
		}
	}

	withMP4 := finishedOutputCandidates(dir, "clip", true)
	if len(withMP4) != len(skipSuffixes)*2 {
		t.Fatalf("mit -mp4: %d Namen, erwartet %d", len(withMP4), len(skipSuffixes)*2)
	}
	want := filepath.Join(dir, "clip.h265.mp4")
	found := false
	for _, name := range withMP4 {
		if name == want {
			found = true
		}
	}
	if !found {
		t.Errorf("mit -mp4 fehlt %s unter den Namen", want)
	}
}

// Nur was im "output"-Ordner DER QUELLE liegt, ist unsere Ausgabe. Ein Original,
// das selbst in einem Ordner namens "output" liegt, ist es nicht — es wurde
// früher für unsere Ausgabe gehalten und nach dem Umpacken gelöscht.
func TestIsOwnOutputFile(t *testing.T) {
	root := filepath.Join("C:", "filme")
	cases := []struct {
		name   string
		input  string
		output string
		want   bool
	}{
		{"Ergebnis im eigenen output-Ordner",
			filepath.Join(root, "clip.mkv"), filepath.Join(root, "output", "clip.h265.mkv"), true},
		{"Groß-/Kleinschreibung des Ordners",
			filepath.Join(root, "clip.mkv"), filepath.Join(root, "OUTPUT", "clip.h265.mkv"), true},
		{"Original liegt selbst in einem output-Ordner",
			filepath.Join(root, "output", "clip.mkv"), filepath.Join(root, "output", "clip.h264.mkv"), false},
		{"Original unverändert zurückgemeldet",
			filepath.Join(root, "clip.mkv"), filepath.Join(root, "clip.mkv"), false},
		{"Quelle unbekannt",
			"", filepath.Join(root, "output", "clip.h265.mkv"), false},
	}
	for _, c := range cases {
		if got := isOwnOutputFile(c.input, c.output); got != c.want {
			t.Errorf("%s: isOwnOutputFile = %v, erwartet %v", c.name, got, c.want)
		}
	}
}

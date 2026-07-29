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

// Netzwerkpfade haben keinen Papierkorb — ein lokaler Long-Path (\\?\C:\...)
// aber schon. Die beiden dürfen nie verwechselt werden.
func TestIsUNCPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{`\\NAS\Filme\a.mkv`, true},
		{`\\?\UNC\NAS\Filme\a.mkv`, true},
		{`\\?\unc\nas\filme\a.mkv`, true},
		{`\\?\C:\Filme\a.mkv`, false},
		{`C:\Filme\a.mkv`, false},
		{`C:\Festplatte1\Filme\a.mkv`, false},
		{`R:\a.mkv`, false},
	}
	for _, c := range cases {
		if got := isUNCPath(c.path); got != c.want {
			t.Errorf("isUNCPath(%q) = %v, erwartet %v", c.path, got, c.want)
		}
	}
}

// Der eigentliche Auslöser des Datenverlusts: Datei größer als das
// Papierkorb-Limit des Laufwerks.
func TestRecycleBinFitsLimit(t *testing.T) {
	const mb = int64(recycleBinBytesPerMB)

	cases := []struct {
		name  string
		size  int64
		capMB uint32
		want  bool
	}{
		{"deutlich kleiner als das Limit", 200 * mb, 204, true},
		{"genau auf dem Limit", 204 * mb, 204, true},
		{"ein Byte über dem Limit", 204*mb + 1, 204, false},
		{"300-MB-Datei bei 204-MB-Limit (RamDisk-Fall)", 300 * mb, 204, false},
		{"62-GB-Rip bei 48,6-GB-Limit", 62 * 1024 * mb, 49740, false},
		{"40-GB-Rip bei 48,6-GB-Limit", 40 * 1024 * mb, 49740, true},
		{"Papierkorb ohne Platz nimmt nichts", 1, 0, false},
		{"Größe unbekannt: kein Urteil", -1, 204, true},
	}
	for _, c := range cases {
		if got := recycleBinFitsLimit(c.size, c.capMB); got != c.want {
			t.Errorf("%s: recycleBinFitsLimit(%d, %d) = %v, erwartet %v",
				c.name, c.size, c.capMB, got, c.want)
		}
	}
}

// Nachkontrolle: Anzahl ODER Größe muss gewachsen sein. Läuft der Papierkorb
// über, wirft Windows Ältere heraus — dann sinkt die Anzahl, obwohl unsere
// Datei angekommen ist.
func TestRecycleBinAccepted(t *testing.T) {
	cases := []struct {
		name          string
		before, after shQueryRBInfo
		want          bool
	}{
		{
			name:   "Normalfall: ein Element mehr",
			before: shQueryRBInfo{i64NumItems: 2, i64Size: 6148},
			after:  shQueryRBInfo{i64NumItems: 3, i64Size: 5249028},
			want:   true,
		},
		{
			name:   "Überlauf: Anzahl sinkt, Größe wächst",
			before: shQueryRBInfo{i64NumItems: 28, i64Size: 8_100_000_000},
			after:  shQueryRBInfo{i64NumItems: 1, i64Size: 45_000_000_000},
			want:   true,
		},
		{
			name:   "endgültig gelöscht: nichts hat sich bewegt",
			before: shQueryRBInfo{i64NumItems: 1, i64Size: 2050},
			after:  shQueryRBInfo{i64NumItems: 1, i64Size: 2050},
			want:   false,
		},
		{
			name:   "Papierkorb wurde nebenher geleert",
			before: shQueryRBInfo{i64NumItems: 5, i64Size: 500_000},
			after:  shQueryRBInfo{i64NumItems: 0, i64Size: 0},
			want:   false,
		},
	}
	for _, c := range cases {
		if got := recycleBinAccepted(c.before, c.after); got != c.want {
			t.Errorf("%s: recycleBinAccepted = %v, erwartet %v", c.name, got, c.want)
		}
	}
}

func TestRecycleSizeText(t *testing.T) {
	const mb = int64(recycleBinBytesPerMB)

	cases := []struct {
		size int64
		want string
	}{
		{0, "0 MB"},
		{204 * mb, "204 MB"},
		{1024 * mb, "1.0 GB"},
		{49740 * mb, "48.6 GB"},
	}
	for _, c := range cases {
		if got := recycleSizeText(c.size); got != c.want {
			t.Errorf("recycleSizeText(%d) = %q, erwartet %q", c.size, got, c.want)
		}
	}
}

// Auf einem Netzwerkpfad darf nichts gelöscht werden — und die Begründung
// muss verständlich sein, sie landet in der Programmausgabe.
func TestCheckRecycleBinRejectsNetworkPath(t *testing.T) {
	check := checkRecycleBin(`\\NAS\Filme\a.mkv`, 1024)
	if check.canRecycle {
		t.Fatal("Netzwerkpfad wurde als papierkorb-fähig gemeldet")
	}
	if !strings.Contains(check.reason, "network") {
		t.Errorf("Begründung nennt das Netzwerk nicht: %q", check.reason)
	}
}

// Integrationsprüfung gegen das echte Windows: eine winzige Datei auf dem
// Testlaufwerk MUSS papierkorb-fähig sein, und der Füllstand muss abfragbar
// sein. Es wird nichts gelöscht.
func TestCheckRecycleBinAcceptsLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recyclebin-probe.tmp")
	if err := os.WriteFile(path, []byte("probe"), 0644); err != nil {
		t.Fatalf("Testdatei konnte nicht angelegt werden: %v", err)
	}

	check := checkRecycleBin(path, 5)
	if !check.canRecycle {
		t.Fatalf("lokale Kleindatei abgelehnt: %s", check.reason)
	}
	if check.volumeRoot == "" {
		t.Fatal("Einhängepunkt wurde nicht ermittelt")
	}
	if _, ok := queryRecycleBin(check.volumeRoot); !ok {
		t.Errorf("Füllstand von %q nicht abfragbar", check.volumeRoot)
	}
}

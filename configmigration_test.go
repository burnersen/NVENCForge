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

// TestOutdatedConfigGetsEveryMissingKey ist die Zusage, an der die
// Einstellungsseite der Oberfläche hängt: eine INI aus einer älteren Ausgabe
// muss nach dem Start den VOLLEN Satz Einstellungen enthalten.
//
// Dass die Vorlage vollständig ist, prüft bereits
// TestDefaultConfigContainsEveryKey. Hier geht es um den Fall, den es bis
// 1.21.2 gar nicht gab — die schon vorhandene, veraltete Datei.
func TestOutdatedConfigGetsEveryMissingKey(t *testing.T) {
	// So sieht eine alte Datei aus: ein paar Schlüssel mit eigenen Werten,
	// alles Spätere fehlt.
	outdated := "maxResolution=1080\r\n\r\ntargetCQ=28\r\n"

	blocks := configBlocksFromTemplate()
	updated, added := insertMissingEntries(outdated, blocks)
	if len(added) == 0 {
		t.Fatal("an einer veralteten Datei wurde nichts ergänzt")
	}

	have := configKeysInFile(updated)
	for _, block := range blocks {
		if !have[block.key] {
			t.Errorf("nach dem Ergänzen fehlt %q immer noch", block.key)
		}
	}
}

// TestInsertMissingEntriesKeepsEverythingElse: an einer Datei, die dem Nutzer
// gehört, darf nichts verrutschen. Werte, eigene Kommentare und Reihenfolge
// müssen Zeichen für Zeichen überleben.
func TestInsertMissingEntriesKeepsEverythingElse(t *testing.T) {
	original := "# meine eigene Notiz\r\n" +
		"maxResolution=1440\r\n" +
		"\r\n" +
		"# noch eine Notiz\r\n" +
		"targetCQ=28\r\n"

	updated, added := insertMissingEntries(original, configBlocksFromTemplate())
	if len(added) == 0 {
		t.Fatal("es hätte etwas ergänzt werden müssen")
	}
	for _, line := range strings.Split(original, "\r\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(updated, line) {
			t.Errorf("Zeile %q ist verlorengegangen", line)
		}
	}
	// Die eingestellten Werte dürfen NICHT auf den Standard zurückfallen.
	if !strings.Contains(updated, "maxResolution=1440") {
		t.Error("maxResolution=1440 wurde überschrieben")
	}
	if !strings.Contains(updated, "targetCQ=28") {
		t.Error("targetCQ=28 wurde überschrieben")
	}
}

// TestInsertMissingEntriesPlacesEntryAtItsPlace: eine nachgerüstete Einstellung
// gehört an ihre Stelle, nicht ans Dateiende. Die Oberfläche gruppiert nach
// dieser Reihenfolge — autoCrop ist ein Alltagsschalter und darf nicht hinter
// den Experten-Reglern landen.
func TestInsertMissingEntriesPlacesEntryAtItsPlace(t *testing.T) {
	// autoCrop steht in der Vorlage direkt hinter maxResolution.
	original := "maxResolution=1080\r\n\r\ntargetCQ=28\r\n"

	updated, added := insertMissingEntries(original, configBlocksFromTemplate())
	if !contains(added, "autoCrop") {
		t.Fatalf("autoCrop wurde nicht ergänzt (ergänzt: %v)", added)
	}
	posRes := strings.Index(updated, "maxResolution=")
	posCrop := strings.Index(updated, "autoCrop=")
	posCQ := strings.Index(updated, "targetCQ=")
	if posCrop < posRes || posCrop > posCQ {
		t.Errorf("autoCrop steht an der falschen Stelle (maxResolution %d, autoCrop %d, targetCQ %d)",
			posRes, posCrop, posCQ)
	}
}

// TestInsertMissingEntriesIsQuietWhenComplete: eine vollständige Datei darf
// nicht angefasst werden. Sonst entstünde bei jedem Start eine Sicherungskopie
// und die Datei würde ohne Grund neu geschrieben.
func TestInsertMissingEntriesIsQuietWhenComplete(t *testing.T) {
	complete := buildDefaultConfigText()
	updated, added := insertMissingEntries(complete, configBlocksFromTemplate())
	if len(added) != 0 {
		t.Errorf("an einer vollständigen Datei wurde ergänzt: %v", added)
	}
	if updated != complete {
		t.Error("die Datei wurde verändert, obwohl nichts fehlte")
	}
}

// TestInsertMissingEntriesKeepsLineEnding: wer seine INI einmal auf LF
// umgestellt hat, soll keine Datei mit gemischten Zeilenenden zurückbekommen.
func TestInsertMissingEntriesKeepsLineEnding(t *testing.T) {
	lf := "maxResolution=1080\n\ntargetCQ=28\n"
	updated, _ := insertMissingEntries(lf, configBlocksFromTemplate())
	if strings.Contains(updated, "\r\n") {
		t.Error("in eine LF-Datei wurden CRLF-Zeilen geschrieben")
	}

	crlf := "maxResolution=1080\r\n\r\ntargetCQ=28\r\n"
	updated, _ = insertMissingEntries(crlf, configBlocksFromTemplate())
	if strings.Contains(strings.ReplaceAll(updated, "\r\n", ""), "\n") {
		t.Error("in eine CRLF-Datei wurden nackte LF-Zeilen geschrieben")
	}
}

// TestAddMissingConfigEntriesWritesBackup: bevor die Datei des Nutzers
// verändert wird, muss eine Sicherung danebenliegen.
func TestAddMissingConfigEntriesWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "NVENCForge_Config.ini")
	original := "maxResolution=1440\r\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("Testdatei nicht schreibbar: %v", err)
	}

	added, err := addMissingConfigEntries(path)
	if err != nil {
		t.Fatalf("unerwarteter Fehler: %v", err)
	}
	if len(added) == 0 {
		t.Fatal("es hätte etwas ergänzt werden müssen")
	}

	backup, err := os.ReadFile(path + configBackupSuffix)
	if err != nil {
		t.Fatalf("keine Sicherungskopie angelegt: %v", err)
	}
	if string(backup) != original {
		t.Errorf("die Sicherung enthält nicht den alten Stand: %q", string(backup))
	}

	// Und die ergänzte Datei muss der Parser wieder lesen können.
	if _, _, warns := parseAppConfig(path); len(warns) > 0 {
		t.Errorf("die ergänzte Datei erzeugt Warnungen: %v", warns)
	}
}

// contains ist ein Testhelfer.
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

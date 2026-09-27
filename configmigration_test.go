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

// TestUpdateConfigEntriesWritesBackup: bevor die Datei des Nutzers
// verändert wird, muss eine Sicherung danebenliegen.
func TestUpdateConfigEntriesWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "NVENCForge_Config.ini")
	original := "maxResolution=1440\r\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("Testdatei nicht schreibbar: %v", err)
	}

	update, err := updateConfigEntries(path)
	if err != nil {
		t.Fatalf("unerwarteter Fehler: %v", err)
	}
	if len(update.added) == 0 {
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

// oldINI134 ist ein Ausschnitt einer INI, wie 1.34.0 sie geschrieben hat —
// mit den Deckel-Schlüsseln, der Toleranz und dem veralteten Text zum
// VMAF-Ziel, der noch auf den Kosten-Deckel verweist. Der Nutzer hat eigene
// Werte eingetragen (96.5, 0.4, 3) und eine eigene Notiz ganz oben.
const oldINI134 = "# meine Notiz\n" +
	"\n" +
	"# How much visible quality the automatic search aims for. It works hand\n" +
	"# in hand with \"autoCQMaxSourcePercent\", which caps what a file may cost.\n" +
	"# Allowed: 70 to 99   |   Default: 97\n" +
	"autoCQTargetVMAF=96.5\n" +
	"\n" +
	"# --- Quality and bitrate ---\n" +
	"\n" +
	"# Upper bitrate limit in kbit/s for normal (downscaled) mode.\n" +
	"# Allowed: more than 1000   |   Default: 8000\n" +
	"maxBitrate1080p=8000\n" +
	"\n" +
	"# Sharpening applied after downscaling.\n" +
	"# Allowed: 0.0 to 1.0   |   Default: 0.4\n" +
	"casStrength=0.3\n" +
	"\n" +
	"# How far below the quality target the search may land.\n" +
	"# Allowed: 0 to 5   |   Default: 0.5\n" +
	"autoCQTolerance=0.4\n" +
	"\n" +
	"# Extra savings allowance.\n" +
	"# Allowed: 0 to 10   |   Default: 1.5\n" +
	"autoCQPlateauTolerance=3\n" +
	"\n" +
	"# Spending limit for the quality search.\n" +
	"# Allowed: 0, or 10 to 100   |   Default: 45\n" +
	"autoCQMaxSourcePercent=0\n"

// TestRemoveRetiredEntries: die Zeilen der weggefallenen Schlüssel gehen samt
// Erklärung und der Leerzeile dahinter; alles andere bleibt Zeichen für
// Zeichen stehen.
func TestRemoveRetiredEntries(t *testing.T) {
	got, removed := removeRetiredEntries(oldINI134, retiredConfigKeys)

	for _, key := range []string{"maxBitrate1080p", "autoCQTolerance", "autoCQMaxSourcePercent"} {
		if !contains(removed, key) {
			t.Errorf("%s wurde nicht als entfernt gemeldet (%v)", key, removed)
		}
		if strings.Contains(got, key+"=") {
			t.Errorf("%s steht noch in der Datei:\n%s", key, got)
		}
	}
	for _, gone := range []string{"Upper bitrate limit", "How far below", "Spending limit"} {
		if strings.Contains(got, gone) {
			t.Errorf("die Erklärung %q eines entfernten Eintrags blieb stehen", gone)
		}
	}
	for _, kept := range []string{
		"# meine Notiz", "autoCQTargetVMAF=96.5", "casStrength=0.3",
		"autoCQPlateauTolerance=3", "# --- Quality and bitrate ---", "# Sharpening applied",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q ist verschwunden:\n%s", kept, got)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("es blieben doppelte Leerzeilen zurück:\n%s", got)
	}

	// Eine Datei ohne alte Schlüssel bleibt unberührt.
	if again, removedAgain := removeRetiredEntries(got, retiredConfigKeys); again != got || len(removedAgain) != 0 {
		t.Error("ein zweiter Durchlauf hat noch etwas geändert")
	}
}

// TestRefreshStaleComments: der Text zu autoCQTargetVMAF nannte den
// weggefallenen Kosten-Deckel — er wird durch den aktuellen ersetzt, der Wert
// des Nutzers bleibt. Texte ohne Verweis bleiben, wie sie sind.
func TestRefreshStaleComments(t *testing.T) {
	got, refreshed := refreshStaleComments(oldINI134, configBlocksFromTemplate(), retiredConfigKeys)

	if len(refreshed) != 1 || refreshed[0] != "autoCQTargetVMAF" {
		t.Fatalf("aufgefrischt: %v, erwartet genau autoCQTargetVMAF", refreshed)
	}
	if strings.Contains(got, "hand in hand") {
		t.Error("der veraltete Text zum VMAF-Ziel steht noch da")
	}
	if !strings.Contains(got, "The target is a floor") {
		t.Errorf("der neue Text zum VMAF-Ziel fehlt:\n%s", got)
	}
	if !strings.Contains(got, "autoCQTargetVMAF=96.5") {
		t.Error("der Wert des Nutzers wurde verändert")
	}
	// Die kurze Erklärung zu casStrength nennt nichts Entferntes und bleibt.
	if !strings.Contains(got, "# Sharpening applied after downscaling.\n# Allowed") {
		t.Error("eine Erklärung ohne Verweis wurde angefasst")
	}
}

// TestUpdateConfigEntriesMigrates134: eine echte 1.34.0-INI wird in einem
// Zug auf 2.0.0 gebracht — eine Sicherung, alte Schlüssel weg, neuer Schlüssel
// an seiner Stelle, Werte des Nutzers unverändert, und der Parser meldet
// danach nichts mehr.
func TestUpdateConfigEntriesMigrates134(t *testing.T) {
	path := filepath.Join(t.TempDir(), "NVENCForge_Config.ini")
	crlf := strings.ReplaceAll(oldINI134, "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0644); err != nil {
		t.Fatalf("Testdatei nicht schreibbar: %v", err)
	}

	update, err := updateConfigEntries(path)
	if err != nil {
		t.Fatalf("unerwarteter Fehler: %v", err)
	}
	if len(update.removed) != 3 || len(update.refreshed) != 1 || !contains(update.added, "minSavePercent") {
		t.Errorf("Bilanz stimmt nicht: %+v", update)
	}
	backup, err := os.ReadFile(path + configBackupSuffix)
	if err != nil || string(backup) != crlf {
		t.Fatalf("die Sicherung enthält nicht den Stand von vorher (%v)", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Error("in eine CRLF-Datei wurden nackte LF-Zeilen geschrieben")
	}
	// Die Mindestersparnis steht direkt hinter dem VMAF-Ziel, wie in der Vorlage.
	target := strings.Index(got, "autoCQTargetVMAF=96.5")
	minSave := strings.Index(got, "minSavePercent=20")
	cas := strings.Index(got, "casStrength=0.3")
	if target < 0 || minSave < target || cas < minSave {
		t.Errorf("minSavePercent steht nicht hinter dem VMAF-Ziel:\n%s", got)
	}

	parsed, invalids, warns := parseAppConfig(path)
	if len(invalids) > 0 || len(warns) > 0 {
		t.Errorf("nach dem Umzug meldet der Parser noch etwas: %v %v", invalids, warns)
	}
	if parsed.autoCQTargetVMAF != 96.5 || parsed.autoCQPlateauTolerance != 3 || parsed.casStrength != 0.3 {
		t.Errorf("Werte des Nutzers verändert: Ziel %.4g, Plateau %.4g, CAS %.4g",
			parsed.autoCQTargetVMAF, parsed.autoCQPlateauTolerance, parsed.casStrength)
	}

	// Ein zweiter Lauf findet nichts mehr zu tun und legt keine neue Sicherung an.
	if err := os.Remove(path + configBackupSuffix); err != nil {
		t.Fatal(err)
	}
	if again, err := updateConfigEntries(path); err != nil || again.changed() {
		t.Errorf("zweiter Lauf hat noch etwas geändert: %+v (%v)", again, err)
	}
	if _, err := os.Stat(path + configBackupSuffix); err == nil {
		t.Error("ein Lauf ohne Änderung hat eine Sicherung geschrieben")
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

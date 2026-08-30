//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

// Sichert die beiden Punkte ab, die aus dem Doom9-Thread kamen: der Prüfmodus
// -cqcheck muss die Suche selbst einschalten, und die Ergebniszeile muss
// eindeutig sagen, welches CQ benutzt wird.

func TestCQCheckFlagTurnsSearchOn(t *testing.T) {
	// Ohne eingeschaltete Suche gäbe es nichts zu zeigen — -cqcheck muss
	// autoCQ mitziehen, genau wie -cropcheck es mit autoCrop tut.
	cfg := AppConfig{}
	cfg.parseArgs([]string{"-cqcheck"})
	if !cfg.cqCheckOnly {
		t.Error("-cqcheck hat den Prüfmodus nicht gesetzt")
	}
	if !cfg.autoCQ {
		t.Error("-cqcheck muss die Auto-CQ-Suche mit einschalten")
	}
	if cfg.cropCheckOnly {
		t.Error("-cqcheck darf den Crop-Prüfmodus nicht anfassen")
	}
}

func TestCropCheckLeavesCQCheckAlone(t *testing.T) {
	// Gegenprobe: die beiden Prüfmodi dürfen sich nicht gegenseitig setzen.
	cfg := AppConfig{}
	cfg.parseArgs([]string{"-cropcheck"})
	if cfg.cqCheckOnly {
		t.Error("-cropcheck darf den CQ-Prüfmodus nicht einschalten")
	}
	if !cfg.cropCheckOnly || !cfg.autoCrop {
		t.Error("-cropcheck hat seinen eigenen Modus nicht mehr gesetzt")
	}
}

func TestNoAutoCQAfterCQCheckWins(t *testing.T) {
	// Wer beides angibt, meint das letzte: -noautocq schaltet die Suche ab,
	// und ohne Suche ist der Prüfmodus gegenstandslos.
	cfg := AppConfig{}
	cfg.parseArgs([]string{"-cqcheck", "-noautocq"})
	if cfg.autoCQ {
		t.Error("-noautocq nach -cqcheck muss die Suche wieder abschalten")
	}
}

func TestAutoCQNoteText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"leere Notiz bleibt leer", "", ""},
		{"Klammern und Leerzeichen fallen weg",
			" (verified)", "verified"},
		{"der Fall aus dem Forum",
			" (cost cap 45%: CQ 18 would spend 52% of the source — CQ 19 measured 94.0 at 46%)",
			"cost cap 45%: CQ 18 would spend 52% of the source — CQ 19 measured 94.0 at 46%"},
		{"Klammern im Text bleiben stehen",
			" (VMAF tops out at ~90.0 (measured) — target unreachable)",
			"VMAF tops out at ~90.0 (measured) — target unreachable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := autoCQNoteText(c.in); got != c.want {
				t.Errorf("autoCQNoteText(%q) = %q, erwartet %q", c.in, got, c.want)
			}
		})
	}
}

// TestCQCheckDocumented hält die Hilfedatei an das Flag gebunden: ein
// Schalter, den niemand findet, ist keiner. (Lehre aus dem lange
// undokumentierten -json, 2026-08-17.)
func TestCQCheckDocumented(t *testing.T) {
	if !strings.Contains(helpFileContent, "-cqcheck") {
		t.Error("-cqcheck fehlt in der Hilfedatei (helpFileContent)")
	}
}

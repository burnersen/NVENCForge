//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Sichert die Unterscheidung ab, an der die Fehldiagnose aus dem Doom9-Forum
// hing: ein von Windows abgewiesener FFmpeg-Start darf nicht als fehlender
// Codec durchgehen — und ein normaler FFmpeg-Fehler nicht als Systemproblem.

func TestNTStatusFromExitCode(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		wantStatus uint32
		wantIsNT   bool
	}{
		{"Erfolg", 0, 0, false},
		{"gewöhnlicher FFmpeg-Fehler", 1, 0, false},
		{"durch Signal beendet", -1, 0, false},
		{"knapp unter der Grenze", 0xBFFFFFFF, 0, false},
		{"genau auf der Grenze", 0xC0000000, 0xC0000000, true},
		{"der gemeldete Fall", 0xC0000139, 0xC0000139, true},
		{"fehlende DLL", 0xC0000135, 0xC0000135, true},
		{"größter möglicher Code", 0xFFFFFFFF, 0xFFFFFFFF, true},
		{"jenseits von uint32", 0x1_0000_0000, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, isNT := ntStatusFromExitCode(c.code)
			if isNT != c.wantIsNT || status != c.wantStatus {
				t.Errorf("ntStatusFromExitCode(%#x) = %#x, %v — erwartet %#x, %v",
					c.code, status, isNT, c.wantStatus, c.wantIsNT)
			}
		})
	}
}

func TestDescribeNTStatus(t *testing.T) {
	known := describeNTStatus(0xC0000139)
	if !strings.Contains(known, "0xC0000139") {
		t.Errorf("Der Code selbst fehlt in der Meldung: %q", known)
	}
	if !strings.Contains(known, "STATUS_ENTRYPOINT_NOT_FOUND") {
		t.Errorf("Der bekannte Name fehlt in der Meldung: %q", known)
	}

	// Für unbekannte Codes wird kein Name erfunden, die Zahl steht trotzdem da.
	unknown := describeNTStatus(0xC0000ABC)
	if !strings.Contains(unknown, "0xC0000ABC") {
		t.Errorf("Der Code selbst fehlt in der Meldung: %q", unknown)
	}
	if strings.Contains(unknown, "STATUS_") {
		t.Errorf("Für einen unbekannten Code wurde ein Name geraten: %q", unknown)
	}
}

// TestNTStatusExitCode prüft die Kette bis zum echten Prozessfehler: nur ein
// Tabellentest würde offenlassen, ob exec.ExitError überhaupt so ausgelesen
// wird, wie die Regel es annimmt.
func TestNTStatusExitCode(t *testing.T) {
	cases := []struct {
		name       string
		exitCode   int
		wantStatus uint32
		wantIsNT   bool
	}{
		{"gewöhnlicher Fehler bleibt gewöhnlich", 1, 0, false},
		{"Windows-Abbruch wird erkannt", 0xC0000139, 0xC0000139, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := runExitHelper(t, c.exitCode)
			if err == nil {
				t.Fatalf("Hilfsprozess sollte mit %#x scheitern, lief aber durch", c.exitCode)
			}
			status, isNT := ntStatusExitCode(err)
			if isNT != c.wantIsNT || status != c.wantStatus {
				t.Errorf("ntStatusExitCode = %#x, %v — erwartet %#x, %v",
					status, isNT, c.wantStatus, c.wantIsNT)
			}
		})
	}
}

// TestFFmpegStartFailureRecognisesSystemAbort deckt den Weg ab, den der
// gemeldete Fall genommen hätte: der Probelauf stirbt mit einem NTSTATUS-Code,
// und ffmpegStartFailure meldet ein Systemproblem, ohne FFmpeg zu befragen.
func TestFFmpegStartFailureRecognisesSystemAbort(t *testing.T) {
	err := runExitHelper(t, 0xC0000139)
	if err == nil {
		t.Fatal("Hilfsprozess lief unerwartet durch")
	}
	reason := ffmpegStartFailure(err)
	if reason == "" {
		t.Fatal("Ein Windows-Abbruch wurde nicht als Systemproblem erkannt")
	}
	if !strings.Contains(reason, "0xC0000139") {
		t.Errorf("Die Meldung nennt den Abbruchcode nicht: %q", reason)
	}
}

// avErrorExternal ist der Exit-Code aus #8 (GTX 1650): AVERROR_EXTERNAL
// (-542398533), wie Windows ihn als vorzeichenlose Zahl ausliest. FFmpeg
// endet damit, wenn hevc_nvenc die Einstellung ablehnt ("No capable devices
// found"), und der Wert liegt oberhalb von winNTStatusFailureBase.
const avErrorExternal = 0xDFABA7BB

// TestFFmpegStartVerdictEncoderErrorIsNotSystemAbort ist der gemeldete Fall
// selbst: der Probelauf endet mit einem hohen Code, FFmpeg startet aber. Das
// ist ein Encoder-Fehler, und der Aufrufer muss bei seiner eigenen Meldung
// bleiben — sonst kommt die Abstufung für ältere Karten nie zum Zug.
func TestFFmpegStartVerdictEncoderErrorIsNotSystemAbort(t *testing.T) {
	err := runExitHelper(t, avErrorExternal)
	if err == nil {
		t.Fatal("Hilfsprozess lief unerwartet durch")
	}
	if _, looksLikeNT := ntStatusExitCode(err); !looksLikeNT {
		t.Fatal("Voraussetzung verfehlt: der Code sollte oberhalb der NTSTATUS-Grenze liegen")
	}
	asked := false
	reason := ffmpegStartVerdict(err, func() error {
		asked = true
		return nil
	})
	if !asked {
		t.Error("Bei einem unbekannten hohen Code wurde FFmpeg nicht gegengeprüft")
	}
	if reason != "" {
		t.Errorf("Ein Encoder-Fehler wurde als Systemabbruch gemeldet: %q", reason)
	}
}

// TestFFmpegStartVerdictKnownLoaderFailureSkipsVersionCheck hält fest, dass
// der eindeutige Fall weiterhin nichts kostet: ein bekannter Ladefehler
// braucht keine Gegenprobe.
func TestFFmpegStartVerdictKnownLoaderFailureSkipsVersionCheck(t *testing.T) {
	err := runExitHelper(t, 0xC0000139)
	if err == nil {
		t.Fatal("Hilfsprozess lief unerwartet durch")
	}
	reason := ffmpegStartVerdict(err, func() error {
		t.Error("Für einen bekannten Ladefehler wurde FFmpeg unnötig gestartet")
		return nil
	})
	if !strings.Contains(reason, "STATUS_ENTRYPOINT_NOT_FOUND") {
		t.Errorf("Der bekannte Ladefehler wurde nicht gemeldet: %q", reason)
	}
}

// TestFFmpegStartVerdictVersionCheckDecides deckt die andere Seite ab: ein
// hoher Code, bei dem auch 'ffmpeg -version' scheitert, ist ein echtes
// Startproblem, ob mit Abbruchcode oder ganz ohne Antwort.
func TestFFmpegStartVerdictVersionCheckDecides(t *testing.T) {
	probeErr := runExitHelper(t, avErrorExternal)
	if probeErr == nil {
		t.Fatal("Hilfsprozess lief unerwartet durch")
	}

	aborted := ffmpegStartVerdict(probeErr, func() error {
		return runExitHelper(t, 0xC0000005)
	})
	if !strings.Contains(aborted, "0xC0000005") {
		t.Errorf("Der Abbruchcode der Gegenprobe fehlt in der Meldung: %q", aborted)
	}

	silent := ffmpegStartVerdict(probeErr, func() error {
		return errors.New("file does not exist")
	})
	if !strings.Contains(silent, "does not even answer") {
		t.Errorf("Ein FFmpeg ohne Antwort wurde nicht gemeldet: %q", silent)
	}
}

// runExitHelper startet die Testdatei selbst noch einmal als Unterprozess, der
// sich sofort mit dem gewünschten Code beendet. Das ist der einzige Weg, an
// einen echten *exec.ExitError zu kommen, ohne FFmpeg zu brauchen.
func runExitHelper(t *testing.T, exitCode int) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExit$")
	cmd.Env = append(os.Environ(), exitHelperEnv+"="+strconv.Itoa(exitCode))
	return cmd.Run()
}

// exitHelperEnv schaltet den Hilfsprozess scharf. Ohne diese Variable ist
// TestHelperExit ein übersprungener Test wie jeder andere.
const exitHelperEnv = "NVENCFORGE_TEST_EXIT_CODE"

// TestHelperExit ist kein Test, sondern der Rumpf des Unterprozesses aus
// runExitHelper.
func TestHelperExit(t *testing.T) {
	raw := os.Getenv(exitHelperEnv)
	if raw == "" {
		t.Skip("Hilfsprozess für TestNTStatusExitCode — läuft nur als Unterprozess")
	}
	code, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s enthält keine Zahl: %q", exitHelperEnv, raw)
	}
	os.Exit(code)
}

//go:build windows && amd64

// NVENCForge (https://github.com/burnersen/NVENCForge)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

// resetPromptInput lässt den einen Leser der Eingabe neu entstehen — für
// Prüfungen, die os.Stdin umleiten. Danach gilt wieder der Zustand von vorher.
func resetPromptInput(t *testing.T) {
	t.Helper()
	promptInputMu.Lock()
	prev := promptInputSource
	promptInputSource = nil
	promptInputMu.Unlock()
	t.Cleanup(func() {
		promptInputMu.Lock()
		promptInputSource = prev
		promptInputMu.Unlock()
	})
}

// waitForPendingLine wartet, bis der Leser eine Zeile weitergereicht hat. Ohne
// das wäre "sie liegt schon da" in den Prüfungen unten Glückssache.
func waitForPendingLine(t *testing.T, source *lineSource) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(source.lines) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("die Zeile ist beim Leser nie angekommen")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLateAnswerIsNotTheNextAnswer ist der Fehler, um den es geht: Eine Frage
// läuft ab, die Antwort kommt zu spät. Bis 2.2.3 nahm der stehen gebliebene
// Leser der alten Frage dann die Antwort auf die NÄCHSTE weg. Jetzt wird die
// verspätete Zeile vor der nächsten Frage verworfen, und deren Antwort kommt an.
func TestLateAnswerIsNotTheNextAnswer(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	source := newLineSource(reader)

	if line, ok := source.readLine(30*time.Millisecond, true); ok {
		t.Fatalf("ohne Eingabe darf keine Antwort kommen, bekommen %q", line)
	}

	// Die verspätete Antwort auf die abgelaufene Frage.
	go func() { _, _ = io.WriteString(writer, "late\n") }()
	waitForPendingLine(t, source)

	// Nächste Frage: erst verwerfen, dann ankündigen, dann lesen.
	source.discardPending()
	go func() { _, _ = io.WriteString(writer, "1,3\n") }()
	line, ok := source.readLine(2*time.Second, true)
	if !ok || strings.TrimSpace(line) != "1,3" {
		t.Errorf("die Antwort auf die zweite Frage ging verloren: %q (ok=%v)", line, ok)
	}
}

// TestEveryLineReachesAReader: Auch ohne Verwerfen geht keine Zeile mehr
// verloren — genau das war der alte Fehler, nur von der anderen Seite. Zwei
// Fragen nacheinander bekommen zwei Zeilen in der Reihenfolge der Eingabe.
func TestEveryLineReachesAReader(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	source := newLineSource(reader)

	go func() { _, _ = io.WriteString(writer, "first\nsecond\n") }()
	for _, want := range []string{"first", "second"} {
		line, ok := source.readLine(2*time.Second, true)
		if !ok || strings.TrimSpace(line) != want {
			t.Fatalf("erwartet %q, bekommen %q (ok=%v)", want, line, ok)
		}
	}
}

// TestClosedInputEndsTheWait: Ist die Eingabe gar nicht lesbar, darf eine
// Frage ohne Zeitlimit nicht ewig stehen. Mit Zeitlimit entscheidet der
// Aufrufer, ob trotzdem die volle Bedenkzeit läuft.
func TestClosedInputEndsTheWait(t *testing.T) {
	reader, writer := io.Pipe()
	source := newLineSource(reader)
	_ = writer.Close()

	done := make(chan bool, 1)
	go func() {
		_, ok := source.readLine(0, true)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("eine geschlossene Eingabe ist keine Antwort")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ohne Zeitlimit blieb die Frage an der geschlossenen Eingabe hängen")
	}

	// Ohne Aufgeben läuft die Bedenkzeit ab, statt sofort zu enden.
	start := time.Now()
	if _, ok := source.readLine(80*time.Millisecond, false); ok {
		t.Error("eine geschlossene Eingabe ist keine Antwort")
	}
	if waited := time.Since(start); waited < 60*time.Millisecond {
		t.Errorf("die Bedenkzeit lief nicht ab: nur %v gewartet", waited)
	}
}

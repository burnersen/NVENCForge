//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import "testing"

// Sichert die letzte Station des gedeckelten Step-Downs ab. Sie war von 1.6.1
// bis 1.30.0 kaputt: beim Umbau in Commit 85bf503 ging ein "} else {" verloren,
// wodurch im Fall "es geht noch eine Stufe tiefer" GAR NICHTS zugewiesen wurde
// und der interpolierte CQ stehen blieb — obwohl zwei Messungen ihn schon
// widerlegt hatten. Die Datei bekam dadurch stillschweigend zu wenig Qualität.

func TestAutoCQFinalStepPick(t *testing.T) {
	cases := []struct {
		name         string
		stepped      int
		final        int
		remeasured   float64
		finalPred    float64
		wantCQ       int
		wantVMAF     float64
		wantMeasured bool // erwartet der Fall den echten Messwert?
	}{
		{
			name: "Klemmgrenze erreicht: der Messwert zählt",
			// autoCQStepDown konnte nicht weiter — final == stepped. Dann ist
			// remeasured eine ECHTE Messung an genau diesem CQ.
			stepped: 20, final: 20, remeasured: 93.4, finalPred: 94.9,
			wantCQ: 20, wantVMAF: 93.4, wantMeasured: true,
		},
		{
			name: "eine Stufe tiefer: dieser Schritt muss ankommen",
			// Der Fall, der vorher unter den Tisch fiel.
			stepped: 24, final: 22, remeasured: 94.8, finalPred: 96.6,
			wantCQ: 22, wantVMAF: 96.6, wantMeasured: false,
		},
		{
			name:    "mehrere Stufen tiefer",
			stepped: 30, final: 25, remeasured: 90.1, finalPred: 96.5,
			wantCQ: 25, wantVMAF: 96.5, wantMeasured: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cq, vmaf := autoCQFinalStepPick(c.stepped, c.final, c.remeasured, c.finalPred)
			if cq != c.wantCQ || vmaf != c.wantVMAF {
				t.Errorf("autoCQFinalStepPick(%d, %d, %.1f, %.1f) = CQ %d / %.1f — erwartet CQ %d / %.1f",
					c.stepped, c.final, c.remeasured, c.finalPred, cq, vmaf, c.wantCQ, c.wantVMAF)
			}
			if c.wantMeasured && vmaf != c.remeasured {
				t.Errorf("an der Klemmgrenze muss der Messwert %.1f gelten, nicht %.1f",
					c.remeasured, vmaf)
			}
		})
	}
}

// TestAutoCQFinalStepNeverKeepsRefutedPick ist der eigentliche Regressionstest:
// Das Ergebnis darf NIE der Wert sein, den die Suche gerade verworfen hat.
func TestAutoCQFinalStepNeverKeepsRefutedPick(t *testing.T) {
	// So sah der Fall aus, der den Fehler sichtbar machte: die Verifikation bei
	// CQ 26 verfehlte das Ziel, der Sprung auf CQ 24 war gedeckelt, die
	// Nachmessung dort verfehlte es ebenfalls, CQ 22 wäre die Folge.
	const refuted = 26 // der interpolierte Pick, durch zwei Messungen widerlegt
	stepped, final := 24, 22

	cq, _ := autoCQFinalStepPick(stepped, final, 94.8, 96.6)
	if cq == refuted {
		t.Fatalf("der widerlegte CQ %d wurde behalten", refuted)
	}
	if cq != final {
		t.Errorf("erwartet wurde der zuletzt berechnete CQ %d, bekam %d", final, cq)
	}
}

// TestAutoCQStepDownReachesTheBrokenPath belegt, dass der reparierte Zweig
// überhaupt erreichbar ist — ein Fix für einen toten Pfad wäre keiner.
func TestAutoCQStepDownReachesTheBrokenPath(t *testing.T) {
	sc := hevcAutoCQScale
	// Steile Kurve, Ziel weit unter dem Messwert: der Sprung wird gedeckelt.
	stepped, _, capped := autoCQStepDown(sc, 26, 97.0, 88.0, -0.5)
	if !capped {
		t.Fatal("der Aufbau trifft den gedeckelten Zweig nicht — Test wertlos")
	}
	// Von dort aus muss ein WEITERER Schritt möglich sein (final != stepped),
	// sonst käme der reparierte else-Zweig nie zum Zug.
	final, _, _ := autoCQStepDown(sc, stepped, 97.0, 90.0, -0.5)
	if final == stepped {
		t.Errorf("kein weiterer Schritt möglich (stepped %d, final %d) — der Zweig wäre unerreichbar",
			stepped, final)
	}
	if final >= stepped {
		t.Errorf("ein Schritt nach unten muss ein kleineres CQ liefern: stepped %d, final %d",
			stepped, final)
	}
}

//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"errors"
	"math"
	"testing"
)

// Das Ziel ist seit 2.0.0 eine Untergrenze (CloudForge-Regel): eine Wahl, die
// es verfehlt, wird nicht mehr auf einer Schätzung weiter nach unten
// geschoben, sondern so lange gemessen, bis ein CQ es wirklich hält. Diese
// Tests spielen die Suche an erfundenen Kurven durch — ohne FFmpeg.

// fakeCurve ist eine erfundene VMAF-Kurve samt Zähler: scoreAt liest aus ihr,
// merkt sich jede Messung in scores (wie autoDetectCQ) und zählt, wie viele
// Messungen wirklich nötig waren.
type fakeCurve struct {
	values   map[int]float64
	scores   map[int]float64
	measured []int
	failAt   int // diese Stufe scheitert beim Messen (0 = keine)
}

func newFakeCurve(values map[int]float64, known ...int) *fakeCurve {
	f := &fakeCurve{values: values, scores: map[int]float64{}}
	for _, cq := range known {
		f.scores[cq] = values[cq]
	}
	return f
}

func (f *fakeCurve) scoreAt(cq int) (float64, error) {
	if score, ok := f.scores[cq]; ok {
		return score, nil
	}
	if cq == f.failAt {
		return 0, errors.New("sample encode failed")
	}
	score, ok := f.values[cq]
	if !ok {
		return 0, errors.New("no value on the fake curve")
	}
	f.scores[cq] = score
	f.measured = append(f.measured, cq)
	return score, nil
}

func TestAutoCQHoldTargetNarrowsTheGap(t *testing.T) {
	// H.265, Ziel 97. Die Wahl CQ 28 misst 96,8 — knapp darunter. Der untere
	// Anker CQ 26 hält das Ziel, also liegt der gesuchte CQ dazwischen.
	curve := newFakeCurve(map[int]float64{26: 97.4, 27: 97.1, 28: 96.8, 29: 96.4, 30: 96.0}, 26, 30, 28)
	hold := autoCQHoldTarget(hevcAutoCQScale, 97, 97.4, -0.35,
		autoCQPoint{28, 96.8}, curve.scores, curve.scoreAt)

	if hold.outcome != holdReached || hold.point.cq != 27 {
		t.Fatalf("got outcome %d at CQ %d, want CQ 27 (the thriftiest that holds 97)",
			hold.outcome, hold.point.cq)
	}
	if hold.point.vmaf < 97 {
		t.Errorf("the chosen CQ measured %.2f — under the target", hold.point.vmaf)
	}
	if len(curve.measured) != 1 {
		t.Errorf("needed %d measurements %v, one is enough here", len(curve.measured), curve.measured)
	}
}

// TestAutoCQHoldTargetDoesNotCreep: ein weit entfernter Fehlpunkt (CQ 44) hält
// die Rechnung fest, die Kurve fällt erst kurz davor steil ab. Ohne die
// Illinois-Korrektur kriecht die Suche Stufe für Stufe heran (29, 30, 31, 32,
// 33, 34 — genau so von CloudForge am 26.09.2026 gemessen), mit ihr springt sie.
func TestAutoCQHoldTargetDoesNotCreep(t *testing.T) {
	curve := newFakeCurve(map[int]float64{
		28: 97.9, 29: 97.8, 30: 97.6, 31: 97.4, 32: 97.2, 33: 97.05, 34: 96.8, 44: 85.0,
	}, 28, 44)
	hold := autoCQHoldTarget(av1AutoCQScale, 97, 97.9, -0.8,
		autoCQPoint{44, 85.0}, curve.scores, curve.scoreAt)

	if hold.outcome != holdReached || hold.point.cq != 33 {
		t.Fatalf("got outcome %d at CQ %d, want CQ 33 (97.05 holds, 34 does not)",
			hold.outcome, hold.point.cq)
	}
	if len(curve.measured) > 4 {
		t.Errorf("needed %d measurements %v — the search crept up one step at a time",
			len(curve.measured), curve.measured)
	}
}

func TestAutoCQHoldTargetStepsBelowTheAnchor(t *testing.T) {
	// AV1, Ziel 97, beide Anker darunter: erreichbar nur unter dem unteren
	// Anker. Jeder Schritt dorthin wird gemessen, und am Ende wird die Lücke
	// zwischen "hält" und "verfehlt" geschlossen.
	curve := newFakeCurve(map[int]float64{
		18: 97.6, 20: 97.3, 21: 97.2, 22: 97.02, 23: 96.75, 24: 96.6, 32: 94.6,
	}, 24, 32, 23)
	hold := autoCQHoldTarget(av1AutoCQScale, 97, 96.6, -0.25,
		autoCQPoint{23, 96.75}, curve.scores, curve.scoreAt)

	if hold.outcome != holdReached || hold.point.cq != 22 {
		t.Fatalf("got outcome %d at CQ %d, want CQ 22 (97.02 holds, 23 does not)",
			hold.outcome, hold.point.cq)
	}
	for _, cq := range curve.measured {
		if _, known := curve.values[cq]; !known {
			t.Errorf("measured CQ %d that is not on the curve", cq)
		}
	}
}

func TestAutoCQHoldTargetClampFloor(t *testing.T) {
	// Ziel 99 ist mit diesem Material nicht zu schaffen — auch nicht an der
	// Klemmgrenze. Das ist bewiesen unerreichbar, und die Wahl liegt auf dem
	// GEMESSENEN Wert dort, nicht auf einer Schätzung.
	curve := newFakeCurve(map[int]float64{
		20: 98.0, 21: 97.67, 22: 97.33, 23: 97.0, 24: 96.67, 26: 96.0, 30: 94.8,
	}, 26, 30, 24)
	hold := autoCQHoldTarget(hevcAutoCQScale, 99, 96.0, -0.3,
		autoCQPoint{24, 96.67}, curve.scores, curve.scoreAt)

	if hold.outcome != holdClampFloor || hold.point.cq != hevcAutoCQScale.clampMin {
		t.Fatalf("got outcome %d at CQ %d, want the clamp floor %d",
			hold.outcome, hold.point.cq, hevcAutoCQScale.clampMin)
	}
	if hold.point.vmaf != 98.0 {
		t.Errorf("the clamp floor must carry its measurement 98.0, got %.2f", hold.point.vmaf)
	}
}

func TestAutoCQHoldTargetBrakes(t *testing.T) {
	cases := []struct {
		name  string
		curve map[int]float64
		want  autoCQHoldOutcome
	}{
		// Tote Kurve unter dem Anker: 0,04 VMAF je Stufe — Mehr Bitrate kauft
		// keine Qualität (Sättigungsbremse, H.265-Schwelle 0,10).
		{"saturated", map[int]float64{23: 96.12, 26: 96.0, 30: 94.8}, holdSaturated},
		// 0,2 VMAF je Stufe: das Ziel wäre weiter unten erreichbar, aber jede
		// Stufe ist ihren Preis nicht wert (Sparbremse, H.265-Schwelle 0,30).
		{"too expensive", map[int]float64{23: 96.6, 26: 96.0, 30: 94.8}, holdTooExpensive},
	}
	for _, c := range cases {
		curve := newFakeCurve(c.curve, 26, 30)
		hold := autoCQHoldTarget(hevcAutoCQScale, 97, 96.0, -0.3,
			autoCQPoint{26, 96.0}, curve.scores, curve.scoreAt)
		if hold.outcome != c.want || hold.point.cq != 23 {
			t.Errorf("%s: got outcome %d at CQ %d, want outcome %d at CQ 23",
				c.name, hold.outcome, hold.point.cq, c.want)
		}
	}
}

func TestAutoCQHoldTargetMeasurementFails(t *testing.T) {
	// Scheitert eine Messung, während noch kein Punkt das Ziel hält, bleibt die
	// beste Messung stehen — ohne Schätzung dazuzuerfinden.
	curve := newFakeCurve(map[int]float64{23: 96.8, 26: 96.3, 30: 95.0}, 26, 30)
	curve.failAt = 23
	hold := autoCQHoldTarget(hevcAutoCQScale, 97, 96.3, -0.325,
		autoCQPoint{26, 96.3}, curve.scores, curve.scoreAt)
	if hold.outcome != holdGaveUp || hold.point.cq != 26 {
		t.Errorf("got outcome %d at CQ %d, want holdGaveUp at the last measured CQ 26",
			hold.outcome, hold.point.cq)
	}

	// Scheitert sie erst beim Eingrenzen, gilt der gemessene Punkt, der das
	// Ziel hält.
	curve = newFakeCurve(map[int]float64{26: 97.4, 27: 97.1, 28: 96.8, 30: 96.0}, 26, 30, 28)
	curve.failAt = 27
	hold = autoCQHoldTarget(hevcAutoCQScale, 97, 97.4, -0.35,
		autoCQPoint{28, 96.8}, curve.scores, curve.scoreAt)
	if hold.outcome != holdReached || hold.point.cq != 26 {
		t.Errorf("got outcome %d at CQ %d, want the measured CQ 26 that holds the target",
			hold.outcome, hold.point.cq)
	}
}

func TestAutoCQHoldTargetStepLimit(t *testing.T) {
	// Eine Kurve, die das Ziel nie erreicht und nur in Einzelschritten
	// weiterkommt: nach autoCQMaxHoldSteps Messungen ist Schluss.
	sc := hevcAutoCQScale
	sc.clampMin, sc.maxStepDown = 1, 1
	values := map[int]float64{30: 94.6}
	for cq := 26; cq >= 10; cq-- {
		values[cq] = 96.0 + 0.35*float64(26-cq)
	}
	curve := newFakeCurve(values, 26, 30)
	hold := autoCQHoldTarget(sc, 99.9, 96.0, -0.35,
		autoCQPoint{26, 96.0}, curve.scores, curve.scoreAt)

	if hold.outcome != holdGaveUp {
		t.Errorf("got outcome %d, want holdGaveUp once the step limit is spent", hold.outcome)
	}
	if len(curve.measured) != autoCQMaxHoldSteps {
		t.Errorf("measured %d steps, want the limit %d", len(curve.measured), autoCQMaxHoldSteps)
	}
}

func TestAutoCQThriftyStep(t *testing.T) {
	cases := []struct {
		name      string
		pick      autoCQPoint
		next      float64 // Messwert der nächsten Stufe
		fail      bool    // die Messung der nächsten Stufe scheitert
		wantCQ    int
		wantTaken bool
		wantProbe bool // wurde überhaupt gemessen?
	}{
		{"clear hit, next step holds", autoCQPoint{28, 97.8}, 97.3, false, 29, true, true},
		{"clear hit, next step misses", autoCQPoint{28, 97.8}, 96.9, false, 28, false, true},
		// Rauschen: die sparsamere Stufe misst sogar besser — sie hält das Ziel
		// und ist kleiner, also wird sie genommen.
		{"next step measures better", autoCQPoint{28, 97.8}, 97.9, false, 29, true, true},
		{"within the margin, no probe", autoCQPoint{28, 97.4}, 97.3, false, 28, false, false},
		{"measurement fails", autoCQPoint{28, 97.8}, 0, true, 28, false, true},
		{"already at the clamp ceiling", autoCQPoint{34, 98.0}, 97.9, false, 34, false, false},
	}
	for _, c := range cases {
		probed := false
		scoreAt := func(cq int) (float64, error) {
			probed = true
			if cq != c.pick.cq+1 {
				t.Errorf("%s: probed CQ %d, want exactly one step thriftier (%d)", c.name, cq, c.pick.cq+1)
			}
			if c.fail {
				return 0, errors.New("sample encode failed")
			}
			return c.next, nil
		}
		got, taken := autoCQThriftyStep(hevcAutoCQScale, 97, c.pick, scoreAt)
		if got.cq != c.wantCQ || taken != c.wantTaken || probed != c.wantProbe {
			t.Errorf("%s: got CQ %d taken=%v probed=%v, want CQ %d taken=%v probed=%v",
				c.name, got.cq, taken, probed, c.wantCQ, c.wantTaken, c.wantProbe)
		}
	}
}

func TestAutoCQBetween(t *testing.T) {
	cases := []struct {
		name         string
		above, below autoCQPoint
		want         int
	}{
		{"straight line crosses at 27", autoCQPoint{26, 97.4}, autoCQPoint{28, 96.8}, 27},
		{"crossing near the miss", autoCQPoint{20, 98.0}, autoCQPoint{30, 96.8}, 28},
		// Rechnerisch läge der Treffer auf dem Punkt darüber — die Klemme hält
		// jeden neuen Wert streng dazwischen, sonst würde nichts eingegrenzt.
		{"clamped away from the point above", autoCQPoint{20, 97.01}, autoCQPoint{30, 90.0}, 21},
		// Keine fallende Kurve (Rauschen): dann die Mitte.
		{"no drop, midpoint", autoCQPoint{20, 96.9}, autoCQPoint{30, 97.2}, 25},
	}
	for _, c := range cases {
		if got := autoCQBetween(c.above, c.below, 97); got != c.want {
			t.Errorf("%s: got CQ %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAutoCQLocalSlope(t *testing.T) {
	scores := map[int]float64{24: 96.6, 32: 94.6, 22: 97.0}
	// Der nächste gemessene Nachbar über CQ 20 ist 22, nicht der Anker 24.
	if got := autoCQLocalSlope(scores, autoCQPoint{20, 97.4}, -0.25); math.Abs(got+0.2) > 1e-9 {
		t.Errorf("slope to the nearest neighbour = %.4f, want -0.2", got)
	}
	// Ohne Nachbarn darüber gilt die Ersatzsteigung.
	if got := autoCQLocalSlope(scores, autoCQPoint{40, 90}, -0.25); got != -0.25 {
		t.Errorf("no neighbour: got %.4f, want the fallback -0.25", got)
	}
	// Eine steigende "Kurve" (Rauschen) taugt nicht als Steigung.
	if got := autoCQLocalSlope(map[int]float64{24: 97.5}, autoCQPoint{22, 97.0}, -0.25); got != -0.25 {
		t.Errorf("rising curve: got %.4f, want the fallback -0.25", got)
	}
}

func TestAutoCQThriftiestHolding(t *testing.T) {
	scores := map[int]float64{24: 97.6, 26: 97.1, 28: 96.9, 30: 95.0}
	got, found := autoCQThriftiestHolding(scores, 97)
	if !found || got.cq != 26 || got.vmaf != 97.1 {
		t.Errorf("got CQ %d (%.1f, found=%v), want CQ 26 — the highest CQ that holds 97",
			got.cq, got.vmaf, found)
	}
	if _, found := autoCQThriftiestHolding(scores, 98); found {
		t.Error("no measured point holds 98, nothing may be found")
	}
}

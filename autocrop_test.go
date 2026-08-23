//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

// TestParseCropDetectTakesLastResult sichert die eine Eigenschaft, auf die es
// beim Auslesen ankommt: cropdetect verfeinert seinen Vorschlag über das
// Fenster hinweg, maßgeblich ist deshalb die LETZTE Zeile. Wer die erste nähme,
// bekäme das Ergebnis nach einem einzigen Bild.
func TestParseCropDetectTakesLastResult(t *testing.T) {
	out := `[Parsed_cropdetect_0 @ 0x1] x1:0 x2:1919 crop=1920:1040:0:20
[Parsed_cropdetect_0 @ 0x1] x1:0 x2:1919 crop=1920:816:0:132`
	got, ok := parseCropDetect(out)
	if !ok {
		t.Fatal("gültige Ausgabe wurde nicht erkannt")
	}
	want := cropRect{w: 1920, h: 816, x: 0, y: 132}
	if got != want {
		t.Errorf("parseCropDetect = %+v, erwartet %+v", got, want)
	}
}

// TestParseCropDetectRejectsGarbage: bei reinem Schwarz im Fenster meldet
// cropdetect "crop=-1:-1:-1:-1". Rutscht das als gültiges Rechteck durch,
// baut die Filterkette anschließend Unsinn.
func TestParseCropDetectRejectsGarbage(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"nur Schwarz", "[cropdetect] crop=-1:-1:-1:-1"},
		{"gar keine Ausgabe", ""},
		{"kein crop enthalten", "frame= 120 fps=60 q=-0.0 size=N/A"},
		{"Breite null", "[cropdetect] crop=0:816:0:132"},
	}
	for _, c := range cases {
		if r, ok := parseCropDetect(c.out); ok {
			t.Errorf("%s: als gültig durchgelassen (%+v)", c.name, r)
		}
	}
}

// TestCropOutlierIsOutvoted ist der Fall, der den Nutzer 2026-08-23 einen
// schiefen Film gekostet hat: im Testfilm "Exodus" blitzt in EINER von neun
// Proben ein Copyright-Logo im unteren Balken auf. Früher genügte das, um den
// unteren Schnitt um 210 Pixel zu verkürzen — oben wurde geschnitten, unten
// blieb ein dicker Balken stehen. Die Mehrheit muss den Ausreißer überstimmen.
func TestCropOutlierIsOutvoted(t *testing.T) {
	const w, h = 3840, 2160
	samples := make([]cropRect, 0, 9)
	for i := 0; i < 8; i++ {
		samples = append(samples, cropRect{w: w, h: 1600, x: 0, y: 280})
	}
	// Die Probe mit dem Logo: unten bleiben nur 70 px Rand.
	samples = append(samples, cropRect{w: w, h: 1810, x: 0, y: 280})

	got, why := symmetricCropFromSamples(samples, w, h)
	if why != "" {
		t.Fatalf("unerwartet abgelehnt: %s", why)
	}
	want := cropRect{w: w, h: 1600, x: 0, y: 280}
	if got != want {
		t.Errorf("= %+v, erwartet %+v — ein einzelnes Logo darf nicht entscheiden", got, want)
	}
}

// TestCropResultIsAlwaysSymmetric: Letterbox sitzt mittig. Ein Ergebnis, das
// oben mehr wegnimmt als unten, ist immer ein Fehler — auch wenn die Proben
// sich uneinig sind, muss das Bild in der Mitte bleiben.
func TestCropResultIsAlwaysSymmetric(t *testing.T) {
	const w, h = 1920, 1080
	// Leicht unterschiedliche Ränder, alle innerhalb der Toleranz:
	// oben 132/134, unten 136/134 — typische Rundungsreste von cropdetect.
	samples := []cropRect{
		{w: w, h: 812, x: 0, y: 132},
		{w: w, h: 810, x: 0, y: 134},
		{w: w, h: 808, x: 0, y: 134},
		{w: w, h: 812, x: 0, y: 132},
		{w: w, h: 810, x: 0, y: 134},
	}
	got, why := symmetricCropFromSamples(samples, w, h)
	if why != "" {
		t.Fatalf("unerwartet abgelehnt: %s", why)
	}
	top, bottom := got.y, h-got.h-got.y
	if top != bottom {
		t.Errorf("oben %d, unten %d — der Schnitt muss symmetrisch sein (%+v)", top, bottom, got)
	}
	left, right := got.x, w-got.w-got.x
	if left != right {
		t.Errorf("links %d, rechts %d — der Schnitt muss symmetrisch sein (%+v)", left, right, got)
	}
}

// TestCropChangingFormatIsLeftAlone ist die Notbremse: Filme mit wechselndem
// Bildformat (die IMAX-Szenen in "Dark Knight" oder "Interstellar") haben in
// einem Teil des Films wirklich ein höheres Bild. Hier darf NICHT geschnitten
// werden — sonst gehen genau diese Szenen oben und unten verloren.
func TestCropChangingFormatIsLeftAlone(t *testing.T) {
	const w, h = 3840, 2160
	samples := []cropRect{
		{w: w, h: 1600, x: 0, y: 280}, // 2.40:1
		{w: w, h: 1600, x: 0, y: 280},
		{w: w, h: 1600, x: 0, y: 280},
		{w: w, h: 1600, x: 0, y: 280},
		{w: w, h: 1600, x: 0, y: 280},
		{w: w, h: 2020, x: 0, y: 70}, // IMAX-Szene, deutlich höher
		{w: w, h: 2020, x: 0, y: 70},
		{w: w, h: 2020, x: 0, y: 70},
		{w: w, h: 2020, x: 0, y: 70},
	}
	got, why := symmetricCropFromSamples(samples, w, h)
	if why == "" {
		t.Errorf("wurde geschnitten (%+v) — bei wechselndem Bildformat muss die "+
			"Erkennung die Finger stillhalten", got)
	}
	if !got.isFullFrame(w, h) {
		t.Errorf("= %+v, erwartet das Vollbild", got)
	}
}

// TestCropSidesAreDetected: Pillarbox (Balken links und rechts) muss genauso
// erkannt werden wie Letterbox — 4:3-Material in einem 16:9-Rahmen.
func TestCropSidesAreDetected(t *testing.T) {
	const w, h = 1920, 1080
	samples := make([]cropRect, 0, 5)
	for i := 0; i < 5; i++ {
		samples = append(samples, cropRect{w: 1440, h: h, x: 240, y: 0})
	}
	got, why := symmetricCropFromSamples(samples, w, h)
	if why != "" {
		t.Fatalf("unerwartet abgelehnt: %s", why)
	}
	want := cropRect{w: 1440, h: h, x: 240, y: 0}
	if got != want {
		t.Errorf("= %+v, erwartet %+v", got, want)
	}
}

// TestCropNoSamplesKeepsFullFrame: ohne brauchbare Probe wird nicht geraten.
func TestCropNoSamplesKeepsFullFrame(t *testing.T) {
	got, why := symmetricCropFromSamples(nil, 1920, 1080)
	if why == "" {
		t.Error("ohne Proben muss ein Grund genannt werden")
	}
	if !got.isFullFrame(1920, 1080) {
		t.Errorf("= %+v, erwartet das Vollbild", got)
	}
}

// TestMedianOfIgnoresOutliers sichert den Kern der Mehrheitsbildung: ein
// einzelner Ausreißer darf den Mittelwert nicht verschieben.
func TestMedianOfIgnoresOutliers(t *testing.T) {
	if got := medianOf([]int{280, 280, 280, 280, 70}); got != 280 {
		t.Errorf("medianOf = %d, erwartet 280", got)
	}
	// Bei gerader Anzahl der kleinere der beiden mittleren: weniger schneiden.
	if got := medianOf([]int{100, 140}); got != 100 {
		t.Errorf("medianOf = %d, erwartet 100 (den kleineren)", got)
	}
}

// TestMakeEvenRoundsOutward: Encoder verlangen gerade Maße. Aufgerundet wird
// nach AUSSEN — das lässt eher eine schwarze Zeile stehen, statt eine
// Bildzeile wegzuschneiden.
func TestMakeEvenRoundsOutward(t *testing.T) {
	got := cropRect{w: 1919, h: 815, x: 1, y: 133}.makeEven()
	if got.w%2 != 0 || got.h%2 != 0 || got.x%2 != 0 || got.y%2 != 0 {
		t.Fatalf("makeEven ließ ungerade Werte stehen: %+v", got)
	}
	// x/y rücken nach außen, die Kantenlängen wachsen entsprechend mit:
	// aus x=1,w=1919 wird x=0,w=1920 — der Bildinhalt bleibt vollständig drin.
	want := cropRect{w: 1920, h: 816, x: 0, y: 132}
	if got != want {
		t.Errorf("makeEven = %+v, erwartet %+v", got, want)
	}
}

// TestCropSampleOffsetsAvoidEdges: Anfang und Ende sind genau die Stellen mit
// Logos, Vorspann und Schwarzblenden. Dort zu messen war der belegte Fehlerfall
// (Video mit 8 s Schwarzblende am Anfang), deshalb bleiben sie ausgespart.
func TestCropSampleOffsetsAvoidEdges(t *testing.T) {
	const dur = 3600.0
	offs := cropSampleOffsets(dur, cropDetectSamples)
	if len(offs) != cropDetectSamples {
		t.Fatalf("erwartet %d Proben, bekommen %d", cropDetectSamples, len(offs))
	}
	if offs[0] < dur*cropSampleStartFrac-0.01 {
		t.Errorf("erste Probe bei %.1f s liegt vor dem Startfenster (%.1f s)", offs[0], dur*cropSampleStartFrac)
	}
	letztesEnde := offs[len(offs)-1] + cropDetectWindowSec
	if letztesEnde > dur*cropSampleEndFrac+0.01 {
		t.Errorf("letzte Probe endet bei %.1f s, hinter dem Endfenster (%.1f s)", letztesEnde, dur*cropSampleEndFrac)
	}
	for i := 1; i < len(offs); i++ {
		if offs[i] <= offs[i-1] {
			t.Errorf("Proben laufen nicht aufsteigend: %v", offs)
			break
		}
	}
}

// TestCropSampleOffsetsShortFile: bei einer Datei, die kürzer als ein Fenster
// ist, darf die Aufteilung keine negativen Startzeiten erzeugen.
func TestCropSampleOffsetsShortFile(t *testing.T) {
	for _, dur := range []float64{0, 1, cropDetectWindowSec, 10, 20} {
		for _, o := range cropSampleOffsets(dur, cropDetectSamples) {
			if o < 0 {
				t.Errorf("Dauer %.1f s ergab negativen Startpunkt %.1f s", dur, o)
			}
		}
	}
}

// TestCropRectGeometry prüft die kleinen Auskunftsfunktionen, auf denen Anzeige
// und Plausibilitätsprüfung aufbauen.
func TestCropRectGeometry(t *testing.T) {
	const w, h = 1920, 1080
	c := cropRect{w: 1920, h: 816, x: 0, y: 132}

	if c.isFullFrame(w, h) {
		t.Error("ein geschnittenes Rechteck darf nicht als Vollbild gelten")
	}
	if !fullFrame(w, h).isFullFrame(w, h) {
		t.Error("fullFrame muss als Vollbild gelten")
	}
	if !c.fitsInside(w, h) {
		t.Error("das Rechteck liegt im Bild, wird aber abgelehnt")
	}
	if (cropRect{w: 1920, h: 1080, x: 0, y: 200}).fitsInside(w, h) {
		t.Error("ein über den Rand ragendes Rechteck muss abgelehnt werden")
	}

	// 1080 - 816 = 264 Zeilen von 1080 = 24,4 %
	if p := c.trimmedAreaPercent(w, h); p < 24.3 || p > 24.5 {
		t.Errorf("trimmedAreaPercent = %.2f, erwartet rund 24,4", p)
	}
	if got := c.filterArg(); got != "crop=1920:816:0:132" {
		t.Errorf("filterArg = %q", got)
	}
	if got := c.describe(w, h); !strings.Contains(got, "top 132") || !strings.Contains(got, "bottom 132") {
		t.Errorf("describe = %q, erwartet oben/unten je 132", got)
	}
}

// TestBuildVideoFilterWithCrop sichert die Reihenfolge in der Filterkette ab.
// Der Schnitt muss VOR dem Verkleinern stehen: sonst wird über die Balkenkante
// hinweg interpoliert und die Randzeilen verschmieren.
func TestBuildVideoFilterWithCrop(t *testing.T) {
	c := cropRect{w: 1920, h: 816, x: 0, y: 132}

	chain := buildVideoFilter(true, false, false, c)
	iCrop := strings.Index(chain, "crop=1920:816:0:132")
	iScale := strings.Index(chain, "scale=")
	switch {
	case iCrop < 0:
		t.Fatalf("der Schnitt fehlt in der Kette: %q", chain)
	case iScale < 0:
		t.Fatalf("die Skalierung fehlt in der Kette: %q", chain)
	case iCrop > iScale:
		t.Errorf("der Schnitt steht HINTER der Skalierung: %q", chain)
	}

	// Ohne Verkleinern: kein zweiter, überflüssiger Crop für gerade Maße.
	chain = buildVideoFilter(false, false, false, c)
	if strings.Count(chain, "crop=") != 1 {
		t.Errorf("erwartet genau einen Crop-Filter, bekommen: %q", chain)
	}
	if strings.Contains(chain, "trunc(") {
		t.Errorf("der Rundungs-Crop ist nach einem Auto-Crop überflüssig: %q", chain)
	}
}

// TestBuildVideoFilterCropNeverOnGPU ist die Rückversicherung gegen einen
// unlauffähigen Filtergraphen: scale_cuda kann nicht zuschneiden, ein
// crop_cuda gibt es nicht. Beides zusammen würde FFmpeg abweisen.
func TestBuildVideoFilterCropNeverOnGPU(t *testing.T) {
	c := cropRect{w: 1920, h: 816, x: 0, y: 132}
	chain := buildVideoFilter(true, false, true, c) // gpuScale ausdrücklich an
	if strings.Contains(chain, "scale_cuda") {
		t.Errorf("Schnitt und scale_cuda dürfen nie zusammen in einer Kette stehen: %q", chain)
	}
	if !strings.Contains(chain, "crop=1920:816:0:132") {
		t.Errorf("der Schnitt wurde stillschweigend verworfen: %q", chain)
	}
}

// TestBuildVideoFilterWithoutCropUnchanged: ohne Auto-Crop muss die Kette
// exakt so aussehen wie vor dem Umbau — sonst ändern sich Ergebnisse für
// Leute, die das Feature gar nicht eingeschaltet haben.
func TestBuildVideoFilterWithoutCropUnchanged(t *testing.T) {
	if got := buildVideoFilter(false, false, false, cropRect{}); !strings.HasPrefix(got, "crop=trunc(iw/2)*2:trunc(ih/2)*2") {
		t.Errorf("ohne Auto-Crop muss der Rundungs-Crop stehen bleiben: %q", got)
	}
	if got := buildVideoFilter(true, false, true, cropRect{}); !strings.Contains(got, "scale_cuda") {
		t.Errorf("ohne Auto-Crop muss die GPU-Kette weiter benutzt werden: %q", got)
	}
}

// TestCropDeinterlaceOrder: bwdif braucht die ursprüngliche Feldstruktur und
// muss deshalb vor dem Schnitt laufen.
func TestCropDeinterlaceOrder(t *testing.T) {
	chain := buildVideoFilter(false, true, false, cropRect{w: 1920, h: 816, x: 0, y: 132})
	iBwdif := strings.Index(chain, "bwdif")
	iCrop := strings.Index(chain, "crop=")
	if iBwdif < 0 || iCrop < 0 || iBwdif > iCrop {
		t.Errorf("bwdif muss vor dem Schnitt stehen: %q", chain)
	}
}

// TestCropCheckImagePathNextToSource: das Kontrollbild gehört neben die
// Quelldatei und muss ihren Namen tragen — bei einem Stapel ist sonst nicht
// zuzuordnen, welches Bild zu welchem Film gehört.
func TestCropCheckImagePathNextToSource(t *testing.T) {
	got := cropCheckImagePath(`C:\filme\Der Film.mkv`)
	want := `C:\filme\Der Film` + cropCheckSuffix
	if got != want {
		t.Errorf("cropCheckImagePath = %q, erwartet %q", got, want)
	}
}

// TestAutoCropDefaultsOff hält die Zusage fest, dass niemand ungefragt
// geschnittene Dateien bekommt.
func TestAutoCropDefaultsOff(t *testing.T) {
	if defaultAppSettings().autoCrop {
		t.Error("autoCrop muss in der Voreinstellung ausgeschaltet sein")
	}
}

// TestCropDetectLimitIsScaledByBitDepth sichert den Fehler ab, der Auto-Crop
// bis 1.21.0 an JEDEM HDR-Film blind gemacht hat.
//
// FFmpeg rechnet die Schwarz-Schwelle nur dann auf die Bittiefe des Videos um,
// wenn sie als Anteil zwischen 0 und 1 ankommt. Eine ganze Zahl nimmt es
// wörtlich — und 24 liegt unter dem 10-Bit-Schwarzwert 64, weshalb dort jede
// schwarze Zeile als Bildinhalt galt und nie ein Balken gefunden wurde.
//
// Der Fehler war von außen unsichtbar: das Programm meldete keinen Absturz,
// sondern in aller Ruhe "keeping the full frame". Genau deshalb steht hier
// eine Prüfung auf die Schreibweise und nicht bloß auf den Zahlenwert.
func TestCropDetectLimitIsScaledByBitDepth(t *testing.T) {
	if cropDetectLimit <= 0 || cropDetectLimit >= 1 {
		t.Fatalf("cropDetectLimit = %v — muss zwischen 0 und 1 liegen, sonst "+
			"skaliert FFmpeg sie nicht auf die Bittiefe und Auto-Crop ist an "+
			"10-Bit-Material blind", cropDetectLimit)
	}

	filter := cropDetectFilter()
	if !strings.Contains(filter, "limit=0.") {
		t.Errorf("Filter %q — die Schwelle muss als Fließkomma geschrieben "+
			"sein; eine ganze Zahl nimmt FFmpeg wörtlich", filter)
	}

	// Der Anteil muss am gewohnten 8-Bit-Verhalten nichts ändern: 24/255 ergibt
	// dort wieder 24. Wandert dieser Wert, ändert sich das Ergebnis für alles
	// bisher konvertierte Material — das wäre eine ganz andere Entscheidung.
	if got := cropDetectLimit * 255; got < 23.5 || got > 24.5 {
		t.Errorf("Schwelle entspricht an 8-Bit %.1f statt 24 — Altmaterial "+
			"würde anders geschnitten als bisher", got)
	}
}

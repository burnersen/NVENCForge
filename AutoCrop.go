//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

// Auto-Crop: schwarze Balken erkennen und wegschneiden.
//
// BEI FESTEM CQ spart das Platz (gemessen 2026-08-23, künstlich letterboxtes
// Material, sonst identische Encoder-Einstellungen):
//
//	Balkenanteil 24,4 % (2.35:1) → 6,0 % kleiner bei CQ 26, 6,1 % bei CQ 34
//	Balkenanteil 35,5 % (2.76:1) → 8,7 % kleiner
//	Bildqualität unverändert (VMAF 98,28 → 98,10, nur auf dem echten
//	Bildbereich gemessen), Encode-Zeit 19 % kürzer (weniger Pixel).
//
// Faustformel: die Ersparnis ist rund ein VIERTEL des Balkenanteils. Die im
// Netz kursierenden "10-20 %" gelten für Encoding mit fester Bitrate; bei
// konstanter Qualität kostet eine schwarze Fläche den Encoder fast nichts.
//
// MIT AUTO-CQ (der Voreinstellung) kehrt sich das um: die Dateien werden
// GRÖSSER. Das ist kein Fehler, sondern der eigentliche Gewinn. Auto-CQ misst
// VMAF über das ganze Bild, und mitencodierte schwarze Balken beschönigen
// diese Messung — Auto-CQ wählt daraufhin einen zu sparsamen CQ. Live gemessen
// an zwei Ausschnitten (Ziel VMAF 96, akzeptiert 95,5), VMAF jeweils nur auf
// dem echten Bildbereich:
//
//	Probe 1: ohne Crop 12.873.968 B / VMAF 95,02 — UNTER dem akzeptierten Ziel
//	         mit Crop  13.760.459 B / VMAF 95,70 — Ziel erreicht
//	Probe 2: ohne Crop 17.616.863 B / VMAF 95,61
//	         mit Crop  23.229.202 B / VMAF 96,56
//
// Letterbox-Filme bekamen also bisher heimlich weniger Qualität als
// eingestellt. Der Schnitt macht die Messung ehrlich; der Encoder gibt danach
// die Bitrate aus, die das Ziel tatsächlich verlangt.
//
// Der Suchlauf selbst kostet rund 2 s pro Datei (neun Stichproben).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ----------------------------------------------------------------------------
// Einstellgrößen der Erkennung
// ----------------------------------------------------------------------------

const (
	// cropDetectSamples ist die Zahl der Stichproben über die Laufzeit.
	// Eine einzige Probe reicht nachweislich NICHT: an einer Schwarzblende
	// liefert cropdetect ein unbrauchbares Ergebnis (gemessen 2026-08-23 an
	// einem Video mit 8 s Schwarzblende am Anfang).
	//
	// Neun statt der früheren fünf, seit über die Proben eine MEHRHEIT
	// gebildet wird (siehe symmetricCropFromSamples): eine Mehrheit aus fünf
	// Stimmen kippt schon bei zwei Ausreißern. Neun Proben kosten rund zwei
	// Sekunden pro Datei und machen die Entscheidung spürbar ruhiger.
	cropDetectSamples = 9

	// cropEdgeTolerancePx: Randbreiten, die sich um höchstens so viele Pixel
	// unterscheiden, gelten als derselbe Wert. cropdetect rundet selbst auf
	// gerade Maße, und ein um ein Pixel versetztes Bild ist kein anderer
	// Schnitt, sondern derselbe mit Rundungsrest.
	cropEdgeTolerancePx = 4

	// cropMajorityPercent ist der Anteil der Stichproben, der sich über eine
	// Kante einig sein muss, damit überhaupt geschnitten wird.
	//
	// Das ist die Notbremse für Filme mit wechselndem Bildformat — etwa die
	// IMAX-Szenen in "Dark Knight" oder "Interstellar", wo das Bild in einem
	// Teil des Films wirklich höher ist. Ein einzelnes Logo im Balken wird von
	// zwei Dritteln locker überstimmt; ein echter Formatwechsel nicht, und
	// dann bleibt das Bild lieber ganz stehen.
	cropMajorityPercent = 67

	// cropDetectWindowSec ist die Länge einer Stichprobe.
	cropDetectWindowSec = 4.0

	// cropDetectLimit ist die Schwelle, ab der ein Pixel als "nicht schwarz"
	// gilt — als ANTEIL des Wertebereichs (0.0 bis 1.0), nicht als feste Zahl.
	//
	// Das ist keine Geschmacksfrage, sondern Pflicht: FFmpeg rechnet nur eine
	// Fließkomma-Angabe auf die Bittiefe des Videos um. Eine ganze Zahl nimmt
	// es wörtlich, und dann ist die Schwelle an 10-Bit-Material zu tief:
	// Schwarz liegt dort bei 64, die frühere feste 24 also DARUNTER. Jede
	// schwarze Zeile galt damit als Bildinhalt, und Auto-Crop hat an HDR-Filmen
	// (die immer mindestens 10 Bit haben) NIE einen Balken gefunden — ohne
	// Fehlermeldung, mit der beruhigenden Auskunft "keeping the full frame".
	// Gemessen 2026-08-23 an drei 4K-HDR-Dateien, alle drei blind.
	//
	// 24/255 ist der FFmpeg-Standard, ausgedrückt als Anteil: an 8-Bit ergibt
	// das wieder exakt 24 (Altmaterial bleibt bitgleich), an 10-Bit 96.
	cropDetectLimit = 24.0 / 255.0

	// cropDetectRound: erkannte Maße werden auf ein Vielfaches hiervon
	// abgerundet. 2 ist das Minimum, das Encoder verlangen (gerade Maße).
	cropDetectRound = 2

	// cropMinAreaPercent ist die Untergrenze der verbleibenden Bildfläche.
	// Schlägt die Erkennung mehr als das vor, stimmt etwas nicht (eine sehr
	// dunkle Szene, ein Fade) — dann wird lieber gar nicht geschnitten.
	cropMinAreaPercent = 50

	// cropMinTrimPercent ist die Untergrenze, ab der sich ein Schnitt lohnt.
	// Ein oder zwei Pixel Rand sind Kompressionsartefakte der Quelle, kein
	// Letterbox — dafür das Bild anzufassen wäre reine Unruhe.
	cropMinTrimPercent = 2

	// cropSampleStartFrac / cropSampleEndFrac klammern den Bereich ein, aus dem
	// Stichproben gezogen werden. Anfang und Ende bleiben außen vor: dort
	// liegen Logos, Vorspann, Abspann und Schwarzblenden.
	cropSampleStartFrac = 0.10
	cropSampleEndFrac   = 0.90

	// cropContactSheetShots / cropContactSheetWidth bestimmen das Kontrollbild.
	cropContactSheetShots = 3
	cropContactSheetWidth = 640

	// cropCheckSuffix ist die Endung des Kontrollbilds.
	cropCheckSuffix = ".cropcheck.png"
)

// ----------------------------------------------------------------------------
// cropRect
// ----------------------------------------------------------------------------

// cropRect ist ein Bildausschnitt in Pixeln der QUELLE, in derselben Reihenfolge
// wie der FFmpeg-Filter ihn erwartet: Breite, Höhe, Abstand von links, Abstand
// von oben.
type cropRect struct {
	w, h, x, y int
}

// valid meldet, ob das Rechteck überhaupt benutzbar ist.
func (c cropRect) valid() bool {
	return c.w > 0 && c.h > 0 && c.x >= 0 && c.y >= 0
}

// fullFrame liefert das ungeschnittene Bild — das Ergebnis, wenn keine Balken
// gefunden wurden.
func fullFrame(w, h int) cropRect {
	return cropRect{w: w, h: h, x: 0, y: 0}
}

// isFullFrame meldet, ob nichts weggeschnitten wird.
func (c cropRect) isFullFrame(srcW, srcH int) bool {
	return c.w == srcW && c.h == srcH && c.x == 0 && c.y == 0
}

// fitsInside prüft, ob das Rechteck vollständig im Quellbild liegt.
func (c cropRect) fitsInside(srcW, srcH int) bool {
	return c.valid() && c.x+c.w <= srcW && c.y+c.h <= srcH
}

// filterArg liefert den FFmpeg-Filter für genau diesen Ausschnitt.
func (c cropRect) filterArg() string {
	return fmt.Sprintf("crop=%d:%d:%d:%d", c.w, c.h, c.x, c.y)
}

// trimmedAreaPercent sagt, wie viel Prozent der Bildfläche wegfallen.
func (c cropRect) trimmedAreaPercent(srcW, srcH int) float64 {
	total := srcW * srcH
	if total <= 0 {
		return 0
	}
	return float64(total-c.w*c.h) * 100 / float64(total)
}

// describe beschreibt den Schnitt in Worten, für das Protokoll.
func (c cropRect) describe(srcW, srcH int) string {
	top, bottom := c.y, srcH-c.h-c.y
	left, right := c.x, srcW-c.w-c.x
	var parts []string
	if top > 0 || bottom > 0 {
		parts = append(parts, fmt.Sprintf("top %d / bottom %d", top, bottom))
	}
	if left > 0 || right > 0 {
		parts = append(parts, fmt.Sprintf("left %d / right %d", left, right))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ") + " px"
}

// cropEdges sind die vier Randbreiten eines Vorschlags in Pixeln.
//
// Für die Mehrheitsbildung ist das die richtige Sicht: verglichen werden muss,
// wie DICK der Rand an einer Kante ist — nicht, wo ein Rechteck liegt.
type cropEdges struct {
	top, bottom, left, right int
}

// edgesOf rechnet ein Rechteck in seine vier Randbreiten um.
func edgesOf(r cropRect, srcW, srcH int) cropEdges {
	return cropEdges{
		top:    r.y,
		bottom: srcH - r.h - r.y,
		left:   r.x,
		right:  srcW - r.w - r.x,
	}
}

// medianOf liefert den mittleren Wert einer Stichprobenreihe. Bei gerader
// Anzahl wird der kleinere der beiden mittleren genommen — das schneidet im
// Zweifel weniger weg.
func medianOf(values []int) int {
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	return sorted[(len(sorted)-1)/2]
}

// agreeingCount zählt, wie viele Werte höchstens cropEdgeTolerancePx vom
// Bezugswert abweichen — also wie groß die Mehrheit für ihn ist.
func agreeingCount(values []int, ref int) int {
	agreeing := 0
	for _, v := range values {
		diff := v - ref
		if diff < 0 {
			diff = -diff
		}
		if diff <= cropEdgeTolerancePx {
			agreeing++
		}
	}
	return agreeing
}

// symmetricCropFromSamples verdichtet die Stichproben zu EINEM Schnitt.
//
// Hier stecken die beiden Regeln, um die es bei Auto-Crop eigentlich geht.
// Beide stammen aus einem echten Fehlerfall, keine ist theoretisch gemeint.
//
// MEHRHEIT statt Vereinigung. Früher genügte eine einzige Probe, die an einer
// Stelle Bildinhalt sah, um diese Stelle stehen zu lassen. Das klingt sicher,
// war es aber nicht: im Testfilm "Exodus" blitzt in einer von fünf Proben ein
// Copyright-Logo im unteren Balken auf. Der Schnitt blieb daraufhin unten
// 210 Pixel zu kurz, und das Bild saß im fertigen Film sichtbar schief.
// Jetzt entscheidet die Mehrheit, und ein einzelnes Logo wird überstimmt.
//
// SYMMETRIE. Letterbox und Pillarbox sitzen immer mittig — das Bild steht in
// der Mitte seines Rahmens. Ein Schnitt, der oben mehr wegnimmt als unten, ist
// deshalb kein Sonderfall, sondern ein Fehler. Beide Seiten bekommen den
// KLEINEREN der beiden Werte: das Ergebnis ist mittig, und im Zweifel bleibt
// lieber eine schwarze Zeile stehen, als dass Bild verloren geht.
func symmetricCropFromSamples(samples []cropRect, srcW, srcH int) (cropRect, string) {
	full := fullFrame(srcW, srcH)
	if len(samples) == 0 {
		return full, "detection found nothing usable"
	}

	tops := make([]int, 0, len(samples))
	bottoms := make([]int, 0, len(samples))
	lefts := make([]int, 0, len(samples))
	rights := make([]int, 0, len(samples))
	for _, s := range samples {
		e := edgesOf(s, srcW, srcH)
		tops = append(tops, e.top)
		bottoms = append(bottoms, e.bottom)
		lefts = append(lefts, e.left)
		rights = append(rights, e.right)
	}

	// Aufgerundet, damit die Schwelle bei kleinen Stichprobenzahlen nicht
	// stillschweigend nach unten rutscht.
	needed := (len(samples)*cropMajorityPercent + 99) / 100
	for _, edge := range [][]int{tops, bottoms, lefts, rights} {
		if agreeingCount(edge, medianOf(edge)) < needed {
			return full, "picture size keeps changing — leaving it alone"
		}
	}

	vertical := max(0, min(medianOf(tops), medianOf(bottoms)))
	horizontal := max(0, min(medianOf(lefts), medianOf(rights)))

	// Gerade Ränder: sonst biegt makeEven die Maße hinterher gerade und
	// zerstört dabei genau die Symmetrie, die hier hergestellt wurde.
	// Abrunden lässt im Zweifel eine Zeile stehen.
	vertical -= vertical % 2
	horizontal -= horizontal % 2

	return cropRect{
		w: srcW - 2*horizontal,
		h: srcH - 2*vertical,
		x: horizontal,
		y: vertical,
	}, ""
}

// makeEven rundet das Rechteck auf gerade Maße ab — ungerade Breiten/Höhen
// weisen die Encoder zurück. Nach außen aufrunden ist hier richtig: es lässt
// eher eine schwarze Zeile stehen, als eine Bildzeile zu opfern.
func (c cropRect) makeEven() cropRect {
	if c.x%2 != 0 {
		c.x--
		c.w++
	}
	if c.y%2 != 0 {
		c.y--
		c.h++
	}
	if c.w%2 != 0 {
		c.w++
	}
	if c.h%2 != 0 {
		c.h++
	}
	return c
}

// ----------------------------------------------------------------------------
// Erkennung
// ----------------------------------------------------------------------------

// cropdetect meldet sein Ergebnis als Zeile "... crop=1920:816:0:132" im
// Protokoll. Negative Werte kommen vor (reines Schwarz im Fenster) und müssen
// mit erfasst werden, damit sie als ungültig verworfen werden können statt
// unbemerkt als "-1" durchzurutschen.
var cropDetectLine = regexp.MustCompile(`crop=(-?\d+):(-?\d+):(-?\d+):(-?\d+)`)

// parseCropDetect zieht das letzte cropdetect-Ergebnis aus einer FFmpeg-Ausgabe.
// Das LETZTE ist das maßgebliche: cropdetect verfeinert seinen Vorschlag über
// das Fenster hinweg und gibt am Schluss das Gesamtergebnis aus.
func parseCropDetect(output string) (cropRect, bool) {
	all := cropDetectLine.FindAllStringSubmatch(output, -1)
	if len(all) == 0 {
		return cropRect{}, false
	}
	m := all[len(all)-1]
	nums := make([]int, 4)
	for i := 0; i < 4; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return cropRect{}, false
		}
		nums[i] = n
	}
	r := cropRect{w: nums[0], h: nums[1], x: nums[2], y: nums[3]}
	if !r.valid() {
		// Reines Schwarz im Fenster: cropdetect liefert dann Unsinn wie
		// "crop=-1:-1:-1:-1". Kein Ergebnis ist besser als ein falsches.
		return cropRect{}, false
	}
	return r, true
}

// cropSampleOffsets verteilt die Stichproben über die Laufzeit. Bei sehr kurzen
// Dateien rücken sie zusammen; ist die Datei kürzer als ein Fenster, bleibt es
// bei einer einzigen Probe ab Sekunde 0.
func cropSampleOffsets(durationSec float64, samples int) []float64 {
	if durationSec <= cropDetectWindowSec || samples <= 1 {
		return []float64{0}
	}
	first := durationSec * cropSampleStartFrac
	last := durationSec*cropSampleEndFrac - cropDetectWindowSec
	if last <= first {
		return []float64{max(0, durationSec/2-cropDetectWindowSec/2)}
	}
	step := (last - first) / float64(samples-1)
	offsets := make([]float64, 0, samples)
	for i := 0; i < samples; i++ {
		offsets = append(offsets, first+step*float64(i))
	}
	return offsets
}

// cropDetectFilter baut den FFmpeg-Filter der Erkennung.
//
// Eigene Funktion, damit eine Prüfung den fertigen Text sehen kann: der Fehler,
// den es hier zu verhindern gilt, steckt nicht in der Logik, sondern in der
// Schreibweise EINER Zahl (siehe cropDetectLimit).
func cropDetectFilter() string {
	return fmt.Sprintf("cropdetect=limit=%.5f:round=%d:reset=0",
		cropDetectLimit, cropDetectRound)
}

// runCropDetectWindow misst ein einzelnes Fenster.
func runCropDetectWindow(ctx context.Context, path string, offsetSec float64) (cropRect, bool) {
	args := []string{
		"-hide_banner", "-nostats", "-nostdin",
		"-ss", strconv.FormatFloat(offsetSec, 'f', 3, 64),
		"-t", strconv.FormatFloat(cropDetectWindowSec, 'f', 3, 64),
		"-i", path,
		"-map", "0:V:0",
		"-vf", cropDetectFilter(),
		"-an", "-sn", "-f", "null", "-",
	}
	// cropdetect schreibt sein Ergebnis nach stderr; CombinedOutput fängt beides.
	out, err := exec.CommandContext(ctx, ffmpegPath, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return cropRect{}, false
	}
	return parseCropDetect(string(out))
}

// cropSample ist eine Stichprobe samt ihrem Zeitpunkt.
//
// Der Zeitpunkt gehört dazu, weil die Untertitel-Prüfung weiter unten genau
// dort noch einmal hinsehen will: Eine Probe, die im Balken etwas gesehen hat,
// ist die einzige Spur, die auf eingebrannten Text führt — ohne ihre Sekunde
// müsste die Prüfung den ganzen Film absuchen.
type cropSample struct {
	offsetSec float64
	rect      cropRect
}

// collectCropSamples misst die Stichproben über die Laufzeit.
//
// Unbrauchbare Fenster (Schwarzblende, Lesefehler) werden übersprungen. Sie
// zählen NICHT als "kein Balken", sondern gar nicht: Eine Schwarzblende weiß
// nichts über das Bildformat des Films.
func collectCropSamples(ctx context.Context, path string, srcW, srcH int, durationSec float64) []cropSample {
	offsets := cropSampleOffsets(durationSec, cropDetectSamples)
	samples := make([]cropSample, 0, len(offsets))
	for _, off := range offsets {
		if ctx.Err() != nil {
			return samples
		}
		r, ok := runCropDetectWindow(ctx, path, off)
		if !ok {
			continue
		}
		if !r.fitsInside(srcW, srcH) {
			// Ein Vorschlag, der über den Bildrand hinausragt, ist kaputt.
			continue
		}
		samples = append(samples, cropSample{offsetSec: off, rect: r})
	}
	return samples
}

// rectsOf zieht die reinen Rechtecke aus den Proben — die Mehrheitsrechnung
// braucht die Zeitpunkte nicht.
func rectsOf(samples []cropSample) []cropRect {
	rects := make([]cropRect, 0, len(samples))
	for _, s := range samples {
		rects = append(rects, s.rect)
	}
	return rects
}

// detectCropRect sucht die schwarzen Balken einer Datei.
//
// Rückgabe ist IMMER ein benutzbares Rechteck: findet sich nichts Verlässliches,
// kommt das ungeschnittene Vollbild zurück. Die Erkennung darf einen Lauf nie
// scheitern lassen — im Zweifel wird eben nicht geschnitten.
//
// Zwei Ausnahmen klammern die eigentliche Messung ein, beide aus demselben
// Grund: Ein Untertitel, der im schwarzen Balken steht, wird mit dem Balken
// weggeschnitten. Deshalb wird VOR der Messung nach Untertitelspuren gesehen,
// die ihre Position selbst mitbringen, und NACH der Messung nach Text, der
// fest im Balken klebt.
func detectCropRect(ctx context.Context, path string, stats *VideoStats) (cropRect, string) {
	full := fullFrame(stats.Width, stats.Height)
	if stats.Width <= 0 || stats.Height <= 0 {
		return full, "source dimensions unknown"
	}

	// Ausnahme 1 — kostenlos, und deshalb ganz vorn: Sie erspart bei einem
	// Blu-ray-Rip die komplette Suche.
	if kind, found := pictureSubtitleKind(stats.SubCodecs); found {
		return full, fmt.Sprintf("%s subtitles sit at fixed positions and would land outside the picture", kind)
	}

	samples := collectCropSamples(ctx, path, stats.Width, stats.Height, stats.DurationSec)
	if ctx.Err() != nil {
		return full, "cancelled"
	}

	combined, why := symmetricCropFromSamples(rectsOf(samples), stats.Width, stats.Height)
	if why != "" {
		return full, why
	}

	combined = combined.makeEven()
	if !combined.fitsInside(stats.Width, stats.Height) {
		return full, "detected rectangle outside the frame"
	}
	if combined.isFullFrame(stats.Width, stats.Height) {
		return full, "no bars"
	}

	trimmed := combined.trimmedAreaPercent(stats.Width, stats.Height)
	if trimmed < cropMinTrimPercent {
		return full, fmt.Sprintf("bars too thin to matter (%.1f%%)", trimmed)
	}
	if 100-trimmed < cropMinAreaPercent {
		// Mehr als die Hälfte wegzuschneiden ist kein Letterbox mehr, sondern
		// eine Fehlerkennung. Nicht schneiden.
		return full, fmt.Sprintf("suspicious result, would remove %.0f%% — ignored", trimmed)
	}

	// Ausnahme 2 — kostet nur dann Zeit, wenn wirklich geschnitten würde.
	if note, found := burnedInSubtitlesInBars(ctx, path, combined, samples, stats); found {
		return full, note
	}
	return combined, ""
}

// ----------------------------------------------------------------------------
// Ausnahme: Untertitel stehen im Balken
// ----------------------------------------------------------------------------

// Warum es diese Ausnahme gibt: Bei vielen Filmen steht der Untertitel nicht
// IM Bild, sondern UNTER ihm — im schwarzen Balken. Das ist keine Schlamperei,
// sondern Absicht: Dort verdeckt er nichts. Wird der Balken weggeschnitten,
// verschwindet der Untertitel mit ihm.
//
// Zwei Bauarten müssen getrennt behandelt werden:
//
//  1. UNTERTITELSPUREN AUS BILDERN (Blu-ray PGS, DVD VobSub). Sie sind fertige
//     Grafiken mit fester Position im Vollbild-Raster, oft unterhalb des
//     Bildbereichs. Der Abspieler kann sie nicht neu setzen. Sie sind an der
//     Spurliste erkennbar — das kostet keine Sekunde Suchzeit.
//     Textspuren (SRT, ASS, mov_text) sind NICHT betroffen: Die setzt der
//     Abspieler selbst, sie überstehen einen Schnitt unbeschadet.
//
//  2. FEST EINGEBRANNTE UNTERTITEL. Die stehen im Bild und lassen sich nur
//     sehen, nicht abfragen. Sie zu erkennen ist Schätzarbeit — die Regeln
//     dafür stehen bei looksLikeSubtitleLine und stripCarriesSubtitles.
//
// Der schwierige Teil ist nicht, Text im Balken zu FINDEN, sondern ihn von
// einem Copyright-Logo zu unterscheiden. Genau so ein Logo hat in v1.21.1
// einmal den halben Schnitt verhindert (siehe symmetricCropFromSamples), und
// diese Ausnahme darf jene Lösung nicht wieder aufheben.
const (
	// subtitleMinBarHeightPx: Unter dieser Balkenhöhe wird gar nicht erst
	// gesucht. In zwei Dutzend Pixel passt keine lesbare Zeile — was dort
	// gefunden würde, wäre Rauschen.
	subtitleMinBarHeightPx = 24

	// subtitleLineMinWidthPercent: So breit muss der Fund mindestens sein,
	// gemessen an der Bildbreite. Eine Untertitelzeile ist breit; ein Logo,
	// ein Zeitstempel oder ein Kompressionsrest ist schmal.
	subtitleLineMinWidthPercent = 12

	// subtitleCenterTolerancePercent: So weit darf die Mitte des Fundes von der
	// Bildmitte abweichen. Untertitel stehen mittig — das ist ihre auffälligste
	// Eigenschaft und das beste Unterscheidungsmerkmal gegenüber einem Logo,
	// das fast immer in einer Ecke klebt.
	subtitleCenterTolerancePercent = 10

	// subtitleHitsNeeded: So viele Fenster müssen eine Zeile zeigen, bevor der
	// Schnitt abgesagt wird. ZWEI, nicht eines — und das ist der Kern:
	// Untertitel kehren über den Film verteilt wieder, ein Copyright-Hinweis
	// steht genau einmal da. Ein einzelner Fund kippt gar nichts.
	subtitleHitsNeeded = 2

	// subtitleProbeWindowSec ist die Länge eines Prüffensters. Vier Sekunden
	// decken eine gesprochene Zeile bequem ab.
	subtitleProbeWindowSec = 4.0
)

// pictureSubtitleKinds sind die Bild-Untertitel und ihre Klartextnamen. Der
// Abgleich läuft über Teilzeichenketten, weil FFmpeg je nach Fassung
// "hdmv_pgs_subtitle", "dvd_subtitle" oder "vobsub" schreibt.
var pictureSubtitleKinds = []struct {
	marker string
	name   string
}{
	{"hdmv_pgs", "Blu-ray (PGS)"},
	{"pgssub", "Blu-ray (PGS)"},
	{"dvd_sub", "DVD (VobSub)"},
	{"vobsub", "DVD (VobSub)"},
	{"dvb_sub", "DVB"},
	{"xsub", "XSUB"},
}

// pictureSubtitleKind nennt die erste gefundene Bild-Untertitelspur.
func pictureSubtitleKind(subCodecs []string) (string, bool) {
	for _, codec := range subCodecs {
		lower := strings.ToLower(strings.TrimSpace(codec))
		for _, kind := range pictureSubtitleKinds {
			if strings.Contains(lower, kind.marker) {
				return kind.name, true
			}
		}
	}
	return "", false
}

// barStrip ist ein Streifen, den der Schnitt entfernen würde, mit seinem Namen
// für die Meldung.
type barStrip struct {
	name string
	rect cropRect
}

// barStripsOf liefert die waagerechten Streifen des geplanten Schnitts, den
// unteren zuerst.
//
// Seitliche Balken (Pillarbox) bleiben absichtlich außen vor: Untertitel stehen
// unter dem Bild, nicht daneben. Dort zu suchen würde nur Zeit kosten und die
// Zahl der Fehlalarme erhöhen.
func barStripsOf(crop cropRect, srcW, srcH int) []barStrip {
	strips := make([]barStrip, 0, 2)
	if bottomHeight := srcH - (crop.y + crop.h); bottomHeight >= subtitleMinBarHeightPx {
		strips = append(strips, barStrip{
			name: "bottom",
			rect: cropRect{w: srcW, h: bottomHeight, x: 0, y: crop.y + crop.h},
		})
	}
	if crop.y >= subtitleMinBarHeightPx {
		strips = append(strips, barStrip{
			name: "top",
			rect: cropRect{w: srcW, h: crop.y, x: 0, y: 0},
		})
	}
	return strips
}

// looksLikeSubtitleLine entscheidet, ob ein Fund im Balken eine Textzeile ist.
//
// Breit UND mittig — beides zusammen, sonst zählt es nicht. Ein Logo ist
// entweder schmal oder sitzt außen, meistens beides.
func looksLikeSubtitleLine(found cropRect, srcW int) bool {
	if !found.valid() || srcW <= 0 {
		return false
	}
	if found.w*100 < srcW*subtitleLineMinWidthPercent {
		return false
	}
	offCentre := (found.x + found.w/2) - srcW/2
	if offCentre < 0 {
		offCentre = -offCentre
	}
	return offCentre*100 <= srcW*subtitleCenterTolerancePercent
}

// suspectOffsets nennt die Zeitpunkte, an denen die Balkenmessung in DIESEN
// Streifen hineingeragt hat — dort stand also etwas, das nicht schwarz war.
//
// Diese Auskunft ist geschenkt: Die Proben sind längst gemessen. Sie macht die
// Prüfung billig, denn ohne einen einzigen Verdacht wird gar nicht erst ein
// zweites Mal in die Datei gesehen.
func suspectOffsets(samples []cropSample, strip cropRect) []float64 {
	stripTop := strip.y + cropEdgeTolerancePx
	stripBottom := strip.y + strip.h - cropEdgeTolerancePx
	offsets := make([]float64, 0, len(samples))
	for _, s := range samples {
		top := s.rect.y
		bottom := s.rect.y + s.rect.h
		if top < stripBottom && bottom > stripTop {
			offsets = append(offsets, s.offsetSec)
		}
	}
	return offsets
}

// midpointOffsets legt zusätzliche Zeitpunkte GENAU ZWISCHEN die vorhandenen
// Proben.
//
// Warum zwischen und nicht irgendwo: So kann kein Fenster zweimal gezählt
// werden. Würde derselbe Zeitpunkt ein zweites Mal geprüft, käme ein einzelnes
// Logo auf zwei Treffer — und die Regel "erst beim zweiten Fund" wäre wertlos.
// Zeitpunkte, die zu dicht beieinanderliegen, fallen aus demselben Grund weg.
func midpointOffsets(samples []cropSample) []float64 {
	mids := make([]float64, 0, len(samples))
	for i := 1; i < len(samples); i++ {
		gap := samples[i].offsetSec - samples[i-1].offsetSec
		if gap < 2*subtitleProbeWindowSec {
			continue
		}
		mids = append(mids, samples[i-1].offsetSec+gap/2)
	}
	return mids
}

// textLineInStrip prüft ein einzelnes Fenster: Steht in diesem Streifen etwas,
// das wie eine Untertitelzeile aussieht?
//
// Der Streifen wird ausgeschnitten, BEVOR cropdetect ihn ansieht. Nur so kann
// cropdetect die Lage des Textes melden — auf dem ganzen Bild würde es
// schlicht den Bildinhalt umranden.
func textLineInStrip(ctx context.Context, path string, offsetSec float64, strip cropRect, srcW int) bool {
	args := []string{
		"-hide_banner", "-nostats", "-nostdin",
		"-ss", strconv.FormatFloat(offsetSec, 'f', 3, 64),
		"-t", strconv.FormatFloat(subtitleProbeWindowSec, 'f', 3, 64),
		"-i", path,
		"-map", "0:V:0",
		"-vf", strip.filterArg() + "," + cropDetectFilter(),
		"-an", "-sn", "-f", "null", "-",
	}
	out, err := exec.CommandContext(ctx, ffmpegPath, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return false
	}
	found, ok := parseCropDetect(string(out))
	if !ok {
		// Ein reinschwarzes Fenster liefert unbrauchbare (auch negative) Maße.
		// Genau das ist hier die gute Nachricht: nichts im Balken.
		return false
	}
	return looksLikeSubtitleLine(found, srcW)
}

// stripCarriesSubtitles prüft EINEN Streifen in zwei Runden.
//
// Runde 1 sieht nur dort nach, wo die Balkenmessung schon etwas bemerkt hat.
// Findet sich dabei keine Zeile, ist der Fall erledigt — ohne einen einzigen
// zusätzlichen FFmpeg-Lauf, wenn es gar keinen Verdacht gab.
//
// Runde 2 startet erst nach dem ersten Fund und beantwortet die entscheidende
// Frage: Kommt der Text wieder? Untertitel tun das, ein Copyright-Hinweis
// nicht. Sie hört auf, sobald die Antwort feststeht — im Regelfall nach ein,
// zwei Fenstern.
func stripCarriesSubtitles(ctx context.Context, path string, strip barStrip, samples []cropSample, srcW int) bool {
	hits := 0
	for _, off := range suspectOffsets(samples, strip.rect) {
		if ctx.Err() != nil {
			return false
		}
		if textLineInStrip(ctx, path, off, strip.rect, srcW) {
			hits++
			if hits >= subtitleHitsNeeded {
				return true
			}
		}
	}
	if hits == 0 {
		return false
	}
	for _, off := range midpointOffsets(samples) {
		if ctx.Err() != nil {
			return false
		}
		if textLineInStrip(ctx, path, off, strip.rect, srcW) {
			hits++
			if hits >= subtitleHitsNeeded {
				return true
			}
		}
	}
	return false
}

// burnedInSubtitlesInBars ist die zweite Ausnahme: Steht in einem der beiden
// Balken fest eingebrannter Text, bleibt das Bild ungeschnitten.
func burnedInSubtitlesInBars(ctx context.Context, path string, crop cropRect,
	samples []cropSample, stats *VideoStats) (string, bool) {

	for _, strip := range barStripsOf(crop, stats.Width, stats.Height) {
		if stripCarriesSubtitles(ctx, path, strip, samples, stats.Width) {
			return fmt.Sprintf("subtitles are burned into the %s bar", strip.name), true
		}
	}
	return "", false
}

// ----------------------------------------------------------------------------
// Kontrollbild
// ----------------------------------------------------------------------------

// cropCheckImagePath ist der Ablageort des Kontrollbilds: neben der Quelldatei,
// mit deren Namen — so bleibt bei einem Stapel klar, welches Bild zu welchem
// Film gehört.
func cropCheckImagePath(sourcePath string) string {
	dir := filepath.Dir(sourcePath)
	base := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	return filepath.Join(dir, base+cropCheckSuffix)
}

// writeCropContactSheet legt das Kontrollbild an: mehrere Standbilder aus dem
// Film, UNGESCHNITTEN, mit rot eingezeichneter Schnittkante, untereinander.
//
// Warum ungeschnitten: eine Vorschau des bereits geschnittenen Bildes kann die
// eigentliche Frage gar nicht beantworten. Man sieht darin nur, was übrig ist —
// ob unten ein Untertitel oder ein Kinn fehlt, fällt erst im Vergleich mit dem
// vollen Bild auf. Deshalb das volle Bild plus Linie.
func writeCropContactSheet(ctx context.Context, sourcePath string, rect cropRect,
	durationSec float64, destPath string) error {

	offsets := cropSampleOffsets(durationSec, cropContactSheetShots)

	args := []string{"-y", "-hide_banner", "-nostats", "-nostdin", "-loglevel", "error"}
	for _, off := range offsets {
		args = append(args,
			"-ss", strconv.FormatFloat(off, 'f', 3, 64),
			"-i", sourcePath)
	}

	// Je Eingang ein Standbild mit Rahmen, danach untereinander stapeln.
	var fg strings.Builder
	for i := range offsets {
		fmt.Fprintf(&fg, "[%d:v:0]drawbox=x=%d:y=%d:w=%d:h=%d:color=red@0.9:t=4,"+
			"scale=%d:-2,setsar=1[s%d];",
			i, rect.x, rect.y, rect.w, rect.h, cropContactSheetWidth, i)
	}
	for i := range offsets {
		fmt.Fprintf(&fg, "[s%d]", i)
	}
	fmt.Fprintf(&fg, "vstack=inputs=%d[out]", len(offsets))

	args = append(args,
		"-filter_complex", fg.String(),
		"-map", "[out]", "-frames:v", "1", destPath)

	out, err := exec.CommandContext(ctx, ffmpegPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("AutoCrop.go: writeCropContactSheet: %w | %s",
			err, strings.TrimSpace(lastLines(string(out), 2)))
	}
	if _, statErr := os.Stat(destPath); statErr != nil {
		return fmt.Errorf("AutoCrop.go: writeCropContactSheet: no image written: %w", statErr)
	}
	return nil
}

// reportCropCheck ist der ganze -cropcheck-Modus: Bild schreiben, sagen wo es
// liegt, fertig. Es wird nichts konvertiert und nichts an der Quelle verändert.
//
// Das Bild entsteht auch dann, wenn keine Balken gefunden wurden — die Antwort
// "hier ist nichts zu holen" ist genauso eine Antwort, und wer sie nur als
// Textzeile bekommt, glaubt sie nicht.
func reportCropCheck(ctx context.Context, sourcePath string, crop cropRect, stats *VideoStats) {
	rect := crop
	if !rect.valid() {
		rect = fullFrame(stats.Width, stats.Height)
	}
	dest := cropCheckImagePath(sourcePath)
	if err := writeCropContactSheet(ctx, sourcePath, rect, stats.DurationSec, dest); err != nil {
		pWarn.Printf("Crop check: could not write the picture (%v).\n\n", plainError(err))
		return
	}
	pOK.Printf("Crop check written: %s\n", filepath.Base(dest))
	pInfo.Println("  The red line marks what would be kept. Check that no picture content sits outside it.")
	fmt.Println()
}

// lastLines kürzt eine Fehlerausgabe auf die letzten n Zeilen — FFmpeg-Fehler
// stehen immer am Schluss, alles davor ist Aufbaugeplapper.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(strings.Join(lines[len(lines)-n:], " | "))
}

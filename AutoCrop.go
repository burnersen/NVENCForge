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

// detectCropRect sucht die schwarzen Balken einer Datei.
//
// Rückgabe ist IMMER ein benutzbares Rechteck: findet sich nichts Verlässliches,
// kommt das ungeschnittene Vollbild zurück. Die Erkennung darf einen Lauf nie
// scheitern lassen — im Zweifel wird eben nicht geschnitten.
func detectCropRect(ctx context.Context, path string, srcW, srcH int, durationSec float64) (cropRect, string) {
	full := fullFrame(srcW, srcH)
	if srcW <= 0 || srcH <= 0 {
		return full, "source dimensions unknown"
	}

	// Erst ALLE Proben einsammeln, dann entscheiden. Früher brach die Schleife
	// ab, sobald eine Probe das volle Bild sah — unter der Vereinigungsregel
	// war das richtig, unter der Mehrheitsregel wäre es das Gegenteil: eine
	// einzelne Probe darf nicht mehr für alle sprechen.
	offsets := cropSampleOffsets(durationSec, cropDetectSamples)
	samples := make([]cropRect, 0, len(offsets))
	for _, off := range offsets {
		if ctx.Err() != nil {
			return full, "cancelled"
		}
		r, ok := runCropDetectWindow(ctx, path, off)
		if !ok {
			// Unbrauchbares Fenster (Schwarzblende, Lesefehler): überspringen.
			// Es zählt nicht als "kein Balken", sondern gar nicht.
			continue
		}
		if !r.fitsInside(srcW, srcH) {
			// Ein Vorschlag, der über den Bildrand hinausragt, ist kaputt.
			continue
		}
		samples = append(samples, r)
	}

	combined, why := symmetricCropFromSamples(samples, srcW, srcH)
	if why != "" {
		return full, why
	}

	combined = combined.makeEven()
	if !combined.fitsInside(srcW, srcH) {
		return full, "detected rectangle outside the frame"
	}
	if combined.isFullFrame(srcW, srcH) {
		return full, "no bars"
	}

	trimmed := combined.trimmedAreaPercent(srcW, srcH)
	if trimmed < cropMinTrimPercent {
		return full, fmt.Sprintf("bars too thin to matter (%.1f%%)", trimmed)
	}
	if 100-trimmed < cropMinAreaPercent {
		// Mehr als die Hälfte wegzuschneiden ist kein Letterbox mehr, sondern
		// eine Fehlerkennung. Nicht schneiden.
		return full, fmt.Sprintf("suspicious result, would remove %.0f%% — ignored", trimmed)
	}
	return combined, ""
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

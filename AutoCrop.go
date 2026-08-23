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
// Der Suchlauf selbst kostet rund 1 s pro Datei.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// einem Video mit 8 s Schwarzblende am Anfang). Fünf Proben kosten rund
	// 1 s und machen genau diesen Fall harmlos.
	cropDetectSamples = 5

	// cropDetectWindowSec ist die Länge einer Stichprobe.
	cropDetectWindowSec = 4.0

	// cropDetectLimit ist die Schwelle, ab der ein Pixel als "nicht schwarz"
	// gilt (0-255). 24 ist der FFmpeg-Standard und hat in der Messreihe auch
	// an extrem dunklem HDR-Material keinen Fehlalarm erzeugt.
	cropDetectLimit = 24

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

// union legt zwei Rechtecke übereinander und liefert das kleinste, das BEIDE
// enthält. Das ist die Sicherheitsregel der ganzen Erkennung: sieht auch nur
// eine Stichprobe an einer Stelle Bildinhalt, bleibt diese Stelle stehen.
// Lieber ein paar schwarze Zeilen mitschleppen als einen Kopf abschneiden.
func (c cropRect) union(o cropRect) cropRect {
	if !c.valid() {
		return o
	}
	if !o.valid() {
		return c
	}
	left := min(c.x, o.x)
	top := min(c.y, o.y)
	right := max(c.x+c.w, o.x+o.w)
	bottom := max(c.y+c.h, o.y+o.h)
	return cropRect{w: right - left, h: bottom - top, x: left, y: top}
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

// runCropDetectWindow misst ein einzelnes Fenster.
func runCropDetectWindow(ctx context.Context, path string, offsetSec float64) (cropRect, bool) {
	args := []string{
		"-hide_banner", "-nostats", "-nostdin",
		"-ss", strconv.FormatFloat(offsetSec, 'f', 3, 64),
		"-t", strconv.FormatFloat(cropDetectWindowSec, 'f', 3, 64),
		"-i", path,
		"-map", "0:V:0",
		"-vf", fmt.Sprintf("cropdetect=limit=%d:round=%d:reset=0", cropDetectLimit, cropDetectRound),
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

	offsets := cropSampleOffsets(durationSec, cropDetectSamples)
	var combined cropRect
	usable := 0
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
		usable++
		combined = combined.union(r)
		if combined.isFullFrame(srcW, srcH) {
			// Eine Probe sieht das volle Bild — dann gibt es keine
			// durchgehenden Balken und weitere Proben ändern daran nichts.
			return full, "no bars"
		}
	}

	if usable == 0 {
		return full, "detection found nothing usable"
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

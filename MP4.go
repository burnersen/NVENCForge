//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

// MP4.go — alles, was das Programm in einen MP4-Container schreibt.
//
// Der Sinn dieser Datei ist die Sammelstelle mp4MuxArgs: JEDE MP4, die dieses
// Programm erzeugt, muss dieselbe Kompatibilitäts-Behandlung bekommen. Vorher
// stand die Regel dreimal wortgleich an verschiedenen Stellen — genau so
// vergisst man sie beim vierten Ausgabeweg.
//
// Der Flag heißt seit 1.10.0 -mp4; -apple ist der alte Name aus 1.4.0 und
// bleibt gültig, weil er in bestehenden Send-to-Verknüpfungen steckt.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pterm/pterm"
)

// mp4TrackPromptTimeout: Wer einen ganzen Ordner draufzieht und weggeht, soll
// nicht bei Datei 3 auf eine unbeantwortete Frage warten. Nach Ablauf gilt
// „alle Spuren behalten" — das Verhalten der Versionen vor 1.10.0.
const mp4TrackPromptTimeout = 30 * time.Second

// ----------------------------------------------------------------------------
// Die eine Stelle für MP4-Kompatibilität
// ----------------------------------------------------------------------------

// mp4MuxArgs liefert die Muxer-Optionen, die jede vom Programm geschriebene
// MP4 braucht:
//
//   - "+faststart" schiebt das Inhaltsverzeichnis (moov-Atom) an den Anfang.
//     Ohne das muss ein Player erst ans Dateiende springen; beim Abspielen über
//     Netzwerk oder Cloud beginnt das Video sonst spürbar später.
//   - "hvc1" statt des FFmpeg-Standards "hev1": Apple-Player (iOS-Fotos,
//     QuickTime) und DaVinci Resolve lehnen hev1 rundheraus ab. Für H.264
//     ("avc1") und AV1 ("av01") ist der Standardwert bereits richtig, dort wäre
//     ein eigener Tag falsch.
//
// videoCodec ist der Codec-Name aus FFprobe (z. B. "hevc").
func mp4MuxArgs(videoCodec string) []string {
	args := []string{"-movflags", "+faststart"}
	switch strings.ToLower(strings.TrimSpace(videoCodec)) {
	case "hevc", "h265":
		args = append(args, "-tag:v", "hvc1")
	}
	return args
}

// primaryVideoCodec liefert den Codec-Namen der Spur, die "-map 0:V:0"
// auswählt: die erste echte Videospur. Ein eingebettetes Titelbild (Cover) ist
// für FFprobe zwar auch "video", wird von 0:V:0 aber übersprungen — würde man
// es hier mitzählen, bekäme eine H.265-Datei mit Cover keinen hvc1-Tag.
func primaryVideoCodec(streams *ffprobeOutput) string {
	if streams == nil {
		return ""
	}
	for _, s := range streams.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			return s.CodecName
		}
	}
	return ""
}

// ----------------------------------------------------------------------------
// Spurauswahl
// ----------------------------------------------------------------------------

// mp4TrackSelection hält die Spuren, die in die MP4 wandern — als relative
// Indizes, wie FFmpeg sie in "0:a:N" bzw. "0:s:N" erwartet. Leere Listen
// bedeuten hier wirklich „keine Spur dieser Art", nicht „alle": die
// Vollauswahl wird beim Bauen der Auswahl explizit eingetragen.
type mp4TrackSelection struct {
	audio []int
	subs  []int
}

// chooseMP4Tracks zeigt die Ton- und Untertitelspuren und lässt den Nutzer
// wählen. Gefragt wird nur, wenn es überhaupt etwas zu entscheiden gibt (zwei
// oder mehr Einträge) — bei einer einzelnen Tonspur läuft alles durch wie
// bisher.
//
// MP4 kann mehrere Tonspuren, das ist kein Hindernis. Bild-Untertitel (PGS,
// VobSub) kann es dagegen wirklich nicht: die tauchen deshalb gar nicht erst
// in der Liste auf, werden aber gemeldet, damit ihr Fehlen nicht wie ein
// Fehler aussieht.
//
// srcPath wird nur weitergereicht, damit die Ankündigung im Datenkanal (-json)
// sagen kann, um welche Datei es geht; auf die Auswahl selbst hat er keinen
// Einfluss.
func chooseMP4Tracks(streams *ffprobeOutput, srcPath string) mp4TrackSelection {
	type entry struct {
		isSub bool
		rel   int
		label string
	}
	var entries []entry
	var sel mp4TrackSelection
	aIdx, sIdx, imageSubs := 0, 0, 0

	for _, s := range streams.Streams {
		lang := "und"
		if s.Tags != nil {
			if l := s.Tags["language"]; l != "" {
				lang = normalizeLang(l)
			}
		}
		switch s.CodecType {
		case "audio":
			entries = append(entries, entry{false, aIdx, fmt.Sprintf(
				"Audio  %-3s  %s %dch", lang, strings.ToUpper(s.CodecName), s.Channels)})
			sel.audio = append(sel.audio, aIdx)
			aIdx++
		case "subtitle":
			// Dieselbe Liste wie beim MKV-Weg: was sich in SRT wandeln lässt,
			// ist Text und passt damit auch als mov_text in eine MP4.
			if subTextConvertibleToSRT(s.CodecName) {
				label := fmt.Sprintf("Sub    %-3s  %s", lang, strings.ToUpper(s.CodecName))
				if s.Disposition.Forced == 1 {
					label += " [forced]"
				}
				if s.Disposition.HearingImpaired == 1 {
					label += " [SDH]"
				}
				entries = append(entries, entry{true, sIdx, label})
				sel.subs = append(sel.subs, sIdx)
			} else {
				imageSubs++
			}
			sIdx++
		}
	}

	if imageSubs > 0 {
		pInfo.Printf("  %d image-based subtitle track(s) skipped — MP4 cannot store those (MKV can).\n",
			imageSubs)
	}
	if len(entries) < 2 {
		return sel
	}

	fmt.Println(pterm.Gray("  → Multiple tracks found:"))
	labels := make([]string, 0, len(entries))
	for i, e := range entries {
		fmt.Printf("    %s %s\n", pterm.LightCyan(fmt.Sprintf("[%d]", i+1)), e.label)
		labels = append(labels, e.label)
	}
	timeout := trackPromptTimeout()
	if timeout > 0 {
		fmt.Printf("  Tracks for the MP4 (Enter = all, e.g. 1,3; all after %.0f s): ",
			timeout.Seconds())
	} else {
		fmt.Print("  Tracks for the MP4 (Enter = all, e.g. 1,3): ")
	}
	emitQuestion(questionKindTracks, srcPath, "Enter = all", labels)

	line, ok := readLineTimeout(timeout)
	if !ok {
		fmt.Println()
		return sel
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return sel
	}

	var picked mp4TrackSelection
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(entries) {
			pWarn.Printf("Invalid selection %q ignored.\n", tok)
			continue
		}
		if e := entries[n-1]; e.isSub {
			picked.subs = append(picked.subs, e.rel)
		} else {
			picked.audio = append(picked.audio, e.rel)
		}
	}
	if len(picked.audio) == 0 && len(picked.subs) == 0 {
		fmt.Println(pterm.Gray("  No valid selection — keeping all tracks."))
		return sel
	}
	return picked
}

// readLineTimeout liest eine Zeile von der Tastatur, gibt aber nach timeout
// auf. ok=false heißt „keine Antwort" — der Aufrufer entscheidet dann selbst,
// was die sichere Vorgabe ist.
//
// Der Unterschied zu askYesNoTimeout: Ist die Eingabe gar nicht lesbar (kein
// Konsolenfenster, umgeleitetes stdin), meldet ReadString sofort einen Fehler.
// Dann wird auch sofort abgebrochen statt die volle Wartezeit abzusitzen —
// bei einem Stapel über einen ganzen Ordner wären das sonst 30 Sekunden
// Leerlauf pro Datei, ohne dass jemand antworten könnte.
func readLineTimeout(timeout time.Duration) (string, bool) {
	answer := make(chan string, 1)
	unreadable := make(chan struct{})
	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			close(unreadable)
			return
		}
		answer <- line
	}()
	// timeout <= 0 heißt „unbegrenzt warten". Ein nil-Kanal liefert nie etwas,
	// deshalb bleibt der Zeitzweig dann einfach für immer stumm.
	var expired <-chan time.Time
	if timeout > 0 {
		expired = time.After(timeout)
	}
	select {
	case a := <-answer:
		return a, true
	case <-unreadable:
		return "", false
	case <-expired:
		return "", false
	}
}

// trackPromptTimeout liefert die Bedenkzeit für die Spurauswahl.
//
// Auf der Konsole gibt das Programm nach 30 s auf (siehe
// mp4TrackPromptTimeout). Im -json-Modus sitzt dagegen eine Oberfläche davor,
// die einen Auswahl-Dialog zeigt — dort wäre eine ablaufende Uhr eine Falle:
// Wer in Ruhe ankreuzt, bekäme sonst stillschweigend alle Spuren. Die
// Oberfläche muss dafür immer antworten; schließt sie die Eingabe, greift
// weiterhin die sichere Vorgabe.
func trackPromptTimeout() time.Duration {
	if jsonMode {
		return 0
	}
	return mp4TrackPromptTimeout
}

// mapArgsForSelection baut die -map-Argumente und liefert die zur Auswahl
// passende Tonspur-Liste zurück. buildPerStreamAudioArgs nummeriert seine
// Optionen nach der Reihenfolge im AUSGABE-Stream (:a:0, :a:1, …) — wer nur
// die dritte Tonspur mappt, muss ihr deshalb den Platz 0 geben, sonst laufen
// Bitrate und Filter auf eine Spur, die es in der Ausgabe nicht gibt.
func mapArgsForSelection(sel mp4TrackSelection, all []AudioStreamInfo) (args []string, picked []AudioStreamInfo) {
	args = []string{"-map", "0:V:0"}
	for _, idx := range sel.audio {
		if idx < 0 || idx >= len(all) {
			continue // Probe und Stats uneinig: Spur lieber auslassen als raten
		}
		args = append(args, "-map", fmt.Sprintf("0:a:%d", idx))
		picked = append(picked, all[idx])
	}
	for _, idx := range sel.subs {
		args = append(args, "-map", fmt.Sprintf("0:s:%d", idx))
	}
	return args, picked
}

// ----------------------------------------------------------------------------
// MP4 schreiben
// ----------------------------------------------------------------------------

// writeCompatMP4 stream-copies the video of srcMKV into outMP4 with the recipe
// that gets a file playing on as many devices as possible: HEVC is re-tagged
// "hvc1", any non-AAC audio is transcoded to AAC (48 kHz, ≤5.1 — the
// DaVinci-safe rule), text subtitles become mov_text and "+faststart" moves the
// moov atom to the front. Attachments and data/timecode tracks are dropped; a
// gallery clip needs none. The video itself is never re-encoded here.
func writeCompatMP4(ctx context.Context, srcMKV string, stats *VideoStats, sel mp4TrackSelection, outMP4 string) error {
	mapArgs, pickedAudio := mapArgsForSelection(sel, stats.AudioStreams)

	args := []string{"-y", "-i", srcMKV}
	args = append(args, mapArgs...)
	args = append(args, "-c:v", "copy")
	args = append(args, buildPerStreamAudioArgs(pickedAudio, false, false)...)
	if len(sel.subs) > 0 {
		// mov_text ist das einzige Untertitelformat, das MP4 sicher trägt.
		args = append(args, "-c:s", "mov_text")
	} else {
		args = append(args, "-sn")
	}
	args = append(args, mp4MuxArgs(stats.VideoCodec)...)
	args = append(args, outMP4)

	pterm.NewStyle(pterm.FgLightMagenta, pterm.Bold).
		Println("  >> MP4 (hvc1 + AAC + faststart, lossless video copy)")
	// Reines Umpacken — ein Größenvergleich meldete hier "+0 MB larger".
	return runFFmpeg(ctx, args, stats.DurationSec, 1, 1, 0)
}

// remuxResultToMP4 repackages a finished conversion output (an .mkv we just
// wrote into the "output" folder) into a compatible .mp4 and removes the now
// redundant .mkv. Only -mp4 runs reach here. On any error the .mkv is kept and
// the result is left unchanged, so a failed remux never costs the conversion.
func remuxResultToMP4(ctx context.Context, cfg *AppConfig, res *ProcessResult) {
	if !res.Success || res.IsPreview || res.OutputFile == "" {
		return
	}
	src := res.OutputFile
	// Only OUR freshly written output is remuxed: an .mkv living in the "output"
	// folder. Some result paths report back an untouched original (e.g. an
	// oversized encode was discarded) — that file must stay exactly as it is.
	if !strings.EqualFold(filepath.Ext(src), ".mkv") ||
		!strings.EqualFold(filepath.Base(filepath.Dir(src)), "output") {
		return
	}
	stats, err := getVideoStats(ctx, src)
	if err != nil {
		pWarn.Printf("MP4: cannot probe %s (%v) — MKV kept.\n", filepath.Base(src), err)
		return
	}
	mp4Out, err := uniquePath(strings.TrimSuffix(src, filepath.Ext(src)) + ".mp4")
	if err != nil {
		pWarn.Printf("MP4: no free output name (%v) — MKV kept.\n", err)
		return
	}
	if err := writeCompatMP4(ctx, src, stats, selectionForFile(ctx, cfg, src, stats), mp4Out); err != nil {
		_ = os.Remove(mp4Out)
		if errors.Is(err, context.Canceled) {
			return
		}
		pWarn.Printf("MP4 creation failed (%v) — the MKV is kept: %s\n",
			err, filepath.Base(src))
		return
	}
	if tsErr := copyTimestamps(src, mp4Out); tsErr != nil {
		pWarn.Printf("MP4: could not transfer file timestamps: %v\n", tsErr)
	}
	// The MKV was only the intermediate container; -mp4 wants the MP4.
	_ = os.Remove(src)
	res.OutputFile = mp4Out
	pOK.Printf("    ✓ %s\n", filepath.Base(mp4Out))
}

// selectionForFile ermittelt die Spuren für eine MP4: fragen, sobald es etwas
// zu entscheiden gibt. Scheitert die Probe, bleibt es bei „alles behalten" —
// eine unlesbare Spurliste ist kein Grund, dem Nutzer Ton wegzunehmen.
func selectionForFile(ctx context.Context, cfg *AppConfig, path string, stats *VideoStats) mp4TrackSelection {
	streams, err := probeStreams(ctx, path)
	if err != nil {
		pWarn.Printf("MP4: cannot list tracks (%v) — keeping all of them.\n", err)
		return allTracksSelection(stats)
	}
	return chooseMP4Tracks(streams, path)
}

// allTracksSelection ist der Rückfall ohne Spurliste: alle bekannten Tonspuren,
// keine Untertitel — exakt das Verhalten der Versionen vor 1.10.0.
func allTracksSelection(stats *VideoStats) mp4TrackSelection {
	var sel mp4TrackSelection
	for i := range stats.AudioStreams {
		sel.audio = append(sel.audio, i)
	}
	return sel
}

// convertedMKVToMP4 handles an already-converted .mkv dropped straight onto
// -mp4: instead of skipping it as "already converted", an H.265/H.264 file is
// repackaged losslessly into a compatible <name>.mp4 in the output folder (no
// second encode) and the original .mkv is KEPT — it is the user's finished
// archive / DaVinci copy. AV1 plays on neither iPhones nor most TVs and would
// need a real re-encode from the untouched source (not available here), so it
// is skipped with a clear hint. The startup header for this file was already
// printed by the caller.
func convertedMKVToMP4(ctx context.Context, cfg *AppConfig, filePath string) ProcessResult {
	result := ProcessResult{InputFile: filePath}
	dir := filepath.Dir(filePath)
	base := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	stats, err := getVideoStats(ctx, filePath)
	if err != nil {
		result.ErrMsg = fmt.Sprintf("MP4.go: convertedMKVToMP4: FFprobe error: %v", err)
		result.FailedAt = time.Now()
		return result
	}
	if strings.EqualFold(stats.VideoCodec, "av1") {
		pWarn.Println("  Skipped: AV1 does not play on iPhones and most TVs — re-run -mp4 on the ORIGINAL source.")
		fmt.Println()
		result.Skipped = true
		return result
	}
	// -8bit kann hier nichts ausrichten: dieser Weg packt nur um, er rechnet das
	// Bild nicht neu. Ohne Hinweis sähe die 10-Bit-Datei wie ein Fehler aus.
	if cfg != nil && cfg.eightBit {
		pInfo.Println("  Note: -8bit only applies to a real conversion — this file is repackaged unchanged.")
	}

	outputDir := filepath.Join(dir, "output")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		result.ErrMsg = fmt.Sprintf("MP4.go: convertedMKVToMP4: cannot create output folder: %v", err)
		result.FailedAt = time.Now()
		return result
	}
	if c := cleanFileBaseName(base); c != "" {
		base = c
	}
	outMP4 := filepath.Join(outputDir, base+".mp4")
	if _, statErr := os.Stat(outMP4); statErr == nil {
		fmt.Println(pterm.Gray("  Skipped: MP4 already exists."))
		fmt.Println()
		result.Skipped = true
		return result
	}

	if err := writeCompatMP4(ctx, filePath, stats, selectionForFile(ctx, cfg, filePath, stats), outMP4); err != nil {
		_ = os.Remove(outMP4)
		if errors.Is(err, context.Canceled) {
			result.Skipped = true
			return result
		}
		result.ErrMsg = fmt.Sprintf("MP4.go: convertedMKVToMP4: %v", err)
		result.FailedAt = time.Now()
		pWarn.Printf("  MP4 creation failed: %v\n", err)
		fmt.Println()
		return result
	}
	if tsErr := copyTimestamps(filePath, outMP4); tsErr != nil {
		pWarn.Printf("  Could not transfer file timestamps: %v\n", tsErr)
	}
	pOK.Printf("    ✓ %s\n", filepath.Base(outMP4))
	fmt.Println()
	result.OutputFile = outMP4
	result.Success = true
	return result
}

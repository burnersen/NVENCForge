//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"strings"
	"testing"
)

// hasPair prüft, ob eine Option MIT ihrem Wert in der Argumentliste steht.
// Ein bloßes strings.Contains über alle Argumente würde "-tag:v" auch dann
// bejahen, wenn dahinter der falsche Wert steht.
func hasPair(args []string, opt, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == opt && args[i+1] == val {
			return true
		}
	}
	return false
}

func hasFlag(args []string, opt string) bool {
	for _, a := range args {
		if a == opt {
			return true
		}
	}
	return false
}

// TestMP4MuxArgs ist die Absicherung des eigentlichen Wunsches: JEDE MP4 des
// Programms bekommt faststart, und H.265 bekommt zusätzlich den hvc1-Tag —
// ohne den lehnen die iOS-Fotos-App und DaVinci Resolve die Datei ab.
func TestMP4MuxArgs(t *testing.T) {
	cases := []struct {
		codec   string
		wantTag bool
	}{
		{"hevc", true},
		{"HEVC", true},   // FFprobe schreibt klein, aber verlassen wollen wir uns nicht darauf
		{" hevc ", true}, // Leerzeichen aus einer Probe dürfen den Tag nicht kosten
		{"h265", true},
		{"h264", false}, // avc1 ist in MP4 bereits der richtige Standardwert
		{"av1", false},  // av01 ebenso
		{"", false},     // unbekannt: lieber keinen Tag als einen falschen
	}
	for _, c := range cases {
		args := mp4MuxArgs(c.codec)
		if !hasPair(args, "-movflags", "+faststart") {
			t.Errorf("codec %q: +faststart fehlt (Wiedergabe startet sonst verzögert)", c.codec)
		}
		got := hasPair(args, "-tag:v", "hvc1")
		if got != c.wantTag {
			t.Errorf("codec %q: hvc1-Tag = %v, erwartet %v", c.codec, got, c.wantTag)
		}
	}
}

// TestPrimaryVideoCodecIgnoresCover deckt den Fall ab, der den hvc1-Tag früher
// hätte kosten können: ein eingebettetes Titelbild ist für FFprobe auch
// "video", wird von "-map 0:V:0" aber übersprungen.
func TestPrimaryVideoCodecIgnoresCover(t *testing.T) {
	streams := &ffprobeOutput{Streams: []ffprobeStream{
		{CodecType: "video", CodecName: "mjpeg", Disposition: ffprobeDisposition{AttachedPic: 1}},
		{CodecType: "video", CodecName: "hevc"},
		{CodecType: "audio", CodecName: "aac"},
	}}
	if got := primaryVideoCodec(streams); got != "hevc" {
		t.Errorf("primaryVideoCodec = %q, erwartet \"hevc\" (Cover-Bild muss übersprungen werden)", got)
	}
	if got := primaryVideoCodec(nil); got != "" {
		t.Errorf("primaryVideoCodec(nil) = %q, erwartet leer", got)
	}
	if got := primaryVideoCodec(&ffprobeOutput{}); got != "" {
		t.Errorf("primaryVideoCodec(leer) = %q, erwartet leer", got)
	}
}

// TestMapArgsForSelection prüft die Stelle, an der eine falsche Nummerierung
// unbemerkt bliebe: wer nur die dritte Tonspur behält, muss sie im AUSGABE-
// Stream auf Platz 0 finden, sonst laufen Bitrate und Filter ins Leere.
func TestMapArgsForSelection(t *testing.T) {
	all := []AudioStreamInfo{
		{Codec: "aac", Channels: 2, Language: "eng"},
		{Codec: "ac3", Channels: 6, Language: "ger"},
		{Codec: "dts", Channels: 8, Language: "jpn"},
	}

	args, picked := mapArgsForSelection(mp4TrackSelection{audio: []int{2}}, all)
	if !hasPair(args, "-map", "0:a:2") {
		t.Errorf("gewählte Spur 2 wird nicht gemappt: %v", args)
	}
	if len(picked) != 1 || picked[0].Codec != "dts" {
		t.Errorf("picked = %+v, erwartet genau die dritte Spur", picked)
	}

	// Alle Spuren: Reihenfolge muss erhalten bleiben.
	args, picked = mapArgsForSelection(mp4TrackSelection{audio: []int{0, 1, 2}}, all)
	if len(picked) != 3 || picked[1].Codec != "ac3" {
		t.Errorf("picked = %+v, erwartet alle drei in Originalreihenfolge", picked)
	}
	if !hasPair(args, "-map", "0:V:0") {
		t.Errorf("Videospur fehlt: %v", args)
	}

	// Untertitel kommen als eigene Map-Einträge dazu.
	args, _ = mapArgsForSelection(mp4TrackSelection{audio: []int{0}, subs: []int{1}}, all)
	if !hasPair(args, "-map", "0:s:1") {
		t.Errorf("gewählter Untertitel fehlt: %v", args)
	}

	// Ein Index außerhalb der Liste darf nicht zu einem falschen -map führen:
	// lieber eine Spur weniger als eine geratene.
	args, picked = mapArgsForSelection(mp4TrackSelection{audio: []int{7, -1}}, all)
	if len(picked) != 0 {
		t.Errorf("picked = %+v, erwartet leer bei ungültigen Indizes", picked)
	}
	if hasFlag(args, "0:a:7") {
		t.Errorf("ungültiger Index wurde gemappt: %v", args)
	}
}

// TestEightBitSwitch belegt beides: mit -8bit wechseln ALLE vier Encoder auf
// 8 Bit, und ohne das Flag steht exakt das Alte da (10 Bit) — sonst hätte der
// Schalter still die Bildqualität aller Bestandsläufe geändert.
func TestEightBitSwitch(t *testing.T) {
	defer func(prev bool) { eightBitActive = prev }(eightBitActive)

	eightBitActive = false
	if got := buildNVENCOptsWithCQ(26, "8000k", "16000k", 240); !hasPair(got, "-pix_fmt", "p010le") ||
		!hasPair(got, "-profile:v", "main10") {
		t.Errorf("Vorgabe muss 10 Bit bleiben (main10/p010le): %v", got)
	}
	if got := buildX265OptsWithCQ(18, "8000k", "16000k", 240); !hasPair(got, "-pix_fmt", "yuv420p10le") {
		t.Errorf("x265-Vorgabe muss yuv420p10le bleiben: %v", got)
	}
	if f := buildVideoFilter(true, false, false, cropRect{}); !strings.HasSuffix(f, ",format=p010le") {
		t.Errorf("Filterkette endet auf %q, erwartet format=p010le", f)
	}

	eightBitActive = true
	if got := buildNVENCOptsWithCQ(26, "8000k", "16000k", 240); !hasPair(got, "-pix_fmt", "yuv420p") ||
		!hasPair(got, "-profile:v", "main") {
		t.Errorf("-8bit: NVENC muss main/yuv420p nutzen: %v", got)
	}
	if got := buildAV1OptsWithCQ(32, "6000k", "12000k", 240); !hasPair(got, "-pix_fmt", "yuv420p") {
		t.Errorf("-8bit: AV1 muss yuv420p nutzen: %v", got)
	}
	if got := buildX265OptsWithCQ(18, "8000k", "16000k", 240); !hasPair(got, "-pix_fmt", "yuv420p") ||
		!hasPair(got, "-profile:v", "main") {
		t.Errorf("-8bit: x265 muss main/yuv420p nutzen: %v", got)
	}
	if got := buildSVTAV1OptsWithCQ(24, "6000k", "12000k", 240); !hasPair(got, "-pix_fmt", "yuv420p") {
		t.Errorf("-8bit: SVT-AV1 muss yuv420p nutzen: %v", got)
	}
	// Die Filterkette muss mitziehen: bliebe sie auf p010le, würde FFmpeg
	// unnötig zweimal wandeln.
	if f := buildVideoFilter(true, false, false, cropRect{}); !strings.HasSuffix(f, ",format=yuv420p") {
		t.Errorf("-8bit: Filterkette endet auf %q, erwartet format=yuv420p", f)
	}
	if f := buildVideoFilter(false, false, false, cropRect{}); !strings.HasSuffix(f, ",format=yuv420p") {
		t.Errorf("-8bit: Crop-Kette endet auf %q, erwartet format=yuv420p", f)
	}
}

// TestGPUScaleChain deckt den Fehler ab, der die erste Messreihe gekostet hat:
// bleibt das Bild im Grafikspeicher, darf der Encoder KEIN -pix_fmt bekommen.
// Und die Kette muss sich nach casStrength richten, weil es für die Karte
// keinen CAS-Filter gibt.
func TestGPUScaleChain(t *testing.T) {
	defer func(cas float64, eight, oncard bool) {
		appSettings.casStrength, eightBitActive, gpuFramesStayOnCard = cas, eight, oncard
	}(appSettings.casStrength, eightBitActive, gpuFramesStayOnCard)
	eightBitActive = false

	// Mit Nachschärfen: das Bild muss für cas zurück in den Arbeitsspeicher.
	appSettings.casStrength = 0.4
	chain := buildVideoFilter(true, false, true, cropRect{})
	if !strings.Contains(chain, "scale_cuda") || !strings.Contains(chain, "interp_algo=lanczos") {
		t.Errorf("GPU-Kette fehlt oder skaliert nicht mit Lanczos: %q", chain)
	}
	if !strings.Contains(chain, "hwdownload") || !strings.Contains(chain, "cas=strength=0.4") {
		t.Errorf("mit casStrength>0 muss die Kette herunterladen und schärfen: %q", chain)
	}
	if chainEndsOnCard(chain) {
		t.Errorf("Kette mit hwdownload endet nicht auf der Karte: %q", chain)
	}

	// Ohne Nachschärfen: das Bild bleibt oben, kein hwdownload.
	appSettings.casStrength = 0
	chain = buildVideoFilter(true, false, true, cropRect{})
	if strings.Contains(chain, "hwdownload") || strings.Contains(chain, "cas=") {
		t.Errorf("ohne CAS darf die Kette den Grafikspeicher nicht verlassen: %q", chain)
	}
	if !chainEndsOnCard(chain) || !chainUsesGPU(chain) {
		t.Errorf("Kette müsste auf der Karte enden: %q", chain)
	}
	// Für den VMAF-Vergleich muss der Rückweg angehängt werden können.
	if cpu := filterChainToCPU(chain); !strings.HasSuffix(cpu, "hwdownload,format=p010le") {
		t.Errorf("filterChainToCPU hängt den Rückweg nicht an: %q", cpu)
	}

	// 8 Bit heißt im Grafikspeicher nv12 — ein yuv420p gibt es dort nicht.
	eightBitActive = true
	chain = buildVideoFilter(true, false, true, cropRect{})
	if !strings.Contains(chain, "format=nv12") {
		t.Errorf("-8bit auf der Karte braucht nv12: %q", chain)
	}
	eightBitActive = false

	// Der entscheidende Punkt: kein -pix_fmt, solange die Bilder oben liegen.
	gpuFramesStayOnCard = true
	if got := buildNVENCOptsWithCQ(26, "8000k", "16000k", 240); hasFlag(got, "-pix_fmt") {
		t.Errorf("Encoder darf bei Bildern im Grafikspeicher kein -pix_fmt setzen: %v", got)
	}
	if got := buildAV1OptsWithCQ(32, "6000k", "12000k", 240); hasFlag(got, "-pix_fmt") {
		t.Errorf("AV1-Encoder darf bei Bildern im Grafikspeicher kein -pix_fmt setzen: %v", got)
	}
	gpuFramesStayOnCard = false
	if got := buildNVENCOptsWithCQ(26, "8000k", "16000k", 240); !hasPair(got, "-pix_fmt", "p010le") {
		t.Errorf("auf dem üblichen Weg muss -pix_fmt gesetzt sein: %v", got)
	}
}

// TestGPUScaleUsable prüft die Vorbedingungen: ohne Bilder auf der Karte, ohne
// Verkleinern, mit Deinterlacing (bwdif rechnet auf dem Prozessor) oder mit
// Auto-Crop (scale_cuda kann nicht zuschneiden) darf die GPU-Kette nicht
// gewählt werden.
func TestGPUScaleUsable(t *testing.T) {
	hw := []string{"-hwaccel", "cuda"}
	cases := []struct {
		name             string
		hw               []string
		scale, int, crop bool
		want             bool
	}{
		{"alles passt", hw, true, false, false, true},
		{"kein GPU-Entpacken", nil, true, false, false, false},
		{"nichts zu verkleinern", hw, false, false, false, false},
		{"interlaced braucht bwdif", hw, true, true, false, false},
		{"Auto-Crop muss auf den Prozessor", hw, true, false, true, false},
	}
	for _, c := range cases {
		if got := gpuScaleUsable(c.hw, c.scale, c.int, c.crop); got != c.want {
			t.Errorf("%s: gpuScaleUsable = %v, erwartet %v", c.name, got, c.want)
		}
	}
}

// TestFallBackToCPUDecode sichert den Rückweg ab, wenn die Karte den Lauf
// abweist: ohne Tausch der Filterkette liefe scale_cuda ins Leere, und ohne
// das zurückgegebene -pix_fmt wüsste der Encoder sein Eingangsformat nicht.
func TestFallBackToCPUDecode(t *testing.T) {
	job := convJob{
		hwaccelOpts:  []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"},
		vfOpts:       []string{"-vf", "scale_cuda=1920:1080:interp_algo=lanczos:format=p010le"},
		vfOptsCPU:    []string{"-vf", "scale=1920:1080,format=p010le"},
		encodeOnCard: true,
		nvencOpts:    []string{"-c:v", "hevc_nvenc", "-cq", "26"},
	}
	back := job.fallBackToCPUDecode()
	if len(back.hwaccelOpts) != 0 {
		t.Errorf("Rückfall muss ohne Grafikkarte laufen: %v", back.hwaccelOpts)
	}
	if hasFlag(back.vfOpts, "scale_cuda=1920:1080:interp_algo=lanczos:format=p010le") {
		t.Errorf("Filterkette wurde nicht getauscht: %v", back.vfOpts)
	}
	if !hasPair(back.vfOpts, "-vf", "scale=1920:1080,format=p010le") {
		t.Errorf("CPU-Filterkette fehlt: %v", back.vfOpts)
	}
	if !hasPair(back.nvencOpts, "-pix_fmt", "p010le") {
		t.Errorf("Eingangsformat wurde nicht zurückgegeben: %v", back.nvencOpts)
	}
	if back.encodeOnCard {
		t.Error("encodeOnCard muss nach dem Rückfall aus sein")
	}
	// Der übliche Weg (nie auf der Karte skaliert) darf nicht angefasst werden.
	plain := convJob{
		hwaccelOpts: []string{"-hwaccel", "cuda"},
		vfOpts:      []string{"-vf", "scale=1920:1080,format=p010le"},
		nvencOpts:   []string{"-c:v", "hevc_nvenc", "-pix_fmt", "p010le"},
	}
	back = plain.fallBackToCPUDecode()
	if !hasPair(back.vfOpts, "-vf", "scale=1920:1080,format=p010le") ||
		len(back.nvencOpts) != 4 {
		t.Errorf("Job ohne GPU-Kette wurde verändert: %v / %v", back.vfOpts, back.nvencOpts)
	}
}

// TestAllTracksSelection sichert den Rückfall ab: ist die Spurliste nicht
// lesbar, behält das Programm alle Tonspuren — es darf niemals still Ton
// wegwerfen, nur weil eine Probe fehlschlug.
func TestAllTracksSelection(t *testing.T) {
	stats := &VideoStats{AudioStreams: []AudioStreamInfo{{Codec: "aac"}, {Codec: "ac3"}}}
	sel := allTracksSelection(stats)
	if len(sel.audio) != 2 || sel.audio[0] != 0 || sel.audio[1] != 1 {
		t.Errorf("allTracksSelection = %+v, erwartet beide Tonspuren", sel)
	}
	if len(sel.subs) != 0 {
		t.Errorf("Rückfall darf keine Untertitel erfinden: %+v", sel)
	}
}

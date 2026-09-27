//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pterm/pterm"
)

// ----------------------------------------------------------------------------
// Auto-CQ (-autocq): per-file CQ search via sampled VMAF measurements.
//
// Idea: place a few short sample windows on the source's bitrate profile
// (the heaviest scene is always part of the sample), encode them at two
// anchor CQ values with EXACTLY the settings of the real encode, measure
// VMAF against the identically filtered source, interpolate the CQ that
// should hit the configured quality target (autoCQTargetVMAF, default 96),
// then confirm the pick by measuring it. Since 2.0.0 the target is a FLOOR,
// as in CloudForge: a pick that misses it is followed by further measured
// steps until one holds it, and a pick that clears it by more than
// autoCQThriftyMargin tries one step thriftier. Only a target that is proven
// unreachable or too expensive may end below it: a saturated curve
// (pre-compressed source) falls back to the cheapest CQ on the measured
// plateau, and there rungs above the pick (up to the clamp ceiling) are
// probed too and taken when their measured score stays within
// autoCQPlateauTolerance of the plateau top — the file shrinks as far as real
// measurements justify, never on extrapolation.
// The same samples predict what re-encoding saves on the whole file
// (autoCQExpectedSavingPercent); processFile remuxes instead when that stays
// under minSavePercent. Close to that limit, a separate size probe across the
// whole film settles it (autoCQSizeProbeNeeded).
// H.265 and AV1 both run this search — the per-codec numbers (anchors, clamps,
// saturation slope, encoder) live in autoCQScale; av1_nvenc uses a wider CQ
// scale (1-63), so its anchors and clamps differ from H.265.
//
// The three documented VMAF measurement pitfalls are handled here:
//   1. A decoded segment keeps the source's start offset, which shifts the
//      frame pairing → setpts=PTS-STARTPTS re-bases every window before use.
//   2. Matroska rounds PTS to milliseconds and libvmaf pairs by timestamp,
//      which makes the comparison jump by one frame → both VMAF inputs get
//      settb=AVTB,setpts=N*fpsDen/fpsNum/TB (frame-number-based timestamps).
//   3. Gaps in the source timeline (missing frames) are filled in by the
//      sample encode through -fps_mode cfr but not on the reference side,
//      which pairs unrelated frames from the first gap on → both sides run
//      through fps= as well (autoCQWindowPrep, measured details there).
// ----------------------------------------------------------------------------

const (
	// Window placement via the source bitrate profile: the first/last 5% are
	// skipped (intros, credits), and a profile whose heaviest bucket stays
	// under 1.25x the median carries no placement signal (CBR-ish source) —
	// the fixed positions are used instead. Max/median (not p90/p10) so a
	// single hard scene inside an otherwise calm film still counts as signal.
	autoCQEdgeMarginPct    = 0.05
	autoCQFlatProfileRatio = 1.25

	// Reporting thresholds for gaps in the source timeline. Below one second
	// of missing material a hiccup is not worth a line; above a quarter of all
	// frames it is not the timeline that has holes but the declared frame rate
	// that is wrong (some containers report twice the real rate), and the
	// notice would mislead. The repair itself runs either way.
	autoCQGapNoticeMinSec   = 1.0
	autoCQGapNoticeMaxShare = 0.25

	// Hard timeout for the packet-size demux (no decode — normally seconds).
	autoCQProfileTimeout = 2 * time.Minute

	// Sampling layout: long sources get three 8-second windows, short sources
	// (under 4 minutes) two 6-second windows, and anything under 30 seconds
	// is not sampled at all (falls back to the configured targetCQ).
	// Three windows instead of four since a 2026-07-12 A/B series: across
	// H.265 and AV1 on long real sources the picks were identical while the
	// analysis ran 22-32% faster. SHORTER windows are not safe — 6 s and 4 s
	// windows shifted the pick by 2-4 CQ steps in the same series, so the
	// window LENGTH must stay at 8 s.
	//
	// The 6 s below is the one number that series never covered: every file
	// in it ran past four minutes, so all of them took the long path. A
	// short source is judged on 2 x 6 s = 12 s of material — less than any
	// layout that series tried, including the ones it rejected for moving
	// the pick. So the 6 s is an inherited value, not a measured one.
	// Settling it needs its own A/B on sources between 30 s and 4 minutes;
	// until someone runs that, it stays, because raising it unmeasured
	// would be the same mistake pointing the other way.
	autoCQMinSourceSec   = 30.0
	autoCQShortSourceSec = 240.0
	autoCQWindowSec      = 8.0
	autoCQShortWindowSec = 6.0

	// Hard per-step timeout: a wedged sample encode or VMAF run must never
	// stall the whole batch (the main encode has its own stall watchdog).
	autoCQStepTimeout = 10 * time.Minute

	// The spinner repaints its line in place, and plain conhost does not
	// clear the old line first — a shorter new text leaves fragments of a
	// longer predecessor standing ("... (43s)s) (6s)"). Padding every phase
	// text to the width of the longest one makes each repaint cover the
	// whole previous line; the trailing "(Ns)" timer only ever grows.
	autoCQSpinnerScanText  = "Auto-CQ: scanning source bitrate profile..."
	autoCQSpinnerTextWidth = len(autoCQSpinnerScanText)
)

// autoCQScale holds the per-codec CQ-scale parameters of the Auto-CQ search.
// The mechanism is identical for H.265 and AV1 — only the numbers differ,
// because av1_nvenc uses a wider CQ scale (1-63) on which the same VMAF change
// spans about twice as many steps. Both anchor pairs come from a real
// VMAF-over-CQ measurement series (H.265: NVENCForge_Qualitaetsanalyse.md;
// AV1: measured 2026-07-06). The VMAF target is NOT part of this struct — it
// measures quality, not CQ, so both codecs share it.
type autoCQScale struct {
	// anchorLow/anchorHigh: the two calibration CQs. anchorLow is the better,
	// larger-file end; together they bracket the practically useful range.
	anchorLow, anchorHigh int
	// clampMin/clampMax: the final pick never leaves this range. Below min the
	// gains are invisible, above max even easy material visibly degrades.
	clampMin, clampMax int
	// maxStepDown caps how many CQ steps a verification miss corrects in one go,
	// so a single noisy measurement cannot push the pick into oversized files.
	maxStepDown int
	// saturationSlope: below this measured VMAF gain per CQ step the curve
	// counts as saturated (a pre-compressed source whose score plateaus). It
	// scales with the step width — one AV1 step is worth about half an H.265 step.
	saturationSlope float64
	// minGainPerStep: how much VMAF one CQ step below the low anchor has to buy
	// before it is worth its price in file size. Where saturationSlope asks "is
	// this curve dead", this asks the user question "does the next step pay for
	// itself" — so it sits well above it. Measured 2026-08-27 on a 50 fps source:
	// one H.265 step costs ~7% file size, and the user had already rejected
	// +0.49 VMAF for +6.6% as invisible, so 0.30 is the conservative side of a
	// trade he has already made. Scales with the step width like saturationSlope.
	minGainPerStep float64
	// buildOpts assembles the real encoder options at a given CQ, so the sample
	// encodes match the actual encode bit for bit.
	buildOpts func(cq int, gop int) []string
	// fallbackCQ is the configured fixed CQ used (and reported) when the
	// analysis cannot run. Read lazily so an INI value applied after startup wins.
	fallbackCQ func() int
	// codecLabel names the codec in progress and warning messages.
	codecLabel string
}

// hevcAutoCQScale keeps the exact H.265 constants of the original Auto-CQ
// implementation (anchors 26/30, clamp 20-34, saturation 0.1) unchanged, so
// H.265 behaves identically to before.
var hevcAutoCQScale = autoCQScale{
	anchorLow: 26, anchorHigh: 30,
	clampMin: 20, clampMax: 34,
	maxStepDown:     3,
	saturationSlope: 0.10,
	minGainPerStep:  0.30,
	buildOpts:       buildNVENCOptsWithCQ,
	fallbackCQ:      func() int { return appSettings.targetCQ },
	codecLabel:      "H.265",
}

// av1AutoCQFallbackCQ is the CQ the AV1 Auto-CQ search falls back to when its
// analysis cannot run (clip too short, unknown frame rate, libvmaf missing). It
// is deliberately NOT av1TargetCQ: that value (32 ≈ VMAF 94) is a lean manual-
// mode setting, too far below the VMAF target (default 96) for a graceful fallback. 24
// equals the low anchor (≈ VMAF 96), so an unmeasurable AV1 clip lands in the
// neighbourhood of the search intent instead of visibly softer, while manual AV1
// mode keeps its own av1TargetCQ. H.265 needs no such constant — its manual
// targetCQ (26) IS the low anchor of the search and therefore lands at the top
// of the measured range, not below the target.
const av1AutoCQFallbackCQ = 24

// av1AutoCQScale mirrors it on the wider av1_nvenc scale. The numbers come from
// the 2026-07-06 VMAF series: VMAF 97 sits near AV1 CQ ~20-24 (not 32), so the
// anchors are 24/32 with an ~2-point VMAF span like the H.265 pair; the clamp
// and the halved saturation slope follow the scale being about twice as fine.
var av1AutoCQScale = autoCQScale{
	anchorLow: 24, anchorHigh: 32,
	clampMin: 16, clampMax: 44,
	maxStepDown:     6,
	saturationSlope: 0.05,
	minGainPerStep:  0.15,
	buildOpts:       buildAV1OptsWithCQ,
	fallbackCQ:      func() int { return av1AutoCQFallbackCQ },
	codecLabel:      "AV1",
}

// x265AutoCQScale ist das Auto-CQ-Profil für den CPU-Modus (-cpu) mit
// libx265. Alle Zahlen stammen aus der VMAF-Messreihe vom 2026-07-25
// (vier Quellen: Animation, Realfilm 1080p60, 4K-HDR mit Downscale,
// vorkomprimiertes Material), gemessen mit derselben Mechanik, die auch
// im Betrieb läuft.
//
// Anker 18/22: VMAF 96 wird bei frischem Material zwischen CRF 17,3 und
// 18,5 erreicht (Realfilm 18,0 / Animation 18,5 / 4K 17,3); stark
// vorkomprimiertes Material liegt erwartungsgemäß höher (22,6) und wird
// von der Plateau-Logik abgefangen. Merkregel aus derselben Messung:
// x265-CRF entspricht etwa NVENC-CQ minus 7.
//
// Die schrittabhängigen Werte sind NICHT von NVENC abgeschrieben, sondern
// mit der gemessenen Schrittbreite skaliert: ein x265-CRF-Schritt ist
// 0,64 VMAF wert, ein NVENC-CQ-Schritt 0,79 — Faktor 0,81. Daraus folgen
// die feinere Sättigungsschwelle (0,08 statt 0,10) und ein Schritt mehr
// Korrekturweite (4 statt 3).
var x265AutoCQScale = autoCQScale{
	anchorLow: 18, anchorHigh: 22,
	clampMin: 12, clampMax: 28,
	maxStepDown:     4,
	saturationSlope: 0.08,
	minGainPerStep:  0.24,
	buildOpts:       buildX265OptsWithCQ,
	fallbackCQ:      func() int { return appSettings.cpuTargetCRF },
	codecLabel:      "H.265 (CPU)",
}

// svtav1AutoCQFallbackCRF ist der CRF, auf den die AV1-Suche im CPU-Modus
// zurückfällt, wenn sie nicht messen kann. Wie bei av1_nvenc bewusst NICHT
// cpuAV1TargetCRF (32 = magerer Handbetriebswert), sondern der untere Anker,
// damit ein nicht messbarer Clip nahe am Qualitätsziel landet.
const svtav1AutoCQFallbackCRF = 24

// svtav1AutoCQScale ist das Auto-CQ-Profil für den CPU-Modus mit -av1
// (libsvtav1). Anker und Klemmen stammen aus der Messreihe vom 2026-07-25;
// am 2026-09-24 mit SVT-AV1 4.2 (FFmpeg n9.0.1) nachgeprüft: sieben Quellen,
// Presets 6/8/9/10, dazu Vergleichsläufe des echten Programms (-cqcheck).
//
// Neu seit dieser Prüfung sind nur die schrittabhängigen Werte. Ein
// SVT-CRF-Schritt ist mit SVT-AV1 4.2 nur noch rund 0,21 VMAF wert (vorher
// 0,30). Mit dem alten Mindestgewinn 0,18 verwarf die Suche an einer
// schweren 1080p50-Quelle den Schritt auf CRF 19 (0,17 VMAF je Stufe) und
// blieb bei VMAF 94,5 stehen; mit 0,12 nimmt sie CRF 19 mit 95,3. Die
// Sättigungsschwelle ist im selben Verhältnis gesenkt (0,04).
//
// Bewusst NICHT geändert, obwohl die Schrittbreite es nahelegt:
//   - Anker 24/32: tiefere Anker (20/28) trafen auf festen Messfenstern
//     besser, im echten Programm (bitraten-geführte Fenster) verfehlte ein
//     leichter Realfilm damit aber das Ziel nach oben (CRF 29 mit VMAF 97,7
//     statt CRF 32 mit 96,6) und die Analyse dauerte fast doppelt so lange.
//   - Korrekturweite 5: mit 7 schoss derselbe Fall über das Ziel hinaus.
//
// WICHTIG: Jedes SVT-Preset hat eine Qualitätsobergrenze, und sie sinkt mit
// dem Tempo — gemessen bei CRF 16: Preset 6 96,6-98,8, Preset 9 96,2-98,2,
// Preset 10 nur 94,0-96,3. Darunter bringen niedrigere CRF-Werte nur größere
// Dateien; das fängt die Plateau-Logik ab, ab Preset 10 warnt zusätzlich
// svtPresetCeilingWarning.
var svtav1AutoCQScale = autoCQScale{
	anchorLow: 24, anchorHigh: 32,
	clampMin: 16, clampMax: 44,
	maxStepDown:     5,
	saturationSlope: 0.04,
	minGainPerStep:  0.12,
	buildOpts:       buildSVTAV1OptsWithCQ,
	fallbackCQ:      func() int { return svtav1AutoCQFallbackCRF },
	codecLabel:      "AV1 (CPU)",
}

const (
	// svtMaxPreset ist das schnellste Preset, das SVT-AV1 4.x noch kennt.
	// 12 und 13 nimmt es an, bildet sie aber still auf 11 ab ("Preset M12 is
	// mapped to M11", gemessen 2026-09-24).
	svtMaxPreset = 11
	// svtLegacyMaxPreset ist die alte Obergrenze (SVT-AV1 bis 3.x). INIs mit
	// 12 oder 13 laufen mit svtMaxPreset weiter — genau das bekamen sie schon.
	svtLegacyMaxPreset = 13
	// Ab svtCeilingPreset erreicht SVT-AV1 hohe VMAF-Ziele gar nicht mehr,
	// egal wie viele Daten es bekommt: gemessen bei CRF 16 an sieben Quellen
	// höchstens 94,0-96,3 (Preset 10), 95,3-95,4 (Preset 11).
	svtCeilingPreset = 10
	// svtCeilingMinTarget ist das VMAF-Ziel, ab dem diese Obergrenze greift.
	svtCeilingMinTarget = 96.0
)

// svtPresetCeilingWarning liefert die Warnung für ein SVT-Preset, das das
// eingestellte VMAF-Ziel nicht erreichen kann — oder "", wenn es passt.
// active fasst zusammen, ob die Warnung überhaupt zählt (CPU-Modus, AV1 und
// Auto-CQ): ohne Auto-CQ gibt es kein Ziel, das verfehlt werden könnte.
// Gewarnt wird einmal pro Lauf, weil Auto-CQ sonst bei jeder Datei still am
// Deckel entlangsucht und niemand erfährt, warum die Dateien groß werden.
func svtPresetCeilingWarning(active bool, preset int, target float64) string {
	if !active || preset < svtCeilingPreset || target < svtCeilingMinTarget {
		return ""
	}
	return fmt.Sprintf("cpuAV1Preset=%d cannot reach VMAF %.4g: from preset %d on, SVT-AV1 tops out "+
		"around VMAF 94-96 however many bits it gets. Auto-CQ will settle below the target "+
		"with large files — cpuAV1Preset 6 reaches it on most material, 9 is the fast compromise.",
		preset, target, svtCeilingPreset)
}

// checkLibVMAF reports whether the FFmpeg build carries the libvmaf filter.
// The auto-downloaded BtbN GPL build has it; slim third-party builds may not.
func checkLibVMAF() error {
	cmd := exec.Command(ffmpegPath, "-v", "error", "-filters")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: winCREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("AutoCQ.go: checkLibVMAF: cannot read FFmpeg filter list: %w", err)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[1] == "libvmaf" {
			return nil
		}
	}
	return errors.New("AutoCQ.go: checkLibVMAF: libvmaf filter missing in this FFmpeg build")
}

// autoCQSampleWindows returns the (start, length) sample windows in seconds
// for a source of the given duration. Start and end of the video are avoided
// (intros, credits, fade-outs are not representative). Returns nil when the
// source is too short for a meaningful measurement.
func autoCQSampleWindows(durationSec float64) [][2]float64 {
	if durationSec < autoCQMinSourceSec {
		return nil
	}
	windowLen := autoCQWindowSec
	positions := []float64{0.20, 0.45, 0.70}
	if durationSec < autoCQShortSourceSec {
		windowLen = autoCQShortWindowSec
		positions = []float64{0.20, 0.60}
	}
	windows := make([][2]float64, 0, len(positions))
	for _, p := range positions {
		start := durationSec * p
		if maxStart := durationSec - windowLen - 2; start > maxStart {
			start = maxStart
		}
		if start < 0 {
			start = 0
		}
		windows = append(windows, [2]float64{start, windowLen})
	}
	return windows
}

// interpolateAutoCQ maps the two anchor measurements linearly onto the CQ that
// should hit the VMAF target, rounded and clamped to the scale's clamp range.
// The second return value is the VMAF predicted for that CQ. A near-flat (or
// rising) slope means the two anchors measured practically the same — pure
// measurement noise on very easy or very hard material — so the pick falls to
// the clamp edge matching the side of the target.
func interpolateAutoCQ(sc autoCQScale, vmafLow, vmafHigh, target float64) (int, float64) {
	slope := (vmafHigh - vmafLow) / float64(sc.anchorHigh-sc.anchorLow)
	var exact, predicted float64
	if slope > -0.01 {
		if vmafHigh >= target {
			exact, predicted = float64(sc.clampMax), vmafHigh
		} else {
			exact, predicted = float64(sc.clampMin), vmafLow
		}
	} else {
		exact = float64(sc.anchorLow) + (target-vmafLow)/slope
	}
	cq := int(math.Round(exact))
	if cq < sc.clampMin {
		cq = sc.clampMin
	}
	if cq > sc.clampMax {
		cq = sc.clampMax
	}
	if slope <= -0.01 {
		predicted = vmafLow + slope*float64(cq-sc.anchorLow)
	}
	return cq, predicted
}

// autoCQStepDown returns the CQ to measure next after a measurement missed the
// target. The step count comes from the slope (how much VMAF one CQ step
// buys), capped at the scale's maxStepDown so a single noisy measurement cannot
// jump into oversized files, and clamped at clampMin — so the returned CQ
// equals the input when the clamp floor is already reached. Since 2.0.0 the
// stepped CQ is always measured; until then it was often taken on the estimate
// alone, and the estimate below the low anchor is provably too optimistic.
func autoCQStepDown(sc autoCQScale, cq int, target, verified, slope float64) int {
	steps := 1
	if slope < -0.01 {
		if s := int(math.Ceil((target - verified) / -slope)); s > steps {
			steps = s
		}
	}
	steps = min(steps, sc.maxStepDown)
	return max(cq-steps, sc.clampMin)
}

// autoCQSaturated reports whether the verification measurement below the
// low anchor exposes a saturated VMAF curve: the measured gain per CQ step
// from the anchor down to the pick stays under the scale's saturationSlope. The
// anchor slope alone cannot see this — saturation starts left of the
// anchors, exactly where the verification measurement sits.
func autoCQSaturated(sc autoCQScale, cq int, verified, vmafLow float64) bool {
	if cq >= sc.anchorLow {
		return false
	}
	return (verified-vmafLow)/float64(sc.anchorLow-cq) < sc.saturationSlope
}

// autoCQGainTooSmall reports whether stepping below the low anchor still buys
// measurable quality, but too little to pay for the file size it costs. This is
// NOT saturation: autoCQSaturated (a much lower threshold) asks whether the
// curve is dead, and it is checked first. This one asks the question the user
// actually cares about — is the next step worth its price? Below the low anchor
// the anchor slope is provably too optimistic (measured 2026-08-27: it promised
// VMAF 98.0 where the encode delivered 97.5), so every step taken there is an
// extrapolation paid for in real bitrate.
func autoCQGainTooSmall(sc autoCQScale, cq int, verified, vmafLow float64) bool {
	if cq >= sc.anchorLow {
		return false
	}
	return (verified-vmafLow)/float64(sc.anchorLow-cq) < sc.minGainPerStep
}

// autoCQSampleKbps returns the video bitrate of a finished sample encode in
// kbit/s. The sample file holds exactly the analysis windows and nothing else
// (no audio, no subtitles, "-an -sn"), so its size over their total length is
// the rate the real encode produces on this very material — measured, not
// modelled, and free: the file is already on disk from the quality search.
func autoCQSampleKbps(tmpDir string, cq int, sampleSec float64) (float64, error) {
	if sampleSec <= 0 {
		return 0, errors.New("sample window length unknown")
	}
	info, err := os.Stat(filepath.Join(tmpDir, fmt.Sprintf("sample_cq%d.mkv", cq)))
	if err != nil {
		return 0, err
	}
	return float64(info.Size()) * 8 / 1000 / sampleSec, nil
}

// autoCQWindowSourceKbps returns the source's video bitrate AT THE GIVEN
// WINDOWS, averaged over them.
//
// The saving prediction compares an encode of these windows against the
// source, and that comparison only holds when both sides describe the same
// seconds of film. With bitrate-guided placement the analysis windows sit
// deliberately on the heaviest scenes, where the source runs well above its
// own average — judging them against the whole-file average would get the
// share wrong. A window overlapping two buckets is weighted by the share it
// covers of each. Returns 0 when no profile exists; the caller then makes no
// prediction instead of guessing.
func autoCQWindowSourceKbps(buckets []bitrateBucket, windows [][2]float64, bucketLen float64) float64 {
	if len(buckets) == 0 || len(windows) == 0 || bucketLen <= 0 {
		return 0
	}
	var weighted, seconds float64
	for _, w := range windows {
		start, length := w[0], w[1]
		if length <= 0 {
			continue
		}
		for _, b := range buckets {
			if b.kbps <= 0 {
				continue
			}
			overlap := math.Min(start+length, b.startSec+bucketLen) - math.Max(start, b.startSec)
			if overlap <= 0 {
				continue
			}
			weighted += b.kbps * overlap
			seconds += overlap
		}
	}
	if seconds <= 0 {
		return 0
	}
	return weighted / seconds
}

// autoCQBitrateRate derives the decay constant of the bitrate-over-CQ curve
// from the two anchor samples. Bitrate falls close to exponentially with CQ,
// so one constant describes the whole curve. Returns 0 when the two samples
// do not form a falling curve (measurement noise on flat material) — the
// caller must then leave the pick alone rather than extrapolate from nonsense.
func autoCQBitrateRate(sc autoCQScale, kbpsLow, kbpsHigh float64) float64 {
	if kbpsLow <= 0 || kbpsHigh <= 0 || kbpsLow <= kbpsHigh || sc.anchorHigh <= sc.anchorLow {
		return 0
	}
	return math.Log(kbpsLow/kbpsHigh) / float64(sc.anchorHigh-sc.anchorLow)
}

// autoCQEstimateKbps evaluates that curve at one CQ.
func autoCQEstimateKbps(sc autoCQScale, kbpsLow, rate float64, cq int) float64 {
	return kbpsLow * math.Exp(-rate*float64(cq-sc.anchorLow))
}

// autoCQPlateauPick returns the pick on a curve whose reachable quality tops
// out below the target: the low anchor (its measurement IS the plateau, minus
// noise) — or, when even the anchor span is flat, the high anchor: the whole
// measured curve is level then and the extra bitrate of the low anchor buys
// nothing. Further savings are left to the plateau climb, which measures every
// rung. Until 1.34.0 the removed autoCQTolerance bought extra steps here on the
// anchor slope alone.
func autoCQPlateauPick(sc autoCQScale, vmafLow, vmafHigh float64) (int, float64) {
	anchorGainPerStep := (vmafLow - vmafHigh) / float64(sc.anchorHigh-sc.anchorLow)
	if anchorGainPerStep < sc.saturationSlope {
		return sc.anchorHigh, vmafHigh
	}
	return sc.anchorLow, vmafLow
}

// ----------------------------------------------------------------------------
// The target as a floor (since 2.0.0, CloudForge's rules)
// ----------------------------------------------------------------------------

// autoCQThriftyMargin: a measured pick that clears the target by more than
// this tries one CQ step thriftier. Near the top of the curve 0.5 VMAF is
// about one H.265 CQ step (0.79 per step in the 2026-07-25 series) and one to
// two AV1 steps, so a pick this far above the target is most likely a step
// too generous.
const autoCQThriftyMargin = 0.5

// autoCQMaxHoldSteps limits the extra measurements the search may spend to
// make a missed pick hold the target — each costs a sample encode plus a VMAF
// run (10-20 s on the graphics card). Narrowing a gap needs two or three,
// stepping below the low anchor a few more; the limit only guards against a
// curve that jumps about.
const autoCQMaxHoldSteps = 6

// autoCQPoint is one measured step of the search.
type autoCQPoint struct {
	cq   int
	vmaf float64
}

// autoCQScorer returns the VMAF of one CQ — from memory when that step was
// measured before, otherwise by a new sample encode and measurement.
type autoCQScorer func(cq int) (float64, error)

// autoCQHoldOutcome says how the search for a CQ that holds the target ended.
type autoCQHoldOutcome int

const (
	holdReached      autoCQHoldOutcome = iota // a measured CQ reaches the target
	holdClampFloor                            // even the clamp floor misses it: proven unreachable
	holdSaturated                             // below the low anchor the curve is dead
	holdTooExpensive                          // below the low anchor each step buys too little
	holdGaveUp                                // a measurement failed or the step limit ran out
)

// autoCQHold is what autoCQHoldTarget ends on: the outcome and the measured
// point it rests on (for holdGaveUp the last miss).
type autoCQHold struct {
	outcome autoCQHoldOutcome
	point   autoCQPoint
}

// autoCQThriftiestHolding returns, among the measured points, the one with the
// highest CQ (smallest file) that still reaches the target.
func autoCQThriftiestHolding(scores map[int]float64, target float64) (autoCQPoint, bool) {
	var best autoCQPoint
	found := false
	for cq, score := range scores {
		if score >= target && (!found || cq > best.cq) {
			best, found = autoCQPoint{cq, score}, true
		}
	}
	return best, found
}

// autoCQLocalSlope returns the VMAF change per CQ step between a point and its
// nearest measured neighbour at a higher CQ — the curve right where the search
// stands. Below the low anchor the anchor slope is provably too optimistic
// (measured 2026-08-27), the local one is not. Without a neighbour, or when the
// two do not form a falling curve, fallback answers.
func autoCQLocalSlope(scores map[int]float64, at autoCQPoint, fallback float64) float64 {
	neighbour, found := 0, false
	for cq := range scores {
		if cq > at.cq && (!found || cq < neighbour) {
			neighbour, found = cq, true
		}
	}
	if !found {
		return fallback
	}
	if slope := (scores[neighbour] - at.vmaf) / float64(neighbour-at.cq); slope < -0.01 {
		return slope
	}
	return fallback
}

// autoCQBetween interpolates the CQ where the straight line between a point
// above the target and one below it crosses the target, kept strictly between
// the two so that every measurement narrows the gap. The caller guarantees at
// least one step in between.
func autoCQBetween(above, below autoCQPoint, target float64) int {
	next := (above.cq + below.cq) / 2
	if drop := above.vmaf - below.vmaf; drop > 0 {
		next = int(math.Round(float64(above.cq) +
			(above.vmaf-target)/drop*float64(below.cq-above.cq)))
	}
	return min(max(next, above.cq+1), below.cq-1)
}

// autoCQHoldTarget finds the thriftiest CQ whose MEASUREMENT reaches the
// target, after the pick measured below it (miss). Until 1.34.0 a step down was
// often taken on its estimate alone; since 2.0.0 the target is a floor, as in
// CloudForge.
//
// Two phases. While no measured point reaches the target, the search steps
// further down (autoCQStepDown on the local slope), and every step below the
// low anchor faces the same brakes as the first verification: a dead curve
// (holdSaturated) or one that buys too little per step (holdTooExpensive) ends
// it. Once a point reaches the target, the gap between it and the nearest miss
// is narrowed until no step lies in between — with the Illinois correction
// against creeping: while the miss stays put, its distance to the target is
// halved for the next calculation (CloudForge measured the creep on 2026-09-26:
// 29, 30, 31, 32, one probe each, because a far point held the line).
//
// scores holds every point measured so far; scoreAt adds to it.
func autoCQHoldTarget(sc autoCQScale, target, vmafLow, anchorSlope float64,
	miss autoCQPoint, scores map[int]float64, scoreAt autoCQScorer) autoCQHold {

	calcMiss := miss // the miss as it enters the interpolation (Illinois)
	for step := 0; step < autoCQMaxHoldSteps; step++ {
		if above, found := autoCQThriftiestHolding(scores, target); found {
			if above.cq > miss.cq || miss.cq-above.cq <= 1 {
				return autoCQHold{holdReached, above}
			}
			next := autoCQBetween(above, calcMiss, target)
			score, err := scoreAt(next)
			switch {
			case err != nil:
				return autoCQHold{holdReached, above} // the measured point above still holds
			case score < target:
				miss = autoCQPoint{next, score}
				calcMiss = miss
			default:
				calcMiss.vmaf = target + (calcMiss.vmaf-target)/2
			}
			continue
		}

		// No measured point reaches the target yet: step further down.
		if miss.cq <= sc.clampMin {
			return autoCQHold{holdClampFloor, miss}
		}
		next := autoCQStepDown(sc, miss.cq, target, miss.vmaf,
			autoCQLocalSlope(scores, miss, anchorSlope))
		score, err := scoreAt(next)
		switch {
		case err != nil:
			return autoCQHold{holdGaveUp, miss}
		case score >= target:
			continue // the next round narrows the gap from here
		case autoCQSaturated(sc, next, score, vmafLow):
			return autoCQHold{holdSaturated, autoCQPoint{next, score}}
		case autoCQGainTooSmall(sc, next, score, vmafLow):
			return autoCQHold{holdTooExpensive, autoCQPoint{next, score}}
		}
		miss = autoCQPoint{next, score}
		calcMiss = miss
	}
	if above, found := autoCQThriftiestHolding(scores, target); found {
		return autoCQHold{holdReached, above}
	}
	return autoCQHold{holdGaveUp, miss}
}

// autoCQThriftyStep tries one CQ step thriftier when a measured pick clears
// the target by more than autoCQThriftyMargin, and takes it when its own
// measurement still reaches the target (CloudForge's rule, taken over
// 2026-09-27). Only one step: each costs a full sample encode and measurement.
func autoCQThriftyStep(sc autoCQScale, target float64, pick autoCQPoint, scoreAt autoCQScorer) (autoCQPoint, bool) {
	if pick.vmaf-target <= autoCQThriftyMargin || pick.cq >= sc.clampMax {
		return pick, false
	}
	next := pick.cq + 1
	score, err := scoreAt(next)
	if err != nil || score < target {
		return pick, false
	}
	return autoCQPoint{next, score}, true
}

// ----------------------------------------------------------------------------
// Does re-encoding pay off? (since 2.0.0, replaces the cost cap)
// ----------------------------------------------------------------------------

const (
	// autoCQSizeProbeSpots: how many stretches the size probe encodes across
	// the whole film — CloudForge's figure (user's choice 2026-09-27). There,
	// 3-5 analysis windows missed whole films by up to 13 points, always on
	// the optimistic side, while evenly spread spots came within 2-5 points.
	autoCQSizeProbeSpots = 10

	// autoCQSizeProbeBand: how close (in percentage points) the first
	// prediction has to lie to the minimum saving for the size probe to run.
	// The largest error of the analysis windows CloudForge measured was 13
	// points; a clear case costs no probe at all.
	autoCQSizeProbeBand = 15.0

	// autoCQSizeProbeBatch: that many stretches go into one probe encode — as
	// many as the analysis decodes at once, which is known to run on the
	// graphics card. Ten inputs at once would open ten decoders.
	autoCQSizeProbeBatch = 3
)

// autoCQExpectedSavingPercent predicts how much smaller the whole file gets
// when the picture costs share of its source bitrate (0.42 = 42 %). Only the
// picture is re-encoded; sound and subtitles are counted unchanged — they are
// treated the same way whether the file is re-encoded or remuxed, so they do
// not decide between the two (CloudForge's formula). ok is false when there is
// nothing to predict from.
func autoCQExpectedSavingPercent(fileMB, videoKbps, durationSec, share float64) (float64, bool) {
	if share <= 0 || fileMB <= 0 || durationSec <= 0 {
		return 0, false
	}
	fileBytes := fileMB * 1048576
	videoBytes := videoKbps * 1000 / 8 * durationSec
	if videoBytes <= 0 || videoBytes > fileBytes {
		videoBytes = fileBytes // split unknown: count everything as picture
	}
	predicted := share*videoBytes + (fileBytes - videoBytes)
	return (fileBytes - predicted) / fileBytes * 100, true
}

// autoCQSavingText phrases a predicted saving for the log. A negative one
// means the re-encode would come out LARGER than the source — "-21% smaller"
// would leave the reader puzzling.
func autoCQSavingText(pct float64) string {
	if pct < 0 {
		return fmt.Sprintf("about %.0f%% larger than the source", -pct)
	}
	return fmt.Sprintf("about %.0f%% smaller", pct)
}

// autoCQSizeProbeNeeded reports whether the first prediction lies so close to
// the minimum saving that its error could flip the decision.
func autoCQSizeProbeNeeded(expectedPct, minSavePct float64) bool {
	return math.Abs(expectedPct-minSavePct) <= autoCQSizeProbeBand
}

// autoCQSizeProbeWindows spreads count stretches evenly over the WHOLE film,
// each centred in its section (5 %, 15 % … 95 % for ten). Unlike the quality
// windows, intro and credits belong in: the question is the size of the whole
// file, not the quality of its hardest scene.
func autoCQSizeProbeWindows(durationSec, length float64, count int) [][2]float64 {
	if durationSec <= 0 || length <= 0 || count <= 0 {
		return nil
	}
	windows := make([][2]float64, 0, count)
	for i := 0; i < count; i++ {
		centre := durationSec * (float64(i) + 0.5) / float64(count)
		start := math.Max(0, math.Min(centre-length/2, durationSec-length))
		windows = append(windows, [2]float64{start, length})
	}
	return windows
}

// ----------------------------------------------------------------------------
// Measuring small pictures the way they look (since 2.0.0)
// ----------------------------------------------------------------------------

// VMAF's default model is built for a picture that fills a 1080p screen.
const (
	vmafScreenLong  = 1920
	vmafScreenShort = 1080
)

// autoCQVMAFMeasureSize returns the size at which an encode is measured: one
// smaller than 1080p is enlarged — both sides alike — until its long edge
// reaches 1920 or its short edge 1080, in the same aspect ratio (portrait the
// same way, turned). In its own small size VMAF scores far better than the
// picture looks full-screen — CloudForge measured on 2026-09-27: 720p 96.2 in
// its own size against 92.9 enlarged, 540p 95.2 against 88.4, 404p 97.0
// against 87.8. At 1080p and above, or with unknown sizes, nothing changes.
func autoCQVMAFMeasureSize(width, height int) (int, int, bool) {
	if width <= 0 || height <= 0 {
		return width, height, false
	}
	long, short := max(width, height), min(width, height)
	factor := math.Min(float64(vmafScreenLong)/float64(long), float64(vmafScreenShort)/float64(short))
	if factor <= 1 {
		return width, height, false
	}
	return evenRound(float64(width) * factor), evenRound(float64(height) * factor), true
}

// evenRound rounds a pixel count to the nearest even number; a value of at
// most 1080 stays at most 1080.
func evenRound(v float64) int {
	return int(math.Round(v/2)) * 2
}

// autoCQClimbCandidates returns the CQ rungs the plateau climb probes above
// the current pick, cheapest file first: the clamp ceiling, the midpoint
// between high anchor and ceiling, and — when the pick sits below them — the
// two anchors themselves. Rungs at or below the pick are dropped, duplicates
// on narrow scales collapse. With the current constants that is 34, 32 for a
// pick at the high anchor, and 34, 32, 30 for a pick below it (AV1: 44, 38
// and 44, 38, 32). The low anchor only ever surfaces for a pick at the clamp
// floor (target missed even there); both anchor rungs are free — their scores
// were measured at the start of the search and are reused by the climb.
func autoCQClimbCandidates(sc autoCQScale, pick int) []int {
	mid := (sc.anchorHigh + sc.clampMax) / 2
	var rungs []int
	for _, r := range []int{sc.clampMax, mid, sc.anchorHigh, sc.anchorLow} {
		if r <= pick {
			continue
		}
		if n := len(rungs); n > 0 && r >= rungs[n-1] {
			continue
		}
		rungs = append(rungs, r)
	}
	return rungs
}

// autoCQPlateauFloor is the minimum VMAF a climb rung must reach when the
// target is proven unreachable: the measured plateau top minus the configured
// plateau tolerance. It is an absolute VMAF budget shared by all codecs — on a
// source whose quality tops out below the target, how much of that
// unreachable quality the savings may cost does not depend on the CQ scale.
// The spread inside a saturated plateau is largely re-encode noise of an
// already degraded picture, while every skipped CQ step wastes real bitrate.
func autoCQPlateauFloor(plateauTop, plateauTolerance float64) float64 {
	return plateauTop - plateauTolerance
}

// autoCQClimbBudgetFloor returns the climb floor for a proven-unreachable
// target. The plateau budget is only justified when the measured curve is
// FLAT: the spread between rungs is then re-encode noise of an already
// degraded picture (the autoCQPlateauFloor rationale). On a steep curve the
// same spread is real, visible quality, so nothing may be given away: the
// floor is the plateau top itself. Until 1.34.0 the removed autoCQTolerance
// was spent there; the user chose the picture over those last percent of
// space (2026-09-11 and again 2026-09-27).
func autoCQClimbBudgetFloor(plateauTop float64, flatCurve bool, plateauTolerance float64) float64 {
	if flatCurve {
		return autoCQPlateauFloor(plateauTop, plateauTolerance)
	}
	return plateauTop
}

// autoCQClimbWorthIt decides whether a plateau-climb rung earns the quality it
// costs. The climb otherwise only ever asks what a rung COSTS in VMAF, never
// what it BUYS in file size — and on a source that is already compressed to the
// bone a rung buys next to nothing. Measured 2026-08-28 on a 2.4 Mbit/s 60 fps
// source: CQ 30 spent 52.7 % of the source, CQ 34 spent 52.2 %, so the climb
// paid 0.68 VMAF for a file about 1 % smaller. The same series holds the
// opposite case as well (49.9 % → 42.3 % across the same rungs), which is why
// the answer cannot be a smaller plateau tolerance: that would block both.
//
// pickKbps/rungKbps are the sample bitrates of the current pick and of the
// rung. A zero on either side means "not measurable" and lets the rung pass —
// an unusable measurement must never flip established behaviour.
func autoCQClimbWorthIt(pickKbps, rungKbps, minSavePercent float64) (savedPct float64, worth bool) {
	if pickKbps <= 0 || rungKbps <= 0 {
		return 0, true
	}
	savedPct = (pickKbps - rungKbps) / pickKbps * 100
	if minSavePercent <= 0 {
		// Prüfung abgeschaltet — die Zahl wird trotzdem geliefert, denn das
		// Protokoll nennt sie auch bei einem angetretenen Aufstieg.
		return savedPct, true
	}
	return savedPct, savedPct >= minSavePercent
}

// bitrateBucket is one window-sized slice of the source with its average
// video bitrate — the complexity proxy for guided window placement: where
// the source encoder needed many bits, the material is hard.
type bitrateBucket struct {
	startSec float64
	kbps     float64
}

// probeSourceBitrateBuckets demuxes the video stream once (packet sizes
// only, NO decode — seconds even on multi-GB files) and sums the packet
// sizes into windowLen-sized buckets. It also returns how many video packets
// that demux saw: the same pass answers "does this source have gaps in its
// timeline" for free (autoCQGapNotice), so no second probe is needed.
func probeSourceBitrateBuckets(ctx context.Context, filePath string,
	durationSec, windowLen float64) ([]bitrateBucket, int, error) {

	runCtx, cancel := context.WithTimeout(ctx, autoCQProfileTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ffprobePath,
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time,size", "-of", "csv=p=0", filePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winCREATE_NO_WINDOW | winIDLE_PRIORITY_CLASS,
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("AutoCQ.go: probeSourceBitrateBuckets: %w", err)
	}
	buckets, packets := bucketsFromPacketCSV(string(out), durationSec, windowLen)
	if len(buckets) == 0 {
		return nil, 0, errors.New("AutoCQ.go: probeSourceBitrateBuckets: no usable packet data")
	}
	return buckets, packets, nil
}

// autoCQGapNotice reports how much material is missing from the source
// timeline, or "" when there is nothing worth reporting. It compares the
// frames the source should hold (duration × frame rate) with the video packets
// actually found. Counting is deliberate: packets arrive in decode order and
// their timestamps jump around B-frames, so measuring distances between
// neighbours would flag perfectly healthy files.
func autoCQGapNotice(videoPackets int, durationSec float64, fpsNum, fpsDen int) string {
	if videoPackets <= 0 || durationSec <= 0 || fpsNum <= 0 || fpsDen <= 0 {
		return ""
	}
	fps := float64(fpsNum) / float64(fpsDen)
	expected := durationSec * fps
	missing := expected - float64(videoPackets)
	if missing < autoCQGapNoticeMinSec*fps || missing > expected*autoCQGapNoticeMaxShare {
		return ""
	}
	return fmt.Sprintf(
		"  · note: the source timeline is missing about %s of frames — samples on both sides were aligned to %.4g fps so the measurement compares matching frames",
		formatDuration(missing/fps), fps)
}

// bucketsFromPacketCSV turns ffprobe "pts_time,size" CSV lines into full
// windowLen-sized buckets and additionally returns how many usable video
// packets the CSV held (the frame count the gap notice compares against).
// Lines without a parsable timestamp or size (e.g. "N/A") are skipped; the
// partial tail bucket is dropped so its deflated average cannot skew the
// placement. Packets beyond the last full bucket still count as frames — they
// exist in the file even though their bucket is not used for placement.
func bucketsFromPacketCSV(csv string, durationSec, windowLen float64) ([]bitrateBucket, int) {
	if windowLen <= 0 || durationSec < windowLen {
		return nil, 0
	}
	n := int(durationSec / windowLen)
	sums := make([]int64, n)
	packets := 0
	for _, line := range strings.Split(csv, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 {
			continue
		}
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || pts < 0 {
			continue
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size <= 0 {
			continue
		}
		packets++
		if idx := int(pts / windowLen); idx < n {
			sums[idx] += size
		}
	}
	buckets := make([]bitrateBucket, 0, n)
	for i, b := range sums {
		buckets = append(buckets, bitrateBucket{
			startSec: float64(i) * windowLen,
			kbps:     float64(b) * 8 / 1000 / windowLen,
		})
	}
	return buckets, packets
}

// autoCQGuidedWindows picks the sample windows from the bucket profile:
// rank 0 (the heaviest bucket) is always included, the remaining windows
// spread down the bitrate-sorted list to 0.80 — the very light end is
// deliberately avoided because black frames and stills score a flattering
// near-100 VMAF. Returns nil (caller keeps the fixed positions) when the
// profile is flat or too few full buckets fit between the edge margins.
func autoCQGuidedWindows(buckets []bitrateBucket, durationSec float64,
	count int, windowLen float64) [][2]float64 {

	if count < 1 {
		return nil
	}
	lo := durationSec * autoCQEdgeMarginPct
	hi := durationSec * (1 - autoCQEdgeMarginPct)
	var usable []bitrateBucket
	for _, b := range buckets {
		if b.kbps > 0 && b.startSec >= lo && b.startSec+windowLen <= hi {
			usable = append(usable, b)
		}
	}
	if len(usable) < count*2 {
		return nil
	}
	sorted := append([]bitrateBucket(nil), usable...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].kbps > sorted[j].kbps })
	median := sorted[len(sorted)/2].kbps
	if median <= 0 || sorted[0].kbps/median < autoCQFlatProfileRatio {
		return nil
	}

	used := make(map[int]bool, count)
	pick := func(rank int) int {
		for i := rank; i < len(sorted); i++ {
			if !used[i] {
				return i
			}
		}
		for i := rank - 1; i >= 0; i-- {
			if !used[i] {
				return i
			}
		}
		return -1 // unreachable: len(usable) >= count*2 leaves free slots
	}
	chosen := make([]bitrateBucket, 0, count)
	for i := 0; i < count; i++ {
		frac := 0.0
		if count > 1 {
			frac = 0.80 * float64(i) / float64(count-1)
		}
		idx := pick(int(math.Round(frac * float64(len(sorted)-1))))
		if idx < 0 {
			return nil
		}
		used[idx] = true
		chosen = append(chosen, sorted[idx])
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].startSec < chosen[j].startSec })
	out := make([][2]float64, len(chosen))
	for i, b := range chosen {
		out[i] = [2]float64{b.startSec, windowLen}
	}
	return out
}

// buildAutoCQEncodeArgs assembles the FFmpeg call that encodes the sample
// windows (video only) into one small anchor file, using exactly the options
// of the real encode (buildOpts, same filter chain, same GOP) at the given
// CQ — so H.265 and AV1 each sample through their own encoder.
// setpts=PTS-STARTPTS per window re-bases the decoded segment timestamps
// (pitfall 1), concat then joins the windows into one stream.
// autoCQWindowInputs baut die Eingabe-Argumente der Messfenster: je Fenster
// Startzeit, Dauer und die Quelldatei.
//
// hwaccel (aus gpuDecodeArgs, sonst nil) muss VOR JEDEM "-i" wiederholt
// werden: FFmpeg wertet "-hwaccel" immer nur für die unmittelbar folgende
// Eingabe aus. Einmal vorne gesetzt würde also nur das erste Fenster auf der
// Grafikkarte entpackt — ein Fehler, der nicht auffällt, weil das Ergebnis
// stimmt und nur die Zeitersparnis ausbleibt.
func autoCQWindowInputs(sourcePath string, windows [][2]float64, hwaccel []string) []string {
	args := make([]string, 0, len(windows)*(6+len(hwaccel)))
	for _, w := range windows {
		args = append(args, hwaccel...)
		args = append(args,
			"-ss", strconv.FormatFloat(w[0], 'f', 3, 64),
			"-t", strconv.FormatFloat(w[1], 'f', 3, 64),
			"-i", sourcePath)
	}
	return args
}

func buildAutoCQEncodeArgs(sourcePath string, windows [][2]float64, hwaccel []string,
	filterChain string, fpsNum, fpsDen int, cq int, gop int, sampleName string,
	buildOpts func(cq int, gop int) []string) []string {

	args := []string{"-y"}
	args = append(args, autoCQWindowInputs(sourcePath, windows, hwaccel)...)
	prep := autoCQWindowPrep(fpsNum, fpsDen)
	var fg strings.Builder
	for i := range windows {
		fmt.Fprintf(&fg, "[%d:V:0]%s[w%d];", i, prep, i)
	}
	for i := range windows {
		fmt.Fprintf(&fg, "[w%d]", i)
	}
	fmt.Fprintf(&fg, "concat=n=%d:v=1:a=0,%s[out]", len(windows), filterChain)
	args = append(args, "-filter_complex", fg.String(), "-map", "[out]", "-an", "-sn")
	args = append(args, buildOpts(cq, gop)...)
	return append(args, sampleName)
}

// autoCQNormPTS returns the filter snippet that rebases timestamps to exact
// frame numbers (pitfall 2: Matroska rounds PTS to milliseconds). Shared by
// every consumer so all analysis inputs use identical timing.
func autoCQNormPTS(fpsNum, fpsDen int) string {
	return fmt.Sprintf("settb=AVTB,setpts=N*%d/%d/TB", fpsDen, fpsNum)
}

// autoCQWindowPrep returns the filters every sample window runs through. It is
// shared by the sample encode and the VMAF reference side on purpose: only if
// both build their windows identically can libvmaf pair matching frames.
//
// setpts=PTS-STARTPTS re-bases the cut window to zero (pitfall 1).
// fps= closes gaps in the source timeline (pitfall 3): where frames are
// missing, the encoder fills them in by itself through "-fps_mode cfr" while
// the reference side keeps the holes — from the first gap on, libvmaf then
// compares frames that do not belong together. Measured on a real file: 240
// against 96 frames in one 8-second window, VMAF 6.9 instead of 96.2. And
// because concat chains the windows, a hole in the first window shifts all
// later ones too, so the score collapses instead of dropping partially.
// Aligning only the reference side is NOT enough (measured 21.4): fps= and
// "-fps_mode cfr" do not pick the same frames. With both sides aligned the
// encoder has nothing left to correct.
// For constant-frame-rate sources the filter is provably inert — the encoded
// bitstream hashes identically and the VMAF score is unchanged to the last
// digit, so every calibrated anchor stays valid.
func autoCQWindowPrep(fpsNum, fpsDen int) string {
	if fpsNum <= 0 || fpsDen <= 0 {
		return "setpts=PTS-STARTPTS"
	}
	return fmt.Sprintf("setpts=PTS-STARTPTS,fps=%d/%d", fpsNum, fpsDen)
}

// autoCQNoteText macht aus dem angehängten Begründungstext (Form " (…)") eine
// eigenständige Zeile: das führende Leerzeichen und die umschließenden
// Klammern fallen weg. Die Notizen selbst behalten ihre Klammerform, weil sie
// an mehr als einer Stelle gebaut werden — umgeformt wird erst bei der Ausgabe.
func autoCQNoteText(note string) string {
	return strings.Trim(strings.TrimSpace(note), "()")
}

// autoCQVMAFThreads returns the libvmaf worker thread count (all cores).
func autoCQVMAFThreads() int {
	if n := runtime.NumCPU(); n > 1 {
		return n
	}
	return 1
}

// buildAutoCQVMAFArgs assembles the FFmpeg call that measures an anchor file
// against the freshly decoded source windows. The reference side runs through
// the SAME filter chain as the encode, so the score isolates the encoder loss
// (not scaling/sharpening). Both sides are forced to yuv420p10le and to
// frame-number-based timestamps (pitfall 2). n_subsample=3 scores every third
// frame — plenty for a sample and three times faster.
// measureWidth/measureHeight > 0 enlarge BOTH sides to that size before the
// comparison (autoCQVMAFMeasureSize): a picture smaller than 1080p is judged
// the way it looks full-screen. Both sides pass through the identical scaler
// in the identical pixel format, so only the encoder loss is scored; 0 keeps
// the encoded size.
func buildAutoCQVMAFArgs(sourcePath string, windows [][2]float64, hwaccel []string,
	filterChain string, fpsNum, fpsDen int, measureWidth, measureHeight int,
	sampleName, logName string) []string {

	args := autoCQWindowInputs(sourcePath, windows, hwaccel)
	// Die Vergleichsdatei bleibt bewusst auf dem Prozessor: sie ist bereits
	// klein (Zielauflösung) und der Messlauf hängt ohnehin an libvmaf, nicht
	// am Entpacken. So bleibt auch der Grafikspeicher-Bedarf niedrig.
	args = append(args, "-i", sampleName)

	normPTS := autoCQNormPTS(fpsNum, fpsDen)
	prep := autoCQWindowPrep(fpsNum, fpsDen)
	n := len(windows)
	var fg strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&fg, "[%d:V:0]%s[w%d];", i, prep, i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&fg, "[w%d]", i)
	}
	enlarge := ""
	if measureWidth > 0 && measureHeight > 0 {
		enlarge = fmt.Sprintf(",scale=%d:%d:flags=bicubic", measureWidth, measureHeight)
	}
	// filterChainToCPU: libvmaf rechnet auf dem Prozessor und kann mit Bildern
	// im Grafikspeicher nichts anfangen (siehe Kommentar dort).
	fmt.Fprintf(&fg, "concat=n=%d:v=1:a=0,%s,format=yuv420p10le%s,%s[ref];",
		n, filterChainToCPU(filterChain), enlarge, normPTS)
	fmt.Fprintf(&fg, "[%d:V:0]format=yuv420p10le%s,%s[dist];", n, enlarge, normPTS)
	fmt.Fprintf(&fg, "[dist][ref]libvmaf=log_fmt=json:log_path=%s:n_subsample=3:n_threads=%d",
		logName, autoCQVMAFThreads())
	return append(args, "-filter_complex", fg.String(), "-f", "null", "-")
}

// readVMAFScore extracts the pooled mean VMAF from a libvmaf JSON log. The
// arithmetic mean (not the harmonic mean) is what the anchor calibration in
// the CQ measurement series was evaluated with.
func readVMAFScore(logPath string) (float64, error) {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return 0, fmt.Errorf("AutoCQ.go: readVMAFScore: %w", err)
	}
	var vmafLog struct {
		PooledMetrics struct {
			VMAF struct {
				Mean float64 `json:"mean"`
			} `json:"vmaf"`
		} `json:"pooled_metrics"`
	}
	if err := json.Unmarshal(raw, &vmafLog); err != nil {
		return 0, fmt.Errorf("AutoCQ.go: readVMAFScore: JSON parse error: %w", err)
	}
	if vmafLog.PooledMetrics.VMAF.Mean <= 0 {
		return 0, errors.New("AutoCQ.go: readVMAFScore: no VMAF score in log")
	}
	return vmafLog.PooledMetrics.VMAF.Mean, nil
}

// runAutoCQFFmpeg runs one quiet analysis step (sample encode or VMAF
// measurement). workDir becomes the process working directory so libvmaf's
// log_path can stay relative — an absolute Windows path (C:\...) would need
// awkward escaping inside the filter graph. Runs at idle priority like every
// other FFmpeg call here, bounded by a hard timeout.
func runAutoCQFFmpeg(ctx context.Context, workDir string, args []string) error {
	runCtx, cancel := context.WithTimeout(ctx, autoCQStepTimeout)
	defer cancel()

	full := append([]string{"-hide_banner", "-v", "error", "-nostats"}, args...)
	cmd := exec.CommandContext(runCtx, ffmpegPath, full...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winCREATE_NO_WINDOW | winIDLE_PRIORITY_CLASS,
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("AutoCQ.go: runAutoCQFFmpeg: step timed out after %s", autoCQStepTimeout)
	}
	lastLine := ""
	for _, ln := range strings.Split(string(out), "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lastLine = t
		}
	}
	return fmt.Errorf("AutoCQ.go: runAutoCQFFmpeg: %w | %s", err, lastLine)
}

// autoCQSpinnerText formats a spinner phase text and pads it to the fixed
// spinner width, so each repaint fully covers the previous, longer line.
func autoCQSpinnerText(format string, args ...any) string {
	return fmt.Sprintf("%-*s", autoCQSpinnerTextWidth, fmt.Sprintf(format, args...))
}

// autoCQInput bundles what the analysis needs to know about one file's encode.
type autoCQInput struct {
	filePath    string
	stats       *VideoStats
	filterChain string
	gop         int
	doScale     bool
	// encWidth/encHeight: the picture size the encoder writes (encodedFrameSize);
	// below 1080p VMAF is measured enlarged to it.
	encWidth, encHeight int
	// minSavePct: the saving processFile will demand (minSaveFor) — the size
	// probe runs when the first prediction lies close to it.
	minSavePct float64
}

// autoCQResult is what the analysis hands back: the CQ to encode with, its
// VMAF and the reason line, and the predicted saving on the whole file
// (savingKnown = false when nothing could be predicted — the finished file is
// judged instead).
//
// The "cq" event for a front-end is sent by processFile, not by the analysis:
// only processFile knows whether the file is re-encoded at all. A window that
// reuses the CQ of a check run must never get one for a file that is only
// remuxed — it would force an encode that the minimum saving then throws away.
type autoCQResult struct {
	cq          int
	vmaf        float64
	note        string
	savingPct   float64
	savingKnown bool
}

// autoDetectCQ runs the full -autocq search for one file and returns the CQ to
// use, together with what re-encoding at that CQ is predicted to save. On ANY
// failure it warns and returns ok=false so the caller keeps the configured
// targetCQ — the Auto-CQ analysis must never break a conversion. The spinner
// keeps the analysis visibly alive (a silent multi-second pause would look like
// a hang).
func autoDetectCQ(ctx context.Context, in autoCQInput, sc autoCQScale) (autoCQResult, bool) {
	filePath, stats, filterChain, gop, doScale := in.filePath, in.stats, in.filterChain, in.gop, in.doScale

	// The target is a floor since 2.0.0: there is no tolerance below it any
	// more (the removed autoCQTolerance), only proven-unreachable or
	// too-expensive targets end below it — and the log says so.
	target := appSettings.autoCQTargetVMAF

	windows := autoCQSampleWindows(stats.DurationSec)
	if windows == nil {
		pWarn.Printf("Auto-CQ: video too short for sampling (< %.0f s) — using fallback CQ %d.\n",
			autoCQMinSourceSec, sc.fallbackCQ())
		return autoCQResult{}, false
	}
	if stats.FPSNum <= 0 || stats.FPSDen <= 0 {
		pWarn.Printf("Auto-CQ: source frame rate unknown — using fallback CQ %d.\n",
			sc.fallbackCQ())
		return autoCQResult{}, false
	}

	tmpDir, err := os.MkdirTemp("", "NVENCForge_autocq_")
	if err != nil {
		pWarn.Printf("Auto-CQ: cannot create temp folder (%v) — using fallback CQ %d.\n",
			err, sc.fallbackCQ())
		return autoCQResult{}, false
	}
	defer os.RemoveAll(tmpDir)

	var sampleSec float64
	for _, w := range windows {
		sampleSec += w[1]
	}
	// Below 1080p both sides are measured enlarged to Full HD size — say so,
	// otherwise a small file's CQ would look strangely generous.
	measureWidth, measureHeight, enlarge := autoCQVMAFMeasureSize(in.encWidth, in.encHeight)
	measureNote := ""
	if enlarge {
		measureNote = fmt.Sprintf(", measured at %dx%d like on a Full HD screen", measureWidth, measureHeight)
	} else {
		measureWidth, measureHeight = 0, 0
	}
	// Erst hier steht fest, dass wirklich gemessen wird — alle Abbruchgründe
	// (zu kurzes Video, unbekannte Bildrate, kein Temp-Ordner) liegen oben.
	// Die Analyse liefert keine Fortschrittswerte und dauert ein bis zwei
	// Minuten; ohne diese Meldung stünde eine Oberfläche so lange bei null,
	// ohne sagen zu können, warum.
	emitStage("analyze")

	pInfo.Printf("%s Auto-CQ: analyzing %d sample windows (%.0f s) for VMAF target %.4g%s...\n",
		pterm.LightMagenta("›"), len(windows), sampleSec, target, measureNote)

	spinner, _ := pterm.DefaultSpinner.WithText(autoCQSpinnerText(autoCQSpinnerScanText)).Start()
	analysisStart := time.Now()

	// Window placement: demux the packet sizes once and put the windows on
	// the bitrate profile, so the hardest scene is guaranteed to be sampled.
	// Any profile problem falls back silently to the fixed positions — the
	// placement is an optimisation, never a reason to fail the analysis.
	placement := "fixed positions"
	var profileErr error
	gapNote := ""
	// The profile outlives the placement decision: the saving prediction needs
	// it again at the end, to read the source rate at exactly the sampled seconds.
	var profile []bitrateBucket
	if buckets, videoPackets, perr := probeSourceBitrateBuckets(ctx, filePath, stats.DurationSec, windows[0][1]); perr != nil {
		if ctx.Err() != nil {
			_ = spinner.Stop()
			return autoCQResult{}, false
		}
		profileErr = perr
	} else {
		profile = buckets
		if gw := autoCQGuidedWindows(buckets, stats.DurationSec, len(windows), windows[0][1]); gw != nil {
			windows, placement = gw, "bitrate-guided"
		}
		// Same demux, no extra cost: say so when the source has holes, otherwise
		// such a file just behaves differently for no visible reason.
		gapNote = autoCQGapNotice(videoPackets, stats.DurationSec, stats.FPSNum, stats.FPSDen)
	}

	// Hier stand bis 1.10.0 ein Referenz-Cache, der die Fenster einmal
	// verlustfrei zwischenspeicherte. Er ist raus und soll nicht zurück: an
	// echten Dateien gemessen war er in JEDER Konstellation langsamer als der
	// direkte Weg — 4K 0:26 mit gegen 0:14 ohne, 1080p60 1:33 gegen 0:57,
	// selbst bei CPU-Decode 0:26 gegen 0:21, und das bei identischen Ankern
	// und identischer CQ-Wahl. Seit NVDEC ist Entpacken billig; das
	// verlustfreie Schreiben des Caches ist es nicht.
	//
	// Entpacken auf der Grafikkarte gilt auch für die Messläufe. Das ist
	// bildgleich (siehe gpuDecodeArgs), die gemessenen VMAF-Werte und damit die
	// CQ-Wahl bleiben also unverändert — nur die Analyse wird schneller.
	hwaccel := gpuDecodeArgs(stats)

	// Wird auf der Grafikkarte VERKLEINERT, gilt das auch hier — sonst würde
	// die Suche den CQ an einem anders skalierten Bild messen als dem, das der
	// echte Encode später liefert. Dafür müssen die entpackten Bilder im
	// Grafikspeicher bleiben, was "-hwaccel_output_format cuda" bewirkt.
	// activeChain ist veränderlich, weil der Rückfall unten die Kette
	// mittauschen muss: ohne Bilder auf der Karte kann scale_cuda nicht laufen.
	activeChain := filterChain
	cpuChain := filterChain
	if chainUsesGPU(filterChain) {
		hwaccel = append(hwaccel, "-hwaccel_output_format", "cuda")
		// Ohne Deinterlacing gebaut — das schließt gpuScaleUsable ohnehin aus.
		// Und ohne Auto-Crop: dass die Kette scale_cuda enthält, heißt
		// zwangsläufig, dass nicht geschnitten wird (gpuScaleUsable gibt bei
		// aktivem Schnitt false zurück, weil scale_cuda nicht zuschneiden kann).
		cpuChain = buildVideoFilter(doScale, false, false, cropRect{})
	}

	// runAutoCQStep führt einen Messlauf aus und wiederholt ihn EINMAL ohne
	// Grafikkarte, falls diese ihn abweist. Ohne diesen Rückfall würde ein
	// Entpack-Problem die komplette Analyse scheitern lassen und die Datei
	// bekäme den groben Ersatzwert statt eines gemessenen CQ.
	runAutoCQStep := func(build func(hw []string, chain string) []string) error {
		err := runAutoCQFFmpeg(ctx, tmpDir, build(hwaccel, activeChain))
		if err == nil || len(hwaccel) == 0 || ctx.Err() != nil {
			return err
		}
		pWarn.Println("Auto-CQ: GPU decoding failed — continuing on the CPU.")
		gpuDecodeDisabled = true
		gpuFramesStayOnCard = false
		hwaccel = nil
		activeChain = cpuChain
		return runAutoCQFFmpeg(ctx, tmpDir, build(nil, activeChain))
	}

	fail := func(step string, err error) (autoCQResult, bool) {
		_ = spinner.Stop()
		if ctx.Err() != nil {
			return autoCQResult{}, false // user abort — no misleading failure warning
		}
		pWarn.Printf("Auto-CQ: %s failed — using fallback CQ %d.\n", step, sc.fallbackCQ())
		pDetail.Printf("Auto-CQ detail: %v\n", err)
		return autoCQResult{}, false
	}

	measure := func(cq int) (float64, error) {
		sampleName := fmt.Sprintf("sample_cq%d.mkv", cq)
		logName := fmt.Sprintf("vmaf_cq%d.json", cq)
		buildEnc := func(hw []string, chain string) []string {
			return buildAutoCQEncodeArgs(filePath, windows, hw, chain,
				stats.FPSNum, stats.FPSDen, cq, gop, sampleName, sc.buildOpts)
		}
		buildVMAF := func(hw []string, chain string) []string {
			return buildAutoCQVMAFArgs(filePath, windows, hw, chain,
				stats.FPSNum, stats.FPSDen, measureWidth, measureHeight, sampleName, logName)
		}
		spinner.UpdateText(autoCQSpinnerText("Auto-CQ: encoding samples at CQ %d...", cq))
		if err := runAutoCQStep(buildEnc); err != nil {
			return 0, fmt.Errorf("sample encode at CQ %d: %w", cq, err)
		}
		spinner.UpdateText(autoCQSpinnerText("Auto-CQ: measuring VMAF at CQ %d...", cq))
		if err := runAutoCQStep(buildVMAF); err != nil {
			return 0, fmt.Errorf("VMAF measurement at CQ %d: %w", cq, err)
		}
		score, err := readVMAFScore(filepath.Join(tmpDir, logName))
		if err != nil {
			return 0, fmt.Errorf("VMAF result at CQ %d: %w", cq, err)
		}
		return score, nil
	}

	// scores remembers every measured CQ of this search: no step is ever
	// encoded twice, and each rule below can ask for a score by CQ.
	scores := make(map[int]float64)
	scoreAt := func(cq int) (float64, error) {
		if score, known := scores[cq]; known {
			return score, nil
		}
		score, err := measure(cq)
		if err == nil {
			scores[cq] = score
		}
		return score, err
	}

	vmafLow, err := scoreAt(sc.anchorLow)
	if err != nil {
		return fail(fmt.Sprintf("anchor measurement at CQ %d", sc.anchorLow), err)
	}
	vmafHigh, err := scoreAt(sc.anchorHigh)
	if err != nil {
		return fail(fmt.Sprintf("anchor measurement at CQ %d", sc.anchorHigh), err)
	}

	// Ab hier liegen beide Anker-Proben im Temp-Ordner. Aus ihren Größen
	// ergibt sich die Zerfallskurve der Bitrate über CQ. sampleKbpsAt liefert
	// damit für jedes CQ eine Bitrate: bevorzugt aus der echten Probendatei,
	// ersatzweise aus der Kurve, und 0, wenn beides nicht geht.
	anchorKbpsLow, bitrateRate := 0.0, 0.0
	if kLow, lerr := autoCQSampleKbps(tmpDir, sc.anchorLow, sampleSec); lerr == nil {
		if kHigh, herr := autoCQSampleKbps(tmpDir, sc.anchorHigh, sampleSec); herr == nil {
			anchorKbpsLow, bitrateRate = kLow, autoCQBitrateRate(sc, kLow, kHigh)
		}
	}
	sampleKbpsAt := func(atCQ int) float64 {
		if kbps, kerr := autoCQSampleKbps(tmpDir, atCQ, sampleSec); kerr == nil {
			return kbps
		}
		if bitrateRate <= 0 {
			return 0
		}
		return autoCQEstimateKbps(sc, anchorKbpsLow, bitrateRate, atCQ)
	}

	// bucketLen: the length of one slice of the source bitrate profile — the
	// window length it was built with.
	bucketLen := windows[0][1]
	// sizeProbeShare encodes stretches spread over the whole film at one CQ —
	// nothing is measured, only the size counts — and returns what the picture
	// costs there against the source at exactly those seconds (0.42 = 42 %).
	// Batches of autoCQSizeProbeBatch keep the number of decoders at what the
	// analysis itself uses.
	sizeProbeShare := func(atCQ int) (float64, error) {
		spots := autoCQSizeProbeWindows(stats.DurationSec, bucketLen, autoCQSizeProbeSpots)
		sourceKbps := autoCQWindowSourceKbps(profile, spots, bucketLen)
		if sourceKbps <= 0 {
			return 0, errors.New("no source bitrate profile for the probe spots")
		}
		var probeBytes int64
		var probeSec float64
		for first := 0; first < len(spots); first += autoCQSizeProbeBatch {
			batch := spots[first:min(first+autoCQSizeProbeBatch, len(spots))]
			name := fmt.Sprintf("sizeprobe_%d.mkv", first)
			build := func(hw []string, chain string) []string {
				return buildAutoCQEncodeArgs(filePath, batch, hw, chain,
					stats.FPSNum, stats.FPSDen, atCQ, gop, name, sc.buildOpts)
			}
			if err := runAutoCQStep(build); err != nil {
				return 0, fmt.Errorf("size probe encode: %w", err)
			}
			info, err := os.Stat(filepath.Join(tmpDir, name))
			if err != nil {
				return 0, fmt.Errorf("size probe result: %w", err)
			}
			probeBytes += info.Size()
			for _, spot := range batch {
				probeSec += spot[1]
			}
		}
		return float64(probeBytes) * 8 / 1000 / probeSec / sourceKbps, nil
	}

	cq, predicted := interpolateAutoCQ(sc, vmafLow, vmafHigh, target)

	// The interpolated pick is ALWAYS confirmed by a real measurement: the
	// linear model is only exact at the anchors, and between/beyond them the
	// bent VMAF(CQ) curve tends to promise slightly more quality than the
	// encode delivers. A pick that IS an anchor already carries its
	// measurement. Since 2.0.0 the target is a floor: a miss is followed by
	// further measured steps (autoCQHoldTarget) instead of an estimated step
	// down, and a clear hit tries one step thriftier (autoCQThriftyStep).
	slope := (vmafHigh - vmafLow) / float64(sc.anchorHigh-sc.anchorLow)
	verifyNote := ""
	plateauLevel := 0.0 // > 0: target proven unreachable — climb may probe higher rungs
	// plateauFlat: the measured curve is proven flat around the pick, so the
	// spread up to the climb rungs is mostly re-encode noise and the plateau
	// budget applies. A steep curve (real quality per CQ step) gives nothing
	// away — see autoCQClimbBudgetFloor.
	plateauFlat := false

	// saturatedPick is the saturation brake: the source is already compressed
	// so hard that VMAF plateaus below the target — more bitrate buys no
	// quality. Fall back to the cheapest CQ still on the plateau instead of
	// stepping further down into pure waste. level is the plateau top.
	saturatedPick := func(level float64) {
		satCQ, satVMAF := autoCQPlateauPick(sc, vmafLow, vmafHigh)
		verifyNote = fmt.Sprintf(
			" (VMAF saturates at ~%.1f — target %.4g unreachable, picking efficient CQ %d)",
			level, target, satCQ)
		cq, predicted = satCQ, satVMAF
		plateauLevel = level
		plateauFlat = true // saturation proven by a real sub-anchor measurement
	}
	// thriftPick is the thrift brake: the target IS still reachable further
	// down, but below the low anchor each step buys so little VMAF that it does
	// not pay for the bitrate it costs. Fall back to the low anchor — the last CQ
	// whose step still earned its place. Deliberately no plateauLevel: the
	// target is NOT proven unreachable here, so the plateau climb (which may
	// spend whole VMAF points on savings) must stay out of this case.
	thriftPick := func(at autoCQPoint) {
		gainPerStep := (at.vmaf - vmafLow) / float64(sc.anchorLow-at.cq)
		verifyNote = fmt.Sprintf(
			" (CQ %d measured %.1f — each step below CQ %d buys only %.2f VMAF, not worth the size)",
			at.cq, at.vmaf, sc.anchorLow, gainPerStep)
		cq, predicted = sc.anchorLow, vmafLow
	}

	verified, verr := scoreAt(cq)
	switch {
	case verr != nil && ctx.Err() != nil:
		return fail("verification", verr)
	case verr != nil:
		// The anchors were fine, so keep the interpolated pick.
		verifyNote = " (verification failed, interpolated value kept)"
		pDetail.Printf("Auto-CQ verification detail: %v\n", verr)
	case verified >= target:
		predicted, verifyNote = verified, " (verified)"
		if cq == sc.anchorLow || cq == sc.anchorHigh {
			verifyNote = " (anchor measurement)"
		}
		if thrifty, ok := autoCQThriftyStep(sc, target, autoCQPoint{cq, verified}, scoreAt); ok {
			verifyNote = fmt.Sprintf(" (CQ %d measured %.1f — one step thriftier still holds the target)",
				cq, verified)
			cq, predicted = thrifty.cq, thrifty.vmaf
		}
	case autoCQSaturated(sc, cq, verified, vmafLow):
		saturatedPick(math.Max(verified, vmafLow))
	case autoCQGainTooSmall(sc, cq, verified, vmafLow):
		thriftPick(autoCQPoint{cq, verified})
	default:
		missed := autoCQPoint{cq, verified}
		hold := autoCQHoldTarget(sc, target, vmafLow, slope, missed, scores, scoreAt)
		if ctx.Err() != nil {
			return fail("search below the target", ctx.Err())
		}
		switch hold.outcome {
		case holdReached:
			verifyNote = fmt.Sprintf(" (CQ %d measured %.1f — stepped down to CQ %d, which holds the target)",
				missed.cq, missed.vmaf, hold.point.cq)
			cq, predicted = hold.point.cq, hold.point.vmaf
		case holdClampFloor:
			// The clamp floor itself measured below the target — proven
			// unreachable. The curve is NOT saturated here (the brake would
			// have fired), so the climb gives nothing away on it.
			verifyNote = fmt.Sprintf(" (measured %.1f — CQ clamp floor reached, target missed)",
				hold.point.vmaf)
			cq, predicted = hold.point.cq, hold.point.vmaf
			plateauLevel = hold.point.vmaf
		case holdSaturated:
			saturatedPick(math.Max(hold.point.vmaf, vmafLow))
		case holdTooExpensive:
			thriftPick(hold.point)
		default: // holdGaveUp: the best measured point so far, below the target
			verifyNote = fmt.Sprintf(" (CQ %d measured %.1f — the search below the target stopped at CQ %d)",
				missed.cq, missed.vmaf, hold.point.cq)
			cq, predicted = hold.point.cq, hold.point.vmaf
		}
	}

	// Plateau climb: a measured plateau below the target says nothing about
	// where the plateau ENDS — CQ rungs above the pick may still cost next to
	// nothing on such sources (2026-07-25 case: the real savings only started
	// above the high anchor). Probe the clamp ceiling first (cheapest file),
	// then the lower rungs; a rung is taken only when its REAL measurement
	// holds the floor: plateau top minus autoCQPlateauTolerance on a
	// proven-flat curve, the plateau top itself on a steep one (see
	// autoCQClimbBudgetFloor). A tolerance of 0 switches the climb off. A probe
	// failure keeps the safe pick — the climb is a bonus, never a reason to fail
	// the analysis. A healthy curve that reaches its target never gets here
	// (plateauLevel == 0).
	// Holding the floor is necessary but not sufficient: since 1.32.0 a rung
	// must also make the file measurably smaller (autoCQClimbWorthIt), because
	// on a source that is already squeezed dry it does not — and paying quality
	// for a file that stays the same size is the one trade nobody wants.
	var plateauProbes []string
	climbing := plateauLevel > 0 && appSettings.autoCQPlateauTolerance > 0
	climbFloor := autoCQClimbBudgetFloor(plateauLevel, plateauFlat, appSettings.autoCQPlateauTolerance)
	// climbSkipNote erklärt einen NICHT angetretenen Aufstieg. Ohne diese
	// Zeile sähe die Gegenrechnung wie ein stiller Ausfall aus: gleiche
	// Ausgangslage, plötzlich anderes Ergebnis, und nichts sagt warum.
	climbSkipNote := ""
	if climbing {
		// Was der aktuelle Pick kostet, ist der Bezugswert jedes Vergleichs.
		pickKbps := sampleKbpsAt(cq)
		for _, rung := range autoCQClimbCandidates(sc, cq) {
			// Rungs measured earlier in the search (the anchors at least) come
			// from memory instead of burning ~15 s on an identical
			// encode+measurement.
			score, cerr := scoreAt(rung)
			if cerr != nil {
				if ctx.Err() == nil {
					pDetail.Printf("Auto-CQ plateau probe detail: %v\n", cerr)
				}
				break
			}
			plateauProbes = append(plateauProbes, fmt.Sprintf("CQ %d = %.2f", rung, score))
			if score < climbFloor {
				continue
			}
			// Gegenrechnung (INI autoCQPlateauMinSavePercent): Die Sprosse hält
			// den Qualitätsboden — das allein rechtfertigt sie aber nicht, sie
			// muss die Datei auch spürbar kleiner machen. Die Sprossen kommen
			// nach steigender Dateigröße; bringt schon die kleinste zu wenig,
			// bringen die übrigen erst recht zu wenig. Deshalb endet der
			// Aufstieg hier, statt weiter zu suchen.
			savedPct, worth := autoCQClimbWorthIt(pickKbps, sampleKbpsAt(rung),
				appSettings.autoCQPlateauMinSavePercent)
			if !worth {
				climbSkipNote = fmt.Sprintf(
					"  · note: no plateau climb — CQ %d would make the file only %.1f%% smaller than CQ %d, under the %.4g%% minimum, so the picture stays",
					rung, math.Max(savedPct, 0), cq, appSettings.autoCQPlateauMinSavePercent)
				break
			}
			cq, predicted = rung, score
			// Die Ersparnis gehört in denselben Satz wie die Entscheidung.
			// Ohne sie steht dort nur, welche Qualität aufgegeben wurde, und
			// nicht, wofür — und die Gegenrechnung bliebe unsichtbar, solange
			// sie durchlässt. Ist die Größe nicht messbar, bleibt der Satz
			// genau der von vorher.
			savingText := ""
			if savedPct > 0 {
				savingText = fmt.Sprintf(", file %.0f%% smaller", savedPct)
			}
			verifyNote = fmt.Sprintf(
				" (VMAF plateaus at ~%.1f — target %.4g unreachable, plateau holds to CQ %d%s)",
				plateauLevel, target, rung, savingText)
			break
		}
	}

	// Saving prediction (since 2.0.0, replaces the cost cap): what the chosen
	// CQ costs at the sample windows against the source at exactly those
	// seconds, extended to the whole file. processFile remuxes instead when it
	// stays under the minimum saving. Near that minimum the few analysis
	// windows are not trusted — the size probe encodes stretches across the
	// whole film and its answer replaces the first prediction.
	saving, savingKnown := 0.0, false
	savingSource := fmt.Sprintf("from the %d analysis windows", len(windows))
	videoKbps := float64(determineBitrateKbps(stats))
	if sourceKbps := autoCQWindowSourceKbps(profile, windows, bucketLen); sourceKbps > 0 {
		if kbps := sampleKbpsAt(cq); kbps > 0 {
			saving, savingKnown = autoCQExpectedSavingPercent(stats.FileSizeMB, videoKbps,
				stats.DurationSec, kbps/sourceKbps)
		}
	}
	if savingKnown && autoCQSizeProbeNeeded(saving, in.minSavePct) {
		spinner.UpdateText(autoCQSpinnerText("Auto-CQ: checking the size at %d spots...", autoCQSizeProbeSpots))
		share, perr := sizeProbeShare(cq)
		switch {
		case perr != nil && ctx.Err() != nil:
			return fail("size probe", perr)
		case perr != nil:
			// The first prediction stands; the finished file is judged anyway.
			pDetail.Printf("Auto-CQ size probe detail: %v\n", perr)
			savingSource += " — the size probe failed"
		default:
			if probed, ok := autoCQExpectedSavingPercent(stats.FileSizeMB, videoKbps,
				stats.DurationSec, share); ok {
				savingSource = fmt.Sprintf("size probe at %d spots across the film; the analysis windows said %s",
					autoCQSizeProbeSpots, autoCQSavingText(saving))
				saving = probed
			}
		}
	}

	_ = spinner.Stop()
	if ctx.Err() != nil {
		return autoCQResult{}, false
	}
	// Die Kopfzeile nennt NUR die Entscheidung. Bis 1.29.0 stand die
	// Begründung mit im selben Satz, und die enthält oft eine zweite CQ-Zahl
	// (den verworfenen Wunschwert) — ein Nutzer im Doom9-Forum fragte
	// daraufhin zu Recht "and what is the CQ used?". Die Begründung steht
	// jetzt als eigene graue Zeile darunter, wie die Anker auch. Aus
	// demselben Grund fällt "predicted" weg: in den meisten Zweigen ist der
	// Wert eine echte Messung, und wo er hochgerechnet ist, sagt das die
	// Begründungszeile ("interpolated value kept", "estimate kept").
	pOK.Printf("Auto-CQ: using CQ %d (VMAF %.1f, target %.4g)\n", cq, predicted, target)
	noteText := autoCQNoteText(verifyNote)
	if noteText != "" {
		fmt.Println(pterm.Gray("  · " + noteText))
	}
	// Whether the file is re-encoded at all hangs on this number, so it stands
	// right under the decision.
	if savingKnown {
		fmt.Println(pterm.Gray(fmt.Sprintf("  · expected file: %s (%s)", autoCQSavingText(saving), savingSource)))
	} else {
		fmt.Println(pterm.Gray("  · expected file: cannot be predicted for this one — the finished file is checked instead"))
	}
	fmt.Println(pterm.Gray(fmt.Sprintf("  · anchors: CQ %d = %.2f, CQ %d = %.2f · windows: %s · analysis took %s",
		sc.anchorLow, vmafLow, sc.anchorHigh, vmafHigh, placement,
		formatDuration(time.Since(analysisStart).Seconds()))))
	if len(plateauProbes) > 0 {
		fmt.Println(pterm.Gray("  · plateau probes: " + strings.Join(plateauProbes, ", ")))
	}
	if climbSkipNote != "" {
		fmt.Println(pterm.Gray(climbSkipNote))
	}
	if gapNote != "" {
		fmt.Println(pterm.Gray(gapNote))
	}
	if profileErr != nil && debugMode {
		fmt.Println(pterm.Gray("  · bitrate profile skipped: " + profileErr.Error()))
	}
	return autoCQResult{cq: cq, vmaf: predicted, note: noteText,
		savingPct: saving, savingKnown: savingKnown}, true
}

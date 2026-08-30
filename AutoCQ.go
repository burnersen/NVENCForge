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
// should hit the configured quality target (autoCQTargetVMAF minus the
// space-saving autoCQTolerance, defaults 97 and 0.5), then confirm the
// pick with one verification measurement. A saturated
// curve (pre-compressed source, target unreachable) falls back to the
// cheapest CQ on the measured plateau instead of chasing the target; on
// every proven-unreachable target, rungs above the pick (up to the clamp
// ceiling) are probed too and taken when their measured score holds the
// climb floor: within autoCQPlateauTolerance of the plateau top on a
// proven-flat curve, only within the small search tolerance on a steep one
// (a grazed target is not a plateau) — the file shrinks as far as real
// measurements justify, never on extrapolation.
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
// AV1: measured 2026-07-06). The VMAF target and tolerance are NOT part of
// this struct — they measure quality, not CQ, so both codecs share them.
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
	// climbToleranceFactor widens the plateau-climb tolerance on the finer AV1
	// scale: one AV1 CQ step is worth about half a VMAF step, so the climb may
	// spend proportionally more tolerance for the same file-size saving as H.265.
	// 1.0 = H.265 (unchanged); 2.0 = AV1 (its anchor span is twice as wide).
	climbToleranceFactor float64
	// buildOpts assembles the real encoder options at a given CQ, so the sample
	// encodes match the actual encode bit for bit.
	buildOpts func(cq int, maxBitrate, bufsize string, gop int) []string
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
	maxStepDown:          3,
	saturationSlope:      0.10,
	minGainPerStep:       0.30,
	climbToleranceFactor: 1.0,
	buildOpts:            buildNVENCOptsWithCQ,
	fallbackCQ:           func() int { return appSettings.targetCQ },
	codecLabel:           "H.265",
}

// av1AutoCQFallbackCQ is the CQ the AV1 Auto-CQ search falls back to when its
// analysis cannot run (clip too short, unknown frame rate, libvmaf missing). It
// is deliberately NOT av1TargetCQ: that value (32 ≈ VMAF 94) is a lean manual-
// mode setting, too far below the VMAF target (default 97) for a graceful fallback. 24
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
	maxStepDown:          6,
	saturationSlope:      0.05,
	minGainPerStep:       0.15,
	climbToleranceFactor: 2.0,
	buildOpts:            buildAV1OptsWithCQ,
	fallbackCQ:           func() int { return av1AutoCQFallbackCQ },
	codecLabel:           "AV1",
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
// die feinere Sättigungsschwelle (0,08 statt 0,10), ein Schritt mehr
// Korrekturweite (4 statt 3) und der etwas größere Kletter-Faktor 1,25.
var x265AutoCQScale = autoCQScale{
	anchorLow: 18, anchorHigh: 22,
	clampMin: 12, clampMax: 28,
	maxStepDown:          4,
	saturationSlope:      0.08,
	minGainPerStep:       0.24,
	climbToleranceFactor: 1.25,
	buildOpts:            buildX265OptsWithCQ,
	fallbackCQ:           func() int { return appSettings.cpuTargetCRF },
	codecLabel:           "H.265 (CPU)",
}

// svtav1AutoCQFallbackCRF ist der CRF, auf den die AV1-Suche im CPU-Modus
// zurückfällt, wenn sie nicht messen kann. Wie bei av1_nvenc bewusst NICHT
// cpuAV1TargetCRF (32 = magerer Handbetriebswert), sondern der untere Anker,
// damit ein nicht messbarer Clip nahe am Qualitätsziel landet.
const svtav1AutoCQFallbackCRF = 24

// svtav1AutoCQScale ist das Auto-CQ-Profil für den CPU-Modus mit -av1
// (libsvtav1). Anker und Klemmen entsprechen denen von av1_nvenc — das ist
// kein Abschreiben, sondern Messergebnis: die Schrittbreiten liegen mit
// 0,30 (SVT) gegen 0,23 VMAF/Stufe (av1_nvenc) nah beieinander, weshalb
// dieselbe Einteilung passt. Sättigungsschwelle und Kletter-Faktor sind mit
// diesem Verhältnis nachgezogen (0,06 statt 0,05; 1,5 statt 2,0).
//
// WICHTIG: SVT-AV1 hat bei Preset 8 einen Qualitätsdeckel um VMAF 96,5 —
// am Realfilm verläuft die Kurve von CRF 12 bis 24 praktisch flach
// (96,56 → 96,20 bei 26 % weniger Dateigröße). Niedrigere CRF-Werte
// erzeugen dort nur größere Dateien. Genau dafür ist die Plateau-Logik
// (autoCQPlateauTolerance) da; die Anker sind deshalb bewusst nicht tiefer
// gelegt.
var svtav1AutoCQScale = autoCQScale{
	anchorLow: 24, anchorHigh: 32,
	clampMin: 16, clampMax: 44,
	maxStepDown:          5,
	saturationSlope:      0.06,
	minGainPerStep:       0.18,
	climbToleranceFactor: 1.5,
	buildOpts:            buildSVTAV1OptsWithCQ,
	fallbackCQ:           func() int { return svtav1AutoCQFallbackCRF },
	codecLabel:           "AV1 (CPU)",
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

// parseMaxrateKbps reads the "8000k" form the encoder options carry for
// -maxrate back into plain kbit/s. Anything unparsable answers 0 (unknown),
// which keeps callers on their silent path.
func parseMaxrateKbps(maxBitrate string) int64 {
	kbps, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(maxBitrate), "k"), 10, 64)
	if err != nil || kbps <= 0 {
		return 0
	}
	return kbps
}

// autoCQCapLimitsQuality reports whether the configured bitrate ceiling — and
// not the source material — is what holds the measured quality down. The encode
// aims at bitrateTargetPercent of the source rate; once the ceiling has cut that
// target, every low CQ rung rides the cap and measures the same score, so the
// saturation brake sees a plateau the source alone would not have produced
// (measured 2026-07-27: CQ 20/22/26 all landed on 23.08 MB and VMAF 89.9).
// The verdict only means something after the target proved unreachable. Returns
// the source rate in kbit/s for the message; false whenever a rate is unknown.
func autoCQCapLimitsQuality(stats *VideoStats, maxBitrate string) (int64, bool) {
	capKbps := parseMaxrateKbps(maxBitrate)
	sourceKbps := determineBitrateKbps(stats)
	if capKbps <= 0 || sourceKbps <= 0 {
		return 0, false
	}
	return sourceKbps, capKbps < sourceKbps*bitrateTargetPercent/100
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

// autoCQStepDown returns the CQ to fall back to after the verification
// measurement missed the target, plus the VMAF predicted for that CQ. The
// step count comes from the anchor slope (how much VMAF one CQ step buys),
// capped at the scale's maxStepDown and clamped at its clampMin — so the
// returned CQ can equal the input when the clamp floor is already reached.
// The third return value reports that the estimated step count exceeded
// maxStepDown: the prediction then extrapolates far beyond any measured
// point (observed several VMAF points off on the fine AV1 scale), so the
// caller must confirm the stepped CQ with a real measurement.
func autoCQStepDown(sc autoCQScale, cq int, target, verified, slope float64) (int, float64, bool) {
	steps := 1
	if slope < -0.01 {
		if s := int(math.Ceil((target - verified) / -slope)); s > steps {
			steps = s
		}
	}
	capped := steps > sc.maxStepDown
	if capped {
		steps = sc.maxStepDown
	}
	stepped := cq - steps
	if stepped < sc.clampMin {
		stepped = sc.clampMin
	}
	predicted := verified - slope*float64(cq-stepped)
	if predicted > 100 {
		predicted = 100
	}
	return stepped, predicted, capped
}

// autoCQFinalStepPick entscheidet, welcher CQ nach der zweiten Messung gilt —
// die letzte Station des gedeckelten Step-Downs.
//
// Zwei Ausgänge, und beide müssen ankommen: Ist der berechnete Schritt gleich
// dem schon gemessenen (die Klemmgrenze ist erreicht), zählt der MESSWERT
// dort. Geht es noch eine Stufe tiefer, zählt diese Stufe mit ihrer Schätzung —
// die Messung hat den Wert darüber ja gerade widerlegt.
//
// Die Funktion existiert, damit genau diese Entscheidung ohne FFmpeg prüfbar
// ist: sie war von 1.6.1 bis 1.30.0 falsch, und ohne eigene Funktion ließ sich
// das nicht absichern.
func autoCQFinalStepPick(stepped, final int, remeasured, finalPred float64) (int, float64) {
	if final == stepped {
		return stepped, remeasured
	}
	return final, finalPred
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

// autoCQWindowSourceKbps returns the source bitrate AT THE SAMPLE WINDOWS,
// averaged over them.
//
// The cost cap compares an encode of these windows against the source, and
// that comparison only holds when both sides describe the same seconds of
// film. With bitrate-guided placement the windows sit deliberately on the
// heaviest scenes, where the source runs well above its own average — judging
// them against the whole-file average would fire the cap on material that is
// not expensive at all. A window overlapping two buckets is weighted by the
// share it covers of each. Returns 0 when no profile exists; the caller then
// skips the cap instead of guessing.
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

// autoCQCostCap is what the cost cap worked out, kept together so the caller
// can both act on it and explain it in one line.
type autoCQCostCap struct {
	pick       int     // the CQ the cap asks for (equals the incoming pick when it does not fire)
	capKbps    float64 // the ceiling itself
	pickKbps   float64 // estimated bitrate of the INCOMING pick
	sourceKbps float64 // source rate at the sample windows
	// thriftiestKbps is what the scale's clamp ceiling would still cost, and
	// unreachable says that even THAT stays above the cap — the source is
	// already compressed too hard for the cap to be met. The pick is then left
	// alone; see the reasoning in autoCQCostCapTarget.
	thriftiestKbps float64
	unreachable    bool
}

// fires reports whether the cap actually moves the pick.
func (c autoCQCostCap) fires(pick int) bool { return c.pick > pick }

// sharePct expresses a bitrate as a share of what the source spends.
func (c autoCQCostCap) sharePct(kbps float64) float64 {
	if c.sourceKbps <= 0 {
		return 0
	}
	return kbps / c.sourceKbps * 100
}

// autoCQCostCapTarget answers one question: does reaching the quality target
// cost more of the source bitrate than the user allows, and if so, which CQ
// still fits?
//
// The model comes free from the two anchor samples the quality search already
// encoded. Verified on a 50 fps source (2026-08-28): anchors CQ 26 = 6625 and
// CQ 30 = 3690 kbit/s predict CQ 28 at 4942 kbit/s against 5008 measured — an
// error of 1.3 %, far inside what a ceiling decision needs.
//
// The result never falls below the incoming pick: the cap may save space, it
// may never spend it. And it never leaves the scale's clamp range, where even
// easy material visibly degrades.
func autoCQCostCapTarget(sc autoCQScale, tmpDir string, buckets []bitrateBucket,
	windows [][2]float64, sampleSec, percent float64, pick int) (autoCQCostCap, error) {

	budget := autoCQCostCap{pick: pick}
	if percent <= 0 || len(windows) == 0 {
		return budget, errors.New("cost cap disabled")
	}
	budget.sourceKbps = autoCQWindowSourceKbps(buckets, windows, windows[0][1])
	if budget.sourceKbps <= 0 {
		return budget, errors.New("no source bitrate profile for the sample windows")
	}
	kbpsLow, err := autoCQSampleKbps(tmpDir, sc.anchorLow, sampleSec)
	if err != nil {
		return budget, fmt.Errorf("sample size at CQ %d: %w", sc.anchorLow, err)
	}
	kbpsHigh, err := autoCQSampleKbps(tmpDir, sc.anchorHigh, sampleSec)
	if err != nil {
		return budget, fmt.Errorf("sample size at CQ %d: %w", sc.anchorHigh, err)
	}
	rate := autoCQBitrateRate(sc, kbpsLow, kbpsHigh)
	if rate <= 0 {
		return budget, errors.New("bitrate does not fall between the anchors")
	}
	budget.capKbps = budget.sourceKbps * percent / 100
	budget.pickKbps = autoCQEstimateKbps(sc, kbpsLow, rate, pick)
	if budget.pickKbps <= budget.capKbps {
		return budget, nil // the target fits the budget — nothing to do
	}
	// Reachability first. A cap that cannot be met even at the thriftiest CQ
	// the scale allows must not interfere at all: the search would clamp to
	// that CQ — the worst picture the setting can produce — and STILL miss the
	// cap. Worse picture, no saving, which is the exact opposite of the point.
	//
	// This is not a corner case. Measured 2026-08-28 across a real library:
	// sources that are already compressed hard need a HIGHER share of their
	// own bitrate, not a lower one, because there is nothing left to squeeze
	// out. One 60 fps source at 2.8 Mbit/s stayed at 53 % of itself from CQ 26
	// all the way down to CQ 30, and would still sit near 52 % at the clamp
	// ceiling. A fat 12 Mbit/s source drops to under 20 % over the same span.
	// Without this check a cap tuned for the fat sources quietly wrecks the
	// thin ones.
	budget.thriftiestKbps = autoCQEstimateKbps(sc, kbpsLow, rate, sc.clampMax)
	if budget.thriftiestKbps > budget.capKbps {
		budget.unreachable = true
		return budget, nil
	}
	// Ceiling, not rounding: the CQ has to land ON or BELOW the cap, and half
	// a step over it would defeat the whole purpose.
	needed := int(math.Ceil(float64(sc.anchorLow) + math.Log(kbpsLow/budget.capKbps)/rate))
	if needed > sc.clampMax {
		needed = sc.clampMax
	}
	if needed > pick {
		budget.pick = needed
	}
	return budget, nil
}

// autoCQPlateauPick returns the cheapest acceptable CQ on a curve whose
// reachable quality tops out below the search target. Base case is the low
// anchor (its measurement IS the plateau, minus noise); the user tolerance
// then buys additional steps toward the high anchor along the measured
// anchor slope — never beyond the high anchor, where no measurement exists.
// When even the anchor span is flat, the high anchor wins outright: the whole
// measured curve is level then and the extra bitrate of the low anchor buys nothing.
func autoCQPlateauPick(sc autoCQScale, vmafLow, vmafHigh, tolerance float64) (int, float64) {
	anchorGainPerStep := (vmafLow - vmafHigh) / float64(sc.anchorHigh-sc.anchorLow)
	if anchorGainPerStep < sc.saturationSlope {
		return sc.anchorHigh, vmafHigh
	}
	steps := int(tolerance / anchorGainPerStep) // floor: stay above (plateau - tolerance)
	if maxSteps := sc.anchorHigh - sc.anchorLow; steps > maxSteps {
		steps = maxSteps
	}
	return sc.anchorLow + steps, vmafLow - anchorGainPerStep*float64(steps)
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

// autoCQClimbFloor is the minimum VMAF a plateau-climb rung must still reach to
// be taken: the high-anchor score minus the (scaled) tolerance. On the finer
// AV1 scale climbToleranceFactor is 2.0, so AV1 may climb further for the same
// spend as H.265 — e.g. the real Big Buck Bunny case (anchor CQ 32 = 94.16)
// accepts CQ 38 at 93.65 (floor 93.16) instead of stalling at CQ 32, while a
// steep plateau (a rung well below the floor) still keeps the conservative pick.
func autoCQClimbFloor(sc autoCQScale, vmafHigh, tolerance float64) float64 {
	return vmafHigh - tolerance*sc.climbToleranceFactor
}

// autoCQPlateauFloor is the minimum VMAF a climb rung must reach when the
// target is proven unreachable: the measured plateau top minus the configured
// plateau tolerance. Unlike autoCQClimbFloor this is an absolute VMAF budget
// shared by both codecs — on a source whose quality tops out below the target,
// how much of that unreachable quality the savings may cost does not depend on
// the CQ scale. The budget is deliberately wider than autoCQTolerance: the
// spread inside a saturated plateau is largely re-encode noise of an already
// degraded picture, while every skipped CQ step wastes real bitrate (measured
// 2026-07-25: CQ 26 vs 28 both ride the maxrate cap and differ by ~2% file
// size at 0.03 VMAF).
func autoCQPlateauFloor(plateauTop, plateauTolerance float64) float64 {
	return plateauTop - plateauTolerance
}

// autoCQClimbBudgetFloor returns the climb floor for a proven-unreachable
// target. The wide plateau budget is only justified when the measured curve is
// FLAT: the spread between rungs is then re-encode noise of an already
// degraded picture (the autoCQPlateauFloor rationale). On a steep curve the
// same spread is real, visible quality — a near-miss at the low anchor must
// not give away several VMAF points for savings — so the climb may only spend
// the small search tolerance there, scaled per codec like autoCQClimbFloor.
func autoCQClimbBudgetFloor(sc autoCQScale, plateauTop float64, flatCurve bool,
	plateauTolerance, tolerance float64) float64 {
	if flatCurve {
		return autoCQPlateauFloor(plateauTop, plateauTolerance)
	}
	return autoCQClimbFloor(sc, plateauTop, tolerance)
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
// of the real encode (buildOpts, same filter chain, maxrate/bufsize/GOP) at the
// given CQ — so H.265 and AV1 each sample through their own encoder.
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
	filterChain string, fpsNum, fpsDen int, cq int, maxBitrate, bufsize string, gop int,
	sampleName string,
	buildOpts func(cq int, maxBitrate, bufsize string, gop int) []string) []string {

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
	args = append(args, buildOpts(cq, maxBitrate, bufsize, gop)...)
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
func buildAutoCQVMAFArgs(sourcePath string, windows [][2]float64, hwaccel []string,
	filterChain string, fpsNum, fpsDen int, sampleName, logName string) []string {

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
	// filterChainToCPU: libvmaf rechnet auf dem Prozessor und kann mit Bildern
	// im Grafikspeicher nichts anfangen (siehe Kommentar dort).
	fmt.Fprintf(&fg, "concat=n=%d:v=1:a=0,%s,format=yuv420p10le,%s[ref];",
		n, filterChainToCPU(filterChain), normPTS)
	fmt.Fprintf(&fg, "[%d:V:0]format=yuv420p10le,%s[dist];", n, normPTS)
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

// autoDetectCQ runs the full -autocq search for one file and returns the CQ to
// use. On ANY failure it warns and returns ok=false so the caller keeps the
// configured targetCQ — the Auto-CQ analysis must never break a conversion.
// The spinner keeps the analysis visibly alive (a silent multi-second pause
// would look like a hang).
func autoDetectCQ(ctx context.Context, filePath string, stats *VideoStats,
	filterChain, maxBitrate, bufsize string, gop int, doScale bool, sc autoCQScale) (int, bool) {

	// The tolerance (INI key autoCQTolerance) deliberately trades invisible
	// quality for disk space: the whole search runs against the reduced
	// target and treats it as hit. 0 = chase the full target.
	tolerance := appSettings.autoCQTolerance
	target := appSettings.autoCQTargetVMAF - tolerance

	windows := autoCQSampleWindows(stats.DurationSec)
	if windows == nil {
		pWarn.Printf("Auto-CQ: video too short for sampling (< %.0f s) — using fallback CQ %d.\n",
			autoCQMinSourceSec, sc.fallbackCQ())
		return 0, false
	}
	if stats.FPSNum <= 0 || stats.FPSDen <= 0 {
		pWarn.Printf("Auto-CQ: source frame rate unknown — using fallback CQ %d.\n",
			sc.fallbackCQ())
		return 0, false
	}

	tmpDir, err := os.MkdirTemp("", "NVENCForge_autocq_")
	if err != nil {
		pWarn.Printf("Auto-CQ: cannot create temp folder (%v) — using fallback CQ %d.\n",
			err, sc.fallbackCQ())
		return 0, false
	}
	defer os.RemoveAll(tmpDir)

	var sampleSec float64
	for _, w := range windows {
		sampleSec += w[1]
	}
	toleranceNote := ""
	if tolerance > 0 {
		toleranceNote = fmt.Sprintf(" (%.4g - %.4g tolerance)",
			appSettings.autoCQTargetVMAF, tolerance)
	}
	// Erst hier steht fest, dass wirklich gemessen wird — alle Abbruchgründe
	// (zu kurzes Video, unbekannte Bildrate, kein Temp-Ordner) liegen oben.
	// Die Analyse liefert keine Fortschrittswerte und dauert ein bis zwei
	// Minuten; ohne diese Meldung stünde eine Oberfläche so lange bei null,
	// ohne sagen zu können, warum.
	emitStage("analyze")

	pInfo.Printf("%s Auto-CQ: analyzing %d sample windows (%.0f s) for VMAF target %.4g%s...\n",
		pterm.LightMagenta("›"), len(windows), sampleSec, target, toleranceNote)

	spinner, _ := pterm.DefaultSpinner.WithText(autoCQSpinnerText(autoCQSpinnerScanText)).Start()
	analysisStart := time.Now()

	// Window placement: demux the packet sizes once and put the windows on
	// the bitrate profile, so the hardest scene is guaranteed to be sampled.
	// Any profile problem falls back silently to the fixed positions — the
	// placement is an optimisation, never a reason to fail the analysis.
	placement := "fixed positions"
	var profileErr error
	gapNote := ""
	// The profile outlives the placement decision: the cost cap needs it again
	// at the end, to read the source rate at exactly the sampled seconds.
	var profile []bitrateBucket
	if buckets, videoPackets, perr := probeSourceBitrateBuckets(ctx, filePath, stats.DurationSec, windows[0][1]); perr != nil {
		if ctx.Err() != nil {
			_ = spinner.Stop()
			return 0, false
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

	fail := func(step string, err error) (int, bool) {
		_ = spinner.Stop()
		if ctx.Err() != nil {
			return 0, false // user abort — no misleading failure warning
		}
		pWarn.Printf("Auto-CQ: %s failed — using fallback CQ %d.\n", step, sc.fallbackCQ())
		pDetail.Printf("Auto-CQ detail: %v\n", err)
		return 0, false
	}

	measure := func(cq int) (float64, error) {
		sampleName := fmt.Sprintf("sample_cq%d.mkv", cq)
		logName := fmt.Sprintf("vmaf_cq%d.json", cq)
		buildEnc := func(hw []string, chain string) []string {
			return buildAutoCQEncodeArgs(filePath, windows, hw, chain,
				stats.FPSNum, stats.FPSDen,
				cq, maxBitrate, bufsize, gop, sampleName, sc.buildOpts)
		}
		buildVMAF := func(hw []string, chain string) []string {
			return buildAutoCQVMAFArgs(filePath, windows, hw, chain,
				stats.FPSNum, stats.FPSDen, sampleName, logName)
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

	vmafLow, err := measure(sc.anchorLow)
	if err != nil {
		return fail(fmt.Sprintf("anchor measurement at CQ %d", sc.anchorLow), err)
	}
	vmafHigh, err := measure(sc.anchorHigh)
	if err != nil {
		return fail(fmt.Sprintf("anchor measurement at CQ %d", sc.anchorHigh), err)
	}

	cq, predicted := interpolateAutoCQ(sc, vmafLow, vmafHigh, target)

	// The interpolated pick is ALWAYS confirmed by one real measurement: the
	// linear model is only exact at the anchors, and between/beyond them the
	// bent VMAF(CQ) curve tends to promise slightly more quality than the
	// encode delivers. A pick that IS an anchor already carries its
	// measurement. On a miss, autoCQStepDown estimates from the anchor slope
	// how many CQ steps the shortfall costs and steps down in one go.
	slope := (vmafHigh - vmafLow) / float64(sc.anchorHigh-sc.anchorLow)
	verifyNote := ""
	plateauLevel := 0.0 // > 0: target proven unreachable — climb may probe higher rungs
	// plateauFlat: the measured curve is proven flat around the pick, so the
	// spread up to the climb rungs is mostly re-encode noise and the full
	// plateau budget applies. A steep curve (real quality per CQ step) keeps
	// the climb on the small search tolerance instead — see autoCQClimbBudgetFloor.
	plateauFlat := false
	switch {
	case cq == sc.anchorLow:
		predicted, verifyNote = vmafLow, " (anchor measurement)"
		if vmafLow < target && tolerance > 0 {
			// Even the low anchor misses the search target, so the target is
			// only reachable (if at all) by escalating below the low anchor —
			// the same spend-vs-gain trade the saturation brake handles. The
			// tolerance picks the cheapest CQ within reach of the anchor score.
			// Flatness evidence here is the anchor span alone: a near-miss on
			// a steep curve is NOT a plateau, merely a target grazed by.
			plateauLevel = vmafLow
			plateauFlat = -slope < sc.saturationSlope
			if satCQ, satVMAF := autoCQPlateauPick(sc, vmafLow, vmafHigh, tolerance); satCQ != cq {
				verifyNote = fmt.Sprintf(
					" (VMAF tops out at ~%.1f — target %.4g unreachable, tolerance picks CQ %d)",
					vmafLow, target, satCQ)
				cq, predicted = satCQ, satVMAF
			}
		}
	case cq == sc.anchorHigh:
		predicted, verifyNote = vmafHigh, " (anchor measurement)"
	default:
		verified, verr := measure(cq)
		switch {
		case verr != nil && ctx.Err() != nil:
			return fail("verification", verr)
		case verr != nil:
			// The anchors were fine, so keep the interpolated pick.
			verifyNote = " (verification failed, interpolated value kept)"
			pDetail.Printf("Auto-CQ verification detail: %v\n", verr)
		case verified < target && autoCQSaturated(sc, cq, verified, vmafLow):
			// Saturation brake: the source is already compressed so hard
			// that VMAF plateaus below the target — more bitrate buys no
			// quality. Fall back to the cheapest CQ still on the plateau
			// instead of stepping further down into pure waste.
			satCQ, satVMAF := autoCQPlateauPick(sc, vmafLow, vmafHigh, tolerance)
			verifyNote = fmt.Sprintf(
				" (VMAF saturates at ~%.1f — target %.4g unreachable, picking efficient CQ %d)",
				math.Max(verified, vmafLow), target, satCQ)
			cq, predicted = satCQ, satVMAF
			plateauLevel = math.Max(verified, vmafLow)
			plateauFlat = true // saturation proven by the real sub-anchor measurement
		case verified < target && autoCQGainTooSmall(sc, cq, verified, vmafLow):
			// Thrift brake: the target IS still reachable further down, but below
			// the low anchor each step buys so little VMAF that it does not pay for
			// the bitrate it costs. Fall back to the low anchor — the last CQ whose
			// step still earned its place, and the last one carrying a measured
			// instead of an extrapolated score. Deliberately no plateauLevel: the
			// target is NOT proven unreachable here, so the plateau climb (which
			// may spend whole VMAF points on savings) must stay out of this case.
			gainPerStep := (verified - vmafLow) / float64(sc.anchorLow-cq)
			verifyNote = fmt.Sprintf(
				" (CQ %d measured %.1f — each step below CQ %d buys only %.2f VMAF, not worth the size)",
				cq, verified, sc.anchorLow, gainPerStep)
			cq, predicted = sc.anchorLow, vmafLow
		case verified < target:
			stepped, pred, capped := autoCQStepDown(sc, cq, target, verified, slope)
			switch {
			case stepped == cq:
				// The clamp floor itself measured below the target — proven
				// unreachable, so the climb may still trade quality for size.
				// The curve is NOT saturated here (the brake would have fired),
				// so only the small climb budget applies.
				predicted = verified
				verifyNote = fmt.Sprintf(" (measured %.1f — CQ clamp floor reached, target missed)", verified)
				plateauLevel = verified
			case capped:
				// The verification miss already proved the anchor slope too
				// optimistic, and the correction jump was capped at maxStepDown —
				// pred would be an extrapolation far outside the measured points.
				// Replace it with a real measurement; if even that misses the
				// target, the two fresh points give a realistic LOCAL slope for
				// one final, ordinary step-down (accepted unmeasured, exactly
				// like an uncapped step-down).
				remeasured, rerr := measure(stepped)
				switch {
				case rerr != nil && ctx.Err() != nil:
					return fail("step-down re-measurement", rerr)
				case rerr != nil:
					// The anchors and the first verification were fine — keep
					// the stepped pick with its estimate, like a failed verify.
					verifyNote = fmt.Sprintf(
						" (CQ %d measured %.1f, stepped down to CQ %d — re-measurement failed, estimate kept)",
						cq, verified, stepped)
					pDetail.Printf("Auto-CQ re-measurement detail: %v\n", rerr)
					cq, predicted = stepped, pred
				case remeasured >= target:
					verifyNote = fmt.Sprintf(" (CQ %d measured %.1f, stepped down to CQ %d, verified)",
						cq, verified, stepped)
					cq, predicted = stepped, remeasured
				default:
					localSlope := (verified - remeasured) / float64(cq-stepped)
					final, finalPred, _ := autoCQStepDown(sc, stepped, target, remeasured, localSlope)
					// Nur die Notiz hängt vom Zweig ab — sie braucht das noch
					// nicht überschriebene cq. Der Pick selbst wird DANACH
					// gesetzt, ein einziges Mal für beide Fälle: von 1.6.1 bis
					// 1.30.0 stand die Zuweisung in beiden Zweigen doppelt, ein
					// Umbau verlor dabei das else, und im Fall "es geht noch
					// eine Stufe tiefer" blieb der interpolierte CQ stehen —
					// obwohl zwei Messungen ihn bereits widerlegt hatten.
					if final == stepped {
						// Same proven-unreachable case as the direct clamp-floor
						// branch above: allow the (small-budget) climb. The pick
						// moves to the clamp floor it just measured — keeping the
						// interpolated CQ would pair the floor's score and note
						// with a pick whose own measurement already fell below
						// the climb budget.
						verifyNote = fmt.Sprintf(" (measured %.1f — CQ clamp floor reached, target missed)", remeasured)
						plateauLevel = remeasured
					} else {
						verifyNote = fmt.Sprintf(" (CQ %d = %.1f, CQ %d = %.1f, stepped down to CQ %d)",
							cq, verified, stepped, remeasured, final)
					}
					cq, predicted = autoCQFinalStepPick(stepped, final, remeasured, finalPred)
				}
			default:
				verifyNote = fmt.Sprintf(" (CQ %d measured %.1f, stepped down to CQ %d)", cq, verified, stepped)
				cq, predicted = stepped, pred
			}
		default:
			predicted = verified
			verifyNote = " (verified)"
		}
	}

	// Plateau climb: a measured plateau below the target says nothing about
	// where the plateau ENDS — CQ rungs above the pick may still cost next to
	// nothing on such sources (2026-07-25 case: CQ 26 and 28 both ride the
	// maxrate cap, the real savings only start above the high anchor). Probe
	// the clamp ceiling first (cheapest file), then the lower rungs; a rung is
	// taken only when its REAL measurement holds the floor. With the plateau
	// tolerance configured every unreachable-target pick climbs; the floor is
	// (plateau top - autoCQPlateauTolerance) on a proven-flat curve and the
	// much tighter search-tolerance floor on a steep one (see
	// autoCQClimbBudgetFloor). At 0 the pre-1.5.0 behaviour remains (only a
	// flat anchor span climbs, floored by autoCQClimbFloor). A probe failure
	// keeps the safe pick — the climb is a bonus, never a reason to fail the
	// analysis. A healthy curve that reaches its target never gets here
	// (plateauLevel == 0).
	var plateauProbes []string
	anchorGainPerStep := -slope // VMAF gained per CQ step down, across the anchors
	climbFloor, climbing := 0.0, false
	switch {
	case plateauLevel <= 0 || tolerance <= 0:
		// healthy curve, or savings disabled entirely
	case appSettings.autoCQPlateauTolerance > 0:
		climbFloor = autoCQClimbBudgetFloor(sc, plateauLevel, plateauFlat,
			appSettings.autoCQPlateauTolerance, tolerance)
		climbing = true
	case anchorGainPerStep < sc.saturationSlope && cq == sc.anchorHigh:
		climbFloor, climbing = autoCQClimbFloor(sc, vmafHigh, tolerance), true
	}
	if climbing {
		for _, rung := range autoCQClimbCandidates(sc, cq) {
			// The anchor rungs were already measured at the start of the
			// search — reuse those scores instead of burning ~15 s on an
			// identical encode+measurement. (No switch here: its break would
			// only leave the switch, not this probing loop.)
			var score float64
			if rung == sc.anchorHigh {
				score = vmafHigh
			} else if rung == sc.anchorLow {
				score = vmafLow
			} else {
				var cerr error
				if score, cerr = measure(rung); cerr != nil {
					if ctx.Err() == nil {
						pDetail.Printf("Auto-CQ plateau probe detail: %v\n", cerr)
					}
					break
				}
			}
			plateauProbes = append(plateauProbes, fmt.Sprintf("CQ %d = %.2f", rung, score))
			if score >= climbFloor {
				cq, predicted = rung, score
				verifyNote = fmt.Sprintf(
					" (VMAF plateaus at ~%.1f — target %.4g unreachable, plateau holds to CQ %d)",
					plateauLevel, target, rung)
				break
			}
		}
	}

	// Cost cap (INI key autoCQMaxSourcePercent, 45 % by default, 0 = off): the quality
	// target counts only as long as reaching it stays within a share of what
	// the source itself spends. Grainy or very busy material can push Auto-CQ
	// into picks that cost more than half the source rate for VMAF nobody sees
	// — measured 2026-08-28 on a 50 fps source: CQ 26 spends 55 % of the
	// source, CQ 29 only 36 %.
	//
	// It runs LAST, after every quality mechanism has had its say, because it
	// is not a quality argument at all: it is the user's budget overruling the
	// result. And it deliberately does NOT touch -maxrate. An encoder ceiling
	// makes several CQ steps measure the same size and the same score, which
	// is precisely the false plateau the saturation brake must never see
	// (measured 2026-07-27) — capping the SEARCH keeps every step honest.
	costCapNote := ""
	if budget, cerr := autoCQCostCapTarget(sc, tmpDir, profile, windows,
		sampleSec, appSettings.autoCQMaxSourcePercent, cq); cerr != nil {
		// A cap that cannot be worked out must never fail the analysis; the
		// file simply keeps the quality-driven pick, as in every version
		// before this one.
		if debugMode && appSettings.autoCQMaxSourcePercent > 0 {
			pDetail.Printf("Auto-CQ cost cap detail: %v\n", cerr)
		}
	} else if budget.unreachable {
		// Say it out loud. Silence here would look like the cap simply did not
		// apply, and the user would have no way to tell an unreachable cap from
		// a file that was already cheap enough.
		costCapNote = fmt.Sprintf(
			"  · note: the %.4g%% cost cap was left alone — this source is already compressed so hard that even CQ %d would still spend %.0f%% of it, so capping would cost picture without saving anything",
			appSettings.autoCQMaxSourcePercent, sc.clampMax,
			budget.sharePct(budget.thriftiestKbps))
	} else if budget.fires(cq) {
		capped := budget.pick
		score, merr := measure(capped)
		switch {
		case merr != nil && ctx.Err() != nil:
			return fail("cost cap measurement", merr)
		case merr != nil:
			// Anchors and search were fine, only the confirming measurement
			// failed. Keep the capped pick with an estimated score: the cap is
			// the point of this step, and dropping it here would hand back
			// exactly the oversized file it exists to prevent.
			pDetail.Printf("Auto-CQ cost cap detail: %v\n", merr)
			predicted = vmafLow + slope*float64(capped-sc.anchorLow)
			verifyNote = fmt.Sprintf(
				" (cost cap %.0f%%: CQ %d would spend %.0f%% of the source, using CQ %d — measurement failed, estimate kept)",
				appSettings.autoCQMaxSourcePercent, cq, budget.sharePct(budget.pickKbps), capped)
			cq = capped
		default:
			// Report what the capped encode really costs, not what the model
			// predicted — the sample for it now exists.
			realShare := budget.sharePct(budget.pickKbps)
			if realKbps, kerr := autoCQSampleKbps(tmpDir, capped, sampleSec); kerr == nil {
				realShare = budget.sharePct(realKbps)
			}
			verifyNote = fmt.Sprintf(
				" (cost cap %.0f%%: CQ %d would spend %.0f%% of the source — CQ %d measured %.1f at %.0f%%)",
				appSettings.autoCQMaxSourcePercent, cq, budget.sharePct(budget.pickKbps),
				capped, score, realShare)
			cq, predicted = capped, score
		}
	}

	_ = spinner.Stop()
	if ctx.Err() != nil {
		return 0, false
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
	// Dieselbe Auskunft noch einmal für eine Oberfläche — als Ereignis, nicht
	// als Text zum Zerlegen.
	emitCQ(cq, predicted, target, noteText)
	fmt.Println(pterm.Gray(fmt.Sprintf("  · anchors: CQ %d = %.2f, CQ %d = %.2f · windows: %s · analysis took %s",
		sc.anchorLow, vmafLow, sc.anchorHigh, vmafHigh, placement,
		formatDuration(time.Since(analysisStart).Seconds()))))
	if len(plateauProbes) > 0 {
		fmt.Println(pterm.Gray("  · plateau probes: " + strings.Join(plateauProbes, ", ")))
	}
	if gapNote != "" {
		fmt.Println(pterm.Gray(gapNote))
	}
	if costCapNote != "" {
		fmt.Println(pterm.Gray(costCapNote))
	}
	// An unreachable target on a cap-limited source says something about the
	// configured ceiling, not about the material. Without this line the plateau
	// message reads as "the source is exhausted" while the real limit is a
	// setting the user can change.
	//
	// The second half used to promise "a higher cap buys quality at the price of
	// size". A measurement over three high-bitrate 50/60 fps sources (2026-08-15,
	// caps 8000/11000/12000) disproved that: the cap only holds back the high CQ
	// steps, and Auto-CQ never picks those — at the step it does pick, raising
	// the cap bought 0.04 VMAF for up to 4 % more size. Saying otherwise sends
	// the reader chasing a setting that cannot help.
	if plateauLevel > 0 {
		if sourceKbps, capLimited := autoCQCapLimitsQuality(stats, maxBitrate); capLimited {
			fmt.Println(pterm.Gray(fmt.Sprintf(
				"  · note: the %s bitrate cap sets this ceiling, not the source (source runs at %.1f Mbit/s) — raising it was measured to add size, not quality",
				maxBitrate, float64(sourceKbps)/1000)))
		}
	}
	if profileErr != nil && debugMode {
		fmt.Println(pterm.Gray("  · bitrate profile skipped: " + profileErr.Error()))
	}
	return cq, true
}

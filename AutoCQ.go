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
// cheapest CQ on the measured plateau instead of chasing the target; when
// that plateau is flat across the anchors, rungs above the high anchor
// (up to the clamp ceiling) are probed too and taken when their measured
// score stays within the tolerance of the high anchor — the file shrinks
// as far as real measurements justify, never on extrapolation.
// H.265 only — av1_nvenc uses a different CQ scale and would need its own
// calibration.
//
// The two documented VMAF measurement pitfalls are handled here:
//   1. A decoded segment keeps the source's start offset, which shifts the
//      frame pairing → setpts=PTS-STARTPTS re-bases every window before use.
//   2. Matroska rounds PTS to milliseconds and libvmaf pairs by timestamp,
//      which makes the comparison jump by one frame → both VMAF inputs get
//      settb=AVTB,setpts=N*fpsDen/fpsNum/TB (frame-number-based timestamps).
// ----------------------------------------------------------------------------

const (
	// Anchor CQ values for the two calibration encodes. 26/30 bracket the
	// practically useful range on real-world material (see the CQ measurement
	// series in NVENCForge_Qualitaetsanalyse.md).
	autoCQAnchorLow  = 26
	autoCQAnchorHigh = 30

	// The final pick is clamped to this range: below 20 the gains are
	// invisible, above 34 even easy material visibly degrades.
	autoCQClampMin = 20
	autoCQClampMax = 34

	// A verification miss steps down at most this many CQ steps — the anchor
	// slope estimates how many are needed, this cap keeps a single noisy
	// measurement from pushing the pick far into oversized files.
	autoCQMaxStepDown = 3

	// Below this measured VMAF gain per CQ step the curve counts as
	// saturated: the source's own compression artifacts cap the score, so
	// chasing the target buys bitrate but no quality (pre-compressed P2P
	// sources plateau this way). 0.1 sits well above sample measurement
	// noise (~0.05) and well below any real slope (0.3+).
	autoCQSaturationSlope = 0.1

	// Window placement via the source bitrate profile: the first/last 5% are
	// skipped (intros, credits), and a profile whose heaviest bucket stays
	// under 1.25x the median carries no placement signal (CBR-ish source) —
	// the fixed positions are used instead. Max/median (not p90/p10) so a
	// single hard scene inside an otherwise calm film still counts as signal.
	autoCQEdgeMarginPct    = 0.05
	autoCQFlatProfileRatio = 1.25

	// Hard timeout for the packet-size demux (no decode — normally seconds).
	autoCQProfileTimeout = 2 * time.Minute

	// Sampling layout: long sources get four 8-second windows, short sources
	// (under 4 minutes) two 6-second windows, and anything under 30 seconds
	// is not sampled at all (falls back to the configured targetCQ).
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
	positions := []float64{0.15, 0.35, 0.55, 0.75}
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
// should hit the VMAF target, rounded and clamped to [autoCQClampMin,
// autoCQClampMax]. The second return value is the VMAF predicted for that CQ.
// A near-flat (or rising) slope means the two anchors measured practically the
// same — pure measurement noise on very easy or very hard material — so the
// pick falls to the clamp edge matching the side of the target.
func interpolateAutoCQ(vmafLow, vmafHigh, target float64) (int, float64) {
	slope := (vmafHigh - vmafLow) / float64(autoCQAnchorHigh-autoCQAnchorLow)
	var exact, predicted float64
	if slope > -0.01 {
		if vmafHigh >= target {
			exact, predicted = autoCQClampMax, vmafHigh
		} else {
			exact, predicted = autoCQClampMin, vmafLow
		}
	} else {
		exact = float64(autoCQAnchorLow) + (target-vmafLow)/slope
	}
	cq := int(math.Round(exact))
	if cq < autoCQClampMin {
		cq = autoCQClampMin
	}
	if cq > autoCQClampMax {
		cq = autoCQClampMax
	}
	if slope <= -0.01 {
		predicted = vmafLow + slope*float64(cq-autoCQAnchorLow)
	}
	return cq, predicted
}

// autoCQStepDown returns the CQ to fall back to after the verification
// measurement missed the target, plus the VMAF predicted for that CQ. The
// step count comes from the anchor slope (how much VMAF one CQ step buys),
// capped at autoCQMaxStepDown and clamped at autoCQClampMin — so the
// returned CQ can equal the input when the clamp floor is already reached.
func autoCQStepDown(cq int, target, verified, slope float64) (int, float64) {
	steps := 1
	if slope < -0.01 {
		if s := int(math.Ceil((target - verified) / -slope)); s > steps {
			steps = s
		}
	}
	if steps > autoCQMaxStepDown {
		steps = autoCQMaxStepDown
	}
	stepped := cq - steps
	if stepped < autoCQClampMin {
		stepped = autoCQClampMin
	}
	predicted := verified - slope*float64(cq-stepped)
	if predicted > 100 {
		predicted = 100
	}
	return stepped, predicted
}

// autoCQSaturated reports whether the verification measurement below the
// low anchor exposes a saturated VMAF curve: the measured gain per CQ step
// from the anchor down to the pick stays under autoCQSaturationSlope. The
// anchor slope alone cannot see this — saturation starts left of the
// anchors, exactly where the verification measurement sits.
func autoCQSaturated(cq int, verified, vmafLow float64) bool {
	if cq >= autoCQAnchorLow {
		return false
	}
	return (verified-vmafLow)/float64(autoCQAnchorLow-cq) < autoCQSaturationSlope
}

// autoCQPlateauPick returns the cheapest acceptable CQ on a curve whose
// reachable quality tops out below the search target. Base case is the low
// anchor (its measurement IS the plateau, minus noise); the user tolerance
// then buys additional steps toward the high anchor along the measured
// anchor slope — never beyond it, past CQ 30 there is no measurement. When
// even the anchor span is flat, the high anchor wins outright: the whole
// measured curve is level then and the extra bitrate of CQ 26 buys nothing.
func autoCQPlateauPick(vmafLow, vmafHigh, tolerance float64) (int, float64) {
	anchorGainPerStep := (vmafLow - vmafHigh) / float64(autoCQAnchorHigh-autoCQAnchorLow)
	if anchorGainPerStep < autoCQSaturationSlope {
		return autoCQAnchorHigh, vmafHigh
	}
	steps := int(tolerance / anchorGainPerStep) // floor: stay above (plateau - tolerance)
	if maxSteps := autoCQAnchorHigh - autoCQAnchorLow; steps > maxSteps {
		steps = maxSteps
	}
	return autoCQAnchorLow + steps, vmafLow - anchorGainPerStep*float64(steps)
}

// autoCQClimbCandidates returns the CQ rungs the plateau climb probes above
// the high anchor, cheapest file first: the clamp ceiling, then the midpoint
// between anchor and ceiling as the smaller fallback step. With the current
// constants that is CQ 34, then CQ 32.
func autoCQClimbCandidates() []int {
	mid := (autoCQAnchorHigh + autoCQClampMax) / 2
	if mid <= autoCQAnchorHigh || mid >= autoCQClampMax {
		return []int{autoCQClampMax}
	}
	return []int{autoCQClampMax, mid}
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
// sizes into windowLen-sized buckets.
func probeSourceBitrateBuckets(ctx context.Context, filePath string,
	durationSec, windowLen float64) ([]bitrateBucket, error) {

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
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("AutoCQ.go: probeSourceBitrateBuckets: %w", err)
	}
	buckets := bucketsFromPacketCSV(string(out), durationSec, windowLen)
	if len(buckets) == 0 {
		return nil, errors.New("AutoCQ.go: probeSourceBitrateBuckets: no usable packet data")
	}
	return buckets, nil
}

// bucketsFromPacketCSV turns ffprobe "pts_time,size" CSV lines into full
// windowLen-sized buckets. Lines without a parsable timestamp or size
// (e.g. "N/A") are skipped; the partial tail bucket is dropped so its
// deflated average cannot skew the placement.
func bucketsFromPacketCSV(csv string, durationSec, windowLen float64) []bitrateBucket {
	if windowLen <= 0 || durationSec < windowLen {
		return nil
	}
	n := int(durationSec / windowLen)
	sums := make([]int64, n)
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
	return buckets
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
// of the real encode (same filter chain, maxrate/bufsize/GOP) at the given CQ.
// setpts=PTS-STARTPTS per window re-bases the decoded segment timestamps
// (pitfall 1), concat then joins the windows into one stream.
func buildAutoCQEncodeArgs(sourcePath string, windows [][2]float64, filterChain string,
	cq int, maxBitrate, bufsize string, gop int, sampleName string) []string {

	args := []string{"-y"}
	for _, w := range windows {
		args = append(args,
			"-ss", strconv.FormatFloat(w[0], 'f', 3, 64),
			"-t", strconv.FormatFloat(w[1], 'f', 3, 64),
			"-i", sourcePath)
	}
	var fg strings.Builder
	for i := range windows {
		fmt.Fprintf(&fg, "[%d:V:0]setpts=PTS-STARTPTS[w%d];", i, i)
	}
	for i := range windows {
		fmt.Fprintf(&fg, "[w%d]", i)
	}
	fmt.Fprintf(&fg, "concat=n=%d:v=1:a=0,%s[out]", len(windows), filterChain)
	args = append(args, "-filter_complex", fg.String(), "-map", "[out]", "-an", "-sn")
	args = append(args, buildNVENCOptsWithCQ(cq, maxBitrate, bufsize, gop)...)
	return append(args, sampleName)
}

// buildAutoCQVMAFArgs assembles the FFmpeg call that measures an anchor file
// against the freshly decoded source windows. The reference side runs through
// the SAME filter chain as the encode, so the score isolates the encoder loss
// (not scaling/sharpening). Both sides are forced to yuv420p10le and to
// frame-number-based timestamps (pitfall 2). n_subsample=3 scores every third
// frame — plenty for a sample and three times faster.
func buildAutoCQVMAFArgs(sourcePath string, windows [][2]float64, filterChain string,
	fpsNum, fpsDen int, sampleName, logName string) []string {

	var args []string
	for _, w := range windows {
		args = append(args,
			"-ss", strconv.FormatFloat(w[0], 'f', 3, 64),
			"-t", strconv.FormatFloat(w[1], 'f', 3, 64),
			"-i", sourcePath)
	}
	args = append(args, "-i", sampleName)

	normPTS := fmt.Sprintf("settb=AVTB,setpts=N*%d/%d/TB", fpsDen, fpsNum)
	threads := runtime.NumCPU()
	if threads < 1 {
		threads = 1
	}
	n := len(windows)
	var fg strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&fg, "[%d:V:0]setpts=PTS-STARTPTS[w%d];", i, i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&fg, "[w%d]", i)
	}
	fmt.Fprintf(&fg, "concat=n=%d:v=1:a=0,%s,format=yuv420p10le,%s[ref];", n, filterChain, normPTS)
	fmt.Fprintf(&fg, "[%d:V:0]format=yuv420p10le,%s[dist];", n, normPTS)
	fmt.Fprintf(&fg, "[dist][ref]libvmaf=log_fmt=json:log_path=%s:n_subsample=3:n_threads=%d",
		logName, threads)
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
	filterChain, maxBitrate, bufsize string, gop int) (int, bool) {

	// The tolerance (INI key autoCQTolerance) deliberately trades invisible
	// quality for disk space: the whole search runs against the reduced
	// target and treats it as hit. 0 = chase the full target.
	tolerance := appSettings.autoCQTolerance
	target := appSettings.autoCQTargetVMAF - tolerance

	windows := autoCQSampleWindows(stats.DurationSec)
	if windows == nil {
		pWarn.Printf("Auto-CQ: video too short for sampling (< %.0f s) — using targetCQ %d.\n",
			autoCQMinSourceSec, appSettings.targetCQ)
		return 0, false
	}
	if stats.FPSNum <= 0 || stats.FPSDen <= 0 {
		pWarn.Printf("Auto-CQ: source frame rate unknown — using targetCQ %d.\n",
			appSettings.targetCQ)
		return 0, false
	}

	tmpDir, err := os.MkdirTemp("", "NVENCForge_autocq_")
	if err != nil {
		pWarn.Printf("Auto-CQ: cannot create temp folder (%v) — using targetCQ %d.\n",
			err, appSettings.targetCQ)
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
	if buckets, perr := probeSourceBitrateBuckets(ctx, filePath, stats.DurationSec, windows[0][1]); perr != nil {
		if ctx.Err() != nil {
			_ = spinner.Stop()
			return 0, false
		}
		profileErr = perr
	} else if gw := autoCQGuidedWindows(buckets, stats.DurationSec, len(windows), windows[0][1]); gw != nil {
		windows, placement = gw, "bitrate-guided"
	}

	fail := func(step string, err error) (int, bool) {
		_ = spinner.Stop()
		if ctx.Err() != nil {
			return 0, false // user abort — no misleading failure warning
		}
		pWarn.Printf("Auto-CQ: %s failed — using targetCQ %d.\n", step, appSettings.targetCQ)
		pErr.Printf("Auto-CQ detail: %v\n", err)
		return 0, false
	}

	measure := func(cq int) (float64, error) {
		sampleName := fmt.Sprintf("sample_cq%d.mkv", cq)
		logName := fmt.Sprintf("vmaf_cq%d.json", cq)
		spinner.UpdateText(autoCQSpinnerText("Auto-CQ: encoding samples at CQ %d...", cq))
		if err := runAutoCQFFmpeg(ctx, tmpDir, buildAutoCQEncodeArgs(
			filePath, windows, filterChain, cq, maxBitrate, bufsize, gop, sampleName)); err != nil {
			return 0, fmt.Errorf("sample encode at CQ %d: %w", cq, err)
		}
		spinner.UpdateText(autoCQSpinnerText("Auto-CQ: measuring VMAF at CQ %d...", cq))
		if err := runAutoCQFFmpeg(ctx, tmpDir, buildAutoCQVMAFArgs(
			filePath, windows, filterChain, stats.FPSNum, stats.FPSDen, sampleName, logName)); err != nil {
			return 0, fmt.Errorf("VMAF measurement at CQ %d: %w", cq, err)
		}
		score, err := readVMAFScore(filepath.Join(tmpDir, logName))
		if err != nil {
			return 0, fmt.Errorf("VMAF result at CQ %d: %w", cq, err)
		}
		return score, nil
	}

	vmafLow, err := measure(autoCQAnchorLow)
	if err != nil {
		return fail(fmt.Sprintf("anchor measurement at CQ %d", autoCQAnchorLow), err)
	}
	vmafHigh, err := measure(autoCQAnchorHigh)
	if err != nil {
		return fail(fmt.Sprintf("anchor measurement at CQ %d", autoCQAnchorHigh), err)
	}

	cq, predicted := interpolateAutoCQ(vmafLow, vmafHigh, target)

	// The interpolated pick is ALWAYS confirmed by one real measurement: the
	// linear model is only exact at the anchors, and between/beyond them the
	// bent VMAF(CQ) curve tends to promise slightly more quality than the
	// encode delivers. A pick that IS an anchor already carries its
	// measurement. On a miss, autoCQStepDown estimates from the anchor slope
	// how many CQ steps the shortfall costs and steps down in one go.
	slope := (vmafHigh - vmafLow) / float64(autoCQAnchorHigh-autoCQAnchorLow)
	verifyNote := ""
	plateauLevel := 0.0 // > 0: a plateau path picked the high anchor — climb may probe higher
	switch {
	case cq == autoCQAnchorLow:
		predicted, verifyNote = vmafLow, " (anchor measurement)"
		if vmafLow < target && tolerance > 0 {
			// Even the low anchor misses the search target, so the target is
			// only reachable (if at all) by escalating below CQ 26 — the same
			// spend-vs-gain trade the saturation brake handles. The tolerance
			// picks the cheapest CQ within reach of the anchor measurement.
			if satCQ, satVMAF := autoCQPlateauPick(vmafLow, vmafHigh, tolerance); satCQ != cq {
				verifyNote = fmt.Sprintf(
					" (VMAF tops out at ~%.1f — target %.4g unreachable, tolerance picks CQ %d)",
					vmafLow, target, satCQ)
				cq, predicted = satCQ, satVMAF
				if satCQ == autoCQAnchorHigh {
					plateauLevel = vmafLow
				}
			}
		}
	case cq == autoCQAnchorHigh:
		predicted, verifyNote = vmafHigh, " (anchor measurement)"
	default:
		verified, verr := measure(cq)
		switch {
		case verr != nil && ctx.Err() != nil:
			return fail("verification", verr)
		case verr != nil:
			// The anchors were fine, so keep the interpolated pick.
			verifyNote = " (verification failed, interpolated value kept)"
			pErr.Printf("Auto-CQ verification detail: %v\n", verr)
		case verified < target && autoCQSaturated(cq, verified, vmafLow):
			// Saturation brake: the source is already compressed so hard
			// that VMAF plateaus below the target — more bitrate buys no
			// quality. Fall back to the cheapest CQ still on the plateau
			// instead of stepping further down into pure waste.
			satCQ, satVMAF := autoCQPlateauPick(vmafLow, vmafHigh, tolerance)
			verifyNote = fmt.Sprintf(
				" (VMAF saturates at ~%.1f — target %.4g unreachable, picking efficient CQ %d)",
				math.Max(verified, vmafLow), target, satCQ)
			cq, predicted = satCQ, satVMAF
			if satCQ == autoCQAnchorHigh {
				plateauLevel = math.Max(verified, vmafLow)
			}
		case verified < target:
			stepped, pred := autoCQStepDown(cq, target, verified, slope)
			if stepped == cq {
				predicted = verified
				verifyNote = fmt.Sprintf(" (measured %.1f — CQ clamp floor reached, target missed)", verified)
			} else {
				verifyNote = fmt.Sprintf(" (CQ %d measured %.1f, stepped down to CQ %d)", cq, verified, stepped)
				cq, predicted = stepped, pred
			}
		default:
			predicted = verified
			verifyNote = " (verified)"
		}
	}

	// Plateau climb: a flat anchor span that sent the plateau pick to the
	// high anchor says nothing about where the plateau ENDS — CQ rungs above
	// 30 may still cost next to nothing on such sources. Probe the clamp
	// ceiling first (cheapest file), then the midpoint; a rung is taken only
	// when its REAL measurement stays within the user tolerance of the high
	// anchor's score. A probe failure keeps the safe high-anchor pick — the
	// climb is a bonus, never a reason to fail the analysis. A healthy curve
	// that reaches its target at CQ 30 never gets here (plateauLevel == 0).
	var plateauProbes []string
	anchorGainPerStep := -slope // VMAF gained per CQ step down, across the anchors
	if plateauLevel > 0 && tolerance > 0 && anchorGainPerStep < autoCQSaturationSlope {
		climbFloor := vmafHigh - tolerance
		for _, rung := range autoCQClimbCandidates() {
			score, cerr := measure(rung)
			if cerr != nil {
				if ctx.Err() == nil {
					pErr.Printf("Auto-CQ plateau probe detail: %v\n", cerr)
				}
				break
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

	_ = spinner.Stop()
	if ctx.Err() != nil {
		return 0, false
	}
	pOK.Printf("Auto-CQ: %d → predicted VMAF %.1f (target %.4g)%s\n",
		cq, predicted, target, verifyNote)
	fmt.Println(pterm.Gray(fmt.Sprintf("  · anchors: CQ %d = %.2f, CQ %d = %.2f · windows: %s · analysis took %s",
		autoCQAnchorLow, vmafLow, autoCQAnchorHigh, vmafHigh, placement,
		formatDuration(time.Since(analysisStart).Seconds()))))
	if len(plateauProbes) > 0 {
		fmt.Println(pterm.Gray("  · plateau probes: " + strings.Join(plateauProbes, ", ")))
	}
	if profileErr != nil && debugMode {
		fmt.Println(pterm.Gray("  · bitrate profile skipped: " + profileErr.Error()))
	}
	return cq, true
}

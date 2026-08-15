//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import (
	"math"
	"testing"
	"time"
)

// TestBatchTrackerShare locks in the byte weighting of the overall bar. The old
// file-count arithmetic reported 50 % after one of two files even when that file
// was a tenth of the work — these cases guard against a return to it.
func TestBatchTrackerShare(t *testing.T) {
	cases := []struct {
		name    string
		tracker batchTracker
		filePct float64
		want    float64
	}{
		{
			name:    "small file finished counts small",
			tracker: batchTracker{totalBytes: 1000, doneBytes: 100},
			filePct: 0,
			want:    0.10,
		},
		{
			name:    "half of the running file",
			tracker: batchTracker{totalBytes: 1000, doneBytes: 100, curBytes: 400},
			filePct: 50,
			want:    0.30, // 100 + 200 of 1000
		},
		{
			name:    "last file completed",
			tracker: batchTracker{totalBytes: 1000, doneBytes: 600, curBytes: 400},
			filePct: 100,
			want:    1,
		},
		{
			name:    "no sizes known signals the fallback",
			tracker: batchTracker{totalBytes: 0, doneBytes: 100, curBytes: 400},
			filePct: 50,
			want:    0,
		},
		{
			name:    "file percentage above 100 is clamped",
			tracker: batchTracker{totalBytes: 1000, curBytes: 1000},
			filePct: 140,
			want:    1,
		},
		{
			name:    "negative file percentage is clamped",
			tracker: batchTracker{totalBytes: 1000, doneBytes: 250, curBytes: 750},
			filePct: -5,
			want:    0.25,
		},
		{
			name:    "bookkeeping beyond the total stays at 100 %",
			tracker: batchTracker{totalBytes: 1000, doneBytes: 1200},
			filePct: 0,
			want:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := tc.tracker
			got := tracker.share(tc.filePct)
			if math.Abs(got-tc.want) > 0.0001 {
				t.Errorf("share(%.0f) = %.4f, want %.4f", tc.filePct, got, tc.want)
			}
		})
	}
}

// TestBatchTrackerRemainingTooEarly makes sure no remaining time is invented
// from the first few seconds of a long batch.
func TestBatchTrackerRemainingTooEarly(t *testing.T) {
	tracker := batchTracker{
		start:      time.Now().Add(-10 * time.Second),
		totalBytes: 1_000_000,
		curBytes:   1_000_000,
	}
	// 1 % of the first file is far below batchETAMinShare.
	if secs, ok := tracker.remaining(1); ok {
		t.Errorf("remaining() reported %.0f s at 1 %% share, want no estimate", secs)
	}
}

// TestBatchTrackerRemainingExtrapolates checks the arithmetic itself: a quarter
// done after ten seconds means roughly thirty seconds to go.
func TestBatchTrackerRemainingExtrapolates(t *testing.T) {
	tracker := batchTracker{
		start:      time.Now().Add(-10 * time.Second),
		totalBytes: 1000,
		doneBytes:  250,
	}
	secs, ok := tracker.remaining(0)
	if !ok {
		t.Fatal("remaining() gave no estimate at 25 % share")
	}
	if secs < 28 || secs > 32 {
		t.Errorf("remaining() = %.1f s, want about 30 s", secs)
	}
}

// TestBatchTrackerRemainingNoSizes covers the fallback path: without file sizes
// the share is 0, so no remaining time may be claimed.
func TestBatchTrackerRemainingNoSizes(t *testing.T) {
	tracker := batchTracker{start: time.Now().Add(-time.Minute)}
	if _, ok := tracker.remaining(50); ok {
		t.Error("remaining() gave an estimate although no file sizes are known")
	}
}

// TestTotalInputBytesIgnoresUnreadable proves that a vanished file does not
// break the sum — it simply contributes nothing.
func TestTotalInputBytesIgnoresUnreadable(t *testing.T) {
	if got := totalInputBytes([]string{`C:\does\not\exist\nowhere.mkv`}); got != 0 {
		t.Errorf("totalInputBytes() = %d, want 0 for an unreadable file", got)
	}
	if got := totalInputBytes(nil); got != 0 {
		t.Errorf("totalInputBytes(nil) = %d, want 0", got)
	}
}

// TestIsInterruptedFragment guards the check that keeps a torn ".part.mkv" from
// being re-encoded as if it were a source file.
func TestIsInterruptedFragment(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{`C:\vids\Film.h265.part.mkv`, true},
		{`C:\vids\Film.remux.part.mkv`, true},
		{`C:\vids\Film.PART.MKV`, true},  // Windows is case-insensitive
		{`C:\vids\Film.h265.mkv`, false}, // finished output
		{`C:\vids\Film.preview.mkv`, false},
		{`C:\vids\Film.mkv`, false},
		{`C:\vids\Compartment.mkv`, false}, // ends in "part" but not ".part"
		{`C:\vids\Film.part.mp4`, false},   // we only ever write .part.mkv
		{`C:\vids\part.mkv`, false},        // no marker, just a file called part
	}

	for _, tc := range cases {
		if got := isInterruptedFragment(tc.path); got != tc.want {
			t.Errorf("isInterruptedFragment(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

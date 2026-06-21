//go:build windows && amd64

// NVENCForge — Required Notice: Copyright (c) 2026 burnersen — NVENCForge
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import "testing"

// TestCappedTargetKbps locks in the source-derived bitrate cap restored in 1.1.4
// (80% of source, floored per output resolution, ceilinged per mode). The 1.1.3
// regression — a fixed per-mode ceiling that never bit, so low/mid-bitrate sources
// grew — is exactly what these cases guard against.
func TestCappedTargetKbps(t *testing.T) {
	cases := []struct {
		name    string
		source  int64
		height  int
		ceiling int64
		want    int64
	}{
		// 1080p mode (ceiling 8000, floor 1500)
		{"1080p lean → floor", 1200, 1080, 8000, 1500},     // 0.8*1200=960 < floor
		{"1080p just-under floor", 1800, 1080, 8000, 1500}, // 0.8*1800=1440 < floor
		{"1080p mid → 80%", 4000, 1080, 8000, 3200},        // 0.8*4000 between floor/ceiling
		{"1080p high → ceiling", 12000, 1080, 8000, 8000},  // 0.8*12000=9600 > ceiling
		// 720p mode (floor 800)
		{"720p lean → floor", 900, 720, 8000, 800}, // 0.8*900=720 < floor
		{"720p mid → 80%", 3000, 720, 8000, 2400},  // 0.8*3000 between floor/ceiling
		// 4K -original mode (ceiling 22000, floor 6000)
		{"4K low → floor", 5000, 2160, 22000, 6000},      // 0.8*5000=4000 < floor
		{"4K mid → 80%", 10000, 2160, 22000, 8000},       // 0.8*10000 between floor/ceiling
		{"4K high → ceiling", 30000, 2160, 22000, 22000}, // 0.8*30000=24000 > ceiling
		// explicit -NNNN override (ceiling) must win even over the floor
		{"manual low ceiling beats floor", 5000, 2160, 1000, 1000},
	}
	for _, c := range cases {
		if got := cappedTargetKbps(c.source, c.height, c.ceiling); got != c.want {
			t.Errorf("%s: cappedTargetKbps(%d, %d, %d) = %d, want %d",
				c.name, c.source, c.height, c.ceiling, got, c.want)
		}
	}
}

// TestCapNeverGrowsWhenItBites is the core guarantee: whenever the cap undercuts
// the source (the condition under which 1.1.4 chooses to re-encode rather than
// remux), the target is strictly below the source bitrate — so the encode can only
// shrink the file, never grow it.
func TestCapNeverGrowsWhenItBites(t *testing.T) {
	type mode struct {
		height  int
		ceiling int64
	}
	modes := []mode{{1080, 8000}, {720, 8000}, {2160, 22000}}
	for _, m := range modes {
		for src := int64(300); src <= 60000; src += 100 {
			target := cappedTargetKbps(src, m.height, m.ceiling)
			if target < src {
				continue // cap bites → re-encode is bounded below source → guaranteed smaller
			}
			// target >= src → 1.1.4 remuxes instead of re-encoding (reEncodeWorthwhile
			// is false), so nothing is encoded and there is no growth risk. The only
			// reasons a target can reach the source are the resolution floor or an
			// explicit ceiling — assert that, so a future change can't silently let a
			// re-encode target climb above the source again.
			if target != bitrateFloorKbps(m.height) && target != m.ceiling {
				t.Fatalf("unexpected non-biting target=%d at src=%d height=%d", target, src, m.height)
			}
		}
	}
}

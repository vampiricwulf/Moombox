package engine

import (
	"testing"
	"time"
)

// TestHlsReloadDelay pins the ffmpeg libavformat/hls.c reload pacing that
// hlsReloadDelay ports: interval = last-segment duration while flowing
// (default_reload_interval), target/2 while stalled at the live edge, measured
// as the REMAINDER since the playlist was loaded, clamped to 0 when already
// behind (ffmpeg's `now - last_load_time >= reload_interval` reloads at once).
func TestHlsReloadDelay(t *testing.T) {
	cases := []struct {
		name           string
		lastSegDur     float64
		targetDur      float64
		hadNewSegments bool
		elapsed        time.Duration
		want           time.Duration
	}{
		// Flowing: interval = last-segment duration (ffmpeg default_reload_interval).
		{"flowing, no elapsed -> full last-seg interval", 2.0, 2.0, true, 0, 2 * time.Second},
		{"flowing, partial elapsed -> remainder", 2.0, 2.0, true, 500 * time.Millisecond, 1500 * time.Millisecond},
		{"flowing, elapsed exceeds interval -> reload immediately", 2.0, 2.0, true, 3 * time.Second, 0},
		{"flowing, fractional last-seg duration", 4.5, 6.0, true, 0, 4500 * time.Millisecond},
		// Stalled at edge: interval = target/2 (ffmpeg's post-reload halving).
		{"stalled -> half target duration", 2.0, 6.0, false, 0, 3 * time.Second},
		{"stalled, partial elapsed -> remainder of half target", 2.0, 6.0, false, 1 * time.Second, 2 * time.Second},
		{"stalled, elapsed exceeds half target -> immediate", 2.0, 6.0, false, 5 * time.Second, 0},
		// Fallbacks: last-seg<=0 falls back to targetDur (flowing); targetDur<=0 falls back to 2s.
		{"flowing, last-seg unknown -> targetDur", 0, 4.0, true, 0, 4 * time.Second},
		{"flowing, both unknown -> 2s default", 0, 0, true, 0, 2 * time.Second},
		{"stalled, targetDur unknown -> 2s default, halved to 1s", 0, 0, false, 0, 1 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hlsReloadDelay(tc.lastSegDur, tc.targetDur, tc.hadNewSegments, tc.elapsed)
			if got != tc.want {
				t.Errorf("hlsReloadDelay(%v, %v, %v, %v) = %v, want %v",
					tc.lastSegDur, tc.targetDur, tc.hadNewSegments, tc.elapsed, got, tc.want)
			}
		})
	}
}

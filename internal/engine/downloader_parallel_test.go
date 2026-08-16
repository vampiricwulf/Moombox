package engine

import (
	"testing"
	"time"
)

// TestCatchUpBatchLimitDamping ports moonarchive's post-failure throttle: a
// fresh downloader may use the full batch, a downloader that just hit a
// failure episode drops to a small batch, and the ceiling regrows with time
// so a single blip doesn't cripple catch-up for the rest of the archive.
func TestCatchUpBatchLimitDamping(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x"})

	if got := d.catchUpBatchLimit(); got != maxCatchupBatch {
		t.Errorf("fresh limit = %d, want the full %d", got, maxCatchupBatch)
	}

	// A failure damps to one full parallel wave, never below it. Collapsing
	// to a single segment is what made real catch-up run 0-2 segments wide
	// (field evidence, 2026-08-15) while 403 episodes arrived every ~15-30s.
	d.noteCatchUpFailureEpisode()
	got := d.catchUpBatchLimit()
	if got != catchUpDampedFloor {
		t.Errorf("limit immediately after a failure = %d, want the floor %d", got, catchUpDampedFloor)
	}

	// 10s later the ceiling has regrown (1 per second) but not to full.
	d.lastCatchUpFailure.Store(time.Now().Add(-10 * time.Second))
	got = d.catchUpBatchLimit()
	if want := catchUpDampedFloor + 10; got != want {
		t.Errorf("limit 10s after a failure = %d, want %d", got, want)
	}

	// A repeating failure cadence must NOT pin catch-up near the floor: with
	// episodes every 20s the ceiling still recovers most of its width, which
	// is the regression this retune fixes.
	d.lastCatchUpFailure.Store(time.Now().Add(-20 * time.Second))
	if got := d.catchUpBatchLimit(); got < 20 {
		t.Errorf("limit 20s after a failure = %d, want >= 20 (a 20s failure cadence must not pin catch-up near the floor)", got)
	}

	// Long after, the full batch is available again.
	d.lastCatchUpFailure.Store(time.Now().Add(-2 * time.Hour))
	if got := d.catchUpBatchLimit(); got != maxCatchupBatch {
		t.Errorf("limit long after a failure = %d, want the full %d", got, maxCatchupBatch)
	}
}

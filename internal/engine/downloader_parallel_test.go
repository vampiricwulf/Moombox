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

	d.noteCatchUpFailureEpisode()
	got := d.catchUpBatchLimit()
	if got != 1 {
		t.Errorf("limit immediately after a failure = %d, want 1", got)
	}

	// 30s later the ceiling should have regrown (1 per 10s) but not to full.
	d.lastCatchUpFailure.Store(time.Now().Add(-30 * time.Second))
	got = d.catchUpBatchLimit()
	if got != 4 {
		t.Errorf("limit 30s after a failure = %d, want 4", got)
	}

	// Long after, the full batch is available again.
	d.lastCatchUpFailure.Store(time.Now().Add(-2 * time.Hour))
	if got := d.catchUpBatchLimit(); got != maxCatchupBatch {
		t.Errorf("limit long after a failure = %d, want the full %d", got, maxCatchupBatch)
	}
}

package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestHandleHTTPErrorVerifiesEndedAfterGap pins the time-based verification: a
// sustained gap since the last segment (>= streamStatusCheckInterval) triggers a
// stream-status check, and an ended stream returns errStreamDone promptly rather
// than waiting out the MaxTimeout backstop.
func TestHandleHTTPErrorVerifiesEndedAfterGap(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil }, // ended
	})
	// Segment last arrived well past the verify interval; never checked status.
	d.lastSegTime.Store(time.Now().Add(-(streamStatusCheckInterval + 5*time.Second)))
	d.lastHeadProbeTime.StoreNow() // skip the network head re-probe

	sameSegRetries, lastRetrySeq, sameHeadRetryDelay, lastConfirmedHead := 0, -1, 0, -1
	err := d.handleHTTPError(context.Background(), true,
		&sameSegRetries, &lastRetrySeq, &sameHeadRetryDelay, &lastConfirmedHead, 60)
	if err != errStreamDone {
		t.Fatalf("handleHTTPError = %v, want errStreamDone (ended stream caught by the 30s status check)", err)
	}
}

// TestHandleHTTPErrorMaxTimeoutForcesFinalize pins the user-specified force
// finalize: once the no-segment gap exceeds MaxTimeout, the recording is
// finalized (errStreamDone) even when YouTube still (falsely) reports the
// stream live.
func TestHandleHTTPErrorMaxTimeoutForcesFinalize(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return false, nil }, // YouTube falsely says LIVE
	})
	d.lastSegTime.Store(time.Now().Add(-2 * time.Minute)) // gap already exceeds MaxTimeout
	d.lastHeadProbeTime.StoreNow()                        // skip the network head re-probe

	sameSegRetries, lastRetrySeq, sameHeadRetryDelay, lastConfirmedHead := 0, -1, 0, -1
	err := d.handleHTTPError(context.Background(), true,
		&sameSegRetries, &lastRetrySeq, &sameHeadRetryDelay, &lastConfirmedHead, 60)
	if err != errStreamDone {
		t.Fatalf("handleHTTPError = %v, want errStreamDone (MaxTimeout force-finalize despite YouTube 'live')", err)
	}
}

// TestHandleHTTPErrorBriefGapSkipsStatusCheck pins that a brief gap (a segment
// arrived within the verify interval) does NOT spend a status-check API call —
// only a sustained wait does.
func TestHandleHTTPErrorBriefGapSkipsStatusCheck(t *testing.T) {
	called := false
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile: filepath.Join(t.TempDir(), "v"),
		CheckStreamStatus: func(context.Context) (bool, error) {
			called = true
			return true, nil
		},
	})
	d.lastSegTime.StoreNow()       // segment just arrived — a brief hiccup
	d.lastHeadProbeTime.StoreNow() // skip the network head re-probe

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the trailing backoff sleep returns immediately

	sameSegRetries, lastRetrySeq, sameHeadRetryDelay, lastConfirmedHead := 0, -1, 0, -1
	_ = d.handleHTTPError(ctx, true,
		&sameSegRetries, &lastRetrySeq, &sameHeadRetryDelay, &lastConfirmedHead, 60)
	if called {
		t.Error("CheckStreamStatus must not be called for a brief (<30s) segment gap")
	}
}

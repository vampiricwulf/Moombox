package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newActivityDownloader(t *testing.T) (*SegmentDownloader, *DownloadActivity) {
	t.Helper()
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	got := ActivityNone
	d.OnActivity = func(a DownloadActivity) { got = a }
	return d, &got
}

func TestHandleGoneErrorEmitsFindingFirstSegment(t *testing.T) {
	d, got := newActivityDownloader(t)
	n := 1 // first-segment hunt: !hasStartedDownloading, n <= goneRetryBeforeFirstSegment
	if err := d.handleGoneError(t.Context(), 403, &n, false); err != nil {
		t.Fatalf("handleGoneError returned %v, want nil (continue)", err)
	}
	if *got != ActivityFindingFirstSegment {
		t.Errorf("activity = %v, want ActivityFindingFirstSegment", *got)
	}
}

func TestHandleGoneErrorEmitsVerifyingEnd(t *testing.T) {
	d, got := newActivityDownloader(t)
	n := goneRetryDuringDownload + 1 // sustained gones while downloading
	// IsOnline nil + CheckStreamStatus nil -> emits VerifyingEnd, then declares ended.
	if err := d.handleGoneError(t.Context(), 403, &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError returned %v, want errStreamDone", err)
	}
	if *got != ActivityVerifyingEnd {
		t.Errorf("activity = %v, want ActivityVerifyingEnd", *got)
	}
}

func TestHandleGoneErrorEmitsWaitingForSegment(t *testing.T) {
	d, got := newActivityDownloader(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancelled so the single-gone retry sleep returns immediately
	n := 1   // one gone while downloading — below the verify threshold
	if err := d.handleGoneError(ctx, 403, &n, true); err != nil {
		t.Fatalf("handleGoneError returned %v, want nil (continue)", err)
	}
	if *got != ActivityWaitingForSegment {
		t.Errorf("activity = %v, want ActivityWaitingForSegment (pre-verify wait)", *got)
	}
}

func TestHandleRateLimitErrorEmitsRateLimited(t *testing.T) {
	d, got := newActivityDownloader(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancelled so the backoff sleep returns immediately
	delay := 0
	_ = d.handleRateLimitError(ctx, &delay, 60)
	if *got != ActivityRateLimited {
		t.Errorf("activity = %v, want ActivityRateLimited", *got)
	}
}

// TestHandleDashErrorGenericEmitsRetrying pins the generic (non-HTTP: a
// transport error or an idle stall, status 0) branch of handleDashError: it
// must say so on the progress line, as the HLS loop's playlist retry does,
// instead of sleeping genericRetry behind a frozen segment counter.
func TestHandleDashErrorGenericEmitsRetrying(t *testing.T) {
	d, got := newActivityDownloader(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancelled so the fixed-delay retry sleep returns immediately
	n, segRetries, retrySeq, headDelay, head := 3, 0, 0, 0, 0
	err := d.handleDashError(ctx, 0, errors.New("read tcp: i/o timeout"),
		&n, true, &segRetries, &retrySeq, &headDelay, &head, 60)
	if err != nil {
		t.Fatalf("handleDashError returned %v, want nil (continue)", err)
	}
	if *got != ActivityRetrying {
		t.Errorf("activity = %v, want ActivityRetrying", *got)
	}
	if n != 0 {
		t.Errorf("consecutiveGoneErrors = %d, want 0 (a non-gone error resets the gone streak)", n)
	}
}

// TestHandleDashErrorGenericOfflineReconnects pins the outage parity with
// handleGoneError and handleHTTPError (TestHandleGoneErrorOfflinePausesTimeoutClock
// is the model): a transport error while the device is offline reports
// ActivityReconnecting, waits for connectivity instead of burning retries,
// and resets lastSegTime on reconnect so the outage does not count toward
// MaxTimeout. Before this the branch had no offline arm at all, so a DASH
// job through a network outage showed nothing.
func TestHandleDashErrorGenericOfflineReconnects(t *testing.T) {
	calls := 0
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile: filepath.Join(t.TempDir(), "v"),
		MaxTimeout: time.Minute,
		IsOnline:   func() bool { calls++; return calls > 1 }, // offline once, then back
	})
	d.delays = fastDelays()
	got := ActivityNone
	d.OnActivity = func(a DownloadActivity) { got = a }
	d.lastSegTime.Store(time.Now().Add(-2 * time.Minute)) // aged past MaxTimeout during the outage

	n, segRetries, retrySeq, headDelay, head := 3, 0, 0, 0, 0
	err := d.handleDashError(t.Context(), 0, errors.New("dial tcp: network is unreachable"),
		&n, true, &segRetries, &retrySeq, &headDelay, &head, 60)
	if err != nil {
		t.Fatalf("offline generic error = %v, want nil (reconnect continues)", err)
	}
	if got != ActivityReconnecting {
		t.Errorf("activity = %v, want ActivityReconnecting", got)
	}
	if d.lastSegTime.Since() > time.Second {
		t.Error("lastSegTime not reset on reconnect — MaxTimeout budget not refreshed")
	}
	if n != 0 {
		t.Errorf("consecutiveGoneErrors = %d, want 0 after reconnect", n)
	}
}

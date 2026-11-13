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

// TestHandleGoneErrorEmitsFindingFirstSegment covers both arms of the
// first-segment hunt (huntMayAdvance): the hold, which asks the same
// sequence again, and the step past a refused sequence below a known head.
func TestHandleGoneErrorEmitsFindingFirstSegment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		head    int // -1: unknown
		wantSeq int64
	}{
		{name: "head unknown: holds", head: -1, wantSeq: 0},
		{name: "below a known head: steps", head: 10, wantSeq: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, got := newActivityDownloader(t)
			if tc.head >= 0 {
				d.noteHeadSeq(tc.head)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // cancelled so the hunt's sleep returns immediately
			n := 1   // first-segment hunt: !hasStartedDownloading, n <= goneRetryBeforeFirstSegment
			if err := d.handleGoneError(ctx, 403, &n, false); err != nil {
				t.Fatalf("handleGoneError returned %v, want nil (continue)", err)
			}
			if *got != ActivityFindingFirstSegment {
				t.Errorf("activity = %v, want ActivityFindingFirstSegment", *got)
			}
			if seq := d.currentSeq.Load(); seq != tc.wantSeq {
				t.Errorf("currentSeq = %d, want %d", seq, tc.wantSeq)
			}
		})
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
// transport error or an idle stall, status 0) branch of handleDashError
// below the head: it is handleHTTPError's transient retry, which says so on
// the progress line, and it ends the gone streak. What bounds the branch is
// pinned in downloader_dash_transport_test.go.
func TestHandleDashErrorGenericEmitsRetrying(t *testing.T) {
	d, got := newActivityDownloader(t)
	d.headSeq.Store(10)
	d.currentSeq.Store(5)
	d.lastHeadProbeTime.StoreNow() // no head probe: there is no server
	d.delays = fastDelays()        // a live context: a cancelled one is not a failure to retry
	rs := &dashRetryState{consecutiveGoneErrors: 3}
	err := d.handleDashError(t.Context(), 0, errors.New("read tcp: i/o timeout"),
		rs, true, 60)
	if err != nil {
		t.Fatalf("handleDashError returned %v, want nil (continue)", err)
	}
	if *got != ActivityRetrying {
		t.Errorf("activity = %v, want ActivityRetrying", *got)
	}
	if n := rs.consecutiveGoneErrors; n != 0 {
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

	rs := &dashRetryState{consecutiveGoneErrors: 3}
	err := d.handleDashError(t.Context(), 0, errors.New("dial tcp: network is unreachable"),
		rs, true, 60)
	if err != nil {
		t.Fatalf("offline generic error = %v, want nil (reconnect continues)", err)
	}
	if got != ActivityReconnecting {
		t.Errorf("activity = %v, want ActivityReconnecting", got)
	}
	if d.lastSegTime.Since() > time.Second {
		t.Error("lastSegTime not reset on reconnect — MaxTimeout budget not refreshed")
	}
	if n := rs.consecutiveGoneErrors; n != 0 {
		t.Errorf("consecutiveGoneErrors = %d, want 0 after reconnect", n)
	}
}

// Behind the head the segment exists, so a failing fetch there is a retry,
// not the live-edge wait: both sites emitted ActivityWaitingForSegment, which
// the progress line renders as "Waiting for next segment" — a healthy live
// edge — for as long as the burst lasted (up to MaxTimeout).
//
// Mutant: emit ActivityWaitingForSegment at either site again.
func TestBehindHeadFetchFailuresEmitRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the gone handler's retry sleep returns immediately

	t.Run("sustained gone below head", func(t *testing.T) {
		d, got := newActivityDownloader(t)
		d.headSeq.Store(10)
		d.currentSeq.Store(5)
		d.lastSegTime.StoreNow()
		n := goneRetryDuringDownload + 1
		if err := d.handleGoneError(ctx, 403, &n, true); err != nil {
			t.Fatalf("handleGoneError returned %v, want nil (keep retrying the pending tail)", err)
		}
		if *got != ActivityRetrying {
			t.Errorf("activity = %v, want ActivityRetrying", *got)
		}
	})

	t.Run("transient failure below head", func(t *testing.T) {
		d, got := newActivityDownloader(t)
		d.headSeq.Store(10)
		d.currentSeq.Store(5)
		d.lastHeadProbeTime.StoreNow() // no head probe: there is no server
		d.delays = fastDelays()        // a live context: the handler leaves a cancelled one to the loop
		rs := &dashRetryState{lastRetrySeq: -1}
		if err := d.handleHTTPError(t.Context(), true, rs, 60); err != nil {
			t.Fatalf("handleHTTPError returned %v, want nil (continue)", err)
		}
		if *got != ActivityRetrying {
			t.Errorf("activity = %v, want ActivityRetrying", *got)
		}
	})
}

package engine

import (
	"context"
	"path/filepath"
	"testing"
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
	if err := d.handleGoneError(context.Background(), 403, &n, false); err != nil {
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
	if err := d.handleGoneError(context.Background(), 403, &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError returned %v, want errStreamDone", err)
	}
	if *got != ActivityVerifyingEnd {
		t.Errorf("activity = %v, want ActivityVerifyingEnd", *got)
	}
}

func TestHandleGoneErrorEmitsWaitingForSegment(t *testing.T) {
	d, got := newActivityDownloader(t)
	ctx, cancel := context.WithCancel(context.Background())
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled so the backoff sleep returns immediately
	delay := 0
	_ = d.handleRateLimitError(ctx, &delay, 60)
	if *got != ActivityRateLimited {
		t.Errorf("activity = %v, want ActivityRateLimited", *got)
	}
}

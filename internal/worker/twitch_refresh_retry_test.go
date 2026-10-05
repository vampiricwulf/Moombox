package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// fastRefreshRetries shrinks retryVariantRefresh's backoff for one test.
func fastRefreshRetries(t *testing.T) {
	t.Helper()
	old := liveRefreshRetryDelay
	liveRefreshRetryDelay = time.Millisecond
	t.Cleanup(func() { liveRefreshRetryDelay = old })
}

// A master-playlist refresh that failed once inside the download loop used to
// end a live capture in Error on the spot, and nothing continues a Twitch job
// from there — Retry wipes its staging and the monitor only recovers offline
// flaps. The refresh is now retried, so a usher blip that clears within the
// retries leaves the capture running.
//
// The media playlist 404s first (the engine's consult says live, so it reports
// the quality lost); the refresh fails twice, then hands back a variant that
// serves segments.
//
// Mutant: the in-loop refresh not retried — the job latches the first failure
// and returns it without fetching a segment.
func TestVariantRefreshFailureIsRetriedBeforeTheCaptureGivesUp(t *testing.T) {
	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_refresh_retry")
	fastRefreshRetries(t)
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	var refreshes atomic.Int32
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		if refreshes.Add(1) <= 2 {
			return nil, errUsher
		}
		return []twitch.TwitchHLSVariant{{
			URL: srv.URL + "/live.m3u8", Name: h.variant.Name, Width: h.variant.Width, Height: h.variant.Height, FPS: h.variant.FPS,
		}}, nil
	}
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "retry"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil) }()
	select {
	case <-first:
	case err := <-done:
		t.Fatalf("ExecuteTwitch returned %v after %d refreshes without fetching a segment from the recovered variant",
			err, refreshes.Load())
	case <-time.After(30 * time.Second):
		t.Fatal("no segment fetched from the recovered variant")
	}
	cancel()
	if err := <-done; errors.Is(err, errUsher) {
		t.Errorf("ExecuteTwitch = %v, want the recovered capture, not the refresh failure", err)
	}
}

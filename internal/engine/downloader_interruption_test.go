package engine

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the real runDashLoop (via Start) against the fake GVS
// harness from downloader_dash_integration_test.go — same pattern as the
// other TestDashLoop* tests in this package: no poking of handleGoneError /
// handleHTTPError directly, because that would validate the helper in
// isolation without proving the two call sites are wired correctly.
//
// Two call sites consult stallForPossibleResume, reached via two different
// HTTP status families on the fake GVS:
//   - handleGoneError's budget-expired fallthrough: 403/410 ("gone") past
//     goneRetryDuringDownload (10) consecutive failures.
//   - handleHTTPError's MaxTimeout backstop: any other >=400 status (500
//     here) once the no-segment gap exceeds MaxTimeout.
// The four tests below deliberately exercise both sites so a regression in
// either insertion is caught by at least one test.

// TestBackstopStallsWhileMayResume drives handleHTTPError's MaxTimeout
// backstop (segments beyond head permanently 500) with MayResume() latched
// true. Would catch: deleting/bypassing the `!d.streamEndVerified &&
// d.stallForPossibleResume()` insertion in handleHTTPError (or a
// stallForPossibleResume that returns false when MayResume is true) — either
// regression finalizes right at MaxTimeout instead of stalling, which the
// first select below observes as an early receive on `done` and fails via
// t.Fatalf. A MayResume that is consulted but ignored (e.g. hard-coded
// false) would show the same symptom.
func TestBackstopStallsWhileMayResume(t *testing.T) {
	t.Parallel()
	const head = 3
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusInternalServerError // routes to handleHTTPError, not handleGoneError
	})

	out := filepath.Join(t.TempDir(), "v")
	const maxTimeout = 1 * time.Second
	var mayResume atomic.Bool
	mayResume.Store(true)
	var mayResumeCalls atomic.Int64
	var sawWaitingResume atomic.Bool

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.interrupt1&itag=140",
		OutputFile: out,
		MaxTimeout: maxTimeout,
		// Still live -- never actually consulted in this short a window
		// (the 30s streamStatusCheckInterval gate never elapses), but wired
		// to prove the stall is independent of it.
		CheckStreamStatus: func(context.Context) (bool, error) { return false, nil },
	})
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return mayResume.Load()
	}
	d.OnActivity = func(a DownloadActivity) {
		if a == ActivityWaitingResume {
			sawWaitingResume.Store(true)
		}
	}

	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	// Would-fail check (a): still running well after MaxTimeout+margin
	// elapsed, with MayResume()==true throughout.
	select {
	case err := <-done:
		t.Fatalf("Start returned (err=%v) before MayResume flipped false -- backstop did not stall", err)
	case <-time.After(5 * time.Second):
		// still running, as expected -- fall through to flip MayResume.
	}

	mayResume.Store(false)

	// Would-fail check (b): flipping MayResume false must release the
	// finalize promptly, not require another full MaxTimeout wait.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil after MayResume flipped false", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return promptly after MayResume flipped false")
	}

	if !d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = false, want true (stalled then finalized)")
	}
	if mayResumeCalls.Load() == 0 {
		t.Error("MayResume was never consulted -- stallForPossibleResume not reached")
	}
	if !sawWaitingResume.Load() {
		t.Error("ActivityWaitingResume never emitted during the stall")
	}
	wantSegments(t, out, 0, head)
}

// TestBackstopCeilingExpires sets InterruptionTimeout=1s with MayResume
// permanently true (it never resolves the interruption itself). Would
// catch: an InterruptionTimeout that isn't enforced (stallForPossibleResume
// ignoring opts.InterruptionTimeout and returning true forever) -- Start
// would never return, and the select below times out and fails via
// t.Fatal instead of silently hanging, because it's a bounded channel wait
// rather than a plain blocking call to Start.
func TestBackstopCeilingExpires(t *testing.T) {
	t.Parallel()
	const head = 3
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusInternalServerError
	})

	out := filepath.Join(t.TempDir(), "v")
	const maxTimeout = 1 * time.Second
	const ceiling = 1 * time.Second

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:             srv.URL + "/videoplayback?id=itest.interrupt2&itag=140",
		OutputFile:          out,
		MaxTimeout:          maxTimeout,
		InterruptionTimeout: ceiling,
	})
	d.MayResume = func() bool { return true } // never resolves -- only the ceiling can end this

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil (ceiling-forced finalize)", err)
		}
	case <-time.After(maxTimeout + ceiling + 3*time.Second):
		t.Fatal("Start did not return once the InterruptionTimeout ceiling expired -- stall is unbounded")
	}
	elapsed := time.Since(start)

	if elapsed < maxTimeout {
		t.Errorf("finalized after %v, before MaxTimeout %v even elapsed", elapsed, maxTimeout)
	}
	if !d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = false, want true (ceiling expired mid-stall)")
	}
	wantSegments(t, out, 0, head)
}

// TestNilMayResumeByteCompat leaves MayResume nil -- the state of every
// production caller until Task 4 wires the worker side. Would catch: a
// stallForPossibleResume that treats a nil callback as "may resume" (e.g. an
// inverted nil check) -- Start would never return, caught by the bounded
// wait below instead of hanging; or a nil callback that still latches
// FinalizedDuringInterruption -- caught by the explicit false check.
func TestNilMayResumeByteCompat(t *testing.T) {
	t.Parallel()
	const head = 3
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusInternalServerError
	})

	out := filepath.Join(t.TempDir(), "v")
	const maxTimeout = 2 * time.Second
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.interrupt3&itag=140",
		OutputFile: out,
		MaxTimeout: maxTimeout,
	})
	// d.MayResume intentionally left nil.

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(maxTimeout + 5*time.Second):
		t.Fatal("Start did not return -- nil MayResume must behave exactly like pre-feature code (bounded finalize at MaxTimeout), not stall indefinitely")
	}
	elapsed := time.Since(start)

	if elapsed < maxTimeout {
		t.Errorf("finalized after %v, before MaxTimeout %v even elapsed", elapsed, maxTimeout)
	}
	if d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = true, want false (nil MayResume must never latch)")
	}
	wantSegments(t, out, 0, head)
}

// TestConfirmedEndedIgnoresMayResume drives handleGoneError's fallthrough
// (403 past head) with CheckStreamStatus confirming the stream ended AND
// MayResume permanently true. A confirmed-ended verdict must finalize
// immediately -- MayResume must never even be consulted. Would catch: the
// `!d.streamEndVerified` guard being dropped or inverted at the
// handleGoneError insertion site -- MayResume would then be consulted, the
// stall would engage, and (since MayResume never flips and
// InterruptionTimeout is unset/0, i.e. no ceiling) Start would never
// return, caught by the bounded 20s wait; the explicit mayResumeCalls
// assertion catches the same regression even if some other unbounded
// ceiling accidentally masked the hang.
func TestConfirmedEndedIgnoresMayResume(t *testing.T) {
	t.Parallel()
	const head = 2
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusForbidden // routes to handleGoneError
	})

	out := filepath.Join(t.TempDir(), "v")
	var mayResumeCalls atomic.Int64
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.interrupt4&itag=140",
		OutputFile: out,
		MaxTimeout: time.Minute, // must not be what ends this run
		CheckStreamStatus: func(context.Context) (bool, error) {
			return true, nil // confirmed ended
		},
	})
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return true // must be ignored once the end is confirmed
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not return -- a confirmed-ended verdict must bypass MayResume/the stall entirely, not wait on it")
	}

	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("finalized after %v -- slower than the ~5.5s goneRetryDuringDownload escalation (10 x singleGoneRetryDelay) should allow", elapsed)
	}
	if mayResumeCalls.Load() != 0 {
		t.Errorf("MayResume consulted %d times, want 0 -- confirmed end must bypass stallForPossibleResume entirely", mayResumeCalls.Load())
	}
	if d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = true, want false (confirmed end must bypass the stall entirely)")
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not set on clean confirmed-end finalize")
	}
	wantSegments(t, out, 0, head)
}

package engine

import (
	"context"
	"errors"
	"net/http"
	"os"
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
//   - handleGoneError's budget-expired fallthrough (site 1): 403/410
//     ("gone") past goneRetryDuringDownload (10) consecutive failures.
//   - handleHTTPError's MaxTimeout backstop (site 2): any other >=400
//     status (500 here) once the no-segment gap exceeds MaxTimeout.
//
// handleGoneError's own CheckStreamStatus consult is UNCONDITIONAL on a
// !ended result (it returns ErrQualityLost outright, with no behind-head
// exception, unlike handleHTTPError's version) -- so reaching site 1's stall
// arm without tripping that pre-existing ErrQualityLost path requires either
// a nil CheckStreamStatus or one that returns an error (the checkErr != nil
// branch defers the verdict without deciding it). Tests 2 and 5 below use
// the error-returning form.
//
// The stall's retry sleep is interruptionStallRetry (the delays field named
// for interruptionStallRetryDelay), an order of magnitude longer than the
// singleGoneRetry used elsewhere -- see its doc comment in
// downloader_dash.go.
//
// Every loop test below installs fastDelays(), which divides every loop wait by
// the same fastScale, and scales its own MaxTimeout / InterruptionTimeout
// knobs with fast() so escalation counts and orderings stay exactly
// production's -- only the wall clock shrinks. Consequently no assertion
// here may key off a wall-clock duration: the "still stalling" checks wait
// for the loop to REACH ActivityWaitingResume (awaitActivity) and the
// bounded time.After arms are safety nets a working loop never reaches, not
// measurements.

// TestBackstopStallsWhileMayResume drives handleHTTPError's MaxTimeout
// backstop (segments beyond head permanently 500) with MayResume() latched
// true. Would catch: deleting/bypassing the `!d.streamEndVerified &&
// d.stallForPossibleResume()` insertion in handleHTTPError (or a
// stallForPossibleResume that returns false when MayResume is true) — either
// regression finalizes right at MaxTimeout instead of stalling, observed as
// a never-emitted ActivityWaitingResume and then an early receive on
// `done`. A MayResume that is consulted but ignored (e.g. hard-coded
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
	maxTimeout := fast(1 * time.Second)
	var mayResume atomic.Bool
	mayResume.Store(true)
	var mayResumeCalls atomic.Int64

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.interrupt1&itag=140",
		OutputFile: out,
		MaxTimeout: maxTimeout,
		// Still live -- never actually consulted in this short a window
		// (the 30s streamStatusCheckInterval gate never elapses), but wired
		// to prove the stall is independent of it.
		CheckStreamStatus: func(context.Context) (bool, error) { return false, nil },
	})
	d.delays = fastDelays()
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return mayResume.Load()
	}
	act := activityRecorder(d)

	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	// Would-fail check (a): the backstop engaged the stall arm and is still
	// running, with MayResume()==true throughout. The stall arm is engaged
	// once the loop reports ActivityWaitingResume; waiting for the event
	// replaces the old fixed-seconds margin (which had to out-sit the
	// backstop-entry escalation and land somewhere in an
	// interruptionStallRetry cycle to mean anything).
	// Two ActivityWaitingResume emissions, not one: the second can only come
	// after the loop slept a full interruptionStallRetry and asked MayResume
	// again, so this proves the stall HELD a cycle — not merely that the arm
	// was reached an instant before a finalize.
	awaitActivity(t, act, ActivityWaitingResume, 5*time.Second)
	awaitActivity(t, act, ActivityWaitingResume, 5*time.Second)
	select {
	case err := <-done:
		t.Fatalf("Start returned (err=%v) while MayResume was still true -- the stall did not hold", err)
	default:
	}

	mayResume.Store(false)

	// Would-fail check (b): flipping MayResume false must release the
	// finalize within about one interruptionStallRetry cycle, not require
	// another full MaxTimeout wait.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil after MayResume flipped false", err)
		}
	case <-time.After(5 * time.Second): // safety net, orders of magnitude past one cycle
		t.Fatal("Start did not return promptly after MayResume flipped false")
	}

	if !d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = false, want true (stalled then finalized)")
	}
	if mayResumeCalls.Load() == 0 {
		t.Error("MayResume was never consulted -- stallForPossibleResume not reached")
	}
	// ActivityWaitingResume is asserted by the awaitActivity call above --
	// reaching this line means the stall arm emitted it before the flip.
	wantSegments(t, out, 0, head)
}

// TestBackstopCeilingExpires drives handleGoneError's fallthrough (site 1,
// 403 past head) with a scaled InterruptionTimeout ceiling (2x this test's
// own MaxTimeout, both run through fast()) and MayResume permanently true
// (it never resolves the interruption itself -- only the ceiling can end
// this run). CheckStreamStatus returns an error so the verdict stays
// deferred (see the file-level comment on why (true/false, nil) can't reach
// this site without either confirming end or tripping ErrQualityLost).
//
// This is also the regression test for the Tier-2 sidecar-preservation fix:
// site 1's confirmed-end fallthrough used to unconditionally
// streamEnded.Store(true) on this path, which made the runDashLoop defer
// call ClearResume() and destroy the resume sidecar in the exact case the
// finalizedDuringInterruption latch tells the worker to preserve it. Would
// catch:
//   - an InterruptionTimeout that isn't enforced (stallForPossibleResume
//     ignoring opts.InterruptionTimeout) -- Start would never return, and
//     the bounded select below times out and fails via t.Fatal instead of
//     hanging.
//   - the Tier-2 fix regressing -- the resume-sidecar-survives assertion
//     below fails if streamEnded gets set (and thus the sidecar cleared) on
//     a finalizedDuringInterruption exit.
func TestBackstopCeilingExpires(t *testing.T) {
	t.Parallel()
	const head = 2
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusForbidden // routes to handleGoneError
	})

	out := filepath.Join(t.TempDir(), "v")
	maxTimeout := fast(1 * time.Second)
	ceiling := fast(2 * time.Second)
	statusCheckErr := errors.New("status check unavailable")
	var mayResumeCalls atomic.Int64

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:             srv.URL + "/videoplayback?id=itest.interrupt2&itag=140",
		OutputFile:          out,
		MaxTimeout:          maxTimeout,
		InterruptionTimeout: ceiling,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return false, statusCheckErr // defers the verdict -- handleGoneError's checkErr branch
		},
	})
	d.delays = fastDelays()
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return true // never resolves -- only the ceiling can end this
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil (ceiling-forced finalize)", err)
		}
	case <-time.After(5 * time.Second): // safety net, far past the scaled ceiling
		t.Fatal("Start did not return once the InterruptionTimeout ceiling expired -- stall is unbounded")
	}
	elapsed := time.Since(start)

	if elapsed < maxTimeout {
		t.Errorf("finalized after %v, before MaxTimeout %v even elapsed", elapsed, maxTimeout)
	}
	if !d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = false, want true (ceiling expired mid-stall)")
	}
	if mayResumeCalls.Load() == 0 {
		t.Error("MayResume was never consulted -- stallForPossibleResume not reached")
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded = true, want false -- Tier 2 finalize must not clear the resume sidecar")
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Errorf("resume sidecar missing after a Tier-2 (interruption) finalize (stat err = %v) -- a later retry could not resume the broadcast", err)
	}
	wantSegments(t, out, 0, head)
}

// TestNilMayResumeByteCompat leaves MayResume nil -- the state of every
// production caller until Task 4 wires the worker side. Would catch: a
// stallForPossibleResume that treats a nil callback as "may resume" (e.g. an
// inverted nil check) -- Start would never return, caught by the bounded
// wait below instead of hanging; or a nil callback that still latches
// FinalizedDuringInterruption -- caught by the explicit false check. A nil
// callback never reaches the interruptionStallRetryDelay sleep at all (the
// nil check short-circuits before it), so this test's timing is unaffected
// by that delay and stays keyed to MaxTimeout alone.
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
	maxTimeout := fast(2 * time.Second)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.interrupt3&itag=140",
		OutputFile: out,
		MaxTimeout: maxTimeout,
	})
	d.delays = fastDelays()
	// d.MayResume intentionally left nil.

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(5 * time.Second): // safety net, far past the scaled MaxTimeout
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
// return, caught by the bounded safety-net wait; the explicit mayResumeCalls
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
	d.delays = fastDelays()
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return true // must be ignored once the end is confirmed
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(5 * time.Second): // safety net
		t.Fatal("Start did not return -- a confirmed-ended verdict must bypass MayResume/the stall entirely, not wait on it")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("finalized after %v -- slower than the goneRetryDuringDownload escalation (10 x singleGoneRetry) should allow", elapsed)
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

// TestGoneErrorStallReachedViaStatusCheckError drives handleGoneError's
// fallthrough (site 1, 403 past head) with a CheckStreamStatus that returns
// an ERROR (the checkErr != nil branch defers the verdict, leaving
// streamEndVerified false) and MayResume latched true.
//
// This closes the coverage gap the reviewer's injection found: all four
// tests above pass even with site 1's `!d.streamEndVerified &&
// d.stallForPossibleResume()` insertion disabled, because none of them
// reach site 1 with MayResume()==true AND an unconfirmed verdict at the
// same time (TestBackstopCeilingExpires reaches it but resolves via the
// ceiling, not a MayResume flip, and after this fix it shares the same
// insertion so it partially overlaps -- this test isolates the flip-driven
// resolution the way TestBackstopStallsWhileMayResume does for site 2).
// Would catch: site 1's stall insertion being deleted or bypassed, or
// stallForPossibleResume returning false when MayResume is true -- either
// regression finalizes right after the goneRetryDuringDownload escalation
// (10 x singleGoneRetry) instead of stalling, observed as a never-emitted
// ActivityWaitingResume and then an early receive on `done`.
func TestGoneErrorStallReachedViaStatusCheckError(t *testing.T) {
	t.Parallel()
	const head = 2
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusForbidden // routes to handleGoneError
	})

	out := filepath.Join(t.TempDir(), "v")
	// Shorter than the goneRetryDuringDownload escalation it takes to reach
	// the block, so the budget has already expired by then.
	maxTimeout := fast(1 * time.Second)
	var mayResume atomic.Bool
	mayResume.Store(true)
	var mayResumeCalls atomic.Int64
	statusCheckErr := errors.New("status check unavailable")

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.interrupt5&itag=140",
		OutputFile: out,
		MaxTimeout: maxTimeout,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return false, statusCheckErr
		},
	})
	d.delays = fastDelays()
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return mayResume.Load()
	}
	act := activityRecorder(d)

	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	// Would-fail check (a): the loop got past the goneRetryDuringDownload
	// escalation and is still running, with MayResume()==true throughout.
	// The stall arm is engaged once the loop reports ActivityWaitingResume;
	// waiting for the event replaces the old fixed-seconds margin.
	// Two ActivityWaitingResume emissions, not one: the second can only come
	// after the loop slept a full interruptionStallRetry and asked MayResume
	// again, so this proves the stall HELD a cycle — not merely that the arm
	// was reached an instant before a finalize.
	awaitActivity(t, act, ActivityWaitingResume, 5*time.Second)
	awaitActivity(t, act, ActivityWaitingResume, 5*time.Second)
	select {
	case err := <-done:
		t.Fatalf("Start returned (err=%v) while MayResume was still true -- the stall did not hold", err)
	default:
	}

	mayResume.Store(false)

	// Would-fail check (b): flipping MayResume false must release the
	// finalize within about one interruptionStallRetry cycle.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil after MayResume flipped false", err)
		}
	case <-time.After(5 * time.Second): // safety net, orders of magnitude past one cycle
		t.Fatal("Start did not return promptly after MayResume flipped false")
	}

	if !d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = false, want true (stalled then finalized)")
	}
	if mayResumeCalls.Load() == 0 {
		t.Error("MayResume was never consulted -- stallForPossibleResume not reached")
	}
	// ActivityWaitingResume is asserted by the awaitActivity call above --
	// reaching this line means the stall arm emitted it before the flip.
	wantSegments(t, out, 0, head)
}

// TestStallForPossibleResumeNoStallSentinel (I1 fix) calls
// stallForPossibleResume directly against InterruptionTimeout ==
// InterruptionNoStall -- no goroutines, no fake server, deterministic and
// fast. Pins the exact contract: MayResume is consulted exactly once per
// call, finalizedDuringInterruption latches iff that one consult reports
// true, and the return value is ALWAYS false (no stall is ever entered,
// regardless of evidence). The last subtest re-confirms InterruptionTimeout
// == 0 (a direct engine caller, distinct from the worker's mapping) still
// means unbounded, unaffected by the new sentinel branch.
func TestStallForPossibleResumeNoStallSentinel(t *testing.T) {
	t.Run("evidence true latches without stalling", func(t *testing.T) {
		d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x", InterruptionTimeout: InterruptionNoStall})
		var calls int
		d.MayResume = func() bool { calls++; return true }

		if d.stallForPossibleResume() {
			t.Error("stallForPossibleResume() = true, want false -- InterruptionNoStall must never stall")
		}
		if !d.FinalizedDuringInterruption() {
			t.Error("FinalizedDuringInterruption() = false, want true -- evidence must still latch")
		}
		if calls != 1 {
			t.Errorf("MayResume called %d times, want exactly 1", calls)
		}
	})

	t.Run("evidence false does not latch", func(t *testing.T) {
		d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x", InterruptionTimeout: InterruptionNoStall})
		d.MayResume = func() bool { return false }

		if d.stallForPossibleResume() {
			t.Error("stallForPossibleResume() = true, want false")
		}
		if d.FinalizedDuringInterruption() {
			t.Error("FinalizedDuringInterruption() = true, want false -- no evidence must not latch")
		}
	})

	t.Run("nil MayResume does not latch and does not panic", func(t *testing.T) {
		d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x", InterruptionTimeout: InterruptionNoStall})
		// d.MayResume intentionally left nil.

		if d.stallForPossibleResume() {
			t.Error("stallForPossibleResume() = true, want false")
		}
		if d.FinalizedDuringInterruption() {
			t.Error("FinalizedDuringInterruption() = true, want false")
		}
	})

	t.Run("repeated calls each consult independently -- no stall state accumulates", func(t *testing.T) {
		d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x", InterruptionTimeout: InterruptionNoStall})
		var calls int
		d.MayResume = func() bool { calls++; return true }

		for i := range 3 {
			if d.stallForPossibleResume() {
				t.Fatalf("call %d: stallForPossibleResume() = true, want false", i)
			}
		}
		if calls != 3 {
			t.Errorf("MayResume called %d times, want 3 (one fresh consult per call, no caching)", calls)
		}
		if !d.FinalizedDuringInterruption() {
			t.Error("FinalizedDuringInterruption() = false, want true")
		}
	})

	t.Run("0 remains unbounded -- unaffected by the new sentinel branch", func(t *testing.T) {
		// Pinned by Task 3; a direct regression guard that adding the
		// InterruptionNoStall branch above did not disturb the pre-existing
		// 0 == unbounded path for a caller that still passes 0 directly
		// (the engine's own contract, distinct from the worker's mapping
		// of its disabled-stall config value onto InterruptionNoStall).
		d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x", InterruptionTimeout: 0})
		d.MayResume = func() bool { return true }

		if !d.stallForPossibleResume() {
			t.Error("stallForPossibleResume() = false, want true -- InterruptionTimeout=0 must remain unbounded for a direct engine caller")
		}
		if d.FinalizedDuringInterruption() {
			t.Error("FinalizedDuringInterruption() = true, want false -- a still-stalling call must not latch yet")
		}
	})
}

// TestInterruptionNoStallPromptFinalize (I1 fix) drives the real runDashLoop
// (via Start) against handleGoneError's fallthrough (403 past head) with
// InterruptionTimeout == InterruptionNoStall and MayResume permanently
// true -- proving the sentinel's "no stall, no clock" contract holds at the
// full Start()-loop level, not just at a direct stallForPossibleResume
// call. Same fake-GVS shape as TestBackstopCeilingExpires (which this test
// is the direct counterpart to): reaching the budget-expired block still
// costs the same goneRetryDuringDownload escalation floor (10 x
// singleGoneRetry) regardless of this fix, but unlike
// TestBackstopCeilingExpires (which adds a further ceiling wait) and
// TestGoneErrorStallReachedViaStatusCheckError (which adds an unbounded
// stall until MayResume flips), this must finalize on the SAME iteration it
// first reaches stallForPossibleResume -- no interruptionStallRetry sleep,
// no ceiling. A regression back to treating InterruptionNoStall as an
// ordinary ceiling (or as 0/unbounded) would either add a further
// interruptionStallRetry cycle or hang outright, either of which this
// test's tight elapsed bound catches.
func TestInterruptionNoStallPromptFinalize(t *testing.T) {
	t.Parallel()
	const head = 2
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusForbidden // routes to handleGoneError
	})

	out := filepath.Join(t.TempDir(), "v")
	maxTimeout := fast(1 * time.Second)
	statusCheckErr := errors.New("status check unavailable")
	var mayResumeCalls atomic.Int64

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:             srv.URL + "/videoplayback?id=itest.interrupt6&itag=140",
		OutputFile:          out,
		MaxTimeout:          maxTimeout,
		InterruptionTimeout: InterruptionNoStall,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return false, statusCheckErr // defers the verdict -- handleGoneError's checkErr branch
		},
	})
	d.delays = fastDelays()
	d.MayResume = func() bool {
		mayResumeCalls.Add(1)
		return true // evidence holds throughout -- must still never stall
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(5 * time.Second): // safety net
		t.Fatal("Start did not return -- InterruptionNoStall must never stall, even with MayResume permanently true")
	}
	elapsed := time.Since(start)

	// The goneRetryDuringDownload escalation to reach the budget-expired
	// block is an unavoidable floor shared with every sibling test in this
	// file; this bound catches the ABSENCE of any further delay on top of
	// it -- a real stall would add at least one interruptionStallRetry
	// cycle (which roughly doubles the elapsed time at either scale) and,
	// since MayResume never flips, would then repeat forever. The exact
	// "not even one cycle" proof is the mayResumeCalls == 1 assertion
	// below, which needs no clock at all.
	if elapsed > 2*time.Second {
		t.Errorf("finalized after %v -- InterruptionNoStall must skip the stall entirely (no interruptionStallRetry cycle on top of the goneRetryDuringDownload escalation floor)", elapsed)
	}
	if !d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = false, want true (evidence held on the one consult)")
	}
	if mayResumeCalls.Load() != 1 {
		t.Errorf("MayResume consulted %d times, want exactly 1", mayResumeCalls.Load())
	}
	// Tier 2: the resume sidecar must survive exactly like a genuine
	// ceiling-expiry finalize (TestBackstopCeilingExpires) -- latched
	// evidence means preserved staging even though there was no stall.
	if d.streamEnded.Load() {
		t.Error("streamEnded = true, want false -- a finalizedDuringInterruption exit must not clear the resume sidecar")
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Errorf("resume sidecar missing after a latched-but-unstalled finalize (stat err = %v)", err)
	}
	wantSegments(t, out, 0, head)
}

// TestInterruptionNoStallNoEvidenceFinalizesNormally is
// TestInterruptionNoStallPromptFinalize's no-evidence companion: MayResume
// false (config-0 job whose downloader nonetheless qualifies for
// attachMayResume, but the resume signal never fires). Must finalize just
// as promptly, WITHOUT latching finalizedDuringInterruption -- a clean
// finish, not a preserved one.
func TestInterruptionNoStallNoEvidenceFinalizesNormally(t *testing.T) {
	t.Parallel()
	const head = 2
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusForbidden
	})

	out := filepath.Join(t.TempDir(), "v")
	maxTimeout := fast(1 * time.Second)
	statusCheckErr := errors.New("status check unavailable")

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:             srv.URL + "/videoplayback?id=itest.interrupt7&itag=140",
		OutputFile:          out,
		MaxTimeout:          maxTimeout,
		InterruptionTimeout: InterruptionNoStall,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return false, statusCheckErr
		},
	})
	d.delays = fastDelays()
	d.MayResume = func() bool { return false }

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Start(t.Context()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(5 * time.Second): // safety net
		t.Fatal("Start did not return")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("finalized after %v, want promptly (same bound as the evidence-true sibling test)", elapsed)
	}
	if d.FinalizedDuringInterruption() {
		t.Error("FinalizedDuringInterruption() = true, want false -- no evidence must not latch")
	}
	wantSegments(t, out, 0, head)
}

// TestNoteOfflineRecoveryRelatchesStallClock pins the offline rule (owner
// ruling 2026-08-21): a connectivity outage must not consume the
// interruption ceiling — recovery re-latches an ACTIVE episode's clock to
// now (mirroring the lastSegTime reset that pauses MaxTimeout) — while a
// downloader with no episode in progress stays at zero (recovery must never
// START an episode).
func TestNoteOfflineRecoveryRelatchesStallClock(t *testing.T) {
	t.Parallel()

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "unused"})

	// No episode: recovery keeps the clock zero.
	d.noteOfflineRecovery()
	if !d.interruptionStallStart.Load().IsZero() {
		t.Fatal("recovery started a stall episode on a downloader with none")
	}

	// Active episode latched 30 minutes ago: recovery re-latches to now.
	d.interruptionStallStart.Store(time.Now().Add(-30 * time.Minute))
	d.noteOfflineRecovery()
	if since := d.interruptionStallStart.Since(); since > 5*time.Second {
		t.Fatalf("active episode not re-latched: clock still %v old", since)
	}
}

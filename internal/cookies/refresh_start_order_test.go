package cookies

import (
	"context"
	"testing"
	"time"
)

// Start returns only when the first check of the process has finished, also
// when that check is not its own: a dashboard recheck that landed between the
// web bind and Start wins the single-flight, Start's pass is turned away, and
// cmd/moombox's cookie startup goroutine would otherwise go on to the profile
// seed and the refresh timer with no check finished.
//
// Mutant: delete the AwaitPass call in Start (Start returns while the recheck
// is still in flight).
func TestStartWaitsForARecheckThatWonTheFirstPass(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t.Cleanup(rs.Stop)

	passes, entered, release := blockingPass(rs)
	recheckDone := make(chan struct{})
	go func() {
		defer close(recheckDone)
		rs.CheckNow(context.Background())
	}()
	<-entered

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		rs.Start(context.Background())
	}()
	select {
	case <-startDone:
		t.Fatal("Start returned while the recheck that won the first pass was still running")
	case <-time.After(150 * time.Millisecond):
	}
	if rs.GetStatus().Checked {
		t.Fatal("Checked is set while the first pass is still running")
	}

	close(release)
	select {
	case <-startDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after the recheck finished")
	}
	<-recheckDone
	if !rs.GetStatus().Checked {
		t.Error("Start returned with the status still unchecked")
	}
	if got := passes.Load(); got != 1 {
		t.Errorf("passes = %d, want 1 (Start must wait for the pass in flight, not run a second)", got)
	}
}

// The wait is under ctx: a run that ends while the pass in flight is stuck
// releases Start, and Start then starts no ticker.
func TestStartStopsWaitingWhenTheRunEnds(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

	_, entered, release := blockingPass(rs)
	recheckDone := make(chan struct{})
	go func() {
		defer close(recheckDone)
		rs.CheckNow(context.Background())
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		rs.Start(ctx)
	}()
	cancel()
	select {
	case <-startDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Start kept waiting for the pass in flight after its context ended")
	}
	close(release)
	<-recheckDone
}

// Stop ahead of Start: cmd/moombox's shutdown can reach Stop while the cookie
// startup goroutine has not called Start yet. Nothing may start after it: no
// pass, no ticker, and the status stays as it was.
//
// Mutant: drop the stopped check at the top of Start (a pass runs).
func TestRefreshStartAfterStopStartsNothing(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	passes, _, release := blockingPass(rs)
	close(release)

	rs.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rs.Start(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Start after Stop did not return")
	}
	if got := passes.Load(); got != 0 {
		t.Errorf("passes = %d after Stop then Start, want 0", got)
	}
	rs.mu.RLock()
	cancelSet := rs.cancel != nil
	rs.mu.RUnlock()
	if cancelSet {
		t.Error("Start after Stop installed a lifetime: a ticker could follow")
	}
}

// Stop during Start's first check cuts the check's context and Start returns
// without starting the ticker.
//
// Mutant: drop the ctx.Err() check after the first pass (the "started" line
// is logged and the ticker goroutine is spawned).
func TestStopDuringTheFirstCheckStartsNoTicker(t *testing.T) {
	healthyRefreshSeams(t)
	logs := &recordingCookieLogger{}
	rs := NewRefreshService(jarWithAuth(t), 0, logs)

	_, entered, release := blockingPass(rs)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rs.Start(context.Background())
	}()
	<-entered
	rs.Stop()
	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	if logs.count("cookie refresh service started") != 0 {
		t.Error("the refresh loop started after Stop")
	}
	if !rs.GetStatus().Checked {
		t.Error("a first check cut short by Stop left the status unchecked")
	}
}

// AwaitPass with nothing in flight answers at once.
func TestAwaitPassWithNothingInFlight(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !rs.AwaitPass(ctx) {
		t.Fatal("AwaitPass reported a pass in flight on a new service")
	}
}

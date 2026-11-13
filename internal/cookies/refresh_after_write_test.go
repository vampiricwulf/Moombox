package cookies

import (
	"context"
	"testing"
	"time"
)

// A re-check that follows a cookie write and collides with a pass in flight
// must not be dropped: that pass may have read the old file, and its status
// block is the only place the new one would be noticed before the next tick.
// It runs once, after the pass ends.
func TestCheckNowAfterWriteRunsOncePassInFlightEnds(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	passes, entered, release := blockingPass(rs)

	first := make(chan bool, 1)
	go func() { first <- rs.CheckNow(context.Background()) }()
	<-entered

	after := make(chan bool, 1)
	go func() { after <- rs.CheckNowAfterWrite(context.Background()) }()

	// Nothing may run beside the pass in flight: two passes rewriting the same
	// cookies.txt is what the single-flight exists to prevent.
	select {
	case ran := <-after:
		t.Fatalf("the re-check returned %v while the first pass was still in flight", ran)
	case <-time.After(50 * time.Millisecond):
	}
	if got := passes.Load(); got != 1 {
		t.Fatalf("passes = %d while one was in flight, want 1", got)
	}

	close(release)
	if !<-first {
		t.Error("the pass in flight reported that it did not run")
	}
	var ran bool
	awaitOrFail(t, 10*time.Second, "CheckNowAfterWrite after the pass ended", func() { ran = <-after })
	if !ran {
		t.Error("the re-check was dropped: no pass ran after the one it collided with")
	}
	if got := passes.Load(); got != 2 {
		t.Errorf("passes = %d, want 2: the collided pass and exactly one re-check after it", got)
	}
}

// The wait is the caller's to end. A request that goes away, or a shutdown,
// must not leave a goroutine waiting on a pass, and must not start one after.
func TestCheckNowAfterWriteStopsWaitingWhenItsContextEnds(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	passes, entered, release := blockingPass(rs)

	first := make(chan bool, 1)
	go func() { first <- rs.CheckNow(context.Background()) }()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	after := make(chan bool, 1)
	go func() { after <- rs.CheckNowAfterWrite(ctx) }()
	cancel()

	var ran bool
	awaitOrFail(t, 10*time.Second, "CheckNowAfterWrite after its context ended", func() { ran = <-after })
	if ran {
		t.Error("a re-check whose context ended during the wait reported that it ran a pass")
	}

	close(release)
	<-first
	if got := passes.Load(); got != 1 {
		t.Errorf("passes = %d, want 1: a cancelled re-check must not run a pass later", got)
	}
}

// With nothing in flight it is CheckNow: one pass, no wait.
func TestCheckNowAfterWriteWithNothingInFlightRunsOnePass(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	passes, _, release := blockingPass(rs)
	close(release)

	if !rs.CheckNowAfterWrite(context.Background()) {
		t.Error("the re-check reported no pass with nothing in flight")
	}
	if got := passes.Load(); got != 1 {
		t.Errorf("passes = %d, want 1", got)
	}
}

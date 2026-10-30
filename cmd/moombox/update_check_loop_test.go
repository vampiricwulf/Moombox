package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Turning auto_check_updates on at runtime used to check nothing until the
// next daily tick. The loop now re-reads the toggle every poll and checks at
// once on a false→true flip — and only on the flip, so a toggle left on does
// not turn the poll into a check a minute.
//
// Each step waits for the loop's own reads of the toggle rather than
// sleeping for them: a poller descheduled across a fixed 30 ms "off" window
// never saw the toggle go off, so it saw no flip and the test waited out its
// 5 s for a check that could not come.
//
// Mutants: the poll arm dropped — no check after enabling; checking on every
// poll while enabled — the count keeps climbing.
func TestUpdateCheckRunsWhenTheToggleIsTurnedOn(t *testing.T) {
	var on atomic.Bool
	var checks, sawOff, sawOn atomic.Int32
	enabled := func() bool { // counts what each read of the toggle saw
		v := on.Load()
		if v {
			sawOn.Add(1)
		} else {
			sawOff.Add(1)
		}
		return v
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUpdateCheckLoop(ctx, enabled, func() { checks.Add(1) },
			updateCheckTiming{initialDelay: time.Millisecond, period: time.Hour, poll: 5 * time.Millisecond})
	}()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("after 5 s: %s (%d checks; the toggle read off %d times, on %d)", what, checks.Load(), sawOff.Load(), sawOn.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}
	// waitSaw waits until the loop has read the toggle as the counter says n
	// more times than when it was called.
	waitSaw := func(what string, counter *atomic.Int32, n int32) {
		t.Helper()
		from := counter.Load()
		waitFor(fmt.Sprintf("%d more reads of the toggle %s", n, what), func() bool { return counter.Load() >= from+n })
	}

	waitSaw("off", &sawOff, 3)
	if n := checks.Load(); n != 0 {
		t.Fatalf("%d checks while the toggle was off", n)
	}
	on.Store(true)
	waitFor("1 check", func() bool { return checks.Load() >= 1 })
	waitSaw("on", &sawOn, 10) // ten polls with the toggle still on
	if n := checks.Load(); n != 1 {
		t.Errorf("%d checks after one flip, want 1 — a toggle left on must not check every poll", n)
	}
	on.Store(false)
	waitSaw("off", &sawOff, 1) // a poll has seen it off
	on.Store(true)
	waitFor("2 checks", func() bool { return checks.Load() >= 2 })
}

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Turning auto_check_updates on at runtime used to check nothing until the
// next daily tick. The loop now re-reads the toggle every poll and checks at
// once on a false→true flip — and only on the flip, so a toggle left on does
// not turn the poll into a check a minute.
//
// Mutants: the poll arm dropped — no check after enabling; checking on every
// poll while enabled — the count keeps climbing.
func TestUpdateCheckRunsWhenTheToggleIsTurnedOn(t *testing.T) {
	var on atomic.Bool
	var checks atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUpdateCheckLoop(ctx, on.Load, func() { checks.Add(1) },
			updateCheckTiming{initialDelay: time.Millisecond, period: time.Hour, poll: 5 * time.Millisecond})
	}()
	t.Cleanup(func() { cancel(); <-done })

	waitChecks := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for checks.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("%d checks, want %d", checks.Load(), want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	time.Sleep(30 * time.Millisecond)
	if n := checks.Load(); n != 0 {
		t.Fatalf("%d checks while the toggle was off", n)
	}
	on.Store(true)
	waitChecks(1)
	time.Sleep(50 * time.Millisecond) // ten polls with the toggle still on
	if n := checks.Load(); n != 1 {
		t.Errorf("%d checks after one flip, want 1 — a toggle left on must not check every poll", n)
	}
	on.Store(false)
	time.Sleep(30 * time.Millisecond)
	on.Store(true)
	waitChecks(2)
}

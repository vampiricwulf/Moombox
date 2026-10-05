package sidecar

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// A child that dies on its first mint a few seconds after each restart is a
// flapping child, and the ladder climbs for it. The reset rule compared its
// uptime with the rung it came back on — 5 s on the first — so a death 6 s
// in read as a healthy child's one-off crash: Node was respawned every ~11 s
// for the rest of the run, below the sidecar_down alert's debounce. A child
// now has to outlive the ladder's ceiling to reset it; one that does still
// gets the quick first rung for its next, unrelated death.
//
// Mutant: nextRung comparing against the rung's own delay again — the eight
// 6-second deaths never leave the 5 s rung.
func TestTheLadderClimbsForAChildThatDiesSecondsAfterEachRestart(t *testing.T) {
	resetHealth(t)
	var delays []time.Duration
	var clockNanos atomic.Int64
	restarted := make(chan struct{}, 16)

	sup := NewSupervisor(SupervisorConfig{
		Logger:  silentLogger{},
		Restart: func(context.Context) error { return nil },
	})
	sup.sleep = func(_ context.Context, d time.Duration) {
		delays = append(delays, d)
		clockNanos.Add(int64(d))
	}
	sup.now = func() time.Time { return time.Unix(0, clockNanos.Load()) }
	sup.SetOnUp(func() { restarted <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	die := func(after time.Duration) {
		t.Helper()
		sup.Notify("stdout EOF")
		select {
		case <-restarted:
		case <-time.After(5 * time.Second):
			t.Fatal("the outage never completed a restart")
		}
		clockNanos.Add(int64(after))
	}
	// die(d): an outage, then the restarted child lives d before the next.
	for range 5 {
		die(6 * time.Second)
	}
	die(10 * time.Minute) // a child that outlives the ceiling…
	die(time.Second)      // …so this outage starts over on the 5 s rung
	die(time.Second)
	cancel()
	<-done

	want := append(slices.Clone(DefaultSupervisorBackoff), 5*time.Minute, 5*time.Minute, 5*time.Second, 15*time.Second)
	if !slices.Equal(delays, want) {
		t.Errorf("delays %v, want %v", delays, want)
	}
}

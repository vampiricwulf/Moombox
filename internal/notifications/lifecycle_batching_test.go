package notifications

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestEditModeTargetNeverBatches: the ruling excludes edit-mode targets from
// the 5 s found/added/auth window. Without this an edit-mode target's first
// two lifecycle events would arrive as one two-embed message with no id to
// edit afterwards.
func TestEditModeTargetNeverBatches(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		wantImmediate bool
	}{
		{ModeSeparate, false},
		{"", false},
		{ModeEdit, true},
	} {
		var emitted []Message
		b := newBatcher(batchWindow, &fakeBatchClock{}, func(m Message) { emitted = append(emitted, m) }, testLogger{})
		b.setMode(normalizeTargetMode(tc.mode))
		b.Add(Embed{Opts: SendOptions{Event: "found", JobID: "yt_1"}}, "", nil)
		// An edit-mode target emits AT ONCE; a separate-mode one holds the
		// embed until its window closes.
		if got := len(emitted) == 1; got != tc.wantImmediate {
			t.Errorf("mode %q: emitted immediately = %v, want %v", tc.mode, got, tc.wantImmediate)
		}
	}
}

// TestNewQueueIsBornInItsTargetsMode: a target ADDED in edit mode by a Reload
// must never coalesce, not even its first embed.
//
// MUTANT: leave the batcher's mode unset in newTargetQueue and let
// applyTargets' setMode bind it. The queue is published — reachable by Send
// and already draining — the moment targetsMu is released, and setMode can
// only run AFTER that, so a `found` arriving in between opens a 5 s window on
// a target that must not have one. The mode is known at construction, so the
// queue is born in it.
func TestNewQueueIsBornInItsTargetsMode(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M"))
	var shuttingDown atomic.Bool
	clock := &fakeBatchClock{}
	// newTargetQueue, NOT applyTargets: this is the state the queue is
	// published in, before any setMode could reach it.
	q := newTargetQueue(editTarget(f, nil, ModeEdit), testLogger{}, &shuttingDown, clock)
	t.Cleanup(q.stopDiscard)

	q.batch.Add(Embed{Opts: SendOptions{Event: "found", JobID: "yt_1"}}, "", nil)
	if n := q.pending(); n != 1 {
		t.Errorf("queued items right after publication = %d, want 1 — the embed was coalesced", n)
	}
	clock.mu.Lock()
	armed := len(clock.armed)
	clock.mu.Unlock()
	if armed != 0 {
		t.Errorf("%d batch windows armed on a fresh edit-mode queue, want 0", armed)
	}
}

// liveWindows counts the batch timers this clock still has waiting.
func liveWindows(c *fakeBatchClock) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.armed {
		if !tm.spent {
			n++
		}
	}
	return n
}

// TestSetModeSwapsUnderOneHold: the flush and the swap are ONE critical
// section, so an Add that lands while a mode flip is in flight can never open
// a window under the mode the flip is leaving.
//
// MUTANT: the check-flush-swap shape — `b.Flush()` outside b.mu, then re-take
// it to write b.mode. An Add arriving in that gap joins a window the flush has
// just emptied and arms its timer, so a `found` on a target that is now in
// edit mode waits out the full 5 s and can be overtaken by a later event that
// emitted immediately — which appends that job's History out of order.
//
// "gap" reproduces exactly that interleaving with no scheduler luck: the emit
// the flush performs re-enters Add, which under the mutant still reads the
// stale mode. "race" is the same shape from another goroutine, for -race.
func TestSetModeSwapsUnderOneHold(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		clock := &fakeBatchClock{}
		var (
			b        *batcher
			emitted  []Message
			reentred bool
		)
		b = newBatcher(batchWindow, clock, func(m Message) {
			emitted = append(emitted, m)
			if reentred {
				return
			}
			reentred = true
			// emit runs outside b.mu in both shapes, so this Add IS the one
			// that lands "in the gap" — under the one-hold swap the mode it
			// reads is already edit and the embed goes out at once.
			b.Add(Embed{Opts: SendOptions{Event: "found", JobID: "yt_2"}}, "", nil)
		}, testLogger{})

		b.Add(Embed{Opts: SendOptions{Event: "found", JobID: "yt_1"}}, "", nil)
		b.setMode(ModeEdit)

		b.mu.Lock()
		pending, mode := len(b.pending), b.mode
		b.mu.Unlock()
		if mode != ModeEdit {
			t.Fatalf("mode after the flip = %q, want %q", mode, ModeEdit)
		}
		if pending != 0 {
			t.Errorf("%d embeds pending on an edit-mode batcher — an Add in the gap opened a window", pending)
		}
		if n := liveWindows(clock); n != 0 {
			t.Errorf("%d batch windows still armed after the flip, want 0", n)
		}
		if len(emitted) != 2 {
			t.Errorf("emitted %d messages, want both embeds out", len(emitted))
		}
	})

	t.Run("race", func(t *testing.T) {
		clock := &fakeBatchClock{}
		var mu sync.Mutex
		emitted := 0
		b := newBatcher(batchWindow, clock, func(Message) {
			mu.Lock()
			defer mu.Unlock()
			emitted++
		}, testLogger{})

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic in the racing Add: %v", r)
				}
			}()
			b.Add(Embed{Opts: SendOptions{Event: "found", JobID: "yt_1"}}, "", nil)
		}()
		b.setMode(ModeEdit)
		wg.Wait()

		// Whichever order they landed in the embed is OUT — either the Add saw
		// edit mode and emitted at once, or the flush took the window it had
		// just joined. An armed window on an edit-mode batcher is the failure.
		b.mu.Lock()
		pending := len(b.pending)
		b.mu.Unlock()
		if pending != 0 || liveWindows(clock) != 0 {
			t.Errorf("pending=%d armed=%d after the flip, want 0/0", pending, liveWindows(clock))
		}
		mu.Lock()
		got := emitted
		mu.Unlock()
		if got != 1 {
			t.Errorf("emitted %d messages, want the one embed delivered exactly once", got)
		}
	})
}

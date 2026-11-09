package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/tui"
)

// TestBackfillMailboxNeverLosesAChannelsLastState (R-E2, Arc E fix round): the
// TUI's "BF: retry" is sticky — an "error" sets it and only a later state of
// that channel clears it — so the per-channel stream the TUI reads must not
// drop a channel's last state, as the drop-on-full send did when the TUI fell
// 16 messages behind. The mailbox coalesces per channel (a page tick
// superseded by a later state is dropped, its last state never is) and
// forwards in the order channels first became pending.
//
// Mutant: put sending straight to the channel with drop-on-full — with no
// reader for 50 messages over 20 channels, most channels' last state is lost.
func TestBackfillMailboxNeverLosesAChannelsLastState(t *testing.T) {
	out := make(chan tui.BackfillStatusMsg, 4)
	mb := newBackfillMailbox()
	for i := range 50 {
		ch := fmt.Sprintf("UC%02d", i%20)
		state := tui.BackfillScanning
		if i >= 30 {
			state = tui.BackfillError
			if i%2 == 0 {
				state = tui.BackfillDone
			}
		}
		mb.put(tui.BackfillStatusMsg{Channel: ch, Pages: i, State: state})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mb.run(ctx, out)

	last := map[string]tui.BackfillStatusMsg{}
	deadline := time.After(5 * time.Second)
	for got := 0; got < 20; got++ { // all 50 were put before run: one per channel
		select {
		case m := <-out:
			last[m.Channel] = m
		case <-deadline:
			t.Fatalf("got %d channels, want 20", len(last))
		}
	}
	for i := 30; i < 50; i++ {
		ch := fmt.Sprintf("UC%02d", i%20)
		want := tui.BackfillError
		if i%2 == 0 {
			want = tui.BackfillDone
		}
		if m := last[ch]; m.State != want || m.Pages != i {
			t.Errorf("%s last = %s page %d, want %s page %d", ch, m.State, m.Pages, want, i)
		}
	}

	// A later state of a channel already forwarded is forwarded too.
	mb.put(tui.BackfillStatusMsg{Channel: "UC00", State: tui.BackfillIdle})
	select {
	case m := <-out:
		if m.Channel != "UC00" || m.State != tui.BackfillIdle {
			t.Errorf("after the drain got %+v, want UC00 idle", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a put after the drain was not forwarded")
	}
}

// TestBackfillMailboxStopsWithItsContext: the forwarder blocked on a TUI that
// never reads (the headless run never starts it; a TUI that quit stops
// reading) returns when the run's context ends.
func TestBackfillMailboxStopsWithItsContext(t *testing.T) {
	mb := newBackfillMailbox()
	mb.put(tui.BackfillStatusMsg{Channel: "UC1", State: tui.BackfillError})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); mb.run(ctx, make(chan tui.BackfillStatusMsg)) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return with its context")
	}
}

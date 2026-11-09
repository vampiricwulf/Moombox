package main

import (
	"context"
	"sync"

	"github.com/vampiricwulf/Moombox/internal/tui"
)

// backfillMailbox carries the backfill OnProgress stream to the TUI without
// losing any channel's last state (R-E2, Arc E fix round). The TUI's
// "BF: retry" is sticky — an "error" sets it, only a later state of the
// same channel clears it — so a drop-on-full send, which was fine while every
// message was a page tick the next one replaced, could hide a failure or
// leave the note up for good once the dashboard had cleared it.
//
// put never blocks and never drops: it keeps the latest message per channel
// (a page tick a later state supersedes is coalesced away; the TUI's reducer
// only needs each channel's last state, and its slot check is by owner, so
// the order across channels does not matter). run forwards them in the order
// channels first became pending, blocking on the TUI. With no TUI reading
// (headless runs never start run) the pending set is bounded by the number
// of channels.
type backfillMailbox struct {
	mu      sync.Mutex
	pending map[string]tui.BackfillStatusMsg
	order   []string
	wake    chan struct{}
}

func newBackfillMailbox() *backfillMailbox {
	return &backfillMailbox{
		pending: make(map[string]tui.BackfillStatusMsg),
		wake:    make(chan struct{}, 1),
	}
}

// put records msg as its channel's latest state.
func (b *backfillMailbox) put(msg tui.BackfillStatusMsg) {
	b.mu.Lock()
	if _, ok := b.pending[msg.Channel]; !ok {
		b.order = append(b.order, msg.Channel)
	}
	b.pending[msg.Channel] = msg
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// take removes and returns the oldest pending channel's latest state.
func (b *backfillMailbox) take() (tui.BackfillStatusMsg, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.order) == 0 {
		return tui.BackfillStatusMsg{}, false
	}
	ch := b.order[0]
	b.order = b.order[1:]
	msg := b.pending[ch]
	delete(b.pending, ch)
	return msg, true
}

// run forwards pending states to out until ctx ends. The caller's goroutine
// carries the panic recovery.
func (b *backfillMailbox) run(ctx context.Context, out chan<- tui.BackfillStatusMsg) {
	for {
		msg, ok := b.take()
		if !ok {
			select {
			case <-b.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- msg:
		case <-ctx.Done():
			return
		}
	}
}

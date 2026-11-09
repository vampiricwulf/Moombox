package main

import (
	"sync"

	"github.com/vampiricwulf/Moombox/internal/tui"
)

// tuiScheduleForwarder carries one monitor's next-check value to the TUI:
// the monitor's OnSchedule calls send, and the startup seed — the value the
// monitor already held before the wrapper was installed — goes through seed.
// Both decode the ms through tui.MonitorNextCheckTime, so 0 (no channels)
// blanks the timer and -1 shows "checking".
//
// The two are serialised, and the seed only sends while no schedule has: a
// seed read just before a check finished would otherwise land after that
// check's countdown and leave a stale "checking" up for a whole interval.
// A schedule that arrives after the seed supersedes it, as the last write.
type tuiScheduleForwarder struct {
	mu    sync.Mutex
	fired bool
	ch    chan<- tui.CheckTimersMsg
	kind  tui.MonitorKind
}

func newTUIScheduleForwarder(ch chan<- tui.CheckTimersMsg, kind tui.MonitorKind) *tuiScheduleForwarder {
	return &tuiScheduleForwarder{ch: ch, kind: kind}
}

// send forwards a value the monitor scheduled. The monitors call OnSchedule
// outside their own lock, so holding mu here cannot invert with seed's read.
func (f *tuiScheduleForwarder) send(nextCheckAt int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fired = true
	f.push(nextCheckAt)
}

// seed forwards the monitor's current value, read under mu, unless a
// schedule has already been forwarded.
func (f *tuiScheduleForwarder) seed(get func() int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired {
		return
	}
	f.push(get())
}

func (f *tuiScheduleForwarder) push(nextCheckAt int64) {
	select {
	case f.ch <- tui.CheckTimersMsg{Monitor: f.kind, Next: tui.MonitorNextCheckTime(nextCheckAt)}:
	default:
	}
}

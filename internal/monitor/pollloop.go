package monitor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// pollLoop is one monitor's poll-cycle lifecycle — Start/Stop, CheckNow, the
// timer chain and the next-check countdown — shared by FeedMonitor,
// DecapiMonitor and TwitchMonitor, which used to carry three hand-kept
// copies of it. Each monitor keeps its own interval, doCheck, logger and
// panic recovery and hands them over in a pollSpec.
//
// The countdown is int64 epoch ms, unchanged on the wire (OnSchedule,
// GetNextCheckAt, the nextFeedCheck/nextDecapiCheck/nextTwitchCheck WS
// keys): a time, -1 while a cycle runs (tui.MonitorCheckingSentinelMs), 0
// when there is nothing to poll or the monitor is stopped.
//
// Its own mutex guards only these fields; the monitors' mutexes guard the
// rest of their state. Nothing here calls into a monitor while holding mu,
// and OnSchedule is always published after mu is released.
type pollLoop struct {
	mu       sync.Mutex
	checking bool
	// pendingKick latches a CheckNow that landed while a cycle was in
	// flight — previously silently dropped, leaving a just-added channel
	// unpolled for a full interval. Consumed in runCycle's defer.
	pendingKick bool
	// warnedSlow rate-limits the oversubscribed warning; atomic because
	// scheduleNext touches it outside mu.
	warnedSlow  atomic.Bool
	timer       *time.Timer
	ctx         context.Context
	cancel      context.CancelFunc
	nextCheckAt int64 // epoch ms; -1 = check in progress, 0 = no channels / stopped
}

// pollSpec is what a monitor supplies to its pollLoop. It is built per call
// (pollSpec methods on each monitor), so it always reads the monitor's
// current OnSchedule.
type pollSpec struct {
	// name prefixes the lifecycle logs: "feed", "decapi", "twitch".
	name   string
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	// cycle is the monitor's runCycle, which owns the goroutine's panic
	// recovery. The timer and a latched kick re-enter through it.
	cycle func(ctx context.Context)
	// next is the interval to the next cycle, or ok=false when there is
	// nothing to poll (no channels): the countdown then reads 0.
	next func() (interval time.Duration, ok bool)
	// slowWarn is logged once when a cycle ran over twice its interval.
	slowWarn string
	// onSchedule publishes a countdown value; nil-safe on the monitor side.
	onSchedule func(nextCheckAt int64)
}

func (s pollSpec) publish(v int64) {
	if s.onSchedule != nil {
		s.onSchedule(v)
	}
}

// start arms the loop on a child of ctx and runs the first cycle at once;
// a second start while running is a no-op.
func (pl *pollLoop) start(ctx context.Context, s pollSpec) {
	pl.mu.Lock()
	if pl.cancel != nil {
		pl.mu.Unlock()
		return // Already running
	}
	ctx, cancel := context.WithCancel(ctx)
	pl.ctx = ctx
	pl.cancel = cancel
	pl.mu.Unlock()

	s.logger.Info(s.name + " monitor started")

	// Immediate first check on startup (runCycle schedules next in its defer)
	go s.cycle(ctx)
}

// stop retires the running chain: its context is cancelled, the pending
// timer stopped, and the countdown reads 0.
func (pl *pollLoop) stop(s pollSpec) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	if pl.cancel != nil {
		pl.cancel()
		pl.ctx = nil
		pl.cancel = nil
	}
	if pl.timer != nil {
		pl.timer.Stop()
		pl.timer = nil
	}
	pl.nextCheckAt = 0
	s.logger.Info(s.name + " monitor stopped")
}

// next is the countdown value (epoch ms, -1 checking, 0 none).
func (pl *pollLoop) next() int64 {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.nextCheckAt
}

// checkNow runs a cycle at once if the loop is running (a running cycle
// latches it as pendingKick instead).
func (pl *pollLoop) checkNow(s pollSpec) {
	pl.mu.Lock()
	if pl.cancel == nil {
		pl.mu.Unlock()
		return // Not running
	}
	ctx := pl.ctx
	pl.mu.Unlock()
	go s.cycle(ctx)
}

// scheduleNext arms the next cycle. cycleStart anchors fixed-RATE
// scheduling: the delay is the interval minus the elapsed cycle time, so the
// interval is a true period (it used to be a GAP after each cycle). A
// zero-value cycleStart behaves as a plain interval.
func (pl *pollLoop) scheduleNext(ctx context.Context, cycleStart time.Time, s pollSpec) {
	// Same rule as runCycle's guard, for the path that arms the timer: a
	// cancelled context means this chain was retired by Stop(), and a retired
	// chain must arm nothing and publish nothing. Checked FIRST, above the
	// channel-count read, because the "no channels" arm below writes
	// nextCheckAt and publishes OnSchedule too — the two things this guard
	// exists to prevent — so a guard placed after it covers only half the
	// function. Deliberately WITHOUT touching nextCheckAt: the countdown
	// belongs to whichever chain is live now, and a dead chain zeroing it
	// would blank the UI's next-check time for no reason. A twin of this check
	// sits inside the locked section below and covers what this one cannot —
	// a Stop()+Start() landing after it; both are wanted.
	if ctx.Err() != nil {
		return
	}

	interval, ok := s.next()
	if !ok {
		pl.mu.Lock()
		pl.nextCheckAt = 0
		pl.mu.Unlock()
		s.publish(0)
		return
	}

	delay := interval
	if !cycleStart.IsZero() {
		elapsed := time.Since(cycleStart)
		delay = interval - elapsed
		if delay < time.Second {
			// Keep a 1s idle floor — the per-request stagger already bounds
			// instantaneous rate. Warn only when the cycle ran WELL past the
			// interval (>2×): feed cycles run inline probes, and DECAPI's and
			// Twitch's stagger floor structurally exceeds the interval on a
			// full channel list, so neither reads as a degradation. Once,
			// via the atomic guard.
			if elapsed >= 2*interval && pl.warnedSlow.CompareAndSwap(false, true) {
				s.logger.Warn(s.slowWarn,
					"cycle", elapsed.Round(time.Second), "interval", interval.Round(time.Second))
			}
			delay = time.Second
		}
	}

	pl.mu.Lock()
	// Unreachable since the leading ctx.Err() guard — Stop() cancels the ctx
	// and nils cancel together, and Stop() itself writes nextCheckAt = 0 — so
	// this no longer clears the -1 sentinel for a cycle racing Stop(). Kept as
	// defence for a future cancel-without-cancel path: a monitor that nils
	// cancel without cancelling its context would otherwise arm a timer and
	// publish a countdown for a chain nothing owns.
	if pl.cancel == nil {
		pl.nextCheckAt = 0
		pl.mu.Unlock()
		return
	}
	// Kept beside the leading guard, not redundant with it: this closes the
	// window between that check and this lock, in which a Stop()+Start() would
	// install a NEW non-nil cancel — so `cancel == nil` above passes and this
	// dead chain would write the LIVE chain's nextCheckAt and publish its
	// OnSchedule. Re-reading ctx.Err() costs nothing; do not "simplify" it away.
	if ctx.Err() != nil {
		pl.mu.Unlock()
		return
	}
	pl.nextCheckAt = time.Now().Add(delay).UnixMilli()
	if pl.timer != nil {
		pl.timer.Stop()
	}
	cycle := s.cycle
	pl.timer = time.AfterFunc(delay, func() {
		cycle(ctx)
	})
	next := pl.nextCheckAt
	pl.mu.Unlock()

	s.publish(next)

	s.logger.Debug(s.name+" check scheduled", "in", delay.Round(time.Second))
}

// runCycle runs one cycle (check) and schedules the next. The caller (each
// monitor's runCycle) owns the panic recovery.
func (pl *pollLoop) runCycle(ctx context.Context, s pollSpec, check func(ctx context.Context)) {
	// A cancelled context means this cycle belongs to a STOPPED chain. Stop()
	// cancels the context but leaves the AfterFunc armed, and a later Start()
	// installs a new cancel — so the dead chain's cycle used to pass every
	// guard, run a full doCheck, and then re-arm the SHARED timer field,
	// cancelling the live chain's pending cycle every interval. Returning here,
	// before the `checking` latch and before the scheduleNext defer is
	// installed, is what stops that. Latent today (Stop runs only at shutdown).
	if ctx.Err() != nil {
		return
	}

	cycleStart := time.Now()
	pl.mu.Lock()
	if pl.checking {
		// A kick (channel add, reconnect, operator force-check) landed
		// mid-cycle: latch it so the defer re-runs immediately instead of
		// silently dropping it (the new channel would otherwise wait a full
		// interval).
		pl.pendingKick = true
		pl.mu.Unlock()
		return
	}
	pl.checking = true
	// -1 sentinel = "checking now" for the UI countdowns (0 keeps its
	// existing meaning of "no channels"). Restored by scheduleNext.
	pl.nextCheckAt = -1
	pl.mu.Unlock()
	s.publish(-1)

	defer func() {
		pl.mu.Lock()
		pl.checking = false
		rerun := pl.pendingKick
		pl.pendingKick = false
		pl.mu.Unlock()
		if rerun {
			// Re-enter via goroutine so the stop-check and offline gate
			// run naturally; that cycle does its own scheduleNext.
			go s.cycle(ctx)
			return
		}
		pl.scheduleNext(ctx, cycleStart, s)
	}()

	check(ctx)
}

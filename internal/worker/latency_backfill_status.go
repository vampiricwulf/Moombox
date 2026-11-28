package worker

import (
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// What the latency-mode backfill is doing, for the operator (owner ruling
// RV-20, "How do I know if it ran?": Info log lines plus one status line in
// Settings). The status is derived, never persisted (CLAUDE.md State
// machines rule 5): the walk's last phase, held in memory, the switch read
// from the live config, and two counts from the database
// (database.LatencyBackfillCounts), read on request and when the phase
// changes, never on a timer. LatencyBackfillLine words it; the dashboard's
// latencyBackfillLine (web/public/modules/utils.js) is its twin, compared
// over web/tests/fixtures/latency-backfill-line-parity.json.

// LatencyBackfillState is the walk's state, a closed vocabulary. Its bytes
// are the wire's (the latencyBackfill object of initial_state, the
// latency_backfill frame and GET /api/latency-backfill).
type LatencyBackfillState string

const (
	// LatencyBackfillOff: monitors.latency_backfill is false.
	LatencyBackfillOff LatencyBackfillState = "off"
	// LatencyBackfillWaiting: on, and its first step has not run yet
	// (latencyBackfillStartDelay after start).
	LatencyBackfillWaiting LatencyBackfillState = "waiting"
	// LatencyBackfillRunning: asking about recordings, one a minute.
	LatencyBackfillRunning LatencyBackfillState = "running"
	// LatencyBackfillPaused: on, with rows to ask about, held by
	// LatencyBackfillStatus.Pause.
	LatencyBackfillPaused LatencyBackfillState = "paused"
	// LatencyBackfillCaughtUp: no row left to ask about in this process.
	LatencyBackfillCaughtUp LatencyBackfillState = "caught_up"
)

// LatencyBackfillPause is why a paused walk is paused, a closed vocabulary,
// in the order the step checks them.
type LatencyBackfillPause string

const (
	// LatencyPauseOffline: the device is offline.
	LatencyPauseOffline LatencyBackfillPause = "offline"
	// LatencyPauseRateLimit: a YouTube rate-limit hold stands, the walk's own
	// or the backlog scheduler's; LatencyBackfillStatus.Until is its end.
	LatencyPauseRateLimit LatencyBackfillPause = "rate_limit"
	// LatencyPauseCaptureLive: a capture is starting (a row in Live).
	LatencyPauseCaptureLive LatencyBackfillPause = "capture_live"
)

// LatencyBackfillStatus is the walk's derived status, as both UIs read it.
type LatencyBackfillStatus struct {
	State LatencyBackfillState `json:"state"`
	// Pause is set only while State is LatencyBackfillPaused.
	Pause LatencyBackfillPause `json:"pause,omitempty"`
	// Until is when a rate-limit pause ends; zero otherwise.
	Until time.Time `json:"until,omitzero"`
	// Done is how many of the Total rows the walk considers have a latency
	// mode (database.LatencyBackfillCounts); both 0 when the read failed.
	Done  int `json:"done"`
	Total int `json:"total"`
}

// latencyPhase is the part of the status the walk owns.
type latencyPhase struct {
	state LatencyBackfillState
	pause LatencyBackfillPause
	until time.Time
}

func (p latencyPhase) same(q latencyPhase) bool {
	return p.state == q.state && p.pause == q.pause && p.until.Equal(q.until)
}

// snapshot is the walk's phase, for another goroutine.
func (b *latencyBackfill) snapshot() latencyPhase {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.phase
	if p.state == "" {
		p.state = LatencyBackfillWaiting
	}
	return p
}

// rateLimitHold reports whether a rate-limit hold stands at now, the walk's
// own or the scheduler's, and when the later of the two runs out.
func (b *latencyBackfill) rateLimitHold(now time.Time) (time.Time, bool) {
	var until time.Time
	held := now.Before(b.heldUntil)
	if held {
		until = b.heldUntil
	}
	if b.rateLimitHeld != nil {
		if s, sched := b.rateLimitHeld(now); sched {
			held = true
			if s.After(until) {
				until = s
			}
		}
	}
	return until, held
}

// enter records the phase the walk is in. Only a change does anything: the
// Info line of the state entered (a state or pause reason the walk was not
// already in, never once per step while it lasts), and a signal on changed
// so both UIs read the status again (a rate-limit pause whose end moved
// signals without a log line).
func (b *latencyBackfill) enter(p latencyPhase) {
	b.mu.Lock()
	prev := b.phase
	if prev.state == "" {
		prev.state = LatencyBackfillWaiting
	}
	b.phase = p
	b.mu.Unlock()
	if prev.same(p) {
		return
	}
	if prev.state != p.state || prev.pause != p.pause {
		b.logEntered(prev, p)
	}
	if b.changed != nil {
		b.changed()
	}
}

// toAsk is how many rows are still without a mode, for the log lines; -1
// when the count could not be read.
func (b *latencyBackfill) toAsk() int {
	done, total, err := b.db.LatencyBackfillCounts()
	if err != nil {
		return -1
	}
	return total - done
}

// logEntered logs the Info line for entering p from prev.
func (b *latencyBackfill) logEntered(prev, p latencyPhase) {
	if prev.state == LatencyBackfillOff {
		b.logger.Info("latency backfill: switched on")
	}
	switch p.state {
	case LatencyBackfillOff:
		b.run = latencyRun{}
		b.logger.Info("latency backfill: switched off (monitors.latency_backfill = false); no recording is asked about until it is switched on")
	case LatencyBackfillRunning:
		if b.run.active {
			b.logger.Info("latency backfill: resumed", "toAsk", b.toAsk())
			return
		}
		b.run = latencyRun{active: true}
		b.logger.Info("latency backfill: started, asking YouTube about one recording a minute", "toAsk", b.toAsk())
	case LatencyBackfillPaused:
		switch p.pause {
		case LatencyPauseOffline:
			b.logger.Info("latency backfill: paused while the device is offline")
		case LatencyPauseRateLimit:
			b.logger.Info("latency backfill: paused for a YouTube rate limit", "until", p.until.UTC().Format(time.RFC3339))
		case LatencyPauseCaptureLive:
			b.logger.Info("latency backfill: paused while a capture is live")
		}
	case LatencyBackfillCaughtUp:
		run := b.run
		b.run = latencyRun{}
		b.logger.Info("latency backfill: caught up", "filled", run.filled, "unstated", run.unstated,
			"gaveUp", run.gaveUp, "left", b.toAsk())
	}
}

// LatencyBackfillChanged is signalled (capacity 1, so changes coalesce)
// each time the latency-mode backfill's status changes: cmd/moombox reads
// LatencyBackfillStatus and pushes it to both UIs.
func (w *DownloadWorker) LatencyBackfillChanged() <-chan struct{} { return w.latencyChanged }

// signalLatencyChanged signals LatencyBackfillChanged without blocking.
func (w *DownloadWorker) signalLatencyChanged() {
	select {
	case w.latencyChanged <- struct{}{}:
	default:
	}
}

// LatencyBackfillStatus is the latency-mode backfill's status now: off
// whenever monitors.latency_backfill is false (read now, so the line never
// lags the switch), else the phase the walk last entered (waiting before
// the walk has run a step); and the counts, read now. Safe from any
// goroutine.
func (w *DownloadWorker) LatencyBackfillStatus() LatencyBackfillStatus {
	s := LatencyBackfillStatus{State: LatencyBackfillWaiting}
	if b := w.latencyWalk.Load(); b != nil {
		p := b.snapshot()
		s.State, s.Pause, s.Until = p.state, p.pause, p.until
	}
	on := true
	w.readConfig(func(c *config.MoomboxConfig) {
		if c != nil {
			on = c.Monitors.LatencyBackfillEnabled()
		}
	})
	if !on {
		s = LatencyBackfillStatus{State: LatencyBackfillOff}
	}
	if w.db != nil {
		if done, total, err := w.db.LatencyBackfillCounts(); err == nil {
			s.Done, s.Total = done, total
		}
	}
	return s
}

// LatencyBackfillLine is the one line both UIs show beside the switch in
// Settings, with a rate-limit pause's end as a wall-clock time in loc:
//
//	off (212 of 540 recordings done)
//	waiting to start (212 of 540 recordings done)
//	212 of 540 recordings done
//	paused: offline (212 of 540 recordings done)
//	paused: YouTube rate limit until 14:05 (212 of 540 recordings done)
//	paused: a capture is live (212 of 540 recordings done)
//	caught up (540 recordings)
//	caught up (538 of 540 recordings done; 2 are asked again at the next start)
//
// The counts are left out while there is no recording to consider. ""
// for a state this build does not know.
func LatencyBackfillLine(s LatencyBackfillStatus, loc *time.Location) string {
	noun := "recordings"
	if s.Total == 1 {
		noun = "recording"
	}
	counts := ""
	if s.Total > 0 {
		counts = fmt.Sprintf("%d of %d %s done", s.Done, s.Total, noun)
	}
	with := func(head string) string {
		if counts == "" {
			return head
		}
		return head + " (" + counts + ")"
	}
	switch s.State {
	case LatencyBackfillOff:
		return with("off")
	case LatencyBackfillWaiting:
		return with("waiting to start")
	case LatencyBackfillRunning:
		if counts == "" {
			return "running"
		}
		return counts
	case LatencyBackfillPaused:
		switch s.Pause {
		case LatencyPauseOffline:
			return with("paused: offline")
		case LatencyPauseCaptureLive:
			return with("paused: a capture is live")
		case LatencyPauseRateLimit:
			if s.Until.IsZero() {
				return with("paused: YouTube rate limit")
			}
			return with("paused: YouTube rate limit until " + s.Until.In(loc).Format("15:04"))
		}
		return with("paused")
	case LatencyBackfillCaughtUp:
		switch {
		case s.Total == 0:
			return "caught up (no recordings to ask about)"
		case s.Done >= s.Total:
			return fmt.Sprintf("caught up (%d %s)", s.Total, noun)
		}
		left := s.Total - s.Done
		verb := "are"
		if left == 1 {
			verb = "is"
		}
		return fmt.Sprintf("caught up (%s; %d %s asked again at the next start)", counts, left, verb)
	}
	return ""
}

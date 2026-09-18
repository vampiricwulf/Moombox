package sidecar

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// DefaultSupervisorBackoff is the restart ladder: 5 s, 15 s, 60 s, then 5 min
// repeating. The first retry is quick because the common death is a V8
// OOM-abort under a BotGuard burst (the shipped V8HardLimitMB, 512, sits just
// above the documented 400-500 MB burst) and the next mint usually fits; the
// 5-minute ceiling is what stops a permanently broken install from spinning
// for the rest of a 24/7 run.
var DefaultSupervisorBackoff = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	60 * time.Second,
	5 * time.Minute,
}

// defaultSupervisorStartTimeout bounds ONE restart attempt. It matches the
// 60 s startup budget cmd/moombox gives the first Start: the work is the same
// (blob extraction on a cold cache, then jsdom's module load inside V8).
const defaultSupervisorStartTimeout = 60 * time.Second

// SupervisorConfig wires a Supervisor. Restart is required; every other field
// has a working default.
type SupervisorConfig struct {
	// Restart brings the sidecar back on the same handle — Sidecar.Restart in
	// production. It is called with a per-attempt deadline.
	Restart func(ctx context.Context) error
	// StartTimeout bounds one attempt. Zero uses defaultSupervisorStartTimeout.
	StartTimeout time.Duration
	// Backoff is the delay before each attempt; the LAST entry repeats for
	// every attempt past it. Nil uses DefaultSupervisorBackoff.
	Backoff []time.Duration
	Logger  Logger
}

// Supervisor restarts a Sidecar whose child process died.
//
// Before this existed the sidecar was started once: a crash or an OOM-abort
// flipped markUnhealthy and nothing ever flipped it back, so for the rest of
// the process signature-ciphered formats were unresolvable (sig has no
// fallback — the routing policy is sidecar-only) and PO tokens fell to the
// goja path, which rejects the websafe-only response. Restarting is the fix;
// the health snapshot beside it is what makes the outage visible while it
// lasts.
type Supervisor struct {
	cfg      SupervisorConfig
	notices  chan string
	onUp     atomic.Pointer[func()]
	restarts atomic.Uint64

	// Ladder position carried ACROSS outages. Touched only by the Run
	// goroutine (Run and restartLoop are the same goroutine), so no lock.
	//
	// A child that dies on the next mint after coming back up — the V8
	// OOM-abort this ladder exists for — makes every attempt "succeed", so a
	// counter reset per outage would pin the process to the first rung and
	// respawn Node every few seconds for the rest of a 24/7 run.
	lastAttempt int           // rung index of the last SUCCESSFUL restart
	lastRung    time.Duration // the delay that preceded that restart
	lastUp      time.Time     // when it came back; zero before the first restart
	// sleep and now are test seams. Production leaves them as sleepCtx and
	// time.Now.
	sleep func(ctx context.Context, d time.Duration)
	now   func() time.Time
}

// NewSupervisor builds a Supervisor. It does not start anything; call Run in a
// goroutine with an inline recover.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultSupervisorStartTimeout
	}
	if len(cfg.Backoff) == 0 {
		// COPIED, not aliased: DefaultSupervisorBackoff is an exported slice,
		// so sharing its backing array would let one supervisor's ladder edit
		// reach every other one and the package default itself.
		cfg.Backoff = append([]time.Duration(nil), DefaultSupervisorBackoff...)
	}
	return &Supervisor{
		cfg: cfg,
		// Buffered: Notify runs on readPump and must never block. One slot is
		// enough — the child that just died cannot die twice, so the only
		// notice that can arrive before the loop drains this one is the
		// REPLACEMENT child's, and that one must be kept, not coalesced away.
		notices: make(chan string, 1),
		sleep:   sleepCtx,
		now:     time.Now,
	}
}

// SetOnUp installs the callback run after every successful restart. It is what
// re-wires the consumers that hold the sidecar: PotProvider and the composite
// cipher solver. Safe to call before or after Run starts.
func (s *Supervisor) SetOnUp(fn func()) {
	s.onUp.Store(&fn)
}

// Notify records that the sidecar died. Safe to call from readPump: the send
// is non-blocking, so it can never stall the pump that drains the child's
// stdout. It is dropped only when a death is ALREADY queued and unread, which
// coalesces two reports of the same outage — a death that arrives while a
// restart is in flight finds the slot empty and is kept, because that one is
// the replacement child's.
func (s *Supervisor) Notify(reason string) {
	select {
	case s.notices <- reason:
	default:
	}
}

// Run drives the restart loop until ctx ends. Call it in a goroutine that
// carries its own inline recover.
func (s *Supervisor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reason := <-s.notices:
			s.cfg.Logger.Error(
				"BotGuard sidecar died — PO tokens and signature-ciphered formats are unavailable until it restarts",
				"reason", reason)
			PublishHealth(Health{
				Healthy:  false,
				Reason:   reason,
				Restarts: s.restarts.Load(),
				Since:    time.Now(),
			})
			if !s.restartLoop(ctx, reason, s.nextRung()) {
				return
			}
		}
	}
}

// nextRung is the ladder rung the coming outage starts on.
//
// The counter carries across outages while the child is FLAPPING, and resets
// only once a child has stayed up longer than the rung it came back on: that
// is a healthy child which later had an unrelated death, and making it wait
// out the previous outage's ceiling would keep a working install down for five
// minutes over a one-off crash. "Stayed up" is measured from the successful
// restart to this death, against that restart's own rung.
func (s *Supervisor) nextRung() int {
	if s.lastUp.IsZero() {
		return 0 // first outage of the process
	}
	if s.now().Sub(s.lastUp) > s.lastRung {
		return 0
	}
	return s.lastAttempt + 1
}

// restartLoop retries Restart on the ladder until it succeeds, beginning at
// startAttempt (the rung Run carried in). Returns false when ctx ended, and
// when the sidecar was stopped for good (Restart reported ErrStopped) — the
// two cases Run must stop on. Every other error is transient and climbs a rung.
func (s *Supervisor) restartLoop(ctx context.Context, reason string, startAttempt int) bool {
	for attempt := startAttempt; ; attempt++ {
		rung := s.cfg.Backoff[min(attempt, len(s.cfg.Backoff)-1)]
		s.sleep(ctx, rung)
		if ctx.Err() != nil {
			return false
		}

		startCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
		err := s.cfg.Restart(startCtx)
		cancel()
		if err != nil {
			// ErrStopped is not transient: shutdown.go called Stop, the handle
			// is terminal, and climbing the ladder against it would keep this
			// goroutine alive until the process context happens to end.
			if errors.Is(err, ErrStopped) {
				s.cfg.Logger.Debug("BotGuard sidecar supervisor stopping: the sidecar was shut down",
					"rung", attempt+1, "deadReason", reason)
				return false
			}
			s.cfg.Logger.Warn("BotGuard sidecar restart failed",
				"rung", attempt+1, "err", err, "deadReason", reason)
			continue
		}

		s.lastAttempt = attempt
		s.lastRung = rung
		s.lastUp = s.now()

		n := s.restarts.Add(1)
		s.cfg.Logger.Info("BotGuard sidecar restarted",
			"rung", attempt+1, "restarts", n)
		PublishHealth(Health{Healthy: true, Restarts: n, Since: time.Now()})
		if fn := s.onUp.Load(); fn != nil {
			s.callOnUp(*fn)
		}
		// The notice slot is deliberately NOT drained here. A notice left over
		// from the child that just died is impossible — markUnhealthy emits
		// only through a CompareAndSwap(true, false), and healthy is false
		// continuously from that death until the replacement's ready event —
		// so anything queued by now is the REPLACEMENT child reporting its own
		// death, and swallowing it would latch the process unhealthy for good
		// behind a snapshot that still read Healthy: true.
		return true
	}
}

// callOnUp isolates a faulty re-wiring callback: the sidecar is already back,
// and a panic here must not take the supervisor loop with it.
func (s *Supervisor) callOnUp(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("BotGuard sidecar OnUp panic", "panic", fmt.Sprint(r))
		}
	}()
	fn()
}

// sleepCtx waits d, or returns early when ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

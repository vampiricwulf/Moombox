package sidecar

import (
	"context"
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
	// sleep is a test seam. Production leaves it as sleepCtx.
	sleep func(ctx context.Context, d time.Duration)
}

// NewSupervisor builds a Supervisor. It does not start anything; call Run in a
// goroutine with an inline recover.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultSupervisorStartTimeout
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = DefaultSupervisorBackoff
	}
	return &Supervisor{
		cfg: cfg,
		// Buffered: Notify runs on readPump and must never block. One slot is
		// enough — a second death cannot happen before the first restart, and
		// a notice that arrives DURING a restart is already covered by it.
		notices: make(chan string, 1),
		sleep:   sleepCtx,
	}
}

// SetOnUp installs the callback run after every successful restart. It is what
// re-wires the consumers that hold the sidecar: PotProvider and the composite
// cipher solver. Safe to call before or after Run starts.
func (s *Supervisor) SetOnUp(fn func()) {
	s.onUp.Store(&fn)
}

// Notify records that the sidecar died. Safe to call from readPump: the send
// is non-blocking, so a notice arriving while a restart is already running is
// dropped rather than stalling the pump that drains the child's stdout.
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
			if !s.restartLoop(ctx, reason) {
				return
			}
		}
	}
}

// restartLoop retries Restart on the ladder until it succeeds. Returns false
// only when ctx ended, which is the one case Run must stop on.
func (s *Supervisor) restartLoop(ctx context.Context, reason string) bool {
	for attempt := 0; ; attempt++ {
		s.sleep(ctx, s.cfg.Backoff[min(attempt, len(s.cfg.Backoff)-1)])
		if ctx.Err() != nil {
			return false
		}

		startCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
		err := s.cfg.Restart(startCtx)
		cancel()
		if err != nil {
			s.cfg.Logger.Warn("BotGuard sidecar restart failed",
				"attempt", attempt+1, "err", err, "deadReason", reason)
			continue
		}

		n := s.restarts.Add(1)
		s.cfg.Logger.Info("BotGuard sidecar restarted",
			"attempt", attempt+1, "restarts", n)
		PublishHealth(Health{Healthy: true, Restarts: n, Since: time.Now()})
		if fn := s.onUp.Load(); fn != nil {
			s.callOnUp(*fn)
		}
		// A death notice queued while we were restarting describes the child
		// we just replaced. Drop it rather than immediately tearing down a
		// healthy sidecar.
		select {
		case <-s.notices:
		default:
		}
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

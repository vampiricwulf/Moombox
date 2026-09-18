package sidecar

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// silentLogger satisfies the package's Logger interface without writing
// anywhere. Deliberately NOT sidecar_test.go's testLogger, which routes
// through testing.T.Logf: the supervisor's Run goroutine can still be
// logging when a failing test returns, and a Logf after that panics.
type silentLogger struct{}

func (silentLogger) Debug(string, ...any) {}
func (silentLogger) Info(string, ...any)  {}
func (silentLogger) Warn(string, ...any)  {}
func (silentLogger) Error(string, ...any) {}

// resetHealth clears the package-level snapshot between tests. These tests
// share process state on purpose (so does production: one sidecar per
// process), so none of them calls t.Parallel().
func resetHealth(t *testing.T) {
	t.Helper()
	ResetHealthForTesting()
	t.Cleanup(ResetHealthForTesting)
}

// TestSupervisorRestartsOnTheBackoffLadder is the whole point of the row: a
// sidecar that died must come back, and the process must re-wire the two
// consumers that hold a reference to it.
//
// Mutants this kills:
//   - the restart loop giving up after one failure   → restarts == 0
//   - a fixed retry delay instead of the ladder      → delays != [5s 15s 60s]
//   - OnUp never called                              → onUp == 0
//   - health never republished as healthy            → CurrentHealth().Healthy false
func TestSupervisorRestartsOnTheBackoffLadder(t *testing.T) {
	resetHealth(t)

	var attempts atomic.Int64
	var delays []time.Duration
	var onUp atomic.Int64

	sup := NewSupervisor(SupervisorConfig{
		Logger: silentLogger{},
		Restart: func(context.Context) error {
			if attempts.Add(1) < 3 {
				return errors.New("node exited 1")
			}
			return nil
		},
	})
	sup.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }
	sup.SetOnUp(func() { onUp.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	sup.Notify("stdout EOF")

	deadline := time.After(5 * time.Second)
	for onUp.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("supervisor never completed a restart (attempts=%d)", attempts.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	if got := attempts.Load(); got != 3 {
		t.Errorf("Restart attempts = %d, want 3", got)
	}
	want := []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delays[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
	h, ok := CurrentHealth()
	if !ok || !h.Healthy || h.Restarts != 1 {
		t.Errorf("CurrentHealth() = (%+v, %v), want a healthy snapshot with Restarts 1", h, ok)
	}
}

// TestSupervisorLadderRepeatsItsLastStep: the ladder is four entries and a
// 24/7 archiver must keep trying past them.
//
// Mutants this kills:
//   - indexing the ladder without clamping  → panic (index out of range)
//   - wrapping back to the first entry      → delays[4] == 5s, not 5m
func TestSupervisorLadderRepeatsItsLastStep(t *testing.T) {
	resetHealth(t)

	var attempts atomic.Int64
	var delays []time.Duration
	sup := NewSupervisor(SupervisorConfig{
		Logger: silentLogger{},
		Restart: func(context.Context) error {
			if attempts.Add(1) < 6 {
				return errors.New("still dead")
			}
			return nil
		},
	})
	sup.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("readPump panic")

	deadline := time.After(5 * time.Second)
	for attempts.Load() < 6 {
		select {
		case <-deadline:
			t.Fatalf("supervisor stalled at %d attempts", attempts.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	if len(delays) < 6 {
		t.Fatalf("delays = %v, want at least 6 entries", delays)
	}
	for i := 3; i < 6; i++ {
		if delays[i] != 5*time.Minute {
			t.Errorf("delays[%d] = %v, want the repeated ceiling 5m", i, delays[i])
		}
	}
}

// TestSupervisorNotifyNeverBlocks: Notify runs on readPump, the goroutine that
// also drains the sidecar's stdout. A blocking send there would wedge the pump
// and with it every pending RPC.
//
// Mutant this kills: an unbuffered notices channel (or a blocking send) →
// the second Notify never returns and the test times out.
func TestSupervisorNotifyNeverBlocks(t *testing.T) {
	resetHealth(t)

	sup := NewSupervisor(SupervisorConfig{Logger: silentLogger{}, Restart: func(context.Context) error { return nil }})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			sup.Notify("stdout EOF")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked with no Run goroutine draining the channel")
	}
}

// TestSupervisorStopsWhenTheProcessShutsDown: Run must return on ctx.Done even
// while the restart ladder is sleeping, or shutdown waits on it.
//
// Mutant this kills: restartLoop ignoring ctx.Err() after the sleep →
// Run never returns and the test times out.
func TestSupervisorStopsWhenTheProcessShutsDown(t *testing.T) {
	resetHealth(t)

	ctx, cancel := context.WithCancel(context.Background())
	sup := NewSupervisor(SupervisorConfig{
		Logger:  silentLogger{},
		Restart: func(context.Context) error { return errors.New("dead") },
	})
	sup.sleep = func(c context.Context, _ time.Duration) { cancel() }

	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("stdout EOF")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestCurrentHealthIsEmptyUntilPublished: "no snapshot" is how a
// sidecar-disabled process looks to both UIs, and it must NOT read as
// "unhealthy" — that would put a permanent red alert on a correct config.
//
// Mutant this kills: CurrentHealth returning (Health{}, true) for a nil
// pointer → ok is true and both UIs would draw the alert.
func TestCurrentHealthIsEmptyUntilPublished(t *testing.T) {
	resetHealth(t)

	if h, ok := CurrentHealth(); ok {
		t.Fatalf("CurrentHealth() before any publish = (%+v, true), want ok=false", h)
	}
	PublishHealth(Health{Healthy: true, Restarts: 2})
	h, ok := CurrentHealth()
	if !ok || !h.Healthy || h.Restarts != 2 {
		t.Fatalf("CurrentHealth() = (%+v, %v), want the published snapshot", h, ok)
	}
}

// TestRestartResetsTheStartGuard: Start refuses a handle that already has a
// cmd ("sidecar: already started"). Restart must clear that state or the
// supervisor's first attempt fails forever with a message about the CORPSE.
//
// This exercises the reset without a real Node child: a Sidecar whose cacheDir
// cannot be resolved fails Start early, and the assertion is that the SECOND
// failure is the same early one and not "already started". Because that early
// failure returns before Start populates s.cmd, the corpse is planted by hand
// below — otherwise the handle would be clean and the mutant would survive.
//
// Mutant this kills: Restart calling Start without zeroing s.cmd (or without
// resetting the teardown guard) → err mentions "already started".
func TestRestartResetsTheStartGuard(t *testing.T) {
	s := New(Config{Logger: silentLogger{}, CacheDir: string([]byte{0})})

	first := s.Start(context.Background())
	if first == nil {
		t.Fatal("Start with an unusable cache dir unexpectedly succeeded")
	}
	// What a crashed child leaves behind: a populated handle Start refuses.
	// Process is nil so Stop's fast path applies and no pipe is touched.
	s.cmd = &exec.Cmd{}

	second := s.Restart(context.Background())
	if second == nil {
		t.Fatal("Restart with an unusable cache dir unexpectedly succeeded")
	}
	if got := second.Error(); got == "sidecar: already started" {
		t.Fatalf("Restart did not reset the start guard: %v", got)
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after a failed Restart")
	}
}

// TestMarkUnhealthyNotifiesTheSupervisor pins the edge that connects a dead
// child to the restart loop: readPump calls markUnhealthy, markUnhealthy hands
// the reason to Config.OnUnhealthy, and cmd/moombox wires that to
// Supervisor.Notify. Without this edge nothing ever calls Restart and the
// supervisor is dead weight.
//
// Mutants this kills:
//   - markUnhealthy not calling OnUnhealthy        → calls == 0
//   - the healthy→unhealthy CAS latch removed      → calls == 2
//   - callOnUnhealthy's recover removed            → the panic kills the test
func TestMarkUnhealthyNotifiesTheSupervisor(t *testing.T) {
	var calls atomic.Int64
	var got atomic.Value
	s := New(Config{
		Logger: silentLogger{},
		OnUnhealthy: func(reason string) {
			calls.Add(1)
			got.Store(reason)
			// readPump must survive a faulty supervisor callback.
			panic("OnUnhealthy blew up")
		},
	})
	s.healthy.Store(true)

	s.markUnhealthy("stdout EOF")
	s.markUnhealthy("stdout EOF again")

	if n := calls.Load(); n != 1 {
		t.Fatalf("OnUnhealthy calls = %d, want 1 (healthy→unhealthy latches once)", n)
	}
	if r, _ := got.Load().(string); r != "stdout EOF" {
		t.Errorf("OnUnhealthy reason = %q, want %q", r, "stdout EOF")
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after markUnhealthy")
	}
}

// TestSubscribeHealthFansOutAndUnsubscribes: the TUI is PUSH-driven (it draws
// on messages, it does not poll a package global), so the health store has to
// call it. The immediate first call matters as much as the updates — a TUI
// that starts after the sidecar died would otherwise show a healthy bar until
// the next transition, which for a permanently dead sidecar is never.
//
// Mutants this kills:
//   - no immediate call on subscribe    → got[0] is missing, len(got) == 1
//   - PublishHealth not fanning out     → len(got) == 1
//   - unsubscribe not removing the fn   → len(got) == 3
func TestSubscribeHealthFansOutAndUnsubscribes(t *testing.T) {
	resetHealth(t)
	PublishHealth(Health{Healthy: false, Reason: "stdout EOF"})

	var got []Health
	unsub := SubscribeHealth(func(h Health) { got = append(got, h) })

	PublishHealth(Health{Healthy: true, Restarts: 1})
	unsub()
	PublishHealth(Health{Healthy: false, Reason: "again"})

	if len(got) != 2 {
		t.Fatalf("callback ran %d times (%+v), want 2: the current value then one update", len(got), got)
	}
	if got[0].Healthy || got[0].Reason != "stdout EOF" {
		t.Errorf("first call = %+v, want the snapshot that already existed", got[0])
	}
	if !got[1].Healthy || got[1].Restarts != 1 {
		t.Errorf("second call = %+v, want the published update", got[1])
	}
}

// TestSupervisorObservesADeathInsideTheRestartWindow: the notice slot must NOT
// be drained after a successful restart. A "stale" notice is impossible —
// markUnhealthy only emits through a CompareAndSwap(true, false) and healthy is
// false continuously from the first death until the new child's ready event —
// so the only thing such a drain can ever discard is the NEW child's death.
// Discarding it re-latched the exact bug this row fixes: healthy stays false
// forever (no further markUnhealthy can fire), the supervisor idles with an
// empty queue, and the published snapshot reads Healthy: true over a dead child.
//
// Mutant this kills: restore the post-success
// `select { case <-s.notices: default: }` drain → only one restart happens and
// the test times out waiting for the second.
func TestSupervisorObservesADeathInsideTheRestartWindow(t *testing.T) {
	resetHealth(t)

	var attempts atomic.Int64
	var notifiedFromOnUp atomic.Bool
	restarted := make(chan struct{}, 4)

	sup := NewSupervisor(SupervisorConfig{
		Logger:  silentLogger{},
		Restart: func(context.Context) error { attempts.Add(1); return nil },
	})
	sup.sleep = func(context.Context, time.Duration) {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup.SetOnUp(func() {
		// The replacement child dies while the supervisor is still inside the
		// successful restart — precisely the window the drain used to swallow.
		if notifiedFromOnUp.CompareAndSwap(false, true) {
			sup.Notify("stdout EOF (new child)")
		}
		restarted <- struct{}{}
	})

	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("stdout EOF")

	for i := range 2 {
		select {
		case <-restarted:
		case <-time.After(5 * time.Second):
			t.Fatalf("restart %d never happened (attempts=%d) — the death inside the restart window was swallowed", i+1, attempts.Load())
		}
	}
	cancel()
	<-done

	if got := attempts.Load(); got != 2 {
		t.Errorf("Restart attempts = %d, want 2 (the original death plus the one inside the window)", got)
	}
}

// TestSupervisorLadderClimbsAcrossRepeatOutages: the failure the ladder exists
// for is a V8 OOM-abort under a BotGuard burst — a child that STARTS fine and
// dies on the next mint. Every attempt "succeeds", so a ladder reset per outage
// never leaves its first rung: the reviewer measured [5s x8] across 8
// successful restarts, i.e. a Node spawn plus a jsdom init every ~5-8 s for the
// rest of a 24/7 run. The rung has to carry across outages.
//
// Mutant this kills: reset the attempt counter on every success (restartLoop
// entered with 0 per notice) → delays == [5s 5s 5s 5s].
func TestSupervisorLadderClimbsAcrossRepeatOutages(t *testing.T) {
	resetHealth(t)

	var delays []time.Duration
	restarted := make(chan struct{}, 8)

	sup := NewSupervisor(SupervisorConfig{
		Logger:  silentLogger{},
		Restart: func(context.Context) error { return nil },
	})
	sup.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }
	sup.SetOnUp(func() { restarted <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	// Four outages, each arriving immediately after the child came back: no
	// meaningful uptime, so nothing resets the counter.
	for i := range 4 {
		sup.Notify("stdout EOF")
		select {
		case <-restarted:
		case <-time.After(5 * time.Second):
			t.Fatalf("outage %d never completed a restart", i+1)
		}
	}
	cancel()
	<-done

	want := []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second, 5 * time.Minute}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delays[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
}

// TestSupervisorLadderResetsAfterTheChildStaysUp: the counter carries only
// while the child is FLAPPING. One that came back and then ran for LONGER than
// the rung it came back on is a healthy child that later had an unrelated
// death, and punishing it with the previous outage's ceiling would leave a
// working install down for five minutes over a one-off crash.
//
// Mutant this kills: never reset the counter (always lastAttempt+1) →
// delays[1] == 15s, not 5s.
func TestSupervisorLadderResetsAfterTheChildStaysUp(t *testing.T) {
	resetHealth(t)

	var delays []time.Duration
	var clockNanos atomic.Int64
	restarted := make(chan struct{}, 4)

	sup := NewSupervisor(SupervisorConfig{
		Logger:  silentLogger{},
		Restart: func(context.Context) error { return nil },
	})
	sup.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }
	sup.now = func() time.Time { return time.Unix(0, clockNanos.Load()) }
	sup.SetOnUp(func() { restarted <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	sup.Notify("stdout EOF")
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the first outage never completed a restart")
	}

	// The replacement child ran for ten minutes — far longer than the 5 s rung
	// it came back on — before dying again.
	clockNanos.Store(int64(10 * time.Minute))

	sup.Notify("stdout EOF, much later")
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the second outage never completed a restart")
	}
	cancel()
	<-done

	want := []time.Duration{5 * time.Second, 5 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delays[%d] = %v, want %v (an outage after real uptime starts at rung 0)", i, delays[i], want[i])
		}
	}
}

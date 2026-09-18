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
	sharedHealth.Store(nil)
	t.Cleanup(func() { sharedHealth.Store(nil) })
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
// resetting stopOnce) → err mentions "already started".
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

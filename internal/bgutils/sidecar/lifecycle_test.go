package sidecar

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// TestRestartAfterStopIsANoOp: Stop is TERMINAL. cmd/moombox calls it once,
// from the shutdown path, while the supervisor's restart loop is still live in
// another goroutine — so a restart that arrives after it must not spawn a new
// Node child that nothing will ever reap.
//
// The unusable cache dir makes Start fail early without a child, so the two
// outcomes are told apart by error IDENTITY: ErrStopped means Restart returned
// before Start, anything else means it ran Start anyway.
//
// Mutants this kills:
//   - the stopped latch removed        → err is the cache-dir error, not ErrStopped
//   - Stop not setting the latch       → same
func TestRestartAfterStopIsANoOp(t *testing.T) {
	s := New(Config{Logger: silentLogger{}, CacheDir: string([]byte{0})})

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop on a never-started sidecar = %v, want nil", err)
	}

	if err := s.Restart(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Restart after Stop = %v, want ErrStopped", err)
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after Stop")
	}
}

// TestStopCancelsAnInFlightRestart: Restart holds the lifecycle lock across the
// whole 60 s startup budget, so a Stop that merely queued behind it would stall
// shutdown for a minute every time the supervisor happened to be restarting.
// Stop therefore latches and cancels BEFORE it takes that lock, and the restart
// unwinds itself.
//
// Mutants this kills:
//   - Stop taking lifecycleMu before latching/cancelling → Stop blocks, the 5 s
//     deadline fires
//   - Restart ignoring the cancel (running Start on the caller's ctx) → same
//   - the post-Start stopped re-check removed → a Restart whose start won the
//     race reports success although the process is shutting down
func TestStopCancelsAnInFlightRestart(t *testing.T) {
	s := New(Config{Logger: silentLogger{}})

	entered := make(chan struct{})
	// sync.Once because a MUTANT that drops the terminal latch reaches this
	// seam a second time, and a panicking double-close would bury the
	// assertion that actually names the defect.
	var enteredOnce sync.Once
	s.start = func(ctx context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		// What a real startLocked does at this point: block on the ready
		// handshake, which is derived from the context it was handed.
		<-ctx.Done()
		return ctx.Err()
	}

	restartErr := make(chan error, 1)
	go func() { restartErr <- s.Restart(context.Background()) }()
	<-entered

	stopErr := make(chan error, 1)
	go func() { stopErr <- s.Stop() }()
	select {
	case <-stopErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked behind an in-flight Restart instead of cancelling it")
	}

	select {
	case err := <-restartErr:
		if err == nil {
			t.Error("Restart reported success although Stop cancelled it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Restart never returned after Stop cancelled it")
	}

	// Bounded, and not on Background: a MUTANT that drops the latch runs the
	// blocking seam again, and an uncancellable context there would hang the
	// whole test binary instead of reporting the defect.
	after, acancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer acancel()
	if err := s.Restart(after); !errors.Is(err, ErrStopped) {
		t.Errorf("Restart after the cancelling Stop = %v, want ErrStopped", err)
	}
}

// TestStopAndRestartAreSerialised is the -race test for the guard. Before it,
// Restart reset readyOnce/stopOnce/cmd under writeMu while Stop read cmd and
// ran stopOnce.Do under nothing at all: shutdown.go's Stop and the supervisor's
// Restart are genuinely concurrent goroutines, so that was an unsynchronised
// read/write on the same fields.
//
// The planted corpse (cmd set, Process nil) is what makes Stop's teardown reach
// the field reads at all.
//
// Mutant this kills: the lifecycle guard removed (the transitions left
// unserialised) → `go test -race` reports a data race on Sidecar.cmd /
// Sidecar.readyOnce. "Stop wins" is asserted separately: whatever the
// interleaving, the latch is terminal afterwards.
func TestStopAndRestartAreSerialised(t *testing.T) {
	for range 20 {
		s := New(Config{Logger: silentLogger{}, CacheDir: string([]byte{0})})
		s.cmd = &exec.Cmd{}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Stop() }()
		go func() { defer wg.Done(); _ = s.Restart(context.Background()) }()
		wg.Wait()

		if err := s.Restart(context.Background()); !errors.Is(err, ErrStopped) {
			t.Fatalf("after a concurrent Stop/Restart pair, Restart = %v, want ErrStopped (Stop must win)", err)
		}
		if s.IsHealthy() {
			t.Fatal("IsHealthy() is true after a concurrent Stop/Restart pair")
		}
	}
}

// TestSupervisorStopsRetryingAStoppedSidecar: ErrStopped is not a transient
// failure — the handle can never come back — so the ladder must not keep
// climbing it until the supervisor's own context happens to end.
//
// Mutant this kills: restartLoop treating ErrStopped like any other error →
// attempts keeps growing and Run never returns on its own.
func TestSupervisorStopsRetryingAStoppedSidecar(t *testing.T) {
	resetHealth(t)

	var attempts int
	sup := NewSupervisor(SupervisorConfig{
		Logger:  silentLogger{},
		Restart: func(context.Context) error { attempts++; return ErrStopped },
	})
	sup.sleep = func(context.Context, time.Duration) {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("stdout EOF")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run kept retrying a stopped sidecar (%d attempts)", attempts)
	}
	if attempts != 1 {
		t.Errorf("Restart attempts = %d, want 1 — ErrStopped is terminal", attempts)
	}
}

// TestStartAfterStopIsRefused: Stop is terminal, and teardownLocked ends by
// clearing s.cmd — so startLocked's `s.cmd != nil` guard alone would let a
// Start arriving after a real Stop extract the blobs, spawn a Node child and
// hand-shake it, with no supervisor left to reap it. Consulting the latch is
// what closes that door, and it costs nothing: Restart already latches before
// it reaches this seam.
//
// Told apart by error IDENTITY against a deliberately unusable cache dir:
// ErrStopped means startLocked returned before it touched the disk at all,
// anything else means it ran the whole start path anyway.
//
// Mutant this kills: the isStopped() check dropped from startLocked → the
// error is the cache-dir failure, not ErrStopped.
func TestStartAfterStopIsRefused(t *testing.T) {
	s := New(Config{Logger: silentLogger{}, CacheDir: string([]byte{0})})

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop on a never-started sidecar = %v, want nil", err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start after Stop = %v, want ErrStopped", err)
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after a refused Start")
	}
}

// TestAQueuedRestartCannotStealStopsCancel: the cancel Stop reaches for must
// belong to the Restart that is RUNNING. Registering it before lifecycleMu is
// taken means a second, queued Restart overwrites the running one's — and then
// Stop cancels a restart that is only waiting for a mutex while the one
// actually inside start keeps the lock for its whole startup budget. Stop then
// queues behind it, which is precisely the minute-long shutdown stall the
// split-lock design exists to avoid.
//
// The settle window below is a mutant-side aid only: there is no observable
// "queued on lifecycleMu" signal, so R2 is given time to reach that point. If
// it somehow has not, this test passes for the wrong reason — it can never
// fail for one, because the fixed code's Stop does not depend on the timing at
// all.
//
// Mutant this kills: the restartCancel registration moved back above
// s.lifecycleMu.Lock() → Stop blocks behind R1's start and the 2 s bound fires.
func TestAQueuedRestartCannotStealStopsCancel(t *testing.T) {
	s := New(Config{Logger: silentLogger{}})

	entered := make(chan struct{})
	var enteredOnce sync.Once
	s.start = func(ctx context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		// What a real startLocked does here: block on the ready handshake,
		// which is derived from the context it was handed.
		<-ctx.Done()
		return ctx.Err()
	}

	// Bounded parents so a MUTANT run unwinds instead of wedging the test
	// binary: once R2 has displaced R1's entry, nothing else can cancel R1.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	r1 := make(chan error, 1)
	go func() { r1 <- s.Restart(ctx1) }()
	<-entered // R1 now holds lifecycleMu and is inside start

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	r2 := make(chan error, 1)
	go func() { r2 <- s.Restart(ctx2) }()
	time.Sleep(100 * time.Millisecond) // see the settle note above

	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop() }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked behind the running Restart: a queued Restart had displaced the cancel Stop reaches for")
	}

	for _, c := range []struct {
		name string
		ch   chan error
	}{{"the running Restart", r1}, {"the queued Restart", r2}} {
		select {
		case err := <-c.ch:
			if !errors.Is(err, ErrStopped) {
				t.Errorf("%s = %v, want ErrStopped", c.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s never returned after Stop", c.name)
		}
	}
}

// TestAStopCancelledRestartReportsErrStopped: a restart that Stop cancelled is
// not a transient failure to retry — the handle is terminal. Reporting the raw
// context error instead makes the supervisor log "BotGuard sidecar restart
// failed" on a perfectly ordinary shutdown and burn one more ladder rung
// against a handle that can never come back.
//
// Driven through a real Supervisor rather than by reading Restart's return,
// because the rung and the Warn are the consequences that matter.
//
// Mutant this kills: `return err` instead of the isStopped() re-check after
// start → attempts == 2 and one spurious Warn.
func TestAStopCancelledRestartReportsErrStopped(t *testing.T) {
	resetHealth(t)

	s := New(Config{Logger: silentLogger{}})
	entered := make(chan struct{})
	var enteredOnce sync.Once
	s.start = func(ctx context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}

	log := &warnCountingLogger{}
	var attempts int // touched only on the Run goroutine; read after it ends
	sup := NewSupervisor(SupervisorConfig{
		Logger: log,
		Restart: func(ctx context.Context) error {
			attempts++
			return s.Restart(ctx)
		},
	})
	sup.sleep = func(context.Context, time.Duration) {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("stdout EOF")
	<-entered

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop during an in-flight restart = %v, want nil", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor kept climbing the ladder after Stop cancelled its restart")
	}
	if attempts != 1 {
		t.Errorf("Restart attempts = %d, want 1 — a Stop-cancelled restart is terminal, not transient", attempts)
	}
	if n := log.warnCount(); n != 0 {
		t.Errorf("the supervisor logged %d Warn(s) during shutdown, want 0 — a cancelled restart is not a failure", n)
	}
}

// warnCountingLogger is silentLogger with a counted Warn: the spurious
// "restart failed" Warn during shutdown is half of what Finding 7 is about,
// and silentLogger cannot see it.
type warnCountingLogger struct {
	mu    sync.Mutex
	warns int
}

func (l *warnCountingLogger) Debug(string, ...any) {}
func (l *warnCountingLogger) Info(string, ...any)  {}
func (l *warnCountingLogger) Error(string, ...any) {}
func (l *warnCountingLogger) Warn(string, ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns++
}

func (l *warnCountingLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.warns
}

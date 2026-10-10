package sidecar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// stuckStdin stands in for the child's stdin pipe once the child has stopped
// reading it: Write blocks until Close, the way a Write into a full pipe
// blocks until the child drains it or dies.
type stuckStdin struct {
	entered   chan struct{} // closed once the first Write is blocked inside
	enterOnce sync.Once
	release   chan struct{} // closed by Close; every blocked Write then returns
	closeOnce sync.Once
}

func newStuckStdin() *stuckStdin {
	return &stuckStdin{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *stuckStdin) Write(p []byte) (int, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.release
	return 0, io.ErrClosedPipe
}

func (w *stuckStdin) Close() error {
	w.closeOnce.Do(func() { close(w.release) })
	return nil
}

// nopReadCloser is the stdout/stderr stand-in for a test that never starts
// the pumps but still runs teardownLocked, which closes both.
func nopReadCloser() io.ReadCloser { return io.NopCloser(bytes.NewReader(nil)) }

// TestACallersContextBoundsItsWaitBehindAStuckWrite: the stdin write lock used
// to be a plain mutex taken before call() ever looked at its context, so once
// one write wedged — the child not reading while a ~3 MB SolveCipher line was
// going out — every later caller queued behind it for as long as the kernel
// kept that write blocked, their own RequestTimeout notwithstanding.
//
// Mutants this kills:
//   - the lock wait not selecting on ctx.Done()   → the second call never returns
//   - the write run inline instead of on its own  → same, the first call cannot
//     goroutine                                      be left either
func TestACallersContextBoundsItsWaitBehindAStuckWrite(t *testing.T) {
	// RequestTimeout well past the test: this is about the caller's own
	// context, not the stall watchdog (TestAStalledWriteMarksTheSidecarUnhealthy).
	s := New(Config{Logger: silentLogger{}, RequestTimeout: time.Minute})
	w := newStuckStdin()
	s.stdin = w
	s.healthy.Store(true)

	first := make(chan error, 1)
	go func() { first <- s.call(context.Background(), "solveCipher", nil, nil) }()
	<-w.entered // the first call now holds the write slot inside a blocked Write

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- s.call(ctx, "ping", nil, nil) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("second call = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call with a 100 ms context waited on the write lock past 5 s")
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false while the write was merely slow, not stalled past RequestTimeout")
	}

	// Free the stranded write: it must come back with the pipe's error and
	// release the slot, or the next writer — a restart's field reset — hangs.
	_ = w.Close()
	select {
	case err := <-first:
		if err == nil {
			t.Error("the stranded write returned nil after its pipe closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stranded write never returned after its pipe closed")
	}
	select {
	case s.writeSem <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the write slot was never released after the stranded write returned")
	}
}

// TestAStalledWriteMarksTheSidecarUnhealthy: a Write the child has not drained
// within RequestTimeout is a wedged child, and the supervisor only ever learns
// of a wedge through OnUnhealthy — stdout stays open, so readPump sees nothing.
//
// Mutants this kills:
//   - no stall watchdog                         → OnUnhealthy never fires
//   - the watchdog not checking stopping        → covered by the Stop test
func TestAStalledWriteMarksTheSidecarUnhealthy(t *testing.T) {
	reasons := make(chan string, 1)
	s := New(Config{
		Logger:         silentLogger{},
		RequestTimeout: 50 * time.Millisecond,
		OnUnhealthy:    func(reason string) { reasons <- reason },
	})
	w := newStuckStdin()
	s.stdin = w
	s.healthy.Store(true)
	defer func() { _ = w.Close() }()

	if err := s.call(context.Background(), "ping", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call behind a stalled write = %v, want context.DeadlineExceeded at RequestTimeout", err)
	}
	select {
	case reason := <-reasons:
		if reason != "stdin write stalled for 50ms" {
			t.Errorf("OnUnhealthy reason = %q, want the stall named with its budget", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write stalled past RequestTimeout never marked the sidecar unhealthy")
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after a stalled write")
	}
}

// TestStopCompletesBehindAStuckWriter: teardownLocked's graceful shutdown RPC
// goes through call(), and with a mutex for the write lock it queued behind a
// stranded write with no context to save it — so Stop, and with it the whole
// shutdown sequence, hung for as long as the wedged child kept the pipe full.
// The ~3 s budget Stop documents has to hold with the slot taken.
//
// A real child process (this test binary, running TestStuckChildProcess) is
// needed so teardownLocked takes its full path — the fast path for a nil
// Process skips the RPC that used to hang.
//
// Mutants this kills:
//   - the shutdown RPC's lock wait ignoring its 1 s context → Stop never returns
func TestStopCompletesBehindAStuckWriter(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestStuckChildProcess$")
	cmd.Env = append(os.Environ(), "MOOMBOX_SIDECAR_STUCK_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	s := New(Config{Logger: silentLogger{}})
	w := newStuckStdin()
	s.cmd = cmd
	s.stdin = w
	s.stdout = nopReadCloser()
	s.stderr = nopReadCloser()
	s.healthy.Store(true)

	writer := make(chan error, 1)
	go func() { writer <- s.call(context.Background(), "solveCipher", nil, nil) }()
	<-w.entered

	started := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Stop = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop hung behind a stranded stdin write")
	}
	// 1 s for the shutdown RPC to give up its lock wait, 2 s for the child
	// to exit on its own before it is killed; the rest is slack.
	if took := time.Since(started); took > 6*time.Second {
		t.Errorf("Stop took %s, want within its ~3 s budget", took)
	}
	select {
	case err := <-writer:
		if err == nil {
			t.Error("the stranded write returned nil after Stop closed its pipe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stranded write never returned after Stop")
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after Stop")
	}
}

// TestStuckChildProcess is not a test: it is the body of the child process
// TestStopCompletesBehindAStuckWriter spawns from this binary (os/exec's own
// helper-process pattern). It plays a child that neither reads stdin nor
// exits on request, so Stop has to kill it.
func TestStuckChildProcess(t *testing.T) {
	if os.Getenv("MOOMBOX_SIDECAR_STUCK_CHILD") != "1" {
		return
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

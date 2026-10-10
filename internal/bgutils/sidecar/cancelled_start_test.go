package sidecar

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A start whose context has already ended used to extract, spawn Node and
// only then notice, spending the 2 s teardown grace of a quitting process's
// shutdown budget. It now returns at once, having started nothing.
//
// Mutant: the ctx.Err() check at the top of startLocked removed — Start
// resolves the cache dir and extracts (or, with no blobs, fails on that).
func TestAStartWithAnEndedContextStartsNothing(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir(), Logger: silentLogger{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t0 := time.Now()
	err := s.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want context.Canceled", err)
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Errorf("Start took %v with an ended context", d)
	}
	if s.cmd != nil || s.cacheDir != "" {
		t.Error("Start got past its context check")
	}
}

type warnCounter struct {
	mu    sync.Mutex
	warns []string
}

func (w *warnCounter) Debug(string, ...any) {}
func (w *warnCounter) Info(string, ...any)  {}
func (w *warnCounter) Error(string, ...any) {}
func (w *warnCounter) Warn(msg string, _ ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warns = append(w.warns, msg)
}

// A restart cut short by the process shutting down is not a failure: the
// supervisor used to log "BotGuard sidecar restart failed … context
// canceled" on an ordinary quit.
//
// Mutant: the ctx.Err() check after Restart removed — the Warn is logged.
func TestARestartEndedByShutdownIsNotAFailure(t *testing.T) {
	resetHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	log := &warnCounter{}
	sup := NewSupervisor(SupervisorConfig{
		Logger: log,
		Restart: func(context.Context) error {
			cancel() // the process quits while the restart is in flight
			return context.Canceled
		},
	})
	sup.sleep = func(context.Context, time.Duration) {}
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("stdout EOF")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not stop with its context")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.warns) != 0 {
		t.Errorf("warned %v on a shutdown", log.warns)
	}
}

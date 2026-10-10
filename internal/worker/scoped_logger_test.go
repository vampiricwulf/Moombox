package worker

import (
	"reflect"
	"sync"
	"testing"
)

// captureLogger records every line logged to it, each as its message followed
// by its args. It is safe for concurrent use: a downloader logs from its own
// goroutines, and a test that runs two of them on one logger (vodPotJob's,
// say) wrote the slice from both at once — a data race under -race, which CI
// runs over the whole module. Read it through lines(), never msgs directly.
type captureLogger struct {
	mu   sync.Mutex
	msgs [][]any
}

func (c *captureLogger) log(msg string, args []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, append([]any{msg}, args...))
}

// lines returns a snapshot of the lines logged so far.
func (c *captureLogger) lines() [][]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]any(nil), c.msgs...)
}

// reset forgets every line logged so far.
func (c *captureLogger) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = nil
}
func (c *captureLogger) Debug(msg string, args ...any) { c.log(msg, args) }
func (c *captureLogger) Info(msg string, args ...any)  { c.log(msg, args) }
func (c *captureLogger) Warn(msg string, args ...any)  { c.log(msg, args) }
func (c *captureLogger) Error(msg string, args ...any) { c.log(msg, args) }

// TestCaptureLoggerIsSafeForConcurrentUse logs from several goroutines at
// once, as two downloaders sharing one job's logger do, and expects every
// line kept.
//
// Mutant: drop the lock in log — -race reports the concurrent appends (and
// without -race, lines go missing).
func TestCaptureLoggerIsSafeForConcurrentUse(t *testing.T) {
	logs := &captureLogger{}
	const writers, each = 4, 200
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("writer %d panicked: %v", i, r)
				}
			}()
			for n := range each {
				logs.Info("line", "writer", i, "n", n)
				_ = logs.lines()
			}
		}()
	}
	wg.Wait()
	if got := len(logs.lines()); got != writers*each {
		t.Errorf("kept %d lines, want %d", got, writers*each)
	}
}

func TestScopedLoggerAppendsFixedArgs(t *testing.T) {
	cap := &captureLogger{}
	sl := newScopedLogger(cap, "jobID", "abc123", "stream", "video")
	sl.Info("segment fetched", "seq", 42)
	want := []any{"segment fetched", "seq", 42, "jobID", "abc123", "stream", "video"}
	if got := cap.lines(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// No-arg call still carries the scope.
	sl.Warn("stopped")
	want = []any{"stopped", "jobID", "abc123", "stream", "video"}
	if got := cap.lines(); len(got) != 2 || !reflect.DeepEqual(got[1], want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScopedLoggerNilInnerDoesNotPanic(t *testing.T) {
	// Regression test: an untyped nil inner logger must not panic on log
	// calls; newScopedLogger substitutes a no-op logger for it.
	sl := newScopedLogger(nil, "jobID", "x")
	// These must not panic.
	sl.Debug("test message")
	sl.Info("test message")
	sl.Warn("test message")
	sl.Error("test message")
}

// panicLogger's methods dereference the receiver, so calling any of them on
// a nil *panicLogger panics — it exists to prove newScopedLogger's typed-nil
// guard actually intercepts before inner is ever touched, not just that the
// call happens not to crash.
type panicLogger struct {
	prefix string
}

func (p *panicLogger) log(msg string) string         { return p.prefix + msg }
func (p *panicLogger) Debug(msg string, args ...any) { p.log(msg) }
func (p *panicLogger) Info(msg string, args ...any)  { p.log(msg) }
func (p *panicLogger) Warn(msg string, args ...any)  { p.log(msg) }
func (p *panicLogger) Error(msg string, args ...any) { p.log(msg) }

func TestScopedLoggerTypedNilInnerDoesNotPanic(t *testing.T) {
	// Regression test: a non-nil interface wrapping a nil concrete value
	// (e.g. `var l *someLogger; jobCtx.Logger = l`) must also be treated as
	// nil. A plain `inner == nil` check does not catch this — the interface
	// value itself is non-nil, only its dynamic value is — so without
	// isNilLogger's reflection-based detection this would nil-deref inside
	// panicLogger.log instead of no-op'ing.
	var p *panicLogger
	sl := newScopedLogger(p, "jobID", "y")
	// These must not panic.
	sl.Debug("test message")
	sl.Info("test message")
	sl.Warn("test message")
	sl.Error("test message")
}

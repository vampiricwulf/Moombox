package worker

import (
	"reflect"
	"testing"
)

type captureLogger struct {
	msgs [][]any
}

func (c *captureLogger) log(msg string, args []any) {
	c.msgs = append(c.msgs, append([]any{msg}, args...))
}
func (c *captureLogger) Debug(msg string, args ...any) { c.log(msg, args) }
func (c *captureLogger) Info(msg string, args ...any)  { c.log(msg, args) }
func (c *captureLogger) Warn(msg string, args ...any)  { c.log(msg, args) }
func (c *captureLogger) Error(msg string, args ...any) { c.log(msg, args) }

func TestScopedLoggerAppendsFixedArgs(t *testing.T) {
	cap := &captureLogger{}
	sl := newScopedLogger(cap, "jobID", "abc123", "stream", "video")
	sl.Info("segment fetched", "seq", 42)
	want := []any{"segment fetched", "seq", 42, "jobID", "abc123", "stream", "video"}
	if len(cap.msgs) != 1 || !reflect.DeepEqual(cap.msgs[0], want) {
		t.Errorf("got %v, want %v", cap.msgs, want)
	}
	// No-arg call still carries the scope.
	sl.Warn("stopped")
	want = []any{"stopped", "jobID", "abc123", "stream", "video"}
	if !reflect.DeepEqual(cap.msgs[1], want) {
		t.Errorf("got %v, want %v", cap.msgs[1], want)
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

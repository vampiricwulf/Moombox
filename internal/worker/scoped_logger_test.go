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
	// Regression test: nil inner logger must not panic on log calls.
	// The engine's nil-check is bypassed by a non-nil interface wrapping
	// a nil value, so scopedLogger must substitute a no-op logger.
	sl := newScopedLogger(nil, "jobID", "x")
	// These must not panic.
	sl.Debug("test message")
	sl.Info("test message")
	sl.Warn("test message")
	sl.Error("test message")
}

package routes

import (
	"sync"
	"testing"
)

// capturingLogger collects Error calls for assertion.
type capturingLogger struct {
	mu      sync.Mutex
	entries []capturedEntry
}

type capturedEntry struct {
	msg  string
	args []any
}

func (l *capturingLogger) Error(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, capturedEntry{msg: msg, args: args})
}

func (l *capturingLogger) snapshot() []capturedEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]capturedEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// resetPanicLogger clears the package-level logger between tests so
// state doesn't leak. SetPanicLogger uses atomic.Value, so an explicit
// store of the empty holder is the documented reset path.
func resetPanicLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		// Restore to nil-logger state for any subsequent test.
		panicLoggerPtr.Store(panicLoggerHolder{l: nil})
	})
}

func TestReportPanicWithLoggerCallsError(t *testing.T) {
	// Once SetPanicLogger has been called, recovered panics route
	// through the configured logger — atomic.Value load in
	// reportPanic finds the holder and dispatches.
	resetPanicLogger(t)
	logger := &capturingLogger{}
	SetPanicLogger(logger)

	reportPanic("test handler", "some panic value")

	entries := logger.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 logged entry, got %d", len(entries))
	}
	if entries[0].msg != "panic in route handler" {
		t.Errorf("msg: want %q, got %q", "panic in route handler", entries[0].msg)
	}

	// args layout: "where", value, "panic", value
	args := entries[0].args
	if len(args) < 4 {
		t.Fatalf("args: want >= 4 entries, got %d", len(args))
	}
	if args[0] != "where" || args[1] != "test handler" {
		t.Errorf("first arg pair: want where=test handler, got %v=%v", args[0], args[1])
	}
	if args[2] != "panic" || args[3] != "some panic value" {
		t.Errorf("second arg pair: want panic=some panic value, got %v=%v", args[2], args[3])
	}
}

func TestReportPanicWithoutLoggerFallsBackToStderr(t *testing.T) {
	// Pre-SetPanicLogger calls (early startup, tests that don't wire
	// it) fall back to os.Stderr so panics aren't silently lost.
	// We can't easily capture stderr in a unit test without race
	// conditions; instead verify the no-logger path doesn't panic.
	resetPanicLogger(t)
	panicLoggerPtr.Store(panicLoggerHolder{l: nil})

	// Should not panic and should not call any logger (none set).
	reportPanic("test handler", "another panic")
}

func TestSetPanicLoggerOverridesPrevious(t *testing.T) {
	// Late SetPanicLogger calls (e.g., a deliberate test override)
	// replace the previous logger atomically. Subsequent reportPanic
	// calls go to the new logger, not the old.
	resetPanicLogger(t)
	first := &capturingLogger{}
	second := &capturingLogger{}
	SetPanicLogger(first)
	SetPanicLogger(second)

	reportPanic("post-override", "p")

	if len(first.snapshot()) != 0 {
		t.Error("first logger should not have received the panic after override")
	}
	if len(second.snapshot()) != 1 {
		t.Errorf("second logger: want 1 entry, got %d", len(second.snapshot()))
	}
}

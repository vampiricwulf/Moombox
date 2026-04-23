package routes

import (
	"fmt"
	"os"
	"sync/atomic"
)

// panicLoggerIface is the minimal surface the routes package needs to
// record background-goroutine panics. Using a narrow interface lets us
// accept any structured logger (slog, zap, the app logger, etc.) without
// coupling to a concrete type.
type panicLoggerIface interface {
	Error(msg string, args ...any)
}

// panicLoggerPtr is set by SetPanicLogger at startup and consulted by
// reportPanic. atomic.Value holds an interface value so the read path is
// a single atomic load (no mutex on every call).
var panicLoggerPtr atomic.Value // of panicLoggerHolder

// panicLoggerHolder boxes the interface so atomic.Value.Store never sees
// typed-nil / untyped-nil mismatches.
type panicLoggerHolder struct {
	l panicLoggerIface
}

// SetPanicLogger installs a logger used by internal route helpers to
// report panics recovered from background goroutines (restart, update
// apply, setup post-save). Safe to call once at startup, nil-safe.
// When no logger is installed, panics fall back to os.Stderr so that
// diagnostics are never fully lost.
func SetPanicLogger(logger panicLoggerIface) {
	panicLoggerPtr.Store(panicLoggerHolder{l: logger})
}

// reportPanic logs a recovered panic from a background goroutine.
// Prefers the configured logger; falls back to os.Stderr (preserving
// pre-SetPanicLogger behavior) when none is set.
func reportPanic(where string, v any) {
	if h, ok := panicLoggerPtr.Load().(panicLoggerHolder); ok && h.l != nil {
		h.l.Error("panic in route handler", "where", where, "panic", v)
		return
	}
	fmt.Fprintf(os.Stderr, "panic in %s: %v\n", where, v)
}

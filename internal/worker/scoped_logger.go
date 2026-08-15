package worker

import "reflect"

// nopLogger is a no-op logger used when inner is nil (including a non-nil
// interface wrapping a nil concrete value — see isNilLogger), preserving
// graceful no-op behavior when scopedLogger wraps a nil inner.
type nopLogger struct{}

func (nopLogger) Debug(msg string, args ...any) {}
func (nopLogger) Info(msg string, args ...any)  {}
func (nopLogger) Warn(msg string, args ...any)  {}
func (nopLogger) Error(msg string, args ...any) {}

// scopedLogger wraps a job logger, appending fixed key-value pairs to every
// call, so engine components — which know nothing about jobs — emit
// attributable lines. Motivated by the 2026-08-14 premiere investigation,
// where the engine's per-segment 403 lines carried no jobID and interleaved
// video/audio downloaders were indistinguishable. Satisfies both the worker's
// anonymous logger interface and engine.DownloaderLogger (same four methods).
type scopedLogger struct {
	inner logger
	args  []any
}

func newScopedLogger(inner logger, args ...any) *scopedLogger {
	if isNilLogger(inner) {
		inner = nopLogger{}
	}
	return &scopedLogger{inner: inner, args: args}
}

// isNilLogger reports whether inner is nil in either sense that matters
// here: the untyped nil interface (inner == nil), or a non-nil interface
// wrapping a nil concrete value — e.g. `var l *someLogger; jobCtx.Logger = l`
// — which passes a plain `== nil` check but nil-derefs on first use. The
// untyped case is checked first so the common (non-nil) path costs nothing
// beyond that comparison; reflection only runs for the rarer typed case.
func isNilLogger(inner logger) bool {
	if inner == nil {
		return true
	}
	switch v := reflect.ValueOf(inner); v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// Merged slice is built with an explicit copy to avoid aliasing a spread
// caller's backing array when they call logger.Info(msg, kv...) and kv
// has spare capacity.
func (s *scopedLogger) Debug(msg string, args ...any) {
	merged := append(append([]any(nil), args...), s.args...)
	s.inner.Debug(msg, merged...)
}
func (s *scopedLogger) Info(msg string, args ...any) {
	merged := append(append([]any(nil), args...), s.args...)
	s.inner.Info(msg, merged...)
}
func (s *scopedLogger) Warn(msg string, args ...any) {
	merged := append(append([]any(nil), args...), s.args...)
	s.inner.Warn(msg, merged...)
}
func (s *scopedLogger) Error(msg string, args ...any) {
	merged := append(append([]any(nil), args...), s.args...)
	s.inner.Error(msg, merged...)
}

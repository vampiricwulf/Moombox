package worker

// nopLogger is a no-op logger used when inner is nil, preserving graceful
// no-op behavior when scopedLogger wraps a nil inner.
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
	if inner == nil {
		inner = nopLogger{}
	}
	return &scopedLogger{inner: inner, args: args}
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

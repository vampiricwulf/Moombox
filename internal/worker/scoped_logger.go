package worker

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
	return &scopedLogger{inner: inner, args: args}
}

// The variadic slice is freshly allocated per call, so appending the fixed
// args to it cannot alias a caller's backing array.
func (s *scopedLogger) Debug(msg string, args ...any) { s.inner.Debug(msg, append(args, s.args...)...) }
func (s *scopedLogger) Info(msg string, args ...any)  { s.inner.Info(msg, append(args, s.args...)...) }
func (s *scopedLogger) Warn(msg string, args ...any)  { s.inner.Warn(msg, append(args, s.args...)...) }
func (s *scopedLogger) Error(msg string, args ...any) { s.inner.Error(msg, append(args, s.args...)...) }

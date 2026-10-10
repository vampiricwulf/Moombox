//go:build !unix

package logger

// SurviveBrokenPipes is a no-op off Unix: Windows has no SIGPIPE, and a write
// to a pipe whose reader has gone away already returns an ordinary error,
// which the sinks swallow (lineSinks). See brokenpipe_unix.go.
func SurviveBrokenPipes() {}

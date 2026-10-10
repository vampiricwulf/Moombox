//go:build unix

package logger

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var surviveBrokenPipesOnce sync.Once

// SurviveBrokenPipes makes a write to a stdout or stderr whose reader has
// gone away fail with EPIPE, as a write to any other descriptor does, instead
// of killing the process.
//
// Unless the program asks for SIGPIPE through os/signal, the Go runtime
// answers an EPIPE on fd 1 or 2 by dying of SIGPIPE — even when the parent
// started it with SIGPIPE ignored (systemd's IgnoreSIGPIPE=, `trap "" PIPE`).
// Moombox's stdout is often a pipe: `ssh host moombox --headless` (no pty, so
// fd 1 is a pipe to sshd), `moombox --headless | tee`, and the launcher's
// child, which inherits the launcher's fd 1. When that reader goes away the
// next log line killed the process from inside the stdout write — before the
// file sink saw the line, and with every later line and every recording in
// progress gone with it — so the error the sinks swallow (lineSinks) was
// never returned at all.
//
// Notify, not Ignore: an ignored signal stays ignored across exec, so FFmpeg
// and the Node sidecar would inherit SIG_IGN, while a handled one is reset
// to the default in an exec'd child. Nobody reads the channel: a registered
// handler is all the runtime checks before sparing the process, and
// os/signal drops what a full channel cannot take.
//
// Safe to call more than once; New calls it, and main calls it first thing,
// for the launcher and the startup banner that write before any Logger
// exists.
func SurviveBrokenPipes() {
	surviveBrokenPipesOnce.Do(func() {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	})
}

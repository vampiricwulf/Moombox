package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// catchStopSignals hands every stop signal (stopSignals) the process receives
// to onStop, for the rest of the process's life. The launcher forwards each
// one to its child; the child's onStop cancels run's context, which starts the
// graceful shutdown.
//
// The child used signal.NotifyContext, whose stop function — run's cancel —
// also unregisters the signals, and both the TUI's exit and a restart call it
// before shutdown() begins: a second signal from then on took the default
// action and killed the child mid-shutdown. That is the common case for a
// hangup, which arrives more than once (the terminal's, the shell's forward to
// its jobs, the launcher's own forward), and it is what a second Ctrl+C at the
// terminal the TUI has just handed back sends. Here a repeated signal only runs
// onStop again, which a cancel shrugs off; shutdown's force-exit backstop
// bounds how long a stopping process outlives the first one.
func catchStopSignals(signals []os.Signal, onStop func(os.Signal)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "panic in stop-signal handler: %v\n", r)
			}
		}()
		for sig := range ch {
			onStop(sig)
		}
	}()
}

// stopOnSignal is the child's handling of a stop signal: run's context ends,
// which starts the graceful shutdown — in TUI mode tui.Run quits on it first,
// because bubbletea handles SIGINT and SIGTERM itself but not a hangup. A
// hangup when stdout is a terminal also marks that terminal gone, so the
// shutdown writes nothing more to it (muteHungUpTerminal). The stop also
// overrides a restart, requested before it or after (restartPending).
func (s *runState) stopOnSignal(sig os.Signal) {
	if isHangup(sig) && s.stdoutIsTerminal {
		s.stdoutHungUp.Store(true) // before the cancel: whoever sees ctx end sees this
	}
	s.stopSignalled.Store(true)
	s.cancel()
}

// restartPending reports whether this run ends in a restart — exit
// exitCodeRestart, which the launcher answers with a respawn: one was
// requested (triggerRestart) and no stop signal arrived, whichever came
// first. A restart's drain gives a stop 5 s and the shutdown after it to
// land, and the child used to exit 42 regardless: the terminal it ran in had
// just closed, or the operator had pressed Ctrl+C or sent SIGTERM, and the
// launcher respawned it — on a closed terminal, as a headless child on the
// deleted pty, recording and holding the instance lock where nobody could see
// or stop it.
func (s *runState) restartPending() bool {
	return s.restartRequested.Load() && !s.stopSignalled.Load()
}

// stopOverrodeRestart reports a restart that was requested and that a stop
// signal overrode (restartPending), for the shutdown to say so.
func (s *runState) stopOverrodeRestart() bool {
	return s.restartRequested.Load() && s.stopSignalled.Load()
}

// muteHungUpTerminal stops the logger writing to stdout once stdout's terminal
// has hung up; moombox.log, the ring buffer and the dashboard still get every
// line. The shutdown makes no attempt on a terminal that is gone. A write to a
// hung-up tty fails at once (EIO), but one whose far side is still open and no
// longer drains it — a `kill -HUP` while the terminal emulator is frozen —
// blocks, and the logger writes stdout under the lock its file writes take,
// which the force-exit backstop's own log line needs: the backstop that ends a
// stalled shutdown would stall with it. restoreStdoutAfterTUI leaves stdout
// suppressed after a hangup for the same reason.
func (s *runState) muteHungUpTerminal() {
	if s.stdoutHungUp.Load() {
		s.log.SuppressStdout()
	}
}

// restoreStdoutAfterTUI gives the logger's stdout back to the terminal the TUI
// has handed back — unless a hangup ended the run, when there is no terminal
// to hand back and stdout stays suppressed (muteHungUpTerminal).
func (s *runState) restoreStdoutAfterTUI() {
	if !s.stdoutHungUp.Load() {
		s.log.RestoreStdout()
	}
}

// signalExitCode is the exit status a shell reports for a process the signal
// ended — 128 plus its number: 130 for SIGINT, 143 for SIGTERM, 129 for SIGHUP.
// The launcher exits with it when a stop signal finds no child to hand it to.
func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}

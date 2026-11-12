package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
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
// overrides a restart, requested before it or after (restartPending). One that
// means Windows is ending the process (isConsoleEnding) also starts the short
// budget such a stop has (consoleEndBudget).
func (s *runState) stopOnSignal(sig os.Signal) {
	if isHangup(sig) && s.stdoutIsTerminal {
		s.stdoutHungUp.Store(true) // before the cancel: whoever sees ctx end sees this
	}
	s.stopSignalled.Store(true)
	if isConsoleEnding(sig) {
		s.consoleEnd.mark()
	}
	s.cancel()
}

// consoleEndBudget is how long a shutdown may take once Windows has said it is
// closing the console, logging the user off or shutting down. Windows ends a
// console process about 5 seconds after it delivers one of those events
// whatever the process is doing, so the ordinary shutdown — up to
// forceExitAfter, 10 seconds of it waiting for in-flight jobs before a mux is
// cancelled — was cut off part-way: the mux was killed instead of cancelled,
// and the notification flush, the final WAL checkpoint and the last log lines
// never ran. Inside this budget the worker cancels its muxes at once
// (worker.StopHurried) and the steps after it run as far as they get; when it
// runs out, the database and the log are closed and the process exits 0, as
// the ordinary backstop does. The clock starts when the signal arrives, not
// when shutdown() does: the TUI is torn down in between.
//
// Only those three events hurry a stop (owner ruling R2-X4). Ctrl+C, Quit from
// either UI, a restart and every stop on Linux keep forceExitAfter.
const consoleEndBudget = 4 * time.Second

// consoleEnd records that Windows is ending the process, and when it said so.
// Its zero value is ready to use.
type consoleEnd struct {
	mu sync.Mutex
	ch chan struct{}
	at time.Time
}

// mark records the event. Only the first call counts: its time is the one
// Windows counts from.
func (c *consoleEnd) mark() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() {
		return
	}
	c.at = time.Now()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	close(c.ch)
}

// marked is closed once the event has been recorded.
func (c *consoleEnd) marked() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
}

// left is how much of consoleEndBudget remains, never less than zero.
func (c *consoleEnd) left() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := consoleEndBudget - time.Since(c.at); d > 0 {
		return d
	}
	return 0
}

// exitWhenConsoleEnds runs exit when a console end has been marked and what is
// left of its budget has passed, unless done closes first (the shutdown
// finished in time). It returns when either has happened; shutdown runs it on
// a goroutine of its own. hurried, when not nil, is called once as soon as the
// event is seen.
func (c *consoleEnd) exitWhenConsoleEnds(done <-chan struct{}, hurried, exit func()) {
	select {
	case <-done:
		return
	case <-c.marked():
	}
	if hurried != nil {
		hurried()
	}
	t := time.NewTimer(c.left())
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		exit()
	}
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
// blocks. The logger writes stdout on its own goroutine, so that no longer
// blocks a goroutine that logs, but every line would still be queued for a
// terminal nobody reads and the logger's Close would wait its flush bound for
// it. restoreStdoutAfterTUI leaves stdout suppressed after a hangup for the
// same reason.
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

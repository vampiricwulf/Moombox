//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// stopSignals are the signals that stop Moombox gracefully, and the ones the
// launcher catches and forwards (launchAndSupervise) rather than dying from:
// Ctrl+C, SIGTERM and a hangup. Closing the terminal the TUI or a foreground
// --headless runs in, or a dropped SSH session, sends SIGHUP to the terminal's
// foreground process group — the launcher and the child alike — and its
// default action killed both outright: no worker drain or final resume save,
// no cancelled muxes, no notification flush, no sidecar stop, no WAL
// checkpoint. A Windows console close arrives as SIGTERM, and Windows ends the
// process about 5 seconds later: that stop runs the same steps on a shorter
// budget (consoleEndBudget).
//
// A hangup the process inherited as ignored — nohup, or a launcher started
// under it — stays ignored: signal.Notify installs a handler even then, which
// would make a nohup'd Moombox stop when the shell it was started from exits.
func stopSignals() []os.Signal {
	return stopSignalSet(signal.Ignored(syscall.SIGHUP))
}

// stopSignalSet is stopSignals for a hangup disposition: hupIgnored leaves
// SIGHUP out.
func stopSignalSet(hupIgnored bool) []os.Signal {
	set := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !hupIgnored {
		set = append(set, syscall.SIGHUP)
	}
	return set
}

// isConsoleEnding reports whether sig means the operating system is about to
// end the process on a deadline of its own, which only Windows does (the
// console close, logoff and shutdown events). A SIGTERM here is a plain stop:
// Docker and systemd wait their own, longer, configurable time after it.
func isConsoleEnding(os.Signal) bool { return false }

// isHangup reports whether sig is a hangup: the terminal it names has gone.
func isHangup(sig os.Signal) bool {
	return sig == syscall.SIGHUP
}

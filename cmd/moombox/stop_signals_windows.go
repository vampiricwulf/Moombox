//go:build windows

package main

import (
	"os"
	"syscall"
)

// stopSignals are the signals that stop Moombox gracefully, and the ones the
// launcher catches rather than dying from (launchAndSupervise). Go raises
// os.Interrupt for Ctrl+C and Ctrl+Break, and SIGTERM for the console close,
// logoff and shutdown events; Windows has no hangup.
func stopSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// isConsoleEnding reports whether sig means Windows is about to end the
// process: the console is closing, the user is logging off or the system is
// shutting down (CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT).
// Go raises SIGTERM for exactly those three and for nothing else on Windows,
// and Windows ends the process about 5 seconds later (consoleEndBudget).
func isConsoleEnding(sig os.Signal) bool { return sig == syscall.SIGTERM }

// isHangup reports whether sig is a hangup, which Windows never sends.
func isHangup(os.Signal) bool { return false }

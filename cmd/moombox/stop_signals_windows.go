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

// isHangup reports whether sig is a hangup, which Windows never sends.
func isHangup(os.Signal) bool { return false }

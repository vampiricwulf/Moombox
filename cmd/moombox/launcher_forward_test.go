package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// recordingChild is a stoppable that records what the launcher did to it.
type recordingChild struct {
	signalErr error
	signals   []os.Signal
	kills     int
}

func (c *recordingChild) Signal(s os.Signal) error {
	c.signals = append(c.signals, s)
	return c.signalErr
}

func (c *recordingChild) Kill() error {
	c.kills++
	return nil
}

// TestForwardStopLeavesWindowsChildToItsOwnShutdown pins the Windows arm: the
// console close / logoff / shutdown event the launcher's SIGTERM stands for,
// and the Ctrl+C or Ctrl+Break its os.Interrupt does, reached the child too,
// so the launcher must neither signal nor kill it. Signal always fails there,
// and the Kill it fell back to was TerminateProcess milliseconds into the
// child's graceful shutdown.
//
// Mutant: drop the `goos == "windows"` return — the child is killed.
func TestForwardStopLeavesWindowsChildToItsOwnShutdown(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		c := &recordingChild{signalErr: errors.New("not supported by windows")}
		forwardStop("windows", c, sig)
		if len(c.signals) != 0 || c.kills != 0 {
			t.Fatalf("windows, %v: signals %v, kills %d; want neither", sig, c.signals, c.kills)
		}
	}
}

// TestForwardStopSignalsElsewhere pins the other arm: a signal sent to the
// launcher's PID reaches only that PID, so it is passed on — the signal the
// launcher caught, so the child's shutdown can tell a hangup from a stop —
// and the child is killed only when it cannot be signalled.
//
// Mutants: always return early (no signal reaches the child); signal SIGTERM
// whatever was caught (the SIGINT and SIGHUP rows); drop the Kill fallback
// (an unsignallable child keeps running).
func TestForwardStopSignalsElsewhere(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt, syscall.SIGHUP} {
		c := &recordingChild{}
		forwardStop("linux", c, sig)
		if len(c.signals) != 1 || c.signals[0] != sig || c.kills != 0 {
			t.Fatalf("linux, %v: signals %v, kills %d; want that one signal and no kill", sig, c.signals, c.kills)
		}
	}

	failing := &recordingChild{signalErr: errors.New("process already finished")}
	forwardStop("linux", failing, syscall.SIGTERM)
	if failing.kills != 1 {
		t.Fatalf("linux, signal failed: kills %d; want the Kill fallback", failing.kills)
	}
}

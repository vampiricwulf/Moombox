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
// console close / logoff / shutdown event the launcher's SIGTERM stands for
// reached the child too, so the launcher must neither signal nor kill it.
// Signal always fails there, and the Kill it fell back to was TerminateProcess
// milliseconds into the child's graceful shutdown.
//
// Mutant: drop the `goos == "windows"` return — the child is killed.
func TestForwardStopLeavesWindowsChildToItsOwnShutdown(t *testing.T) {
	c := &recordingChild{signalErr: errors.New("not supported by windows")}
	forwardStop("windows", c)
	if len(c.signals) != 0 || c.kills != 0 {
		t.Fatalf("windows: signals %v, kills %d; want neither", c.signals, c.kills)
	}
}

// TestForwardStopSignalsElsewhere pins the other arm: SIGTERM reaches only the
// launcher's PID, so it is passed on, and the child is killed only when it
// cannot be signalled.
//
// Mutants: always return early (no SIGTERM reaches the child); drop the Kill
// fallback (an unsignallable child keeps running).
func TestForwardStopSignalsElsewhere(t *testing.T) {
	c := &recordingChild{}
	forwardStop("linux", c)
	if len(c.signals) != 1 || c.signals[0] != syscall.SIGTERM || c.kills != 0 {
		t.Fatalf("linux: signals %v, kills %d; want one SIGTERM and no kill", c.signals, c.kills)
	}

	failing := &recordingChild{signalErr: errors.New("process already finished")}
	forwardStop("linux", failing)
	if failing.kills != 1 {
		t.Fatalf("linux, signal failed: kills %d; want the Kill fallback", failing.kills)
	}
}

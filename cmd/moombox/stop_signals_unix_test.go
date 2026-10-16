//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"testing"
)

// TestStopSignalSet pins the Unix stop signals: Ctrl+C, SIGTERM and a hangup,
// the last unless it was inherited as ignored. A hangup — the terminal the TUI
// or a foreground --headless ran in closed, an SSH session dropped — reached
// the launcher and the child with their default disposition, and killed both
// past every step of the graceful shutdown. A nohup'd Moombox must not start
// stopping when the shell it came from exits, which a handler installed over
// the inherited ignore would make it do.
//
// Mutants: drop SIGHUP from the set (the first row); drop the hupIgnored
// guard (the second).
func TestStopSignalSet(t *testing.T) {
	if got, want := stopSignalSet(false), []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}; !slices.Equal(got, want) {
		t.Errorf("stopSignalSet(hangup not ignored) = %v, want %v", got, want)
	}
	if got, want := stopSignalSet(true), []os.Signal{os.Interrupt, syscall.SIGTERM}; !slices.Equal(got, want) {
		t.Errorf("stopSignalSet(hangup ignored) = %v, want %v", got, want)
	}
}

// TestStopSignalsLeaveAnIgnoredHangupIgnored pins that stopSignals asks the
// process's real disposition: with SIGHUP ignored, as nohup leaves it, the set
// has no SIGHUP in it. (TestLauncherHandsStopSignalsToTheChild runs the same
// case end to end under nohup.)
//
// Mutant: stopSignals calling stopSignalSet(false).
func TestStopSignalsLeaveAnIgnoredHangupIgnored(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("hangups are already ignored in this process (nohup); the other row is what this test is for")
	}
	if !slices.Contains(stopSignals(), os.Signal(syscall.SIGHUP)) {
		t.Fatalf("stopSignals() = %v with hangups at their default, want SIGHUP in it", stopSignals())
	}
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() {
		// Notify clears the runtime's ignored mark, which Reset alone leaves
		// set; Stop then puts the default disposition back.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		signal.Stop(ch)
	})
	if got := stopSignals(); slices.Contains(got, os.Signal(syscall.SIGHUP)) {
		t.Errorf("stopSignals() = %v with hangups ignored, want no SIGHUP: a nohup'd Moombox would stop when its shell exits", got)
	}
}

// TestStopOnSignal pins the child's handling of a stop signal: every one ends
// the run's context, and a hangup when stdout is a terminal also marks that
// terminal gone, so the shutdown writes nothing more to it. A hangup with
// stdout elsewhere (a pipe to a service manager's journal, which a unit's
// SendSIGHUP= reaches) keeps its lines.
//
// Mutants: drop the isHangup check (the SIGTERM row marks the terminal
// gone); drop the stdoutIsTerminal guard (the piped row); drop the cancel
// (every row's context stays live).
func TestStopOnSignal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sig        os.Signal
		onTerminal bool
		wantHungUp bool
	}{
		{"a hangup with stdout on the terminal", syscall.SIGHUP, true, true},
		{"SIGTERM with stdout on the terminal", syscall.SIGTERM, true, false},
		{"Ctrl+C with stdout on the terminal", os.Interrupt, true, false},
		{"a hangup with stdout piped", syscall.SIGHUP, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := &runState{ctx: ctx, cancel: cancel, stdoutIsTerminal: tc.onTerminal}
			s.stopOnSignal(tc.sig)
			if ctx.Err() == nil {
				t.Error("the run's context is still live: the signal stopped nothing")
			}
			if got := s.stdoutHungUp.Load(); got != tc.wantHungUp {
				t.Errorf("stdoutHungUp = %v, want %v", got, tc.wantHungUp)
			}
		})
	}
}

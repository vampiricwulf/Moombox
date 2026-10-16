//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
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

// hupHelperEnv makes the re-executed test binary print the stop signals
// stopSignals() reports, one per line, and exit; with hupHelperIgnore also
// set, after ignoring hangups itself.
const (
	hupHelperEnv    = "MOOMBOX_HUP_HELPER"
	hupHelperIgnore = "MOOMBOX_HUP_HELPER_IGNORE"
)

// TestStopSignalsLeaveAnIgnoredHangupIgnored pins that stopSignals asks the
// process's real disposition: with SIGHUP ignored — as nohup leaves it for the
// process it starts, or as the process set it itself — the set has no SIGHUP
// in it; at its default the set has one. (TestLauncherHandsStopSignalsToTheChild
// runs the nohup case end to end.)
//
// Each case runs in a helper process. Ignoring the hangup in the test binary
// itself could not be undone: Notify then Stop puts back the disposition that
// was in force before Notify — the ignore — while clearing signal.Ignored, so
// every process the test binary started afterwards inherited SIGHUP ignored,
// unseen by TestLauncherHandsStopSignalsToTheChild's guard, whose hangup rows
// then timed out under -count=2 or -shuffle.
//
// Mutant: stopSignals calling stopSignalSet(false) (the two ignored rows).
func TestStopSignalsLeaveAnIgnoredHangupIgnored(t *testing.T) {
	if os.Getenv(hupHelperEnv) == "1" {
		if os.Getenv(hupHelperIgnore) == "1" {
			signal.Ignore(syscall.SIGHUP)
		}
		for _, sig := range stopSignals() {
			fmt.Println(sig)
		}
		os.Exit(0)
	}
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("hangups are ignored in this process (nohup), and so in every helper it starts")
	}
	nohup, nohupErr := exec.LookPath("nohup")
	for _, tc := range []struct {
		name    string
		wrapper []string
		ignore  bool
		wantHUP bool
	}{
		{"a hangup at its default", nil, false, true},
		{"a hangup the process ignored", nil, true, false},
		{"a hangup inherited as ignored (nohup)", []string{nohup}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wrapper != nil && nohupErr != nil {
				t.Skip("no nohup on PATH")
			}
			argv := append(append([]string{}, tc.wrapper...), os.Args[0], "-test.run=^TestStopSignalsLeaveAnIgnoredHangupIgnored$")
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), hupHelperEnv+"=1")
			if tc.ignore {
				cmd.Env = append(cmd.Env, hupHelperIgnore+"=1")
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("helper: %v", err)
			}
			got := strings.Split(strings.TrimSpace(string(out)), "\n")
			if has := slices.Contains(got, syscall.SIGHUP.String()); has != tc.wantHUP {
				t.Errorf("stopSignals() = %q, want SIGHUP in it: %v — a nohup'd Moombox must not stop when its shell exits, and one at the default must stop on a hangup", got, tc.wantHUP)
			}
		})
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

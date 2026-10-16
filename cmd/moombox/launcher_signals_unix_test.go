//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The re-executed test binary acts out a launcher (signalHelperEnv set, no
// _MOOMBOX_CHILD) running the real launchAndSupervise, and the child that
// launcher spawns (the same binary, with _MOOMBOX_CHILD=1 added by the
// launcher). The child records every stop signal it catches into
// signalDirEnv/signals, one per line, and holds its "shutdown" open until the
// test creates signalDirEnv/release.
const (
	signalHelperEnv = "MOOMBOX_SIGNAL_HELPER"
	signalDirEnv    = "MOOMBOX_SIGNAL_DIR"
)

// launcherSignalHelper is the helper process's whole life; it never returns.
func launcherSignalHelper() {
	if os.Getenv("_MOOMBOX_CHILD") != "1" {
		launchAndSupervise() // exits with the classified child exit
		os.Exit(99)
	}
	dir := os.Getenv(signalDirEnv)
	ctx, cancel := context.WithCancel(context.Background())
	s := &runState{ctx: ctx, cancel: cancel}
	// run()'s own wiring (TestRunWiresTheStopSignals), plus the record.
	catchStopSignals(stopSignals(), func(sig os.Signal) {
		if f, err := os.OpenFile(filepath.Join(dir, "signals"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, sig)
			f.Close()
		}
		s.stopOnSignal(sig)
	})
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644); err != nil {
		os.Exit(98)
	}
	<-ctx.Done()
	for {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			os.Exit(0) // a shutdown that finished
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// launcherRun is one launcher the test started, in a process group of its own
// (pgid == pid), with its child.
type launcherRun struct {
	cmd  *exec.Cmd
	dir  string
	out  bytes.Buffer
	done chan struct{}
	err  error // cmd.Wait's, once done is closed
}

func startLauncher(t *testing.T, wrapper ...string) *launcherRun {
	t.Helper()
	l := &launcherRun{dir: t.TempDir(), done: make(chan struct{})}
	argv := append(append([]string{}, wrapper...), os.Args[0], "-test.run=^TestLauncherHandsStopSignalsToTheChild$")
	l.cmd = exec.Command(argv[0], argv[1:]...)
	l.cmd.Dir = l.dir
	// HOME places the single-instance lock in the temp dir, clear of any
	// other moombox on this machine.
	env := []string{signalHelperEnv + "=1", signalDirEnv + "=" + l.dir, "HOME=" + l.dir}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "_MOOMBOX_") && !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	l.cmd.Env = env
	l.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	l.cmd.Stdout, l.cmd.Stderr = &l.out, &l.out
	if err := l.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("waiting for the launcher panicked: %v", r)
			}
		}()
		l.err = l.cmd.Wait()
		close(l.done)
	}()
	t.Cleanup(func() {
		select {
		case <-l.done:
		default:
			_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL) // only the group this test started
			<-l.done
		}
	})
	if !l.waitFor(t, "ready") {
		t.Fatalf("the child never armed its stop signals; output:\n%s", l.output())
	}
	return l
}

// output is what the launcher and its child wrote, once both are gone.
func (l *launcherRun) output() string {
	select {
	case <-l.done:
		return l.out.String()
	default:
		return "(still running)"
	}
}

// waitFor reports whether the named file appeared in the run's directory
// before the launcher exited or 10 s passed.
func (l *launcherRun) waitFor(t *testing.T, name string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(l.dir, name)); err == nil {
			return true
		}
		select {
		case <-l.done:
			_, err := os.Stat(filepath.Join(l.dir, name))
			return err == nil
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

// signals is what the child has recorded so far.
func (l *launcherRun) signals() []string {
	raw, _ := os.ReadFile(filepath.Join(l.dir, "signals"))
	return strings.Fields(strings.ReplaceAll(string(raw), " ", "_"))
}

// release lets the child's shutdown finish, then waits for the launcher.
func (l *launcherRun) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(l.dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("the launcher did not exit after its child's shutdown finished; output:\n%s", l.out.String())
	}
}

// TestLauncherHandsStopSignalsToTheChild runs the real launchAndSupervise and
// a child armed the way run() arms itself, and stops them the ways a Unix
// operator does.
//
//   - A signal to the launcher's PID alone — `kill`, `docker run` without -t
//     proxying Ctrl+C to the container's main process, a supervisor
//     signalling the main PID — is handed to the child, and the launcher
//     waits for the child's shutdown and exits with its code. Ctrl+C was
//     ignored here (W28-A05): nothing stopped. A hangup killed the launcher
//     (W28-A02) and the child got only Pdeathsig's SIGTERM.
//   - A signal to the whole process group — a terminal's hangup when the
//     window closes or an SSH session drops, Ctrl+C at a --headless run —
//     reaches both, then again (the shell's forward of the hangup to its
//     jobs, a second Ctrl+C at the terminal the TUI handed back): neither
//     process dies of it, and the child's shutdown runs to its end. A hangup
//     killed both outright.
//   - A hangup ignored from the start (nohup) stays ignored by both.
//
// Mutants: SIGHUP dropped from stopSignalSet (the hangup rows: the launcher
// dies of it); signal.Ignore(os.Interrupt) back in the launcher in place of
// catching it (the PID-alone Ctrl+C row times out); catchStopSignals
// unregistering after the first signal (the twice rows: the repeat kills the
// child); stopSignals ignoring the inherited disposition (the nohup row).
func TestLauncherHandsStopSignalsToTheChild(t *testing.T) {
	if os.Getenv(signalHelperEnv) == "1" {
		launcherSignalHelper()
	}
	hupIgnored := signal.Ignored(syscall.SIGHUP)

	for _, tc := range []struct {
		name  string
		sig   syscall.Signal
		group bool
	}{
		{"Ctrl+C to the launcher's PID alone", syscall.SIGINT, false},
		{"SIGTERM to the launcher's PID alone", syscall.SIGTERM, false},
		{"a hangup to the launcher's PID alone", syscall.SIGHUP, false},
		{"a hangup to the process group, twice", syscall.SIGHUP, true},
		{"Ctrl+C to the process group, twice", syscall.SIGINT, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sig == syscall.SIGHUP && hupIgnored {
				t.Skip("hangups are ignored in this process (nohup), and so in the launcher it starts")
			}
			l := startLauncher(t)
			target := l.cmd.Process.Pid
			if tc.group {
				target = -target
			}
			if err := syscall.Kill(target, tc.sig); err != nil {
				t.Fatal(err)
			}
			if !l.waitFor(t, "signals") {
				t.Fatalf("the child never caught the %v; launcher exit %v; output:\n%s", tc.sig, l.err, l.output())
			}
			want := strings.ReplaceAll(tc.sig.String(), " ", "_")
			if got := l.signals(); len(got) == 0 || got[0] != want {
				t.Errorf("the child caught %v first, want %s", got, want)
			}
			if tc.group {
				// The repeat lands while the child is still shutting down.
				if err := syscall.Kill(target, tc.sig); err != nil {
					t.Fatal(err)
				}
				time.Sleep(300 * time.Millisecond)
			}
			l.release(t)
			if l.err != nil {
				var exitErr *exec.ExitError
				if errors.As(l.err, &exitErr) {
					if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
						t.Fatalf("the launcher died of %v instead of waiting for its child; output:\n%s", ws.Signal(), l.out.String())
					}
				}
				t.Fatalf("the launcher exited with %v, want the child's clean 0 — the repeated signal killed the child mid-shutdown?; output:\n%s", l.err, l.out.String())
			}
		})
	}

	t.Run("a hangup ignored from the start stays ignored", func(t *testing.T) {
		nohup, err := exec.LookPath("nohup")
		if err != nil {
			t.Skip("no nohup on PATH")
		}
		l := startLauncher(t, nohup)
		if err := syscall.Kill(-l.cmd.Process.Pid, syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		select {
		case <-l.done:
			t.Fatalf("a nohup'd launcher exited on a hangup (%v); output:\n%s", l.err, l.out.String())
		default:
		}
		if got := l.signals(); len(got) != 0 {
			t.Fatalf("a nohup'd child caught %v, want nothing: the hangup was to be ignored", got)
		}
		if err := syscall.Kill(l.cmd.Process.Pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if !l.waitFor(t, "signals") {
			t.Fatalf("the child never caught the SIGTERM that followed; output:\n%s", l.output())
		}
		l.release(t)
		if l.err != nil {
			t.Fatalf("the launcher exited with %v after SIGTERM, want 0; output:\n%s", l.err, l.out.String())
		}
	})
}

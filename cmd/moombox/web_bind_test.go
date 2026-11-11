package main

import (
	"context"
	"errors"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/logger"
)

// A bind failure reaches the failure arm however late it arrives: run()
// exits headless, or marks the dashboard failed, on it. Under the old fixed
// 500 ms wait a later failure was only logged. The wait is on the signals
// alone, so the test holds the error back on a channel instead of sleeping.
//
// Mutant: return webBindUndecided from awaitWebBind before either signal.
func TestAwaitWebBindWaitsForALateFailure(t *testing.T) {
	startErr := make(chan error, 1)
	want := errors.New("TLS setup: boom")
	type result struct {
		outcome webBindOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := awaitWebBind(startErr, make(chan struct{}), 30*time.Second)
		done <- result{outcome, err}
	}()
	// Nothing has been signalled, so it must still be waiting.
	select {
	case got := <-done:
		t.Fatalf("returned %v, %v before any signal", got.outcome, got.err)
	case <-time.After(50 * time.Millisecond):
	}
	startErr <- want
	select {
	case got := <-done:
		if got.outcome != webStartReturned || !errors.Is(got.err, want) {
			t.Fatalf("outcome %v, err %v; want webStartReturned with the Start error", got.outcome, got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("awaitWebBind did not return on the Start error")
	}
}

// A bound server ends the wait on the signal, not on the backstop (30 s
// here): a healthy boot no longer pays a fixed delay.
func TestAwaitWebBindReturnsWhenBound(t *testing.T) {
	startErr := make(chan error, 1)
	bound := make(chan struct{})
	close(bound)
	outcome, err := awaitWebBind(startErr, bound, 30*time.Second)
	if outcome != webBound || err != nil {
		t.Fatalf("outcome %v, err %v; want webBound, nil", outcome, err)
	}
}

// Start returning nil (a shutdown before the bind was read) is reported as a
// return with no error, which run() treats as nothing to do.
func TestAwaitWebBindReportsACleanReturn(t *testing.T) {
	startErr := make(chan error, 1)
	startErr <- nil
	outcome, err := awaitWebBind(startErr, make(chan struct{}), 30*time.Second)
	if outcome != webStartReturned || err != nil {
		t.Fatalf("outcome %v, err %v; want webStartReturned, nil", outcome, err)
	}
}

// The backstop is not a failure: with neither signal it reports undecided
// and no error, so run() goes on exactly as it did after the old wait and a
// slow but healthy boot is never turned into a startup error.
func TestAwaitWebBindBackstopIsNotAFailure(t *testing.T) {
	outcome, err := awaitWebBind(make(chan error, 1), make(chan struct{}), 20*time.Millisecond)
	if outcome != webBindUndecided || err != nil {
		t.Fatalf("outcome %v, err %v; want webBindUndecided, nil", outcome, err)
	}
}

// webBindState is a runState with only what resolveWebBind reads: the log,
// written to a file the test reads back, and useTUI.
func webBindState(t *testing.T, useTUI bool) (s *runState, logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "moombox.log")
	log, err := logger.New(logPath, "INFO", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return &runState{log: log, useTUI: useTUI}, logPath
}

// logHas waits for a line holding want to reach the log file: the drainer
// logs from its own goroutine.
func logHas(t *testing.T, logPath, want string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), want) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With a TUI a bind failure is not fatal: resolveWebBind logs it and reports
// the dashboard failed, which the Update Applied notice then says.
//
// Mutants: drop `webBindFailed = true`; return before the Warn.
func TestResolveWebBindReportsAFailureUnderTheTUI(t *testing.T) {
	s, logPath := webBindState(t, true)
	startErr := make(chan error, 1)
	startErr <- errors.New("listen tcp :774: bind: address already in use")

	if !s.resolveWebBind(startErr, make(chan struct{})) {
		t.Error("resolveWebBind = false for a bind failure under the TUI, want true")
	}
	if !logHas(t, logPath, "continuing in TUI-only mode") {
		t.Error("the bind failure was not logged")
	}
}

// Start returning nil is a shutdown, not a failure.
//
// Mutant: `webBindFailed = true` moved out of the `err != nil` block.
func TestResolveWebBindTakesANilStartResultAsNoFailure(t *testing.T) {
	s, _ := webBindState(t, true)
	startErr := make(chan error, 1)
	startErr <- nil
	if s.resolveWebBind(startErr, make(chan struct{})) {
		t.Error("resolveWebBind = true for Start returning nil")
	}
}

// A bound server is no failure in either mode, and an error Start returns
// after the bind still reaches the log.
//
// Mutants: drop the drainer goroutine (the later error is never logged);
// treat the default arm as a failure.
func TestResolveWebBindLogsAnErrorThatArrivesAfterTheBind(t *testing.T) {
	for _, useTUI := range []bool{true, false} {
		s, logPath := webBindState(t, useTUI)
		startErr := make(chan error, 1)
		bound := make(chan struct{})
		close(bound)
		if s.resolveWebBind(startErr, bound) {
			t.Errorf("useTUI=%v: resolveWebBind = true for a bound server", useTUI)
		}
		startErr <- errors.New("serve: late failure")
		if !logHas(t, logPath, "Web server error") || !logHas(t, logPath, "late failure") {
			t.Errorf("useTUI=%v: an error from Start after the bind was not logged", useTUI)
		}
	}
}

const webBindExitHelperEnv = "MOOMBOX_TEST_WEB_BIND_EXIT"

// A headless run has no UI left once the dashboard cannot bind, so it exits,
// and with exitCodeStartupError (3): the launcher reads that as the
// environment failing (the port is taken), never as a broken update to roll
// back, and a restart policy sees a failed start. The exit is os.Exit, so the
// test runs resolveWebBind in a child process and reads its exit code.
//
// Mutants: another exit code; `!useTUI` inverted (the child returns and
// exits 0); the stderr lines dropped.
func TestResolveWebBindExitsAHeadlessRunWithTheStartupErrorCode(t *testing.T) {
	if logPath := os.Getenv(webBindExitHelperEnv); logPath != "" {
		log, err := logger.New(logPath, "INFO", 1<<20, 1)
		if err != nil {
			os.Exit(90)
		}
		s := &runState{log: log, useTUI: false}
		startErr := make(chan error, 1)
		startErr <- errors.New("listen tcp :774: bind: address already in use")
		s.resolveWebBind(startErr, make(chan struct{}))
		os.Exit(0) // not reached: the headless arm exits
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResolveWebBindExitsAHeadlessRunWithTheStartupErrorCode$")
	cmd.Env = append(os.Environ(), webBindExitHelperEnv+"="+filepath.Join(t.TempDir(), "moombox.log"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitCodeStartupError {
		t.Fatalf("the headless child ended with %v, want exit %d; stderr:\n%s", err, exitCodeStartupError, stderr.String())
	}
	for _, want := range []string{"Error: listen tcp :774: bind: address already in use", "Web dashboard is unavailable."} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr has no %q:\n%s", want, stderr.String())
		}
	}
}

// resolveWebBind is the boot's wait only while run() calls it where the
// inline wait was (TestDashboardBindsBeforeTheFirstCookieCheck pins that
// place), on the server's own Start result and Bound signal, and waits with
// awaitWebBind on the channels it was handed.
//
// Mutants: run() handing it a fresh channel in place of webErrCh or
// webServer.Bound(); resolveWebBind waiting on anything but its arguments.
func TestRunResolvesTheBindOfItsOwnWebServer(t *testing.T) {
	run, fset := callsInFunc(t, "main.go", "run")
	var seen bool
	for _, c := range run {
		if c.recv == "s" && c.name == "resolveWebBind" {
			seen = true
			if got := strings.Join(c.args, ", "); got != "webErrCh, webServer.Bound()" {
				t.Errorf("run() at %s calls s.resolveWebBind(%s), want (webErrCh, webServer.Bound())", fset.Position(c.pos), got)
			}
		}
	}
	if !seen {
		t.Fatal("run() does not call s.resolveWebBind")
	}
	if pos := firstCall(run, "", "awaitWebBind"); pos != token.NoPos {
		t.Errorf("run() calls awaitWebBind itself at %s: the wait belongs to resolveWebBind", fset.Position(pos))
	}

	inner, ifset := callsInFunc(t, "web_bind.go", "resolveWebBind")
	var waits int
	for _, c := range inner {
		if c.name == "awaitWebBind" {
			waits++
			if got := strings.Join(c.args, ", "); got != "webErrCh, bound, webBindBackstop" {
				t.Errorf("resolveWebBind at %s calls awaitWebBind(%s), want (webErrCh, bound, webBindBackstop)", ifset.Position(c.pos), got)
			}
		}
	}
	if waits != 1 {
		t.Errorf("resolveWebBind calls awaitWebBind %d times, want once", waits)
	}
}

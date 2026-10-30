package childproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// The test binary is its own stand-in for FFmpeg. With childprocRoleEnv set it
// never runs a test:
//
//   - "sleep" is the program a mux would run: it sleeps until it is killed.
//   - "starter" is the Moombox child that runs it: it starts a "sleep" through
//     CommandContext, writes the stand-in's pid to the file named by
//     childprocPIDFileEnv, and waits on it — a mux blocked in FFmpeg.
//   - "fail" writes to both streams and exits 3.
const (
	childprocRoleEnv    = "MOOMBOX_CHILDPROC_TEST_ROLE"
	childprocPIDFileEnv = "MOOMBOX_CHILDPROC_TEST_PIDFILE"
)

func TestMain(m *testing.M) {
	switch os.Getenv(childprocRoleEnv) {
	case "sleep":
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	case "starter":
		os.Exit(runStarter())
	case "fail":
		fmt.Fprint(os.Stdout, "out")
		fmt.Fprint(os.Stderr, "Conversion failed!")
		os.Exit(3)
	}
	// The main goroutine keeps the main thread to itself. The runtime never
	// retires the main thread — a goroutine that exits locked to it wedges it
	// instead — so a program forked from there cannot lose its parent thread,
	// and TestTheParentThreadOutlivesTheProgram would pass with no lock at all
	// whenever its goroutine happened to start the program from it.
	runtime.LockOSThread()
	// The helper processes these tests re-run this binary as skip the race
	// runtime's sleep at exit (atexit_sleep_ms, 1000 by default), which cost
	// each of them over a second under -race. A race a helper hits is still
	// reported as it happens and still fails it.
	if g := os.Getenv("GORACE"); !strings.Contains(g, "atexit_sleep_ms") {
		os.Setenv("GORACE", strings.TrimSpace(g+" atexit_sleep_ms=0"))
	}
	// treeguard.Run fails a run that leaves files in the package directory.
	os.Exit(treeguard.Run(m))
}

// runStarter is the "starter" role.
func runStarter() int {
	c := standIn(context.Background(), "sleep")
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	if err := os.WriteFile(os.Getenv(childprocPIDFileEnv), []byte(fmt.Sprint(c.Process.Pid)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "pid file:", err)
		return 1
	}
	_ = c.Wait()
	return 0
}

// standIn is CommandContext for this test binary in one of its roles.
func standIn(ctx context.Context, role string, env ...string) *Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	c := CommandContext(ctx, exe, "-test.run=^$")
	c.Env = append(append(os.Environ(), childprocRoleEnv+"="+role), env...)
	return c
}

// TestOutputCarriesStdoutAndTheStderrTail: Output is exec.Cmd.Output's
// contract on a tied start — stdout returned, a failed run's ExitError holding
// what the program wrote to stderr, and a Stdout already set refused.
//
// Mutants: Output not filling ee.Stderr (the stderr assertion fails); Output
// returning nil instead of the buffer (the stdout assertion fails); Output
// ignoring a Stdout already set (no error).
func TestOutputCarriesStdoutAndTheStderrTail(t *testing.T) {
	out, err := standIn(t.Context(), "fail").Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("Output err = %v, want the stand-in's exit status 3", err)
	}
	if string(out) != "out" {
		t.Errorf("Output = %q, want the stand-in's stdout", out)
	}
	if !strings.Contains(string(ee.Stderr), "Conversion failed!") {
		t.Errorf("ExitError.Stderr = %q, want the stand-in's stderr", ee.Stderr)
	}

	c := standIn(t.Context(), "fail")
	c.Stdout = os.Stdout
	if _, err := c.Output(); err == nil || errors.As(err, &ee) {
		t.Errorf("Output with Stdout already set ran the program (err = %v)", err)
	}
}

// TestAFailedStartLeavesNothingLocked: a program that cannot be started
// returns the error and gives the OS thread back — the next Start on this
// goroutine works, and Wait was never needed.
//
// Mutant: Start returning without c.unlock() on a failed start — c.locked
// stays true.
func TestAFailedStartLeavesNothingLocked(t *testing.T) {
	c := CommandContext(t.Context(), "moombox-childproc-test-no-such-program")
	if err := c.Start(); err == nil {
		t.Fatal("a program that does not exist started")
	}
	if c.locked {
		t.Error("a failed Start left the goroutine locked to its OS thread")
	}
	if err := standIn(t.Context(), "fail").Run(); err == nil {
		t.Error("the stand-in's exit status 3 was not reported")
	}
}

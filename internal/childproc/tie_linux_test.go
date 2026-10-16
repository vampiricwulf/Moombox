//go:build linux

package childproc

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// alive reports whether pid is a live process: present in /proc and neither a
// zombie nor dead. A killed stand-in is reparented to init, or to whatever
// subreaper the test runs under, and one that does not reap would leave it a
// zombie for kill(pid, 0) to find — so the state is read, not signalled.
func alive(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z' && s[i+2] != 'X'
}

// waitGone polls until pid is no longer alive or d passes.
func waitGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(pid)
}

// TestTheProgramDiesWithTheProcessThatStartedIt is W28-A01's own scenario
// with a stand-in for FFmpeg: a process blocked in a program it started is
// SIGKILLed — the crash the launcher respawns from — and the program has to
// go with it. Before CommandContext it was reparented to init and ran on, and
// the respawned child's boot re-mux wrote the same archive beside it.
//
// Mutant: prepare not setting Pdeathsig — the stand-in outlives the starter.
func TestTheProgramDiesWithTheProcessThatStartedIt(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	starter := standIn(t.Context(), "starter", childprocPIDFileEnv+"="+pidFile)
	if err := starter.Cmd.Start(); err != nil { // the starter's own tie is not what is under test
		t.Fatal(err)
	}
	var pid int
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && pid == 0; {
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if pid == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if pid == 0 {
		_ = starter.Process.Kill()
		_ = starter.Cmd.Wait()
		t.Fatal("the starter never started its stand-in")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	_ = starter.Process.Kill()
	_ = starter.Cmd.Wait()

	if !waitGone(pid, 5*time.Second) {
		t.Errorf("the stand-in (pid %d) is still running 5s after the process that started it was killed", pid)
	}
}

// TestTheParentThreadOutlivesTheProgram pins the thread lock. The parent-death
// signal belongs to the THREAD that forked, and a thread is retired whenever a
// goroutine exits while locked to it. Unlocked, the goroutine that started
// the program leaves its thread the moment it parks — on FFmpeg's stderr pipe
// in the progress runner, on the process in Wait — and any goroutine can run
// there next; one that locks and exits takes the thread down, and the kernel
// SIGKILLs a mux that had nothing wrong with it.
//
// Here the starting goroutine parks on a channel while two hundred goroutines
// each lock a thread and exit. Locked, its thread is its own and the stand-in
// lives; unlocked, the retirements reach that thread.
//
// Mutant: Start not calling runtime.LockOSThread — the stand-in is killed.
func TestTheParentThreadOutlivesTheProgram(t *testing.T) {
	started := make(chan int, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c := standIn(t.Context(), "sleep")
		if err := c.Start(); err != nil {
			t.Error(err)
			started <- 0
			return
		}
		started <- c.Process.Pid
		<-release
		_ = c.Process.Kill()
		_ = c.Wait()
	}()
	pid := <-started
	if pid == 0 {
		return
	}
	defer func() { close(release); <-done }()

	for range 20 {
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				runtime.LockOSThread()
				// Exit locked: the runtime retires the thread.
			})
		}
		wg.Wait()
		runtime.Gosched()
	}
	time.Sleep(200 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("the stand-in was killed while the goroutine that started it was still waiting on it: " +
			"its parent thread was retired under it")
	}
}

// hangupStarterEnv makes the re-executed test binary act out Moombox for
// TestTheProgramIsOutOfTheTerminalsProcessGroup: it catches SIGHUP, as the
// child does, starts the sleep stand-in through CommandContext, writes the
// stand-in's PID to the file the variable names, and waits to be killed.
const hangupStarterEnv = "MOOMBOX_CHILDPROC_TEST_HANGUP_STARTER"

// TestTheProgramIsOutOfTheTerminalsProcessGroup does to a stand-in for
// Moombox what a closing terminal does: SIGHUP to every process in its
// foreground process group. Moombox catches it and shuts down in order,
// cancelling its muxes itself; FFmpeg, in the same group with the default
// disposition, died of it at once while the mux was still live, and MuxJob
// wrote the row Error ("mux: ffmpeg: signal: hangup"), which the next start's
// boot re-mux skips, stranding the recording. The program must survive the
// group's hangup, sit in a group of its own, and still die when the process
// that started it dies — out of that group, nothing else would end it.
//
// Mutants: prepare not setting Setpgid (the stand-in dies of the hangup and
// shares the starter's group); prepare not setting Pdeathsig (the stand-in
// outlives the killed starter).
func TestTheProgramIsOutOfTheTerminalsProcessGroup(t *testing.T) {
	if pidFile := os.Getenv(hangupStarterEnv); pidFile != "" {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		c := standIn(context.Background(), "sleep")
		if err := c.Start(); err != nil {
			os.Exit(97)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(c.Process.Pid)), 0o644); err != nil {
			os.Exit(98)
		}
		for range hup {
		}
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	// The starter gets a group of its own, as a terminal's foreground job
	// does; it is started with os/exec, untied, since its own tie is not what
	// is under test.
	starter := exec.Command(exe, "-test.run=^TestTheProgramIsOutOfTheTerminalsProcessGroup$")
	starter.Env = append(os.Environ(), hangupStarterEnv+"="+pidFile)
	starter.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	starterDone := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("waiting for the starter panicked: %v", r)
			}
		}()
		_ = starter.Wait()
		close(starterDone)
	}()
	pid := 0
	t.Cleanup(func() {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-starter.Process.Pid, syscall.SIGKILL) // only the group this test made
		<-starterDone
	})

	for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && pid == 0; {
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if pid == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatal("the starter never started its stand-in")
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Errorf("the stand-in's process group is %d (%v), want its own (%d)", pgid, err, pid)
	}

	if err := syscall.Kill(-starter.Process.Pid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("the stand-in died of the hangup sent to its starter's group: a closing terminal kills FFmpeg mid-mux")
	}

	if err := starter.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-starterDone
	if !waitGone(pid, 5*time.Second) {
		t.Fatal("the stand-in outlived the process that started it: a crashed Moombox leaves its FFmpeg running")
	}
	pid = 0
}

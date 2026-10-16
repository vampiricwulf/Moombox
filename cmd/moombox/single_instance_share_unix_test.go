//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper roles TestTheInstanceLockOutlivesTheLauncherWhileItsChildRuns
// runs the test binary in: a launcher that takes the lock and starts a child,
// and that child.
const (
	lockShareRoleEnv = "MOOMBOX_TEST_LOCK_SHARE_ROLE"
	lockShareDirEnv  = "MOOMBOX_TEST_LOCK_SHARE_DIR"
)

// lockShareHelper is the test binary as a launcher or as its child. The
// launcher takes the instance lock, starts its child with the lock shared as
// launchAndSupervise does, and then either waits to be killed ("launcher") or
// releases the lock and exits normally ("launcher-release"). The child keeps
// the lock as main does, runs a grandchild (an FFmpeg or a sidecar would be
// one) that must not inherit it, collects garbage (a descriptor nothing refers
// to is closed by its finalizer), ignores SIGTERM — the shutdown the
// parent-death signal starts in a real child takes up to forceExitAfter — and
// waits to be killed.
func lockShareHelper(role, dir string) {
	write := func(name, v string) { _ = os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644) }
	switch role {
	case "launcher", "launcher-release":
		if err := acquireSingleInstanceLock(); err != nil {
			write("error", err.Error())
			os.Exit(3)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestTheInstanceLockOutlivesTheLauncherWhileItsChildRuns$", "-test.timeout=120s")
		cmd.Env = append(os.Environ(), lockShareRoleEnv+"=child")
		shareInstanceLock(cmd)
		if err := cmd.Start(); err != nil {
			write("error", err.Error())
			os.Exit(3)
		}
		write("child-pid", strconv.Itoa(cmd.Process.Pid))
		if role == "launcher-release" {
			releaseSingleInstanceLock()
			write("released", "1")
			os.Exit(0)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "child":
		signal.Ignore(syscall.SIGTERM)
		holdInheritedInstanceLock()
		grandchild := exec.Command("sleep", "60")
		if err := grandchild.Start(); err == nil {
			write("grandchild-pid", strconv.Itoa(grandchild.Process.Pid))
		}
		runtime.GC()
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
		write("ready", "1")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// waitForFile returns the contents of dir/name once it exists, or fails.
func waitForFile(t *testing.T, dir, name string) string {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(dir, "error")); err == nil {
			t.Fatalf("the helper failed: %s", b)
		}
	}
	t.Fatalf("the helper never wrote %s", name)
	return ""
}

// TestTheInstanceLockOutlivesTheLauncherWhileItsChildRuns is the W30 review's
// overlap finding at its root. The single-instance lock lived in the launcher
// alone, so a kill -9 of the launcher — the PID the operator sees — released
// it while the child, sent SIGTERM by its parent-death signal, was still
// shutting down with a mux in flight. A start in that window took the lock and
// ran a second child on the same database beside the first: its boot sweep
// removed the first child's live temporary file, and its boot re-mux of the
// same Muxing row ran beside the first child's, which then wrote Error over
// the row. The child now holds a descriptor on the same locked file, so the
// lock is held until the launcher AND its child are gone.
//
// Each case: a launcher (the test binary) takes the lock and starts a child
// that holds it; the launcher is SIGKILLed, or releases the lock and exits;
// a new acquire must fail while the child lives, and succeed once it has
// died — though the grandchild it started lives on.
//
// Mutants: shareInstanceLock not passing the descriptor (the acquire succeeds
// while the child runs); holdInheritedInstanceLock keeping the descriptor in
// a local the collector finalizes (likewise); holdInheritedInstanceLock not
// marking it close-on-exec (the grandchild holds the lock after the child
// dies); releaseSingleInstanceLock unlocking with LOCK_UN before it closes
// (the "launcher-release" case: the acquire succeeds while the child runs).
func TestTheInstanceLockOutlivesTheLauncherWhileItsChildRuns(t *testing.T) {
	if role := os.Getenv(lockShareRoleEnv); role != "" {
		lockShareHelper(role, os.Getenv(lockShareDirEnv))
		return
	}
	for _, role := range []string{"launcher", "launcher-release"} {
		t.Run(role, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := t.TempDir()
			launcher := exec.Command(os.Args[0], "-test.run=^TestTheInstanceLockOutlivesTheLauncherWhileItsChildRuns$", "-test.timeout=120s")
			launcher.Env = append(os.Environ(), lockShareRoleEnv+"="+role, lockShareDirEnv+"="+dir)
			if err := launcher.Start(); err != nil {
				t.Fatal(err)
			}
			killPID := func(name string) {
				if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			}
			t.Cleanup(func() {
				_ = launcher.Process.Kill()
				_ = launcher.Wait()
				killPID("child-pid")
				killPID("grandchild-pid")
				releaseSingleInstanceLock()
			})

			childPID, _ := strconv.Atoi(waitForFile(t, dir, "child-pid"))
			waitForFile(t, dir, "ready")
			if role == "launcher" {
				_ = launcher.Process.Signal(syscall.SIGKILL)
			}
			_ = launcher.Wait()
			if role == "launcher-release" {
				waitForFile(t, dir, "released")
			}
			if syscall.Kill(childPID, 0) != nil {
				t.Fatal("premise: the child died with its launcher")
			}

			if err := acquireSingleInstanceLock(); err == nil {
				releaseSingleInstanceLock()
				t.Fatal("a new instance took the lock while the old child was still running")
			}

			_ = syscall.Kill(childPID, syscall.SIGKILL)
			var err error
			for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
				if err = acquireSingleInstanceLock(); err == nil {
					break
				}
			}
			if err != nil {
				t.Fatalf("the lock is still held after the child died (the grandchild it started inherited it?): %v", err)
			}
			releaseSingleInstanceLock()
		})
	}
}

// TestHoldInheritedInstanceLockLeavesADescriptorThatIsNoLockFile: the
// variable set with nothing passed (by hand, or a descriptor the runtime
// opened standing at that number) is dropped from the environment and the
// descriptor left alone — wrapping it in an os.File nothing keeps would let
// the finalizer close a descriptor this process does not own.
//
// Mutant: dropping the regular-file check (the pipe's read end is wrapped
// and held as the lock).
func TestHoldInheritedInstanceLockLeavesADescriptorThatIsNoLockFile(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	saved := inheritedLock
	t.Cleanup(func() { inheritedLock = saved })
	inheritedLock = nil
	t.Setenv(instanceLockFDEnv, strconv.Itoa(int(r.Fd())))
	holdInheritedInstanceLock()
	if inheritedLock != nil {
		t.Errorf("a pipe was held as the inherited instance lock")
	}
	if _, ok := os.LookupEnv(instanceLockFDEnv); ok {
		t.Errorf("%s is still in the environment the child's programs inherit", instanceLockFDEnv)
	}
}

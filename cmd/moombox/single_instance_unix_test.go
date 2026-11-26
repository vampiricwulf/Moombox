//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSingleInstanceLockAcquireAndRelease(t *testing.T) {
	// Use a temp HOME so the test doesn't touch the user's real lock dir
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	releaseSingleInstanceLock()

	// After release, should be acquirable again
	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("second acquire after release failed: %v", err)
	}
	releaseSingleInstanceLock()
}

func TestSingleInstanceLockSecondAcquireFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer releaseSingleInstanceLock()

	// Second acquire in the same process should be a no-op success
	// (already held), not an error
	if err := acquireSingleInstanceLock(); err != nil {
		t.Errorf("second acquire (same process) should succeed, got: %v", err)
	}
}

func TestSingleInstanceLockWritesPidFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer releaseSingleInstanceLock()

	lockPath := filepath.Join(tmp, ".local", "share", "moombox", "moombox.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Error("lock file is empty; expected PID")
	}
}

// TestSingleInstanceLockNamesOnlyContentionAsAnotherInstance: the flock arm
// reported every errno as "another moombox instance is already running", so a
// filesystem without lock support (ENOLCK) restart-looped a service claiming
// a second instance that did not exist, and the launcher added "wait a few
// seconds". Only EWOULDBLOCK is that answer, with its text unchanged (the CLI
// reference quotes it); any other failure is returned as itself.
//
// Mutants: flockRefusal calling every errno contention, or none; the held
// text reworded; a lock-dir failure marked as contention.
func TestSingleInstanceLockNamesOnlyContentionAsAnotherInstance(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	lockDir := filepath.Join(tmp, ".local", "share", "moombox")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, singleInstanceLockName)
	other, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// Another holder: a descriptor of its own, so the lock is contended
	// within this process exactly as it is across two.
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	err = acquireSingleInstanceLock()
	if err == nil {
		releaseSingleInstanceLock()
		t.Fatal("acquired a lock another descriptor holds")
	}
	if !errors.Is(err, errInstanceRunning) {
		t.Errorf("contention %v is not errInstanceRunning", err)
	}
	if want := "another moombox instance is already running, or the last one is still shutting down (lock held on " + lockPath + ")"; err.Error() != want {
		t.Errorf("contention text = %q, want %q", err, want)
	}

	noLock := flockRefusal(lockPath, syscall.ENOLCK)
	if errors.Is(noLock, errInstanceRunning) || !errors.Is(noLock, syscall.ENOLCK) {
		t.Errorf("ENOLCK refusal %v: want the errno itself, not another instance", noLock)
	}

	// A HOME whose lock directory cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocker)
	if err := acquireSingleInstanceLock(); err == nil {
		releaseSingleInstanceLock()
		t.Fatal("acquired a lock under a HOME that is a file")
	} else if errors.Is(err, errInstanceRunning) {
		t.Errorf("a lock dir that cannot be created reads as another instance: %v", err)
	}
}

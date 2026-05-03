//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
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

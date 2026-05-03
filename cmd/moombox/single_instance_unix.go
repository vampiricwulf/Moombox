//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockFile holds the open file handle whose flock we own. Kept at
// package scope so releaseSingleInstanceLock can close it. nil when no
// lock is currently held.
var lockFile *os.File

// lockDirFor returns the directory in which the lock file should live.
// Prefers $HOME/.local/share/moombox; falls back to /tmp if HOME is
// unset (rare; happens in some service contexts).
func lockDirFor() string {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "share", "moombox")
	}
	return "/tmp"
}

// acquireSingleInstanceLock obtains an exclusive flock on a lock file
// in the user's data dir. Returns nil if this process is the first
// moombox in the user's session. If another process already holds the
// lock (kernel-managed, released automatically on process death — no
// stale locks possible), returns a clear error.
//
// Idempotent: a second call from the same process is a no-op success.
func acquireSingleInstanceLock() error {
	if lockFile != nil {
		return nil // already held
	}

	lockDir := lockDirFor()
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("create lock dir %q: %w", lockDir, err)
	}

	lockPath := filepath.Join(lockDir, "moombox.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %q: %w", lockPath, err)
	}

	// LOCK_EX (exclusive) | LOCK_NB (non-blocking — fail fast if held)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("another moombox instance is already running (lock held on %s)", lockPath)
	}

	// Truncate and write our PID for human debugging. The lock is on the
	// file descriptor, not the contents, so this doesn't affect locking.
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())

	lockFile = f
	return nil
}

// releaseSingleInstanceLock releases the flock and closes the file.
// Safe to call on a process that never acquired (no-op).
func releaseSingleInstanceLock() {
	if lockFile == nil {
		return
	}
	syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	lockFile.Close()
	lockFile = nil
}

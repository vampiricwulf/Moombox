//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// lockFile holds the open file handle whose flock we own. Kept at
// package scope so releaseSingleInstanceLock can close it. nil when no
// lock is currently held.
//
// Not thread-safe: acquireSingleInstanceLock and releaseSingleInstanceLock
// must be called from a single goroutine. Currently both are called only
// from launchAndSupervise() in launcher.go, which is a single-goroutine
// caller. Adding additional callers requires either a mutex here or a
// guarantee of caller serialization.
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
		return fmt.Errorf("another moombox instance is already running, or the last one is still shutting down (lock held on %s)", lockPath)
	}

	// Truncate and write our PID for human debugging. The lock is on the
	// file descriptor, not the contents, so write failures don't affect
	// locking. Errors silenced because PID file is purely diagnostic —
	// the user can always check `ps`/`pidof moombox` to find the running
	// process if the file ends up empty or stale.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())

	lockFile = f
	return nil
}

// releaseSingleInstanceLock closes this process's descriptor on the lock
// file. Safe to call on a process that never acquired (no-op).
//
// It closes, and never asks for LOCK_UN: the child holds a descriptor on the
// same open file (shareInstanceLock), and an unlock through either one
// releases the lock for both, while a close releases it only once the last
// descriptor on it is gone — so a child still running keeps it.
func releaseSingleInstanceLock() {
	if lockFile == nil {
		return
	}
	lockFile.Close()
	lockFile = nil
}

// instanceLockFDEnv tells a child which descriptor its launcher's instance
// lock arrives on (shareInstanceLock).
const instanceLockFDEnv = "_MOOMBOX_INSTANCE_LOCK_FD"

// inheritedLock is the child's descriptor on its launcher's lock file
// (holdInheritedInstanceLock), referenced for the child's whole life: an
// os.File nothing refers to is closed by its finalizer.
var inheritedLock *os.File

// shareInstanceLock hands the child cmd is about to start the launcher's
// descriptor on the instance lock, so the lock is held for as long as the
// launcher OR its child lives. A flock belongs to the open file, and the
// kernel releases it only when the last descriptor on that file closes.
//
// Without it the lock lived in the launcher alone, and a kill -9 of the
// launcher — the PID the operator sees — released it while the child, sent
// SIGTERM by its parent-death signal (configureLauncherChild), spent up to
// forceExitAfter shutting down: an instance started in that window ran a
// second child on the same database beside the first, and its boot sweep and
// boot re-mux met the first child's mux still running. Now that start is
// refused until the old child has exited ("wait a few seconds and try
// again", launchAndSupervise).
//
// Called after cmd.Env is set; with none set, the launcher's environment is
// the base, as os/exec's would be.
func shareInstanceLock(cmd *exec.Cmd) {
	if lockFile == nil {
		return
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, lockFile)
	cmd.Env = append(cmd.Env, instanceLockFDEnv+"="+strconv.Itoa(2+len(cmd.ExtraFiles)))
}

// holdInheritedInstanceLock is the child's half of shareInstanceLock: it
// keeps the descriptor the launcher passed for the rest of this process's
// life, and marks it close-on-exec first, so nothing the child starts — the
// sidecar, the browser, an FFmpeg — inherits the lock and holds it past the
// child. Called at the top of the child, before anything is started. The
// variable is dropped from the environment for the same reason; a descriptor
// that is not a regular file (the variable set by hand, nothing passed) is
// left alone, since it may be one the runtime opened.
func holdInheritedInstanceLock() {
	v, ok := os.LookupEnv(instanceLockFDEnv)
	if !ok {
		return
	}
	_ = os.Unsetenv(instanceLockFDEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return
	}
	syscall.CloseOnExec(fd)
	inheritedLock = os.NewFile(uintptr(fd), "moombox.lock")
}

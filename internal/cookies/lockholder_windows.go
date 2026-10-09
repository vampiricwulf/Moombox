package cookies

import (
	"errors"
	"os"
	"syscall"
)

// errorInvalidParameter is ERROR_INVALID_PARAMETER, which OpenProcess answers
// for a pid no process holds. syscall keeps its own copy unexported.
const errorInvalidParameter = syscall.Errno(87)

// pidRunning reports whether pid names a live process, through
// os.FindProcess's OpenProcess.
//
// Only ERROR_INVALID_PARAMETER says "no such process". Access denied is a
// process that exists and is not ours to open, and any other failure is an
// answer this cannot read; both count as running, for the reason the POSIX
// twin gives. Nothing on Windows reaches it in practice — Chromium's Windows
// lock is the plain `lockfile`, never the symlink SingletonLock that
// singletonLockHolder judges — but the rule is the same on every platform.
func pidRunning(pid int) bool {
	if pid <= 0 {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return !errors.Is(err, errorInvalidParameter)
	}
	_ = p.Release()
	return true
}

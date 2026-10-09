//go:build !windows

package cookies

import (
	"errors"
	"syscall"
)

// pidRunning reports whether pid names a live process, by sending it signal 0:
// the kernel runs every check a real signal would and delivers nothing.
//
// Only ESRCH says "no such process". EPERM is a process that exists and is
// someone else's — a browser another user started on a shared profile — and
// any other error is an answer this cannot read; both count as running,
// because the one caller deletes a lock on a false here and skips a refresh on
// a true, and only the second mistake is cheap.
//
// parseSingletonLockTarget hands it positive pid_t values only: kill(0, …) and
// kill(-n, …) address process GROUPS, which is not a question about a browser.
func pidRunning(pid int) bool {
	if pid <= 0 {
		return true
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

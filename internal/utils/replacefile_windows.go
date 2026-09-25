//go:build windows

package utils

import (
	"errors"
	"syscall"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION (32): the file is open by
// another process with sharing flags that exclude the replace. Package
// syscall does not name it.
const errorSharingViolation = syscall.Errno(32)

// transientReplaceError reports the two refusals a scanner or indexer
// produces while it briefly holds the target open. Everything else (a missing
// source, a read-only directory) is permanent and is returned on the first
// attempt.
//
// A DIRECTORY in the target's place is the one case that is permanent but does
// not look it: Windows answers the rename with ERROR_ACCESS_DENIED, the very
// code the antivirus hold produces, so it takes the whole ladder (~1 s) before
// its error surfaces. The Linux twin fails immediately, because POSIX rename
// answers EISDIR/ENOTEMPTY instead — the asymmetry is Windows-only and
// deliberate. Telling the two apart would mean an Lstat on the antivirus retry
// path the ladder exists to serve, to speed up a case only an operator error
// produces (owner ruling, option A, 2026-09-25).
func transientReplaceError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)
}

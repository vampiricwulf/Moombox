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
// produces while it briefly holds the target open. Everything else (a
// missing source, a read-only directory, a directory in the target's place)
// is permanent and must not be retried.
func transientReplaceError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)
}

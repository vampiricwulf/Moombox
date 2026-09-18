//go:build windows

package engine

import (
	"errors"
	"syscall"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION (32): the file is open by
// another process with sharing flags that exclude the truncate. Package
// syscall does not name it. Same constant utils.ReplaceFile needs for the
// rename twin of this problem.
const errorSharingViolation = syscall.Errno(32)

// transientTruncateError reports the two refusals an antivirus scanner or the
// search indexer produces while it briefly holds a just-written recording
// open. Everything else — a missing file, a directory in its place, a
// read-only volume — is permanent: retrying it only delays the error by the
// ladder's full 1270 ms while the staged recording sits untouched anyway.
func transientTruncateError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)
}

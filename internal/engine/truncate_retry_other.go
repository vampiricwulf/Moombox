//go:build !windows

package engine

import (
	"errors"
	"syscall"
)

// transientTruncateError: POSIX has no scanner-induced refusal to wait out —
// ftruncate succeeds on a file other processes hold open. The only busy-file
// refusal is EBUSY/ETXTBSY (a mapped executable), which a staged recording
// never is, so it is listed for completeness rather than because the archiver
// can reach it. Every other error (ENOENT, EISDIR, EACCES on a read-only
// mount) is permanent and must surface on the first attempt.
func transientTruncateError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ETXTBSY)
}

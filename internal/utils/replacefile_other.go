//go:build !windows

package utils

// transientReplaceError: POSIX rename replaces an open target atomically, so
// there is no transient refusal to wait out.
func transientReplaceError(error) bool { return false }

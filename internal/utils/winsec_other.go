//go:build !windows

package utils

import (
	"fmt"
	"os"
)

// ApplyUserOnlyDACL restricts the given path so only the current user has
// access — the POSIX counterpart of the Windows icacls implementation.
// Mode bits are the native mechanism here: 0700 for directories and 0600 for
// files. Unlike the Windows (OI)(CI) inheritance — which writes restrictive
// ACEs onto each child object — this only changes the path's own bits; files
// already inside a directory keep their existing modes and rely on the
// untraversable 0700 parent to block other local users.
//
// As on Windows it is for a folder that is Moombox's own and nothing else:
// the sidecar cache under the user cache directory, and a browser profile
// folder in the call that created it. It is never applied to a folder that
// already existed and that Moombox did not make (owner ruling, 2026-11-19);
// the cookie file and config.toml are written 0600, and a folder Moombox has
// to create for either is created 0700.
func ApplyUserOnlyDACL(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	mode := os.FileMode(0o600)
	if info.IsDir() {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	return nil
}

// RestrictFileToOwner is the Windows per-file DACL (see winsec_windows.go).
// Nothing to do here: the callers already write their secrets 0600, which is
// the same restriction in this platform's own terms.
func RestrictFileToOwner(path string) error { return nil }

// DACLProtected reports whether a Windows path has stopped inheriting
// permissions from its parent. There is no such flag on this platform.
func DACLProtected(path string) (bool, error) { return false, nil }

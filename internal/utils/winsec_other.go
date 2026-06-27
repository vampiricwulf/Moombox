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
// ACEs onto each child object so the file stays protected on its own — this
// only changes the path's own bits; files already inside a directory keep
// their existing modes and rely on the untraversable 0700 parent to block
// other local users (adequate for the single-host multi-user threat model).
// Callers create these paths with default modes (0755/0644) and call this to
// tighten them afterwards, exactly as on Windows; without this, sensitive
// surfaces (cookies.sqlite, moombox.toml with the password hash) stay
// readable by other local users on multi-user Linux hosts.
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

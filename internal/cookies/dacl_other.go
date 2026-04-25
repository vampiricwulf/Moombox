//go:build !windows

package cookies

// applyUserOnlyDACL is a no-op on non-Windows. POSIX hosts handle
// multi-user isolation natively via the directory's mode bits — a
// chmod 0700 on the profile dir at creation time is enough. Non-
// Windows is stub-only for cross-platform compile; Moombox is
// Windows-only at runtime.
func applyUserOnlyDACL(path string) error {
	return nil
}

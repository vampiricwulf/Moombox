//go:build !windows

package utils

// ApplyUserOnlyDACL is a no-op on non-Windows. POSIX hosts handle
// multi-user isolation natively via mode bits — chmod 0700 on a dir
// or 0600 on a file is enough. Stub-only for cross-platform compile;
// Moombox is Windows-only at runtime.
func ApplyUserOnlyDACL(path string) error {
	return nil
}

//go:build !windows

package dpapi

// FindBrowserProfiles is a no-op on non-Windows platforms. Moombox is
// Windows-only at runtime; the stub keeps cross-platform builds clean.
// Returns an empty slice so callers can range without nil-checks.
func FindBrowserProfiles() []BrowserProfile {
	return []BrowserProfile{}
}

//go:build !windows

package dpapi

// ReadChromeCookiesStats is unsupported on non-Windows platforms. Moombox
// is Windows-only for this path; the stub exists so cross-platform builds
// (the Linux/arm64 release binaries, and devs running `go build` on macOS)
// succeed without compiler errors. Returns ErrNotSupported.
func ReadChromeCookiesStats(profilePath, originFilter string) ([]ChromeCookie, ChromeReadStats, error) {
	return nil, ChromeReadStats{}, ErrNotSupported
}

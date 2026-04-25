//go:build !windows

package dpapi

// ReadChromeCookies is unsupported on non-Windows platforms. Moombox
// is Windows-only at runtime; the stub exists so cross-platform builds
// (used by devs running `go build` on macOS / Linux for sanity) succeed
// without compiler errors. Returns ErrNotSupported.
func ReadChromeCookies(profilePath, originFilter string) ([]ChromeCookie, error) {
	return nil, ErrNotSupported
}

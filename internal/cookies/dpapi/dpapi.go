// Package dpapi reads cookies from a Chromium-family browser's real user
// profile by decrypting the SQLite Cookies file directly. Used as a
// fallback when CDP-based extraction can't acquire the profile lock
// (e.g. user has the browser open). DECISIONS #6 / cookies.md Q1.
//
// Windows-only at runtime — the cross-platform stub returns
// ErrNotSupported. The pure-crypto helpers (AES-GCM, epoch conversion,
// SameSite mapping) live in this file and are tested cross-platform.
//
// SECURITY: Reading from the user's REAL Chrome profile path is
// explicitly safe because this code does NOT launch any process. The
// validateBrowserProfileDir check in the autocookies package (audit
// cookies.md #26) refuses real Chrome paths for the LAUNCH path because
// that would let Moombox open the user's signed-in session against
// their will. The DPAPI path is read-only and therefore safe — no new
// browser window, no stolen session, just a decrypt of the SQLite file
// the user already owns.
package dpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
)

// ChromeCookie is one decrypted cookie row from a Chrome / Chromium
// profile's SQLite Cookies database.
type ChromeCookie struct {
	Host     string // host_key — typically ".youtube.com" or "twitch.tv"
	Name     string
	Value    string
	Path     string
	Expires  int64  // unix seconds; 0 = session cookie
	Secure   bool
	HttpOnly bool
	SameSite string // "None" | "Lax" | "Strict" | "Unspecified"
}

// ErrNotSupported is returned by ReadChromeCookies on non-Windows
// platforms. Moombox is Windows-only at runtime, but the package
// compiles cross-platform so unit tests for shared helpers stay
// reachable on dev hosts.
var ErrNotSupported = errors.New("DPAPI cookie extraction is Windows-only")

// chromeV10Prefix tags Chrome's modern AES-GCM-encrypted cookie values.
// Pre-v10 cookies were raw DPAPI blobs; we don't support those — they
// haven't shipped in any Chrome release since 2020 and the rare row
// that still has the legacy form will fail the prefix check and get
// skipped by ReadChromeCookies (the fail-soft "skip undecryptable
// rows" path).
const chromeV10Prefix = "v10"
const chromeV11Prefix = "v11"

// decryptV10Cookie decrypts a Chrome v10+ encrypted cookie value:
//
//	"v10" || nonce(12) || ciphertext || tag(16)
//
// The version-prefix branch on v10 vs v11 is purely informational —
// both use the same AES-GCM-with-12-byte-nonce-and-16-byte-tag layout.
// Chrome on Windows produces v10; Chrome on Linux / desktop-keystore
// configurations produces v11; Edge has been seen producing both.
func decryptV10Cookie(masterKey, encrypted []byte) (string, error) {
	if len(encrypted) == 0 {
		return "", nil
	}
	prefix := ""
	switch {
	case len(encrypted) >= 3 && string(encrypted[:3]) == chromeV10Prefix:
		prefix = chromeV10Prefix
	case len(encrypted) >= 3 && string(encrypted[:3]) == chromeV11Prefix:
		prefix = chromeV11Prefix
	default:
		return "", fmt.Errorf("encrypted_value missing v10/v11 prefix (legacy DPAPI cookies are not supported)")
	}

	const nonceLen = 12
	const tagLen = 16
	if len(encrypted) < len(prefix)+nonceLen+tagLen {
		return "", fmt.Errorf("v10/v11 ciphertext too short: %d bytes (want >= %d)", len(encrypted), len(prefix)+nonceLen+tagLen)
	}
	nonce := encrypted[len(prefix) : len(prefix)+nonceLen]
	ciphertextWithTag := encrypted[len(prefix)+nonceLen:]

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertextWithTag, nil)
	if err != nil {
		return "", fmt.Errorf("AES-GCM open: %w", err)
	}
	return string(plaintext), nil
}

// chromeEpochUnixDelta is the number of seconds between Chrome's
// 1601-01-01 epoch and the unix 1970-01-01 epoch. Chrome stores
// expires_utc as microseconds since 1601-01-01.
const chromeEpochUnixDelta = 11644473600

// chromeEpochToUnix converts Chrome's microseconds-since-1601 expiry
// timestamp to unix seconds. 0 is Chrome's "session cookie" sentinel
// and is preserved as 0.
func chromeEpochToUnix(chromeUS int64) int64 {
	if chromeUS == 0 {
		return 0
	}
	return chromeUS/1_000_000 - chromeEpochUnixDelta
}

// chromeSameSiteString maps Chrome's int samesite enum to the
// canonical Set-Cookie header strings. Unknown ints map to
// "Unspecified" so a future Chrome enum extension doesn't crash
// the cookie loader. The -1 case ("not specified by site") is the
// value Chrome writes when a Set-Cookie omits SameSite.
func chromeSameSiteString(v int) string {
	switch v {
	case 0:
		return "None"
	case 1:
		return "Lax"
	case 2:
		return "Strict"
	default:
		return "Unspecified"
	}
}

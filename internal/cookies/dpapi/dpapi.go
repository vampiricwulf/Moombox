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
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// ChromeCookie is one decrypted cookie row from a Chrome / Chromium
// profile's SQLite Cookies database.
type ChromeCookie struct {
	Host     string // host_key — typically ".youtube.com" or "twitch.tv"
	Name     string
	Value    string
	Path     string
	Expires  int64 // unix seconds; 0 = session cookie
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

// chromeV20Prefix tags Chrome's App-Bound Encryption cookies (Chrome 127+,
// July 2024; Edge followed). Their key is bound to the browser via a
// SYSTEM-level service and is NOT recoverable with a plain CURRENT_USER
// CryptUnprotectData, so this fallback cannot decrypt them — but the prefix
// must be recognized so the error explains WHY the fallback yields nothing
// on current Chrome/Edge profiles (Brave kept v10 and still works).
const chromeV20Prefix = "v20"

// chromeHashPrefixMetaVersion is the Cookies-database `meta.version` at
// which Chrome started prepending a 32-byte SHA-256 of the cookie's domain
// to the plaintext INSIDE the encrypted value. Landed around Chrome 130
// (Edge followed); everything below 24 stores the bare value.
//
// Ref: https://chromium.googlesource.com/chromium/src/+/b02dcebd7cafab92770734dc2bc317bd07f1d891/net/extras/sqlite/sqlite_persistent_cookie_store.cc#223
// Mirrors yt-dlp's `meta_version >= 24` gate (yt_dlp/cookies.py:328, :559).
const chromeHashPrefixMetaVersion = 24

// chromeDomainHashLen is the length of that prefix (SHA-256 digest).
const chromeDomainHashLen = 32

// chromeUsesHashPrefix reports whether a Cookies database at the given
// meta.version prefixes decrypted values with the domain hash.
func chromeUsesHashPrefix(metaVersion int64) bool {
	return metaVersion >= chromeHashPrefixMetaVersion
}

// readChromeMetaVersion reads `meta.version` from an open Chrome Cookies
// database — the schema stamp that says whether decrypted cookie values
// carry the 32-byte domain-hash prefix.
//
// Chrome declares the column LONGVARCHAR but writes an integer, so SQLite
// may hand it back as either storage class; the value is read as text and
// parsed, which covers both.
//
// Every failure mode — no meta table (a Cookies file from a pre-meta
// Chrome, or a corrupt/truncated copy), no `version` row, NULL, a
// non-numeric value, a negative value — degrades to 0, i.e. "no hash
// prefix". The asymmetry is deliberate. Stripping 32 bytes that were never
// there silently amputates the front of every cookie value and there is no
// way to detect it downstream; NOT stripping a prefix that IS there leaves
// binary SHA-256 bytes at the head of the plaintext, which the UTF-8 check
// in decryptV10Cookie rejects — so that direction fails loudly, per row,
// and the operator sees an empty extraction rather than a poisoned one.
func readChromeMetaVersion(db *sql.DB) int64 {
	var raw sql.NullString
	if err := db.QueryRow("SELECT value FROM meta WHERE key = 'version'").Scan(&raw); err != nil {
		return 0
	}
	if !raw.Valid {
		return 0
	}
	version, err := strconv.ParseInt(strings.TrimSpace(raw.String), 10, 64)
	if err != nil || version < 0 {
		return 0
	}
	return version
}

// decryptV10Cookie decrypts a Chrome v10+ encrypted cookie value:
//
//	"v10" || nonce(12) || ciphertext || tag(16)
//
// The version-prefix branch on v10 vs v11 is purely informational —
// both use the same AES-GCM-with-12-byte-nonce-and-16-byte-tag layout.
// Chrome on Windows produces v10; Chrome on Linux / desktop-keystore
// configurations produces v11; Edge has been seen producing both.
//
// hashPrefix must come from chromeUsesHashPrefix(readChromeMetaVersion(db))
// for the profile the row was read from: at meta.version >= 24 the
// decrypted plaintext is `sha256(domain) || value` and the digest has to be
// dropped before the value is usable.
func decryptV10Cookie(masterKey, encrypted []byte, hashPrefix bool) (string, error) {
	if len(encrypted) == 0 {
		return "", nil
	}
	prefix := ""
	switch {
	case len(encrypted) >= 3 && string(encrypted[:3]) == chromeV10Prefix:
		prefix = chromeV10Prefix
	case len(encrypted) >= 3 && string(encrypted[:3]) == chromeV11Prefix:
		prefix = chromeV11Prefix
	case len(encrypted) >= 3 && string(encrypted[:3]) == chromeV20Prefix:
		return "", fmt.Errorf("cookie uses App-Bound Encryption (v20, Chrome 127+) which the DPAPI fallback cannot decrypt — use the auto-cookie browser setup instead")
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

	if hashPrefix {
		// A plaintext shorter than the digest cannot be carrying one. Slicing
		// anyway would report an empty value as a successful decrypt; erroring
		// makes ReadChromeCookies skip the row instead.
		if len(plaintext) < chromeDomainHashLen {
			return "", fmt.Errorf("decrypted cookie is %d bytes, shorter than the %d-byte domain hash prefix Chrome writes at meta.version >= %d",
				len(plaintext), chromeDomainHashLen, chromeHashPrefixMetaVersion)
		}
		plaintext = plaintext[chromeDomainHashLen:]
	}

	// AES-GCM already authenticated the plaintext, so invalid UTF-8 here does
	// not mean a wrong key — it means the bytes aren't a cookie value: most
	// likely an un-stripped domain hash from a profile whose meta.version
	// probe came back empty. Refusing the row (upstream drops it on
	// UnicodeDecodeError) keeps binary garbage out of cookies.txt.
	if !utf8.Valid(plaintext) {
		return "", fmt.Errorf("decrypted cookie value is not valid UTF-8 (%d bytes) — profile may use the Chrome 130+ domain hash prefix", len(plaintext))
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

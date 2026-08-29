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

// Why one cookie row failed to decrypt. The extraction is fail-soft per
// row, so these are never returned to the caller as-is — they exist so the
// skip can be COUNTED by reason. "Nothing came out" has four completely
// different causes here, and they need four different responses from the
// operator: App-Bound encryption means this fallback cannot work at all on
// that browser, a master-key mismatch means the profile belongs to another
// user, and a plaintext that isn't UTF-8 usually means the meta.version
// probe came back empty on a Chrome 130+ profile.
var (
	// ErrAppBoundEncryption marks a v20 (Chrome 127+) value, whose key is
	// held by a SYSTEM service and is not recoverable with CURRENT_USER
	// DPAPI. Not a malfunction — a hard capability limit.
	ErrAppBoundEncryption = errors.New("cookie uses App-Bound Encryption (v20, Chrome 127+)")

	// ErrLegacyEncryption marks a value with no v10/v11 prefix: a pre-2020
	// raw DPAPI blob, or not an encrypted value at all.
	ErrLegacyEncryption = errors.New("encrypted_value has no v10/v11 prefix")

	// ErrMasterKeyMismatch marks an AES-GCM open failure — the value is
	// v10/v11 but this profile's master key does not open it.
	ErrMasterKeyMismatch = errors.New("master key does not decrypt this cookie")

	// ErrUnusablePlaintext marks a value that decrypted but is not a usable
	// cookie value. The common cause is a Chrome 130+ profile whose
	// meta.version could not be read, leaving the 32-byte domain hash on
	// the front of the plaintext.
	ErrUnusablePlaintext = errors.New("decrypted value is not a usable cookie value")
)

// ChromeReadStats accounts for what one ReadChromeCookies pass skipped.
//
// The extraction is fail-soft per row by design, but before this existed the
// reason was discarded entirely: an operator saw only a final "no relevant
// cookies in any profile" and could not tell a failed meta.version probe
// from App-Bound encryption from a master-key mismatch. Upstream yt-dlp
// counts the same thing and reports "({failed_cookies} could not be
// decrypted)".
type ChromeReadStats struct {
	Rows            int   // rows considered (past the origin filter, if any)
	Decrypted       int   // rows whose value came out usable
	ScanFailed      int   // rows whose Scan failed
	Failed          int   // rows whose value could not be decrypted
	AppBound        int   // ... because of v20 App-Bound Encryption
	Legacy          int   // ... because there was no v10/v11 prefix
	KeyMismatch     int   // ... because AES-GCM would not open it
	UnusablePlain   int   // ... because the plaintext was not a cookie value
	Other           int   // ... for any reason not classified above
	MetaVersion     int64 // Cookies meta.version, or 0 when the probe failed
	MetaProbeFailed int   // databases whose meta.version could not be read
}

// Add folds another profile's counts into this one, so a multi-profile
// sweep can report a single picture. MetaVersion is per-database and is
// deliberately NOT merged — MetaProbeFailed is the part that composes.
func (s *ChromeReadStats) Add(other ChromeReadStats) {
	s.Rows += other.Rows
	s.Decrypted += other.Decrypted
	s.ScanFailed += other.ScanFailed
	s.Failed += other.Failed
	s.AppBound += other.AppBound
	s.Legacy += other.Legacy
	s.KeyMismatch += other.KeyMismatch
	s.UnusablePlain += other.UnusablePlain
	s.Other += other.Other
	s.MetaProbeFailed += other.MetaProbeFailed
}

// recordDecryptFailure classifies one per-row decrypt error.
func (s *ChromeReadStats) recordDecryptFailure(err error) {
	s.Failed++
	switch {
	case errors.Is(err, ErrAppBoundEncryption):
		s.AppBound++
	case errors.Is(err, ErrLegacyEncryption):
		s.Legacy++
	case errors.Is(err, ErrMasterKeyMismatch):
		s.KeyMismatch++
	case errors.Is(err, ErrUnusablePlaintext):
		s.UnusablePlain++
	default:
		s.Other++
	}
}

// Summary describes the skipped rows in one line, or "" when nothing was
// skipped. Written to be pasted into an error message or a log line.
func (s ChromeReadStats) Summary() string {
	if s.Failed == 0 && s.ScanFailed == 0 {
		return ""
	}
	parts := make([]string, 0, 5)
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(s.AppBound, "use App-Bound Encryption (v20, Chrome 127+), which this fallback cannot decrypt — use the auto-cookie browser setup instead")
	add(s.KeyMismatch, "did not open with this profile's master key")
	add(s.Legacy, "are legacy pre-v10 values")
	add(s.UnusablePlain, "decrypted to something that is not a cookie value"+s.hashPrefixHint())
	add(s.Other, "failed for another reason")
	add(s.ScanFailed, "could not be read from the database at all")

	return fmt.Sprintf("%d of %d cookie values could not be decrypted: %s",
		s.Failed+s.ScanFailed, s.Rows, strings.Join(parts, "; "))
}

// hashPrefixHint names the likeliest cause of unusable plaintext when the
// meta.version probe came back empty: the strip that would have removed
// Chrome 130+'s domain-hash prefix was gated off by that failed probe.
func (s ChromeReadStats) hashPrefixHint() string {
	if s.MetaProbeFailed == 0 {
		return ""
	}
	return " (the Cookies meta.version could not be read, so the Chrome 130+ domain-hash prefix was left in place)"
}

// ReadChromeCookies returns the decrypted cookies for a profile, discarding
// the per-row accounting. ReadChromeCookiesStats is the same call with the
// accounting kept.
func ReadChromeCookies(profilePath, originFilter string) ([]ChromeCookie, error) {
	cookies, _, err := ReadChromeCookiesStats(profilePath, originFilter)
	return cookies, err
}

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
// on current Chrome/Edge profiles.
//
// Re-checked 2026-08-29 (H6): the previous version of this comment claimed
// "Brave kept v10 and still works" — NO LONGER TRUE, and possibly never
// was. references/yt-dlp's cookies.py (@81ecd58, v2026.08.19) implements
// no App-Bound Encryption handling at all — on Windows it treats any
// non-"v10" prefix as a raw DPAPI blob, so it is silent on which browsers
// use v20 and cannot be used to confirm or deny the claim either way.
// The deciding evidence is xaitax/Chrome-App-Bound-Encryption-Decryption
// (github.com/xaitax/Chrome-App-Bound-Encryption-Decryption): its
// RESEARCH.md documents Brave running its OWN registered IElevator COM
// elevation service (IID 5A9A9462-2FA1-4FEB-B7F2-DF3D19134463, distinct
// from Chrome/Edge's A949CB4E-C4F9-44C4-B213-6BF8AA9AC69C) — i.e. Brave
// has its own App-Bound Encryption service, not a DPAPI-only fallback.
// Functional Brave support landed in that tool's v0.18.1 (dated 2026-01-24
// in its release notes). Vivaldi, by contrast, was reported still on v10
// DPAPI-only as of the same check.
//
// No code change follows from this: v20 detection here is prefix-based,
// not browser-name-based, so Brave's v20 cookies already hit
// ErrAppBoundEncryption and the AppBound counter exactly like Chrome's or
// Edge's — only this comment's claim was stale. Any future user-facing
// copy that says "Brave" and "DPAPI fallback" in the same sentence should
// be re-checked against this note before shipping.
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
// The second return says whether a version was actually read, so a probe
// that degraded can be reported instead of passing for a genuine version 0.
// The probe itself stays silent — it has no logger and should not grow one
// to say that a one-row lookup came back empty; ChromeReadStats carries the
// fact to a caller that does.
func readChromeMetaVersion(db *sql.DB) (int64, bool) {
	var raw sql.NullString
	if err := db.QueryRow("SELECT value FROM meta WHERE key = 'version'").Scan(&raw); err != nil {
		return 0, false
	}
	if !raw.Valid {
		return 0, false
	}
	version, err := strconv.ParseInt(strings.TrimSpace(raw.String), 10, 64)
	if err != nil || version < 0 {
		return 0, false
	}
	return version, true
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
		return "", fmt.Errorf("%w — the DPAPI fallback cannot decrypt it; use the auto-cookie browser setup instead", ErrAppBoundEncryption)
	default:
		return "", fmt.Errorf("%w (legacy DPAPI cookies are not supported)", ErrLegacyEncryption)
	}

	const nonceLen = 12
	const tagLen = 16
	if len(encrypted) < len(prefix)+nonceLen+tagLen {
		return "", fmt.Errorf("%w: v10/v11 ciphertext too short: %d bytes (want >= %d)",
			ErrUnusablePlaintext, len(encrypted), len(prefix)+nonceLen+tagLen)
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
		// AES-GCM authenticates, so a failure here is not "corrupt data" —
		// it is the wrong key, i.e. a profile that belongs to another user
		// or a Local State that has since been re-keyed.
		return "", fmt.Errorf("%w (AES-GCM open: %v)", ErrMasterKeyMismatch, err)
	}

	if hashPrefix {
		// A plaintext shorter than the digest cannot be carrying one. Slicing
		// anyway would report an empty value as a successful decrypt; erroring
		// makes ReadChromeCookies skip the row instead.
		if len(plaintext) < chromeDomainHashLen {
			return "", fmt.Errorf("%w: decrypted cookie is %d bytes, shorter than the %d-byte domain hash prefix Chrome writes at meta.version >= %d",
				ErrUnusablePlaintext, len(plaintext), chromeDomainHashLen, chromeHashPrefixMetaVersion)
		}
		plaintext = plaintext[chromeDomainHashLen:]
	}

	// AES-GCM already authenticated the plaintext, so invalid UTF-8 here does
	// not mean a wrong key — it means the bytes aren't a cookie value: most
	// likely an un-stripped domain hash from a profile whose meta.version
	// probe came back empty. Refusing the row (upstream drops it on
	// UnicodeDecodeError) keeps binary garbage out of cookies.txt.
	if !utf8.Valid(plaintext) {
		return "", fmt.Errorf("%w: not valid UTF-8 (%d bytes) — profile may use the Chrome 130+ domain hash prefix",
			ErrUnusablePlaintext, len(plaintext))
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

// Package cookies provides Netscape cookie file parsing, SAPISIDHASH generation,
// and cookie management for YouTube and Twitch authentication.
package cookies

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cookieJarLogger is the optional logger interface CookieJar uses to report
// malformed or skipped cookie lines. Kept optional so NewCookieJar callers
// that don't want diagnostics need not supply a logger.
type cookieJarLogger interface {
	Debug(msg string, args ...any)
}

// cookieEntry is one cookie's stored state: the value plus the two fields the
// jar needs to reason about identity and lifetime.
//
// Before this existed the jar kept a `cookies` map (name -> value) beside a
// `domains` map (name -> domain) that Load wrote and NOTHING ever read. The
// domain a row came from was therefore parsed, stored, and then unreachable to
// every accessor on the jar — which is why the Cookie header has never been
// scopeable by platform. One entry per cookie makes that fact reachable.
type cookieEntry struct {
	value  string
	domain string
	// expiry is Netscape field 5, a unix timestamp. 0 means "not expired by
	// this field" and covers BOTH a genuine session cookie and a row whose
	// expiry column does not parse — the same convention rowExpired already
	// uses, so the two agree by construction. Load CAPTURES this; it never
	// filters on it (see Load).
	expiry int64
}

// CookieJar parses and manages cookies from a Netscape-format cookie file.
type CookieJar struct {
	mu       sync.RWMutex
	cookies  map[string]cookieEntry // name -> entry
	filePath string
	logger   cookieJarLogger // optional; set via SetLogger
}

// Essential YouTube cookies needed for authentication.
var essentialYouTubeCookies = map[string]bool{
	"SAPISID": true, "__Secure-1PAPISID": true, "__Secure-3PAPISID": true,
	"SID": true, "HSID": true, "SSID": true, "APISID": true,
	"__Secure-1PSID": true, "__Secure-3PSID": true,
	"__Secure-1PSIDTS": true, "__Secure-3PSIDTS": true,
	"__Secure-1PSIDCC": true, "__Secure-3PSIDCC": true,
	"LOGIN_INFO": true, "VISITOR_INFO1_LIVE": true, "VISITOR_PRIVACY_METADATA": true,
	"YSC": true, "__Secure-ROLLOUT_TOKEN": true, "CONSENT": true, "PREF": true,
}

// Essential Twitch cookies.
var essentialTwitchCookies = map[string]bool{
	"auth-token": true, "twilight-user": true, "login": true, "name": true,
}

// NewCookieJar creates an empty CookieJar.
func NewCookieJar() *CookieJar {
	return &CookieJar{
		cookies: make(map[string]cookieEntry),
	}
}

// SetLogger attaches an optional logger that CookieJar uses to report
// diagnostic events like malformed lines in the cookie file. Safe to call
// before or after Load. Pass nil to clear.
func (j *CookieJar) SetLogger(logger cookieJarLogger) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.logger = logger
}

// Load reads and parses a Netscape cookie file.
//
// The file is fully read and parsed into a new map before the jar's live state
// is replaced. This way a transient read error (EIO, permission flip) cannot
// silently wipe authentication that was valid a moment ago — either the load
// fully succeeds and swaps in the new map, or the previous state is left
// intact. A not-exist file is still treated as an empty jar.
//
// Load CAPTURES the expiry column; it deliberately does NOT filter on it. Two
// reasons, both load-bearing:
//
//   - The autocookies layer detects credential loss by comparing what the jar
//     holds against what mergeCookieFiles produced, and merge DOES prune
//     expired rows (see rowExpired). That comparison only says anything
//     because the two disagree about expired rows. Make them agree here and
//     the signal vanishes.
//   - Dropping rows here would silently change what GetCookieHeader sends.
//
// The jar loads what the file says. Expiry is a diagnostic
// (ExpiredAuthCookies / AuthCookieHorizon), not a gate.
func (j *CookieJar) Load(filePath string) error {
	// Snapshot logger once; the field is protected by the mutex.
	j.mu.RLock()
	logger := j.logger
	j.mu.RUnlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No cookies file is OK; clear state so callers see an empty jar.
			j.mu.Lock()
			j.filePath = filePath
			j.cookies = make(map[string]cookieEntry)
			j.mu.Unlock()
			return nil
		}
		return fmt.Errorf("failed to read cookie file: %w", err)
	}

	cookies := make(map[string]cookieEntry)

	lines := strings.SplitSeq(string(data), "\n")
	for line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip comments, but NOT #HttpOnly_ lines (those are data)
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}

		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			// Malformed line — Netscape format requires exactly 7 tab-delimited fields.
			// Log at Debug so the file can be diagnosed without being spammy when
			// third-party tools leave trailing blank lines or stray comments.
			if logger != nil {
				logger.Debug("skipping malformed cookie line", "fields", len(parts))
			}
			continue
		}

		rawDomain := strings.TrimSpace(parts[0])
		domain := rawDomain
		if after, ok := strings.CutPrefix(domain, "#HttpOnly_"); ok {
			domain = after
		}

		// Netscape field 5 (parts[4]) is the expiry. Parsed with exactly
		// rowExpired's semantics — TrimSpace then ParseInt, and 0 on a parse
		// error — so "expired" means the same thing to the jar and to the
		// merge. Captured only; see the Load doc comment.
		expiry, expErr := strconv.ParseInt(strings.TrimSpace(parts[4]), 10, 64)
		if expErr != nil {
			expiry = 0
		}

		name := strings.TrimSpace(parts[5])
		// Join tab-separated remainder so a cookie whose value legitimately
		// contains a tab (rare, but permitted by some emitters) is preserved
		// instead of truncated to the first tab.
		value := strings.TrimSpace(strings.Join(parts[6:], "\t"))

		// Skip entries with empty domain or name
		if domain == "" || name == "" {
			continue
		}

		// Only include YouTube/Google and Twitch cookies. Use suffix-anchored
		// matchers so a malicious or hand-edited file entry like
		// ".fakegoogle.com.evil.tld" does not get treated as google.com.
		isYouTubeGoogle := isYouTubeDomain(domain) || isGoogleDomain(domain)
		isTwitch := isTwitchDomain(domain)

		if !isYouTubeGoogle && !isTwitch {
			continue
		}

		// Filter to essential cookies
		isGoogleAuth := isGoogleDomain(domain) && (name == "SID" || name == "HSID" ||
			name == "SSID" || name == "APISID" || name == "SAPISID" ||
			strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P"))
		isTwitchEssential := isTwitch && essentialTwitchCookies[name]

		if !essentialYouTubeCookies[name] && !isGoogleAuth && !isTwitchEssential {
			continue
		}

		// A name can arrive from several domains. The clause above admits
		// essentialYouTubeCookies with NO domain guard, so a .twitch.tv row named
		// SID lands under the bare name "SID" — the same slot Google's real auth
		// SID occupies, which arrives on a .google.com domain. Under the previous
		// rule (skip only when the incumbent is YouTube and the incoming is not)
		// neither of those is YouTube, so whichever row the FILE listed last won:
		// a stray Twitch-domain SID could displace a live Google auth cookie.
		//
		// Decide by a TOTAL order on the domain instead, so a set of rows loads
		// to the same jar under any permutation. See compareCookieDomains.
		//
		// Skip only when the incumbent ranks STRICTLY better. Rows that compare
		// equal are rows whose stored domain string is identical, i.e. true
		// duplicates: those keep today's last-wins behaviour. The property this
		// buys is permutation-invariance of a SET of rows, not a reordering of
		// duplicate-identical ones.
		if existing, exists := cookies[name]; exists && compareCookieDomains(existing.domain, domain) < 0 {
			continue
		}

		cookies[name] = cookieEntry{value: value, domain: domain, expiry: expiry}
	}

	j.mu.Lock()
	j.filePath = filePath
	j.cookies = cookies
	j.mu.Unlock()

	return nil
}

// domainTier ranks a cookie's domain by platform, lowest wins. This preserves
// the jar's original youtube-over-google preference and extends it to a third
// tier so google-vs-twitch stops being decided by file order.
//
// Load rejects any domain outside these three before the comparison is
// reached, so the default is unreachable in practice; it ranks worst rather
// than panicking so a future caller cannot turn a stray domain into a crash.
func domainTier(domain string) int {
	switch {
	case isYouTubeDomain(domain):
		return 0
	case isGoogleDomain(domain):
		return 1
	case isTwitchDomain(domain):
		return 2
	default:
		return 3
	}
}

// compareCookieDomains is a total order over the domains two rows sharing one
// cookie name can carry. Negative means a is the better carrier, positive
// means b is, and 0 means the two domain strings are identical — the only tie.
//
// Ordering, in priority:
//
//  1. Platform tier (youtube < google < twitch).
//  2. Fewer labels wins: ".youtube.com" (2) beats "www.youtube.com" (3).
//     Broader scope is the better carrier for a shared name.
//  3. Dot-prefixed wins over host-only: ".youtube.com" beats "youtube.com",
//     same reason — the leading dot is the Netscape include-subdomains flag.
//  4. Lexically smaller wins on the stored domain string. Arbitrary but
//     deterministic; this is the backstop that makes the order total.
//
// Rule 3 is currently SUBSUMED by rule 4 and is kept deliberately. '.' (0x2E)
// sorts below every character a hostname label may start with, so today the
// lexical backstop happens to agree with it on every input. It is written out
// anyway because the agreement is an accident of ASCII, not of intent: change
// rule 4 to compare the dot-stripped form — a plausible tidy-up — and without
// rule 3 ".youtube.com" and "youtube.com" would compare EQUAL, silently
// restoring the file-order dependence this function exists to remove.
//
// Compared on the STORED domain, i.e. after Load strips "#HttpOnly_" — so a
// row and its HttpOnly twin tie and fall to last-wins, exactly as before.
func compareCookieDomains(a, b string) int {
	if ta, tb := domainTier(a), domainTier(b); ta != tb {
		return ta - tb
	}
	if la, lb := domainLabelCount(a), domainLabelCount(b); la != lb {
		return la - lb
	}
	da, db := strings.HasPrefix(a, "."), strings.HasPrefix(b, ".")
	if da != db {
		if da {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// domainLabelCount counts the dot-separated labels of a cookie domain,
// ignoring the leading dot that only encodes include-subdomains.
func domainLabelCount(domain string) int {
	return strings.Count(strings.TrimPrefix(domain, "."), ".") + 1
}

// Reload reloads cookies from the same file.
func (j *CookieJar) Reload() error {
	j.mu.RLock()
	path := j.filePath
	j.mu.RUnlock()

	if path == "" {
		return nil
	}
	return j.Load(path)
}

// GetCookie returns a cookie value by name.
func (j *CookieJar) GetCookie(name string) string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.cookies[name].value
}

// cookieValueSanitizer strips characters that could enable header injection.
// cookieValueSanitizer strips characters that could enable header injection
// or silently corrupt the Cookie: header parser:
//   - \r / \n: classic header-injection vectors.
//   - ; , : cookie header field separators per RFC 6265; a value containing
//     these terminates the pair early and mis-routes everything after.
var cookieValueSanitizer = strings.NewReplacer("\r", "", "\n", "", ";", "", ",", "")

// sanitizeCookieValue strips characters that could enable header injection.
func sanitizeCookieValue(v string) string {
	return cookieValueSanitizer.Replace(v)
}

// GetCookieHeader returns a Cookie header string with all cookies.
//
// Pairs are emitted in a stable sorted-by-name order. Go's map iteration is
// deliberately randomized, which meant two successive calls could produce
// different Cookie headers for the same jar contents — a poor fit for
// SAPISIDHASH flows where an attacker-controlled reshuffle makes HTTP-level
// debugging painful and occasionally trips YouTube endpoints that inspect
// __Secure-* ordering. Alphabetical is simple and deterministic.
func (j *CookieJar) GetCookieHeader() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	names := make([]string, 0, len(j.cookies))
	for name := range j.cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+sanitizeCookieValue(j.cookies[name].value))
	}
	return strings.Join(pairs, "; ")
}

// HasYouTubeAuthCookies returns true if SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO are present.
func (j *CookieJar) HasYouTubeAuthCookies() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()

	hasSapisid := j.cookies["SAPISID"].value != "" || j.cookies["__Secure-3PAPISID"].value != ""
	hasLoginInfo := j.cookies["LOGIN_INFO"].value != ""
	return hasSapisid && hasLoginInfo
}

// youtubeAuthCookieNames are the cookies whose presence means "this install
// was configured for YouTube auth at some point". Deliberately broader than
// HasYouTubeAuthCookies' SAPISID+LOGIN_INFO pair: that pair answers "is
// there a complete working set right now", which is the wrong question for
// the auth-loss gate. A file holding SAPISID with LOGIN_INFO cleared is a
// CONFIGURED platform with BROKEN credentials — exactly the state worth
// reporting — and the narrower predicate reads it as never-configured.
//
// Every name here must also be in essentialYouTubeCookies above, or Load
// drops it and this predicate can never observe it. Pinned by
// TestAuthCookieNameListsDoNotDrift.
var youtubeAuthCookieNames = []string{
	"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID",
	"SID", "HSID", "SSID", "APISID",
	"__Secure-1PSID", "__Secure-3PSID",
	"LOGIN_INFO",
}

// HasAnyYouTubeAuthCookie reports whether the jar holds ANY YouTube/Google
// auth cookie with a non-empty value — i.e. whether this install was ever
// configured for YouTube auth, regardless of whether the set is still
// complete. See youtubeAuthCookieNames for why this is not
// HasYouTubeAuthCookies.
func (j *CookieJar) HasAnyYouTubeAuthCookie() bool {
	if j == nil {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, name := range youtubeAuthCookieNames {
		if j.cookies[name].value != "" {
			return true
		}
	}
	return false
}

// ExpiredAuthCookies reports how many of the jar's YouTube/Google auth cookies
// carry an expiry that has already passed at `now` (unix seconds).
//
// "Auth" is youtubeAuthCookieNames — the same deliberately-broad set
// HasAnyYouTubeAuthCookie reasons about, for the same reason: a half-dead
// session is exactly the state worth reporting, and the narrower
// SAPISID+LOGIN_INFO pair cannot see it.
//
// "Expired" is exactly rowExpired's rule: expiry > 0 && expiry < now. A jar of
// session cookies (expiry 0) therefore returns 0 — 0 is a live session cookie,
// not an ancient one.
//
// Diagnostic only. Nothing in the jar acts on this; Load does not filter.
func (j *CookieJar) ExpiredAuthCookies(now int64) int {
	if j == nil {
		return 0
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	count := 0
	for _, name := range youtubeAuthCookieNames {
		entry, ok := j.cookies[name]
		if !ok {
			continue
		}
		if entry.expiry > 0 && entry.expiry < now {
			count++
		}
	}
	return count
}

// AuthCookieHorizon returns the soonest non-zero expiry among the jar's
// YouTube/Google auth cookies, or 0 when none of them carries one.
//
// Same auth set as ExpiredAuthCookies. Zero is not a timestamp here: it means
// "no auth cookie in this jar has an expiry to run out", which is the honest
// answer for a jar of session cookies and for an empty jar alike.
func (j *CookieJar) AuthCookieHorizon() int64 {
	if j == nil {
		return 0
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	var soonest int64
	for _, name := range youtubeAuthCookieNames {
		entry, ok := j.cookies[name]
		if !ok || entry.expiry <= 0 {
			continue
		}
		if soonest == 0 || entry.expiry < soonest {
			soonest = entry.expiry
		}
	}
	return soonest
}

// twitchAuthCookieNames is the Twitch counterpart to youtubeAuthCookieNames.
//
// twilight-user earns its place because auth-token can disappear while it
// survives, by a path documented in-tree: the jar ignores cookie expiry but
// mergeCookieFiles prunes on it (see the comment above hadTWAuth in
// autocookies.go), so a lapsed auth-token can be pruned out of the file while
// the rest of the session's cookies are written back. What remains is a Twitch
// session that WAS configured and now holds no credential — the state the
// auth-loss gate has to be able to see. twilight-user is the signed-in user's
// own record, so it is evidence of configuration on its own merits.
//
// essentialTwitchCookies also keeps "login" and "name"; those are left out,
// and NOT for a cross-site reason — Load admits twitch.tv rows only (they
// reach the jar solely via isTwitchEssential), so another site's "login"
// cookie never gets in. They are out because nothing in-tree or in
// references/ establishes that Twitch sets them only for signed-in visitors,
// and this predicate drives a recovery pass plus an operator-facing alarm.
// auth-token and twilight-user are unambiguously artifacts of a signed-in
// session; those two are not, and an alarm raised on a guess is worse than
// one missed. Adding them is safe mechanically and would close the last
// silent Twitch state — do it if that assumption is ever confirmed.
var twitchAuthCookieNames = []string{"auth-token", "twilight-user"}

// HasAnyTwitchAuthCookie reports whether this install was ever configured for
// Twitch auth, as opposed to HasTwitchAuthCookies' "is the bearer token
// present right now". See twitchAuthCookieNames.
func (j *CookieJar) HasAnyTwitchAuthCookie() bool {
	if j == nil {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, name := range twitchAuthCookieNames {
		if j.cookies[name].value != "" {
			return true
		}
	}
	return false
}

// GetSapisid returns the SAPISID cookie value, falling back to __Secure-3PAPISID.
func (j *CookieJar) GetSapisid() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if v := j.cookies["SAPISID"].value; v != "" {
		return v
	}
	return j.cookies["__Secure-3PAPISID"].value
}

// YouTubeIdentity returns a stable, non-reversible fingerprint of WHICH
// Google account the jar currently holds — "" when it cannot tell.
//
// Both halves are required, and LOGIN_INFO is the load-bearing one.
//
// SAPISID identifies a Google SESSION, not an account: a multi-login browser
// holds several accounts under one cookie session sharing one SAPISID. That
// is precisely why internal/youtube/auth.go cannot rely on cookies alone to
// pick an account and has to send X-Goog-AuthUser (ytcfg SESSION_INDEX) and
// X-Goog-PageId (DELEGATED_SESSION_ID) alongside them. A fingerprint over
// SAPISID alone would therefore be blind to the exact remedy Moombox prints
// for a not-a-member failure — "switch the browser to the account that holds
// the membership, then export again" — because switching the active account
// rewrites LOGIN_INFO and leaves SAPISID untouched.
//
// LOGIN_INFO is the jar-local equivalent of the DataSyncID that
// internal/youtube already uses for this same "which account is this"
// question (see types.go / pot_binding.go). SAPISID stays in the mix so a
// full re-login also registers. Deliberately excluded: __Secure-1PSIDTS and
// SIDCC, which rotate constantly and would fire on every refresh cycle.
//
// A changed fingerprint is a hint, not proof — a same-account cookie rotation
// that touched either value reads as a change. That direction is cheap: the
// sweep it wakes retries a parked job once and re-parks it under the new
// value. The opposite error (missing a real account switch) is the bug this
// exists to prevent, so the fingerprint is deliberately biased toward
// sensitivity.
//
// Hashed rather than returned raw because this value is compared in code
// paths near logging and is persisted on job rows, while the cookies it
// derives from are the highest-value secrets the app holds. Callers must
// treat it as an opaque equality token — never as a credential, and never as
// something to display.
func (j *CookieJar) YouTubeIdentity() string {
	if j == nil {
		return ""
	}
	// ONE RLock covering both reads. Load swaps the whole cookie map under
	// Lock, so taking the lock twice (once inside GetSapisid, once for
	// LOGIN_INFO) could pair a SAPISID from the pre-Reload jar with a
	// LOGIN_INFO from the post-Reload one and fingerprint a state that never
	// existed. The interleaving is reachable: the worker's park path and the
	// refresh loop's Reload run on different goroutines.
	//
	// The SAPISID fallback is inlined rather than delegated for that reason —
	// KEEP IN SYNC with GetSapisid.
	j.mu.RLock()
	sapisid := j.cookies["SAPISID"].value
	if sapisid == "" {
		sapisid = j.cookies["__Secure-3PAPISID"].value
	}
	loginInfo := j.cookies["LOGIN_INFO"].value
	j.mu.RUnlock()

	if sapisid == "" || loginInfo == "" {
		return ""
	}
	// NUL separator: neither cookie may contain one, so no pair of distinct
	// (sapisid, loginInfo) inputs can concatenate to the same string.
	sum := sha256.Sum256([]byte(sapisid + "\x00" + loginInfo))
	return hex.EncodeToString(sum[:])
}

// GetSapisidCookies returns all SAPISID variants.
func (j *CookieJar) GetSapisidCookies() (sapisid, sapisid1p, sapisid3p string) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	sapisid = j.cookies["SAPISID"].value
	if sapisid == "" {
		sapisid = j.cookies["__Secure-3PAPISID"].value
	}
	sapisid1p = j.cookies["__Secure-1PAPISID"].value
	sapisid3p = j.cookies["__Secure-3PAPISID"].value
	return
}

// allowedSAPISIDHASHOrigins is the set of origins for which Moombox will
// generate a SAPISIDHASH Authorization header. Keeping this tight is
// defense-in-depth: if a bug or config path ever lets a caller supply an
// attacker-controlled origin (e.g. "https://evil.example"), we must not
// hand them a valid SAPISIDHASH bound to that origin — Google's auth uses
// the origin as a shared secret between client and server. The current
// in-tree callers only pass https://www.youtube.com, so tightening to an
// allowlist is zero-regression.
var allowedSAPISIDHASHOrigins = map[string]struct{}{
	"https://www.youtube.com":     {},
	"https://youtube.com":         {},
	"https://studio.youtube.com":  {},
	"https://music.youtube.com":   {},
	"https://www.youtubekids.com": {},
}

// GenerateAuthorizationHeader generates the full Authorization header with SAPISIDHASH variants.
// If origin is not a recognized YouTube origin, returns "" — callers must not
// use the result to authenticate against arbitrary origins.
func (j *CookieJar) GenerateAuthorizationHeader(origin string) string {
	if _, ok := allowedSAPISIDHASHOrigins[origin]; !ok {
		return ""
	}
	sapisid, sapisid1p, sapisid3p := j.GetSapisidCookies()

	// Capture timestamp once so all SID hashes in the same header share the same time
	now := time.Now().Unix()
	var parts []string

	if sapisid != "" {
		parts = append(parts, makeSidAuthorization("SAPISIDHASH", sapisid, origin, now))
	}
	if sapisid1p != "" {
		parts = append(parts, makeSidAuthorization("SAPISID1PHASH", sapisid1p, origin, now))
	}
	if sapisid3p != "" {
		parts = append(parts, makeSidAuthorization("SAPISID3PHASH", sapisid3p, origin, now))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " ")
}

func makeSidAuthorization(scheme, sid, origin string, unixTime int64) string {
	timestamp := strconv.FormatInt(unixTime, 10)
	hashInput := timestamp + " " + sid + " " + origin
	hash := fmt.Sprintf("%x", sha1.Sum([]byte(hashInput)))
	return fmt.Sprintf("%s %s_%s", scheme, timestamp, hash)
}

// GetTwitchAuthToken returns the Twitch auth-token cookie value.
func (j *CookieJar) GetTwitchAuthToken() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.cookies["auth-token"].value
}

// HasTwitchAuthCookies returns true if the Twitch auth-token is present.
func (j *CookieJar) HasTwitchAuthCookies() bool {
	return j.GetTwitchAuthToken() != ""
}

// IsEmpty returns true if no cookies are loaded.
func (j *CookieJar) IsEmpty() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.cookies) == 0
}

// GetFilePath returns the path to the cookie file.
func (j *CookieJar) GetFilePath() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.filePath
}

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

// Platform names one of the two cookie stores a caller can mean. The values
// are exactly the strings the config already uses for platform names
// (cfg.Cookies.Platforms / cfg.Cookies.ActivePlatforms, read by
// config.GetActivePlatforms), so a config string converts to a Platform
// without a translation table.
type Platform string

const (
	PlatformYouTube Platform = "youtube"
	PlatformTwitch  Platform = "twitch"
)

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
//
// One FILE, two in-memory jars. cookies.txt stays a single store holding every
// platform's rows and every write path keeps updating it in place; the split
// here is purely how the parsed state is REPRESENTED. Twitch does not need
// YouTube's cookies and YouTube does not need Twitch's, so they are kept apart.
//
// The split is not cosmetic. A single map keyed by bare cookie NAME cannot hold
// a .twitch.tv "SID" and a .google.com "SID" at once — they are the same map
// key — so one silently evicted the other, and the winner was whichever row the
// file happened to list last. Partitioning by domain at parse time makes the
// collision structurally impossible instead of arbitrating it.
type CookieJar struct {
	mu sync.RWMutex
	// youtube holds youtube.com and google.com rows; twitch holds twitch.tv
	// rows. Two named fields rather than map[Platform]map[string]cookieEntry:
	// there are exactly two platforms, and a map-of-maps only adds a
	// nil-inner-map failure mode.
	youtube  map[string]cookieEntry // name -> entry
	twitch   map[string]cookieEntry // name -> entry
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
		youtube: make(map[string]cookieEntry),
		twitch:  make(map[string]cookieEntry),
	}
}

// jarFor returns the map backing one platform, or nil for an unrecognised one.
// Callers must hold j.mu. A nil map is safe to read from and to range over, so
// every read accessor degrades to "empty" rather than panicking on a Platform
// value that does not name a jar.
func (j *CookieJar) jarFor(p Platform) map[string]cookieEntry {
	switch p {
	case PlatformYouTube:
		return j.youtube
	case PlatformTwitch:
		return j.twitch
	default:
		return nil
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
// The file is fully read and parsed into fresh per-platform maps before the
// jar's live state is replaced. This way a transient read error (EIO,
// permission flip) cannot silently wipe authentication that was valid a moment
// ago — either the load fully succeeds and swaps in both new maps, or the
// previous state is left intact.
//
// ENOENT is deliberately NOT on that list: a missing file loads as an EMPTY
// jar, both maps cleared. That is a ruling, not an oversight. Deleting
// cookies.txt is how an operator logs Moombox out, and keeping the last good
// session in memory until the next restart would make a deliberate delete do
// nothing observable — on the one path where "it forgot my account" is the
// point.
//
// The objection to that ruling is a race, and the race does not exist. Every
// writer of this file goes through writeFileAtomic (autocookies.go); the
// refresh service's own updateCookieFile ends there too. It writes a temp file
// and calls os.Rename, and never unlinks the destination first — so the only
// question is whether the rename itself can leave the name momentarily absent,
// and on both shipped platforms it cannot:
//
//   - Windows, verified against the go1.26.1 sources rather than assumed:
//     os.Rename → os.rename (src/os/file_windows.go) →
//     internal/syscall/windows.Rename
//     (src/internal/syscall/windows/syscall_windows.go), whose entire body is
//     one MoveFileEx(from, to, MOVEFILE_REPLACE_EXISTING). One syscall, no
//     DeleteFile ahead of it, so cookies.txt never transiently ceases to
//     exist. The traffic in fact runs the other way, on the open path Load
//     actually takes: os.ReadFile → os.Open → openFileNolog
//     (src/os/file_windows.go) → syscall.Open, which asks for
//     FILE_SHARE_READ|FILE_SHARE_WRITE and NOT FILE_SHARE_DELETE
//     (src/syscall/syscall_windows.go), so a Load in flight makes the RENAME
//     fail rather than being made to read a missing file — and that failure is
//     returned and logged by writeFileAtomic's caller instead of vanishing.
//     That is a property of THAT open, not of "os opens" generally: an
//     os.Root-rooted open goes through internal/syscall/windows.Openat
//     (src/internal/syscall/windows/at_windows.go), which DOES pass
//     FILE_SHARE_DELETE, so a Load rewritten onto os.Root would trade this
//     loud failure for a silent one and would need its own re-derivation.
//   - Linux: rename(2) replaces the destination name atomically, by POSIX.
//
// A concurrent Reload during a write therefore reads the old file or the new
// one, never no file. Re-verify this paragraph if a writer ever stops going
// through writeFileAtomic, or starts removing the target before renaming over
// it: that, and not this branch, is where an empty read would be introduced.
//
// Both maps are swapped under ONE Lock, so no reader can observe a jar holding
// the new YouTube rows beside the old Twitch ones. Accessors that need two
// values to agree must take a single RLock to match — see GetTwitchCredentials
// and YouTubeIdentity.
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
// (ExpiredAuthCookiesFor / AuthCookieHorizonFor), not a gate.
func (j *CookieJar) Load(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No cookies file is OK; clear state so callers see an empty jar.
			j.mu.Lock()
			j.filePath = filePath
			j.youtube = make(map[string]cookieEntry)
			j.twitch = make(map[string]cookieEntry)
			j.mu.Unlock()
			return nil
		}
		return fmt.Errorf("failed to read cookie file: %w", err)
	}

	j.loadFrom(data, filePath)
	return nil
}

// loadFrom is Load's parser, over bytes a caller already holds. Split out of
// Load — behaviour identical, the ONLY caller that does not come through Load
// is netscapeCookiesHoldACredential — so that a caller with the Netscape text
// in memory can ask the jar's own predicates about it without inventing a
// second parser or round-tripping through a temp file. The domain routing, the
// name admission and the total order on duplicate domains are subtle enough
// that a second reading of the same text would drift; there is one.
//
// filePath is recorded as the jar's origin exactly as Load records it. Pass ""
// for a jar that came from no file: GetFilePath then answers "" and Reload
// becomes a no-op, which is the honest answer for a throwaway jar built out of
// a buffer.
func (j *CookieJar) loadFrom(data []byte, filePath string) {
	// Snapshot logger once; the field is protected by the mutex.
	j.mu.RLock()
	logger := j.logger
	j.mu.RUnlock()

	youtube := make(map[string]cookieEntry)
	twitch := make(map[string]cookieEntry)

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

		// Admission is DOMAIN-FIRST, and that ordering is the fix rather than a
		// tidy-up. The rule used to read
		//
		//	if !essentialYouTubeCookies[name] && !isGoogleAuth && !isTwitchEssential { continue }
		//
		// whose first clause carries no domain guard at all. Every one of "SID",
		// "SAPISID", "__Secure-3PSID" and the rest is in essentialYouTubeCookies,
		// so a .twitch.tv row carrying one of those names was ADMITTED — and
		// then, in a single map keyed by bare name, it landed in the same slot as
		// Google's real auth cookie of that name and one of the two was lost.
		//
		// A three-tier domain comparator (youtube < google < twitch) was written
		// to decide which of the two survived. That was containment for a problem
		// the storage shape created. Deciding the platform first, and giving each
		// platform its own map, means the twitch-domain "SID" is never admitted
		// in the first place and there is nothing left to arbitrate.
		//
		// Suffix-anchored matchers throughout, so a hand-edited or malicious row
		// on ".fakegoogle.com.evil.tld" is not treated as google.com.
		var dest map[string]cookieEntry
		switch {
		case isYouTubeDomain(domain) || isGoogleDomain(domain):
			// The google.com auth names are admitted on google.com only; the
			// broader essential set is admitted on either YouTube or Google.
			isGoogleAuth := isGoogleDomain(domain) && (name == "SID" || name == "HSID" ||
				name == "SSID" || name == "APISID" || name == "SAPISID" ||
				strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P"))
			if !essentialYouTubeCookies[name] && !isGoogleAuth {
				continue
			}
			dest = youtube
		case isTwitchDomain(domain):
			if !essentialTwitchCookies[name] {
				continue
			}
			dest = twitch
		default:
			// Not a domain this jar tracks. isRelevantDomain agrees.
			continue
		}

		// Within ONE jar a name can still arrive from several domains — a
		// youtube.com and a google.com SAPISID, or ".youtube.com" beside
		// "www.youtube.com". That is a real question and it is settled by a
		// TOTAL order on the domain, so a set of rows loads to the same jar
		// under any permutation of the file. See compareCookieDomains.
		//
		// Skip only when the incumbent ranks STRICTLY better. Rows that compare
		// equal are rows whose stored domain string is identical, i.e. true
		// duplicates: those keep today's last-wins behaviour. The property this
		// buys is permutation-invariance of a SET of rows, not a reordering of
		// duplicate-identical ones.
		if existing, exists := dest[name]; exists && compareCookieDomains(existing.domain, domain) < 0 {
			continue
		}

		dest[name] = cookieEntry{value: value, domain: domain, expiry: expiry}
	}

	j.mu.Lock()
	j.filePath = filePath
	j.youtube = youtube
	j.twitch = twitch
	j.mu.Unlock()
}

// compareCookieDomains is a total order over the domains two rows sharing one
// cookie name can carry WITHIN ONE JAR. Negative means a is the better carrier,
// positive means b is, and 0 means the two domain strings are identical — the
// only tie.
//
// It is never asked a cross-platform question. Load routes each row to a jar by
// domain before this is reached, so the two domains compared here always belong
// to the same platform. The cross-platform rungs this function used to carry
// (a google-beats-twitch tier, and a fallback rank for an unreachable fourth
// domain class) existed only because one flat map forced rows from both
// platforms into the same key.
//
// Ordering, in priority:
//
//  1. youtube.com beats google.com. This one is REAL and survives the split:
//     Google auth cookies legitimately appear on both domains inside the
//     youtube jar, and preferring the YouTube-domain copy is long-standing
//     intended behaviour. In the twitch jar every domain is a twitch.tv domain,
//     so isYouTubeDomain is false for both sides and this rule cannot fire —
//     which is exactly the "no tier on the Twitch side" the split gives for
//     free, rather than as a second code path.
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
	if ya, yb := isYouTubeDomain(a), isYouTubeDomain(b); ya != yb {
		if ya {
			return -1
		}
		return 1
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

// GetCookieFor returns one platform's cookie value by name, or "" when that
// jar does not hold the name (or p names no jar).
func (j *CookieJar) GetCookieFor(p Platform, name string) string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.jarFor(p)[name].value
}

// GetCookie returns a TWITCH cookie value by name.
//
// The name reads generic; the behaviour is not, and the mismatch is deliberate
// rather than an oversight. This method has exactly ONE consumer in the tree —
// internal/twitch/auth.go, fetching "auth-token" — so it is a Twitch accessor
// in everything but spelling, and routing it to the YouTube jar to match its
// name would de-authenticate Twitch.
//
// That failure would be silent, which is why this comment is here: Twitch IRC
// treats an empty token as "log in anonymously" (PASS SCHMOOPIIE,
// internal/twitch/chat_irc.go) rather than as an error, so chat would keep
// connecting and would simply stop seeing subscriber-only messages and badges.
// Nothing would report a fault.
//
// Use GetCookieFor when you know which platform you mean.
func (j *CookieJar) GetCookie(name string) string {
	return j.GetCookieFor(PlatformTwitch, name)
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

// GetCookieHeaderFor returns a Cookie header string carrying one platform's
// cookies and no other platform's. An unrecognised Platform yields "".
//
// Pairs are emitted in a stable sorted-by-name order. Go's map iteration is
// deliberately randomized, which meant two successive calls could produce
// different Cookie headers for the same jar contents — a poor fit for
// SAPISIDHASH flows where an attacker-controlled reshuffle makes HTTP-level
// debugging painful and occasionally trips YouTube endpoints that inspect
// __Secure-* ordering. Alphabetical is simple and deterministic.
func (j *CookieJar) GetCookieHeaderFor(p Platform) string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	entries := j.jarFor(p)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+sanitizeCookieValue(entries[name].value))
	}
	return strings.Join(pairs, "; ")
}

// GetCookieHeader returns the YOUTUBE Cookie header.
//
// Every one of this method's production callers is a YouTube request path
// (internal/youtube, the YouTube worker strategies, and the refresh service's
// own YouTube probes), which is why the unqualified name means YouTube. Their
// behaviour is unchanged except that Twitch rows no longer ride along in the
// header — before the jar was partitioned, a Twitch "login" or "auth-token"
// cookie was emitted to youtube.com on every authenticated request.
func (j *CookieJar) GetCookieHeader() string {
	return j.GetCookieHeaderFor(PlatformYouTube)
}

// HasYouTubeAuthCookies returns true if SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO are present.
func (j *CookieJar) HasYouTubeAuthCookies() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()

	hasSapisid := j.youtube["SAPISID"].value != "" || j.youtube["__Secure-3PAPISID"].value != ""
	hasLoginInfo := j.youtube["LOGIN_INFO"].value != ""
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
		if j.youtube[name].value != "" {
			return true
		}
	}
	return false
}

// authCookieNamesFor is the "was this platform configured, and is its session
// still alive" name set for one platform. Nil for an unrecognised Platform, so
// every caller degrades to "nothing to report".
func authCookieNamesFor(p Platform) []string {
	switch p {
	case PlatformYouTube:
		return youtubeAuthCookieNames
	case PlatformTwitch:
		return twitchAuthCookieNames
	default:
		return nil
	}
}

// ExpiredAuthCookiesFor reports how many of one platform's auth cookies carry
// an expiry that has already passed at `now` (unix seconds).
//
// Per-platform rather than YouTube-only, and that matters most on the Twitch
// side. RefreshService rotates YouTube credentials in-process
// (checkAndRefreshYouTube / processYouTubeSetCookies); for Twitch it only has
// checkTwitchAuth, a check with no refresh, so this count is the earliest
// warning that a Twitch credential is running out. And a dead Twitch auth-token
// does not error — it downgrades chat capture to anonymous and quietly loses
// subscriber-only messages and badges. Folding Twitch into a single YouTube
// number would report expired=0 for exactly that state.
//
// "Auth" is authCookieNamesFor — the same deliberately-broad sets
// HasAnyYouTubeAuthCookie / HasAnyTwitchAuthCookie reason about, for the same
// reason: a half-dead session is exactly the state worth reporting, and the
// narrower "is the set complete" predicates cannot see it.
//
// "Expired" is exactly rowExpired's rule: expiry > 0 && expiry < now. A jar of
// session cookies (expiry 0) therefore returns 0 — 0 is a live session cookie,
// not an ancient one.
//
// Diagnostic only. Nothing in the jar acts on this; Load does not filter.
func (j *CookieJar) ExpiredAuthCookiesFor(p Platform, now int64) int {
	if j == nil {
		return 0
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	entries := j.jarFor(p)
	count := 0
	for _, name := range authCookieNamesFor(p) {
		entry, ok := entries[name]
		if !ok {
			continue
		}
		if entry.expiry > 0 && entry.expiry < now {
			count++
		}
	}
	return count
}

// AuthCookieHorizonFor returns the soonest non-zero expiry among one platform's
// auth cookies, or 0 when none of them carries one.
//
// Same auth sets as ExpiredAuthCookiesFor. Zero is not a timestamp here: it
// means "no auth cookie in this jar has an expiry to run out", which is the
// honest answer for a jar of session cookies and for an empty jar alike.
func (j *CookieJar) AuthCookieHorizonFor(p Platform) int64 {
	if j == nil {
		return 0
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	entries := j.jarFor(p)
	var soonest int64
	for _, name := range authCookieNamesFor(p) {
		entry, ok := entries[name]
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
// essentialTwitchCookies also keeps "login" and "name". Both are left out of
// this list, and NOT for a cross-site reason — Load admits twitch.tv rows only
// (they reach the jar solely via isTwitchEssential), so another site's "login"
// cookie never gets in.
//
// What changed since that was first written: "login" is no longer inert.
// internal/twitch SENDS it, as the IRC NICK (see ircHandshakeLines), so a live
// auth-token with no USABLE "login" beside it is a functional degradation this
// tree can NAME — chat captured anonymously, with no subscriber-only messages
// and no badges — rather than a guess about what Twitch means by the cookie.
// That degradation is now reported where it is observed, by a
// once-per-downloader Warn on the IRC path (ChatDownloader.noteMissingLogin)
// and a once-per-job report to the worker, which notifies. That is the right
// site for it: the consumer knows the handshake actually went anonymous,
// whereas this list can only know that a name is absent — and absent is not the
// same as unusable, which is state 2 below.
//
// "login" still does NOT join this list, and the reason is the alarm the list
// drives — traced end to end rather than assumed:
//
//	HasAnyTwitchAuthCookie → doRefresh's hasTWCookies (refresh.go)
//	  → shouldFireRecovery(…, cookiesPresent) → OnRecoveryNeeded("twitch")
//	  → runState.handleRecoveryNeeded (cmd/moombox/monitor_callbacks.go)
//
// The alarm does not require a failed validate, which is what makes adding a
// name here expensive. checkTwitchAuth returns a CONCLUSIVE (false, nil)
// WITHOUT issuing any request at all when auth-token is absent, and
// shouldFireRecovery's first-conclusive-check arm then returns cookiesPresent
// verbatim. A file holding "login" and no auth-token would therefore fire
// "twitch auth lost" on the first check of every start: a TypeError
// notification telling the operator to re-export credentials that may never
// have existed, or, with automatic refresh on, a headless-browser launch. That
// is exactly the "alarm raised on a guess" cost, and it survives unchanged —
// nothing in-tree or in references/ establishes that Twitch sets "login" only
// for signed-in visitors, and being CONSUMED when present says nothing about
// who it is set FOR.
//
// The silent Twitch states, ENUMERATED. "The last silent state" is a claim
// this comment has made and got wrong TWICE — first as a superlative, then as a
// list that was one entry short. A list can be checked; a superlative cannot,
// and neither can an unfinished one, so add to it rather than re-declaring it
// complete:
//
//  1. auth-token present, "login" absent. HasTwitchAuthCookies reads true and
//     both UIs show green, while IRC goes anonymous WITHOUT ATTEMPTING the
//     login — so no refusal happens and the chat fallback's Warn never runs. A
//     minimal hand-written cookies.txt lands here on day one. No longer
//     silent: noteMissingLogin logs it, once per downloader. Not fixed by this
//     list, which already reads such a jar as configured.
//  2. auth-token present, "login" present but not sendable as a single IRC
//     parameter — it holds a space, tab, CR, LF or NUL (see
//     twitch.hasRowBreakingChar). ircHandshakeLines throws such a value away
//     and renders the full anonymous pair, so this reaches the wire exactly
//     as state 1 does while looking, to every predicate here, like a complete
//     credential pair. A hand-edited cookies.txt whose login row was filled in
//     with a display name — "archiver account" — lands here. No longer silent:
//     noteMissingLogin's condition is login-UNUSABLE, not login-absent, so it
//     covers both. This entry is why that condition is not `login == ""`.
//  3. "login" present, auth-token absent. Reads as never-configured here, and
//     is left that way deliberately — see the alarm trace above. The reachable
//     way to produce it is mergeCookieFiles pruning an expired auth-token, and
//     whenever the export also carried twilight-user (Twitch's own client sets
//     it on login) that case is already covered by the name above.
//  4. "name" present alone. Same reasoning as 3, on thinner evidence still.
//
// Add "login" here only together with a change that stops checkTwitchAuth's
// empty-token early return from reading as a conclusive negative. That early
// return is the alarm's precondition, and it is not this list's to fix.
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
		if j.twitch[name].value != "" {
			return true
		}
	}
	return false
}

// GetSapisid returns the SAPISID cookie value, falling back to __Secure-3PAPISID.
func (j *CookieJar) GetSapisid() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if v := j.youtube["SAPISID"].value; v != "" {
		return v
	}
	return j.youtube["__Secure-3PAPISID"].value
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
	sapisid := j.youtube["SAPISID"].value
	if sapisid == "" {
		sapisid = j.youtube["__Secure-3PAPISID"].value
	}
	loginInfo := j.youtube["LOGIN_INFO"].value
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

	sapisid = j.youtube["SAPISID"].value
	if sapisid == "" {
		sapisid = j.youtube["__Secure-3PAPISID"].value
	}
	sapisid1p = j.youtube["__Secure-1PAPISID"].value
	sapisid3p = j.youtube["__Secure-3PAPISID"].value
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
	return j.twitch["auth-token"].value
}

// HasTwitchAuthCookies returns true if the Twitch auth-token is present.
func (j *CookieJar) HasTwitchAuthCookies() bool {
	return j.GetTwitchAuthToken() != ""
}

// GetTwitchCredentials returns the auth-token and the "login" account name it
// belongs to, read together under ONE RLock.
//
// They are returned as a pair because they are USED as a pair, and because a
// pair read under two locks is not a pair. internal/twitch/chat_irc.go builds
// one IRC handshake out of both — `PASS oauth:<token>` binds the session to a
// user and `NICK <login>` names that user — so a token from one account beside
// a login from another authenticates as neither. That interleaving is
// reachable: Reload swaps the jar's maps under Lock from the refresh loop, the
// Twitch service and the YouTube auth path, all on goroutines other than the
// one running the handshake, and a reconnect hours into a stream re-reads both
// values at exactly that moment.
//
// This is the same discipline YouTubeIdentity uses for the same reason (see the
// ONE-RLock note there): two reads that must describe one session cannot be two
// calls.
//
// Read from the TWITCH jar, so both come from the same rows. There is no
// platform parameter because neither cookie exists on another platform. Either
// half may be "" — a jar with a token and no login is not authenticated, and
// chat_irc.go treats it as fully anonymous rather than as half a session.
//
// Never log, print or persist either value: one is a credential and the other
// names the signed-in account.
func (j *CookieJar) GetTwitchCredentials() (token, login string) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.twitch["auth-token"].value, j.twitch["login"].value
}

// TwitchIdentity returns a stable, non-reversible fingerprint of WHICH Twitch
// credential pair the jar currently holds — "" only when it holds neither
// half.
//
// The counterpart of YouTubeIdentity, and deliberately NOT its rule.
// YouTubeIdentity requires BOTH halves and returns "" if either is missing,
// because its question is "which Google ACCOUNT is this" and a SAPISID without
// LOGIN_INFO cannot answer it. The question HERE is "is this the same
// credential PAIR the chat downgrade was observed under", and an auth-token
// with no login beside it is not an unanswerable state — it is one of the four
// routes a job with credentials goes anonymous by (twitch.AuthDowngradeNo-
// LoginCookie). Folding it to "" would make the operator's fix, adding the
// login row, compare equal to the broken state it replaced: the auth mark
// would never clear and the live chat session would never re-authenticate. So
// "" means "no Twitch credentials at all" and nothing else.
//
// A changed fingerprint is a HINT, not proof, in the same direction
// YouTubeIdentity chose: Twitch rotates auth-token on its own schedule, so a
// same-account rotation reads as a change. That direction is cheap — one
// re-check and one IRC reconnect, both of which the credentials will pass. The
// opposite error, missing a real credential change, strands a capture in
// anonymous chat for the rest of the job.
//
// Hashed rather than returned raw because this value is compared in code paths
// near logging and is held on a RefreshService field, while both inputs are
// among the highest-value secrets the app holds: one is a bearer token, the
// other names the signed-in account. Callers must treat it as an opaque
// equality token — never as a credential, and never as something to display.
func (j *CookieJar) TwitchIdentity() string {
	if j == nil {
		return ""
	}
	// ONE RLock covering both reads, for the reason GetTwitchCredentials
	// documents at length: Load swaps the whole map under Lock, so two
	// separate locks could pair a token from the pre-Reload jar with a login
	// from the post-Reload one and fingerprint a pair that never existed.
	j.mu.RLock()
	token := j.twitch["auth-token"].value
	login := j.twitch["login"].value
	j.mu.RUnlock()

	if token == "" && login == "" {
		return ""
	}
	// NUL separator: neither cookie may contain one (rowBreakingChars covers
	// tab, CR, LF and NUL), so no pair of distinct (token, login) inputs can
	// concatenate to the same string.
	sum := sha256.Sum256([]byte(token + "\x00" + login))
	return hex.EncodeToString(sum[:])
}

// IsEmpty returns true when NEITHER platform's jar holds a cookie. A file that
// configured only one platform is not an empty jar.
func (j *CookieJar) IsEmpty() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.youtube) == 0 && len(j.twitch) == 0
}

// GetFilePath returns the path to the cookie file.
func (j *CookieJar) GetFilePath() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.filePath
}

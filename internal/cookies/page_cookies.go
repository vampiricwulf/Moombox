package cookies

import (
	"net/url"
	"sort"
	"strings"
	"time"
)

// The consent and language yt-dlp puts on its cookie jar before any YouTube
// request (_real_initialize, yt_dlp/extractor/youtube/_base.py:700-704), sent
// here on the Cookie header of YouTube's HTML pages only — the watch page,
// the homepage, the embed page, the liveness and membership pages and chat's
// continuation page. Two cookies:
//
//   - SOCS, the consent: CAI ("accept all") for a session that is not signed
//     in, unless it already holds a SOCS that is not CAA, the not-consented
//     value (_initialize_consent, _base.py:675-681; yt-dlp moved to it from
//     CONSENT=YES+ in 378ae9f9f, #7774, when CONSENT stopped passing the
//     wall). Without it an EU, EEA or UK address is redirected to
//     consent.youtube.com, whose 200 carries no ytcfg: no player URL, STS or
//     visitor data, and no chat continuation.
//   - PREF with hl=en and tz=UTC merged in, every other key the jar's PREF
//     holds kept (_initialize_pref, _base.py:683-692: hl since f3aa3c3f9,
//     #1018; tz since 396a76f7b, #2402). A PREF hl picked in the browser
//     outranks Accept-Language, and the watch page's player response is
//     read by English keyword (the members-only verdict, the age gate,
//     "original" audio tracks): a Japanese PREF read "not a member" as
//     "Login required".
//
// Header only (owner ruling D1, 2026-10-31): nothing is written back to the
// jar or to cookies.txt, so a refresh or an export carries the user's cookies
// exactly as they were. yt-dlp sets both on its in-memory jar, which every
// request then carries, the Innertube calls too; Moombox keeps them off the
// Innertube calls, whose contexts state hl, timeZone and utcOffsetMinutes
// themselves (constants.go) — the same three _extract_context forces
// (_base.py:753-759).

const (
	// socsAcceptAll is the SOCS value yt-dlp sets: "accept all (required
	// for mixes)", _base.py:681.
	socsAcceptAll = "CAI"
	// socsNotConsented is the prefix of a SOCS that records no consent, which
	// yt-dlp replaces as if there were none (_base.py:679).
	socsNotConsented = "CAA"
)

// YouTubePageCookieHeader is the Cookie header for a YouTube HTML page: the
// jar's YouTube cookies, as GetCookieHeader sends them, with the consent and
// language cookies above applied — SOCS=CAI when the jar holds no complete
// signed-in set (HasYouTubeAuthCookies, this port's _has_auth_cookies; see
// hasYouTubeAuthLocked for where the two differ) and no consented SOCS, and
// PREF carrying hl=en and tz=UTC. A signed-in jar's own SOCS, whatever it
// holds, goes out as it is: yt-dlp leaves a signed-in session's consent
// alone. Never empty: PREF is always sent. Callers that must know whether
// the jar holds anything to send ask GetCookieHeader.
//
// The jar's SOCS and PREF are read as yt-dlp reads them (youtubeSees): one
// scoped to google.com, or expired, is not the jar's, and is left off the
// header — so CAI and a fresh PREF take their places, as upstream's would.
//
// One read lock over the whole build, so the auth verdict and the cookies
// emitted are the same jar contents.
func (j *CookieJar) YouTubePageCookieHeader() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	now := time.Now().Unix()
	pairs := make(map[string]string, len(j.youtube)+2)
	for name, e := range j.youtube {
		if pageCookieNames[name] && !youtubeSees(e, now) {
			continue
		}
		pairs[name] = e.value
	}
	applyPageCookies(pairs, j.hasYouTubeAuthLocked())
	return joinCookiePairs(pairs)
}

// YouTubeConsentCookieHeader is the consent and language cookies alone — SOCS
// and PREF, the jar's own where it holds them, applied as above for a request
// that is not signed in — for a page fetched deliberately without the jar's
// session (GetVideoInfoPublic's watch page). Neither cookie is a credential,
// so the page stays as anonymous as it was; and since no auth cookie rides on
// it, the request is signed out whatever the jar holds, so SOCS is always
// sent: the jar's when it is consented, CAI otherwise.
//
// The jar's SOCS and PREF are read as YouTubePageCookieHeader reads them
// (youtubeSees). On an empty jar this is constants.SignedOutYouTubePageCookie,
// the pair the pages that never see the jar carry.
func (j *CookieJar) YouTubeConsentCookieHeader() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	now := time.Now().Unix()
	pairs := make(map[string]string, 2)
	for name := range pageCookieNames {
		if e, ok := j.youtube[name]; ok && youtubeSees(e, now) {
			pairs[name] = e.value
		}
	}
	applyPageCookies(pairs, false)
	return joinCookiePairs(pairs)
}

// pageCookieNames are the two cookies applyPageCookies decides on.
var pageCookieNames = map[string]bool{"SOCS": true, "PREF": true}

// youtubeSees reports whether yt-dlp's read of the jar's YouTube cookies
// would hold e. _initialize_consent and _initialize_pref read
// self._youtube_cookies, _get_cookies('https://www.youtube.com')
// (_base.py:777-779, the property since 8cb08028f, #13014; the read itself
// is older), which is YoutubeDLCookieJar.get_cookie_header for that URL
// (yt_dlp/cookies.py:1405-1409): http.cookiejar's add_cookie_header, whose
// policy returns only a cookie whose domain www.youtube.com domain-matches
// and that has not expired. Two kinds of the jar's rows fail that read:
//
//   - one scoped to google.com (or to a YouTube host www is not under, such
//     as music.youtube.com). Load admits the essential names on a YouTube or
//     a Google domain into one map, so with no .youtube.com copy beside it,
//     google.com's own consent (an EU browser that accepted Google's but
//     never YouTube's) or PREF stands in the slot. Upstream never sees
//     either, and sets CAI and a fresh PREF.
//   - one whose expiry has passed (rowExpired's rule: expiry > 0 and before
//     now). Load keeps expired rows on purpose (see Load), so a cookies.txt
//     exported a year ago still holds the consent it had then; no browser,
//     and no request of upstream's, would send it.
//
// Only the two cookies the page decides on are read this way. Every other
// cookie goes out as GetCookieHeader sends it, google.com's auth names
// included, which is the jar's contract with every YouTube request.
//
// The domain read is RFC 6265's domain-match from the host's side: the row's
// domain, leading dot dropped, is www.youtube.com or a domain it is under.
func youtubeSees(e cookieEntry, now int64) bool {
	domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e.domain)), ".")
	return domainMatches("www.youtube.com", domain) && !(e.expiry > 0 && e.expiry < now)
}

// applyPageCookies applies the consent (unless signedIn) and the language to
// a page's name→value cookie pairs, in place.
//
// An empty SOCS counts as none. yt-dlp keeps one — its test is the cookie
// object's truth, which an empty value does not change — but an empty value
// records no consent, and the wall it would leave standing is what SOCS is
// sent to pass.
func applyPageCookies(pairs map[string]string, signedIn bool) {
	if socs := pairs["SOCS"]; !signedIn && (socs == "" || strings.HasPrefix(socs, socsNotConsented)) {
		pairs["SOCS"] = socsAcceptAll
	}
	pairs["PREF"] = englishPref(pairs["PREF"])
}

// joinCookiePairs renders name→value pairs as a Cookie header, sorted by name
// and sanitised exactly as GetCookieHeaderFor renders the jar.
func joinCookiePairs(pairs map[string]string) string {
	names := make([]string, 0, len(pairs))
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+sanitizeCookieValue(pairs[name]))
	}
	return strings.Join(out, "; ")
}

// prefField is one key and value of a PREF cookie.
type prefField struct{ key, value string }

// englishPref is _initialize_pref (_base.py:683-692) over a PREF value: read
// it as a query string, set hl=en and tz=UTC, keep every other key, and
// re-encode. yt-dlp's read is dict(urllib.parse.parse_qsl(value)), mirrored
// field for field: '&' separates, a field with no '=' or an empty value is
// dropped (keep_blank_values=False), '+' is a space, a key seen twice keeps
// its first place and its last value. Its write is urlencode, quote_plus on
// each side, which url.QueryEscape matches byte for byte over valid UTF-8 —
// the only kind a PREF YouTube wrote holds (it is ASCII). An escape of bytes
// that are not UTF-8 is where the two part: Python's unquote decodes with
// errors='replace', so %ff comes back %EF%BF%BD, where this keeps %FF, the
// value the user's PREF actually held. hl and tz keep their places when the
// PREF has them and are appended, hl first, when it has not: a Python dict's
// update.
//
// A PREF that cannot be read is replaced by hl=en&tz=UTC alone, yt-dlp's
// except arm. The difference is what cannot be read: Python's unquote leaves
// a malformed %-escape as it is, so that arm is all but unreachable upstream,
// where Go's QueryUnescape refuses one. A PREF YouTube wrote never holds one;
// a PREF that does is not one YouTube would read either, and re-encoding the
// literal '%' (Python's outcome) would hand it a value it never set.
func englishPref(jarPref string) string {
	fields, ok := parsePref(jarPref)
	if !ok {
		fields = nil
	}
	fields = setPrefField(fields, "hl", "en")
	fields = setPrefField(fields, "tz", "UTC")
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, url.QueryEscape(f.key)+"="+url.QueryEscape(f.value))
	}
	return strings.Join(parts, "&")
}

// parsePref is dict(urllib.parse.parse_qsl(v)) in field order (englishPref),
// or false when a key or value is not a valid query escape.
func parsePref(v string) ([]prefField, bool) {
	var fields []prefField
	at := map[string]int{}
	for raw := range strings.SplitSeq(v, "&") {
		rawKey, rawValue, hasEq := strings.Cut(raw, "=")
		if !hasEq || rawValue == "" {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, false
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, false
		}
		if i, seen := at[key]; seen {
			fields[i].value = value
			continue
		}
		at[key] = len(fields)
		fields = append(fields, prefField{key: key, value: value})
	}
	return fields, true
}

// setPrefField sets key to value where the PREF holds it, or appends it.
func setPrefField(fields []prefField, key, value string) []prefField {
	for i := range fields {
		if fields[i].key == key {
			fields[i].value = value
			return fields
		}
	}
	return append(fields, prefField{key: key, value: value})
}

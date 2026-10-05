package cookies

// refresh_cookiefile.go — the Netscape cookie-file rewrite rules:
// parsing one Set-Cookie header (admitSetCookie) and applying it
// against the file's existing rows (updateCookieFile).

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxCookieLifetime caps how far into the future a Max-Age may push a row's
// expiry: 400 days, which is RFC 6265bis §5.5's limit and what Chrome, Edge and
// Safari already enforce. A browser-exported cookies.txt therefore never carries
// a longer one, so the clamp brings this writer into line with the rest of the
// file rather than shortening anything the file could otherwise hold.
//
// It is also what makes the arithmetic safe. `now + maxAge` on an int64 wraps:
// Max-Age=9223372036854775807 parses fine and produces a large NEGATIVE expiry,
// which lands in Netscape field 5 — and every expiry guard in this package is
// `exp > 0 && exp < now` (rowExpired, CookieJar.Load's capture,
// ExpiredAuthCookiesFor), so a negative value reads as "not expired" everywhere.
// The row would be unprunable AND invisible to the freshness accounting, and
// yt-dlp's loader would reject it outright on its `[0-9]+` expires match.
// Clamping the addend below the cap makes the overflow unreachable rather than
// detected.
//
// Deliberately applied to Max-Age ONLY. Expires stays exactly as it is: real
// Google auth cookies carry multi-year Expires values, and clamping those would
// shorten what ExpiredAuthCookiesFor and AuthCookieHorizonFor report about a
// session this code did not actually change.
const maxCookieLifetime = int64(400 * 24 * 60 * 60)

// rowBreakingChars are the bytes a Netscape cookie row cannot carry inside any
// of its fields: tab is the field separator, CR and LF end the row, and NUL is
// not representable in a line-oriented text file at all.
//
// Only the TAB is a live vector, and the distinction is worth stating so nobody
// later reads this as a defence against header injection. Go's HTTP client
// cannot hand this package a CR, LF or NUL inside a header value at all:
// net/textproto's validHeaderValueByte admits only VCHAR, SP, HTAB and %x80-FF,
// and readMIMEHeader fails the entire header block otherwise — so
// resp.Header.Values("Set-Cookie") can never carry one. They are checked anyway
// because this predicate guards a WRITE into a line-oriented file, and the cost
// of the belt is one ContainsAny over strings that are already in cache.
const rowBreakingChars = "\t\r\n\x00"

// hasRowBreakingChar reports whether s would corrupt the row it is written into.
func hasRowBreakingChar(s string) bool { return strings.ContainsAny(s, rowBreakingChars) }

// trackedCookieName reports whether name is one the cookie file tracks for the
// declared origin's credential platform. It is the admission set for an
// UNSCOPED Set-Cookie — one with no Domain= of its own to be judged on.
//
// The set is the union of the two name predicates the rest of this package
// already uses for the Google platform: essentialYouTubeCookies (the names
// CookieJar.Load keeps) and isGoogleOnlyAuthName. Neither contains the other —
// isGoogleOnlyAuthName matches the whole __Secure-1P/3P families,
// essentialYouTubeCookies names PREF, CONSENT, YSC and the rest — so the union
// is exactly "a name this package can store", and a random unscoped foo=bar is
// not one.
//
// isGoogleOnlyAuthName is used here as a NAME predicate and nothing more. It
// used to double as updateCookieFile's domain-inventor for a Domain-less update
// and no longer does; see the insertion loop for why that was wrong.
//
// Only the Google platform has a set here, and the reason is now simply that
// only a Google-platform caller exists. Until this fix round there was a
// structural reason as well — updateCookieFile invented a Domain-less update's
// domain from the cookie NAME and knew only youtube.com and google.com, so an
// admitted Twitch cookie would have been refused at the write — but that
// fallback now uses the declared origin's own site and would place a .twitch.tv
// row correctly. What remains is that a new admission surface is not something
// to open speculatively. Adding essentialTwitchCookies here is a one-line change
// the day a Twitch caller arrives, and it needs that caller's tests, not a
// guess.
func trackedCookieName(name string, origin cookieOrigin) bool {
	if origin.platform() != originYouTube.platform() {
		return false
	}
	return essentialYouTubeCookies[name] || isGoogleOnlyAuthName(name)
}

// admitSetCookie parses ONE raw Set-Cookie header from a response that came from
// origin and decides whether it may become a pending cookie-file update. It is
// the OUTER admission layer: a header it turns down never reaches the write path
// in any form.
//
// B2. This loop used to open with a substring pre-filter — the header had to
// mention "youtube.com" or "google.com" somewhere — and it ran BEFORE Domain=
// was parsed. It was wrong in both directions at once:
//
//   - It dropped every legitimate unscoped rotation. RFC 6265 §4.1.2.3: a
//     Set-Cookie with no Domain= is host-scoped to the responding host, which is
//     an ordinary way for youtube.com to rotate its own first-party cookies.
//     Such a header contains neither substring, so it never reached the parser —
//     while the rest of this package plainly expected it to. resolveRowUpdate's
//     rule 2 exists to confine a Domain-less deletion, and updateCookieFile's
//     insertion loop has a whole branch for a Domain-less update; both were
//     unreachable. That is the "cookies.txt untouched for a day" symptom: it
//     fails safe, and it still strands the session. (The insertion branch had
//     rotted while it was dead — it invented a domain from the cookie NAME —
//     and had to be corrected when this commit made it live.)
//   - It was never the guard it looked like. `x=youtube.com; Domain=evil.tld`
//     passes a substring test on its VALUE. The real guard has always been the
//     parsed-Domain= check below, which its own comment already claimed it was —
//     true only for headers that carried a Domain= at all.
//
// Admission is therefore by what the header SAYS, in three steps, after the
// whole attribute list has been read:
//
//  1. Row-breaking characters in the NAME, the VALUE or the DOMAIN are refused
//     outright, before either branch. A tab splits the Netscape row into the
//     wrong fields on the next Load; CR, LF and NUL are checked as belt (see
//     rowBreakingChars — Go's header parser cannot deliver them). The VALUE is
//     included because RFC 6265's cookie-octet excludes HTAB and every control
//     character, and browsers reject them, so no legitimate rotation carries one
//     — a tab in a value is a malformed header, not a shape worth preserving.
//     CookieJar.Load's tolerance of tab-carrying rows is a separate question and
//     is unchanged: it must keep reading whatever a browser export or a
//     third-party tool already wrote into the file. This rule governs only what
//     THIS writer may add.
//  2. SCOPED (Domain= present): admitted only when the domain lies on the
//     declared origin's credential platform. For originYouTube that is precisely
//     `isYouTubeDomain || isGoogleDomain` — the test this branch has always made,
//     since those two are one platform and nothing is both Google and Twitch — so
//     the only caller's behaviour is unchanged. accounts.google.com.evil.tld,
//     evil.tld, a bare "." and a value that merely contains "youtube.com" are all
//     refused here.
//  3. UNSCOPED (no Domain=): the key keeps Domain "" and stays host-scoped to the
//     declared origin, which resolveRowUpdate rule 2, sameCookiePlatform and the
//     insertion loop each enforce downstream. This is the newly-reachable surface,
//     so it is also the narrow one: admitted only under a name the jar actually
//     tracks (trackedCookieName), which keeps an unscoped foo=bar out of the file.
//
// Why none of this widens what a hostile header can reach:
//
//   - A scoped header can only land on the declared platform's domains (2).
//   - An unscoped header is admitted only under a tracked name (3), and what it
//     may then DO is scoped by verb — see WHAT AN ADMITTED HEADER MAY DO, BY
//     VERB below. In particular an unscoped DELETION still cannot reach
//     .google.com auth from a youtube.com reply: that was already true, and it
//     is now true and reachable rather than true and dead.
//   - Row-breaking characters cannot reach the file (1).
//   - Everything downstream is untouched: updateCookieFile's refusal to blank an
//     essential cookie, the seven-field rebuild, writeFileAtomic.
//
// WHAT AN ADMITTED HEADER MAY DO, BY VERB. The design first, because the rules
// below are only its enforcement and reading them in isolation gives the wrong
// shape: YOUTUBE COOKIES SHOULD ALLOW GOOGLE COOKIES AS WELL. YouTube and Google
// are ONE credential platform — .google.com and .youtube.com rows live in one
// jar, keyed by bare cookie name — so a youtube.com reply is ENTITLED to move
// Google rows, and does:
//
//   - a YouTube reply sending a cookie explicitly scoped `Domain=.google.com` is
//     admitted, and CREATES that Google row if the file holds none;
//   - an unscoped rotation from YouTube REFRESHES existing Google rows of the
//     same name, when it is the only candidate (rule 3's disambiguation, below).
//
// The one thing declined is MISATTRIBUTION. A host-only cookie from
// www.youtube.com carrying no Domain= is, per RFC 6265 §4.1.2.3, a youtube.com
// cookie — so a NEW row for it goes on .youtube.com and is not invented on
// .google.com. As observed, Google's own cookies carry an explicit Domain= (that
// is a fact about Google's servers, not one this code can enforce), so nothing
// real is caught by that: the retired branch that guessed .google.com from the
// cookie NAME was minting a DIFFERENT cookie under a real one's name.
//
// Three verbs, and their scopes are not one scope. Written out here because no
// single downstream rule states it — each verb is enforced somewhere else, so
// reading any one of them gives the wrong answer about the other two.
//
// REFRESH — an unscoped header may rewrite an existing same-name row anywhere
// inside the declared origin's PLATFORM. An unscoped `SID=fresh` from a
// youtube.com reply DOES rewrite an existing `.google.com SID` row. Two rules
// carry that between them: resolveRowUpdate's rule 2 takes the rows the
// origin's own site covers, and rule 3 takes the rest of the
// platform through sameCookiePlatform, whose Domain-less default is
// the DECLARED ORIGIN's platform. Rule 3 also DISAMBIGUATES rather than
// guessing: it fires only when exactly one non-deleting candidate qualifies
// (`refreshes == 1`), so two same-name updates inside the platform
// decline rather than let map order pick. The fan-out is deliberate (Arc 2 built
// it, Arc 8 preserved it): it is what stops one domain variant going stale while
// the other moves on, the drift finding #4 was about. Pinned by
// TestUnscopedRefreshCrossesDomainsOnlyInsideTheDeclaredPlatform.
//
// A SCOPED header refreshes across the platform on the same rule and is not
// narrowed here — rule 3 reads `sameCookiePlatform(k.Domain, …)`, so a
// `Domain=.google.com` rotation also repairs a stale `.youtube.com` twin when it
// is the only candidate. REFRESH is stated for the unscoped case because that is
// the case bullet 3 above admits and the one the deleted comment got wrong; the
// scoped/unscoped split appears under CREATE and, in narrower form, under DELETE
// (an unscoped deletion matches subdomain-wide through rule 2, a scoped one
// exact-host through rule 1).
//
// CREATE — the verb where scoped and unscoped part company, and that difference
// IS the misattribution rule:
//
//   - a SCOPED header creates on the domain it declared, whenever that domain is
//     inside the origin's platform. It reaches the insertion loop like any other
//     update that matched no row — that loop's own doc says
//     "everything the matching rules turned down arrives here" — keeps
//     `domain = key.Domain` and passes the platform guard.
//     So an admitted `SID=x; Domain=.google.com` from a
//     youtube.com reply DOES create a `.google.com` row against a file holding
//     none, and that is CORRECT: it is the rule above, not a leak past it.
//   - an UNSCOPED header creates only on the declared origin's own SITE. The
//     loop derives that row's domain from the origin and nothing else
//     (`domain = "." + string(origin)`). The branch that used to guess
//     it from the cookie NAME — writing `.google.com SID` out of an ordinary
//     youtube.com reply — is the one Arc 8 Task 2 removed. The real .google.com
//     SID is rotated by accounts.google.com with an explicit Domain=, which
//     takes the scoped path and never reaches that branch at all. Same name,
//     different cookie.
//   - narrower still, for one batch shape: an unscoped key is not inserted
//     beside a scoped NON-DELETING sibling of the same name (hasScopedSibling),
//     because the scoped header has already claimed a row and the
//     unscoped twin would override it by name in the jar. A scoped DELETION is
//     not such a sibling — delete-plus-insert is "replace" — see
//     hasScopedSibling's own doc.
//
// DELETE — an unscoped header may delete only inside the declared origin's own
// SITE, through rule 2 alone. Rule 1 needs a Domain=, rule 3 skips deletions
// (`!updates[k].Delete`) and the insertion loop skips them as well,
// so origin.covers is the only door a Domain-less deletion has. (A
// SCOPED deletion is rule 1's, and reaches only rows whose domain it exactly
// scope-matches — the scope the server actually named.)
//
// WHY UNSCOPED CREATE IS NARROWER THAN UNSCOPED REFRESH, given that both are
// "the same cookie": a rewrite repairs a row the FILE has already asserted
// belongs to this platform. That domain came from a browser export or an earlier
// scoped Set-Cookie, and the response is only supplying a fresher value for a
// scope something else established. A creation has no such prior assertion to
// lean on, so inventing `.google.com` out of an unscoped youtube.com header
// would be THIS WRITER asserting a scope nobody named — and a false one, per the
// misattribution rule above. A scoped header carries its own assertion, which is
// exactly why its CREATE is not narrowed. Deletion is narrow for the ordinary
// reason, that it is the unrecoverable verb.
//
// Two layers, and they are not redundant. THIS one decides what becomes an
// update at all, and it is the only code that ever sees the raw header — so
// row-breaking characters and the tracked-name rule belong here and nowhere else.
// updateCookieFile's insertion guard is the last check before a row is WRITTEN,
// and it covers the domain that loop DERIVES from the declared origin for an
// unscoped update — a domain that does not exist yet when this function returns,
// and which this function therefore cannot judge. Neither subsumes the other.
//
// This layer is also per-header and pure: it cannot see the other Set-Cookie
// headers in the same response. The one rule that needs that view — an unscoped
// key must not be INSERTED beside a scoped key of the same name — therefore
// lives in the insertion loop, which holds the whole batch.
//
// Scoped headers are admitted under ANY name; only unscoped ones are name-gated.
// That asymmetry is pre-existing and deliberately left alone here: narrowing it
// would mix a widening and a narrowing into one commit.
func admitSetCookie(sc string, origin cookieOrigin) (cookieUpdateKey, cookieUpdate, bool) {
	parts := strings.Split(sc, ";")
	if len(parts) == 0 {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}
	nameValue := strings.TrimSpace(parts[0])
	name, value, ok := strings.Cut(nameValue, "=")
	if !ok {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}
	// RFC 6265 §5.2 step 3: remove leading and trailing WS from the name-string
	// AND the value-string, separately. Trimming only the whole `name=value`
	// pair (the line above, kept because it also eats a stray line ending) left
	// `SAPISID = v` parsed as the name "SAPISID " and the value " v" — a name no
	// predicate in this package recognises and a value with a leading space.
	//
	// WS here is SP and HTAB exactly, per the grammar, which is why this is
	// strings.Trim and not strings.TrimSpace: TrimSpace would also eat a leading
	// CR or LF and quietly rescue a header that step 1 below should refuse.
	//
	// NOT de-quoted, and that is deliberate rather than unfinished. §5.2 takes
	// everything up to the first ";" as the name/value pair and never strips
	// DQUOTEs, so `Customer="WILE_E_COYOTE"` has the quotes as part of its value
	// and `chips="a;hoy"` really does truncate at the semicolon — every browser
	// behaves this way. CPython's SimpleCookie strips quotes because it
	// implements the older RFC 2109; matching it here would diverge from both
	// RFC 6265 and the browsers whose exports fill this file.
	name = strings.Trim(name, " \t")
	value = strings.Trim(value, " \t")
	if name == "" {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}

	now := time.Now().Unix()
	expiry := now + 365*24*60*60

	var (
		expiresAt  int64
		hasExpires bool
		maxAge     int64
		hasMaxAge  bool
		httpOnly   bool
		domainAttr string
	)
	// Every attribute is read to the end of the header. The old loop broke
	// out early on Max-Age<=0, which threw away the Domain= that usually
	// follows it — and Domain= is what scopes the deletion below.
	for _, part := range parts[1:] {
		trimmed := strings.TrimSpace(strings.ToLower(part))
		switch {
		case strings.HasPrefix(trimmed, "expires="):
			_, dateStr, _ := strings.Cut(part, "=")
			dateStr = strings.TrimSpace(dateStr)
			if t, err := time.Parse(time.RFC1123, dateStr); err == nil {
				expiresAt, hasExpires = t.Unix(), true
			} else if t, err := time.Parse("Mon, 02-Jan-2006 15:04:05 MST", dateStr); err == nil {
				expiresAt, hasExpires = t.Unix(), true
			} else if t, err := time.Parse(time.RFC1123Z, dateStr); err == nil {
				expiresAt, hasExpires = t.Unix(), true
			}
			// If all date formats fail, hasExpires stays false and the
			// default one-year expiry below applies. An unreadable date
			// must not fall through as "expired" and delete the row.
		case strings.HasPrefix(trimmed, "max-age="):
			if v, err := strconv.ParseInt(strings.TrimSpace(trimmed[len("max-age="):]), 10, 64); err == nil {
				maxAge, hasMaxAge = v, true
			}
		case strings.HasPrefix(trimmed, "domain="):
			_, dom, _ := strings.Cut(part, "=")
			// Lowercased here, not just at comparison time. Domains are
			// case-insensitive and this string becomes a MAP KEY: without
			// this, "Domain=.YouTube.com" and "Domain=.youtube.com" are two
			// distinct keys that both scope-match the same row, and which
			// one wins is map-iteration order. CPython normalizes the same
			// way (_normalized_cookie_tuples: `if k == "domain": v = v.lower()`).
			domainAttr = strings.ToLower(strings.TrimSpace(dom))
		case trimmed == "httponly":
			httpOnly = true
		}
	}

	// RFC 6265 §4.1.2.2: Max-Age takes precedence over Expires.
	//
	// An expiry at or before now is a DELETION request and is treated
	// exactly as Max-Age<=0 is. This is the same rule yt-dlp gets from
	// Python's http.cookiejar: _cookie_from_cookie_tuple converts Max-Age
	// to an absolute expiry and then, for `expires <= self._now`, calls
	// self.clear(domain, path, name) and returns None — the cookie is
	// dropped from the jar entirely, keyed by domain+path+name. It is
	// never stored with an empty value, which is what this code used to
	// write: a row with value "" and expiry 0 that rowExpired will not
	// prune (it ignores exp == 0) and that CookieJar.Load cannot even see
	// (TrimSpace eats the trailing tab, leaving a 6-field "malformed" row).
	deleteCookie := false
	switch {
	case hasMaxAge:
		switch {
		case maxAge <= 0:
			deleteCookie = true
		case maxAge > maxCookieLifetime:
			// Clamped, not refused. A too-long Max-Age is a statement about
			// lifetime, not a malformed header, and refusing it would throw away
			// a perfectly good rotated VALUE over an attribute every browser
			// silently caps anyway. See maxCookieLifetime for why the clamp also
			// closes the int64 overflow.
			expiry = now + maxCookieLifetime
		default:
			expiry = now + maxAge
		}
	case hasExpires:
		if expiresAt <= now {
			deleteCookie = true
		} else {
			expiry = expiresAt
		}
	}

	// Normalize domain so the Netscape row uses a leading-dot form when
	// the Set-Cookie explicitly said Domain= (which implies subdomain
	// scope per RFC 6265). A bare "Domain=" carries no value at all and
	// leaves domainAttr empty, which RFC 6265 §5.2.3 also says to treat as
	// host-only — so it takes the unscoped branch, not a "." one.
	if domainAttr != "" && !strings.HasPrefix(domainAttr, ".") {
		domainAttr = "." + domainAttr
	}

	// Step 1. Before either admission branch, and on all three fields that become
	// their own tab-separated column in the row. Normalization above can only
	// lowercase and prepend a dot, so checking after it sees the same characters
	// checking before it would; the WSP trim above has already removed the tabs
	// that RFC 6265 §5.2 says are not part of the name or the value at all, so
	// what reaches here is an INTERIOR one.
	if hasRowBreakingChar(name) || hasRowBreakingChar(value) || hasRowBreakingChar(domainAttr) {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}

	if domainAttr != "" {
		// Step 2. cookiePlatformOf returns "" for a domain on no known platform
		// (evil.tld, accounts.google.com.evil.tld, a bare "."), and an undeclared
		// origin has no platform either — so the emptiness test is load-bearing:
		// without it, bare equality would read "" == "" as a match.
		p := cookiePlatformOf(domainAttr)
		if p == "" || p != origin.platform() {
			return cookieUpdateKey{}, cookieUpdate{}, false
		}
	} else if !trackedCookieName(name, origin) {
		// Step 3.
		return cookieUpdateKey{}, cookieUpdate{}, false
	}

	return cookieUpdateKey{Name: name, Domain: domainAttr}, cookieUpdate{
		Value:    value,
		Expiry:   expiry,
		HTTPOnly: httpOnly,
		Delete:   deleteCookie,
	}, true
}

// errCookieSessionReplaced is updateCookieFile declining to write rotations
// for a session cookies.txt no longer holds.
var errCookieSessionReplaced = errors.New("cookies.txt holds a different session than the one the rotations are for")

// updateCookieFile re-reads the cookie file, updates matching cookies with new
// values and expiry, and adds new cookies not already in the file.
//
// Behavior notes relative to the original implementation:
//   - Every row matching an updated cookie name is refreshed (per finding #4),
//     not just the first one. Leaving stale duplicates on .google.com while a
//     fresh value lands on .youtube.com caused silent cookie drift on legacy
//     files that contained multiple domain variants of the same name.
//   - The Netscape "include subdomains" flag is derived from whether the
//     domain begins with "." (finding #5) instead of being hardcoded TRUE.
//   - Domain for newly-inserted rows is taken from the Set-Cookie Domain=
//     attribute when the server provided one (finding #40); when it did not,
//     from the DECLARED ORIGIN's own site. It used to be guessed from the
//     cookie name — see the insertion loop for why that was wrong and why
//     nobody noticed for so long.
//   - Deletions remove the row. See resolveRowUpdate for why a value refresh
//     may cross domain variants while a deletion may not.
//
// origin is the SITE whose response produced these updates, and the caller has
// to state it because three decisions here need it and none can recover it from
// the map: a Set-Cookie with no Domain= is host-scoped to the response that
// carried it, and that response is not in `updates`. resolveRowUpdate's rule 2
// and sameCookiePlatform's Domain-less default both used to assume youtube.com,
// which was a true statement about the single call site rather than about those
// functions — correct today, and silently wrong in the DESTROY-SCOPE direction
// the day a second caller appears (the open one being a re-auth ingest response
// from accounts.google.com, whose unscoped deletions would have reached
// .youtube.com rows).
//
// The third is the INSERTION loop, and it is the one that is easy to miss:
// declining to match a row is not declining to write. Every update the two
// matching rules turn down lands in that loop, which derives a domain from the
// cookie name alone — so without an origin check there, a caller whose updates
// were refused everywhere still appended new rows, under a domain nobody
// declared. The loop now refuses any row outside the declared platform, and
// refuses everything when no origin was declared.
//
// This parameter is ENFORCEMENT of the existing rule, not a change to it: with
// originYouTube every case behaves exactly as before, insertion included (a
// youtube.com response's cookies land on youtube.com and google.com, which are
// one platform). The grow-broadly/destroy-narrowly asymmetry is likewise
// unchanged and deliberate — name-loose updates re-sync stale twins on purpose,
// domain-strict deletions keep .google.com auth out of reach of an unscoped
// YouTube deletion.
//
// sentAs, when not empty, is the YouTubeIdentity of the session the response
// answered (see checkAndRefreshYouTube). The file is re-read here, at write
// time, and an import — or anything else that writes cookies.txt — can have
// replaced it while the request was in flight: the old session's rotated
// __Secure-1PSIDTS then landed on the new session's rows, a mixed file the
// operator had just been told was imported. A file whose session is no longer
// sentAs is left alone (errCookieSessionReplaced).
func (rs *RefreshService) updateCookieFile(updates map[cookieUpdateKey]cookieUpdate, origin cookieOrigin, sentAs string) error {
	filePath := rs.jar.GetFilePath()
	if filePath == "" {
		return fmt.Errorf("no cookie file path configured")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read cookie file: %w", err)
	}
	if sentAs != "" {
		onDisk := NewCookieJar()
		onDisk.loadFrom(data, filePath)
		if onDisk.YouTubeIdentity() != sentAs {
			return errCookieSessionReplaced
		}
	}

	// Index by name once so each row costs a map lookup rather than a scan of
	// every pending update.
	byName := make(map[string][]cookieUpdateKey, len(updates))
	for k := range updates {
		byName[k.Name] = append(byName[k.Name], k)
	}

	var result strings.Builder
	handled := make(map[cookieUpdateKey]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Netscape cookie files occasionally contain values that push a single
	// line past bufio.Scanner's default 64KiB buffer; bump the ceiling to
	// 1MiB so an oversized line surfaces as an error below instead of a
	// silently truncated row.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Check if this is a cookie line that we need to update.
		// Every matching row is rewritten (not just the first) so multi-domain
		// duplicates do not drift out of sync with the refreshed values.
		if trimmed != "" && (!strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "#HttpOnly_")) {
			parts := strings.Split(trimmed, "\t")
			if len(parts) >= 7 {
				cookieName := strings.TrimSpace(parts[5])
				rowDomain := strings.TrimPrefix(strings.TrimSpace(parts[0]), "#HttpOnly_")
				if key, cu, ok := resolveRowUpdate(updates, byName[cookieName], rowDomain, origin); ok {
					handled[key] = true
					// essentialYouTubeCookies is a set of NAMES, and several of
					// them (PREF, CONSENT, YSC, LOGIN_INFO, the rotating
					// SIDTS/SIDCC pair) are not YouTube-exclusive strings — just
					// names YouTube happens to use. So a row only carries an
					// essential YouTube cookie when its DOMAIN says so too, the
					// same guard Arc 5 put on jar.Load and isEssentialCookie.
					//
					// Both readers below select log SEVERITY only and gate no
					// mutation, which is why the unguarded form was not wrong on
					// the wire. It is guarded because it was the last surviving
					// copy of a shape this plan has now removed three times, and
					// the next reader would reasonably lift it somewhere it does
					// decide something.
					//
					// Computed inside this branch: nearly every row in a real
					// cookies.txt matches no pending update, and neither reader
					// below is reachable for those.
					rowHasEssential := essentialYouTubeCookies[cookieName] &&
						(isYouTubeDomain(rowDomain) || isGoogleDomain(rowDomain))
					if cu.Delete {
						// Drop the row. Writing it back with an empty value
						// left a credential-shaped hole nothing could prune.
						if rowHasEssential {
							rs.logger.Info("youtube session refresh: the server deleted an essential cookie — the signed-in session may have ended",
								"name", cookieName, "domain", rowDomain)
						} else {
							rs.logger.Debug("youtube session refresh: server deleted a cookie", "name", cookieName, "domain", rowDomain)
						}
						continue
					}
					// An empty value with NO expiry attribute is refused, not
					// applied. Scoped to this path only — the global version of
					// this guard was rejected in review, and this function is
					// reachable from processYouTubeSetCookies alone.
					//
					// Two reasons it is a refusal rather than a third deletion form:
					//
					//  1. This package cannot represent an empty-valued row at
					//     all. CookieJar.Load TrimSpaces the line first, so the
					//     trailing tab disappears, the row reads as 6 fields and
					//     is skipped as malformed — the credential vanishes from
					//     the jar while the row sits in the file. Writing one is
					//     never the right answer.
					//  2. The server has two unambiguous ways to say "delete"
					//     (a past Expires, Max-Age<=0) and both are honoured
					//     above, and a real Google logout carries a past
					//     Expires — so it takes the deletion branch and never
					//     reaches here. A bare "NAME=" states no intent.
					//     Stronger still: this function only ever runs on a
					//     response YouTube just told us was AUTHENTICATED
					//     (refresh.go's `if authenticated` gate, further
					//     narrowed by authResponseIsOurs, the non-200 check
					//     and the unreadable-body check). A reply that asserts
					//     "you are signed in" while blanking the credential
					//     that proves it is self-contradictory; a value-
					//     stripping intermediary explains it, a logout does
					//     not. Keeping a stale value is recoverable — the
					//     auth check fails, park/sweep flags it, and the Warn
					//     below says so. Destroying a live one is not.
					//
					//     (Not "a truncated response": Set-Cookie is a header,
					//     and net/http parses the whole header block before Do
					//     returns, so a truncated body cannot blank one.)
					if cu.Value == "" {
						if rowHasEssential && strings.Join(parts[6:], "\t") != "" {
							rs.logger.Warn("youtube session refresh: refused to blank an essential cookie — the Set-Cookie carried an empty value but no expiry, so it is not a deletion and the existing value was kept",
								"name", cookieName, "domain", rowDomain)
						} else {
							rs.logger.Debug("youtube session refresh: ignoring empty-valued Set-Cookie with no expiry", "name", cookieName, "domain", rowDomain)
						}
						result.WriteString(line)
						result.WriteString("\n")
						continue
					}
					// Rebuild the row from EXACTLY the first seven fields of the
					// row being replaced, with the new expiry and value
					// substituted in. That is a claim about the row READ, and it
					// is the one that matters: CookieJar.Load reads fields 6.. as
					// one value that may itself contain tabs, so a live row can
					// arrive split into 8+ parts, and assigning parts[6] and
					// re-joining left the tail of the replaced value dangling on
					// the end of the new one.
					//
					// It is NOT a claim about the row WRITTEN. A tab inside
					// cu.Value emits 8+ fields again, and both readers in this
					// package handle that correctly — CookieJar.Load joins fields
					// 6.. back into one value, and mergeCookieFiles keys on
					// fields 0 and 5 and carries the whole line verbatim.
					//
					// parts[0] is emitted verbatim, which also settles the
					// "#HttpOnly_" question for this path: the prefix is
					// PRESERVED when the existing row carries it and never ADDED
					// when it does not. For a rewrite the file's own row is the
					// authority on HttpOnly-ness — it is what a browser export or
					// a previous insertion recorded — and this path is changing a
					// value and an expiry, not re-deciding the flag. A
					// Set-Cookie's HttpOnly attribute only matters on INSERTION,
					// where no existing row can be that authority and cu.HTTPOnly
					// is read instead (below). The consequence is that a server
					// which starts or stops sending HttpOnly for a cookie already
					// in the file does not flip the prefix; nothing in this
					// package treats the flag as a control (CookieJar.Load,
					// rowExpired and mergeCookieFiles all merely strip it, and
					// the jar does not retain it), so the cost is a stale
					// annotation rather than a downgraded cookie.
					result.WriteString(strings.Join([]string{
						parts[0], parts[1], parts[2], parts[3],
						strconv.FormatInt(cu.Expiry, 10),
						parts[5], cu.Value,
					}, "\t"))
					result.WriteString("\n")
					continue
				}
			}
		}

		result.WriteString(line)
		result.WriteString("\n")
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan cookie file: %w", err)
	}

	// Add new cookies that weren't found in the existing file. A deletion for
	// a row that is not there is simply done — it must never be inserted. Nor
	// may an empty value: same refusal as the rewrite path above, and an
	// inserted empty row would be one this package's own reader cannot read.
	// Nor, per the origin check below, may a row outside the platform the
	// caller declared: everything the matching rules turned down arrives here,
	// so this loop is where "declined" has to become "not written".
	for key, cu := range updates {
		if handled[key] || cu.Delete {
			continue
		}
		if cu.Value == "" {
			rs.logger.Debug("youtube session refresh: not inserting an empty-valued cookie", "name", key.Name, "domain", key.Domain)
			continue
		}
		name := key.Name
		domain := key.Domain
		// An unscoped update may not be INSERTED when the same response also
		// carried a scoped one of the same name that it means to keep. The scoped
		// header has already claimed whichever row it matches; inserting the
		// unscoped twin appends a row that then OVERRIDES it in the jar.
		//
		// The override is by NAME, not by domain, and stating that correctly
		// matters because the two rows are usually on different domains. Since
		// fix round 1 the twin lands on the declared origin's own site, so an
		// originYouTube response carrying `SID=v1` and `SID=v2; Domain=.google.com`
		// against a file holding the .google.com row leaves `.google.com` and
		// `.youtube.com` SID rows side by side — not two rows on one domain, which
		// is only what the originGoogle case would produce. It is still a
		// downgrade: CookieJar.Load puts youtube.com and google.com rows in ONE
		// map keyed by bare cookie name, so the last row read wins and the
		// unscoped value silently defeats the value the server was more specific
		// about.
		//
		// Narrow on purpose, and all three halves of that matter. It fires only
		// for an unscoped key; only when a scoped SIBLING of the same name is in
		// this batch; and not when that sibling is a DELETION (see
		// hasScopedSibling — a delete plus an insert of one name is "replace",
		// and suppressing the insert loses the replacement). The obvious wider
		// rule — "once any key of this name matched a row, treat every key of
		// that name as handled" — destroys the legitimate case: a response
		// rotating SID on both .google.com and .youtube.com against a file
		// holding only the .google.com row must still insert the .youtube.com one.
		if domain == "" && hasScopedSibling(updates, byName[name]) {
			rs.logger.Debug("cookie update: not inserting an unscoped cookie beside a scoped one of the same name",
				"name", name, "origin", string(origin))
			continue
		}
		if domain == "" {
			// The Set-Cookie carried no Domain=, so RFC 6265 §4.1.2.3 host-scopes
			// it to the response that carried it — and the only thing here that
			// knows which response that was is the declared origin. So the domain
			// is the origin's own site, and nothing else contributes to it.
			//
			// It used to be guessed from the cookie NAME: .youtube.com, or
			// .google.com when isGoogleOnlyAuthName said so. That branch was DEAD
			// CODE for as long as it existed — processYouTubeSetCookies opened
			// with a substring pre-filter that dropped every Domain-less header
			// before it could become an unscoped key — and going live exposed it
			// as the exact inverse of resolveRowUpdate's rule 2. An unscoped SID
			// from an ordinary youtube.com reply was written as `.google.com SID`
			// and then sent to accounts.google.com on the next request. It is a
			// DIFFERENT COOKIE: the real .google.com SID is rotated by
			// accounts.google.com with an explicit Domain=, which takes the scoped
			// path and never reaches this branch at all. isGoogleOnlyAuthName is
			// retired as a domain-inventor and survives only as half of
			// trackedCookieName's admission set.
			//
			// The leading-dot registrable domain (".youtube.com") rather than the
			// host-only form the response literally scopes it to
			// ("www.youtube.com") because that is the shape the rest of the file
			// speaks: browser exports, mergeCookieFiles and CookieJar.Load all key
			// on the registrable domain with include-subdomains set, and
			// resolveRowUpdate's rule 2 matches through origin.covers(), which
			// accepts exactly this. A host-only row would be a shape no other
			// writer in this package produces, and the next refresh would not
			// match it.
			//
			// This makes the cross-platform hazard structural rather than caught:
			// an unscoped insertion now lands inside the declaring origin by
			// construction, so it CANNOT reach another platform's rows. The check
			// below still earns its place for the two cases construction does not
			// cover — an explicit cross-platform Domain=, and an undeclared origin
			// (which yields "." here, a domain on no platform at all).
			domain = "." + string(origin)
		}
		// An insertion may not leave the declared origin's platform, and an
		// undeclared origin may not insert at all.
		//
		// This is the half of the rule the matching rules cannot enforce, and
		// missing it inverted the whole point of declaring an origin. Declining
		// to MATCH is not declining to WRITE: when resolveRowUpdate turns down
		// every row, the update is not dropped, it arrives here. When this branch
		// guessed the domain from the cookie name, a Twitch (or undeclared)
		// caller's unscoped "SID" was refused by rules 2 and 3 and then appended a
		// brand-new .google.com SID row anyway, landing a foreign credential in
		// the Google jar — WIDER than the hardcoded behaviour the origin parameter
		// replaced, not narrower.
		//
		// The unscoped half of that is now impossible by construction (the domain
		// IS the origin's site), so what this check still decides is the explicit
		// -Domain= case and the undeclared origin. Kept whole rather than narrowed
		// to those two: it is one sentence about the row about to be written, and
		// splitting it would make the guarantee depend on which branch produced
		// the domain.
		//
		// Checked against the domain actually about to be written, not only the
		// fallback, so a key carrying an explicit cross-platform Domain= is
		// refused on the same rule. Nothing legitimate is lost: a Google session
		// covers youtube.com and google.com alike, so the two never fail this
		// against each other, and admitSetCookie already drops a Domain= that
		// is neither.
		//
		// A domain with no recognised platform is refused by the same test — an
		// undeclared origin and an unplaceable domain both yield "", and equality
		// alone would read that as a match.
		insertPlatform := cookiePlatformOf(domain)
		if insertPlatform == "" || insertPlatform != origin.platform() {
			rs.logger.Debug("cookie update: refusing to insert a row outside the declaring origin's platform",
				"name", name, "domain", domain, "origin", string(origin))
			continue
		}
		// Subdomain flag follows RFC 6265: leading-dot domain = include
		// subdomains. The legacy code hardcoded TRUE even for no-dot domains.
		subdomains := "FALSE"
		if strings.HasPrefix(domain, ".") {
			subdomains = "TRUE"
		}
		secure := "FALSE"
		if strings.HasPrefix(name, "__Secure-") {
			secure = "TRUE"
		}
		// An HttpOnly cookie is written as a "#HttpOnly_"-prefixed row — the
		// Netscape convention every reader in this package already honours
		// (CookieJar.Load, rowExpired, mergeCookieFiles). Inserting it without
		// the prefix silently downgraded the flag.
		prefix := ""
		if cu.HTTPOnly {
			prefix = "#HttpOnly_"
		}
		// Netscape format: domain, include_subdomains, path, secure, expiry, name, value
		if _, werr := fmt.Fprintf(&result, "%s%s\t%s\t/\t%s\t%d\t%s\t%s\n",
			prefix, domain, subdomains, secure, cu.Expiry, name, cu.Value); werr != nil {
			return fmt.Errorf("write new cookie row: %w", werr)
		}
		rs.logger.Debug("added new cookie to file", "name", name, "domain", domain)
		handled[key] = true
	}

	// Atomic write via the shared same-package helper — it uses a unique
	// temp name (the AutoCookieService writes the same cookies.txt, so a
	// fixed ".tmp" would let the two writers interleave) and applies the
	// memoized parent-dir DACL tightening.
	if err := writeFileAtomic(filePath, []byte(result.String()), 0o600); err != nil {
		return err
	}

	if len(handled) > 0 {
		rs.logger.Debug("updated cookies in file", "updated", len(handled))
	}

	return nil
}

// hasScopedSibling reports whether any of these same-name update keys carries an
// explicit Domain= AND is a value it intends to keep. The caller has the keys for
// one name already grouped (the byName index), so this is a walk over one or two
// entries, not a scan.
//
// A scoped DELETION is not a sibling for this purpose, and that exclusion is the
// whole reason this takes the updates map rather than the keys alone. `SID=;
// Domain=.google.com; Max-Age=0` beside an unscoped `SID=fresh` is one response
// saying REPLACE — retire the cookie on google.com, set it host-scoped here.
// Counting the deletion as a claim on the name made the guard eat the
// replacement: the delete removed the .google.com row, the unscoped insert was
// then suppressed as "beside a scoped one", and the fresh value reached nothing.
// A deletion claims no row that an insertion could duplicate, because after it
// runs there is no row.
func hasScopedSibling(updates map[cookieUpdateKey]cookieUpdate, candidates []cookieUpdateKey) bool {
	for _, k := range candidates {
		if k.Domain != "" && !updates[k].Delete {
			return true
		}
	}
	return false
}

// resolveRowUpdate picks the pending update that applies to one file row, from
// the candidate keys that already share the row's cookie name.
//
// The rule is asymmetric on purpose: grow broadly, destroy narrowly.
//
//   - A value refresh may cross domain variants, but only within one platform.
//     The same session value is valid on .youtube.com and .google.com alike,
//     and leaving one variant stale while the other moves on is the drift that
//     finding #4 was about. Crossing to .twitch.tv is a different matter:
//     growing onto another platform's occupied slot IS destruction, so the
//     platforms are kept apart even though no name collides between them today.
//   - A deletion may not cross at all. It is unrecoverable, so it only ever
//     removes rows inside the scope the server actually named.
//
// The narrow half is deliberately under-applied rather than over-applied: a
// deletion scoped to ".youtube.com" does NOT remove a host-only
// "www.youtube.com" row, even though RFC 6265 domain-matching says it covers
// it. Browser extraction really does write host-only rows, so this is
// reachable — and the chosen failure is a stale row that keeps being sent
// (recoverable) over a deleted credential (not). Do not "fix" it into a
// suffix match without re-deciding that trade.
//
// At most one candidate can scope-match a given row: Domain= is normalized to
// one lowercased leading-dot form before it becomes a key, so two distinct keys
// for one name always name different hosts. Both halves of that normalization
// are load-bearing — without the lowercasing, ".YouTube.com" and ".youtube.com"
// are separate keys that both match, and which one wins is map-iteration order.
func resolveRowUpdate(updates map[cookieUpdateKey]cookieUpdate, candidates []cookieUpdateKey, rowDomain string, origin cookieOrigin) (cookieUpdateKey, cookieUpdate, bool) {
	if len(candidates) == 0 {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}
	// 1. A Set-Cookie scoped to this row's own host always wins.
	for _, k := range candidates {
		if k.Domain != "" && sameCookieScope(k.Domain, rowDomain) {
			return k, updates[k], true
		}
	}
	// 2. A Set-Cookie with no Domain= is host-scoped to the response that
	//    carried it, so it may only reach rows inside the site the CALLER
	//    declared that response came from. Confining it that way is what stops
	//    an unscoped deletion in a youtube.com reply reaching .google.com auth —
	//    and, the day a second caller exists, an unscoped deletion in a
	//    google.com reply reaching .youtube.com. An undeclared origin covers
	//    nothing, so the rule simply does not fire.
	if origin.covers(rowDomain) {
		for _, k := range candidates {
			if k.Domain == "" {
				return k, updates[k], true
			}
		}
	}
	// 3. Otherwise only a value refresh may cross domains — within one platform,
	//    and only when a single non-deleting update is in play so the choice is
	//    unambiguous.
	var only cookieUpdateKey
	refreshes := 0
	for _, k := range candidates {
		if !updates[k].Delete && sameCookiePlatform(k.Domain, rowDomain, origin) {
			only, refreshes = k, refreshes+1
		}
	}
	if refreshes == 1 {
		return only, updates[only], true
	}
	return cookieUpdateKey{}, cookieUpdate{}, false
}

// sameCookiePlatform reports whether an update's domain and a file row's domain
// belong to the same credential platform. YouTube and Google are one platform:
// a Google session covers both, which is exactly why a refresh is allowed to
// fan out across them. Twitch is another, and a row on neither matches nothing.
//
// An update with no Domain= counts as the platform of the site the CALLER
// declared the response came from. Like rule 2 in resolveRowUpdate, that is a
// property of the call site and not of this function, which is why it arrives as
// a parameter instead of the hardcoded "google" that used to stand here. An
// undeclared origin has no platform, so a Domain-less update matches nothing —
// the narrow direction, which is the safe one.
func sameCookiePlatform(updateDomain, rowDomain string, origin cookieOrigin) bool {
	up := origin.platform()
	if updateDomain != "" {
		up = cookiePlatformOf(updateDomain)
	}
	return up != "" && up == cookiePlatformOf(rowDomain)
}

// sameCookieScope reports whether two domain strings name the same host. The
// leading dot only encodes the Netscape include-subdomains flag, so
// ".youtube.com" and "youtube.com" are one scope written two ways.
func sameCookieScope(a, b string) bool {
	return strings.EqualFold(
		strings.TrimPrefix(strings.TrimSpace(a), "."),
		strings.TrimPrefix(strings.TrimSpace(b), "."),
	)
}

// isGoogleOnlyAuthName returns true for cookie names that live on the
// google.com domain (not youtube.com) in a typical Google session. The
// legacy code used strings.Contains(name, "GOOGLE") which matched nothing
// real — most google.com auth cookies are named SID, HSID, SSID, APISID,
// SAPISID, or the __Secure- variants.
//
// It is a statement about NAMES, and it must not be used to decide a DOMAIN.
// updateCookieFile's insertion loop used to do exactly that for a Set-Cookie
// with no Domain=, which wrote a host-only youtube.com SID onto .google.com —
// a different cookie, sent to a different host. The sole caller now is
// trackedCookieName, which asks only whether the jar tracks the name.
func isGoogleOnlyAuthName(name string) bool {
	switch name {
	case "SID", "HSID", "SSID", "APISID", "SAPISID":
		return true
	}
	return strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P")
}

package cookies

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// extractedCookie holds a cookie from browser extraction before Netscape formatting.
type extractedCookie struct {
	domain   string
	httpOnly bool
	path     string
	secure   bool
	expiry   int64
	name     string
	value    string
}

// domainMatches is true when domain is exactly target or a proper subdomain
// of target (i.e. ends with "." + target). Unlike strings.Contains it does
// not match unrelated hosts like "fakegoogle.com.evil.tld" that merely
// embed the target as a substring.
func domainMatches(domain, target string) bool {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	t := strings.ToLower(strings.TrimSpace(target))
	return d == t || strings.HasSuffix(d, "."+t)
}

// isYouTubeDomain is true for youtube.com / www.youtube.com / music.youtube.com / etc.
func isYouTubeDomain(domain string) bool { return domainMatches(domain, "youtube.com") }

// isGoogleDomain is true for google.com and its subdomains (accounts.google.com etc.).
func isGoogleDomain(domain string) bool { return domainMatches(domain, "google.com") }

// isTwitchDomain is true for twitch.tv and its subdomains.
func isTwitchDomain(domain string) bool { return domainMatches(domain, "twitch.tv") }

// isRelevantDomain returns true for YouTube/Google/Twitch domains (matching TS).
func isRelevantDomain(domain string) bool {
	return isYouTubeDomain(domain) || isGoogleDomain(domain) || isTwitchDomain(domain)
}

// isEssentialCookie checks if a cookie should be included in extraction (matching TS).
func isEssentialCookie(name, domain string) bool {
	// YouTube essential cookies. Domain-guarded for the same reason the
	// other two clauses below already are: several names in
	// essentialYouTubeCookies (PREF, CONSENT, YSC, LOGIN_INFO, the rotating
	// SIDTS/SIDCC pair) are not YouTube-exclusive strings, just names YouTube
	// happens to use. Without this guard, a row carrying one of those names
	// on ANY domain — a .twitch.tv row, or a third party's cookie of the same
	// name — was admitted here under YouTube's identity.
	//
	// That is worse than a leak on this path specifically: deduplicateAndFormat
	// below keys its byName map by bare name across platforms, and its only
	// skip rule protects an incumbent youtube.com row, not a google.com one.
	// A wrongly-admitted .twitch.tv row landing after a .google.com row of the
	// same name overwrites it — the real Google auth cookie is evicted before
	// the file is ever written, someplace CookieJar.Load's domain-aware
	// admission can never reach because a row that was never written is a row
	// the jar never sees.
	//
	// Currently unreachable: no real Twitch cookie name collides with
	// essentialYouTubeCookies today. It becomes reachable the day one does, or
	// the day a third party sets one of these generically-named cookies on a
	// domain this extractor visits.
	if (isYouTubeDomain(domain) || isGoogleDomain(domain)) && essentialYouTubeCookies[name] {
		return true
	}
	// Google domain auth cookies
	if isGoogleDomain(domain) {
		if name == "SID" || name == "HSID" || name == "SSID" || name == "APISID" || name == "SAPISID" ||
			strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P") {
			return true
		}
	}
	// Twitch essential cookies
	if isTwitchDomain(domain) && essentialTwitchCookies[name] {
		return true
	}
	return false
}

// deduplicateAndFormat filters, deduplicates (preferring youtube.com over google.com),
// and formats cookies to Netscape format. Matches TS deduplicateCookies() behavior.
func deduplicateAndFormat(cookies []extractedCookie) []string {
	// Deduplicate by name, preferring youtube.com over google.com
	type entry struct {
		cookie extractedCookie
		domain string
	}
	byName := make(map[string]entry)
	// Preserve order
	var order []string

	for _, c := range cookies {
		if !isRelevantDomain(c.domain) {
			continue
		}
		if !isEssentialCookie(c.name, c.domain) {
			continue
		}

		existing, exists := byName[c.name]
		if exists && isYouTubeDomain(existing.domain) && !isYouTubeDomain(c.domain) {
			continue // Already have youtube.com version, skip google.com
		}
		if !exists {
			order = append(order, c.name)
		}
		byName[c.name] = entry{cookie: c, domain: c.domain}
	}

	var lines []string
	for _, name := range order {
		e := byName[name]
		c := e.cookie

		subdomain := "FALSE"
		if strings.HasPrefix(c.domain, ".") {
			subdomain = "TRUE"
		}
		secureStr := "FALSE"
		if c.secure {
			secureStr = "TRUE"
		}
		prefix := ""
		if c.httpOnly {
			prefix = "#HttpOnly_"
		}

		lines = append(lines, fmt.Sprintf("%s%s\t%s\t%s\t%s\t%d\t%s\t%s",
			prefix, c.domain, subdomain, c.path, secureStr, c.expiry, c.name, c.value))
	}
	return lines
}

// mergeCookieFiles merges existing and new Netscape cookie strings.
// New cookies take priority over existing ones with the same name+domain+path.
func mergeCookieFiles(existing, newCookies string) string {
	// RFC 6265 §5.3: a cookie's identity is name + domain + PATH. The key
	// carried only the first two, so two rows differing solely in path
	// collided on one map entry and the later-parsed line silently replaced
	// the earlier one — a row lost from cookies.txt on every write, through
	// all three writers (FinishSetup, the browser refresh, ImportCookies).
	//
	// Secure and HttpOnly stay OUT deliberately: they are attributes OF a
	// cookie, not part of its identity, and keying on them would let a row
	// that merely flipped Secure accumulate beside its own replacement.
	//
	// This does NOT make the jar path-aware — CookieJar keys by name within a
	// platform and keeps last-wins for equal domains (jar.go). Two paths still
	// load as one entry, the same one the old key kept; what changes is that
	// the file stops losing the other row.
	type cookieKey struct {
		name   string
		domain string
		path   string
	}

	// Parse a Netscape cookie file into ordered keys and a map
	parseCookies := func(content string) ([]cookieKey, map[cookieKey]string) {
		keys := make([]cookieKey, 0)
		m := make(map[cookieKey]string)
		for line := range strings.SplitSeq(content, "\n") {
			trimmed := line
			// Handle #HttpOnly_ prefix
			if after, ok := strings.CutPrefix(trimmed, "#HttpOnly_"); ok {
				trimmed = after
			} else if strings.HasPrefix(trimmed, "#") || strings.TrimSpace(trimmed) == "" {
				continue
			}
			fields := strings.Split(trimmed, "\t")
			if len(fields) < 7 {
				continue
			}
			domain := fields[0]
			path := fields[2]
			name := fields[5]
			k := cookieKey{name: name, domain: domain, path: path}
			if _, exists := m[k]; !exists {
				keys = append(keys, k)
			}
			m[k] = line
		}
		return keys, m
	}

	existingKeys, existingMap := parseCookies(existing)
	_, newMap := parseCookies(newCookies)

	// Start with existing cookies, overwrite with new ones
	merged := make(map[cookieKey]string, len(existingMap))
	maps.Copy(merged, existingMap)
	allKeys := make([]cookieKey, 0, len(existingKeys))
	allKeys = append(allKeys, existingKeys...)

	for k, v := range newMap {
		if _, exists := merged[k]; !exists {
			allKeys = append(allKeys, k)
		}
		merged[k] = v
	}

	// Prune clearly-expired rows while assembling: merge otherwise keeps every
	// existing row forever, and CookieJar.Load ignores the expiry field — so a
	// cookie name that stops being refreshed (e.g. the platform retires it)
	// would be sent in the Cookie header indefinitely. Session cookies
	// (expiry 0) and unparseable rows are never pruned.
	now := time.Now().Unix()
	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Extracted by Moombox auto-cookie service")
	lines = append(lines, "")
	for _, k := range allKeys {
		if line, ok := merged[k]; ok {
			if rowExpired(line, now) {
				continue
			}
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

// twitchCredentialsIn reads a Netscape cookie text's Twitch credential pair
// through a throwaway jar.
//
// A probe jar rather than a second row scanner, as netscapeCookiesHoldACredential
// already does it: Load knows which rows are twitch.tv rows, which are
// essential, and how a `#HttpOnly_` prefix parses, and a private
// reimplementation would disagree the first time one of those moved.
//
// Load deliberately ignores expiry, so a row that is PRESENT reads as present
// whatever its date — which makes the before/after comparison in
// twitchLoginPrunedFromMerge mean "the prune removed it".
func twitchCredentialsIn(netscape string) (token, login string) {
	probe := NewCookieJar()
	probe.loadFrom([]byte(netscape), "")
	return probe.GetTwitchCredentials()
}

// twitchLoginPrunedFromMerge reports the one merge outcome worth an
// operator-visible line (Q7): the expiry prune dropped Twitch's `login` while
// an `auth-token` survived.
//
// That is a degradation this tree can NAME. ircHandshakeLines throws away a
// missing login and renders the full anonymous pair, so chat is captured with
// no subscriber-only messages and no badges — WITHOUT a refusal, so the IRC
// path's own once-per-downloader Warn never fires. Every predicate in jar.go
// still reads the platform as configured, because `login` is outside
// twitchAuthCookieNames and stays outside it.
//
// Deliberately silent on the neighbours: a file that never held a `login` is a
// hand-written cookies.txt, and a prune taking the auth-token too is a total
// credential loss RefreshCookiesDetailed already reports — saying "chat went
// anonymous" about that points at the smaller of two problems.
//
// The `login` may arrive from either input (an import can supply a fresh one),
// so both are consulted going in.
func twitchLoginPrunedFromMerge(previous, fetched, merged string) bool {
	_, prevLogin := twitchCredentialsIn(previous)
	_, fetchedLogin := twitchCredentialsIn(fetched)
	if prevLogin == "" && fetchedLogin == "" {
		return false
	}
	mergedToken, mergedLogin := twitchCredentialsIn(merged)
	return mergedLogin == "" && mergedToken != ""
}

// rowExpired reports whether a Netscape cookie row's expiry (5th field) is a
// positive unix timestamp in the past. Session cookies (expiry 0) and rows
// whose expiry field doesn't parse are never treated as expired — the safe
// failure mode is keeping the row.
func rowExpired(line string, now int64) bool {
	trimmed := strings.TrimPrefix(line, "#HttpOnly_")
	fields := strings.Split(trimmed, "\t")
	if len(fields) < 7 {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
	if err != nil {
		return false
	}
	return exp > 0 && exp < now
}

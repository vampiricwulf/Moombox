package cookies

// refresh_update_types.go — the pending Set-Cookie update vocabulary
// (cookieUpdateKey, cookieUpdate) and cookieOrigin, the site a batch of
// updates came from and the platform/scope predicates over it.

// cookieUpdateKey identifies one pending Set-Cookie change. Name alone is not
// enough: a real cookie file carries the same name on both .youtube.com and
// .google.com, and a name-keyed map both loses one of two same-name headers in
// a single response and lets a deletion scoped to one domain destroy the other
// domain's row.
//
// Domain is the normalized Set-Cookie Domain= attribute (leading dot, e.g.
// ".youtube.com"), or "" when the server sent no Domain= at all.
type cookieUpdateKey struct {
	Name   string
	Domain string
}

// cookieUpdate holds a parsed Set-Cookie value, expiry and flags. The domain
// lives in the map key (cookieUpdateKey) so there is exactly one copy of it;
// the write path reads it from there rather than re-deriving it from the
// cookie name.
type cookieUpdate struct {
	Value    string
	Expiry   int64
	HTTPOnly bool // Set-Cookie carried the HttpOnly attribute -> "#HttpOnly_" row prefix
	// Delete marks a deletion request: Max-Age<=0, or an Expires at or before
	// now. The row is removed, not rewritten empty — see processYouTubeSetCookies.
	Delete bool
}

// cookieOrigin names the SITE whose response produced a batch of cookie
// updates, stated as that site's registrable domain.
//
// It exists because a Set-Cookie with no Domain= attribute is host-scoped to
// the response that carried it, and by the time the updates reach
// updateCookieFile that response is gone: the map holds a nil Domain and
// nothing else. Two of the matching rules need to know where it came from
// anyway — resolveRowUpdate's rule 2 and sameCookiePlatform's Domain-less
// default — and both used to assume youtube.com, which was a true statement
// about the ONE call site rather than about those functions. Declaring it makes
// the assumption an argument.
//
// The three constants are the complete set. The zero value names no site and is
// deliberately inert: covers reports false for every row, platform reports
// nothing, and updateCookieFile's insertion loop refuses every row whose
// platform does not match the declared one — which for the zero value is all of
// them. So a caller that forgets to declare an origin updates only rows whose
// own Domain= scope-matches, and writes nothing new at all.
//
// That last clause is load-bearing and was wrong when this type was introduced.
// "Matches nothing" and "does nothing" are different: an update no row accepts
// falls through to the insertion loop, which invents a domain from the cookie
// name. Without the platform check there, an inert origin was not narrow — it
// appended new rows under a domain nobody declared.
type cookieOrigin string

const (
	originYouTube cookieOrigin = "youtube.com"
	originGoogle  cookieOrigin = "google.com"
	originTwitch  cookieOrigin = "twitch.tv"
)

// covers reports whether a cookie file row's domain lies inside this origin's
// site, which is the scope a Domain-less Set-Cookie from it may reach.
func (o cookieOrigin) covers(rowDomain string) bool {
	return o != "" && domainMatches(rowDomain, string(o))
}

// platform reports the credential platform this origin belongs to, in the same
// vocabulary cookiePlatformOf uses for domains — so originYouTube and
// originGoogle are one platform, exactly as .youtube.com and .google.com rows
// are.
func (o cookieOrigin) platform() string { return cookiePlatformOf(string(o)) }

// cookiePlatformOf maps a domain to its credential platform. YouTube and Google
// are ONE platform: a Google session covers both, which is why a value refresh
// is allowed to fan out across them. Twitch is another. A domain on neither has
// no platform and therefore matches nothing — see sameCookiePlatform.
func cookiePlatformOf(domain string) string {
	switch {
	case isTwitchDomain(domain):
		return "twitch"
	case isYouTubeDomain(domain) || isGoogleDomain(domain):
		return "google"
	}
	return ""
}

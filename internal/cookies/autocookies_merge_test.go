package cookies

import (
	"strings"
	"testing"
)

// TestIsRelevantDomain covers the suffix-anchored dispatcher used by both
// extraction and deduplication (finding #54).
func TestIsRelevantDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   bool
	}{
		{"youtube.com", true},
		{".youtube.com", true},
		{"www.youtube.com", true},
		{"accounts.google.com", true},
		{"twitch.tv", true},
		{".twitch.tv", true},
		{"example.com", false},
		{"youtube.com.evil.tld", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isRelevantDomain(tc.domain); got != tc.want {
			t.Errorf("isRelevantDomain(%q) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

// TestIsEssentialCookie covers the name+domain filter used to decide which
// cookies survive extraction (finding #54).
func TestIsEssentialCookie(t *testing.T) {
	cases := []struct {
		name   string
		domain string
		want   bool
	}{
		// YouTube essentials are accepted regardless of the exact host (the
		// caller already constrained to a relevant domain upstream).
		{"SAPISID", ".youtube.com", true},
		{"LOGIN_INFO", ".youtube.com", true},
		{"__Secure-3PAPISID", ".youtube.com", true},
		// Google-only auth cookies need a google.com domain.
		{"SID", ".google.com", true},
		{"HSID", ".google.com", true},
		{"SAPISID", ".google.com", true},
		{"__Secure-1PSID", "accounts.google.com", true},
		{"__Secure-3PSID", "accounts.google.com", true},
		// SID on a non-google domain is rejected (the YouTube essential list
		// already covers SID on youtube.com since it's in essentialYouTubeCookies).
		// All four Twitch-essential names, so a domain-guard fix on the
		// YouTube clause cannot be mistaken for over-tightening the Twitch one.
		{"auth-token", ".twitch.tv", true},
		{"twilight-user", ".twitch.tv", true},
		{"login", ".twitch.tv", true},
		{"name", ".twitch.tv", true},
		// Unknown name is rejected.
		{"random", ".youtube.com", false},
		{"auth-token", ".youtube.com", false}, // twitch cookie name, not on twitch.tv
		// Note: the upstream caller gates on isRelevantDomain first, so
		// isEssentialCookie doesn't separately re-check domain relevance
		// for known YouTube names like "SID" — it trusts essential names.

		// PREF, CONSENT, YSC and LOGIN_INFO are the names guarded ONLY by the
		// essentialYouTubeCookies clause — unlike SID/SAPISID/HSID/etc, they
		// are not repeated in the explicit google-domain auth list below, so
		// they are the names that actually exercise the domain guard on the
		// first clause (finding: Arc 5 Task 6). None of these strings is
		// YouTube-exclusive; a .twitch.tv row (or any third party's cookie)
		// using one of them must NOT pass under YouTube's identity.
		{"PREF", ".twitch.tv", false},
		{"CONSENT", ".twitch.tv", false},
		{"YSC", ".twitch.tv", false},
		{"LOGIN_INFO", ".twitch.tv", false},
		// The guard must accept BOTH youtube.com and google.com — Google auth
		// legitimately mints these on accounts.google.com too. A guard
		// narrowed to isYouTubeDomain only would wrongly reject this.
		{"PREF", "accounts.google.com", true},
		{"CONSENT", ".google.com", true},
	}
	for _, tc := range cases {
		if got := isEssentialCookie(tc.name, tc.domain); got != tc.want {
			t.Errorf("isEssentialCookie(%q, %q) = %v, want %v", tc.name, tc.domain, got, tc.want)
		}
	}
}

// TestDeduplicateAndFormatPrefersYouTube verifies youtube.com wins when a
// cookie name exists on both youtube.com and google.com (finding #54).
func TestDeduplicateAndFormatPrefersYouTube(t *testing.T) {
	cookies := []extractedCookie{
		{domain: ".google.com", name: "SAPISID", value: "google_val", path: "/", secure: true, expiry: 1000},
		{domain: ".youtube.com", name: "SAPISID", value: "youtube_val", path: "/", secure: true, expiry: 1000},
		{domain: ".youtube.com", name: "LOGIN_INFO", value: "login_val", path: "/", secure: true, expiry: 1000},
		{domain: "example.com", name: "SAPISID", value: "ignored", path: "/", secure: false, expiry: 0},
	}
	lines := deduplicateAndFormat(cookies)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "youtube_val") {
		t.Errorf("expected youtube SAPISID value in output, got:\n%s", joined)
	}
	if strings.Contains(joined, "google_val") {
		t.Errorf("expected google SAPISID value to be deduplicated out, got:\n%s", joined)
	}
	if strings.Contains(joined, "ignored") {
		t.Errorf("expected non-relevant domain to be filtered, got:\n%s", joined)
	}
}

// crossPlatformCollisionRows returns a .google.com SID and a .twitch.tv SID —
// the same bare name on two platforms that isEssentialCookie's first clause
// (before the Arc 5 Task 6 domain guard) admitted identically, and that
// deduplicateAndFormat's byName map (keyed by bare name — see its comment)
// cannot tell apart.
//
// google.com is used for the incumbent rather than youtube.com deliberately:
// deduplicateAndFormat's ONLY collision guard is "skip a non-youtube.com row
// when a youtube.com row already holds the name" (:87-89). A youtube.com
// incumbent would survive on that rule alone regardless of whether the
// isEssentialCookie fix is present, which would make the test pass for the
// wrong reason. google.com is not youtube.com, so that guard does not fire
// here and the outcome depends entirely on whether the Twitch row is
// admitted at all.
func crossPlatformCollisionRows(googleFirst bool) []extractedCookie {
	google := extractedCookie{domain: ".google.com", name: "SID", value: "google_sid_value", path: "/", secure: true, expiry: 4102444800}
	twitch := extractedCookie{domain: ".twitch.tv", name: "SID", value: "twitch_sid_value", path: "/", secure: true, expiry: 4102444800}
	if googleFirst {
		return []extractedCookie{google, twitch}
	}
	return []extractedCookie{twitch, google}
}

// TestDeduplicateAndFormatDropsCrossPlatformNameCollision is the eviction
// case from Arc 5 Task 6: a .twitch.tv row named SID must never survive to
// the formatted output, and — this is the half a name-collision fix can get
// wrong — the .google.com SID present in the very same input must survive
// alongside it. Asserting "the Twitch row is absent" alone would pass even if
// isEssentialCookie's bug were still live and BOTH rows fell out of the
// output for some unrelated reason, or if the Google row were the one lost;
// neither is the property this guards.
//
// Google is placed FIRST (see crossPlatformCollisionRows): with the domain
// guard removed, the Twitch row is admitted and arrives SECOND, so it is the
// row that overwrites byName["SID"] — the Twitch row genuinely wins the
// eviction, not merely coexists with the Google one. Reversing the order
// makes deduplicateAndFormat's plain last-write-wins behaviour hand the win
// back to Google even under the bug, which would satisfy this exact
// assertion for the wrong reason — see
// TestDeduplicateAndFormatCollisionOrderIndependent for why both orders must
// agree.
func TestDeduplicateAndFormatDropsCrossPlatformNameCollision(t *testing.T) {
	lines := deduplicateAndFormat(crossPlatformCollisionRows(true))
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "twitch_sid_value") {
		t.Errorf("expected the .twitch.tv SID row to be dropped entirely, got:\n%s", joined)
	}
	if !strings.Contains(joined, "google_sid_value") {
		t.Errorf("expected the .google.com SID row to survive — it must not be the row that was evicted, got:\n%s", joined)
	}
}

// TestDeduplicateAndFormatCollisionOrderIndependent asserts the same two
// colliding rows (see crossPlatformCollisionRows) produce IDENTICAL output
// regardless of input order. The eviction this guards against is
// order-dependent — deduplicateAndFormat's collision handling is plain
// last-write-wins once two non-youtube.com rows share a name — so a test that
// only checks one ordering can pass by accident of which row happened to
// arrive last, rather than because the Twitch row was never admitted.
func TestDeduplicateAndFormatCollisionOrderIndependent(t *testing.T) {
	googleFirst := strings.Join(deduplicateAndFormat(crossPlatformCollisionRows(true)), "\n")
	twitchFirst := strings.Join(deduplicateAndFormat(crossPlatformCollisionRows(false)), "\n")

	if googleFirst != twitchFirst {
		t.Fatalf("output depends on input order:\n  google-first: %q\n  twitch-first: %q", googleFirst, twitchFirst)
	}
	if !strings.Contains(googleFirst, "google_sid_value") || strings.Contains(googleFirst, "twitch_sid_value") {
		t.Fatalf("order-independent output is still wrong: %q", googleFirst)
	}
}

// TestDeduplicateAndFormatHttpOnlyPrefix ensures #HttpOnly_ rows are emitted
// for httpOnly cookies and not for others.
func TestDeduplicateAndFormatHttpOnlyPrefix(t *testing.T) {
	cookies := []extractedCookie{
		{domain: ".youtube.com", name: "SSID", value: "v1", path: "/", secure: true, httpOnly: true, expiry: 1000},
		{domain: ".youtube.com", name: "CONSENT", value: "v2", path: "/", secure: false, httpOnly: false, expiry: 1000},
		{domain: ".youtube.com", name: "LOGIN_INFO", value: "v3", path: "/", secure: true, httpOnly: false, expiry: 1000},
	}
	lines := deduplicateAndFormat(cookies)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t1000\tSSID\tv1") {
		t.Errorf("expected #HttpOnly_ SSID row, got:\n%s", joined)
	}
	if strings.Contains(joined, "#HttpOnly_.youtube.com\tTRUE\t/\tFALSE\t1000\tCONSENT") {
		t.Errorf("CONSENT should not have #HttpOnly_ prefix, got:\n%s", joined)
	}
}

// TestDeduplicateAndFormatSubdomainFlag verifies the include-subdomains
// Netscape field follows the presence of a leading dot in the domain.
func TestDeduplicateAndFormatSubdomainFlag(t *testing.T) {
	cookies := []extractedCookie{
		{domain: ".youtube.com", name: "LOGIN_INFO", value: "v1", path: "/", secure: true, expiry: 1},
		{domain: "www.youtube.com", name: "SAPISID", value: "v2", path: "/", secure: true, expiry: 1},
	}
	lines := deduplicateAndFormat(cookies)

	var withDot, withoutDot string
	for _, l := range lines {
		if strings.Contains(l, "LOGIN_INFO") {
			withDot = l
		}
		if strings.Contains(l, "SAPISID") {
			withoutDot = l
		}
	}
	if !strings.Contains(withDot, "\tTRUE\t/\t") {
		t.Errorf("dotted domain should have TRUE include-subdomains, got %q", withDot)
	}
	if !strings.Contains(withoutDot, "\tFALSE\t/\t") {
		t.Errorf("no-dot domain should have FALSE include-subdomains, got %q", withoutDot)
	}
}

// TestMergeCookieFilesPrefersNew verifies that cookies in the "new" file
// overwrite existing ones with the same name+domain (finding #54).
func TestMergeCookieFilesPrefersNew(t *testing.T) {
	existing := `# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	0	SAPISID	old_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	keep_me
`
	// Far-future expiry (2100-01-01): merge prunes rows whose expiry is in
	// the past, so fixtures must not use tiny epoch offsets like 100.
	newer := `# New
.youtube.com	TRUE	/	TRUE	4102444800	SAPISID	new_value
.youtube.com	TRUE	/	TRUE	4102444800	SID	added_cookie
`
	merged := mergeCookieFiles(existing, newer)

	if strings.Contains(merged, "old_value") {
		t.Errorf("expected old_value to be overwritten, got:\n%s", merged)
	}
	if !strings.Contains(merged, "new_value") {
		t.Errorf("expected new_value present, got:\n%s", merged)
	}
	if !strings.Contains(merged, "keep_me") {
		t.Errorf("expected LOGIN_INFO preserved from existing, got:\n%s", merged)
	}
	if !strings.Contains(merged, "added_cookie") {
		t.Errorf("expected new SID row added, got:\n%s", merged)
	}
}

// TestMergeCookieFilesHandlesHttpOnlyPrefix checks that #HttpOnly_ rows are
// handled for both existing and new files.
func TestMergeCookieFilesHandlesHttpOnlyPrefix(t *testing.T) {
	existing := `#HttpOnly_.youtube.com	TRUE	/	TRUE	0	SSID	old_ssid
`
	newer := `#HttpOnly_.youtube.com	TRUE	/	TRUE	4102444800	SSID	new_ssid
`
	merged := mergeCookieFiles(existing, newer)
	if strings.Contains(merged, "old_ssid") {
		t.Errorf("expected new_ssid to overwrite old_ssid, got:\n%s", merged)
	}
	if !strings.Contains(merged, "new_ssid") {
		t.Errorf("expected new_ssid present, got:\n%s", merged)
	}
}

// TestMergeCookieFilesPrunesExpiredRows: merge drops rows whose expiry is a
// positive unix timestamp in the past — otherwise a cookie name the platform
// retired lingers in cookies.txt forever and (since CookieJar.Load ignores
// expiry) keeps being sent in the Cookie header. Session cookies (expiry 0)
// and future-dated rows survive.
func TestMergeCookieFilesPrunesExpiredRows(t *testing.T) {
	existing := `# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	1000	PREF	long_dead
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	session_kept
.youtube.com	TRUE	/	TRUE	4102444800	SAPISID	future_kept
#HttpOnly_.youtube.com	TRUE	/	TRUE	1000	SSID	dead_httponly
`
	merged := mergeCookieFiles(existing, "")

	if strings.Contains(merged, "long_dead") {
		t.Errorf("expected expired PREF row pruned, got:\n%s", merged)
	}
	if strings.Contains(merged, "dead_httponly") {
		t.Errorf("expected expired #HttpOnly_ row pruned, got:\n%s", merged)
	}
	if !strings.Contains(merged, "session_kept") {
		t.Errorf("expected session cookie (expiry 0) kept, got:\n%s", merged)
	}
	if !strings.Contains(merged, "future_kept") {
		t.Errorf("expected future-dated row kept, got:\n%s", merged)
	}
}

// TestMergeCookieFilesSkipsCommentsAndBlanks ensures the parse step ignores
// comment lines and blank rows so they don't end up re-emitted as data.
func TestMergeCookieFilesSkipsCommentsAndBlanks(t *testing.T) {
	existing := `# Some comment
# Another

.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	x
`
	merged := mergeCookieFiles(existing, "")
	// The leading "# Netscape HTTP Cookie File" header is added by the
	// merge function, but "# Some comment" / "# Another" should not appear
	// as cookie rows.
	if strings.Contains(merged, "# Some comment") {
		t.Errorf("expected arbitrary comments to be dropped, got:\n%s", merged)
	}
	if !strings.Contains(merged, "LOGIN_INFO\tx") {
		t.Errorf("expected LOGIN_INFO row preserved, got:\n%s", merged)
	}
}

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
		{"__Secure-1PSID", "accounts.google.com", true},
		{"__Secure-3PSID", "accounts.google.com", true},
		// SID on a non-google domain is rejected (the YouTube essential list
		// already covers SID on youtube.com since it's in essentialYouTubeCookies).
		{"auth-token", ".twitch.tv", true},
		{"login", ".twitch.tv", true},
		// Unknown name is rejected.
		{"random", ".youtube.com", false},
		{"auth-token", ".youtube.com", false}, // twitch cookie name, not on twitch.tv
		// Note: the upstream caller gates on isRelevantDomain first, so
		// isEssentialCookie doesn't separately re-check domain relevance
		// for known YouTube names like "SID" — it trusts essential names.
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
	newer := `# New
.youtube.com	TRUE	/	TRUE	100	SAPISID	new_value
.youtube.com	TRUE	/	TRUE	100	SID	added_cookie
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
	newer := `#HttpOnly_.youtube.com	TRUE	/	TRUE	100	SSID	new_ssid
`
	merged := mergeCookieFiles(existing, newer)
	if strings.Contains(merged, "old_ssid") {
		t.Errorf("expected new_ssid to overwrite old_ssid, got:\n%s", merged)
	}
	if !strings.Contains(merged, "new_ssid") {
		t.Errorf("expected new_ssid present, got:\n%s", merged)
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

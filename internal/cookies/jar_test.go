package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCookieFile = `# Netscape HTTP Cookie File
# This is a generated file! Do not edit.

.youtube.com	TRUE	/	TRUE	0	SAPISID	abc123sapisid
.youtube.com	TRUE	/	TRUE	0	__Secure-3PAPISID	def456sapisid3p
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	logininfo123
.youtube.com	TRUE	/	TRUE	0	SID	sid123
.youtube.com	TRUE	/	TRUE	0	HSID	hsid123
#HttpOnly_.youtube.com	TRUE	/	TRUE	0	SSID	ssid123
.google.com	TRUE	/	TRUE	0	SAPISID	google_sapisid
.twitch.tv	TRUE	/	FALSE	0	auth-token	twitchtoken123
.twitch.tv	TRUE	/	FALSE	0	login	testuser
.example.com	TRUE	/	FALSE	0	irrelevant	shouldbeskipped
`

func TestLoadCookies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	// YouTube SAPISID should prefer youtube.com over google.com
	sapisid := jar.GetSapisid()
	if sapisid != "abc123sapisid" {
		t.Errorf("expected youtube.com SAPISID, got %q", sapisid)
	}

	// Login info
	if jar.GetCookie("LOGIN_INFO") != "logininfo123" {
		t.Error("expected LOGIN_INFO cookie")
	}

	// Has auth
	if !jar.HasYouTubeAuthCookies() {
		t.Error("expected HasYouTubeAuthCookies to be true")
	}

	// HttpOnly_ cookies should be parsed
	if jar.GetCookie("SSID") != "ssid123" {
		t.Error("expected SSID from HttpOnly_ line")
	}

	// Twitch
	if jar.GetTwitchAuthToken() != "twitchtoken123" {
		t.Error("expected Twitch auth token")
	}
	if !jar.HasTwitchAuthCookies() {
		t.Error("expected HasTwitchAuthCookies to be true")
	}

	// Irrelevant domain should be skipped
	if jar.GetCookie("irrelevant") != "" {
		t.Error("expected irrelevant cookie to be filtered out")
	}
}

func TestCookieHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	header := jar.GetCookieHeader()
	if header == "" {
		t.Error("expected non-empty cookie header")
	}
	if !strings.Contains(header, "SAPISID=") {
		t.Error("expected SAPISID in cookie header")
	}
}

func TestGenerateAuthorizationHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	auth := jar.GenerateAuthorizationHeader("https://www.youtube.com")
	if auth == "" {
		t.Fatal("expected non-empty auth header")
	}
	if !strings.Contains(auth, "SAPISIDHASH") {
		t.Error("expected SAPISIDHASH in auth header")
	}
}

// TestGenerateAuthorizationHeaderOriginAllowlist verifies that bogus origins
// do not produce an Authorization header — the allowlist prevents emitting a
// SAPISIDHASH bound to an attacker-controlled origin (finding #12).
func TestGenerateAuthorizationHeaderOriginAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	for _, ok := range []string{
		"https://www.youtube.com",
		"https://studio.youtube.com",
		"https://music.youtube.com",
	} {
		if jar.GenerateAuthorizationHeader(ok) == "" {
			t.Errorf("expected allowlisted origin %q to produce header", ok)
		}
	}
	for _, bad := range []string{
		"https://evil.example",
		"https://youtube.com.evil.example",
		"http://www.youtube.com", // wrong scheme
		"",
	} {
		if got := jar.GenerateAuthorizationHeader(bad); got != "" {
			t.Errorf("expected origin %q to be rejected, got %q", bad, got)
		}
	}
}

// TestMakeSidAuthorizationKnownVector locks down the SAPISIDHASH format
// against a known (timestamp, sid, origin) input so future refactors can't
// silently break Google's hash scheme (finding #58).
func TestMakeSidAuthorizationKnownVector(t *testing.T) {
	// sha1("1234567890 test-sid https://www.youtube.com") computed once:
	// printf '%s' "1234567890 test-sid https://www.youtube.com" | sha1sum
	// = 96d13ea75f0bf102d71aebdfdaa599807584b65c
	got := makeSidAuthorization("SAPISIDHASH", "test-sid", "https://www.youtube.com", 1234567890)
	want := "SAPISIDHASH 1234567890_96d13ea75f0bf102d71aebdfdaa599807584b65c"
	if got != want {
		t.Errorf("SAPISIDHASH mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEmptyJar(t *testing.T) {
	jar := NewCookieJar()
	if jar.HasYouTubeAuthCookies() {
		t.Error("empty jar should not have auth cookies")
	}
	if jar.GetCookieHeader() != "" {
		t.Error("empty jar should have empty cookie header")
	}
}

func TestNonExistentFile(t *testing.T) {
	jar := NewCookieJar()
	err := jar.Load("/nonexistent/path/cookies.txt")
	if err != nil {
		t.Error("loading non-existent file should not error")
	}
	if !jar.IsEmpty() {
		t.Error("should be empty after loading non-existent file")
	}
}

func TestMalformedCookieLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	// Test with malformed lines: too few fields, empty name, empty domain
	content := `# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	0	SAPISID	valid_sapisid
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	valid_login
short_line	only_two_fields
	TRUE	/	TRUE	0		empty_name_value
.youtube.com	TRUE	/	TRUE	0		empty_cookie_name
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	// Valid cookies should be loaded
	if jar.GetCookie("SAPISID") != "valid_sapisid" {
		t.Error("expected valid SAPISID cookie")
	}
	if jar.GetCookie("LOGIN_INFO") != "valid_login" {
		t.Error("expected valid LOGIN_INFO cookie")
	}
	// Empty name cookie should be skipped
	if jar.GetCookie("") != "" {
		t.Error("empty-name cookie should not be loaded")
	}
}

func TestReloadCookies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	initial := `.youtube.com	TRUE	/	TRUE	0	SAPISID	first_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	first_login
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	if jar.GetCookie("SAPISID") != "first_value" {
		t.Error("expected first_value")
	}

	// Update cookie file
	updated := `.youtube.com	TRUE	/	TRUE	0	SAPISID	second_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	second_login
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := jar.Reload(); err != nil {
		t.Fatal(err)
	}

	if jar.GetCookie("SAPISID") != "second_value" {
		t.Error("expected second_value after reload")
	}
}

// TestDomainMatchers locks down the suffix-anchored domain matchers so the
// substring-match hazard (e.g. "fakegoogle.com.evil.tld") does not regress
// (finding #39 in reports/cookies.md).
func TestDomainMatchers(t *testing.T) {
	cases := []struct {
		fn     func(string) bool
		name   string
		domain string
		want   bool
	}{
		// isYouTubeDomain
		{isYouTubeDomain, "youtube exact", "youtube.com", true},
		{isYouTubeDomain, "youtube subdomain", "www.youtube.com", true},
		{isYouTubeDomain, "youtube subdomain with dot prefix", ".www.youtube.com", true},
		{isYouTubeDomain, "youtube music subdomain", "music.youtube.com", true},
		{isYouTubeDomain, "evil substring", "fakeyoutube.com.evil.tld", false},
		{isYouTubeDomain, "youtube in middle", "foo.youtube.com.bar", false},
		{isYouTubeDomain, "unrelated", "example.com", false},
		// isGoogleDomain
		{isGoogleDomain, "google exact", "google.com", true},
		{isGoogleDomain, "accounts.google.com", "accounts.google.com", true},
		{isGoogleDomain, "fakegoogle", "fakegoogle.com", false},
		{isGoogleDomain, "google substring not subdomain", "notgoogle.com", false},
		// isTwitchDomain
		{isTwitchDomain, "twitch exact", "twitch.tv", true},
		{isTwitchDomain, "twitch subdomain", "api.twitch.tv", true},
		{isTwitchDomain, "twitch substring", "twitch.tv.evil.tld", false},
	}
	for _, tc := range cases {
		if got := tc.fn(tc.domain); got != tc.want {
			t.Errorf("%s: %q -> got %v, want %v", tc.name, tc.domain, got, tc.want)
		}
	}
}

// TestLoadRejectsLookalikeDomains ensures a cookie entry whose domain merely
// embeds "youtube.com" as a substring is filtered out during Load so it
// cannot leak into the Cookie header (finding #39).
func TestLoadRejectsLookalikeDomains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := `# Netscape HTTP Cookie File
fakeyoutube.com.evil.tld	TRUE	/	TRUE	0	SAPISID	attacker_value
.youtube.com	TRUE	/	TRUE	0	SAPISID	legit_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	legit_login
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if jar.GetSapisid() != "legit_value" {
		t.Errorf("lookalike domain must not win SAPISID, got %q", jar.GetSapisid())
	}
}

// TestLoadPreservesStateOnReadError verifies that Load does not clobber
// previously loaded cookies when a subsequent read fails for a reason other
// than "file does not exist" (finding #28 in reports/cookies.md).
func TestLoadPreservesStateOnReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if jar.GetSapisid() != "abc123sapisid" {
		t.Fatal("expected SAPISID loaded before error")
	}

	// Point at a directory to force a read error that isn't os.IsNotExist.
	if err := jar.Load(dir); err == nil {
		t.Fatal("expected error loading a directory")
	}

	// State must be preserved: valid auth cookies from the first Load are still present.
	if got := jar.GetSapisid(); got != "abc123sapisid" {
		t.Errorf("expected jar state preserved after failed Load, got SAPISID=%q", got)
	}
	if !jar.HasYouTubeAuthCookies() {
		t.Error("expected HasYouTubeAuthCookies still true after failed Load")
	}
}

// TestLoadTabInValue ensures values containing tabs are preserved rather
// than truncated at the first tab (finding #16 in reports/cookies.md).
func TestLoadTabInValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	// SAPISID value contains a literal tab mid-string; strings.Split would
	// leave 8 parts instead of 7 — the fix joins parts[6:] with "\t".
	content := ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tpart1\tpart2\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetSapisid(); got != "part1\tpart2" {
		t.Errorf("expected SAPISID with embedded tab preserved, got %q", got)
	}
}

type recordingLogger struct {
	debugs [][]any
}

func (r *recordingLogger) Debug(msg string, args ...any) {
	r.debugs = append(r.debugs, append([]any{msg}, args...))
}

// TestLoadLogsMalformedLines ensures SetLogger + a malformed line produces
// a Debug log (finding #16 in reports/cookies.md).
func TestLoadLogsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tok\n" +
		"malformed\tline\twith\ttoo\tfew\tfields\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := &recordingLogger{}
	jar := NewCookieJar()
	jar.SetLogger(logger)
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	if len(logger.debugs) == 0 {
		t.Fatal("expected at least one Debug log for malformed line")
	}
	foundMalformed := false
	for _, entry := range logger.debugs {
		if msg, ok := entry[0].(string); ok && strings.Contains(msg, "malformed") {
			foundMalformed = true
			break
		}
	}
	if !foundMalformed {
		t.Errorf("expected Debug log containing 'malformed', got %v", logger.debugs)
	}
}

func TestSapisidCookieVariants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	content := `.youtube.com	TRUE	/	TRUE	0	__Secure-1PAPISID	sapisid1p_val
.youtube.com	TRUE	/	TRUE	0	__Secure-3PAPISID	sapisid3p_val
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	logininfo
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	// Without SAPISID, GetSapisid should fall back to __Secure-3PAPISID
	if jar.GetSapisid() != "sapisid3p_val" {
		t.Errorf("expected sapisid3p_val, got %q", jar.GetSapisid())
	}

	sapisid, sapisid1p, sapisid3p := jar.GetSapisidCookies()
	if sapisid != "sapisid3p_val" {
		t.Errorf("expected sapisid3p_val fallback, got %q", sapisid)
	}
	if sapisid1p != "sapisid1p_val" {
		t.Errorf("expected sapisid1p_val, got %q", sapisid1p)
	}
	if sapisid3p != "sapisid3p_val" {
		t.Errorf("expected sapisid3p_val, got %q", sapisid3p)
	}
}

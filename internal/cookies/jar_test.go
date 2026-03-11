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

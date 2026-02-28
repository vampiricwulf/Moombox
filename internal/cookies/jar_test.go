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
	if !jar.HasAuthCookies() {
		t.Error("expected HasAuthCookies to be true")
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
	if jar.HasAuthCookies() {
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

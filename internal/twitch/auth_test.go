package twitch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// nopLogger satisfies Auth's anonymous logger interface without producing output.
type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// jarFromRows writes a synthetic Netscape cookie file and loads a jar from it.
// Values are synthetic throughout — no real cookie value is ever written here.
func jarFromRows(t *testing.T, rows ...string) *cookies.CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

func row(domain, name, value string) string {
	return strings.Join([]string{domain, "TRUE", "/", "TRUE", "0", name, value}, "\t")
}

// TestGetAuthTokenReadsTheTwitchCookie pins this package's end of the cookie
// jar's platform split.
//
// Auth.GetAuthToken is the ONLY caller of CookieJar.GetCookie in the tree, and
// GetCookie's name reads generic while its behaviour is Twitch-specific. If
// either side is repointed at the YouTube jar this call returns "", and the
// failure is SILENT: chat_irc.go sends "PASS SCHMOOPIIE" for an empty token
// rather than erroring, so chat still connects and merely stops receiving
// subscriber-only messages and badges. Nothing logs a fault, so only a test
// catches it.
//
// The fixture carries YouTube rows too, so "" here cannot be explained by an
// empty jar.
func TestGetAuthTokenReadsTheTwitchCookie(t *testing.T) {
	jar := jarFromRows(t,
		row(".twitch.tv", "auth-token", "synthetic-twitch-token"),
		row(".twitch.tv", "login", "synthetic-user"),
		row(".youtube.com", "SAPISID", "synthetic-youtube-cookie"),
		row(".youtube.com", "LOGIN_INFO", "synthetic-youtube-cookie"),
	)
	a := NewAuth(jar, nopLogger{})

	if got := a.GetAuthToken(); got != "synthetic-twitch-token" {
		t.Errorf("GetAuthToken() = %q, want %q — the IRC client would fall back to anonymous login "+
			"(PASS SCHMOOPIIE) without erroring, silently losing subscriber-only chat", got, "synthetic-twitch-token")
	}
	if !a.HasAuthToken() {
		t.Error("HasAuthToken() = false on a jar that holds a Twitch auth-token")
	}
}

// TestGetAuthTokenIsEmptyWithoutATwitchCookie is the other half: a jar holding
// ONLY YouTube cookies must yield no Twitch token. A reader that consulted the
// YouTube jar would still return "" here for a name YouTube does not use, so
// the fixture plants a .youtube.com row literally named "auth-token" — which
// Load refuses, because Twitch's cookie names are only honoured on twitch.tv.
func TestGetAuthTokenIsEmptyWithoutATwitchCookie(t *testing.T) {
	jar := jarFromRows(t,
		row(".youtube.com", "auth-token", "a-youtube-row-wearing-the-twitch-name"),
		row(".youtube.com", "SAPISID", "synthetic-youtube-cookie"),
	)
	a := NewAuth(jar, nopLogger{})

	if got := a.GetAuthToken(); got != "" {
		t.Errorf("GetAuthToken() = %q, want empty — a youtube.com row named auth-token is not a "+
			"Twitch credential and must never be presented as one", got)
	}
	if a.HasAuthToken() {
		t.Error("HasAuthToken() = true without a twitch.tv auth-token")
	}
}

// TestGetAuthTokenNilJar: the guard in GetAuthToken, so a service constructed
// before cookies are wired cannot panic.
func TestGetAuthTokenNilJar(t *testing.T) {
	a := NewAuth(nil, nopLogger{})
	if got := a.GetAuthToken(); got != "" {
		t.Errorf("GetAuthToken() = %q on a nil jar, want empty", got)
	}
	if a.HasAuthToken() {
		t.Error("HasAuthToken() = true on a nil jar")
	}
}

// TestGetLoginReadsTheTwitchCookie is the NICK half of the same cookie-jar
// contract. The login names the account the IRC session identifies as, and it
// must come from the same twitch.tv rows as the auth-token so the pair belongs
// to one session.
//
// The fixture carries YouTube rows too, so "" here cannot be explained by an
// empty jar — and a "" here is silent: chat_irc.go falls all the way back to
// the anonymous handshake rather than erroring.
func TestGetLoginReadsTheTwitchCookie(t *testing.T) {
	jar := jarFromRows(t,
		row(".twitch.tv", "auth-token", "synthetic-twitch-token"),
		row(".twitch.tv", "login", "syntheticuser"),
		row(".youtube.com", "SAPISID", "synthetic-youtube-cookie"),
		row(".youtube.com", "LOGIN_INFO", "synthetic-youtube-cookie"),
	)
	a := NewAuth(jar, nopLogger{})

	if got := a.GetLogin(); got != "syntheticuser" {
		t.Errorf("GetLogin() = %q, want %q — without it the IRC handshake sends the anonymous "+
			"justinfan nickname and the session captures no subscriber-only chat", got, "syntheticuser")
	}
}

// TestGetLoginIsEmptyWithoutATwitchCookie: "login" is a plausible cookie name
// on any site, so the fixture plants a .youtube.com row literally named "login"
// — which Load refuses, because Twitch's cookie names are only honoured on
// twitch.tv. Presenting another site's "login" as a Twitch nickname would pair
// a real Twitch token with a foreign identity.
func TestGetLoginIsEmptyWithoutATwitchCookie(t *testing.T) {
	jar := jarFromRows(t,
		row(".youtube.com", "login", "a-youtube-row-wearing-the-twitch-name"),
		row(".youtube.com", "SAPISID", "synthetic-youtube-cookie"),
	)
	a := NewAuth(jar, nopLogger{})

	if got := a.GetLogin(); got != "" {
		t.Errorf("GetLogin() = %q, want empty — a youtube.com row named login is not a Twitch "+
			"identity and must never be presented as one", got)
	}
}

// TestGetLoginNilJar: the guard in GetLogin, mirroring GetAuthToken's, so a
// service constructed before cookies are wired cannot panic.
func TestGetLoginNilJar(t *testing.T) {
	a := NewAuth(nil, nopLogger{})
	if got := a.GetLogin(); got != "" {
		t.Errorf("GetLogin() = %q on a nil jar, want empty", got)
	}
}

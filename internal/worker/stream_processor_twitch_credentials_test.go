package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// These tests pin the wiring end of the Twitch IRC handshake's credential
// pair. internal/twitch owns the rule that PASS and NICK agree; this file owns
// the rule that the worker never hands the downloader half a pair.
//
// Every credential below is synthetic and none is ever logged.

// writeTwitchSessionCookies writes a Netscape cookie file holding one synthetic
// Twitch session: the auth-token and the login it belongs to.
func writeTwitchSessionCookies(t *testing.T, path, token, login string) {
	t.Helper()
	rows := []string{
		strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t"),
		strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", login}, "\t"),
	}
	content := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTwitchChatCredentialsReturnsBothHalves is the claim: a configured Auth
// yields BOTH getters, each reading its own cookie.
//
// Asserting only that the token getter is non-nil is the failure mode — that is
// exactly the state that shipped, and it produces `PASS oauth:<token>` beside
// the anonymous `justinfan<random>` nickname, which Twitch refuses or silently
// downgrades. So both getters are invoked and both values are checked.
func TestTwitchChatCredentialsReturnsBothHalves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchSessionCookies(t, path, "synthetic-token", "syntheticaccount")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	token, login := twitchChatCredentials(twitch.NewAuth(jar, nopWorkerLogger{}))
	if token == nil {
		t.Fatal("token getter is nil for a configured Auth")
	}
	if login == nil {
		t.Fatal("login getter is nil for a configured Auth — the IRC handshake would send the " +
			"anonymous justinfan nickname alongside a real OAuth token")
	}
	if got := token(); got != "synthetic-token" {
		t.Errorf("token getter returned %q, want %q", got, "synthetic-token")
	}
	if got := login(); got != "syntheticaccount" {
		t.Errorf("login getter returned %q, want %q", got, "syntheticaccount")
	}
}

// TestTwitchChatCredentialsTrackTheJar: both getters are METHOD VALUES, so a
// cookie re-import underneath a running job moves both halves together. A
// snapshot of either would leave the next reconnect presenting one session's
// credential under another session's identity.
func TestTwitchChatCredentialsTrackTheJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchSessionCookies(t, path, "token-before", "accountbefore")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	token, login := twitchChatCredentials(twitch.NewAuth(jar, nopWorkerLogger{}))
	if got, want := token(), "token-before"; got != want {
		t.Fatalf("token getter returned %q, want %q", got, want)
	}
	if got, want := login(), "accountbefore"; got != want {
		t.Fatalf("login getter returned %q, want %q", got, want)
	}

	writeTwitchSessionCookies(t, path, "token-after", "accountafter")
	if err := jar.Reload(); err != nil {
		t.Fatal(err)
	}

	if got, want := token(), "token-after"; got != want {
		t.Errorf("token getter returned %q after a re-import, want %q", got, want)
	}
	if got, want := login(), "accountafter"; got != want {
		t.Errorf("login getter returned %q after a re-import, want %q — the credential moved and "+
			"the identity did not", got, want)
	}
}

// TestTwitchChatCredentialsNilAuthIsFullyAnonymous: neither half without the
// other. A service constructed before cookies are wired yields two nils, which
// the downloader reads as the anonymous login on both lines — never a token
// with no identity behind it, and never an identity with no token.
func TestTwitchChatCredentialsNilAuthIsFullyAnonymous(t *testing.T) {
	token, login := twitchChatCredentials(nil)
	if token != nil {
		t.Error("token getter is non-nil for a nil Auth")
	}
	if login != nil {
		t.Error("login getter is non-nil for a nil Auth")
	}
}

// nopWorkerLogger satisfies twitch.NewAuth's anonymous logger interface.
type nopWorkerLogger struct{}

func (nopWorkerLogger) Debug(string, ...any) {}
func (nopWorkerLogger) Info(string, ...any)  {}
func (nopWorkerLogger) Warn(string, ...any)  {}
func (nopWorkerLogger) Error(string, ...any) {}

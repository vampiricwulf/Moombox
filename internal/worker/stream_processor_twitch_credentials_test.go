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
// yields ONE getter that produces BOTH halves, each read from its own cookie.
//
// Asserting only that the getter is non-nil is the failure mode. A wiring that
// supplied a token and no login produces `PASS oauth:<token>` beside the
// anonymous `justinfan<random>` nickname, which Twitch refuses or silently
// downgrades — so both values are pulled and both are checked.
func TestTwitchChatCredentialsReturnsBothHalves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchSessionCookies(t, path, "synthetic-token", "syntheticaccount")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	credentials := twitchChatCredentials(twitch.NewAuth(jar, nopWorkerLogger{}))
	if credentials == nil {
		t.Fatal("credential getter is nil for a configured Auth")
	}
	token, login := credentials()
	if token != "synthetic-token" {
		t.Errorf("token = %q, want %q", token, "synthetic-token")
	}
	if login != "syntheticaccount" {
		t.Errorf("login = %q, want %q — the IRC handshake would send the anonymous justinfan "+
			"nickname alongside a real OAuth token", login, "syntheticaccount")
	}
}

// TestTwitchChatCredentialsTrackTheJar: the getter is a METHOD VALUE, so a
// cookie re-import underneath a running job moves both halves together. A
// snapshot would leave the next reconnect presenting one session's credential
// under another session's identity.
func TestTwitchChatCredentialsTrackTheJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchSessionCookies(t, path, "token-before", "accountbefore")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	credentials := twitchChatCredentials(twitch.NewAuth(jar, nopWorkerLogger{}))
	if token, login := credentials(); token != "token-before" || login != "accountbefore" {
		t.Fatalf("credentials = (%q, %q), want (%q, %q)",
			token, login, "token-before", "accountbefore")
	}

	writeTwitchSessionCookies(t, path, "token-after", "accountafter")
	if err := jar.Reload(); err != nil {
		t.Fatal(err)
	}

	token, login := credentials()
	if token != "token-after" {
		t.Errorf("token = %q after a re-import, want %q", token, "token-after")
	}
	if login != "accountafter" {
		t.Errorf("login = %q after a re-import, want %q — the credential moved and the identity "+
			"did not", login, "accountafter")
	}
}

// TestTwitchChatCredentialsNilAuthIsFullyAnonymous: no half without the other.
// A service constructed before cookies are wired yields a nil getter, which the
// downloader reads as the anonymous login on both lines — never a token with no
// identity behind it, and never an identity with no token.
func TestTwitchChatCredentialsNilAuthIsFullyAnonymous(t *testing.T) {
	if credentials := twitchChatCredentials(nil); credentials != nil {
		token, login := credentials()
		t.Errorf("credential getter is non-nil for a nil Auth (yields %q, %q)", token, login)
	}
}

// nopWorkerLogger satisfies twitch.NewAuth's anonymous logger interface.
type nopWorkerLogger struct{}

func (nopWorkerLogger) Debug(string, ...any) {}
func (nopWorkerLogger) Info(string, ...any)  {}
func (nopWorkerLogger) Warn(string, ...any)  {}
func (nopWorkerLogger) Error(string, ...any) {}

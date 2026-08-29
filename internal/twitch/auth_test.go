package twitch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
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

// TestGetCredentialsReadsTheTwitchCookies is the IRC handshake's half of the
// cookie-jar contract. The login names the account the session identifies as
// and must come from the same twitch.tv rows as the auth-token, so both halves
// are asserted from ONE call — which is also the only way they are reachable.
//
// The fixture carries YouTube rows too, so an empty half here cannot be
// explained by an empty jar. An empty half is silent: chat_irc.go falls all the
// way back to the anonymous handshake rather than erroring.
func TestGetCredentialsReadsTheTwitchCookies(t *testing.T) {
	jar := jarFromRows(t,
		row(".twitch.tv", "auth-token", "synthetic-twitch-token"),
		row(".twitch.tv", "login", "syntheticuser"),
		row(".youtube.com", "SAPISID", "synthetic-youtube-cookie"),
		row(".youtube.com", "LOGIN_INFO", "synthetic-youtube-cookie"),
	)
	a := NewAuth(jar, nopLogger{})

	token, login := a.GetCredentials()
	if token != "synthetic-twitch-token" {
		t.Errorf("GetCredentials() token = %q, want %q", token, "synthetic-twitch-token")
	}
	if login != "syntheticuser" {
		t.Errorf("GetCredentials() login = %q, want %q — without it the IRC handshake sends the "+
			"anonymous justinfan nickname and the session captures no subscriber-only chat",
			login, "syntheticuser")
	}
}

// TestGetCredentialsIsEmptyWithoutTwitchCookies: "login" is a plausible cookie
// name on any site, so the fixture plants .youtube.com rows literally named
// "login" and "auth-token" — which Load refuses, because Twitch's cookie names
// are only honoured on twitch.tv. Presenting another site's rows as a Twitch
// session would authenticate as nobody.
func TestGetCredentialsIsEmptyWithoutTwitchCookies(t *testing.T) {
	jar := jarFromRows(t,
		row(".youtube.com", "login", "a-youtube-row-wearing-the-twitch-name"),
		row(".youtube.com", "auth-token", "another-youtube-row-wearing-a-twitch-name"),
		row(".youtube.com", "SAPISID", "synthetic-youtube-cookie"),
	)
	a := NewAuth(jar, nopLogger{})

	token, login := a.GetCredentials()
	if token != "" || login != "" {
		t.Errorf("GetCredentials() = (%q, %q), want both empty — youtube.com rows wearing Twitch "+
			"cookie names are not a Twitch session and must never be presented as one", token, login)
	}
}

// TestGetCredentialsNilJar: the guard in GetCredentials, mirroring
// GetAuthToken's, so a service constructed before cookies are wired cannot
// panic.
func TestGetCredentialsNilJar(t *testing.T) {
	a := NewAuth(nil, nopLogger{})
	token, login := a.GetCredentials()
	if token != "" || login != "" {
		t.Errorf("GetCredentials() = (%q, %q) on a nil jar, want both empty", token, login)
	}
}

// --- what ValidateToken is allowed to say ---
//
// Both claims below are about a log line, and both failures are invisible from
// inside the program: the code works, and the damage is whatever ends up in
// the operator's log file or CI output. Only a test that reads the rendered
// error and the rendered attribute set can see either.

// startValidateStub points constants.TwitchURLs.OAuthValidate at a local
// server answering with a fixed status, content type and body, and restores
// the endpoint afterwards. No request ever leaves the machine.
func startValidateStub(t *testing.T, status int, contentType, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	prev := constants.TwitchURLs.OAuthValidate
	constants.TwitchURLs.OAuthValidate = srv.URL
	t.Cleanup(func() { constants.TwitchURLs.OAuthValidate = prev })
}

// authWithSyntheticToken builds an Auth over a jar holding one synthetic
// Twitch auth-token, which is all ValidateToken needs to get past its
// empty-token early return.
func authWithSyntheticToken(t *testing.T, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Auth {
	t.Helper()
	return NewAuth(jarFromRows(t, row(".twitch.tv", "auth-token", "synthetic-twitch-token")), logger)
}

// TestValidateTokenErrorNeverEchoesAnUntrustedBody pins what an unexpected
// status may carry back to its caller.
//
// The body of a non-200 here is usually not Twitch speaking at all — it is
// whatever sits between Moombox and id.twitch.tv. The two answers that turn up
// in practice are a captive portal or corporate proxy's HTML sign-in page, and
// a service error page that echoes the request that produced it, Authorization
// header included. Every caller logs this error, so interpolating that body
// whole writes an intermediary's markup — and, in the echo case, this
// install's own bearer token — into the log.
//
// The rule is by CONTENT TYPE, so each case names one: only the two types that
// cannot carry markup contribute a body, and only a prefix of it.
func TestValidateTokenErrorNeverEchoesAnUntrustedBody(t *testing.T) {
	// A page of the exact shape that motivates this: markup, plus an echo of
	// the request headers with the bearer token among them.
	htmlEcho := "<!doctype html><html><head><title>503</title></head><body>" +
		"<h1>Service Unavailable</h1><pre>Authorization: OAuth synthetic-twitch-token</pre>" +
		"</body></html>"

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		wantBody    bool
	}{
		{"HTML error page echoing the request", "text/html; charset=utf-8", htmlEcho, false},
		{"HTML with no charset", "text/html", htmlEcho, false},
		{"XML", "application/xml", "<error><token>synthetic-twitch-token</token></error>", false},
		{"unparseable content type", "this is not a media type ;;;", htmlEcho, false},
		{"plain text", "text/plain; charset=utf-8", "rate limited: retry after 30s", true},
		{"JSON", "application/json", `{"status":503,"message":"backend unavailable"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startValidateStub(t, http.StatusServiceUnavailable, tc.contentType, tc.body)
			auth := authWithSyntheticToken(t, nopLogger{})

			ok, err := auth.ValidateToken(context.Background())
			if err == nil {
				t.Fatal("ValidateToken returned no error for a 503 — an unexpected status must stay inconclusive")
			}
			if ok {
				t.Error("ValidateToken = true on a 503")
			}
			got := err.Error()

			if !strings.Contains(got, "503") {
				t.Errorf("error = %q, want the status code in it — it is the whole diagnostic", got)
			}
			if strings.Contains(got, "synthetic-twitch-token") {
				t.Errorf("error echoed the bearer token back into the log: %q", got)
			}
			if strings.Contains(got, "<") {
				t.Errorf("error = %q, want no markup — an intermediary's page must never reach the log", got)
			}
			// The body travels %q-rendered — a text/plain answer may hold
			// newlines and a log line must stay one line — so the expectation
			// is the quoted form, not the raw bytes.
			if tc.wantBody && !strings.Contains(got, fmt.Sprintf("%q", tc.body)) {
				t.Errorf("error = %q, want it to carry the %s body %q — a type that cannot hold "+
					"markup is exactly what is safe to report", got, tc.contentType, tc.body)
			}
			if !tc.wantBody && strings.Contains(got, "body \"") {
				t.Errorf("error = %q, want no body at all for content type %q", got, tc.contentType)
			}
		})
	}
}

// TestValidateTokenErrorTruncatesAReportableBody: "safe type" is not "any
// length". A JSON body is reportable, but it is still remote input and a
// caller logs whatever comes back, so only a prefix may travel.
//
// The old code read to 1 MB, so the fixture is a body far past the cap with a
// marker at each end: the head must survive, the tail must not.
func TestValidateTokenErrorTruncatesAReportableBody(t *testing.T) {
	body := `{"head-marker":"` + strings.Repeat("x", 4096) + `","tail-marker":"1"}`
	startValidateStub(t, http.StatusServiceUnavailable, "application/json", body)
	auth := authWithSyntheticToken(t, nopLogger{})

	_, err := auth.ValidateToken(context.Background())
	if err == nil {
		t.Fatal("ValidateToken returned no error for a 503")
	}
	got := err.Error()

	if !strings.Contains(got, "head-marker") {
		t.Errorf("error = %q, want the start of the body — truncation must not mean silence", got)
	}
	if strings.Contains(got, "tail-marker") {
		t.Errorf("error carried the whole %d-byte body: %.300q", len(body), got)
	}
	// The prefix is %q-rendered, which can grow an escape-heavy body, so the
	// bound is a multiple of the SOURCE budget rather than an exact length: a
	// cap that had drifted to kilobytes still blows through this.
	if len(got) > 4*validateErrorBodyPrefix {
		t.Errorf("error is %d bytes long, want one bounded by the %d-byte body prefix: %.300q",
			len(got), validateErrorBodyPrefix, got)
	}
}

// TestValidateTokenErrorClampsAnAbsurdContentType: the media type is remote
// input too.
//
// It is the one field reported unconditionally, including for the types whose
// body is withheld — so a header long enough to be a payload would walk
// straight past the body rule and into the log. A well-formed type/subtype of
// four kilobytes is a valid parse, so the parser is not the bound.
func TestValidateTokenErrorClampsAnAbsurdContentType(t *testing.T) {
	startValidateStub(t, http.StatusServiceUnavailable, "text/"+strings.Repeat("z", 4096), "body")
	auth := authWithSyntheticToken(t, nopLogger{})

	_, err := auth.ValidateToken(context.Background())
	if err == nil {
		t.Fatal("ValidateToken returned no error for a 503")
	}
	got := err.Error()
	if !strings.Contains(got, "503") {
		t.Errorf("error = %.200q, want the status code in it", got)
	}
	if len(got) > 200 {
		t.Errorf("error is %d bytes long — the content type was reported unclamped: %.200q", len(got), got)
	}
}

// recordingDebugLogger captures Debug calls with their attribute lists, which
// is the only way to assert that a value is ABSENT from a log line.
type recordingDebugLogger struct {
	mu    sync.Mutex
	lines []loggedDebugLine
}

type loggedDebugLine struct {
	msg  string
	args []any
}

func (l *recordingDebugLogger) Debug(msg string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, loggedDebugLine{msg: msg, args: append([]any(nil), args...)})
	l.mu.Unlock()
}
func (l *recordingDebugLogger) Info(string, ...any)  {}
func (l *recordingDebugLogger) Warn(string, ...any)  {}
func (l *recordingDebugLogger) Error(string, ...any) {}

func (l *recordingDebugLogger) debugLines() []loggedDebugLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]loggedDebugLine(nil), l.lines...)
}

// TestValidateTokenSuccessLineDoesNotNameTheLogin.
//
// The login is a CREDENTIAL HALF, not a display name: it is the IRC NICK this
// install authenticates chat under, and every line of the handshake work is
// careful never to log it. This line runs on the ordinary success path — every
// auth check, forever — so a debug-level install wrote the account name to
// disk on a cadence. The user id stays: an opaque number is not a credential,
// and it is what a support question actually needs.
//
// Asserted over the whole rendered line, message and attributes both, so
// moving the value from an attribute into the message would not pass either.
func TestValidateTokenSuccessLineDoesNotNameTheLogin(t *testing.T) {
	startValidateStub(t, http.StatusOK, "application/json",
		`{"client_id":"synthetic-client","login":"syntheticaccount","user_id":"123456789"}`)
	logger := &recordingDebugLogger{}
	auth := authWithSyntheticToken(t, logger)

	ok, err := auth.ValidateToken(context.Background())
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if !ok {
		t.Fatal("ValidateToken = false on a 200 — the success path is what this test is about")
	}

	lines := logger.debugLines()
	if len(lines) != 1 {
		t.Fatalf("Debug was called %d times, want exactly one validated line", len(lines))
	}
	rendered := strings.ToLower(fmt.Sprint(append([]any{lines[0].msg}, lines[0].args...)...))
	if strings.Contains(rendered, "syntheticaccount") {
		t.Errorf("the success log line named the account login: %q", rendered)
	}
	if strings.Contains(rendered, "login") {
		t.Errorf("the success log line still carries a login attribute: %q", rendered)
	}
	if !strings.Contains(rendered, "123456789") {
		t.Errorf("log line = %q, want the user id kept — dropping it too would leave support nothing", rendered)
	}
}

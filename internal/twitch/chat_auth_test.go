package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// These tests pin two claims about Twitch chat credentials.
//
//  1. They are read AT USE TIME, not captured at construction — on the IRC
//     path per session, on the VOD comment path per page.
//  2. The IRC handshake's PASS and NICK lines agree about whether the session
//     is authenticated. Twitch binds the session to the token's user through
//     NICK, so `PASS oauth:<token>` beside the anonymous `justinfan<random>`
//     nickname is refused or downgraded.
//
// Both failures are SILENT: chat still connects, and merely stops carrying
// subscriber-only messages and badges. Only a test sees them, and only a test
// that reads BOTH handshake lines sees the second.
//
// Every credential below is synthetic and none is ever logged.

// ircSessionRecorder is a local websocket server standing in for Twitch IRC.
// It records the messages of each accepted connection and publishes them on a
// channel, one entry per connection.
type ircSessionRecorder struct {
	server   *httptest.Server
	sessions chan []string
}

// startIRCRecorder brings up the server AND repoints the IRC endpoint at it for
// the duration of the test. wantMessages is how many messages to read before
// the handler stops (the handshake is PASS, NICK, CAP REQ, JOIN).
func startIRCRecorder(t *testing.T, wantMessages int) *ircSessionRecorder {
	t.Helper()
	rec := &ircSessionRecorder{sessions: make(chan []string, 8)}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		var lines []string
		for len(lines) < wantMessages {
			_, data, readErr := conn.Read(r.Context())
			if readErr != nil {
				break
			}
			lines = append(lines, string(data))
		}
		rec.sessions <- lines
	}))
	t.Cleanup(rec.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(rec.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return rec
}

// nextSession returns the messages of the next accepted connection.
func (rec *ircSessionRecorder) nextSession(t *testing.T) []string {
	t.Helper()
	select {
	case lines := <-rec.sessions:
		return lines
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an IRC session to be recorded")
		return nil
	}
}

// runOneIRCSession drives exactly one handshake. runIRCSession's read loop is
// gated on IsRunning, which is false unless Start set it, so the session writes
// its handshake and returns — which is precisely the code under test.
func runOneIRCSession(t *testing.T, cd *ChatDownloader) {
	t.Helper()
	if err := cd.runIRCSession(context.Background()); err != nil {
		t.Fatalf("runIRCSession: %v", err)
	}
}

// newAuthTestChatDownloader builds an IRC downloader with the given credential
// getters and nothing else that touches disk.
func newAuthTestChatDownloader(t *testing.T, token, login func() string) *ChatDownloader {
	t.Helper()
	return NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin:   "testchan",
		ChannelDisplay: "TestChan",
		StreamID:       "stream-1",
		AuthToken:      token,
		Login:          login,
		OutputPath:     filepath.Join(t.TempDir(), "chat.json"),
	}, &testLogger{})
}

// staticGetter is a credential that never changes.
func staticGetter(v string) func() string { return func() string { return v } }

// tokenSequence yields values in order, repeating the last once exhausted —
// a stand-in for a credential that rotates underneath a running job.
func tokenSequence(values ...string) func() string {
	var mu sync.Mutex
	i := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		v := values[min(i, len(values)-1)]
		i++
		return v
	}
}

// handshakeLines returns the PASS and NICK messages of one recorded session.
//
// Both, always. "An IRC handshake happened" is a junction several handshakes
// satisfy, and the defect these tests exist to pin — a real OAuth token
// presented beside the anonymous justinfan nickname — is invisible to any
// assertion that inspects one line and not the other.
func handshakeLines(t *testing.T, lines []string) (pass, nick string) {
	t.Helper()
	if len(lines) < 2 {
		t.Fatalf("session recorded %d messages (%q), want at least PASS then NICK", len(lines), lines)
	}
	return lines[0], lines[1]
}

// anonymousNickPattern is the EXACT rendering of the anonymous nickname:
// literally "justinfan" plus rand.IntN(100000)'s decimal digits and nothing
// else. Anchored and digit-typed rather than prefix-matched, so a nick that
// merely starts with the anonymous convention and then carries an account
// name — or any other extension of it — fails here.
var anonymousNickPattern = regexp.MustCompile(`^NICK justinfan[0-9]{1,5}$`)

// assertAnonymousHandshake pins BOTH halves of the cookieless login that most
// installs run on. Byte-identical to the behaviour that shipped before the
// login cookie existed.
func assertAnonymousHandshake(t *testing.T, lines []string) {
	t.Helper()
	pass, nick := handshakeLines(t, lines)
	if pass != "PASS SCHMOOPIIE" {
		t.Errorf("PASS = %q, want %q — the anonymous login is load-bearing for the "+
			"majority of installs, which hold no Twitch cookies at all", pass, "PASS SCHMOOPIIE")
	}
	if !anonymousNickPattern.MatchString(nick) {
		t.Errorf("NICK = %q, want the anonymous form NICK justinfan<digits>", nick)
	}
}

// assertNotOnTheWire fails if a credential appears in ANY recorded message. It
// is the negative half of the anonymous assertions: "PASS SCHMOOPIIE" being
// correct does not by itself prove the token was withheld from the rest of the
// handshake.
func assertNotOnTheWire(t *testing.T, lines []string, secret string) {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, secret) {
			t.Errorf("handshake message %d leaked a credential the session must not present: %q", i, line)
		}
	}
}

// TestIRCHandshakeAuthenticatesUnderTheAccountNickname is the headline claim,
// and it asserts BOTH lines of one handshake on purpose.
//
// Twitch binds an IRC session to the token's user through NICK. Before this,
// the PASS line carried a real OAuth token while the NICK line carried the
// anonymous justinfan convention unconditionally, so the login was either
// refused outright or silently downgraded to an anonymous session — and
// subscriber-only messages and badges were never captured. Asserting only the
// PASS line passes in exactly that broken state.
func TestIRCHandshakeAuthenticatesUnderTheAccountNickname(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, staticGetter("token-one"), staticGetter("archiveraccount"))

	runOneIRCSession(t, cd)
	pass, nick := handshakeLines(t, rec.nextSession(t))

	if pass != "PASS oauth:token-one" {
		t.Errorf("PASS = %q, want %q", pass, "PASS oauth:token-one")
	}
	if nick != "NICK archiveraccount" {
		t.Errorf("NICK = %q, want %q — a real token presented with any other nickname is not an "+
			"authenticated session, and the downgrade is silent", nick, "NICK archiveraccount")
	}
}

// TestIRCHandshakeLowercasesTheNickname: IRC nicknames are case-insensitive and
// Twitch expects the lowercase login, so a display-cased cookie must still
// produce the canonical nick.
func TestIRCHandshakeLowercasesTheNickname(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, staticGetter("token-one"), staticGetter("ArchiverAccount"))

	runOneIRCSession(t, cd)
	pass, nick := handshakeLines(t, rec.nextSession(t))

	if nick != "NICK archiveraccount" {
		t.Errorf("NICK = %q, want %q", nick, "NICK archiveraccount")
	}
	if pass != "PASS oauth:token-one" {
		t.Errorf("PASS = %q, want %q — lowercasing the nick must not disturb the other half of the "+
			"pair", pass, "PASS oauth:token-one")
	}
}

// TestIRCHandshakeReadsTokenPerSession is the per-session claim on the PASS
// half. Two connections, a getter whose value changes between them: the SECOND
// handshake must carry the SECOND token. Inspecting only the first connection
// passes without the fix.
func TestIRCHandshakeReadsTokenPerSession(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, tokenSequence("token-one", "token-two"), staticGetter("archiveraccount"))

	runOneIRCSession(t, cd)
	firstPass, firstNick := handshakeLines(t, rec.nextSession(t))

	runOneIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rec.nextSession(t))

	if firstPass != "PASS oauth:token-one" {
		t.Errorf("first handshake PASS = %q, want %q", firstPass, "PASS oauth:token-one")
	}
	if secondPass != "PASS oauth:token-two" {
		t.Errorf("second handshake PASS = %q, want %q — the reconnect re-presented the token it "+
			"started with instead of the current one", secondPass, "PASS oauth:token-two")
	}
	// Both sessions are authenticated, so neither may fall back to justinfan.
	if firstNick != "NICK archiveraccount" || secondNick != "NICK archiveraccount" {
		t.Errorf("NICKs = %q, %q, want %q for both", firstNick, secondNick, "NICK archiveraccount")
	}
}

// TestIRCHandshakeReadsLoginPerSession is the same claim on the NICK half: a
// login re-imported mid-job (a different account, a repaired cookie file) must
// reach the next reconnect. A nick captured at construction would keep
// identifying the session as the previous account while the PASS line moved on.
func TestIRCHandshakeReadsLoginPerSession(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t,
		staticGetter("token-one"),
		tokenSequence("firstaccount", "secondaccount"))

	runOneIRCSession(t, cd)
	_, firstNick := handshakeLines(t, rec.nextSession(t))

	runOneIRCSession(t, cd)
	_, secondNick := handshakeLines(t, rec.nextSession(t))

	if firstNick != "NICK firstaccount" {
		t.Errorf("first handshake NICK = %q, want %q", firstNick, "NICK firstaccount")
	}
	if secondNick != "NICK secondaccount" {
		t.Errorf("second handshake NICK = %q, want %q — the reconnect re-presented the account it "+
			"started with instead of the current one", secondNick, "NICK secondaccount")
	}
}

// TestIRCHandshakeNilGettersLogInAnonymously: nil getters must behave exactly
// as empty credentials do — the documented justinfan login, not a panic and not
// an error.
func TestIRCHandshakeNilGettersLogInAnonymously(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, nil, nil)

	runOneIRCSession(t, cd)

	assertAnonymousHandshake(t, rec.nextSession(t))
}

// TestIRCHandshakeEmptyGettersLogInAnonymously preserves the != "" guards.
func TestIRCHandshakeEmptyGettersLogInAnonymously(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, staticGetter(""), staticGetter(""))

	runOneIRCSession(t, cd)

	assertAnonymousHandshake(t, rec.nextSession(t))
}

// TestIRCHandshakeTokenWithoutLoginIsFullyAnonymous is the trap. A token with
// no account name must fall back to the COMPLETE anonymous handshake, not to a
// token-with-justinfan hybrid — that hybrid is the defect, so half-fixing it by
// keeping the token and defaulting the nick would reproduce it exactly.
//
// The token must therefore not appear anywhere in the handshake at all.
func TestIRCHandshakeTokenWithoutLoginIsFullyAnonymous(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, staticGetter("token-without-an-account"), staticGetter(""))

	runOneIRCSession(t, cd)
	lines := rec.nextSession(t)

	assertAnonymousHandshake(t, lines)
	assertNotOnTheWire(t, lines, "token-without-an-account")
}

// TestIRCHandshakeLoginWithoutTokenIsFullyAnonymous is the mirror image: a
// nickname with no credential behind it is not an authenticated session, so
// presenting it alongside PASS SCHMOOPIIE would claim an identity the session
// cannot prove.
func TestIRCHandshakeLoginWithoutTokenIsFullyAnonymous(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, staticGetter(""), staticGetter("archiveraccount"))

	runOneIRCSession(t, cd)
	lines := rec.nextSession(t)

	assertAnonymousHandshake(t, lines)
	assertNotOnTheWire(t, lines, "archiveraccount")
}

// TestIRCHandshakeUnsendableLoginIsFullyAnonymous covers a login that cannot be
// spoken as a single IRC parameter. A space truncates the nickname; a bare CR
// splits one websocket frame into two IRC commands. Either way the value is not
// a usable identity, so it is treated as no identity — the same full-anonymous
// fallback, never a hybrid.
func TestIRCHandshakeUnsendableLoginIsFullyAnonymous(t *testing.T) {
	for _, tc := range []struct {
		name  string
		login string
	}{
		{"space", "archiver account"},
		{"carriage return", "archiver\rNICK someoneelse"},
		{"newline", "archiver\nJOIN #elsewhere"},
		{"tab", "archiver\taccount"},
		{"NUL", "archiver\x00account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := startIRCRecorder(t, 4)
			cd := newAuthTestChatDownloader(t, staticGetter("token-one"), staticGetter(tc.login))

			runOneIRCSession(t, cd)
			lines := rec.nextSession(t)

			assertAnonymousHandshake(t, lines)
			assertNotOnTheWire(t, lines, "archiver")
			assertNotOnTheWire(t, lines, "token-one")
		})
	}
}

// TestIRCHandshakeRecoversFromAnonymousWhenCredentialsArrive is the failure this
// task exists to remove, stated positively: a job that started without
// credentials (anonymous login, silently) picks BOTH of them up on its next
// reconnect instead of staying anonymous for the rest of the stream.
func TestIRCHandshakeRecoversFromAnonymousWhenCredentialsArrive(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t,
		tokenSequence("", "token-imported-mid-job"),
		tokenSequence("", "archiveraccount"))

	runOneIRCSession(t, cd)
	firstLines := rec.nextSession(t)

	runOneIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rec.nextSession(t))

	assertAnonymousHandshake(t, firstLines)
	if secondPass != "PASS oauth:token-imported-mid-job" {
		t.Errorf("second handshake PASS = %q, want %q", secondPass, "PASS oauth:token-imported-mid-job")
	}
	if secondNick != "NICK archiveraccount" {
		t.Errorf("second handshake NICK = %q, want %q — recovering the token while leaving the "+
			"nickname anonymous is the very hybrid Twitch rejects", secondNick, "NICK archiveraccount")
	}
}

// --- VOD comment paging ---

// gqlAuthRecorder is a local stand-in for Twitch GQL that records the
// Authorization header of every request and serves canned pages in order.
type gqlAuthRecorder struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []string // "" means the header was absent
}

// startGQLRecorder brings up the server and repoints the GQL endpoint at it.
func startGQLRecorder(t *testing.T, pages ...string) *gqlAuthRecorder {
	t.Helper()
	rec := &gqlAuthRecorder{}
	page := 0
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Header.Get("Authorization"))
		body := pages[min(page, len(pages)-1)]
		page++
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(rec.server.Close)

	prev := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = rec.server.URL
	t.Cleanup(func() { constants.TwitchURLs.GQL = prev })
	return rec
}

func (rec *gqlAuthRecorder) authHeaders() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.seen...)
}

// vodCommentPage renders one VideoCommentsByOffsetOrCursor response carrying a
// single comment at the given offset.
func vodCommentPage(id string, offsetSeconds int, hasNext bool) string {
	next := "false"
	if hasNext {
		next = "true"
	}
	return `{"data":{"video":{"comments":{"edges":[{"node":{` +
		`"id":"` + id + `",` +
		`"contentOffsetSeconds":` + strconv.Itoa(offsetSeconds) + `,` +
		`"commenter":{"displayName":"Viewer","id":"u1","login":"viewer"},` +
		`"message":{"fragments":[{"text":"hello"}],"userBadges":[],"userColor":null}` +
		`}}],"pageInfo":{"hasNextPage":` + next + `}}}}}`
}

// TestVodChatReadsTokenPerPage is the VOD half of the claim: the paging loop
// runs for the length of the VOD, so it must present the CURRENT token on each
// page rather than the one captured at construction.
func TestVodChatReadsTokenPerPage(t *testing.T) {
	rec := startGQLRecorder(t,
		vodCommentPage("comment-1", 10, true),
		vodCommentPage("comment-2", 20, false),
	)

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "vod-1",
		AuthToken:  tokenSequence("vod-token-one", "vod-token-two"),
		OutputPath: filepath.Join(t.TempDir(), "chat.json"),
	}, &testLogger{})

	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := rec.authHeaders()
	want := []string{"OAuth vod-token-one", "OAuth vod-token-two"}
	if len(got) != len(want) {
		t.Fatalf("server saw %d requests (%q), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("page %d Authorization = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestVodChatNilGetterSendsNoAuthorization: a nil getter is an anonymous
// comment fetch, exactly as an empty token was.
func TestVodChatNilGetterSendsNoAuthorization(t *testing.T) {
	rec := startGQLRecorder(t, vodCommentPage("comment-1", 10, false))

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "vod-1",
		AuthToken:  nil,
		OutputPath: filepath.Join(t.TempDir(), "chat.json"),
	}, &testLogger{})

	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := rec.authHeaders()
	if len(got) != 1 {
		t.Fatalf("server saw %d requests (%q), want 1", len(got), got)
	}
	if got[0] != "" {
		t.Errorf("Authorization = %q, want no header at all", got[0])
	}
}

// writeTwitchCookieFile writes a Netscape cookie file holding a synthetic
// Twitch session: the auth-token and the login it belongs to, exactly as
// Twitch's own web client stores them. No real credential is ever written here.
func writeTwitchCookieFile(t *testing.T, path, token, login string) {
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

// jarLoadedFrom loads a jar from path so a later Reload picks up a rewrite of
// that same file.
func jarLoadedFrom(t *testing.T, path string) *cookies.CookieJar {
	t.Helper()
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestAuthGettersAreUsableGetters pins the composition the worker construction
// site relies on: twitch.Auth.GetAuthToken and twitch.Auth.GetLogin taken as
// METHOD VALUES both track the jar, so re-importing cookies underneath a live
// downloader changes BOTH halves of what the next reconnect presents.
//
// Both getters, one jar, one rewrite: an account switch moves the token and the
// login together, and a reconnect that picked up one but not the other would
// present the new session's credential under the old session's identity.
func TestAuthGettersAreUsableGetters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	writeTwitchCookieFile(t, path, "synthetic-token-before", "accountbefore")

	jar := jarLoadedFrom(t, path)
	auth := NewAuth(jar, nopLogger{})

	rec := startIRCRecorder(t, 4)
	// Exactly what internal/worker now assigns to AuthToken and Login.
	cd := newAuthTestChatDownloader(t, auth.GetAuthToken, auth.GetLogin)

	runOneIRCSession(t, cd)
	firstPass, firstNick := handshakeLines(t, rec.nextSession(t))

	writeTwitchCookieFile(t, path, "synthetic-token-after", "accountafter")
	if err := jar.Reload(); err != nil {
		t.Fatalf("reload jar: %v", err)
	}

	runOneIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rec.nextSession(t))

	if firstPass != "PASS oauth:synthetic-token-before" {
		t.Errorf("first handshake PASS = %q, want %q", firstPass, "PASS oauth:synthetic-token-before")
	}
	if firstNick != "NICK accountbefore" {
		t.Errorf("first handshake NICK = %q, want %q", firstNick, "NICK accountbefore")
	}
	if secondPass != "PASS oauth:synthetic-token-after" {
		t.Errorf("second handshake PASS = %q, want %q", secondPass, "PASS oauth:synthetic-token-after")
	}
	if secondNick != "NICK accountafter" {
		t.Errorf("second handshake NICK = %q, want %q — the reconnect kept the identity it started "+
			"with while the credential moved on", secondNick, "NICK accountafter")
	}
}

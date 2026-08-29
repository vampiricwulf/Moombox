package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// These tests pin ONE claim on both Twitch chat paths: the OAuth token is read
// AT USE TIME, not captured at construction. A stale Twitch token does not
// error — the IRC handshake falls through to the anonymous justinfan login and
// chat capture silently continues without subscriber-only messages or badges.
//
// Every token below is synthetic and none is ever logged.

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

// newAuthTestChatDownloader builds an IRC downloader with the given token
// getter and nothing else that touches disk.
func newAuthTestChatDownloader(t *testing.T, token func() string) *ChatDownloader {
	t.Helper()
	return NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin:   "testchan",
		ChannelDisplay: "TestChan",
		StreamID:       "stream-1",
		AuthToken:      token,
		OutputPath:     filepath.Join(t.TempDir(), "chat.json"),
	}, &testLogger{})
}

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

// firstLine returns the handshake's PASS message, failing if the session
// recorded nothing.
func firstLine(t *testing.T, lines []string) string {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("session recorded no messages")
	}
	return lines[0]
}

// TestIRCHandshakeReadsTokenPerSession is the claim. Two connections, a getter
// whose value changes between them: the SECOND handshake must carry the SECOND
// token. Inspecting only the first connection passes without the fix.
func TestIRCHandshakeReadsTokenPerSession(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, tokenSequence("token-one", "token-two"))

	runOneIRCSession(t, cd)
	first := firstLine(t, rec.nextSession(t))

	runOneIRCSession(t, cd)
	second := firstLine(t, rec.nextSession(t))

	if first != "PASS oauth:token-one" {
		t.Errorf("first handshake PASS = %q, want %q", first, "PASS oauth:token-one")
	}
	if second != "PASS oauth:token-two" {
		t.Errorf("second handshake PASS = %q, want %q — the reconnect re-presented the token it "+
			"started with instead of the current one", second, "PASS oauth:token-two")
	}
}

// TestIRCHandshakeNilGetterLogsInAnonymously: a nil getter must behave exactly
// as an empty token does today — the documented justinfan login, not a panic
// and not an error.
func TestIRCHandshakeNilGetterLogsInAnonymously(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, nil)

	runOneIRCSession(t, cd)

	if got := firstLine(t, rec.nextSession(t)); got != "PASS SCHMOOPIIE" {
		t.Errorf("nil-getter handshake PASS = %q, want %q", got, "PASS SCHMOOPIIE")
	}
}

// TestIRCHandshakeEmptyGetterLogsInAnonymously preserves the != "" guard.
func TestIRCHandshakeEmptyGetterLogsInAnonymously(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, func() string { return "" })

	runOneIRCSession(t, cd)

	if got := firstLine(t, rec.nextSession(t)); got != "PASS SCHMOOPIIE" {
		t.Errorf("empty-getter handshake PASS = %q, want %q", got, "PASS SCHMOOPIIE")
	}
}

// TestIRCHandshakeRecoversFromAnonymousWhenTheTokenArrives is the failure this
// task exists to remove, stated positively: a job that started without
// credentials (anonymous login, silently) picks the token up on its next
// reconnect instead of staying anonymous for the rest of the stream.
func TestIRCHandshakeRecoversFromAnonymousWhenTheTokenArrives(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	cd := newAuthTestChatDownloader(t, tokenSequence("", "token-imported-mid-job"))

	runOneIRCSession(t, cd)
	first := firstLine(t, rec.nextSession(t))

	runOneIRCSession(t, cd)
	second := firstLine(t, rec.nextSession(t))

	if first != "PASS SCHMOOPIIE" {
		t.Errorf("first handshake PASS = %q, want the anonymous login %q", first, "PASS SCHMOOPIIE")
	}
	if second != "PASS oauth:token-imported-mid-job" {
		t.Errorf("second handshake PASS = %q, want %q", second, "PASS oauth:token-imported-mid-job")
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

// writeTwitchCookieFile writes a one-row Netscape cookie file holding a
// synthetic Twitch auth-token. No real token is ever written here.
func writeTwitchCookieFile(t *testing.T, path, token string) {
	t.Helper()
	line := strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t")
	if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"+line+"\n"), 0o600); err != nil {
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

// TestAuthGetAuthTokenIsAUsableGetter pins the composition the worker
// construction sites rely on: twitch.Auth.GetAuthToken taken as a METHOD VALUE
// tracks the jar, so rotating the jar underneath a live downloader changes what
// the next reconnect presents.
func TestAuthGetAuthTokenIsAUsableGetter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	writeTwitchCookieFile(t, path, "synthetic-token-before")

	jar := jarLoadedFrom(t, path)
	auth := NewAuth(jar, nopLogger{})

	rec := startIRCRecorder(t, 4)
	// Exactly what internal/worker now assigns to AuthToken.
	cd := newAuthTestChatDownloader(t, auth.GetAuthToken)

	runOneIRCSession(t, cd)
	first := firstLine(t, rec.nextSession(t))

	writeTwitchCookieFile(t, path, "synthetic-token-after")
	if err := jar.Reload(); err != nil {
		t.Fatalf("reload jar: %v", err)
	}

	runOneIRCSession(t, cd)
	second := firstLine(t, rec.nextSession(t))

	if first != "PASS oauth:synthetic-token-before" {
		t.Errorf("first handshake PASS = %q, want %q", first, "PASS oauth:synthetic-token-before")
	}
	if second != "PASS oauth:synthetic-token-after" {
		t.Errorf("second handshake PASS = %q, want %q", second, "PASS oauth:synthetic-token-after")
	}
}

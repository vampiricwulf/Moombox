package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// fastChatDelays is defaultChatDelays() scaled so the whole keepalive cycle
// runs in ~150 ms instead of ~75 s, keeping the RATIOS production has: the
// idle window is three check ticks, and the pong window is between one and two.
// Nothing here is small enough to race the Windows timer granularity (~15 ms).
func fastChatDelays() chatDelays {
	return chatDelays{
		keepaliveIdle:     60 * time.Millisecond,
		keepalivePongWait: 30 * time.Millisecond,
		keepaliveCheck:    20 * time.Millisecond,
	}
}

// TestDefaultChatDelaysMatchConstants pins production timing: the struct the
// constructor installs equals the constants that document it, and no field is
// left at its zero value (a zero check interval makes time.NewTicker panic).
func TestDefaultChatDelaysMatchConstants(t *testing.T) {
	want := chatDelays{
		keepaliveIdle:     ircKeepaliveIdle,
		keepalivePongWait: ircKeepalivePongWait,
		keepaliveCheck:    ircKeepaliveCheck,
	}
	if got := defaultChatDelays(); got != want {
		t.Fatalf("defaultChatDelays() = %+v, want %+v", got, want)
	}
	for name, pair := range map[string][2]time.Duration{
		"keepaliveIdle":     {want.keepaliveIdle, 45 * time.Second},
		"keepalivePongWait": {want.keepalivePongWait, 10 * time.Second},
		"keepaliveCheck":    {want.keepaliveCheck, 15 * time.Second},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %v, want %v (production timing must not move in this arc)",
				name, pair[0], pair[1])
		}
		if pair[0] == 0 {
			t.Errorf("chatDelays.%s has no default", name)
		}
	}
	// The detection bound the doc promises: one idle window, plus at most one
	// check tick to notice it, plus the pong window.
	if ircKeepaliveIdle+ircKeepaliveCheck+ircKeepalivePongWait >= ircReadDeadline {
		t.Error("the keepalive cannot detect a dead socket sooner than ircReadDeadline does; " +
			"it would be doing nothing at all")
	}

	cd := NewChatDownloader(ChatDownloaderOptions{ChannelLogin: "c", StreamID: "s"}, &testLogger{})
	if cd.delays != want {
		t.Errorf("NewChatDownloader installed %+v, want the defaults", cd.delays)
	}
}

// keepaliveServer is a websocket IRC server that completes the handshake,
// welcomes the client, and then goes QUIET — recording every line the client
// sends afterwards and, when answerPong is set, answering a client PING with a
// PONG.
//
// It is separate from ircReplier (chat_irc_fallback_test.go) on purpose: that
// one reads exactly one post-script line and then parks, which is the shape its
// own tests need. These tests need every line, for as long as the session runs.
type keepaliveServer struct {
	server *httptest.Server
	mu     sync.Mutex
	lines  []string
}

func (k *keepaliveServer) clientLines() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.lines...)
}

func (k *keepaliveServer) pings() int {
	n := 0
	for _, l := range k.clientLines() {
		if strings.HasPrefix(l, "PING") {
			n++
		}
	}
	return n
}

func startKeepaliveServer(t *testing.T, answerPong bool) *keepaliveServer {
	t.Helper()
	k := &keepaliveServer{}
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		for range 4 { // PASS, NICK, CAP REQ, JOIN
			if _, _, readErr := conn.Read(r.Context()); readErr != nil {
				return
			}
		}
		if writeErr := conn.Write(r.Context(), websocket.MessageText,
			[]byte(":tmi.twitch.tv 001 justinfan1 :Welcome, GLHF!")); writeErr != nil {
			return
		}
		for {
			_, data, readErr := conn.Read(r.Context())
			if readErr != nil {
				return
			}
			line := string(data)
			k.mu.Lock()
			k.lines = append(k.lines, line)
			k.mu.Unlock()
			if answerPong && strings.HasPrefix(line, "PING") {
				if writeErr := conn.Write(r.Context(), websocket.MessageText,
					[]byte(":tmi.twitch.tv PONG tmi.twitch.tv :moombox")); writeErr != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(k.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(k.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return k
}

// newKeepaliveTestDownloader is an ANONYMOUS downloader (no Credentials), so
// runIRCSession installs no handshake-outcome defer and nothing in these tests
// can be mistaken for a login verdict.
func newKeepaliveTestDownloader(t *testing.T) *ChatDownloader {
	t.Helper()
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   t.TempDir() + "/chat.json",
	}, &testLogger{})
	cd.delays = fastChatDelays()
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	return cd
}

// TestIRCKeepalivePingsAndGivesUpOnASilentSocket is T3-25.
//
// A half-open socket — one the OS still believes is connected — used to cost up
// to ircReadDeadline (6 minutes) of chat, and Twitch IRC has NO replay: every
// message in that window is simply absent from the archive. So the session
// speaks first, exactly as chatterino7 does
// (references/chatterino7/src/providers/twitch/IrcConnection2.cpp).
//
// Mutants this kills:
//   - no client PING at all (today): the server records zero lines and
//     runIRCSession blocks until the test's own deadline.
//   - PING sent but no pong deadline: pings() keeps climbing and the session
//     never returns, so the reconnect that recovers chat never happens.
//   - counting the keepalive tick as a read error: the session would burn
//     chatMaxConsecutiveErrs and return a "too many IRC errors" wrapper
//     instead — the error-text assertion is what separates the two.
func TestIRCKeepalivePingsAndGivesUpOnASilentSocket(t *testing.T) {
	k := startKeepaliveServer(t, false)
	cd := newKeepaliveTestDownloader(t)

	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a silent socket ended the session with nil — Start's loop treats that as a " +
				"clean close and does not reconnect")
		}
		if !strings.Contains(err.Error(), "keepalive") {
			t.Errorf("session ended with %v, want the keepalive error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session never noticed the silent socket")
	}

	if got := k.pings(); got != 1 {
		t.Errorf("the client sent %d PINGs, want exactly 1", got)
	}
	for _, l := range k.clientLines() {
		if strings.HasPrefix(l, "PING") && l != "PING :moombox" {
			t.Errorf("client PING was %q, want %q", l, "PING :moombox")
		}
	}
}

// TestIRCKeepaliveKeepsAConnectionThatAnswers is the other edge: a quiet
// channel is not a dead socket. Twitch answering PONG must reset the idle clock
// and leave the session running.
//
// Mutant: failing on the pong deadline regardless of what arrived (e.g.
// comparing against the last DATA message rather than the last inbound FRAME) —
// a channel with no chatter would then reconnect every ~75 s forever, and each
// reconnect costs the messages in flight.
func TestIRCKeepaliveKeepsAConnectionThatAnswers(t *testing.T) {
	k := startKeepaliveServer(t, true)
	cd := newKeepaliveTestDownloader(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(ctx) }()

	// Long enough for several idle windows, so a session that fails on the
	// pong deadline has many chances to.
	time.Sleep(400 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the session ended while the server was answering PONG: %v", err)
	default:
	}
	if got := k.pings(); got < 1 {
		t.Errorf("the client sent %d PINGs over 400ms of silence, want at least 1", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cancelled session returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session ignored its cancelled context")
	}
}

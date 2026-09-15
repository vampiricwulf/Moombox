package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// reconnectDirectiveServer completes the IRC handshake, welcomes the client,
// and then immediately sends RECONNECT — what Twitch does when it takes a chat
// edge out of service. It then STAYS OPEN: the client has to leave on the
// directive alone, not because the socket died under it, or the test would
// prove nothing about how RECONNECT is handled.
//
// Separate from keepaliveServer (chat_keepalive_test.go), which goes quiet
// after the welcome so its own tests can measure PINGs. This one speaks once
// and then parks.
type reconnectDirectiveServer struct {
	server *httptest.Server
	mu     sync.Mutex
	// conns counts connections that finished the handshake AND were sent the
	// directive, which is how the budget test below measures sessions without
	// reading Start's internals.
	conns int
}

func (s *reconnectDirectiveServer) sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func startReconnectDirectiveServer(t *testing.T) *reconnectDirectiveServer {
	t.Helper()
	s := &reconnectDirectiveServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if writeErr := conn.Write(r.Context(), websocket.MessageText,
			[]byte("RECONNECT")); writeErr != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		s.mu.Unlock()
		<-r.Context().Done()
	}))
	t.Cleanup(s.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(s.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return s
}

// TestIRCServerReconnectIsNotACleanExit is the session half of O1.
//
// nil is Start's CLEAN-EXIT value: on nil the loop returns, the orchestrator's
// chat goroutine closes chatDone, and nothing relaunches chat for the rest of
// the job (it only relaunches after a connectivity outage). So answering a
// routine RECONNECT with nil silently ended chat capture on a live stream.
//
// Mutant this kills: today's `return nil` at the RECONNECT branch — the session
// ends with nil and the assertion below names exactly why that is fatal.
func TestIRCServerReconnectIsNotACleanExit(t *testing.T) {
	startReconnectDirectiveServer(t)
	cd := newKeepaliveTestDownloader(t)

	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, errServerReconnect) {
			t.Fatalf("a server RECONNECT ended the session with %v, want the sentinel — nil is "+
				"Start's clean-exit value, so it ends chat capture for the rest of the job", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session never acted on the RECONNECT directive")
	}
}

// TestIRCServerReconnectDoesNotChargeTheReconnectBudgetOrEndChat is the
// cross-file half, and it is what makes the sentinel the KEEPALIVE's shape
// rather than a second mechanism.
//
// Start charges reconnectAttempts for every failed session and forgives the
// charge only for one that stayed up past reconnectResetUptime (5 min). A
// server rotating its chat edges can issue several RECONNECTs in one marathon
// stream, none of them after five minutes of uptime, so charging them would
// exhaust maxReconnects (10) and "exceeded max IRC reconnects" would abandon
// chat — for messages Twitch asked us to come back for.
//
// Mutants this kills:
//   - `return nil` at the RECONNECT branch: Start returns after ONE session and
//     the done arm below fires immediately, naming the session count.
//   - dropping the errors.Is(err, errServerReconnect) arm from Start's loop:
//     the backoff path's own Info line appears on the second session (checked
//     every poll), so the mutant dies in well under a second naming its cause,
//     and Start then gives up at eleven.
func TestIRCServerReconnectDoesNotChargeTheReconnectBudgetOrEndChat(t *testing.T) {
	// Two past maxReconnects+1: enough that a charged budget has certainly
	// given up, not so many that the test is measuring the fixture.
	const wantSessions = 12

	s := startReconnectDirectiveServer(t)
	logger := &acceptedLoginRecorder{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   t.TempDir() + "/chat.json",
	}, logger)
	// Before Start, and the only field this test pokes: Start owns `running`.
	// The keepalive never fires here (every session ends on the directive
	// first) — this only stops a wedged session parking for 45 s.
	cd.delays = fastChatDelays()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("Start panicked: %v", r)
			}
		}()
		done <- cd.Start(context.Background())
	}()
	t.Cleanup(func() {
		cd.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after Stop")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for s.sessions() < wantSessions {
		if n := logger.backoffReconnects(); n > 0 {
			t.Fatalf("%d backoff reconnects after %d server RECONNECTs — the directive is being "+
				"charged to the reconnect budget, so a server rotating its chat edges abandons "+
				"chat for the rest of the job", n, s.sessions())
		}
		select {
		case err := <-done:
			// Put it back: done is buffered, and the t.Cleanup above still
			// wants to read it rather than add a second, spurious failure.
			done <- err
			t.Fatalf("Start returned after %d sessions (%v), want it still reconnecting at %d",
				s.sessions(), err, wantSessions)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d sessions in 10s, want %d — a RECONNECT costs one dial and one "+
				"handshake, so something is waiting that should not be", s.sessions(), wantSessions)
		}
		time.Sleep(2 * time.Millisecond)
	}

	select {
	case err := <-done:
		done <- err
		t.Fatalf("Start returned after %d sessions (%v), want it still reconnecting", wantSessions, err)
	default:
	}
	if n := logger.backoffReconnects(); n != 0 {
		t.Errorf("%d backoff reconnects over %d server RECONNECTs, want none", n, wantSessions)
	}
}

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

// fastChatDelays is defaultChatDelays() scaled so the whole keepalive cycle
// runs in ~150 ms instead of ~75 s, keeping the RATIOS production has: the
// idle window is three check ticks, and the pong window is between one and two.
// Nothing here is small enough to race the Windows timer granularity (~15 ms).
//
// resumeSaveFloor is deliberately left at its zero value rather than scaled:
// zero DISABLES the throttle (saveResumeStateThrottled), and none of the tests
// that use this fixture (chat_keepalive_test.go, chat_reconnect_directive_test.go)
// assert anything about the resume sidecar's cadence — only the keepalive
// mechanism. A future test that adds a sidecar assertion on top of this
// fixture must set resumeSaveFloor itself, the way the sidecar tests
// (chat_progress_and_sidecar_test.go) already do.
func fastChatDelays() chatDelays {
	return chatDelays{
		keepaliveIdle:     60 * time.Millisecond,
		keepalivePongWait: 30 * time.Millisecond,
		keepaliveCheck:    20 * time.Millisecond,
		reconnectBase:     time.Millisecond,
		reconnectCap:      5 * time.Millisecond,
		exhaustedRetry:    10 * time.Millisecond,
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
		resumeSaveFloor:   ircResumeSaveFloor,
		reconnectBase:     ircReconnectBase,
		reconnectCap:      ircReconnectCap,
		exhaustedRetry:    ircExhaustedRetry,
	}
	if got := defaultChatDelays(); got != want {
		t.Fatalf("defaultChatDelays() = %+v, want %+v", got, want)
	}
	for name, pair := range map[string][2]time.Duration{
		"keepaliveIdle":     {want.keepaliveIdle, 45 * time.Second},
		"keepalivePongWait": {want.keepalivePongWait, 10 * time.Second},
		"keepaliveCheck":    {want.keepaliveCheck, 15 * time.Second},
		"resumeSaveFloor":   {want.resumeSaveFloor, 5 * time.Second},
		"reconnectBase":     {want.reconnectBase, time.Second},
		"reconnectCap":      {want.reconnectCap, 30 * time.Second},
		"exhaustedRetry":    {want.exhaustedRetry, 2 * time.Minute},
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
	// conns counts connections that finished the handshake, which is how the
	// reconnect-budget test below measures sessions without reading Start's
	// internals. Counted after the four handshake reads so a half-opened
	// connection cannot inflate it.
	conns int
}

func (k *keepaliveServer) clientLines() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.lines...)
}

func (k *keepaliveServer) sessions() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.conns
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
		k.mu.Lock()
		k.conns++
		k.mu.Unlock()
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

// waitFor polls cond until it holds or the budget runs out, failing with msg if
// it never does. A bounded poll rather than a fixed sleep: the condition is
// produced by a server goroutine these tests do not synchronise with, so the
// only two honest shapes are "wait until it is true" and "prove it stayed
// false", and a sleep-then-assert is neither.
func waitFor(t *testing.T, budget time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (within %v)", msg, budget)
		}
		time.Sleep(2 * time.Millisecond)
	}
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

	// The server records on its own goroutine, so "exactly one" has to be
	// REACHED before it can be held: a single immediate check races the
	// scheduler and would pass on 0 as readily as on 1. Poll to 1, then settle
	// and confirm nothing followed — a second PING would mean the keepalive
	// kept running past its own verdict.
	waitFor(t, 500*time.Millisecond, func() bool { return k.pings() >= 1 },
		"the client never sent a keepalive PING")
	if got := k.pings(); got != 1 {
		t.Fatalf("the client sent %d PINGs before the pong deadline, want exactly 1", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := k.pings(); got != 1 {
		t.Errorf("the client sent %d PINGs in total, want exactly 1 — the keepalive kept pinging "+
			"after it had already declared the socket dead", got)
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
// reconnect costs the messages in flight. Such a session cannot reach a second
// PING, which is what the poll below waits for.
func TestIRCKeepaliveKeepsAConnectionThatAnswers(t *testing.T) {
	k := startKeepaliveServer(t, true)
	cd := newKeepaliveTestDownloader(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(ctx) }()

	// Wait for the condition, not for a clock. A SECOND keepalive PING can
	// only be sent by a session that survived the first one's pong window and
	// then went idle again, so polling to two is a stronger statement than any
	// fixed sleep — and it does not get weaker on a loaded CI box, where a
	// 400 ms sleep might have covered only one cycle.
	waitFor(t, 3*time.Second, func() bool { return k.pings() >= 2 },
		"the client did not complete two keepalive cycles against a server answering PONG")
	// The settle check: the second cycle's pong window must pass with the
	// session still up. Without it, a mutant that fails on the pong deadline
	// regardless of what arrived could have declared the socket dead in the
	// instant after the PING waitFor observed.
	time.Sleep(3 * cd.delays.keepalivePongWait)
	select {
	case err := <-done:
		t.Fatalf("the session ended while the server was answering PONG: %v", err)
	default:
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

// slowWarnLogger is testLogger with a Warn that costs what a real one costs.
//
// It exists because the hazard this file guards is a RACE whose loser is
// decided by how long the keepalive spends between ending the session and
// publishing its verdict, and the production logger writes that Warn to a file
// or a console. A no-op Warn hides the whole window, so a test that used one
// would pass against code that is wrong in the field.
type slowWarnLogger struct{}

func (slowWarnLogger) Debug(string, ...any) {}
func (slowWarnLogger) Info(string, ...any)  {}
func (slowWarnLogger) Error(string, ...any) {}
func (slowWarnLogger) Warn(string, ...any)  { time.Sleep(25 * time.Millisecond) }

// TestIRCKeepaliveWriteThatNeverCompletesIsAKeepaliveVerdict pins the arm of the
// keepalive a real socket cannot be made to exercise: a PING write that never
// completes because the peer's send buffer is full. Reaching it needs the
// keepaliveWrite seam — there is no way to conjure a stuck peer from a fixture.
//
// The stand-in models coder/websocket's own write deadline
// (context.AfterFunc(ctx, func(){ clearWriteTimeout(); close() }), conn.go:171-181):
// it blocks until the write context is done and then closes the connection.
// That close is the hazard. The read loop wakes from a dead socket instantly,
// and each of its next iterations is a context.WithTimeout plus an immediate
// net.ErrClosed read — so it can burn all chatMaxConsecutiveErrs of them inside
// one log line, return "too many IRC errors: …", and have that charged to the
// reconnect budget because it is not errors.Is-able to errKeepaliveTimeout.
//
// Mutant this kills, verified by execution and not by reading: restore the
// round-1 shape — declareDead logging BEFORE it closes keepaliveFailed, and the
// write bounded by context.WithTimeout instead of a timer we own. Both halves
// let the socket die while the verdict is still unpublished, and with a Warn
// that costs anything at all the read loop wins every time.
//
// Mutant this does NOT kill, stated here so nobody mistakes the coverage:
// cancelling the write context immediately BEFORE declareDead rather than
// after. The window is real but the correct code closes it rather than losing
// it — every route to the socket's death needs a goroutine wakeup, while
// close(keepaliveFailed) is the next instruction, so the verdict wins on its
// own. It survived 50 runs under -race. The ordering is kept because it costs
// one line and removes the need to win.
func TestIRCKeepaliveWriteThatNeverCompletesIsAKeepaliveVerdict(t *testing.T) {
	startKeepaliveServer(t, false)
	cd := newKeepaliveTestDownloader(t)
	cd.logger = slowWarnLogger{}
	cd.keepaliveWrite = func(ctx context.Context, conn *websocket.Conn, _ string) error {
		<-ctx.Done()
		conn.CloseNow()
		return ctx.Err()
	}

	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, errKeepaliveTimeout) {
			t.Errorf("a PING write that never completed ended the session with %v, want a "+
				"keepalive verdict — anything else is charged to the reconnect budget", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session never noticed that its keepalive PING could not be written")
	}
}

// TestIRCKeepaliveFailureDoesNotChargeTheReconnectBudget is the cross-file half
// of T3-25, and it is what decides whether the faster detector is an
// improvement at all.
//
// Start's loop charges reconnectAttempts for every failed session and forgives
// the charge only for one that stayed up past reconnectResetUptime (5 min).
// Before this task a half-open socket was detected at ircReadDeadline (6 min)
// — always ABOVE that line, so chat retried forever. The keepalive detects it
// at ~70 s, BELOW it. Charging that to the budget would mean a middlebox that
// swallows PONGs exhausts maxReconnects (10) in about fifteen minutes and
// "exceeded max IRC reconnects" abandons chat for the rest of the job: a faster
// detector turning a recoverable network into a surrendered one.
//
// Mutant: drop the errors.Is(err, errKeepaliveTimeout) arm from Start's loop.
// The assertion is on the BACKOFF path's own Info line, checked every poll,
// rather than only on the session count — with the arm gone the loop stalls in
// exponential backoff (2s, 4s, 8s, 16s, then 30s a time) and would report a
// timeout four minutes later instead of the reason. The line appears on the
// second session, so the mutant dies in under a second, naming its cause.
func TestIRCKeepaliveFailureDoesNotChargeTheReconnectBudget(t *testing.T) {
	// Two past maxReconnects+1: enough that a charged budget has certainly
	// given up, not so many that the test is measuring the fixture.
	const wantSessions = 12

	k := startKeepaliveServer(t, false)
	logger := &acceptedLoginRecorder{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   t.TempDir() + "/chat.json",
	}, logger)
	// Before Start, and the only field this test pokes: Start owns `running`.
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
	for k.sessions() < wantSessions {
		if n := logger.backoffReconnects(); n > 0 {
			t.Fatalf("%d backoff reconnects after %d keepalive failures — the keepalive verdict is "+
				"being charged to the reconnect budget, so a network that never answers within the "+
				"pong window abandons chat for the rest of the job", n, k.sessions())
		}
		select {
		case err := <-done:
			// Put it back: done is buffered, and the t.Cleanup above still
			// wants to read it rather than add a second, spurious failure.
			done <- err
			t.Fatalf("Start returned after %d sessions (%v), want it still reconnecting at %d",
				k.sessions(), err, wantSessions)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d sessions in 10s, want %d — each scaled keepalive cycle is ~120ms, so "+
				"something is waiting that should not be", k.sessions(), wantSessions)
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
		t.Errorf("%d backoff reconnects over %d keepalive failures, want none", n, wantSessions)
	}
}

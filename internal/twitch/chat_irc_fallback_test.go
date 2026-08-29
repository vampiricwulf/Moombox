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

// These tests pin the floor: authenticated if Twitch accepts it, anonymous if
// it does not, never nothing.
//
// Sending the account nickname is new behaviour against real Twitch. Before it,
// every install used the anonymous handshake, which Twitch always accepts. If
// Twitch refuses the authenticated one — a stale login cookie, a web-session
// token tmi will not take — nothing in chat_irc.go parses the refusal, the
// socket closes, and Start's reconnect loop burns all ten attempts on a login
// that cannot succeed before abandoning chat for the whole job. A working
// degraded capture would become no capture at all.
//
// The floor has two edges and both are pinned here. A session that HEARD from
// Twitch and was never welcomed is a refusal and falls back. A session that
// heard NOTHING is a dropped socket — it learned nothing about the login, and
// treating it as a refusal would surrender subscriber-only chat for the rest of
// a marathon stream on the strength of one bad reconnect.
//
// Every credential below is synthetic and none is ever logged.

// ircReplier is a local websocket server standing in for Twitch IRC that reads
// each connection's handshake and then answers it from a per-connection script.
type ircReplier struct {
	server   *httptest.Server
	sessions chan []string
	mu       sync.Mutex
	replies  [][]string
	conns    int
	// echoes, when non-nil, receives one further message read from the client
	// AFTER the script has been written. It is proof the client actually
	// processed an inbound line, which a test that must act at that exact
	// moment cannot get any other way — the write returning says only that the
	// bytes left the server.
	echoes chan string
}

// startIRCReplier brings up the server and repoints the IRC endpoint at it.
// replies[i] is sent on connection i after its four handshake messages are
// read; a connection past the end of the script is answered with silence and a
// close — a dropped socket that says nothing about the login.
func startIRCReplier(t *testing.T, replies ...[]string) *ircReplier {
	t.Helper()
	return newIRCReplier(t, nil, replies...)
}

// startIRCReplierAwaitingEcho is the same, but after the script it reads ONE
// more message from the client and publishes it on the returned replier's
// echoes channel. Pair it with a PING script: the client answers PONG only
// after it has read and handled the PING, so the echo is a round-trip proof
// that an inbound line was processed.
//
// Opt-in, because that extra read holds the connection open — a client with no
// reply to send would keep it parked until its own read deadline.
func startIRCReplierAwaitingEcho(t *testing.T, replies ...[]string) *ircReplier {
	t.Helper()
	return newIRCReplier(t, make(chan string, 8), replies...)
}

func newIRCReplier(t *testing.T, echoes chan string, replies ...[]string) *ircReplier {
	t.Helper()
	rep := &ircReplier{sessions: make(chan []string, 8), replies: replies, echoes: echoes}
	rep.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		rep.mu.Lock()
		var script []string
		if rep.conns < len(rep.replies) {
			script = rep.replies[rep.conns]
		}
		rep.conns++
		rep.mu.Unlock()

		var lines []string
		for len(lines) < 4 { // PASS, NICK, CAP REQ, JOIN
			_, data, readErr := conn.Read(r.Context())
			if readErr != nil {
				break
			}
			lines = append(lines, string(data))
		}
		rep.sessions <- lines

		for _, line := range script {
			if writeErr := conn.Write(r.Context(), websocket.MessageText, []byte(line)); writeErr != nil {
				return
			}
		}

		// Only a connection that was actually given a script waits for an echo.
		// An unscripted one has said nothing the client could answer, so the
		// read would park until the client's own 6-minute deadline while the
		// client parks waiting for the server — a deadlock, not a test.
		if rep.echoes != nil && len(script) > 0 {
			if _, data, readErr := conn.Read(r.Context()); readErr == nil {
				rep.echoes <- string(data)
			}
		}
	}))
	t.Cleanup(rep.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(rep.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return rep
}

// nextSession returns the handshake messages of the next accepted connection.
func (rep *ircReplier) nextSession(t *testing.T) []string {
	t.Helper()
	rec := &ircSessionRecorder{sessions: rep.sessions}
	return rec.nextSession(t)
}

// welcomeLine is what Twitch sends once it has accepted a login.
const welcomeLine = ":tmi.twitch.tv 001 archiveraccount :Welcome, GLHF!"

// loginFailedNotice is one of the two texts Twitch sends when it refuses one.
const loginFailedNotice = ":tmi.twitch.tv NOTICE * :Login authentication failed"

// recordingLogger captures Warn messages so the fallback's single log line can
// be asserted on. It records the message only — never the args — because the
// fallback must not put a credential in either.
type recordingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *recordingLogger) Debug(string, ...any) {}
func (l *recordingLogger) Info(string, ...any)  {}
func (l *recordingLogger) Error(string, ...any) {}
func (l *recordingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
}

func (l *recordingLogger) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

// fallbackWarnings returns only the warnings the anonymous fallback emits, so
// an unrelated Warn on the session path cannot be mistaken for one.
func (l *recordingLogger) fallbackWarnings() []string {
	var out []string
	for _, w := range l.warnings() {
		if strings.Contains(w, "continuing anonymously") {
			out = append(out, w)
		}
	}
	return out
}

// missingLoginWarnings returns only the warnings noteMissingLogin emits.
//
// Two separate filters over one recorder, and the two phrases must stay
// distinct: every fallback test counts fallbackWarnings exactly, so a
// missing-login line that also said "continuing anonymously" would be counted
// as a fallback and quietly defang all of them.
func (l *recordingLogger) missingLoginWarnings() []string {
	var out []string
	for _, w := range l.warnings() {
		if strings.Contains(w, "no login cookie") {
			out = append(out, w)
		}
	}
	return out
}

// runLiveIRCSession drives one session with the downloader marked RUNNING, so
// the read loop actually executes and the handshake outcome is observed.
//
// The session always ends in an error, because the stand-in server closes the
// socket after its script — which is the point: a refused login and a dropped
// connection reach this code in exactly the same shape, and only the presence
// of RPL_WELCOME separates them.
func runLiveIRCSession(t *testing.T, cd *ChatDownloader) {
	t.Helper()
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	_ = cd.runIRCSession(context.Background())
}

// TestIRCLineClassifiersAreExact pins WHICH lines mean what, against the real
// traffic Twitch sends around a handshake.
//
// Both classifiers are narrow on purpose and the narrowness is the mechanism.
// RPL_WELCOME is the numeric 001 and nothing else: Twitch's own accepted-login
// burst also carries 002, 003, 004, 375, 372 and 376, and a CAP ACK arrives
// before any of them, so a classifier that merely recognised "a three-character
// command" or "some numeric" would read acceptance out of a session Twitch
// refused. The NOTICE matcher is gated on the command so a chat message quoting
// the refusal text cannot masquerade as one.
func TestIRCLineClassifiersAreExact(t *testing.T) {
	for _, tc := range []struct {
		name         string
		line         string
		welcome      bool
		loginFailure bool
	}{
		{"RPL_WELCOME", ":tmi.twitch.tv 001 archiveraccount :Welcome, GLHF!", true, false},
		{"RPL_WELCOME with tags", "@badge=1 :tmi.twitch.tv 001 archiveraccount :Welcome, GLHF!", true, false},
		{"RPL_YOURHOST 002", ":tmi.twitch.tv 002 archiveraccount :Your host is tmi.twitch.tv", false, false},
		{"RPL_CREATED 003", ":tmi.twitch.tv 003 archiveraccount :This server is rather new", false, false},
		{"RPL_MYINFO 004", ":tmi.twitch.tv 004 archiveraccount :-", false, false},
		{"MOTD 372", ":tmi.twitch.tv 372 archiveraccount :You are in a maze of twisty passages", false, false},
		{"end of MOTD 376", ":tmi.twitch.tv 376 archiveraccount :>", false, false},
		{"CAP ACK", ":tmi.twitch.tv CAP * ACK :twitch.tv/tags twitch.tv/commands", false, false},
		{"JOIN", ":archiveraccount!archiveraccount@archiveraccount.tmi.twitch.tv JOIN #testchan", false, false},
		{"login failure NOTICE", ":tmi.twitch.tv NOTICE * :Login authentication failed", false, true},
		{"login unsuccessful NOTICE", ":tmi.twitch.tv NOTICE * :Login unsuccessful", false, true},
		{"unrelated NOTICE", ":tmi.twitch.tv NOTICE #testchan :This room is in subscribers-only mode.", false, false},
		{
			"PRIVMSG quoting the refusal text",
			"@id=1 :viewer!viewer@viewer.tmi.twitch.tv PRIVMSG #testchan :Login authentication failed lol",
			false, false,
		},
		{"PING", "PING :tmi.twitch.tv", false, false},
		{"empty", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ircIsWelcome(tc.line); got != tc.welcome {
				t.Errorf("ircIsWelcome(%q) = %v, want %v", tc.line, got, tc.welcome)
			}
			if got := ircIsLoginFailureNotice(tc.line); got != tc.loginFailure {
				t.Errorf("ircIsLoginFailureNotice(%q) = %v, want %v", tc.line, got, tc.loginFailure)
			}
		})
	}
}

// TestIRCNonWelcomeTrafficStillFallsBack is the same claim end-to-end: a
// refused session is not silent — Twitch answers the CAP REQ and may send other
// numerics — so traffic arriving is not acceptance. Only 001 is.
func TestIRCNonWelcomeTrafficStillFallsBack(t *testing.T) {
	rep := startIRCReplier(t, []string{
		":tmi.twitch.tv CAP * ACK :twitch.tv/tags twitch.tv/commands twitch.tv/membership",
		":tmi.twitch.tv 002 archiveraccount :Your host is tmi.twitch.tv",
		":tmi.twitch.tv 372 archiveraccount :You are in a maze of twisty passages",
	})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	rep.nextSession(t)

	runLiveIRCSession(t, cd)
	second := rep.nextSession(t)

	assertAnonymousHandshake(t, second)
	assertNotOnTheWire(t, second, "token-one")
	if got := logger.fallbackWarnings(); len(got) != 1 {
		t.Errorf("fallback warnings = %q, want exactly one — a CAP ACK and a couple of numerics "+
			"are not an accepted login", got)
	}
}

// TestIRCChatQuotingTheRefusalIsNotANotice: the fallback's wording must come
// from what TWITCH said, not from what a viewer typed. A chat message quoting
// the refusal text arrives on a session that was never welcomed, so the fallback
// still fires — but it must report that nothing acknowledged the login, not that
// Twitch named a failure.
func TestIRCChatQuotingTheRefusalIsNotANotice(t *testing.T) {
	rep := startIRCReplier(t, []string{
		"@id=1 :viewer!viewer@viewer.tmi.twitch.tv PRIVMSG #testchan :Login authentication failed lol",
	})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	rep.nextSession(t)

	got := logger.fallbackWarnings()
	if len(got) != 1 {
		t.Fatalf("fallback warnings = %q, want exactly one", got)
	}
	if strings.Contains(got[0], "Twitch replied") {
		t.Errorf("warning = %q, want the generic wording — a viewer's chat message was read as "+
			"Twitch refusing the login", got[0])
	}
	if !strings.Contains(got[0], "never acknowledged") {
		t.Errorf("warning = %q, want it to report that nothing acknowledged the login", got[0])
	}
}

// TestIRCRefusedLoginFallsBackToAnonymous is the claim.
//
// The assertion is on the WIRE BYTES of the SECOND session, not on the fact
// that a reconnect happened — "a second handshake occurred" is a junction the
// unfixed code satisfies too, by presenting the same rejected credentials
// again.
func TestIRCRefusedLoginFallsBackToAnonymous(t *testing.T) {
	// Twitch's documented refusal: the NOTICE, then the socket closes. No
	// RPL_WELCOME ever arrives.
	rep := startIRCReplier(t, []string{loginFailedNotice})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	firstPass, firstNick := handshakeLines(t, rep.nextSession(t))

	runLiveIRCSession(t, cd)
	second := rep.nextSession(t)

	// The first session must actually have tried to authenticate, or a green
	// second session would prove nothing.
	if firstPass != "PASS oauth:token-one" || firstNick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", firstPass, firstNick)
	}
	assertAnonymousHandshake(t, second)
	assertNotOnTheWire(t, second, "token-one")
	assertNotOnTheWire(t, second, "archiveraccount")

	if got := logger.fallbackWarnings(); len(got) != 1 {
		t.Errorf("fallback warnings = %q, want exactly one", got)
	}
	for _, w := range logger.warnings() {
		if strings.Contains(w, "token-one") || strings.Contains(w, "archiveraccount") {
			t.Errorf("a warning named a credential: %q", w)
		}
	}
}

// TestIRCSilentDropIsNotARefusal is the claim of this round: a session that
// heard NOTHING before the socket died learned nothing about the login, so the
// job must stay credentialed.
//
// The path that makes this matter is real, not hypothetical.
// orchestrator_twitch.go relaunches startChat() on this same downloader the
// instant a connectivity outage is declared over — exactly when the link is
// least trustworthy. Classifying that first shaky reconnect as a refusal would
// latch subscriber-only chat off for the rest of a marathon stream, on evidence
// that never mentioned the credentials.
//
// The assertion is on the SECOND session's wire bytes. "A reconnect happened"
// is satisfied by the broken behaviour too — it would reconnect anonymously.
func TestIRCSilentDropIsNotARefusal(t *testing.T) {
	// No script at all: read the handshake, close, say nothing. A dead socket.
	rep := startIRCReplier(t)
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	firstPass, firstNick := handshakeLines(t, rep.nextSession(t))

	runLiveIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rep.nextSession(t))

	if firstPass != "PASS oauth:token-one" || firstNick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", firstPass, firstNick)
	}
	if cd.authRefused.Load() {
		t.Error("a session that heard nothing at all latched the anonymous fallback")
	}
	if secondPass != "PASS oauth:token-one" {
		t.Errorf("second handshake PASS = %q, want %q — a dropped socket demoted the job's chat "+
			"without Twitch ever saying a word about the login", secondPass, "PASS oauth:token-one")
	}
	if secondNick != "NICK archiveraccount" {
		t.Errorf("second handshake NICK = %q, want %q", secondNick, "NICK archiveraccount")
	}
	if got := logger.fallbackWarnings(); len(got) != 0 {
		t.Errorf("fallback warnings = %q, want none for a dropped connection", got)
	}
}

// TestIRCStrayLineWithoutWelcomeIsARefusal pins the discriminator itself: it is
// "did Twitch talk to us at all without welcoming us", NOT "did it send the
// refusal NOTICE".
//
// Narrowing it to the NOTICE text would mean a Twitch wording change silently
// restores the whole-job chat loss the fallback exists to prevent — the failure
// would be invisible, because chat keeps connecting. So a single stray line
// with no 001 behind it is enough.
func TestIRCStrayLineWithoutWelcomeIsARefusal(t *testing.T) {
	rep := startIRCReplier(t, []string{"PING :tmi.twitch.tv"})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	rep.nextSession(t)

	runLiveIRCSession(t, cd)
	second := rep.nextSession(t)

	assertAnonymousHandshake(t, second)
	assertNotOnTheWire(t, second, "token-one")
	if got := logger.fallbackWarnings(); len(got) != 1 {
		t.Errorf("fallback warnings = %q, want exactly one — a server that spoke without welcoming "+
			"us refused the login, whatever it happened to say", got)
	}
}

// TestIRCWelcomeKeepsTheAuthenticatedLogin is the other side: a login Twitch
// ACCEPTS must survive a dropped connection. RPL_WELCOME is the only thing
// separating "Twitch refused us" from "the socket died", and treating the
// second as the first would demote every long stream to anonymous at its first
// network hiccup.
func TestIRCWelcomeKeepsTheAuthenticatedLogin(t *testing.T) {
	rep := startIRCReplier(t, []string{welcomeLine}, []string{welcomeLine})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	rep.nextSession(t)

	runLiveIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rep.nextSession(t))

	if secondPass != "PASS oauth:token-one" {
		t.Errorf("second handshake PASS = %q, want %q — a welcomed login was demoted by a mere "+
			"disconnect", secondPass, "PASS oauth:token-one")
	}
	if secondNick != "NICK archiveraccount" {
		t.Errorf("second handshake NICK = %q, want %q", secondNick, "NICK archiveraccount")
	}
	if got := logger.fallbackWarnings(); len(got) != 0 {
		t.Errorf("fallback warnings = %q, want none for an accepted login", got)
	}
}

// TestIRCRefusedLoginNoticeIsNamedInTheWarning: when Twitch says why, the single
// Warn says so. This is the ONLY thing the two recognised NOTICE texts are used
// for — they do not trigger the fallback, RPL_WELCOME's absence does.
func TestIRCRefusedLoginNoticeIsNamedInTheWarning(t *testing.T) {
	rep := startIRCReplier(t, []string{loginFailedNotice})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runLiveIRCSession(t, cd)
	rep.nextSession(t)

	got := logger.fallbackWarnings()
	if len(got) != 1 {
		t.Fatalf("fallback warnings = %q, want exactly one", got)
	}
	if !strings.Contains(got[0], "Twitch replied that the login failed") {
		t.Errorf("warning = %q, want it to report that Twitch named the failure", got[0])
	}
}

// TestIRCFallbackIsOneShot: once anonymous, the job stays anonymous. Flapping
// would re-pay the rejected handshake on every reconnect, which is the cost the
// fallback exists to bound — and it would re-log on every one.
func TestIRCFallbackIsOneShot(t *testing.T) {
	// Only the FIRST connection is scripted: it is the credentialed attempt and
	// the only one that can be refused. The three that follow are anonymous and
	// need no reply.
	rep := startIRCReplier(t, []string{loginFailedNotice})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	for range 4 {
		runLiveIRCSession(t, cd)
	}

	rep.nextSession(t) // the credentialed attempt
	for i := 2; i <= 4; i++ {
		lines := rep.nextSession(t)
		assertAnonymousHandshake(t, lines)
		assertNotOnTheWire(t, lines, "token-one")
	}
	if got := logger.fallbackWarnings(); len(got) != 1 {
		t.Errorf("fallback warnings = %q, want exactly one across four sessions", got)
	}
}

// TestIRCAnonymousSessionNeverWarns: an install with no Twitch cookies uses the
// anonymous handshake, which has nothing to fall back FROM. A dropped
// connection there must not raise an alarm about a login that was never
// attempted — most installs are this install.
func TestIRCAnonymousSessionNeverWarns(t *testing.T) {
	// The server talks and never welcomes — everything a refusal looks like
	// except that this session never presented credentials.
	rep := startIRCReplier(t, []string{loginFailedNotice})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t, nil, logger)

	runLiveIRCSession(t, cd)
	assertAnonymousHandshake(t, rep.nextSession(t))

	if got := logger.fallbackWarnings(); len(got) != 0 {
		t.Errorf("fallback warnings = %q, want none for a session that never authenticated", got)
	}
}

// TestIRCShutdownIsNotARefusal: a session WE end — Stop, MarkStreamEnded, or a
// cancelled caller context — reaches the same "no RPL_WELCOME" state as a
// refusal. Latching anonymous there would demote the next job's chat on the
// strength of a clean shutdown.
func TestIRCShutdownIsNotARefusal(t *testing.T) {
	// The server PINGs and never welcomes, so by the time this test cancels,
	// the session has heard from Twitch without being accepted — every
	// condition of a refusal except that WE ended it. The caller-ended guard is
	// therefore the only thing standing between this session and the latch.
	rep := startIRCReplierAwaitingEcho(t, []string{"PING :tmi.twitch.tv"})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()

	// The session runs on the goroutine and the assertions stay on the test's
	// own, so nextSession's t.Fatal is always called from a legal place.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cd.runIRCSession(ctx)
	}()
	rep.nextSession(t)

	// Cancel only once the client's PONG proves it read and handled the PING.
	// Cancelling before that would leave the session having heard nothing, and
	// the DROP rule rather than the caller-ended guard would be what kept the
	// latch clear — the test would pass without testing anything.
	select {
	case echo := <-rep.echoes:
		if !strings.HasPrefix(echo, "PONG") {
			t.Fatalf("echo = %q, want the client's PONG", echo)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the client to answer the server's PING")
	}
	cancel()
	<-done

	if cd.authRefused.Load() {
		t.Error("a cancelled session latched the anonymous fallback")
	}
	if got := logger.fallbackWarnings(); len(got) != 0 {
		t.Errorf("fallback warnings = %q, want none for a session the caller ended", got)
	}

	// And the next session still authenticates.
	runLiveIRCSession(t, cd)
	pass, nick := handshakeLines(t, rep.nextSession(t))
	if pass != "PASS oauth:token-one" || nick != "NICK archiveraccount" {
		t.Errorf("next handshake = (%q, %q), want the authenticated pair", pass, nick)
	}
}

// TestIRCCallerEndedSessionIsNotARefusal pins the OTHER half of the
// caller-ended guard.
//
// The guard reads `ctx.Err() != nil || !cd.IsRunning()`, and only the first
// half has a test — TestIRCShutdownIsNotARefusal above cancels the parent
// context. Deleting `|| !cd.IsRunning()` survived the whole tree, because no
// Twitch test ended a credentialed session through Stop or MarkStreamEnded
// after it had heard a line but before it was welcomed. Those are the two
// ordinary ways a real job ends: the orchestrator marks the stream over, or
// the operator cancels.
//
// The parent context stays LIVE here on purpose. Cancelling it would let the
// already-pinned half carry this test and the assertion would prove nothing.
func TestIRCCallerEndedSessionIsNotARefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		// end is how the caller finishes with this downloader. Stop is an
		// interruption; MarkStreamEnded is the clean end of a live stream.
		// Both clear cd.running and both reach the guard identically.
		end func(*ChatDownloader)
	}{
		{"Stop", (*ChatDownloader).Stop},
		{"MarkStreamEnded", (*ChatDownloader).MarkStreamEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The server PINGs and never welcomes, so by the time the session
			// is ended it has heard from Twitch without being accepted — every
			// condition of a refusal except that WE ended it.
			rep := startIRCReplierAwaitingEcho(t, []string{"PING :tmi.twitch.tv"})
			logger := &recordingLogger{}
			cd := newAuthTestChatDownloaderWithLogger(t,
				staticCredentials("token-one", "archiveraccount"), logger)

			cd.mu.Lock()
			cd.running = true
			cd.mu.Unlock()

			// The session runs on its own goroutine so nextSession's t.Fatal
			// is always called from the test's goroutine.
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = cd.runIRCSession(context.Background())
			}()
			rep.nextSession(t)

			// End it only once the client's PONG proves it read and handled
			// the PING. Ending before that would leave the session having
			// heard nothing, and the DROP rule rather than the caller-ended
			// guard would be what kept the latch clear.
			select {
			case echo := <-rep.echoes:
				if !strings.HasPrefix(echo, "PONG") {
					t.Fatalf("echo = %q, want the client's PONG", echo)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for the client to answer the server's PING")
			}
			tc.end(cd)
			<-done

			if cd.authRefused.Load() {
				t.Errorf("a session ended by %s latched the anonymous fallback — a clean shutdown "+
					"was read as Twitch refusing the login", tc.name)
			}
			if got := logger.fallbackWarnings(); len(got) != 0 {
				t.Errorf("fallback warnings = %q, want none for a session the caller ended", got)
			}

			// And the next session still authenticates. Without this the latch
			// could be re-armed by any later change and only the boolean above
			// would notice.
			runLiveIRCSession(t, cd)
			pass, nick := handshakeLines(t, rep.nextSession(t))
			if pass != "PASS oauth:token-one" || nick != "NICK archiveraccount" {
				t.Errorf("next handshake = (%q, %q), want the authenticated pair", pass, nick)
			}
		})
	}
}

// --- the token-without-a-login state ---

// TestIRCMissingLoginWarnsOncePerDownloader is the fourth silent state.
//
// A cookies.txt holding auth-token and no login is not an error anywhere: the
// handshake renders the full anonymous pair (correctly — a token beside the
// justinfan nickname is the hybrid Twitch rejects), Twitch accepts it, and
// nothing is refused, so the anonymous fallback's Warn cannot fire. Meanwhile
// HasTwitchAuthCookies reads true and both UIs show green. The capture simply
// never carries a subscriber-only message or a badge, for the whole job.
//
// ONCE, across sessions: the condition is a property of the cookie file, so a
// job reconnecting hourly for three days would otherwise repeat the line
// hourly for three days. Three sessions here, one warning.
func TestIRCMissingLoginWarnsOncePerDownloader(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t, staticCredentials("token-one", ""), logger)

	for range 3 {
		runOneIRCSession(t, cd)
		lines := rec.nextSession(t)
		// The wire bytes stay exactly what they were: this is a report, not a
		// behaviour change. In particular the token is still withheld.
		assertAnonymousHandshake(t, lines)
		assertNotOnTheWire(t, lines, "token-one")
	}

	if got := logger.missingLoginWarnings(); len(got) != 1 {
		t.Errorf("missing-login warnings = %q, want exactly one across three sessions", got)
	}
	// It must not be counted as a fallback: every fallback test asserts an
	// exact fallbackWarnings length, so an overlapping phrase would defang
	// them all.
	if got := logger.fallbackWarnings(); len(got) != 0 {
		t.Errorf("fallback warnings = %q, want none — nothing was refused here", got)
	}
	for _, w := range logger.warnings() {
		if strings.Contains(w, "token-one") {
			t.Errorf("a warning named a credential: %q", w)
		}
	}
}

// TestIRCCompleteCredentialsRaiseNoMissingLoginWarning: a working authenticated
// session must say nothing. The warning describes a degradation, and there is
// none here.
func TestIRCCompleteCredentialsRaiseNoMissingLoginWarning(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	runOneIRCSession(t, cd)
	pass, nick := handshakeLines(t, rec.nextSession(t))

	if pass != "PASS oauth:token-one" || nick != "NICK archiveraccount" {
		t.Fatalf("handshake = (%q, %q), want the authenticated pair — a session that never "+
			"authenticated would make the silence below meaningless", pass, nick)
	}
	if got := logger.missingLoginWarnings(); len(got) != 0 {
		t.Errorf("missing-login warnings = %q, want none for a complete credential pair", got)
	}
}

// TestIRCCookielessInstallRaisesNoMissingLoginWarning is the mutant this
// warning is one edit away from becoming.
//
// MOST installs hold no Twitch cookies at all and read public chat through the
// anonymous handshake. A condition that keyed on the missing login alone —
// dropping the `token == ""` half — would warn all of them, on every job, about
// credentials they never had and do not want. That is the difference between
// reporting a degradation and nagging.
func TestIRCCookielessInstallRaisesNoMissingLoginWarning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		credentials func() (string, string)
	}{
		{"no getter at all", nil},
		{"both halves empty", staticCredentials("", "")},
		{"a login with no token behind it", staticCredentials("", "archiveraccount")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := startIRCRecorder(t, 4)
			logger := &recordingLogger{}
			cd := newAuthTestChatDownloaderWithLogger(t, tc.credentials, logger)

			runOneIRCSession(t, cd)
			assertAnonymousHandshake(t, rec.nextSession(t))

			if got := logger.missingLoginWarnings(); len(got) != 0 {
				t.Errorf("missing-login warnings = %q, want none — this install has no Twitch "+
					"credentials and is not missing anything", got)
			}
		})
	}
}

// TestIRCMissingLoginWarningDoesNotDemoteTheJob is the cost of getting the
// latch wrong, and it is not the extra log line.
//
// The obvious economy is to latch this warning on authRefused, which already
// exists and is already one-shot per job. Every other test in this file passes
// if you do — but authRefused is not a log flag: sessionCredentials returns an
// EMPTY PAIR once it is set, so warning through it would permanently demote a
// job to anonymous chat as a side effect of REPORTING that it is anonymous.
// The state this warning describes is the one most likely to be repaired
// mid-job — the operator reads the line and re-exports their cookies — so the
// reconnect that picks the login up is exactly what must keep working.
func TestIRCMissingLoginWarningDoesNotDemoteTheJob(t *testing.T) {
	rec := startIRCRecorder(t, 4)
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t, credentialSequence(
		[2]string{"token-one", ""},
		[2]string{"token-one", "archiveraccount"}), logger)

	runOneIRCSession(t, cd)
	firstLines := rec.nextSession(t)

	runOneIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rec.nextSession(t))

	assertAnonymousHandshake(t, firstLines)
	if got := logger.missingLoginWarnings(); len(got) != 1 {
		t.Fatalf("missing-login warnings = %q, want exactly one — the state this test repairs was "+
			"never reported", got)
	}
	if cd.authRefused.Load() {
		t.Error("reporting the missing login also latched the anonymous fallback — a log line " +
			"turned into a demotion")
	}
	if secondPass != "PASS oauth:token-one" || secondNick != "NICK archiveraccount" {
		t.Errorf("second handshake = (%q, %q), want the authenticated pair — the re-exported login "+
			"cookie never reached the wire", secondPass, secondNick)
	}
}

// TestIRCMissingLoginWarningSurvivesTheFallbackLatch: the two latches are
// independent and must stay so.
//
// authRefused makes sessionCredentials return an empty pair, so once the
// anonymous fallback has latched, every later session looks exactly like a
// cookieless install from inside runIRCSession. If noteMissingLogin keyed off
// anything the fallback also sets — or if the fallback reused warnedNoLogin —
// a refused login would start producing this warning too, telling the operator
// to fix a login cookie that is present and was rejected.
func TestIRCMissingLoginWarningSurvivesTheFallbackLatch(t *testing.T) {
	rep := startIRCReplier(t, []string{loginFailedNotice})
	logger := &recordingLogger{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("token-one", "archiveraccount"), logger)

	for range 3 {
		runLiveIRCSession(t, cd)
		rep.nextSession(t)
	}

	if !cd.authRefused.Load() {
		t.Fatal("the refusal never latched — the state this test is about was never reached")
	}
	if got := logger.fallbackWarnings(); len(got) != 1 {
		t.Errorf("fallback warnings = %q, want exactly one", got)
	}
	if got := logger.missingLoginWarnings(); len(got) != 0 {
		t.Errorf("missing-login warnings = %q, want none — the login cookie is present and was "+
			"refused, which is a different problem with a different remedy", got)
	}
}

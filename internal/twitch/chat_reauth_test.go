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

// Arc 10 R5, downloader half. Every credential here is synthetic and none is
// ever logged.
//
// What the existing fakes cannot do. ircReplier (chat_irc_fallback_test.go)
// closes each connection as soon as its script is written, so the client's
// session ends on its own and a test cannot act on a LIVE session — which is
// the only state Reauthenticate has to work in. The fixture below holds the
// connection open instead.

// pingLine is the one server line a Twitch IRC client answers, which is what
// makes it usable as a round-trip proof.
const pingLine = "PING :tmi.twitch.tv"

// holdingIRCServer answers a handshake, writes a scripted burst, waits for the
// client's PONG, publishes the handshake, and then HOLDS the connection open
// until the client drops it.
//
// Publishing only AFTER the PONG is what makes the wait deterministic:
// receiving from sessions means this session read and handled every scripted
// line. Every script must therefore end in a line the client answers — PING.
//
// The hold matters as much as the script. A server that closed after its
// script would produce a reconnect of its OWN, and a test could not tell it
// from the reconnect Reauthenticate causes.
//
// An EMPTY script is the one exception, and it is the opposite fixture: the
// handshake is read and published and the socket is then closed with nothing
// said, which is a dropped connection — the only way to make Start charge its
// reconnect budget and enter the backoff path on purpose.
type holdingIRCServer struct {
	server   *httptest.Server
	sessions chan []string
}

// startHoldingIRCServer scripts connection i with scripts[i], repeating the
// last entry once they run out. One entry — the ordinary case — therefore
// scripts every connection identically.
func startHoldingIRCServer(t *testing.T, scripts ...[]string) *holdingIRCServer {
	t.Helper()
	h := &holdingIRCServer{sessions: make(chan []string, 32)}
	var mu sync.Mutex
	conns := 0
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		mu.Lock()
		var script []string
		if len(scripts) > 0 {
			script = scripts[min(conns, len(scripts)-1)]
		}
		conns++
		mu.Unlock()

		var lines []string
		for len(lines) < 4 { // PASS, NICK, CAP REQ, JOIN
			_, data, readErr := conn.Read(r.Context())
			if readErr != nil {
				return
			}
			lines = append(lines, string(data))
		}
		// A silent connection has nothing for the client to answer, so waiting
		// for a PONG would park both sides until the client's own six-minute
		// deadline. Publish and drop instead.
		if len(script) == 0 {
			h.sessions <- lines
			return
		}
		for _, line := range script {
			if writeErr := conn.Write(r.Context(), websocket.MessageText, []byte(line)); writeErr != nil {
				return
			}
		}
		if _, _, readErr := conn.Read(r.Context()); readErr != nil { // the PONG
			return
		}
		h.sessions <- lines

		for {
			if _, _, readErr := conn.Read(r.Context()); readErr != nil {
				return
			}
		}
	}))
	t.Cleanup(h.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(h.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return h
}

func (h *holdingIRCServer) nextSession(t *testing.T) []string {
	t.Helper()
	select {
	case lines := <-h.sessions:
		return lines
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an IRC session to reach its scripted state")
		return nil
	}
}

// nextSessionWhileRunning is nextSession for the tests that drive a real Start,
// and it watches Start as well as the fixture.
//
// A Start that has given up produces no further session, so waiting on the
// fixture alone reports the generic ten-second timeout above — the fixture
// complaining that nothing arrived — instead of the reason nothing did. A
// non-blocking poll of done before the wait does not fix that: Start unwinds
// its session, its loop and its deferred flush AFTER the last reconnect the
// test asked for, so the poll almost always loses the race. Selecting on both
// is the only shape that reports the real failure.
//
// n is how many sessions have already been consumed, so the message says where
// it stopped. The value is put back on done, which is buffered, so the
// t.Cleanup that waits for Start still completes instead of adding a second,
// spurious failure on top of the real one.
func (h *holdingIRCServer) nextSessionWhileRunning(t *testing.T, done chan error, n int) []string {
	t.Helper()
	select {
	case lines := <-h.sessions:
		return lines
	case err := <-done:
		done <- err
		t.Fatalf("Start returned after %d session(s) (%v) — it gave up reconnecting instead of "+
			"going straight back in, so a reconnect WE asked for was charged to the budget", n, err)
		return nil
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an IRC session to reach its scripted state")
		return nil
	}
}

// acceptedLoginRecorder counts the Info line an accepted authenticated login
// writes. It records the MESSAGE only, never the args, for the same reason
// recordingLogger does: neither the token nor the login may reach a log line,
// and a recorder that captured args would make a leak invisible here.
type acceptedLoginRecorder struct {
	mu    sync.Mutex
	infos []string
}

func (l *acceptedLoginRecorder) Debug(string, ...any) {}
func (l *acceptedLoginRecorder) Warn(string, ...any)  {}
func (l *acceptedLoginRecorder) Error(string, ...any) {}
func (l *acceptedLoginRecorder) Info(msg string, args ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, msg)
	l.mu.Unlock()
}

// countInfo counts the Info lines whose message is EXACTLY want.
//
// Exact rather than a substring, and the plan's Global Constraints say why in
// terms ("substring checks are no guard"). Both messages counted here are fixed
// literals that the field gate and Task 7's plan text quote verbatim as the
// thing an operator greps for, so a drift — "…accepted, probably" — breaks a
// documented instruction, and a Contains check walks straight past it.
func (l *acceptedLoginRecorder) countInfo(want string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.infos {
		if m == want {
			n++
		}
	}
	return n
}

// acceptedLogins counts only the accepted-login line, so the several other
// Info lines Start writes per reconnect cannot be mistaken for it.
func (l *acceptedLoginRecorder) acceptedLogins() int {
	return l.countInfo("twitch chat: authenticated login accepted")
}

// backoffReconnects counts the BACKOFF path's own Info line — the one written
// beside the sleep, and only there. It is how a test tells "we waited" from "we
// went straight back in" without measuring wall time.
func (l *acceptedLoginRecorder) backoffReconnects() int {
	return l.countInfo("reconnecting to twitch IRC")
}

// TestReauthenticateClearsTheRefusalLatchAndRepresentsCredentials is the
// central claim: a job that went anonymous after a refusal comes back
// credentialed when told to.
//
// The assertion is on the SECOND session's WIRE BYTES. "A second handshake
// happened" is a junction the unfixed code satisfies too — it reconnects
// anonymously.
//
// Two mutations: not resetting authRefused (session 2 is the justinfan pair,
// and the whole feature is dead), and not resetting downgradeReported (the
// second refusal is silent, so the mark and the job notice never fire again).
func TestReauthenticateClearsTheRefusalLatchAndRepresentsCredentials(t *testing.T) {
	rep := startIRCReplier(t, []string{loginFailedNotice}, []string{loginFailedNotice})
	var reports downgradeRecorder
	logger := &acceptedLoginRecorder{}
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), logger, reports.record)

	runLiveIRCSession(t, cd)
	firstPass, firstNick := handshakeLines(t, rep.nextSession(t))
	if firstPass != "PASS oauth:test-token-aaaa" || firstNick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", firstPass, firstNick)
	}
	if !cd.authRefused.Load() {
		t.Fatal("the fixture is wrong: the refusal did not latch the anonymous fallback")
	}
	reports.assertReportedExactly(t, AuthDowngradeLoginRefused)

	cd.Reauthenticate()

	runLiveIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rep.nextSession(t))
	if secondPass != "PASS oauth:test-token-aaaa" || secondNick != "NICK archiveraccount" {
		t.Errorf("second handshake = (%q, %q), want the authenticated pair — the refusal latch survived Reauthenticate", secondPass, secondNick)
	}
	reports.assertReportedExactly(t, AuthDowngradeLoginRefused, AuthDowngradeLoginRefused)

	// Both sessions were REFUSED. The accepted-login line is the operator's only
	// positive confirmation that a repaired credential was taken, and the field
	// gate tells them to grep for it — so a false positive on a refusal is its
	// worst possible failure, and the twelve-welcomed-sessions count elsewhere
	// in this file is one-sided without this.
	//
	// The mutation: hoisting that Info line out of the `if ircIsWelcome(line)`
	// arm, where it fires on the first inbound line of ANY credentialed session
	// — the "Login authentication failed" NOTICE included.
	if got := logger.acceptedLogins(); got != 0 {
		t.Errorf("accepted-login lines = %d across two REFUSED sessions, want none", got)
	}
}

// TestReauthenticateReReportsAMissingLoginCookie is the latch the design's
// two-name list missed.
//
// noteMissingLogin returns early on warnedNoLogin.Swap(true) BEFORE it reaches
// reportAuthDowngrade, so leaving that flag set means a repaired cookie file
// that is STILL missing its login row reports nothing the second time — the
// exact silence this arc exists to end.
//
// The mutation: resetting only authRefused and downgradeReported.
func TestReauthenticateReReportsAMissingLoginCookie(t *testing.T) {
	rep := startIRCReplier(t, nil, nil)
	var reports downgradeRecorder
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", ""), &recordingLogger{}, reports.record)

	runLiveIRCSession(t, cd)
	assertAnonymousHandshake(t, rep.nextSession(t))
	reports.assertReportedExactly(t, AuthDowngradeNoLoginCookie)

	cd.Reauthenticate()

	runLiveIRCSession(t, cd)
	assertAnonymousHandshake(t, rep.nextSession(t))
	reports.assertReportedExactly(t, AuthDowngradeNoLoginCookie, AuthDowngradeNoLoginCookie)
}

// TestReauthenticateOnAnIdleDownloaderDoesNotArmThePendingFlag.
//
// reauthPending is set only when there IS a session to interrupt. Setting it
// unconditionally would leave the flag standing until some LATER session
// ended, and that session's handshake-outcome defer would then read a GENUINE
// refusal as our own cancel: the fallback would never latch and the job would
// spend its whole reconnect budget on a login Twitch will not take.
//
// The mutation: `cd.reauthPending.Store(true)` outside the sessionCancel != nil
// check.
func TestReauthenticateOnAnIdleDownloaderDoesNotArmThePendingFlag(t *testing.T) {
	rep := startIRCReplier(t, []string{loginFailedNotice})
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), &recordingLogger{})

	cd.Reauthenticate() // no live session
	if cd.reauthPending.Load() {
		t.Fatal("Reauthenticate armed the pending flag with no session to interrupt")
	}

	runLiveIRCSession(t, cd)
	_ = rep.nextSession(t)
	if !cd.authRefused.Load() {
		t.Error("a genuine refusal did not latch the anonymous fallback — a stale pending flag suppressed it")
	}
}

// TestReauthenticateDoesNotLatchTheFallbackOnItsOwnCancel.
//
// The window is real: Reauthenticate can land between the CAP ACK and the 001,
// and runIRCSession's deferred noteHandshakeOutcome then sees welcomed=false
// with heardFromServer=true — indistinguishable from "Twitch spoke and never
// acknowledged the login" unless the defer knows WE cancelled.
//
// The script is a PING and no welcome, so the client has heard from the server
// and has not been welcomed at the moment the test acts. The PONG the fixture
// waits for is the proof of exactly that state.
//
// The mutation: dropping `cd.reauthPending.Load()` from the defer's guard.
// Under it the reconnect Reauthenticate asked for comes back ANONYMOUS and a
// spurious login-never-acknowledged is reported — the feature inverts itself.
func TestReauthenticateDoesNotLatchTheFallbackOnItsOwnCancel(t *testing.T) {
	h := startHoldingIRCServer(t, []string{pingLine})
	var reports downgradeRecorder
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), &recordingLogger{}, reports.record)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
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

	first := h.nextSessionWhileRunning(t, done, 0)
	if pass, nick := handshakeLines(t, first); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", pass, nick)
	}

	cd.Reauthenticate()

	second := h.nextSessionWhileRunning(t, done, 1)
	if pass, nick := handshakeLines(t, second); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Errorf("second handshake = (%q, %q), want the authenticated pair — our own cancel was read as a refusal", pass, nick)
	}
	if cd.authRefused.Load() {
		t.Error("the anonymous fallback latched on a session WE cancelled")
	}
	reports.assertReportedExactly(t)
}

// TestReauthenticateSurvivesAVerdictThatLandsAfterTheArm is review finding 1,
// Sequence A: the outgoing session's handshake verdict is decided BEFORE
// Reauthenticate arms reauthPending, but lands AFTER Reauthenticate has cleared
// the three latches — so authRefused is set again on the new credential pair,
// sessionCredentials returns an empty pair, and the reconnect the operator's
// repair asked for goes out ANONYMOUS and stays that way for the job. There is
// no second chance: Task 3 fires OnCredentialsChanged only when the fingerprint
// MOVES, and it has not moved since.
//
// Determinism comes from a barrier rather than a sleep. Start's reauth branch
// flushes before it re-clears, and flush takes flushMu unconditionally (the
// emptiness test is inside flushLocked), so a test holding flushMu parks the
// branch at exactly the instant Sequence A's verdict lands: after the arm,
// before the re-clear. The lock is taken BEFORE Reauthenticate, so the branch
// cannot slip past it.
//
// The verdict is delivered by hand because the interleaving that produces it —
// the handshake defer's guard reading reauthPending microseconds before
// Reauthenticate arms it — has no synchronisation point a test can hook.
// noteHandshakeOutcome is the real production call the real defer makes, with
// the arguments a refused credentialed session produces.
//
// The mutation: drop the three Stores from Start's reauth branch. Session two
// comes back as the justinfan pair. (Its sibling half — moving the resets back
// outside Reauthenticate's critical section, which closes Sequence B — is a
// lock-ordering invariant with no behavioural witness; see the report.)
func TestReauthenticateSurvivesAVerdictThatLandsAfterTheArm(t *testing.T) {
	h := startHoldingIRCServer(t, []string{pingLine})
	var reports downgradeRecorder
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), &recordingLogger{}, reports.record)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
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

	if pass, nick := handshakeLines(t, h.nextSessionWhileRunning(t, done, 0)); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", pass, nick)
	}

	cd.flushMu.Lock()
	cd.Reauthenticate()
	cd.noteHandshakeOutcome(false, true, true)
	latched := cd.authRefused.Load()
	cd.flushMu.Unlock()

	// Nothing below means anything unless the injected verdict really did
	// re-latch the fallback after the reset.
	if !latched {
		t.Fatal("the fixture is wrong: the verdict injected after the reset did not latch the fallback")
	}

	second := h.nextSessionWhileRunning(t, done, 1)
	if pass, nick := handshakeLines(t, second); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Errorf("second handshake = (%q, %q), want the authenticated pair — the dying session's "+
			"verdict landed after the reset and demoted the reconnect it asked for", pass, nick)
	}
	if cd.authRefused.Load() {
		t.Error("the anonymous fallback is still latched after the reconnect Reauthenticate asked for")
	}

	// Exactly the injected one. The report that fired between the reset and the
	// re-clear is the residual the review priced and declined to suppress: at
	// the verdict the code cannot know a reset is coming, and holding cd.mu
	// across noteHandshakeOutcome's Warn to fix it is the worse trade. It is a
	// stale report on the new pair, not a stale HANDSHAKE, and it belongs to
	// the sticky mark in Tasks 6/7 rather than to this file.
	reports.assertReportedExactly(t, AuthDowngradeLoginRefused)
}

// TestReauthenticateSpendsNoReconnectBudget.
//
// maxReconnects is 10, so eleven credential-driven reconnects exhaust the
// budget if each costs one. The assertion is on session TWELVE arriving and on
// Start not having returned — not on elapsed time, which would be flaky.
//
// Two mutations, both caught. Charging the budget: Start returns "exceeded max
// IRC reconnects" and session twelve never arrives. Keeping the backoff ON TOP
// of that charge: the delays run 2s, 4s, 8s, 16s, 30s, 30s..., so nextSession's
// 10-second wait trips by the fifth cycle. (The backoff SKIP has a test of its
// own below — with the budget uncharged, reconnectAttempts never leaves zero
// and the backoff block is unreachable from here.)
//
// The script is a welcome followed by a PING, so every session is fully
// established (welcomed=true) before the test acts on it — which keeps this
// test independent of the handshake-defer guard the previous test owns.
func TestReauthenticateSpendsNoReconnectBudget(t *testing.T) {
	h := startHoldingIRCServer(t, []string{welcomeLine, pingLine})
	logger := &acceptedLoginRecorder{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), logger)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
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

	// The initial session plus eleven forced reconnects: one more than the
	// budget, so a budget-charging implementation gives up before the last.
	const forcedReconnects = 11
	for i := 0; i <= forcedReconnects; i++ {
		lines := h.nextSessionWhileRunning(t, done, i)
		pass, nick := handshakeLines(t, lines)
		if pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
			t.Fatalf("session %d handshake = (%q, %q), want the authenticated pair", i, pass, nick)
		}
		if i < forcedReconnects {
			cd.Reauthenticate()
		}
	}

	// R5's "a credentialed 001 logs at Info". Every session in this test was
	// welcomed, so every session must have said so — that line is the only
	// positive confirmation an operator gets that a repaired credential was
	// accepted, and the field gate asks them to look for it.
	//
	// The mutation: not logging on 001 at all, which leaves the operator with
	// only the ABSENCE of a downgrade report, and absence is not evidence.
	if got := logger.acceptedLogins(); got != forcedReconnects+1 {
		t.Errorf("accepted-login lines = %d across %d welcomed sessions, want one each", got, forcedReconnects+1)
	}

	// Nothing in this test may ever reach the backoff path. It is the other
	// half of "spends no budget": reconnectAttempts never leaves zero, which is
	// also why the stability reset this branch now carries is a no-op here and
	// has to be argued rather than asserted (see the report).
	if got := logger.backoffReconnects(); got != 0 {
		t.Errorf("backoff reconnects = %d across %d credential-driven reconnects, want none", got, forcedReconnects)
	}
}

// TestReauthenticateSkipsTheBackoffAfterAGenuineError pins the `immediate`
// flag, which the budget test above cannot reach.
//
// The two counters answer different questions — how often the network has
// failed us, and whether to wait before the next attempt — and only the second
// is what a repaired credential must not pay. With reconnectAttempts left at
// zero (every session in the test above ended because WE cancelled it) the
// backoff block is unreachable, so deleting `&& !immediate` survives it. Here
// one session is dropped by the server first, which charges the budget for
// real; the credential-driven reconnect that follows must still go straight
// back in.
//
// The assertion is the backoff path's own log line, not elapsed time: the
// backoff writes "reconnecting to twitch IRC" beside its sleep and nothing else
// does. Exactly one, for the genuine drop. Under the mutation there are two.
func TestReauthenticateSkipsTheBackoffAfterAGenuineError(t *testing.T) {
	// Connection one is answered with silence and a close — a dropped socket,
	// which is the only thing that charges the reconnect budget. Every
	// connection after it is welcomed and held.
	h := startHoldingIRCServer(t, nil, []string{welcomeLine, pingLine})
	logger := &acceptedLoginRecorder{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), logger)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
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

	_ = h.nextSessionWhileRunning(t, done, 0) // the dropped one: reconnectAttempts becomes 1
	_ = h.nextSessionWhileRunning(t, done, 1) // reached through the backoff path, and it logged

	if got := logger.backoffReconnects(); got != 1 {
		t.Fatalf("backoff reconnects before the credential change = %d, want exactly one — the "+
			"fixture never charged the reconnect budget, so this test proves nothing", got)
	}

	cd.Reauthenticate()

	third := h.nextSessionWhileRunning(t, done, 2)
	if pass, nick := handshakeLines(t, third); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Errorf("third handshake = (%q, %q), want the authenticated pair", pass, nick)
	}
	if got := logger.backoffReconnects(); got != 1 {
		t.Errorf("backoff reconnects = %d, want still exactly one — the reconnect the operator's "+
			"repaired credential asked for waited out a network backoff it did not cause", got)
	}
}

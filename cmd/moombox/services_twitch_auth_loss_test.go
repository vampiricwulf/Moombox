package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// Arc 10 R1's delivery contract, at the one hop that implements it.
//
// twitchAuthLossHook is the only asynchronous step between an IRC session
// noticing it went anonymous and the platform mark being written:
// ChatDownloader calls its OnAuthDowngrade inline (chat.go's contract: "must
// not block: the read loop is waiting behind it"), and the worker's
// twitchChatDowngradeCallback calls the hook inline too. Everything downstream
// of the `go` is synchronous — NoteTwitchAuthLoss takes the refresh service's
// write lock and then fires OnAuthChange and OnRecoveryNeeded inline.

// authLossLogger records Error lines on a channel, so a test can WAIT for one
// that is written on another goroutine rather than poll for it. The other
// three levels are discarded; no cookie value can reach any of them.
type authLossLogger struct {
	errors chan string
}

func newAuthLossLogger() *authLossLogger {
	return &authLossLogger{errors: make(chan string, 4)}
}

func (l *authLossLogger) Debug(string, ...any) {}
func (l *authLossLogger) Info(string, ...any)  {}
func (l *authLossLogger) Warn(string, ...any)  {}
func (l *authLossLogger) Error(msg string, args ...any) {
	select {
	case l.errors <- msg:
	default:
	}
}

// TestTwitchAuthLossHookDoesNotBlockItsCaller is the whole reason this seam is
// a function rather than three lines inside initServices.
//
// The caller is internal/twitch's IRC session goroutine with the read loop
// parked behind it. Drop the `go` and that read loop waits on the refresh
// service's write lock, an OnAuthChange fan-out and handleRecoveryNeeded's
// disabled arm (a config-store read, a Warn, a cooldown-map lock and a
// notification fan-out) — none of it work the chat path controls or can bound.
//
// THE MUTATION: dropping the `go` in twitchAuthLossHook. The hook then does
// not return until `mark` does, so the first select falls through to the timer
// instead of hanging the suite.
func TestTwitchAuthLossHookDoesNotBlockItsCaller(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	// Always released, including on t.Fatal below, so a parked mark unwinds
	// instead of leaking for the rest of the run.
	defer close(release)

	hook := twitchAuthLossHook(func(string) {
		close(entered)
		<-release
	}, newAuthLossLogger())

	returned := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the hook panicked in its caller: %v", r)
			}
		}()
		hook(twitch.AuthDowngradeNoLoginCookie)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the hook had not returned while the mark was still parked — its caller is the " +
			"IRC session goroutine with the read loop waiting behind it, and every chat message " +
			"for the duration is dropped")
	}

	// Non-vacuous: a hook that returns promptly because it never calls the
	// mark at all would pass the assertion above.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the mark was never called — the hook returned because it does nothing, which is " +
			"not the property under test")
	}
}

// TestTwitchAuthLossHookForwardsTheReasonOnce.
//
// The reason is a closed vocabulary token and it is the only thing that tells
// the operator WHICH of the chat-downgrade routes broke, so a hook that
// forwarded a constant would render one fixed sentence for all four.
//
// THE MUTATION: passing a literal instead of `reason` (the equality check
// fails), or calling `mark` twice (the second receive fails). Both are live
// hazards in a body whose whole content is one forwarded call.
func TestTwitchAuthLossHookForwardsTheReasonOnce(t *testing.T) {
	got := make(chan string, 4)
	hook := twitchAuthLossHook(func(reason string) { got <- reason }, newAuthLossLogger())

	hook(twitch.AuthDowngradeUnusableLoginCookie)

	select {
	case reason := <-got:
		if reason != twitch.AuthDowngradeUnusableLoginCookie {
			t.Errorf("the mark was called with %q, want %q — the four reasons render four different "+
				"sentences and a forwarded constant would collapse them to one",
				reason, twitch.AuthDowngradeUnusableLoginCookie)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the mark was never called, so no downgrade reaches the platform status at all")
	}

	select {
	case extra := <-got:
		t.Errorf("the mark was called a second time with %q — one downgrade is one mark", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTwitchAuthLossHookSurvivesAPanickingMark.
//
// The goroutine this hook spawns is one of the project's "every goroutine
// carries an inline recover" cases, and it is not decorative here: a panic on
// an unrecovered goroutine takes the whole process down, so a bug reachable
// only from a dead-credential path would kill every running capture.
//
// THE MUTATION: dropping the recover. The panic then crosses the goroutine
// boundary and the test BINARY dies — which is the catch.
func TestTwitchAuthLossHookSurvivesAPanickingMark(t *testing.T) {
	log := newAuthLossLogger()
	hook := twitchAuthLossHook(func(string) { panic("the mark blew up") }, log)

	hook(twitch.AuthDowngradeLoginRefused)

	select {
	case msg := <-log.errors:
		if msg != "panic marking twitch auth loss" {
			t.Errorf("the recover logged %q, want the line an operator can grep for", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was logged for a panicking mark — a recover that swallows silently is " +
			"how this failure stays invisible")
	}
}

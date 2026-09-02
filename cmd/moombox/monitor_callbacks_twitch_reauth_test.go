package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
)

// Arc 10 R5's last hop. OnCredentialsChanged now fires for both platforms, and
// only one of them has live chat sessions to reach.

// TestOnlyATwitchCredentialChangeBroadcasts is the platform gate.
//
// The mutation: dropping the gate, so a YouTube cookie rotation drops and
// re-establishes every live Twitch IRC session. That is not merely wasteful —
// each reconnect re-runs the handshake and, on a marathon stream, a YouTube
// refresh cadence would keep tearing chat down for no reason at all.
func TestOnlyATwitchCredentialChangeBroadcasts(t *testing.T) {
	calls := 0
	broadcast := func() int { calls++; return 3 }

	if got := reauthenticateTwitchChats("youtube", broadcast); got != 0 {
		t.Errorf("a youtube credential change broadcast to %d sessions, want 0", got)
	}
	if calls != 0 {
		t.Errorf("the broadcaster was called %d times for youtube, want 0", calls)
	}

	if got := reauthenticateTwitchChats("twitch", broadcast); got != 3 {
		t.Errorf("a twitch credential change broadcast to %d sessions, want 3", got)
	}
	if calls != 1 {
		t.Errorf("the broadcaster was called %d times for twitch, want 1", calls)
	}
}

// TestBroadcastWithNoWorkerIsSafe. wireMonitorCallbacks runs during startup and
// a test harness may build a runState without a worker; a nil deref at the
// moment an operator repairs their credentials is the worst time for one.
//
// The mutation: dropping the nil guard.
func TestBroadcastWithNoWorkerIsSafe(t *testing.T) {
	if got := reauthenticateTwitchChats("twitch", nil); got != 0 {
		t.Errorf("a broadcast with no worker returned %d, want 0", got)
	}
}

// TestUnknownPlatformDoesNotBroadcast. The callback's platform argument comes
// from RefreshService and is "youtube" or "twitch" today; an equality test
// rather than a not-youtube test is what keeps a third platform from silently
// inheriting Twitch behaviour.
//
// The mutation: `if platform != "youtube"` instead of `if platform != "twitch"`.
func TestUnknownPlatformDoesNotBroadcast(t *testing.T) {
	calls := 0
	if got := reauthenticateTwitchChats("kick", func() int { calls++; return 2 }); got != 0 || calls != 0 {
		t.Errorf("an unknown platform broadcast to %d sessions with %d calls, want 0 and 0", got, calls)
	}
}

// repairCallbackState builds the minimum runState wireCredentialRepairCallbacks
// needs, and returns it with a counter the broadcast increments.
//
// A real RefreshService over an empty jar (no network is reached — nothing
// calls a check) and a real empty database, following
// monitor_callbacks_recovery_test.go's fixture. The empty DB is load-bearing:
// resumeCookieParkedJobs finds no jobs, so `resumed` stays 0, so notifyMgr is
// never touched and may stay nil.
func repairCallbackState(t *testing.T) (*runState, *int) {
	t.Helper()
	log, err := logger.New(filepath.Join(t.TempDir(), "repair.log"), "error", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)

	db, err := database.Open(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := &runState{
		log:           log,
		db:            db,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log),
	}
	calls := 0
	s.wireCredentialRepairCallbacks(func() int { calls++; return 1 })
	return s, &calls
}

// TestBothRepairEdgesBroadcastForTwitch is the Task 3 review's finding 1.
//
// OnAuthRecovered is a repair edge OnCredentialsChanged does not cover: a
// transient Twitch refusal, or an operator restoring the exact pair they had
// before, brings validate back to authenticated with the credential
// fingerprint UNCHANGED, so shouldObserveCredentials returns false and
// OnCredentialsChanged never fires. Wired to that callback alone, a chat
// session that went anonymous on the refusal stays anonymous until the job
// ends — R5's "immediately" simply not covered for that path.
//
// THE MUTATION: dropping `reauth(platform)` from the OnAuthRecovered closure
// (which is how the plan's first draft had it). The first subtest then reports
// 0 broadcasts. Dropping it from OnCredentialsChanged fails the second.
func TestBothRepairEdgesBroadcastForTwitch(t *testing.T) {
	t.Run("auth recovered", func(t *testing.T) {
		s, calls := repairCallbackState(t)
		s.cookieRefresh.OnAuthRecovered("twitch")
		if *calls != 1 {
			t.Errorf("OnAuthRecovered(\"twitch\") broadcast %d times, want 1 — a transient refusal that heals produces no OnCredentialsChanged, so this is the only edge covering it", *calls)
		}
	})

	t.Run("credentials changed", func(t *testing.T) {
		s, calls := repairCallbackState(t)
		s.cookieRefresh.OnCredentialsChanged("twitch", "an-opaque-identity-token")
		if *calls != 1 {
			t.Errorf("OnCredentialsChanged(\"twitch\") broadcast %d times, want 1", *calls)
		}
	})
}

// TestAYouTubeRepairDoesNotBroadcast: the platform gate, driven through the
// REGISTERED callbacks rather than through the helper.
//
// A YouTube cookie rotation is routine and fires both edges on its own
// cadence. Broadcasting there would drop and re-establish every live Twitch
// IRC session for a credential that has nothing to do with them — on a
// marathon stream, repeatedly.
//
// THE MUTATION: calling `broadcast()` from `reauth` without the platform
// filter, or filtering on `platform != "youtube"`.
func TestAYouTubeRepairDoesNotBroadcast(t *testing.T) {
	s, calls := repairCallbackState(t)

	s.cookieRefresh.OnAuthRecovered("youtube")
	s.cookieRefresh.OnCredentialsChanged("youtube", "an-opaque-identity-token")

	if *calls != 0 {
		t.Errorf("a YouTube repair broadcast to Twitch chat sessions %d times, want 0", *calls)
	}
}

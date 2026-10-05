package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
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
// needs, and returns it with two counters: one the Twitch chat broadcast
// increments, one the YouTube membership-memo clear does.
//
// A real RefreshService over an empty jar (no network is reached — nothing
// calls a check) and a real empty database, following
// monitor_callbacks_recovery_test.go's fixture. The empty DB is load-bearing:
// resumeCookieParkedJobs finds no jobs, so `resumed` stays 0, so notifyMgr is
// never touched and may stay nil. Since A4 the empty DB is only half of that —
// the close also fires when a failure was ANNOUNCED for the platform, so this
// fixture's wasNotified answers false and keeps that second edge shut too.
func repairCallbackState(t *testing.T) (*runState, *int, *int) {
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
	memoClears := 0
	s.wireCredentialRepairCallbacks(
		func() int { calls++; return 1 },
		func() int { memoClears++; return 2 },
		// No failure was ever announced in these fixtures, so the A4 close
		// stays shut and notifyMgr is never reached — which is what lets it
		// stay nil here.
		func(string) bool { return false },
	)
	return s, &calls, &memoClears
}

// repairCallbackStateWithNotice is repairCallbackState with a recorder
// installed as the notifier and a caller-chosen wasNotified, for the tests that
// assert on the embed rather than on the broadcast counters.
func repairCallbackStateWithNotice(t *testing.T, wasNotified func(string) bool) (*runState, *notificationtest.Recorder) {
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

	rec := notificationtest.New()
	s := &runState{
		log:           log,
		db:            db,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log),
		notifyMgr:     rec,
	}
	s.wireCredentialRepairCallbacks(func() int { return 0 }, func() int { return 0 }, wasNotified)
	return s, rec
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
		s, calls, _ := repairCallbackState(t)
		s.cookieRefresh.OnAuthRecovered("twitch")
		if *calls != 1 {
			t.Errorf("OnAuthRecovered(\"twitch\") broadcast %d times, want 1 — a transient refusal that heals produces no OnCredentialsChanged, so this is the only edge covering it", *calls)
		}
	})

	t.Run("credentials changed", func(t *testing.T) {
		s, calls, _ := repairCallbackState(t)
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
	s, calls, _ := repairCallbackState(t)

	s.cookieRefresh.OnAuthRecovered("youtube")
	s.cookieRefresh.OnCredentialsChanged("youtube", "an-opaque-identity-token")

	if *calls != 0 {
		t.Errorf("a YouTube repair broadcast to Twitch chat sessions %d times, want 0", *calls)
	}
}

// TestAuthRecoveredFiresWithNothingParked is A4. The close was gated on
// resumed > 0, so a platform whose cookies died BETWEEN recordings got the
// loudest family Moombox sends — "Cookie Auto-Refresh Failed", Error, a
// 30-minute cooldown — and then, once the operator repaired it, silence. The
// failure that opened the incident had no close.
//
// THE MUTANT: restoring `if resumed > 0`. The first subtest records nothing.
func TestAuthRecoveredFiresWithNothingParked(t *testing.T) {
	t.Run("a notified failure gets its close even with no parked jobs", func(t *testing.T) {
		s, rec := repairCallbackStateWithNotice(t, func(string) bool { return true })
		s.cookieRefresh.OnAuthRecovered("youtube")

		got := rec.ByEvent("auth_recovered")
		if len(got) != 1 {
			t.Fatalf("recorded %d auth_recovered notifications, want 1: %+v", len(got), rec.Calls())
		}
		if got[0].Title != "Authentication Recovered" {
			t.Errorf("title = %q, want \"Authentication Recovered\"", got[0].Title)
		}
		if !strings.Contains(got[0].Description, "no jobs were parked") {
			t.Errorf("description = %q — with nothing resumed it must say so rather than claim 0 jobs were resumed", got[0].Description)
		}
	})

	t.Run("no prior failure means no close", func(t *testing.T) {
		s, rec := repairCallbackStateWithNotice(t, func(string) bool { return false })
		s.cookieRefresh.OnAuthRecovered("youtube")
		if got := rec.Calls(); len(got) != 0 {
			t.Errorf("recorded %d notifications for a recovery nobody was told about: %+v", len(got), got)
		}
	})
}

// TestAuthRecoveredClosesOncePerEpisode drives A4's close through the REAL
// wasNotified from withAuthFailureCooldown rather than a fixture predicate,
// because the bug lived in the predicate and nowhere else.
//
// OnAuthRecovered is not a rare edge: it also fires on the first successful
// validate of a healthy refresh pass, so a long-lived process crosses it over
// and over. While the announcement stamp was "deliberately never cleared",
// every one of those crossings re-fired "Authentication Recovered" for a
// failure that had been closed hours earlier — the close became periodic noise
// attached to an incident nobody remembered.
//
// Consuming the stamp also settles the other half: the 30-minute cooldown
// exists to suppress repeats INSIDE an episode, so a failure arriving after a
// close must announce at once. Were the close to keep a separate "already
// closed" bool and leave the stamp standing, the second failure would be
// swallowed as a repeat and ITS recovery would have nothing to close.
//
// THE MUTANT: `return !last[platform].IsZero()` without the delete. The first
// subtest records 3 closes; the second records 1 failure and 1 close.
func TestAuthRecoveredClosesOncePerEpisode(t *testing.T) {
	// announceFailure drives the real cooldown wrapper, counting what actually
	// reached the operator, and returns the predicate the callbacks read.
	newEpisodeTracker := func() (announce func(platform string), failures *int, wasNotified func(string) bool) {
		n := 0
		notify, pred := withAuthFailureCooldown(func(string, string, string, notifications.NotificationType) { n++ })
		return func(platform string) {
			notify(platform, "Cookie Auto-Refresh Failed", "the session is dead", notifications.TypeError)
		}, &n, pred
	}

	t.Run("one announced failure closes once however many recovery edges follow", func(t *testing.T) {
		announce, failures, wasNotified := newEpisodeTracker()
		s, rec := repairCallbackStateWithNotice(t, wasNotified)

		announce("youtube")
		s.cookieRefresh.OnAuthRecovered("youtube")
		s.cookieRefresh.OnAuthRecovered("youtube")
		s.cookieRefresh.OnAuthRecovered("youtube")

		if *failures != 1 {
			t.Fatalf("the fixture announced %d failures, want 1 — the premise is wrong before the close is even read", *failures)
		}
		if got := rec.ByEvent("auth_recovered"); len(got) != 1 {
			t.Errorf("one failure and three recovery edges produced %d \"Authentication Recovered\" embeds, want 1: %+v", len(got), got)
		}
	})

	t.Run("a failure after the close is a new episode with its own close", func(t *testing.T) {
		announce, failures, wasNotified := newEpisodeTracker()
		s, rec := repairCallbackStateWithNotice(t, wasNotified)

		announce("youtube")
		s.cookieRefresh.OnAuthRecovered("youtube")
		// Well inside the 30-minute window: this must NOT be treated as a
		// repeat, because the episode it would be repeating is closed.
		announce("youtube")
		s.cookieRefresh.OnAuthRecovered("youtube")

		if *failures != 2 {
			t.Errorf("the second failure reached the operator %d times in total, want 2 — a closed episode must not keep swallowing its successor", *failures)
		}
		if got := rec.ByEvent("auth_recovered"); len(got) != 2 {
			t.Errorf("two failure episodes produced %d closes, want 2: %+v", len(got), got)
		}
	})

	t.Run("a platform that was never announced never closes", func(t *testing.T) {
		announce, _, wasNotified := newEpisodeTracker()
		s, rec := repairCallbackStateWithNotice(t, wasNotified)

		announce("youtube")
		s.cookieRefresh.OnAuthRecovered("twitch")

		if got := rec.Calls(); len(got) != 0 {
			t.Errorf("recorded %d notifications for a platform nobody was told about: %+v", len(got), got)
		}
	})
}

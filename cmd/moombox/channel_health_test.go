package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// freshSibling is a sibling monitor that vouches for the channel: zero
// consecutive errors AND a check inside crossMonitorVouchWindow. The freshness
// is load-bearing — a zero or stale LastCheckedAt makes siblingReachable
// return false, and the suppression subtest would then pass for the wrong
// reason.
func freshSibling(channelID string) channelHealthReporter {
	return fakeHealthReporter{health: []monitor.ChannelHealth{{
		ChannelID:         channelID,
		LastCheckedAt:     time.Now().UnixMilli(),
		ConsecutiveErrors: 0,
	}}}
}

// TestChannelHealthNotifiersPairAnAlertWithItsClose is audit A3's cmd half.
//
// Mutants this kill:
//   - closing a streak whose alert was SUPPRESSED by the cross-monitor
//     confirmation: the operator gets "Channel Recovered" for a channel they
//     were never told about. The tracker's own `notified` flag cannot see the
//     suppression — it is applied here, one layer up — which is why the sent
//     set lives in this closure and not in internal/monitor.
//   - firing the close for a channel that never alerted at all.
func TestChannelHealthNotifiersPairAnAlertWithItsClose(t *testing.T) {
	t.Run("alert then close", func(t *testing.T) {
		rec := notificationtest.New()
		unhealthy, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube")

		unhealthy("UC_dead", 20, "404")
		if got := len(rec.ByEvent("channel_unhealthy")); got != 1 {
			t.Fatalf("recorded %d channel_unhealthy calls, want 1", got)
		}
		rec.Reset()

		healthy("UC_dead")
		calls := rec.ByEvent("channel_healthy")
		if len(calls) != 1 {
			t.Fatalf("recorded %d channel_healthy calls, want 1", len(calls))
		}
		if calls[0].Type != notifications.TypeSuccess {
			t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
		}

		rec.Reset()
		healthy("UC_dead")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("a second recovery recorded %d calls, want 0", got)
		}
	})

	t.Run("a suppressed alert has no close", func(t *testing.T) {
		rec := notificationtest.New()
		// A sibling that vouches for the channel suppresses the alert.
		unhealthy, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", freshSibling("UC_ok"))

		unhealthy("UC_ok", 20, "404")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("a suppressed streak recorded %d calls, want 0", got)
		}
		healthy("UC_ok")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("the close of a suppressed streak recorded %d calls, want 0", got)
		}
	})

	t.Run("a recovery with no alert says nothing", func(t *testing.T) {
		rec := notificationtest.New()
		_, healthy := channelHealthNotifiers(rec, &nopLogger{}, "twitch")
		healthy("streamer")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("recorded %d calls, want 0", got)
		}
	})
}

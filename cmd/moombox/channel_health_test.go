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
		unhealthy, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", newChannelIncidents())

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
		unhealthy, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", newChannelIncidents(), freshSibling("UC_ok"))

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
		_, healthy := channelHealthNotifiers(rec, &nopLogger{}, "twitch", newChannelIncidents())
		healthy("streamer")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("recorded %d calls, want 0", got)
		}
	})
}

// "Channel Not Responding" says NO monitor reaches the channel — one incident
// however many monitors observe it. With a set per monitor, a real YouTube
// outage (the feed and DECAPI both failing) sent two identical alerts, and
// when DECAPI reached the channel again while RSS kept failing only one was
// closed; the other stayed open for good. The two monitors now share one.
//
// Mutant: the alert sent without checking the shared set — two alerts.
func TestOneYouTubeOutageIsOneIncident(t *testing.T) {
	rec := notificationtest.New()
	shared := newChannelIncidents()
	feedUnhealthy, feedHealthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", shared)
	decapiUnhealthy, decapiHealthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", shared)

	feedUnhealthy("UC_dead", 5, "rss 404")
	decapiUnhealthy("UC_dead", 5, "decapi 404")
	if got := len(rec.ByEvent("channel_unhealthy")); got != 1 {
		t.Fatalf("one outage seen by two monitors sent %d alerts, want 1", got)
	}

	decapiHealthy("UC_dead") // DECAPI reaches it again; RSS still failing
	feedHealthy("UC_dead")
	if got := len(rec.ByEvent("channel_healthy")); got != 1 {
		t.Errorf("closes = %d, want exactly 1 for the one alert", got)
	}
}

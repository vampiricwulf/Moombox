package main

import (
	"strings"
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

type warnRecorder struct {
	nopLogger
	warns []string
}

func (l *warnRecorder) Warn(msg string, _ ...any) { l.warns = append(l.warns, msg) }

// TestChannelNotRespondingWordsAFeed404AsYouTubeBeingBusy pins owner ruling
// R2-M3 for the alert: a feed 404 is common while YouTube is busy, so the
// alert and the Warn beside it say that, as the Last Error field already
// does, and do not call the channel renamed, banned or misconfigured. Every
// other cause keeps its wording.
//
// Mutants: channelUnhealthyText without the 404 branch (the 404 alert says
// "banned"); the branch taken for every error (the 503 alert loses "banned").
func TestChannelNotRespondingWordsAFeed404AsYouTubeBeingBusy(t *testing.T) {
	feed404 := ""
	for _, h := range []string{"feed http 404: common when YouTube is busy, usually clears by itself; the channel is still checked"} {
		if !monitor.IsFeedBusy404(h) {
			t.Fatalf("IsFeedBusy404(%q) = false: the feed's 404 text moved", h)
		}
		feed404 = h
	}
	for _, other := range []string{"feed http 503", "feed http 404", "decapi 404", ""} {
		if monitor.IsFeedBusy404(other) {
			t.Errorf("IsFeedBusy404(%q) = true", other)
		}
	}

	for _, tc := range []struct {
		name, lastErr string
		want, wantNot []string
	}{
		{"feed 404", feed404,
			[]string{"answered 404 for 20 checks", "busy", "usually clears by itself", "still checked", "check the channel ID"},
			[]string{"banned", "misconfigured", "being missed"}},
		{"another status", "feed http 503",
			[]string{"failed 20 consecutive monitor checks", "renamed, banned, or misconfigured"},
			[]string{"busy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := notificationtest.New()
			log := &warnRecorder{}
			unhealthy, _ := channelHealthNotifiers(rec, log, "youtube", newChannelIncidents())
			unhealthy("UC_x", 20, tc.lastErr)
			calls := rec.ByEvent("channel_unhealthy")
			if len(calls) != 1 || calls[0].Title != "Channel Not Responding" {
				t.Fatalf("alerts = %+v, want one Channel Not Responding", calls)
			}
			desc := calls[0].Description
			for _, w := range tc.want {
				if !strings.Contains(desc, w) {
					t.Errorf("description lacks %q:\n%s", w, desc)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(desc, w) {
					t.Errorf("description says %q:\n%s", w, desc)
				}
			}
			if len(log.warns) != 1 {
				t.Fatalf("Warn lines = %v, want one", log.warns)
			}
			if busy := strings.Contains(log.warns[0], "busy"); busy != (tc.name == "feed 404") {
				t.Errorf("Warn %q: says busy = %v", log.warns[0], busy)
			}
		})
	}
}

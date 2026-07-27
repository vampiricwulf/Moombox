package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
)

type fakeHealthReporter struct{ health []monitor.ChannelHealth }

func (f fakeHealthReporter) Health() []monitor.ChannelHealth { return f.health }

// TestSiblingReachable pins the cross-monitor confirmation that suppresses the
// "Channel Not Responding" false positive: a channel is only alerted on when
// every monitor covering it has lost it. The canonical false positive is
// YouTube serving RSS feed 404s during peak hours while the independent DECAPI
// monitor stays healthy — the healthy DECAPI sibling must veto the alert.
func TestSiblingReachable(t *testing.T) {
	const ch = "UC_target"
	now := time.Unix(1_700_000_000, 0)
	atMs := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }

	healthy := monitor.ChannelHealth{ChannelID: ch, LastCheckedAt: atMs(30 * time.Second), ConsecutiveErrors: 0}
	failing := monitor.ChannelHealth{ChannelID: ch, LastCheckedAt: atMs(30 * time.Second), ConsecutiveErrors: 7, LastError: "boom"}
	stale := monitor.ChannelHealth{ChannelID: ch, LastCheckedAt: atMs(crossMonitorVouchWindow + time.Minute), ConsecutiveErrors: 0}
	otherChannel := monitor.ChannelHealth{ChannelID: "UC_other", LastCheckedAt: atMs(time.Second), ConsecutiveErrors: 0}
	neverChecked := monitor.ChannelHealth{ChannelID: ch, LastCheckedAt: 0, ConsecutiveErrors: 0}

	reporter := func(hs ...monitor.ChannelHealth) channelHealthReporter {
		return fakeHealthReporter{health: hs}
	}

	cases := []struct {
		name     string
		siblings []channelHealthReporter
		want     bool
	}{
		{"no siblings (e.g. Twitch) never vouches", nil, false},
		// The real-world case: feed 404s, DECAPI healthy → suppress.
		{"sibling fresh success vouches", []channelHealthReporter{reporter(healthy)}, true},
		{"sibling also failing does not vouch", []channelHealthReporter{reporter(failing)}, false},
		{"sibling stale success does not vouch", []channelHealthReporter{reporter(stale)}, false},
		{"sibling never checked does not vouch", []channelHealthReporter{reporter(neverChecked)}, false},
		{"sibling tracks only other channels does not vouch", []channelHealthReporter{reporter(otherChannel)}, false},
		{"among two siblings one healthy vouches", []channelHealthReporter{reporter(failing), reporter(healthy)}, true},
		{"nil sibling in slice is skipped", []channelHealthReporter{nil, reporter(healthy)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := siblingReachable(tc.siblings, ch, now); got != tc.want {
				t.Errorf("siblingReachable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJobCreationForDisposition pins spec §10's creator table verbatim —
// the mapping createYouTubeJob applies to every feed/DECAPI video:
//
//	Broadcast (live/upcoming) → Upcoming, queue_priority 0, enqueue now
//	NewVOD                    → Upcoming, queue_priority 0, enqueue now
//	BacklogVOD                → Queued,   queue_priority 1, NO enqueue (scheduler wake)
//
// An unknown disposition fails open to immediate admission — a wrongly
// Queued job would wedge until the scheduler notices it; a wrongly admitted
// job just downloads early.
func TestJobCreationForDisposition(t *testing.T) {
	cases := []struct {
		name           string
		d              monitor.JobDisposition
		wantStatus     database.JobStatus
		wantPriority   int
		wantEnqueueNow bool
	}{
		{"broadcast admits immediately", monitor.DispositionBroadcast, database.StatusUpcoming, 0, true},
		{"new VOD admits immediately", monitor.DispositionNewVOD, database.StatusUpcoming, 0, true},
		{"backlog VOD rests in Queued", monitor.DispositionBacklogVOD, database.StatusQueued, 1, false},
		{"unknown disposition fails open", monitor.JobDisposition(99), database.StatusUpcoming, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, priority, enqueueNow := jobCreationForDisposition(tc.d)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if priority != tc.wantPriority {
				t.Errorf("queue_priority = %d, want %d", priority, tc.wantPriority)
			}
			if enqueueNow != tc.wantEnqueueNow {
				t.Errorf("enqueueNow = %v, want %v", enqueueNow, tc.wantEnqueueNow)
			}
		})
	}
}

// TestOutageAlert pins the "Outage Alert" notification sent on connectivity
// restore (the only connectivity webhook that can actually deliver — a
// "lost" notification has, by definition, no connectivity to ride). Start
// and end render as Discord dynamic timestamps (<t:unix:f> — each viewer's
// local timezone); duration is a plain second-rounded string.
func TestOutageAlert(t *testing.T) {
	start := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	end := start.Add(4*time.Minute + 32*time.Second + 600*time.Millisecond)

	title, _, fields := outageAlert(start, end)

	if title != "Outage Alert" {
		t.Errorf("title = %q, want \"Outage Alert\"", title)
	}
	want := []struct{ name, value string }{
		{"Started", "<t:1785146400:f>"},
		{"Ended", "<t:1785146672:f>"},
		{"Duration", "4m33s"},
	}
	if len(fields) != len(want) {
		t.Fatalf("fields = %d, want %d (%+v)", len(fields), len(want), fields)
	}
	for i, w := range want {
		if fields[i].Name != w.name || fields[i].Value != w.value {
			t.Errorf("field[%d] = %q=%q, want %q=%q", i, fields[i].Name, fields[i].Value, w.name, w.value)
		}
		if !fields[i].Inline {
			t.Errorf("field[%d] %q must be inline — the three cells read as one row", i, w.name)
		}
	}
}

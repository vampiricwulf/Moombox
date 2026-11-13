package main

import (
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

type pushedFrame struct {
	Type    string
	Payload any
}

type pushRecorder struct{ frames []pushedFrame }

func (r *pushRecorder) Broadcast(msgType string, payload any) {
	r.frames = append(r.frames, pushedFrame{msgType, payload})
}

// The push sends only when the failing set, or a failing channel's newest
// error, changed (owner ruling R-H1): a channel under the streak, a channel a
// sibling monitor still reaches and a longer streak with the same error all
// send nothing.
func TestChannelMonitorsPushSendsOnlyAChange(t *testing.T) {
	entry := func(id string, n int, lastErr string) monitor.ChannelHealth {
		return monitor.ChannelHealth{ChannelID: id, LastCheckedAt: 1000, ConsecutiveErrors: n, LastError: lastErr}
	}
	var cur monitor.ChannelMonitors
	rec := &pushRecorder{}
	var toTUI []tui.FailingChannels
	p := &channelMonitorsPush{snapshot: func() monitor.ChannelMonitors { return cur }, hub: rec}
	fn := func(f tui.FailingChannels) { toTUI = append(toTUI, f) }
	p.tui.Store(&fn)

	ytKey := tui.FailingChannelKey("youtube", "UC1")
	twKey := tui.FailingChannelKey("twitch", "login")
	steps := []struct {
		name string
		snap monitor.ChannelMonitors
		want tui.FailingChannels // nil: nothing is pushed
	}{
		{"nothing failing at boot", monitor.ChannelMonitors{}, nil},
		{"two failures", monitor.ChannelMonitors{YouTube: [][]monitor.ChannelHealth{{entry("UC1", 2, "404")}, {}}}, nil},
		{"three on the feed, DECAPI still reaches it", monitor.ChannelMonitors{YouTube: [][]monitor.ChannelHealth{{entry("UC1", 3, "404")}, {entry("UC1", 0, "")}}}, nil},
		{"three on both", monitor.ChannelMonitors{YouTube: [][]monitor.ChannelHealth{{entry("UC1", 3, "404")}, {entry("UC1", 3, "404")}}}, tui.FailingChannels{ytKey: "404"}},
		{"a longer streak, same error", monitor.ChannelMonitors{YouTube: [][]monitor.ChannelHealth{{entry("UC1", 9, "404")}, {entry("UC1", 8, "404")}}}, nil},
		{"a new error", monitor.ChannelMonitors{YouTube: [][]monitor.ChannelHealth{{entry("UC1", 10, "500")}, {entry("UC1", 9, "500")}}}, tui.FailingChannels{ytKey: "500"}},
		{"a Twitch channel joins", monitor.ChannelMonitors{
			YouTube: [][]monitor.ChannelHealth{{entry("UC1", 10, "500")}, {entry("UC1", 9, "500")}},
			Twitch:  [][]monitor.ChannelHealth{{entry("login", 3, "user not found")}},
		}, tui.FailingChannels{ytKey: "500", twKey: "user not found"}},
		{"one success clears the YouTube flag", monitor.ChannelMonitors{
			YouTube: [][]monitor.ChannelHealth{{entry("UC1", 11, "500")}, {entry("UC1", 0, "")}},
			Twitch:  [][]monitor.ChannelHealth{{entry("login", 4, "user not found")}},
		}, tui.FailingChannels{twKey: "user not found"}},
		{"all clear", monitor.ChannelMonitors{Twitch: [][]monitor.ChannelHealth{{entry("login", 0, "")}}}, tui.FailingChannels{}},
		{"still clear", monitor.ChannelMonitors{Twitch: [][]monitor.ChannelHealth{{entry("login", 0, "")}}}, nil},
	}
	for _, st := range steps {
		cur = st.snap
		frames, sent := len(rec.frames), len(toTUI)
		p.check()
		if st.want == nil {
			if len(rec.frames) != frames || len(toTUI) != sent {
				t.Errorf("%s: pushed, want nothing", st.name)
			}
			continue
		}
		if len(rec.frames) != frames+1 || len(toTUI) != sent+1 {
			t.Fatalf("%s: %d frames and %d TUI sends added, want one each", st.name, len(rec.frames)-frames, len(toTUI)-sent)
		}
		last := rec.frames[len(rec.frames)-1]
		if last.Type != "channel_monitors" || !reflect.DeepEqual(last.Payload, st.snap) {
			t.Errorf("%s: frame %q %+v, want channel_monitors with the snapshot", st.name, last.Type, last.Payload)
		}
		if got := toTUI[len(toTUI)-1]; !reflect.DeepEqual(got, st.want) {
			t.Errorf("%s: TUI got %v, want %v", st.name, got, st.want)
		}
		if got := p.failing(); !reflect.DeepEqual(got, st.want) {
			t.Errorf("%s: failing() = %v, want %v", st.name, got, st.want)
		}
	}
}

// A channel ID is only unique within its platform.
func TestFailingChannelsOfKeepsThePlatformsApart(t *testing.T) {
	bad := monitor.ChannelHealth{ChannelID: "same", LastCheckedAt: 1, ConsecutiveErrors: 3, LastError: "gone"}
	got := failingChannelsOf(monitor.ChannelMonitors{Twitch: [][]monitor.ChannelHealth{{bad}}})
	if _, ok := got[tui.FailingChannelKey("youtube", "same")]; ok {
		t.Error("a failing Twitch channel flagged the YouTube channel of the same ID")
	}
	if got[tui.FailingChannelKey("twitch", "same")] != "gone" {
		t.Errorf("got %v", got)
	}
}

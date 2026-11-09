package tui

import (
	"testing"
	"time"
)

// TestMonitorNextCheckTimeDecodesTheSentinels: a monitor reports its next
// check in epoch ms, with -1 = checking and 0 = no channels (nothing
// scheduled). The wiring decoded 0 as time.Unix(0,0) — a real instant in the
// past — so a platform with no channels showed a green "now" timer where the
// Web shows "--".
//
// Mutant: decode 0 through time.UnixMilli — the zero-time assertion fails.
func TestMonitorNextCheckTimeDecodesTheSentinels(t *testing.T) {
	if got := MonitorNextCheckTime(0); !got.IsZero() {
		t.Errorf("0 (no channels) decoded to %v, want the zero time", got)
	}
	if got := MonitorNextCheckTime(MonitorCheckingSentinelMs); !isMonitorChecking(got) {
		t.Errorf("-1 decoded to %v, want the checking sentinel", got)
	}
	want := time.UnixMilli(1_762_700_000_123)
	if got := MonitorNextCheckTime(1_762_700_000_123); !got.Equal(want) {
		t.Errorf("a real schedule decoded to %v, want %v", got, want)
	}
}

// TestCheckTimersMsgClearsATimer: the message used the zero time to mean "this
// field is not part of the update", so nothing could blank a timer once set —
// a platform whose last channel was removed kept its stale countdown. Each
// message now names its monitor, and a zero Next clears that one timer.
func TestCheckTimersMsgClearsATimer(t *testing.T) {
	a := NewApp()
	soon := time.Now().Add(time.Minute)
	a.Update(CheckTimersMsg{Monitor: MonitorFeed, Next: soon})
	a.Update(CheckTimersMsg{Monitor: MonitorTwitch, Next: soon})
	if !a.taskList.NextTwitchCheck.Equal(soon) || !a.taskList.NextFeedCheck.Equal(soon) {
		t.Fatalf("premise lost: timers not set (F %v, T %v)", a.taskList.NextFeedCheck, a.taskList.NextTwitchCheck)
	}
	a.Update(CheckTimersMsg{Monitor: MonitorTwitch, Next: MonitorNextCheckTime(0)})
	if !a.taskList.NextTwitchCheck.IsZero() {
		t.Errorf("Twitch timer = %v after a no-channels update, want cleared", a.taskList.NextTwitchCheck)
	}
	if !a.taskList.NextFeedCheck.Equal(soon) {
		t.Errorf("the Twitch update touched the feed timer: %v", a.taskList.NextFeedCheck)
	}
}

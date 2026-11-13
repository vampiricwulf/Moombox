package monitor

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

// channelFailingParityFile is the table the Go predicate and utils.js
// failingChannels are both run against (CLAUDE.md, State machines rule 2).
const channelFailingParityFile = "../../web/tests/fixtures/channel-failing-parity.json"

func TestFailingChannelsParity(t *testing.T) {
	raw, err := os.ReadFile(channelFailingParityFile)
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []struct {
			Name     string                   `json:"name"`
			Monitors [][]ChannelHealth        `json:"monitors"`
			Want     map[string]ChannelHealth `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("the parity table is empty")
	}
	for _, tc := range table.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := FailingChannels(tc.Monitors)
			if !reflect.DeepEqual(got, tc.Want) {
				t.Errorf("FailingChannels = %+v, want %+v", got, tc.Want)
			}
		})
	}
}

// The streak is the owner's number (R-H1); the alert's is a different one
// and must not move with it.
func TestFailingStreakIsApartFromTheAlertThreshold(t *testing.T) {
	if FailingStreak != 3 {
		t.Errorf("FailingStreak = %d, want 3 (owner ruling R-H1)", FailingStreak)
	}
	if unhealthyThreshold != 20 {
		t.Errorf("unhealthyThreshold = %d, want 20: the Discord alert is unchanged", unhealthyThreshold)
	}
}

// One success clears the flag, through the tracker the monitors write.
func TestChannelFlagClearsOnTheNextSuccess(t *testing.T) {
	h := newHealthTracker()
	for range FailingStreak - 1 {
		h.recordError("a", errors.New("404"))
	}
	if _, ok := FailingChannels([][]ChannelHealth{h.snapshot()})["a"]; ok {
		t.Fatal("flagged one failure short of the streak")
	}
	h.recordError("a", errors.New("404"))
	got, ok := FailingChannels([][]ChannelHealth{h.snapshot()})["a"]
	if !ok || got.LastError != "404" || got.ConsecutiveErrors != FailingStreak {
		t.Fatalf("after %d failures: %+v, flagged %v", FailingStreak, got, ok)
	}
	h.recordSuccess("a")
	if _, ok := FailingChannels([][]ChannelHealth{h.snapshot()})["a"]; ok {
		t.Fatal("still flagged after a success")
	}
}

func TestChannelMonitorsFailingReadsThePlatformsOwnLists(t *testing.T) {
	bad := ChannelHealth{ChannelID: "x", LastCheckedAt: 1, LastError: "gone", ConsecutiveErrors: 3}
	c := ChannelMonitors{YouTube: [][]ChannelHealth{{}, {}}, Twitch: [][]ChannelHealth{{bad}}}
	if _, ok := c.Failing("youtube", "x"); ok {
		t.Error("a Twitch entry flagged a YouTube channel of the same ID")
	}
	if _, ok := c.Failing("", "x"); ok {
		t.Error("an empty platform is YouTube")
	}
	if got, ok := c.Failing("twitch", "x"); !ok || got != bad {
		t.Errorf("Failing(twitch) = %+v, %v", got, ok)
	}
}

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/vampiricwulf/Moombox/internal/config"
)

// The failing-channel flag in the Settings channel list (owner ruling R-H1).
// The rule is monitor.ChannelFailing, applied in cmd/moombox; these tests
// are about the list drawing the set it is handed.

const failingTestErr = "feed returned HTTP 404 for this channel"

// failingRigSet flags the rig's first channel (YouTube) and its second (a
// disabled Twitch channel, which must stay unflagged).
func failingRigSet() FailingChannels {
	return FailingChannels{
		FailingChannelKey("", "UCabcdefghijklmnopqrstuv"): failingTestErr,
		FailingChannelKey("twitch", "streamer_name"):      "user not found",
	}
}

func init() {
	// In the size and shrink-and-back tables with every other Settings state.
	bareSettingsStates["channels, some failing"] = func(t *testing.T, r *compactRig) {
		r.app.Update(ChannelFailingMsg{Failing: failingRigSet()})
		toSection(t, r.app.settings, "Channels")
	}
}

func channelRows(view string) []string {
	return strings.Split(ansi.Strip(view), "\n")
}

func rowWith(rows []string, sub string) string {
	for _, r := range rows {
		if strings.Contains(r, sub) {
			return r
		}
	}
	return ""
}

// Mutants: the tag not drawn; drawn on a disabled channel; drawn on every
// channel; the error dropped; the set not cleared by the next message.
func TestSettingsChannelListFlagsAFailingChannel(t *testing.T) {
	r := bareSettingsRig(t, 120, 40)
	toSection(t, r.app.settings, "Channels")
	if strings.Contains(ansi.Strip(r.app.settings.View()), "failing") {
		t.Fatal("a channel is flagged with nothing failing")
	}

	r.app.Update(ChannelFailingMsg{Failing: failingRigSet()})
	rows := channelRows(r.app.settings.View())
	row := rowWith(rows, "Some Channel Name")
	if !strings.Contains(row, "(failing: "+failingTestErr+")") {
		t.Errorf("the failing channel's row has no flag and error: %q", row)
	}
	if !strings.Contains(row, "UCabcdefghijklmnopqrstuv") {
		t.Errorf("the ID was dropped at 120 columns: %q", row)
	}
	if tw := rowWith(rows, "streamer_name"); strings.Contains(tw, "failing") || !strings.Contains(tw, "(disabled)") {
		t.Errorf("a disabled channel is not monitored and must not be flagged: %q", tw)
	}
	if other := rowWith(rows, "Channel 0"); strings.Contains(other, "failing") {
		t.Errorf("a healthy channel is flagged: %q", other)
	}
	if n := strings.Count(ansi.Strip(r.app.settings.View()), "(failing"); n != 1 {
		t.Errorf("%d rows flagged, want 1", n)
	}

	// The next success: the set arrives without the channel.
	r.app.Update(ChannelFailingMsg{Failing: FailingChannels{}})
	if strings.Contains(ansi.Strip(r.app.settings.View()), "failing") {
		t.Error("the flag stayed after the channel left the set")
	}
}

// At the normal layout's narrowest the tag survives whole and the row is
// still one line no wider than the list.
func TestSettingsChannelFlagFitsTheNarrowestBox(t *testing.T) {
	r := bareSettingsRig(t, 60, 20)
	r.app.Update(ChannelFailingMsg{Failing: failingRigSet()})
	toSection(t, r.app.settings, "Channels")
	plain := ansi.Strip(r.app.settings.View())
	row := rowWith(strings.Split(plain, "\n"), "Some Channel Name")
	if !strings.Contains(row, "(failing") {
		t.Errorf("the tag was cut at 60 columns: %q", row)
	}
	lines := strings.Split(plain, "\n")
	if len(lines) > 20 {
		t.Errorf("the box is %d rows at 60x20", len(lines))
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > 60 {
			t.Errorf("row %d is %d cells wide: %q", i, ansi.StringWidth(l), l)
		}
	}
}

// A newline in a check's error must not break the one-row-per-channel list.
func TestSettingsChannelFlagKeepsOneRowPerChannel(t *testing.T) {
	r := bareSettingsRig(t, 120, 40)
	toSection(t, r.app.settings, "Channels")
	before := len(channelRows(r.app.settings.View()))
	r.app.Update(ChannelFailingMsg{Failing: FailingChannels{
		FailingChannelKey("youtube", "UCabcdefghijklmnopqrstuv"): "line one\nline two",
	}})
	rows := channelRows(r.app.settings.View())
	if len(rows) != before {
		t.Errorf("the view has %d rows, had %d", len(rows), before)
	}
	if row := rowWith(rows, "Some Channel Name"); !strings.Contains(row, "(failing: line one line two)") {
		t.Errorf("row = %q", row)
	}
}

// The bare list: the tag is whole at the floor and the name gives way.
func TestBareChannelListFlagsAFailingChannel(t *testing.T) {
	r := bareSettingsRig(t, 30, 6)
	r.app.Update(ChannelFailingMsg{Failing: failingRigSet()})
	toSection(t, r.app.settings, "Channels")
	view := r.app.settings.View()
	wantExact(t, "bare channels, one failing", view, 30, 6)
	rows := channelRows(view)
	if row := rowWith(rows, "Some Chan"); !strings.HasSuffix(strings.TrimRight(row, " "), "(failing)") {
		t.Errorf("the bare row does not end in the whole tag: %q", row)
	}
	if tw := rowWith(rows, "streamer"); strings.Contains(tw, "failing") {
		t.Errorf("a disabled channel is flagged in the bare list: %q", tw)
	}
}

// Settings derives the set again each time it opens, so a change that came
// while the program was not yet delivering messages is not lost.
func TestSettingsReadsTheFailingSetWhenItOpens(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	r.app.cfg.Channels = []config.ChannelConfig{{ID: "UCabcdefghijklmnopqrstuv", Name: "Some Channel"}}
	calls := 0
	r.app.OnFailingChannels = func() FailingChannels {
		calls++
		return FailingChannels{FailingChannelKey("youtube", "UCabcdefghijklmnopqrstuv"): "gone"}
	}
	r.press("`")
	toSection(t, r.app.settings, "Channels")
	if calls != 1 {
		t.Errorf("OnFailingChannels called %d times on open, want 1", calls)
	}
	if !strings.Contains(ansi.Strip(r.app.settings.View()), "(failing: gone)") {
		t.Errorf("the set read on open is not drawn:\n%s", ansi.Strip(r.app.settings.View()))
	}
}

func TestFailingChannelKeyReadsAnUnknownPlatformAsYouTube(t *testing.T) {
	if FailingChannelKey("", "x") != FailingChannelKey("youtube", "x") {
		t.Error("an empty platform is not YouTube")
	}
	if FailingChannelKey("twitch", "x") == FailingChannelKey("youtube", "x") {
		t.Error("the platforms share a key")
	}
}

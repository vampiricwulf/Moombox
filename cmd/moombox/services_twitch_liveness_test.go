package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// ptrBool is a local helper: ChannelConfig.Enabled is a *bool whose nil means
// enabled.
func ptrBool(b bool) *bool { return &b }

// TestPickTwitchProbeChannelMirrorsTheMonitor pins the one decision the closure
// makes on its own: WHICH login the tier-2 probe targets. The predicate is
// copied from TwitchMonitor.getTwitchChannels (internal/monitor/twitch.go) so
// the probe can only ever ask about a channel the monitor would actually poll.
//
// MUTATION CLOSED (platform): using GetPlatform(), which defaults an empty
// platform to "youtube" and would send a YouTube channel's ID to Twitch GQL.
// MUTATION CLOSED (enabled): dropping the disabled skip — a channel the
// operator turned off would still generate traffic every tick.
// MUTATION CLOSED (order): returning the last match, or any match.
func TestPickTwitchProbeChannelMirrorsTheMonitor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels []config.ChannelConfig
		want     string
	}{
		{"no channels at all", nil, ""},
		{
			"youtube only",
			[]config.ChannelConfig{{ID: "UC-something", Platform: "youtube"}},
			"",
		},
		{
			"platform absent is not twitch",
			[]config.ChannelConfig{{ID: "UC-something"}},
			"",
		},
		{
			"first twitch entry wins",
			[]config.ChannelConfig{
				{ID: "UC-something", Platform: "youtube"},
				{ID: "first_login", Platform: "twitch"},
				{ID: "second_login", Platform: "twitch"},
			},
			"first_login",
		},
		{
			"a disabled twitch channel is skipped",
			[]config.ChannelConfig{
				{ID: "disabled_login", Platform: "twitch", Enabled: ptrBool(false)},
				{ID: "enabled_login", Platform: "twitch"},
			},
			"enabled_login",
		},
		{
			"every twitch channel disabled",
			[]config.ChannelConfig{
				{ID: "disabled_login", Platform: "twitch", Enabled: ptrBool(false)},
			},
			"",
		},
		{
			"an explicit enabled=true is honoured",
			[]config.ChannelConfig{
				{ID: "explicit_login", Platform: "twitch", Enabled: ptrBool(true)},
			},
			"explicit_login",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickTwitchProbeChannel(&config.MoomboxConfig{Channels: tc.channels})
			if got != tc.want {
				t.Errorf("pickTwitchProbeChannel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPickTwitchProbeChannelIsStable: repeated calls on one config must return
// the same login. The verdict is session-level so the choice does not change
// the answer, but a target that wanders between ticks reads as a defect in the
// log — and a future refactor that indexed channels by ID would make it wander
// silently, because Go map iteration is random.
//
// MUTATION CLOSED: any implementation that does not walk cfg.Channels in order.
func TestPickTwitchProbeChannelIsStable(t *testing.T) {
	cfg := &config.MoomboxConfig{Channels: []config.ChannelConfig{
		{ID: "aaa_login", Platform: "twitch"},
		{ID: "bbb_login", Platform: "twitch"},
		{ID: "ccc_login", Platform: "twitch"},
	}}
	for range 20 {
		if got := pickTwitchProbeChannel(cfg); got != "aaa_login" {
			t.Fatalf("pickTwitchProbeChannel = %q, want the first configured login %q on every call", got, "aaa_login")
		}
	}
}

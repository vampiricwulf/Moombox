package worker

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// A Twitch job's quality preference is its quality_preference, the column
// every row carries: the quality the job was created to record, written by
// whoever creates the row — the Twitch monitor (cmd/moombox), the Web add
// (internal/web/routes) and `moombox add` (cmd/moombox) — and never
// overwritten. Every Twitch variant selection reads it and nothing else
// (selectTwitchVariant at the capture start, live and VOD, and
// newTwitchVariantInfo for every re-selection during the capture).
//
// twitch_quality used to carry it too. It was set to the preference at
// creation, overwritten with the picked variant's name at the stream start
// and read back as the preference by the next selection, so a job resumed
// after a restart re-selected by the name of what it last recorded
// ("chunked", "720p60") — and a quality split, which never rewrote it, left it
// naming a variant the job was no longer recording. twitch_quality is now that
// variant alone.

// TwitchJobQualityPreference is the quality_preference a new Twitch job
// records for the preference it was created with: that preference, or "best"
// when none was named, so every Twitch row created now names one. An empty
// value — which a row created before this rule can hold — selects as "best"
// all the same (twitch.SelectBestVariant).
func TwitchJobQualityPreference(pref string) string {
	if pref == "" {
		return "best"
	}
	return pref
}

// TwitchChannelQualityPreference is the preference a job on a Twitch channel
// records when nothing more specific named one: the channel's
// quality_preference while the channel is configured (enabled or not), else
// "best". login is matched without regard to case, as the monitor's own
// channel IDs are.
func TwitchChannelQualityPreference(channels []config.ChannelConfig, login string) string {
	if login != "" {
		for i := range channels {
			if channels[i].GetPlatform() == "twitch" && strings.EqualFold(channels[i].ID, login) {
				return TwitchJobQualityPreference(channels[i].QualityPreference)
			}
		}
	}
	return "best"
}

package worker

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// twitch_quality_preference (owner decision D-T9) is the quality a Twitch job
// was created to record. It is written once, by whoever creates the row — the
// Twitch monitor (cmd/moombox), the Web add (internal/web/routes) and
// `moombox add` (cmd/moombox) — and never again: database.fieldToColumn has no
// entry for it, so no UpdateJobFields call can reach it. Every Twitch variant
// selection is handed this value and nothing else (selectTwitchVariant at the
// capture start, newTwitchVariantInfo for every re-selection during it).
//
// twitch_quality used to carry it. It was set to the preference at creation,
// overwritten with the picked variant's name at the stream start and read back
// as the preference by the next selection, so a job resumed after a restart
// re-selected by the name of what it last recorded ("chunked", "720p60") —
// and a quality split, which never rewrote it, left it naming a variant the
// job was no longer recording. twitch_quality is now that variant alone.

// TwitchJobQualityPreference is the twitch_quality_preference a new Twitch job
// records for the preference it was created with: that preference, or "best"
// when none was named. Never empty, so an empty column marks a row that
// predates the column — the rows BackfillTwitchQualityPreferences fills.
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

// BackfillTwitchQualityPreferences gives every Twitch row that predates schema
// v21 the preference it was created with, and reports how many it wrote. Run
// once per start, before the worker reads a row; a no-op after the first,
// since every row created since records one.
//
// The rule, per row, is the one owner decision D-T9 states: its channel's
// current quality_preference while the channel is still configured, else
// "best". The row's own quality_preference is not consulted, though the
// monitor and the Web add have written their creation preference there all
// along: the decision names the channel's current setting, so a row created
// while its channel was set to 720p re-selects at the 1080p60 the channel is
// set to now, as the next broadcast's row will. A VOD row is never matched to
// a channel: its URL names no login, and the display name it carries is not
// one.
func BackfillTwitchQualityPreferences(db *database.Database, channels []config.ChannelConfig) (int, error) {
	return db.BackfillTwitchQualityPreference(func(j *database.Job) string {
		if strings.Contains(j.URL, "twitch.tv/videos/") {
			return "best"
		}
		return TwitchChannelQualityPreference(channels, extractTwitchLoginFromJob(j))
	})
}

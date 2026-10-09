package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestNewTwitchStreamJobRecordsThePreferenceOnly is the monitor's half: the
// row records the channel's quality_preference as its own ("best" when the
// channel names none) — the one preference every Twitch selection reads — and
// leaves twitch_quality, the variant being recorded, empty until a capture
// starts.
//
// Mutants: twitch_quality set to the preference again (the pre-D-T9 row);
// quality_preference left unset; the channel's preference written raw (an
// unset one stores "").
func TestNewTwitchStreamJobRecordsThePreferenceOnly(t *testing.T) {
	info := &twitch.TwitchStreamInfo{StreamID: "42", ChannelLogin: "streamer", ChannelDisplayName: "Streamer", Title: "hi"}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	job := newTwitchStreamJob(info, &config.ChannelConfig{ID: "streamer", Platform: "twitch", QualityPreference: "720p60"}, "/out", now)
	if job.QualityPreference != "720p60" {
		t.Errorf("quality_preference = %q, want the channel's 720p60", job.QualityPreference)
	}
	if job.TwitchQuality != "" {
		t.Errorf("twitch_quality = %q at creation, want empty — nothing is recording yet", job.TwitchQuality)
	}
	if job.ID != "tw_42" || job.CreatedAt != "2026-10-08T12:00:00Z" {
		t.Errorf("job = %s created %s, want tw_42 at the given time", job.ID, job.CreatedAt)
	}

	job = newTwitchStreamJob(info, &config.ChannelConfig{ID: "streamer", Platform: "twitch"}, "/out", now)
	if job.QualityPreference != "best" {
		t.Errorf("quality_preference = %q for a channel with no preference, want best", job.QualityPreference)
	}
}

// TestNewCLITwitchJob is `moombox add`'s row: a live channel's job and a VOD's
// both record a quality_preference — the configured channel's for a login the
// config holds, "best" otherwise — and leave twitch_quality empty. The row
// used to record no preference at all.
//
// Mutants: quality_preference left out of the row (both rows store "");
// twitch_quality set to the preference.
func TestNewCLITwitchJob(t *testing.T) {
	channels := []config.ChannelConfig{{ID: "streamer", Platform: "twitch", QualityPreference: "480p"}}
	for _, tc := range []struct {
		target        *utils.TwitchTarget
		id, url, pref string
		channel       string
	}{
		{&utils.TwitchTarget{Type: utils.TwitchChannel, Value: "streamer"}, "streamer", "https://www.twitch.tv/streamer", "480p", "streamer"},
		{&utils.TwitchTarget{Type: utils.TwitchVOD, Value: "123"}, "tw_v123", "https://www.twitch.tv/videos/123", "best", "Manual"},
	} {
		job := newCLITwitchJob(channels, tc.target, "2026-10-09T00:00:00Z")
		if job.ID != tc.id || job.VideoID != tc.id || job.URL != tc.url || job.ChannelName != tc.channel {
			t.Errorf("%+v: row = %s / %s / %s / %s, want %s / %s / %s / %s", *tc.target,
				job.ID, job.VideoID, job.URL, job.ChannelName, tc.id, tc.id, tc.url, tc.channel)
		}
		if job.QualityPreference != tc.pref {
			t.Errorf("%+v: quality_preference = %q, want %q", *tc.target, job.QualityPreference, tc.pref)
		}
		if job.TwitchQuality != "" {
			t.Errorf("%+v: twitch_quality = %q at creation, want empty", *tc.target, job.TwitchQuality)
		}
		if job.Platform != "twitch" || !job.ManuallyAdded || job.Status != database.StatusUpcoming {
			t.Errorf("%+v: row = %s / manual %v / %s, want a manually added Upcoming Twitch row", *tc.target,
				job.Platform, job.ManuallyAdded, job.Status)
		}
	}
}

// TestCLITwitchQualityPreference is the rule behind it: the command takes no
// quality flag, so a live channel the config holds records that channel's
// preference — what the monitor records for the same broadcast — and a VOD or
// an unconfigured channel records "best".
//
// Mutants: the VOD guard dropped (the VOD's numeric ID is looked up as a
// login, and the "123" channel's 160p comes back); the channel lookup dropped
// (the configured channel records "best").
func TestCLITwitchQualityPreference(t *testing.T) {
	channels := []config.ChannelConfig{
		{ID: "streamer", Platform: "twitch", QualityPreference: "480p"},
		{ID: "123", Platform: "twitch", QualityPreference: "160p"},
	}
	for _, tc := range []struct {
		target *utils.TwitchTarget
		want   string
	}{
		{&utils.TwitchTarget{Type: utils.TwitchChannel, Value: "streamer"}, "480p"},
		{&utils.TwitchTarget{Type: utils.TwitchChannel, Value: "someoneelse"}, "best"},
		{&utils.TwitchTarget{Type: utils.TwitchVOD, Value: "123"}, "best"},
	} {
		if got := cliTwitchQualityPreference(channels, tc.target); got != tc.want {
			t.Errorf("cliTwitchQualityPreference(%+v) = %q, want %q", *tc.target, got, tc.want)
		}
	}
}

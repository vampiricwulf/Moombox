package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestNewTwitchStreamJobRecordsThePreferenceOnly is the monitor's half of
// D-T9: the row records the channel's quality_preference as its
// twitch_quality_preference ("best" when the channel names none), and leaves
// twitch_quality — the variant being recorded — empty until a capture starts.
//
// Mutants: twitch_quality set to the preference again (the pre-D-T9 row);
// twitch_quality_preference left unset (the row reads as one that predates the
// column); the channel's preference written raw (an unset one stores "").
func TestNewTwitchStreamJobRecordsThePreferenceOnly(t *testing.T) {
	info := &twitch.TwitchStreamInfo{StreamID: "42", ChannelLogin: "streamer", ChannelDisplayName: "Streamer", Title: "hi"}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	job := newTwitchStreamJob(info, &config.ChannelConfig{ID: "streamer", Platform: "twitch", QualityPreference: "720p60"}, "/out", now)
	if job.TwitchQualityPreference != "720p60" {
		t.Errorf("twitch_quality_preference = %q, want the channel's 720p60", job.TwitchQualityPreference)
	}
	if job.TwitchQuality != "" {
		t.Errorf("twitch_quality = %q at creation, want empty — nothing is recording yet", job.TwitchQuality)
	}
	if job.ID != "tw_42" || job.CreatedAt != "2026-10-08T12:00:00Z" {
		t.Errorf("job = %s created %s, want tw_42 at the given time", job.ID, job.CreatedAt)
	}

	job = newTwitchStreamJob(info, &config.ChannelConfig{ID: "streamer", Platform: "twitch"}, "/out", now)
	if job.TwitchQualityPreference != "best" {
		t.Errorf("twitch_quality_preference = %q for a channel with no preference, want best", job.TwitchQualityPreference)
	}
}

// TestCLITwitchQualityPreference is `moombox add`'s half: the command takes
// no quality flag, so a live channel the config holds records that channel's
// preference — what the monitor records for the same broadcast — and a VOD or
// an unconfigured channel records "best". The row used to record nothing.
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

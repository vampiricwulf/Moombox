package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestNewTwitchClipJobIsAPacedBacklogRow pins the row the clip sweep creates
// (owner decision D26 follow-up 2): the clip kind's VOD-style row, Queued at
// queue_priority 1 under the channel's ID — the backlog VOD's shape, which is
// what the archive-slots scheduler admits (QueuedChannels / NextQueuedJobs key
// on channel_id) — with the clip's published time and the channel's quality
// preference.
//
// Mutants: Upcoming instead of Queued (admitted at once, unpaced); the
// channel_id left nil (the scheduler never admits it); priority 0.
func TestNewTwitchClipJobIsAPacedBacklogRow(t *testing.T) {
	ch := &config.ChannelConfig{ID: "vanillatv", Name: "VanillaTV", Platform: "twitch", ArchiveClips: true, QualityPreference: "720p"}
	card := twitch.TwitchClipCard{Slug: "FaintLightGullWholeWheat", Title: "Big play", CreatedAt: "2026-10-30T10:00:00Z",
		ThumbnailURL: "https://clips-media-assets2.twitch.tv/x.jpg"}
	job := newTwitchClipJob(card, ch, "/out", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))

	if job.ID != "tw_clip_FaintLightGullWholeWheat" || job.VideoID != job.ID || job.Platform != "twitch" || !job.IsVod {
		t.Errorf("identity = %s / %s / %s / isVod %v", job.ID, job.VideoID, job.Platform, job.IsVod)
	}
	if job.Status != database.StatusQueued || job.QueuePriority != 1 || job.ChannelID == nil || *job.ChannelID != "vanillatv" {
		t.Errorf("pacing = %s / priority %d / channel %v, want Queued / 1 / vanillatv", job.Status, job.QueuePriority, job.ChannelID)
	}
	if job.URL != "https://www.twitch.tv/vanillatv/clip/FaintLightGullWholeWheat" || job.Title != "VanillaTV — Big play" ||
		job.StreamStartTime != "2026-10-30T10:00:00Z" || job.QualityPreference != "720p" || job.OutputDirectory != "/out" {
		t.Errorf("row = %+v", job)
	}
}

// TestChannelArchivesClipsReadsTheLiveConfig: the sweep's job creation asks
// the live config, so a channel opted out, disabled or removed since the
// sweep began creates nothing.
//
// Mutant: the ArchiveClips term dropped (the opted-out channel creates jobs).
func TestChannelArchivesClipsReadsTheLiveConfig(t *testing.T) {
	off := false
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{
		{ID: "on", Platform: "twitch", ArchiveClips: true},
		{ID: "optedout", Platform: "twitch"},
		{ID: "disabled", Platform: "twitch", ArchiveClips: true, Enabled: &off},
	}
	store := config.NewStore(cfg, "")
	for id, want := range map[string]bool{"on": true, "optedout": false, "disabled": false, "gone": false} {
		if got := channelArchivesClips(store, id); got != want {
			t.Errorf("channelArchivesClips(%s) = %v, want %v", id, got, want)
		}
	}
}

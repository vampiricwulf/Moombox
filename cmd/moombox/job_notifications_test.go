package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestCLIAddedFactsGainsAThumbnailAndChannel is audit row #23/#24: the CLI add
// sent a bare id with no channel, no thumbnail and no author, while the web
// add sent all three for the same action.
//
// Mutants this kill:
//   - leaving the YouTube thumbnail empty: the embed loses the image the web
//     add has always had, and nothing else changes.
//   - setting Title from the row's "Manual Add" placeholder: the description
//     reads "Manually added: Manual Add".
//   - naming a Twitch VOD's channel: a VOD add knows a video id, not a login.
func TestCLIAddedFactsGainsAThumbnailAndChannel(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		f := cliAddedFacts("youtube", "abc123", "https://www.youtube.com/watch?v=abc123", "")
		if f.ThumbnailURL != youtubeThumbnailURL("abc123") {
			t.Errorf("ThumbnailURL = %q, want the maxres thumbnail", f.ThumbnailURL)
		}
		if f.Title != "" {
			t.Errorf("Title = %q, want empty so the builder falls back to the id", f.Title)
		}
		if f.Channel != "" || f.ChannelURL != "" {
			t.Errorf("channel = %q/%q — a CLI YouTube add knows no channel", f.Channel, f.ChannelURL)
		}
	})

	t.Run("twitch live add names the channel", func(t *testing.T) {
		f := cliAddedFacts("twitch", "streamer", "https://www.twitch.tv/streamer", "streamer")
		if f.Channel != "streamer" {
			t.Errorf("Channel = %q, want the login", f.Channel)
		}
		if f.ChannelURL != "https://www.twitch.tv/streamer" {
			t.Errorf("ChannelURL = %q", f.ChannelURL)
		}
	})

	t.Run("twitch vod add names no channel", func(t *testing.T) {
		f := cliAddedFacts("twitch", "tw_v123", "https://www.twitch.tv/videos/123", "")
		if f.Channel != "" || f.ChannelURL != "" {
			t.Errorf("channel = %q/%q — a VOD add knows a video id, not a login", f.Channel, f.ChannelURL)
		}
	})
}

// TestCLIAddedFactsProduceAJobAddedEmbed runs the composed path a recorder can
// observe, which the addVideo function itself cannot offer (it loads config,
// opens the database and calls os.Exit).
func TestCLIAddedFactsProduceAJobAddedEmbed(t *testing.T) {
	rec := notificationtest.New()
	rec.Send(notifications.JobAdded(cliAddedFacts("youtube", "abc123", "https://www.youtube.com/watch?v=abc123", "")))

	calls := rec.ByEvent("added")
	if len(calls) != 1 {
		t.Fatalf("recorded %d added calls, want 1", len(calls))
	}
	c := calls[0]
	if c.Title != "Job Added" {
		t.Errorf("title = %q, want %q", c.Title, "Job Added")
	}
	if c.Description != "Manually added: abc123" {
		t.Errorf("description = %q", c.Description)
	}
	if c.Opts.Thumbnail == "" {
		t.Error("the CLI add still sends no thumbnail")
	}
	if c.Opts.JobID != "abc123" || c.Opts.Platform != "youtube" {
		t.Errorf("opts = %+v, want the job id and platform", c.Opts)
	}
}

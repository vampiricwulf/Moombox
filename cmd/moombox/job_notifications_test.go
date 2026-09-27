package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
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

// TestNotifyStreamFoundIsOneEmbedForBothMonitors is audit C4. Both discovery
// sites lived inside closures in wireMonitorCallbacks and had no test at all;
// this seam is what makes them assertable.
//
// Mutants this kill:
//   - keeping "Twitch Stream Found" as a second title.
//   - dropping the Twitch channel page, which is the only channel link a
//     Twitch row can produce (channel_id is NULL for every Twitch job).
//   - sending the category for YouTube, where it is always empty.
func TestNotifyStreamFoundIsOneEmbedForBothMonitors(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		rec := notificationtest.New()
		chID := "UC_abc"
		notifyStreamFound(rec, &database.Job{
			ID: "vid1", VideoID: "vid1", Platform: "youtube", Title: "A Stream",
			ChannelName: "A Channel", ChannelID: &chID,
			URL: "https://www.youtube.com/watch?v=vid1", ThumbnailURL: "https://i.ytimg.example/t.jpg",
		}, "", "")

		calls := rec.ByEvent("found")
		if len(calls) != 1 {
			t.Fatalf("recorded %d found calls, want 1", len(calls))
		}
		if calls[0].Title != "Stream Found" {
			t.Errorf("title = %q", calls[0].Title)
		}
		if calls[0].Description != "Found matching stream: A Stream" {
			t.Errorf("description = %q", calls[0].Description)
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.URL != "https://www.youtube.com/channel/UC_abc" {
			t.Error("the YouTube find lost its channel-page link")
		}
	})

	t.Run("twitch", func(t *testing.T) {
		rec := notificationtest.New()
		notifyStreamFound(rec, &database.Job{
			ID: "tw_9", VideoID: "9", Platform: "twitch", Title: "Streamer — live",
			ChannelName: "Streamer", ChannelAvatarURL: "https://static.example/p.png",
			URL: "https://twitch.tv/streamer", ThumbnailURL: "https://static.example/prev.jpg",
		}, "https://twitch.tv/streamer", "Just Chatting")

		calls := rec.ByEvent("found")
		if len(calls) != 1 {
			t.Fatalf("recorded %d found calls, want 1", len(calls))
		}
		if calls[0].Title != "Stream Found" {
			t.Errorf("title = %q — the Twitch find still names the platform", calls[0].Title)
		}
		if calls[0].Description != "Live: Streamer — live" {
			t.Errorf("description = %q", calls[0].Description)
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.URL != "https://twitch.tv/streamer" {
			t.Error("the Twitch find has no channel-page link")
		}
	})
}

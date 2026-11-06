package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TwitchBacklog is the per-channel Twitch VOD backlog (owner decision D18,
// `twitch_vod_backlog`, default off): on a slow cadence it lists each opted-in
// channel's newest videos and queues every one no job has had yet as a backlog
// VOD — Queued at queue_priority 1, which the archive-slots scheduler admits
// archive_slots at a time, as a YouTube channel's backfill is. Beyond upstream:
// yt-dlp lists a channel's videos only when asked (TwitchVideosIE, twitch.py
// b77c9304e); with no monitor of its own, a broadcast Moombox missed, and
// every highlight and upload, would otherwise expire from Twitch unarchived.
//
// Skipped, besides a VOD already in history (a job was created for it once)
// and one whose title the channel's terms do not match (MatchesTerms, as for
// every monitor creator):
// the archive of a broadcast a live job of the channel captured — its
// publishedAt within twitchBacklogCapturedWindow of that job's
// stream_start_time — since the live capture is that broadcast's archive, and
// gap-fill (owner decision D17) is what recovers what it missed.
type TwitchBacklog struct {
	configStore *config.Store
	db          *database.Database
	logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// ListVideos reads a channel's newest videos (production: one page of
	// twitch.API.ListChannelVideos, every type, newest first).
	ListVideos func(ctx context.Context, login string) ([]twitch.TwitchVideo, error)
	// OnQueued is told how many rows a cycle created (production: the
	// worker's Scheduler().Wake), never with zero.
	OnQueued func(n int)
	// IsOnline gates a cycle (nil: always online).
	IsOnline func() bool
}

// twitchBacklogInterval is the backlog's cadence: Twitch keeps a past
// broadcast 7 to 60 days, so an hour costs nothing in coverage and one GQL
// request per opted-in channel per hour.
const twitchBacklogInterval = time.Hour

// twitchBacklogFirstDelay is how long after start the first cycle runs: past
// the boot's own burst of monitor and worker requests.
const twitchBacklogFirstDelay = 2 * time.Minute

// twitchBacklogCapturedWindow is how far a video's publishedAt may sit from a
// live job's stream_start_time and still be that broadcast's archive. Twitch
// publishes a broadcast's archive as the stream starts; a highlight or upload
// published within minutes of another broadcast's start is rare enough to
// cost nothing when it is wrongly taken for the archive.
const twitchBacklogCapturedWindow = 10 * time.Minute

// NewTwitchBacklog builds the backlog over tw's GQL client.
func NewTwitchBacklog(store *config.Store, db *database.Database, tw *twitch.Service, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *TwitchBacklog {
	b := &TwitchBacklog{configStore: store, db: db, logger: logger}
	if tw != nil {
		b.ListVideos = func(ctx context.Context, login string) ([]twitch.TwitchVideo, error) {
			return tw.API.ListChannelVideos(ctx, login, "", "time", 1, tw.Auth.GetAuthToken())
		}
	}
	return b
}

// Start runs the backlog's cycles on its own goroutine until ctx ends: the
// first twitchBacklogFirstDelay after start, then every
// twitchBacklogInterval. A cycle with no opted-in channel sends nothing.
func (b *TwitchBacklog) Start(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("panic in the Twitch VOD backlog", "panic", fmt.Sprint(r))
			}
		}()
		t := time.NewTimer(twitchBacklogFirstDelay)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			b.RunCycle(ctx, time.Now())
			t.Reset(twitchBacklogInterval)
		}
	}()
}

// RunCycle lists every opted-in, enabled Twitch channel once and queues what
// it finds. One channel's failure is logged and the next is tried.
func (b *TwitchBacklog) RunCycle(ctx context.Context, now time.Time) {
	if b.ListVideos == nil || (b.IsOnline != nil && !b.IsOnline()) {
		return
	}
	var channels []config.ChannelConfig
	var opted []config.ChannelConfig
	b.configStore.Read(func(c *config.MoomboxConfig) {
		channels = append([]config.ChannelConfig(nil), c.Channels...)
	})
	for _, ch := range channels {
		if ch.GetPlatform() == "twitch" && ch.IsEnabled() && ch.TwitchVodBacklog {
			opted = append(opted, ch)
		}
	}
	if len(opted) == 0 {
		return
	}
	starts := b.capturedStarts()
	total := 0
	for _, ch := range opted {
		if ctx.Err() != nil {
			return
		}
		login := strings.ToLower(ch.ID)
		videos, err := b.ListVideos(ctx, login)
		if err != nil {
			b.logger.Warn("twitch VOD backlog: could not list the channel's videos; trying again next cycle",
				"channel", ch.ID, "err", err)
			continue
		}
		var fresh []twitch.TwitchVideo
		for _, v := range videos {
			// The channel's title terms gate the backlog as they gate its
			// live broadcasts (TwitchMonitor) and a YouTube channel's
			// backfill (archive.go): a broadcast the monitor deliberately
			// skipped is not archived through the back door.
			if !capturedLive(v, starts[login]) && MatchesTerms(v.Title, &ch) {
				fresh = append(fresh, v)
			}
		}
		res := worker.QueueTwitchVideos(b.db, fresh, worker.TwitchListingOptions{
			Channels: channels, Login: login, SkipHistory: true,
		}, now)
		if n := len(res.Added); n > 0 {
			b.logger.Info("twitch VOD backlog: queued the channel's new videos", "channel", ch.ID, "queued", n)
			total += n
		}
	}
	if total > 0 && b.OnQueued != nil {
		b.OnQueued(total)
	}
}

// capturedStarts maps each Twitch login to the stream_start_times of its live
// jobs (any status: a capture that ended in Error still recorded the
// broadcast). The login is read off the job's URL, the one place every live
// Twitch row names its channel — the monitor's, a manual add's and a
// recovered one's alike.
func (b *TwitchBacklog) capturedStarts() map[string][]time.Time {
	out := map[string][]time.Time{}
	jobs, err := b.db.GetAllJobs()
	if err != nil {
		return out
	}
	for _, j := range jobs {
		if j.Platform != "twitch" || j.IsVod || j.StreamStartTime == "" {
			continue
		}
		start, err := time.Parse(time.RFC3339, j.StreamStartTime)
		if err != nil {
			continue
		}
		login := twitchLoginFromURL(j.URL)
		if login == "" {
			continue
		}
		out[login] = append(out[login], start)
	}
	return out
}

// twitchLoginFromURL is the login a live Twitch job's URL names
// ("https://twitch.tv/<login>", "https://www.twitch.tv/<login>"), "" for any
// other shape.
func twitchLoginFromURL(u string) string {
	for _, prefix := range []string{"https://www.twitch.tv/", "https://twitch.tv/", "http://www.twitch.tv/", "http://twitch.tv/"} {
		if rest, ok := strings.CutPrefix(u, prefix); ok {
			login, _, _ := strings.Cut(rest, "/")
			return strings.ToLower(login)
		}
	}
	return ""
}

// capturedLive reports whether v was published within
// twitchBacklogCapturedWindow of one of starts — the archive of a broadcast a
// live job captured.
func capturedLive(v twitch.TwitchVideo, starts []time.Time) bool {
	published := v.Published()
	if published.IsZero() {
		return false
	}
	for _, s := range starts {
		if d := published.Sub(s); d > -twitchBacklogCapturedWindow && d < twitchBacklogCapturedWindow {
			return true
		}
	}
	return false
}

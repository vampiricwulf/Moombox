package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
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
// gap-fill (owner decision D17) is what recovers what it missed. A live job
// counts as a capture only when it recorded something or still may
// (capturedLiveJob, owner decision PR-M4): a capture that failed or parked
// before its first segment holds nothing of the broadcast, and the VOD,
// then its only copy, is queued.
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

	// deferLogged is the VODs whose skip for a live capture was logged this
	// process: the skip is decided again every cycle (a skipped VOD is not
	// recorded in history), and one Info line per VOD per hour said nothing
	// the first did not. Touched only by RunCycle, on the backlog's one
	// goroutine.
	deferLogged map[string]bool

	// wake carries CheckNow's kicks to the loop; one slot, so kicks coalesce.
	// nil on a TwitchBacklog built without NewTwitchBacklog: CheckNow is then
	// a no-op.
	wake chan struct{}
	// Zero means the constant of the same name; a test shortens them.
	interval, firstDelay, kickGap time.Duration
}

// twitchBacklogInterval is the backlog's cadence: Twitch keeps a past
// broadcast 7 to 60 days, so an hour costs nothing in coverage and one GQL
// request per opted-in channel per hour.
const twitchBacklogInterval = time.Hour

// twitchBacklogFirstDelay is how long after start the first cycle runs: past
// the boot's own burst of monitor and worker requests.
const twitchBacklogFirstDelay = 2 * time.Minute

// twitchBacklogKickGap is the least time between a cycle and a kicked one.
// Every channel save kicks the backlog (a reorder too), and each cycle costs
// one GQL request per opted-in channel: a burst of saves is one cycle now and
// one more when the gap has passed, never a burst of requests. A kick inside
// the gap is kept and run at its end.
const twitchBacklogKickGap = 5 * time.Minute

// twitchBacklogCapturedWindow is how far a video's publishedAt may sit from a
// live job's stream_start_time and still be that broadcast's archive. Twitch
// publishes a broadcast's archive as the stream starts; a highlight or upload
// published within minutes of another broadcast's start is rare enough to
// cost nothing when it is wrongly taken for the archive. The gap-fill's
// archive lookup asks the same question with the same window
// (twitch.ArchiveVodStartTolerance, owner decision PR-M3).
const twitchBacklogCapturedWindow = twitch.ArchiveVodStartTolerance

// NewTwitchBacklog builds the backlog over tw's GQL client.
func NewTwitchBacklog(store *config.Store, db *database.Database, tw *twitch.Service, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *TwitchBacklog {
	b := &TwitchBacklog{configStore: store, db: db, logger: logger, wake: make(chan struct{}, 1)}
	if tw != nil {
		b.ListVideos = func(ctx context.Context, login string) ([]twitch.TwitchVideo, error) {
			return tw.API.ListChannelVideos(ctx, login, "", "time", 1, tw.Auth.GetAuthToken())
		}
	}
	return b
}

// CheckNow asks for a cycle now rather than at the next hourly tick: a channel
// was saved (twitch_vod_backlog may just have been turned on) or connectivity
// came back. It never blocks and never runs the cycle on the caller's
// goroutine. Kicks coalesce, and the loop spaces kicked cycles by
// twitchBacklogKickGap.
func (b *TwitchBacklog) CheckNow() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Start runs the backlog's cycles on its own goroutine until ctx ends: the
// first twitchBacklogFirstDelay after start, then every
// twitchBacklogInterval. A cycle with no opted-in channel sends nothing.
//
// CheckNow adds a cycle between ticks without moving the hourly tick. A kick
// before the first cycle is dropped (that cycle is at most two minutes away,
// and the boot's own saves should not pull it into the boot burst); one
// inside twitchBacklogKickGap of the last cycle runs when the gap has passed.
// A cycle that falls due while offline, by tick or by kick, is owed: the
// connectivity handler's kick on the way back runs it at once, gap or not,
// rather than an hour later.
func (b *TwitchBacklog) Start(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("panic in the Twitch VOD backlog", "panic", fmt.Sprint(r))
			}
		}()
		interval := orDuration(b.interval, twitchBacklogInterval)
		gap := orDuration(b.kickGap, twitchBacklogKickGap)
		t := time.NewTimer(orDuration(b.firstDelay, twitchBacklogFirstDelay))
		defer t.Stop()
		var (
			started bool             // the first scheduled cycle has come due
			owed    bool             // a cycle came due while offline
			last    time.Time        // when the last cycle ran
			gapT    *time.Timer      // armed while a kick waits out the gap
			gapC    <-chan time.Time // gapT.C, nil while not armed
		)
		defer func() {
			if gapT != nil {
				gapT.Stop()
			}
		}()
		run := func() {
			if b.IsOnline != nil && !b.IsOnline() {
				owed = true
				return
			}
			owed = false
			last = time.Now()
			b.RunCycle(ctx, last)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				started = true
				run()
				t.Reset(interval)
			case <-b.wake:
				if !started {
					continue
				}
				if wait := gap - time.Since(last); owed || wait <= 0 {
					run()
				} else if gapC == nil {
					gapT = time.NewTimer(wait)
					gapC = gapT.C
				}
			case <-gapC:
				gapC = nil
				run()
			}
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
			if !MatchesTerms(v.Title, &ch) {
				continue
			}
			if by := capturedLive(v, starts[login]); by != "" {
				if !b.deferLogged[v.ID] {
					if b.deferLogged == nil {
						b.deferLogged = map[string]bool{}
					}
					b.deferLogged[v.ID] = true
					b.logger.Info("twitch VOD backlog: not queuing a broadcast's archive; its live capture holds it",
						"channel", ch.ID, "vod", v.ID, "jobID", by)
				}
				continue
			}
			fresh = append(fresh, v)
		}
		res := worker.QueueTwitchVideos(b.db, fresh, worker.TwitchListingOptions{
			Channels: channels, Login: login, SkipHistory: true, Logger: b.logger,
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

// capturedBroadcast is one live capture's broadcast: its stream_start_time
// and the job that captured it.
type capturedBroadcast struct {
	start time.Time
	jobID string
}

// capturedStarts maps each Twitch login to the broadcasts its live jobs
// captured (capturedLiveJob). The login is read off the job's URL, the one
// place every live Twitch row names its channel — the monitor's, a manual
// add's and a recovered one's alike.
func (b *TwitchBacklog) capturedStarts() map[string][]capturedBroadcast {
	out := map[string][]capturedBroadcast{}
	jobs, err := b.db.GetAllJobs()
	if err != nil {
		return out
	}
	stagingBase := ""
	if b.configStore != nil {
		b.configStore.Read(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
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
		if login == "" || !b.capturedLiveJob(j, stagingBase) {
			continue
		}
		out[login] = append(out[login], capturedBroadcast{start: start, jobID: j.ID})
	}
	return out
}

// capturedLiveJob reports whether a live Twitch job stands for its
// broadcast's archive, so the backlog leaves the VOD to it (owner decision
// PR-M4): it holds footage — recorded parts, an archive file, or staged
// media a Resume or a Mux can still finalize — or it was Cancelled (the
// operator chose to stop, FQ-2), or it already has a gap-fill plan, or it is
// still capturing (Upcoming, Live, Downloading or Muxing: a capture whose
// first segment is moments away is not a capture that recorded nothing, and
// queuing the VOD beside it would record the broadcast twice), or it is an
// Error row the Twitch monitor will re-initialise when the same broadcast
// is found live again (jobstate.TwitchAutoRetryPending —
// a twitch_offline or twitch_unreachable stop before the first
// segment, retries left): that row is a capture that is about to resume,
// and the VOD queued in the gap would record the broadcast a second time
// beside it. Its VOD is queued only once no automatic restart is left.
//
// What is left is a capture that failed or parked — Error, COOKIES? —
// before it recorded anything. stream_start_time is written before the
// first segment (the monitor's row, the processor's StartedAt), and every
// such row used to count: a non-recoverable error or a cookie park early in
// a broadcast skipped its archive VOD, the only copy, which then expired
// from Twitch unqueued, with nothing logged. Nothing else picks it up:
// gap-fill runs only for a Finished row.
func (b *TwitchBacklog) capturedLiveJob(j *database.Job, stagingBase string) bool {
	switch j.Status {
	case database.StatusCancelled, database.StatusUpcoming, database.StatusLive,
		database.StatusDownloading, database.StatusMuxing:
		return true
	}
	if jobstate.TwitchAutoRetryPending(j) {
		return true
	}
	if j.OutputFile != "" {
		return true
	}
	if segs, err := b.db.GetSegments(j.ID); err != nil || len(segs) > 0 {
		return true // a read that failed is taken as footage: skipping is what this did before
	}
	if g, err := b.db.GetGapFill(j.ID); err != nil || g != nil {
		return true
	}
	return stagingBase != "" && worker.HasSegmentFiles(stagingBase, j.ID)
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

// capturedLive is the job whose broadcast v is the archive of — v published
// within twitchBacklogCapturedWindow of its stream_start_time — "" for none.
func capturedLive(v twitch.TwitchVideo, starts []capturedBroadcast) string {
	published := v.Published()
	if published.IsZero() {
		return ""
	}
	for _, c := range starts {
		if d := published.Sub(c.start); d > -twitchBacklogCapturedWindow && d < twitchBacklogCapturedWindow {
			return c.jobID
		}
	}
	return ""
}

package worker

import (
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// Twitch VOD discovery (owner decision D18): a pasted channel video list
// (/<ch>/videos[?filter=…]) or collection (/collections/<id>), and the
// per-channel monitor backlog, each queue one tw_v job per VOD. They enter
// Queued at queue_priority 1 — the backlog disposition of spec §10 — so the
// archive-slots scheduler admits them archive_slots at a time per channel
// rather than handing a channel's whole catalogue to the download pool at
// once. yt-dlp has the listings (TwitchVideosIE, TwitchCollectionIE,
// twitch.py b77c9304e) and downloads every entry in turn; it has no monitor,
// and the backlog is Moombox's own, beyond upstream.

// TwitchListingOptions says how QueueTwitchVideos builds its rows.
type TwitchListingOptions struct {
	// Channels is the config's channel list: a VOD whose owner is a
	// configured Twitch channel is paced under that channel's archive_slots
	// and recorded with its quality_preference and output directory.
	Channels []config.ChannelConfig
	// Login is the listing's own channel (a /<ch>/videos listing), the owner
	// a node without one is taken to belong to. "" for a collection.
	Login string
	// QualityPreference, when set, is every row's quality_preference (the
	// add dialog's); else the owner channel's (TwitchChannelQualityPreference).
	QualityPreference string
	// ManuallyAdded marks rows an operator pasted, as every manual add is.
	ManuallyAdded bool
	// SkipHistory also skips a VOD the history already holds — a job was
	// created for it once, whatever became of it. The monitor backlog sets
	// it, as every monitor creator consults history; a pasted listing does
	// not, so pasting a list again re-queues the VODs whose jobs were since
	// deleted.
	SkipHistory bool
	// OutputDirectory, when set, is every row's output_directory; else the
	// owner channel's, when it has one.
	OutputDirectory string
	// Logger, when set, is told of each VOD that could not be queued, with
	// why (purpose review M9): the counts the caller answers with say how
	// many, never which.
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// TwitchListingResult counts what QueueTwitchVideos did: every listed video
// is exactly one of Added, Skipped and Failed.
type TwitchListingResult struct {
	Added   []*database.Job
	Skipped int // already a job (or, with SkipHistory, in history)
	Failed  int // the insert errored, or the video names no owner to pace it under
}

// QueueTwitchVideos creates one Queued tw_v<id> row per video, skipping a VOD
// that already has a job, and records each in history (what makes
// HasProcessed mean "a job was created", spec §15). The caller wakes the
// scheduler (DownloadWorker.Scheduler().Wake()) once it returns; a side
// process with no scheduler of its own leaves the rows for the daemon's
// heartbeat sweep.
//
// The rows carry only what the listing knows: the title, owner, publish time
// and length. processTwitchVod reads VideoMetadata when the scheduler admits
// the row and fills in the rest — thumbnail, description, category — as it
// does for a pasted VOD URL.
func QueueTwitchVideos(db *database.Database, videos []twitch.TwitchVideo, opts TwitchListingOptions, now time.Time) TwitchListingResult {
	var res TwitchListingResult
	stamp := now.UTC().Format(time.RFC3339)
	for _, v := range videos {
		jobID := twitch.BuildJobID(v.ID, true)
		if opts.SkipHistory {
			if seen, err := db.HasProcessed(jobID); err == nil && seen {
				res.Skipped++
				continue
			}
		}
		job := newTwitchListingJob(v, opts, stamp)
		if job.ChannelID == nil {
			// No channel to pace it under: the scheduler admits Queued rows
			// channel by channel (QueuedChannels, channel_id IS NOT NULL),
			// and a row without one would sit Queued forever while the add
			// reported it queued. A collection node whose owner neither the
			// listing nor VideoMetadata named (twitch.API.CollectionVideos).
			res.Failed++
			opts.warnNotQueued(v, "no channel to pace it under: neither the listing nor Twitch named its owner")
			continue
		}
		added, err := db.AddJob(job)
		if err != nil {
			res.Failed++
			opts.warnNotQueued(v, "the job could not be created", "err", err)
			continue
		}
		if !added { // a job already has the ID (INSERT OR IGNORE)
			res.Skipped++
			continue
		}
		db.AddToHistory(jobID)
		res.Added = append(res.Added, job)
	}
	return res
}

// warnNotQueued logs a VOD QueueTwitchVideos counted as Failed, when the
// caller gave it a logger.
func (o TwitchListingOptions) warnNotQueued(v twitch.TwitchVideo, reason string, args ...any) {
	if o.Logger == nil {
		return
	}
	o.Logger.Warn("twitch listing: a VOD could not be queued — "+reason,
		append([]any{"vod", v.ID, "title", v.Title, "channel", o.Login}, args...)...)
}

// Logger is the worker's logger, for a caller outside the package that
// queues on its behalf (the add route's TwitchListingOptions.Logger).
func (w *DownloadWorker) Logger() interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
} {
	return w.logger
}

// newTwitchListingJob is the Queued backlog row for one listed VOD. The
// channel_id is the configured channel's ID when its owner is one (so
// archive_slots paces it as that channel's), else the owner's login — paced
// under the global archive_slots, as a removed channel's kept backlog is
// (W25-09) — since the scheduler sweeps channels and never a NULL one.
//
// That channel_id is for pacing. A row a pasted listing queued is also
// manually_added, and removing its channel never deletes it (owner decision
// PR-D1, DeletePendingChannelJobs); the monitor backlog's rows are the
// channel's own, and its "delete its pending jobs" does.
func newTwitchListingJob(v twitch.TwitchVideo, opts TwitchListingOptions, stamp string) *database.Job {
	login := strings.ToLower(opts.Login)
	display := login
	if v.Owner != nil {
		if v.Owner.Login != "" {
			login = strings.ToLower(v.Owner.Login)
		}
		if v.Owner.DisplayName != "" {
			display = v.Owner.DisplayName
		} else if v.Owner.Login != "" {
			display = v.Owner.Login
		}
	}
	title := v.Title
	if strings.TrimSpace(title) == "" {
		title = "Untitled Broadcast" // yt-dlp's default, as GetVodInfo's
	}
	if display != "" {
		title = display + " — " + title
	}
	channelID := login
	var ch *config.ChannelConfig
	for i := range opts.Channels {
		if opts.Channels[i].GetPlatform() == "twitch" && login != "" && strings.EqualFold(opts.Channels[i].ID, login) {
			ch = &opts.Channels[i]
			channelID = ch.ID
			break
		}
	}
	pref := opts.QualityPreference
	if pref == "" {
		pref = TwitchChannelQualityPreference(opts.Channels, login)
	}
	outDir := opts.OutputDirectory
	if outDir == "" && ch != nil {
		outDir = ch.OutputDirectory
	}
	jobID := twitch.BuildJobID(v.ID, true)
	job := &database.Job{
		ID:                jobID,
		VideoID:           jobID,
		URL:               "https://www.twitch.tv/videos/" + v.ID,
		Title:             title,
		ChannelName:       display,
		Platform:          "twitch",
		Status:            database.StatusQueued,
		QueuePriority:     1,
		IsVod:             true,
		ManuallyAdded:     opts.ManuallyAdded,
		StreamStartTime:   v.PublishedAt,
		QualityPreference: TwitchJobQualityPreference(pref),
		OutputDirectory:   outDir,
		CreatedAt:         stamp,
		UpdatedAt:         stamp,
	}
	if channelID != "" {
		id := channelID
		job.ChannelID = &id
	}
	if v.LengthSeconds > 0 {
		n := v.LengthSeconds
		job.LengthSeconds = &n
	}
	return job
}

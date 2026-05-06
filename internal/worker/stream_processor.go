package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

const (
	maxConsecutiveProbeErrors = 10
	chatSurgeWindowMs         = 15_000 // 15 seconds (matches TS CHAT_SURGE_WINDOW_MS)
	chatSurgeThreshold        = 30     // messages in window to trigger early probe (matches TS CHAT_SURGE_THRESHOLD)
	probeJitterMax            = 30 * time.Second
	fullFetchInterval         = 30 * time.Minute // Periodic full WEB fetch for metadata ANDROID_VR can't see
	twitchPollInterval        = 15 * time.Second
	twitchPollJitterMax       = 5 * time.Second
)

// StreamProcessResult contains the result of processing a stream.
type StreamProcessResult struct {
	VideoInfo      *youtube.VideoInfo
	ShouldDownload bool
	IsVod          bool
	Error          string
	// ErrSentinel optionally wraps Error with a worker-package sentinel
	// (ErrCookiesRequired / ErrNonActionable / ErrCancelled) so the
	// downstream consumer can errors.Is the category without parsing
	// the display string. Producers set this when the failure category
	// is known; consumers fall back to plain Error otherwise.
	ErrSentinel    error
	ChatDownloader *chat.ChatDownloader // Pre-started chat downloader from upcoming phase (B2)

	// Twitch-specific fields
	TwitchStreamInfo     *twitch.TwitchStreamInfo
	TwitchVariant        *twitch.TwitchHLSVariant
	TwitchChatDownloader *twitch.ChatDownloader
	TwitchVodChatDl      *twitch.VodChatDownloader
}

// AsError converts the result's failure into an error, preserving the
// display string and (when ErrSentinel is set) the categorisation that
// errors.Is can match against. Returns nil when there's no error to
// surface (Error == "").
func (r *StreamProcessResult) AsError() error {
	if r.Error == "" {
		return nil
	}
	if r.ErrSentinel != nil {
		// Multi-%w form: errors.Is matches both the display-only inner
		// error AND the sentinel. The display string is "<message>
		// (<sentinel>)" — slightly longer than just the message but the
		// sentinel suffix is reasonably descriptive.
		return fmt.Errorf("%w (%w)", errors.New(r.Error), r.ErrSentinel)
	}
	return errors.New(r.Error)
}

// StreamProcessor handles stream status probing and waiting.
type StreamProcessor struct {
	yt          *youtube.Service
	tw          *twitch.Service
	cfg         *config.MoomboxConfig // captured for early-init reads before SetConfigStore
	configStore *config.Store         // shared config store (set via SetConfigStore)
	db          *database.Database
	notifier    *notifications.Manager
	logger      logger
	isOnline    func() bool

	twitchHints *twitchHintCache // populated by OnStreamFound; consumed by processTwitchLive

	mu          sync.Mutex
	activeChats []*chat.ChatDownloader // Track active chat downloaders for cleanup
}

// NewStreamProcessor creates a new stream processor.
func NewStreamProcessor(yt *youtube.Service, tw *twitch.Service, cfg *config.MoomboxConfig, db *database.Database, logger logger) *StreamProcessor {
	return &StreamProcessor{
		yt:          yt,
		tw:          tw,
		cfg:         cfg,
		db:          db,
		logger:      logger,
		twitchHints: newTwitchHintCache(),
	}
}

// SetConfigStore wires the shared *config.Store onto the stream processor.
// Called by DownloadWorker.SetConfigStore during startup.
func (sp *StreamProcessor) SetConfigStore(store *config.Store) {
	sp.configStore = store
}

// readConfig runs fn under configStore's read lock when wired, or against
// sp.cfg directly during early init (matches DownloadWorker.readConfig).
func (sp *StreamProcessor) readConfig(fn func(*config.MoomboxConfig)) {
	if sp.configStore != nil {
		sp.configStore.Read(fn)
		return
	}
	fn(sp.cfg)
}

// SetNotifier sets the notification manager for the stream processor.
func (sp *StreamProcessor) SetNotifier(nm *notifications.Manager) {
	sp.notifier = nm
}

// SetIsOnline sets the connectivity check function.
func (sp *StreamProcessor) SetIsOnline(fn func() bool) {
	sp.isOnline = fn
}

// Stop gracefully stops the stream processor and any active chat downloaders.
func (sp *StreamProcessor) Stop() {
	sp.mu.Lock()
	chats := make([]*chat.ChatDownloader, len(sp.activeChats))
	copy(chats, sp.activeChats)
	sp.mu.Unlock()

	for _, c := range chats {
		c.Stop()
	}
}

func (sp *StreamProcessor) trackChat(dl *chat.ChatDownloader) {
	sp.mu.Lock()
	sp.activeChats = append(sp.activeChats, dl)
	sp.mu.Unlock()
}

func (sp *StreamProcessor) untrackChat(dl *chat.ChatDownloader) {
	sp.mu.Lock()
	for i, c := range sp.activeChats {
		if c == dl {
			sp.activeChats = append(sp.activeChats[:i], sp.activeChats[i+1:]...)
			break
		}
	}
	sp.mu.Unlock()
}

// Process probes a video and determines if/how to download it.
func (sp *StreamProcessor) Process(ctx context.Context, job *database.Job) (*StreamProcessResult, error) {
	sp.logger.Info("processing stream", "videoID", job.VideoID, "jobID", job.ID, "platform", job.Platform)

	// Twitch path — completely separate from YouTube
	if job.Platform == "twitch" {
		return sp.processTwitch(ctx, job)
	}

	// Full multi-client fetch on first discovery. The lightweight ANDROID_VR probe
	// is reserved for the waitForLive polling loop where speed matters and we already
	// know the video exists. On initial processing, use the full WEB + TV fetch to
	// get accurate playability and complete metadata.
	info, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
	if err != nil {
		return nil, fmt.Errorf("full fetch failed: %w", err)
	}

	sp.logger.Debug("fetch result",
		"status", info.StreamStatus,
		"playability", info.PlayabilityError,
		"videoID", job.VideoID)

	sp.updateJobMetadata(job, info, false)

	if errMsg, sentinel := sp.checkPlayability(info); errMsg != "" {
		return &StreamProcessResult{
			VideoInfo:      info,
			ShouldDownload: false,
			Error:          errMsg,
			ErrSentinel:    sentinel,
		}, nil
	}

	return sp.handleStreamStatus(ctx, job, info)
}

// handleStreamStatus handles the stream after full video info fetch.
func (sp *StreamProcessor) handleStreamStatus(ctx context.Context, job *database.Job, info *youtube.VideoInfo) (*StreamProcessResult, error) {
	switch info.StreamStatus {
	case youtube.StreamLive:
		sp.logger.Info("stream is live, starting download", "videoID", job.VideoID)
		// Fallback: use current time if YouTube didn't provide a start time
		if info.ScheduledStartTime == "" && job.StreamStartTime == "" {
			now := time.Now().UTC().Format(time.RFC3339)
			info.ScheduledStartTime = now
		}
		sp.updateJobMetadata(job, info, true)
		sp.db.UpdateJobFields(job.ID, map[string]any{
			"status": database.StatusLive,
			"is_vod": false,
		})
		return &StreamProcessResult{
			VideoInfo:      info,
			ShouldDownload: true,
			IsVod:          false,
		}, nil

	case youtube.StreamVOD, youtube.StreamPostLive:
		sp.logger.Info("stream is VOD, downloading directly", "videoID", job.VideoID)
		sp.db.UpdateJobFields(job.ID, map[string]any{
			"status": database.StatusDownloading,
			"is_vod": true,
		})
		return &StreamProcessResult{
			VideoInfo:      info,
			ShouldDownload: true,
			IsVod:          true,
		}, nil

	case youtube.StreamNotAStream:
		if job.ManuallyAdded || job.AllowNonStream {
			reason := "include_non_live_content enabled"
			if job.ManuallyAdded {
				reason = "manually added"
			}
			sp.logger.Info("not a stream but downloading as VOD",
				"videoID", job.VideoID, "reason", reason)
			sp.db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusDownloading,
				"is_vod": true,
			})
			return &StreamProcessResult{
				VideoInfo:      info,
				ShouldDownload: true,
				IsVod:          true,
			}, nil
		}
		return &StreamProcessResult{
			ShouldDownload: false,
			Error:          "not a stream",
		}, nil

	case youtube.StreamUpcoming:
		return sp.waitForLive(ctx, job, info)

	default:
		return &StreamProcessResult{
			ShouldDownload: false,
			Error:          fmt.Sprintf("unhandled status: %s", info.StreamStatus),
		}, nil
	}
}

// checkPlayability returns an error string and an optional sentinel for
// classification when the video is not playable. The display string is
// the user-facing error; the sentinel (ErrCookiesRequired for member /
// login, ErrNonActionable for age-restricted) lets the downstream
// worker route the job to StatusCookies or suppress notifications via
// errors.Is rather than substring matching. Both empty/nil → playable.
func (sp *StreamProcessor) checkPlayability(info *youtube.VideoInfo) (string, error) {
	if info.PlayabilityError == "" || info.PlayabilityError == youtube.PlayabilityOK {
		return "", nil
	}
	reason := info.PlayabilityReason
	if reason == "" {
		reason = "Unknown error"
	}
	switch info.PlayabilityError {
	case youtube.PlayabilityMembersOnly:
		return fmt.Sprintf("Member-only: %s", reason), ErrCookiesRequired
	case youtube.PlayabilityLoginRequired:
		return fmt.Sprintf("Login required: %s", reason), ErrCookiesRequired
	case youtube.PlayabilityAgeRestricted:
		return fmt.Sprintf("Age restricted: %s", reason), ErrNonActionable
	default:
		return reason, nil
	}
}

// Probe-interval bands. Tighter polling near the scheduled go-live time so
// we don't miss the live transition, looser polling for distant streams to
// avoid hammering YouTube. Audit reports/worker.md F46.
const (
	probeIntervalImminent = 1 * time.Minute  // < 5 min until scheduled start
	probeIntervalNear     = 5 * time.Minute  // < 1 hour
	probeIntervalDistant  = 10 * time.Minute // ≥ 1 hour OR no schedule
)

func (sp *StreamProcessor) calculateProbeInterval(info *youtube.VideoInfo) time.Duration {
	if info.ScheduledStartTime != "" {
		if t, err := time.Parse(time.RFC3339, info.ScheduledStartTime); err == nil {
			until := time.Until(t)
			switch {
			case until <= 5*time.Minute:
				return probeIntervalImminent
			case until <= 1*time.Hour:
				return probeIntervalNear
			default:
				return probeIntervalDistant
			}
		}
	}
	return probeIntervalDistant
}

// updateJobMetadata updates job metadata from video info.
//
// When overwrite is false (initial fetch): fills blank fields only.
// When overwrite is true (probe refresh / live transition): overwrites fields
// that have actually changed, skipping DB writes when nothing differs.
//
// In both modes, guards apply: empty strings and "Unknown Title"/"Unknown Channel"
// sentinel values are never written.
func (sp *StreamProcessor) updateJobMetadata(job *database.Job, info *youtube.VideoInfo, overwrite bool) {
	updates := map[string]any{}
	notifyStartTimeConfirmed := false
	notifyScheduleChanged := false
	oldStartTime := ""

	// Title
	if info.Title != "" && info.Title != "Unknown Title" {
		if overwrite {
			if info.Title != job.Title {
				updates["title"] = info.Title
			}
		} else {
			updates["title"] = info.Title
		}
	}

	// Channel name
	if info.ChannelName != "" && info.ChannelName != "Unknown Channel" {
		if overwrite {
			if info.ChannelName != job.ChannelName {
				updates["channel_name"] = info.ChannelName
			}
		} else {
			updates["channel_name"] = info.ChannelName
		}
	}

	// Thumbnail
	if info.ThumbnailURL != "" {
		if overwrite {
			if info.ThumbnailURL != job.ThumbnailURL {
				updates["thumbnail_url"] = info.ThumbnailURL
			}
		} else {
			updates["thumbnail_url"] = info.ThumbnailURL
		}
	}

	// Description
	if info.Description != "" {
		if overwrite {
			if info.Description != job.Description {
				updates["description"] = info.Description
			}
		} else {
			updates["description"] = info.Description
		}
	}

	// Stream start time
	if info.ScheduledStartTime != "" {
		if overwrite {
			if info.ScheduledStartTime != job.StreamStartTime {
				if job.StreamStartTime == "" {
					notifyStartTimeConfirmed = true
				} else if !info.IsLive {
					// Only notify schedule change for genuine reschedules (upcoming).
					// When a stream goes live, YouTube updates the scheduled time to the
					// actual start — that's not a reschedule, and Download Starting covers it.
					notifyScheduleChanged = true
					oldStartTime = job.StreamStartTime
				}
				updates["stream_start_time"] = info.ScheduledStartTime
			}
		} else if job.StreamStartTime == "" {
			updates["stream_start_time"] = info.ScheduledStartTime
			notifyStartTimeConfirmed = true
		}
	}

	// Stream end time
	if info.EndTimestamp != "" {
		if overwrite {
			if info.EndTimestamp != job.StreamEndTime {
				updates["stream_end_time"] = info.EndTimestamp
			}
		} else if job.StreamEndTime == "" {
			updates["stream_end_time"] = info.EndTimestamp
		}
	}

	// Length
	if info.LengthSeconds != nil && *info.LengthSeconds > 0 {
		if overwrite {
			if job.LengthSeconds == nil || *info.LengthSeconds != *job.LengthSeconds {
				updates["length_seconds"] = *info.LengthSeconds
			}
		} else {
			updates["length_seconds"] = *info.LengthSeconds
		}
	}

	if len(updates) > 0 {
		sp.db.UpdateJobFields(job.ID, updates)

		// Sync local job object
		if v, ok := updates["title"].(string); ok {
			job.Title = v
		}
		if v, ok := updates["channel_name"].(string); ok {
			job.ChannelName = v
		}
		if v, ok := updates["thumbnail_url"].(string); ok {
			job.ThumbnailURL = v
		}
		if v, ok := updates["description"].(string); ok {
			job.Description = v
		}
		if v, ok := updates["stream_start_time"].(string); ok {
			job.StreamStartTime = v
		}
		if v, ok := updates["stream_end_time"].(string); ok {
			job.StreamEndTime = v
		}
		if v, ok := updates["length_seconds"].(int); ok {
			job.LengthSeconds = &v
		}
	}

	// Notifications
	if notifyStartTimeConfirmed && (info.IsUpcoming || info.IsLive) && sp.notifier != nil {
		startsAt := info.ScheduledStartTime
		if t, err := time.Parse(time.RFC3339, startsAt); err == nil {
			startsAt = fmt.Sprintf("<t:%d:f>", t.Unix())
		}
		sp.notifier.Send("YouTube Start Time Confirmed",
			fmt.Sprintf("Scheduled: %s", job.Title),
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Channel", Value: job.ChannelName, Inline: true},
				{Name: "Starts At", Value: startsAt, Inline: true},
			},
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "scheduled",
			},
		)
	}

	if notifyScheduleChanged && sp.notifier != nil {
		fmtTime := func(raw string) string {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return fmt.Sprintf("<t:%d:f>", t.Unix())
			}
			return raw
		}
		sp.notifier.Send("YouTube Schedule Changed",
			fmt.Sprintf("Rescheduled: %s", job.Title),
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Channel", Value: job.ChannelName, Inline: true},
				{Name: "Old Time", Value: fmtTime(oldStartTime), Inline: true},
				{Name: "New Time", Value: fmtTime(info.ScheduledStartTime), Inline: true},
			},
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "rescheduled",
			},
		)
	}
}

// StashTwitchStreamInfo records a freshly-fetched Twitch stream info under
// the given jobID. The next processTwitchLive call for that jobID will use
// this info instead of re-querying Twitch — eliminating the duplicate GQL
// call that exposed the worker to transient StreamMetadata flaps.
//
// Take-once: the hint is removed on consumption. If never consumed, it
// expires after twitchHintTTL.
func (sp *StreamProcessor) StashTwitchStreamInfo(jobID string, info *twitch.TwitchStreamInfo) {
	sp.twitchHints.stash(jobID, info)
}

// TwitchHintStats returns a snapshot of hit/miss counters for the take-once
// hint cache. Exposed so the stats endpoint can surface whether the
// monitor → processor pass-through is actually firing in production. A drop
// to zero after a refactor would otherwise be invisible.
func (sp *StreamProcessor) TwitchHintStats() TwitchHintStats {
	return sp.twitchHints.Stats()
}

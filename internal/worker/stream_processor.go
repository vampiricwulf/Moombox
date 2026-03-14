package worker

import (
	"context"
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
	chatSurgeWindowMs        = 15_000 // 15 seconds (matches TS CHAT_SURGE_WINDOW_MS)
	chatSurgeThreshold       = 30     // messages in window to trigger early probe (matches TS CHAT_SURGE_THRESHOLD)
	probeJitterMax           = 30 * time.Second
	twitchPollInterval       = 15 * time.Second
	twitchPollJitterMax      = 5 * time.Second
)

// StreamProcessResult contains the result of processing a stream.
type StreamProcessResult struct {
	VideoInfo      *youtube.VideoInfo
	ShouldDownload bool
	IsVod          bool
	Error          string
	ChatDownloader *chat.ChatDownloader // Pre-started chat downloader from upcoming phase (B2)

	// Twitch-specific fields
	TwitchStreamInfo     *twitch.TwitchStreamInfo
	TwitchVariant        *twitch.TwitchHLSVariant
	TwitchChatDownloader *twitch.ChatDownloader
	TwitchVodChatDl      *twitch.VodChatDownloader
}

// StreamProcessor handles stream status probing and waiting.
type StreamProcessor struct {
	yt       *youtube.Service
	tw       *twitch.Service
	cfg      *config.MoomboxConfig
	cfgMu    *sync.RWMutex // shared config mutex (set via SetCfgMu)
	db       *database.Database
	notifier *notifications.Manager
	logger   logger

	mu          sync.Mutex
	activeChats []*chat.ChatDownloader // Track active chat downloaders for cleanup
}

// NewStreamProcessor creates a new stream processor.
func NewStreamProcessor(yt *youtube.Service, tw *twitch.Service, cfg *config.MoomboxConfig, db *database.Database, logger logger) *StreamProcessor {
	return &StreamProcessor{yt: yt, tw: tw, cfg: cfg, db: db, logger: logger}
}

// SetCfgMu sets the shared config mutex for synchronized config access.
func (sp *StreamProcessor) SetCfgMu(mu *sync.RWMutex) {
	sp.cfgMu = mu
}

// SetNotifier sets the notification manager for the stream processor.
func (sp *StreamProcessor) SetNotifier(nm *notifications.Manager) {
	sp.notifier = nm
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

	sp.updateJobMetadata(job, info)

	if errMsg := sp.checkPlayability(info); errMsg != "" {
		return &StreamProcessResult{
			VideoInfo:      info,
			ShouldDownload: false,
			Error:          errMsg,
		}, nil
	}

	return sp.handleStreamStatus(ctx, job, info)
}

// handleStreamStatus handles the stream after full video info fetch.
func (sp *StreamProcessor) handleStreamStatus(ctx context.Context, job *database.Job, info *youtube.VideoInfo) (*StreamProcessResult, error) {
	switch info.StreamStatus {
	case youtube.StreamLive:
		sp.logger.Info("stream is live, starting download", "videoID", job.VideoID)
		sp.updateJobMetadataOnLive(job, info)
		sp.db.UpdateJobFields(job.ID, map[string]any{
			"status": database.StatusLive,
			"is_vod": false,
		})
		sp.sendLiveNotification(job, info)
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

// checkPlayability returns an error string if the video is not playable (matches TS checkPlayability).
func (sp *StreamProcessor) checkPlayability(info *youtube.VideoInfo) string {
	if info.PlayabilityError == "" || info.PlayabilityError == youtube.PlayabilityOK {
		return ""
	}
	reason := info.PlayabilityReason
	if reason == "" {
		reason = "Unknown error"
	}
	switch info.PlayabilityError {
	case youtube.PlayabilityMembersOnly:
		return fmt.Sprintf("Member-only: %s", reason)
	case youtube.PlayabilityLoginRequired:
		return fmt.Sprintf("Login required: %s", reason)
	case youtube.PlayabilityAgeRestricted:
		return fmt.Sprintf("Age restricted: %s", reason)
	default:
		return reason
	}
}

func (sp *StreamProcessor) calculateProbeInterval(info *youtube.VideoInfo) time.Duration {
	if info.ScheduledStartTime != "" {
		if t, err := time.Parse(time.RFC3339, info.ScheduledStartTime); err == nil {
			until := time.Until(t)
			switch {
			case until <= 5*time.Minute:
				return 1 * time.Minute
			case until <= 1*time.Hour:
				return 5 * time.Minute
			default:
				return 10 * time.Minute
			}
		}
	}
	return 10 * time.Minute
}

func (sp *StreamProcessor) updateJobMetadata(job *database.Job, info *youtube.VideoInfo) {
	updates := map[string]any{}
	shouldNotifyStartTime := false

	if info.Title != "" && info.Title != "Unknown Title" {
		updates["title"] = info.Title
	}
	if info.ChannelName != "" && info.ChannelName != "Unknown Channel" {
		updates["channel_name"] = info.ChannelName
	}
	if info.ThumbnailURL != "" {
		updates["thumbnail_url"] = info.ThumbnailURL
	}
	if info.Description != "" {
		updates["description"] = info.Description
	}
	// Store stream start time for filename template
	if info.ScheduledStartTime != "" && job.StreamStartTime == "" {
		updates["stream_start_time"] = info.ScheduledStartTime
		shouldNotifyStartTime = true
	}
	if info.LengthSeconds != nil && *info.LengthSeconds > 0 {
		updates["length_seconds"] = *info.LengthSeconds
	}
	// Stream end time
	if info.EndTimestamp != "" && job.StreamEndTime == "" {
		updates["stream_end_time"] = info.EndTimestamp
	}

	if len(updates) > 0 {
		sp.db.UpdateJobFields(job.ID, updates)
		// Apply updates to local job object
		if v, ok := updates["title"].(string); ok {
			job.Title = v
		}
		if v, ok := updates["channel_name"].(string); ok {
			job.ChannelName = v
		}
		if v, ok := updates["thumbnail_url"].(string); ok {
			job.ThumbnailURL = v
		}
		if v, ok := updates["stream_start_time"].(string); ok {
			job.StreamStartTime = v
		}
		if v, ok := updates["stream_end_time"].(string); ok {
			job.StreamEndTime = v
		}
	}

	// Notify when scheduled start time is first confirmed
	if shouldNotifyStartTime && (info.IsUpcoming || info.IsLive) && sp.notifier != nil {
		startsAt := info.ScheduledStartTime
		if t, err := time.Parse(time.RFC3339, startsAt); err == nil {
			startsAt = t.Format("2006-01-02 15:04:05")
		}
		sp.notifier.Send("Start Time Confirmed",
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
}

// updateJobMetadataOnLive refreshes all metadata when stream transitions to live.
// Always overwrites stream_start_time from scheduledStartTime (YouTube updates it when going live),
// or falls back to current time if no start time is available.
func (sp *StreamProcessor) updateJobMetadataOnLive(job *database.Job, info *youtube.VideoInfo) {
	updates := map[string]any{}

	if info.Title != "" && info.Title != "Unknown Title" {
		updates["title"] = info.Title
		job.Title = info.Title
	}
	if info.ChannelName != "" && info.ChannelName != "Unknown Channel" {
		updates["channel_name"] = info.ChannelName
		job.ChannelName = info.ChannelName
	}
	if info.ThumbnailURL != "" {
		updates["thumbnail_url"] = info.ThumbnailURL
		job.ThumbnailURL = info.ThumbnailURL
	}
	if info.Description != "" {
		updates["description"] = info.Description
	}

	// Always overwrite streamStartTime — YouTube updates scheduledStartTime when stream goes live
	if info.ScheduledStartTime != "" {
		updates["stream_start_time"] = info.ScheduledStartTime
		job.StreamStartTime = info.ScheduledStartTime
	} else if job.StreamStartTime == "" {
		// No start time from YouTube — use the moment we detected it going live
		now := time.Now().UTC().Format(time.RFC3339)
		updates["stream_start_time"] = now
		job.StreamStartTime = now
	}

	if info.LengthSeconds != nil && *info.LengthSeconds > 0 {
		updates["length_seconds"] = *info.LengthSeconds
	}
	// Always update end time (not conditional on empty)
	if info.EndTimestamp != "" {
		updates["stream_end_time"] = info.EndTimestamp
		job.StreamEndTime = info.EndTimestamp
	}

	if len(updates) > 0 {
		sp.db.UpdateJobFields(job.ID, updates)
		sp.logger.Info("refreshed metadata on live", "videoID", job.VideoID)
	}
}

// sendLiveNotification sends a "Stream Live" notification (matches TS sendLiveNotification).
func (sp *StreamProcessor) sendLiveNotification(job *database.Job, info *youtube.VideoInfo) {
	if sp.notifier == nil {
		return
	}
	url := job.URL
	if url == "" {
		url = fmt.Sprintf("https://www.youtube.com/watch?v=%s", job.VideoID)
	}
	thumbnail := ""
	if info != nil {
		thumbnail = info.ThumbnailURL
	}
	sp.notifier.Send("Stream Live",
		fmt.Sprintf("Stream is now live: %s", job.Title),
		notifications.TypeSuccess,
		[]notifications.Field{
			{Name: "Channel", Value: job.ChannelName, Inline: true},
			{Name: "Video ID", Value: job.VideoID, Inline: true},
		},
		notifications.SendOptions{
			URL:       url,
			Thumbnail: thumbnail,
			Event:     "live",
		},
	)
}

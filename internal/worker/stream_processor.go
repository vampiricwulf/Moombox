package worker

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
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
	TwitchStreamInfo    *twitch.TwitchStreamInfo
	TwitchVariant       *twitch.TwitchHLSVariant
	TwitchChatDownloader *twitch.ChatDownloader
	TwitchVodChatDl     *twitch.VodChatDownloader
}

// StreamProcessor handles stream status probing and waiting.
type StreamProcessor struct {
	yt       *youtube.Service
	tw       *twitch.Service
	cfg      *config.MoomboxConfig
	db       *database.Database
	notifier *notifications.Manager
	logger   Logger

	mu          sync.Mutex
	activeChats []*chat.ChatDownloader // Track active chat downloaders for cleanup
}

// NewStreamProcessor creates a new stream processor.
func NewStreamProcessor(yt *youtube.Service, tw *twitch.Service, cfg *config.MoomboxConfig, db *database.Database, logger Logger) *StreamProcessor {
	return &StreamProcessor{yt: yt, tw: tw, cfg: cfg, db: db, logger: logger}
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

// waitForLive polls until a stream goes live or is cancelled.
// B2: Starts early chat downloader during upcoming phase to capture pre-stream chat.
func (sp *StreamProcessor) waitForLive(ctx context.Context, job *database.Job, initialInfo *youtube.VideoInfo) (*StreamProcessResult, error) {
	sp.logger.Info("waiting for stream to go live", "videoID", job.VideoID)

	sp.db.UpdateJobFields(job.ID, map[string]any{
		"status":          database.StatusUpcoming,
		"last_recheck_at": time.Now().UTC().Format(time.RFC3339),
	})

	consecutiveErrors := 0
	scheduledStartTime := initialInfo.ScheduledStartTime
	membersOnly := false

	// B2: Start early chat downloader (only if chat download is enabled)
	var chatDl *chat.ChatDownloader
	if sp.cfg.Downloader.DownloadChat {
		chatDl = sp.tryStartEarlyChat(ctx, job, initialInfo)
	}

	// B2: Chat surge detection + throttled DB updates for chat count
	var surgeMu sync.Mutex
	surgeWindowStart := time.Now()
	surgeWindowCount := 0
	surgeCh := make(chan struct{}, 1)
	lastChatDBUpdate := time.Now()

	chatProgressFn := func(p chat.ChatProgress) {
		surgeMu.Lock()
		defer surgeMu.Unlock()

		// Throttled DB update for chat count (every 5 seconds)
		if time.Since(lastChatDBUpdate) >= 5*time.Second {
			sp.db.UpdateJobFields(job.ID, map[string]any{
				"total_chat_messages": p.MessageCount,
			})
			lastChatDBUpdate = time.Now()
		}

		elapsed := time.Since(surgeWindowStart)
		if elapsed >= time.Duration(chatSurgeWindowMs)*time.Millisecond {
			surgeWindowStart = time.Now()
			surgeWindowCount = p.MessageCount
			return
		}

		delta := p.MessageCount - surgeWindowCount
		if delta >= chatSurgeThreshold {
			sp.logger.Info("chat surge detected — triggering early probe",
				"delta", delta, "videoID", job.VideoID)
			select {
			case surgeCh <- struct{}{}:
			default:
			}
			surgeWindowStart = time.Now()
			surgeWindowCount = p.MessageCount
		}
	}

	if chatDl != nil {
		chatDl.OnProgress = chatProgressFn
	}

	for {
		select {
		case <-ctx.Done():
			sp.stopEarlyChat(chatDl)
			return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
		default:
		}

		// Check if job was cancelled
		currentJob, err := sp.db.GetJob(job.ID)
		if err == nil && currentJob.Status == database.StatusCancelled {
			sp.stopEarlyChat(chatDl)
			return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
		}

		// A5: Calculate probe interval with jitter
		info := &youtube.VideoInfo{ScheduledStartTime: scheduledStartTime}
		interval := sp.calculateProbeInterval(info)
		jitter := time.Duration(rand.Int63n(int64(probeJitterMax)))

		// B2: Race sleep against chat surge
		probeTimer := time.NewTimer(interval + jitter)
		select {
		case <-ctx.Done():
			probeTimer.Stop()
			sp.stopEarlyChat(chatDl)
			return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
		case <-probeTimer.C:
			// Normal poll
		case <-surgeCh:
			probeTimer.Stop()
			sp.logger.Info("chat surge triggered early probe", "videoID", job.VideoID)
		}

		// Update last recheck time
		sp.db.UpdateJobFields(job.ID, map[string]any{
			"last_recheck_at": time.Now().UTC().Format(time.RFC3339),
		})

		// Probe — use lightweight authenticated probe if members-only was detected
		var probeInfo *youtube.VideoInfo
		var probeErr error
		if membersOnly {
			probeInfo, probeErr = sp.yt.ProbeVideoStatusAuthenticated(ctx, job.VideoID)
		} else {
			probeInfo, probeErr = sp.yt.ProbeVideoStatus(ctx, job.VideoID)
		}
		if err := probeErr; err != nil {
			consecutiveErrors++
			sp.logger.Warn("probe error", "videoID", job.VideoID, "err", err, "consecutive", consecutiveErrors)
			if consecutiveErrors >= maxConsecutiveProbeErrors {
				sp.stopEarlyChat(chatDl)
				return nil, fmt.Errorf("max probe errors: %w", err)
			}
			continue
		}
		consecutiveErrors = 0

		// Update scheduled start time + persist to DB if not yet stored
		if probeInfo.ScheduledStartTime != "" {
			scheduledStartTime = probeInfo.ScheduledStartTime
			if job.StreamStartTime == "" {
				job.StreamStartTime = probeInfo.ScheduledStartTime
				sp.db.UpdateJobFields(job.ID, map[string]any{
					"stream_start_time": probeInfo.ScheduledStartTime,
				})
			}
		}

		// B2: Try starting chat if not yet available
		if chatDl == nil && sp.cfg.Downloader.DownloadChat {
			chatDl = sp.tryStartEarlyChat(ctx, job, probeInfo)
			if chatDl != nil {
				chatDl.OnProgress = chatProgressFn
			}
		}

		// B1: Handle transition to members-only during upcoming
		if !membersOnly &&
			(probeInfo.PlayabilityError == youtube.PlayabilityMembersOnly ||
				probeInfo.PlayabilityError == youtube.PlayabilityLoginRequired) &&
			sp.yt.Auth.HasAuthCookies() {
			sp.logger.Info("stream became members-only, switching to authenticated probe",
				"videoID", job.VideoID)
			membersOnly = true
		}

		switch probeInfo.StreamStatus {
		case youtube.StreamLive:
			sp.logger.Info("stream is now live", "videoID", job.VideoID)
			fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
			if err != nil {
				sp.stopEarlyChat(chatDl)
				return nil, fmt.Errorf("full fetch on live: %w", err)
			}
			sp.updateJobMetadataOnLive(job, fullInfo)
			sp.db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusLive,
				"is_vod": false,
			})
			sp.sendLiveNotification(job, fullInfo)

			// Untrack early chat — it will be handed to the orchestrator
			if chatDl != nil {
				sp.untrackChat(chatDl)
			}
			return &StreamProcessResult{
				VideoInfo:      fullInfo,
				ShouldDownload: true,
				IsVod:          false,
				ChatDownloader: chatDl, // Pass pre-started chat to orchestrator
			}, nil

		case youtube.StreamVOD, youtube.StreamPostLive:
			sp.logger.Info("stream became VOD", "videoID", job.VideoID)
			fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
			if err != nil {
				sp.stopEarlyChat(chatDl)
				return nil, fmt.Errorf("full fetch on VOD: %w", err)
			}
			sp.updateJobMetadataOnLive(job, fullInfo)
			sp.db.UpdateJobFields(job.ID, map[string]any{
				"is_vod": true,
			})
			if chatDl != nil {
				sp.untrackChat(chatDl)
			}
			return &StreamProcessResult{
				VideoInfo:      fullInfo,
				ShouldDownload: true,
				IsVod:          true,
				ChatDownloader: chatDl,
			}, nil

		case youtube.StreamUpcoming:
			// Still waiting
			continue

		default:
			// Auth probe may return unclear status for members-only content.
			// Do full fetch to determine actual state (matching TS auth probe unclear handling).
			if membersOnly {
				sp.logger.Info("auth probe unclear, doing full fetch",
					"status", probeInfo.StreamStatus, "videoID", job.VideoID)
				fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
				if err != nil {
					sp.logger.Warn("full fetch after unclear auth probe failed", "err", err)
					continue // Retry on next iteration
				}
				switch fullInfo.StreamStatus {
				case youtube.StreamLive:
					sp.updateJobMetadataOnLive(job, fullInfo)
					sp.db.UpdateJobFields(job.ID, map[string]any{
						"status": database.StatusLive,
						"is_vod": false,
					})
					sp.sendLiveNotification(job, fullInfo)
					if chatDl != nil {
						sp.untrackChat(chatDl)
					}
					return &StreamProcessResult{VideoInfo: fullInfo, ShouldDownload: true, IsVod: false, ChatDownloader: chatDl}, nil
				case youtube.StreamVOD, youtube.StreamPostLive:
					sp.updateJobMetadataOnLive(job, fullInfo)
					sp.db.UpdateJobFields(job.ID, map[string]any{
						"is_vod": true,
					})
					if chatDl != nil {
						sp.untrackChat(chatDl)
					}
					return &StreamProcessResult{VideoInfo: fullInfo, ShouldDownload: true, IsVod: true, ChatDownloader: chatDl}, nil
				default:
					// Still upcoming per full fetch — continue polling
					continue
				}
			}
			sp.stopEarlyChat(chatDl)
			return &StreamProcessResult{
				ShouldDownload: false,
				Error:          fmt.Sprintf("unexpected status: %s", probeInfo.StreamStatus),
			}, nil
		}
	}
}

// tryStartEarlyChat attempts to start a chat downloader during the upcoming phase (B2).
func (sp *StreamProcessor) tryStartEarlyChat(ctx context.Context, job *database.Job, info *youtube.VideoInfo) *chat.ChatDownloader {
	// Fetch watch page to get chat continuation token
	cookieHeader := ""
	if sp.yt != nil && sp.yt.Auth != nil {
		cookieHeader = sp.yt.Auth.GetCookieHeader()
	}

	watchResult, err := youtube.FetchWatchPage(ctx, job.VideoID, cookieHeader)
	if err != nil {
		sp.logger.Debug("failed to fetch watch page for early chat", "err", err, "videoID", job.VideoID)
		return nil
	}

	continuation, isReplay, err := chat.ExtractChatContinuation(watchResult.HTML)
	if err != nil || continuation == "" {
		sp.logger.Debug("no chat continuation for early chat", "videoID", job.VideoID, "err", err)
		return nil
	}

	visitorData := ""
	if watchResult.Ytcfg != nil {
		visitorData = watchResult.Ytcfg.VisitorData
	}

	// Create staging dir for early chat output (matches TypeScript behavior)
	stagingBase := sp.cfg.Paths.StagingDirectory
	if stagingBase == "" {
		stagingBase = "./staging"
	}
	chatStagingDir := filepath.Join(stagingBase, job.ID)
	if err := os.MkdirAll(chatStagingDir, 0o755); err != nil {
		sp.logger.Warn("failed to create staging dir for early chat", "err", err)
		return nil
	}
	chatPath := filepath.Join(chatStagingDir, "chat.json")

	opts := chat.ChatDownloaderOptions{
		VideoID:             job.VideoID,
		VideoTitle:          job.Title,
		ChannelName:         job.ChannelName,
		OutputFile:          chatPath,
		InitialContinuation: continuation,
		ApiKey:              constants.DefaultAPIKey,
		VisitorData:         visitorData,
		CookieHeader:        cookieHeader,
		IsReplay:            isReplay,
		IsLiveOrUpcoming:    true,
	}
	if sp.yt != nil && sp.yt.Auth != nil {
		opts.GenerateAuth = sp.yt.Auth.GenerateAuthorizationHeader
	}
	if info.ScheduledStartTime != "" {
		opts.StreamStartTime = info.ScheduledStartTime
	}

	dl := chat.NewChatDownloader(opts)
	dl.OnError = func(err error) {
		sp.logger.Warn("[Chat] Early chat API error", "jobID", job.ID, "err", err)
	}
	sp.trackChat(dl)

	sp.db.UpdateJobFields(job.ID, map[string]any{
		"chat_status": "downloading",
	})

	// Start in background
	go func() {
		defer func() {
			if r := recover(); r != nil {
				sp.logger.Error("panic in early chat downloader", "jobID", job.ID, "panic", fmt.Sprint(r))
			}
		}()
		dl.Start(ctx)
	}()

	sp.logger.Info("started early chat download for upcoming stream", "videoID", job.VideoID)
	return dl
}

func (sp *StreamProcessor) stopEarlyChat(chatDl *chat.ChatDownloader) {
	if chatDl != nil {
		chatDl.Stop()
		sp.untrackChat(chatDl)
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
		if v, ok := updates["title"]; ok {
			job.Title = v.(string)
		}
		if v, ok := updates["channel_name"]; ok {
			job.ChannelName = v.(string)
		}
		if v, ok := updates["thumbnail_url"]; ok {
			job.ThumbnailURL = v.(string)
		}
		if v, ok := updates["stream_start_time"]; ok {
			job.StreamStartTime = v.(string)
		}
		if v, ok := updates["stream_end_time"]; ok {
			job.StreamEndTime = v.(string)
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

// processTwitch handles Twitch stream/VOD processing.
func (sp *StreamProcessor) processTwitch(ctx context.Context, job *database.Job) (*StreamProcessResult, error) {
	if sp.tw == nil {
		return &StreamProcessResult{ShouldDownload: false, Error: "twitch service not available"}, nil
	}

	login := extractTwitchLoginFromJob(job)
	if login == "" {
		return &StreamProcessResult{ShouldDownload: false, Error: "could not determine Twitch channel login"}, nil
	}

	isVodJob := strings.HasPrefix(job.VideoID, "tw_v")

	if isVodJob {
		return sp.processTwitchVod(ctx, job, login)
	}
	return sp.processTwitchLive(ctx, job, login)
}

func (sp *StreamProcessor) processTwitchVod(ctx context.Context, job *database.Job, login string) (*StreamProcessResult, error) {
	vodID := strings.TrimPrefix(job.VideoID, "tw_v")

	vodInfo, err := sp.tw.GetVodInfo(ctx, vodID)
	if err != nil {
		return &StreamProcessResult{ShouldDownload: false, IsVod: true, Error: fmt.Sprintf("twitch VOD error: %v", err)}, nil
	}

	vodUpdates := map[string]any{
		"status":         database.StatusDownloading,
		"is_vod":         true,
		"title":          vodInfo.ChannelDisplayName + " — " + vodInfo.Title,
		"channel_name":   vodInfo.ChannelDisplayName,
		"thumbnail_url":  vodInfo.ThumbnailURL,
		"length_seconds": vodInfo.Duration,
	}
	if vodInfo.GameCategory != "" {
		vodUpdates["twitch_category"] = vodInfo.GameCategory
	}
	sp.db.UpdateJobFields(job.ID, vodUpdates)

	variants, err := sp.tw.GetVodHLSPlaylist(ctx, vodID)
	if err != nil {
		return &StreamProcessResult{ShouldDownload: false, IsVod: true, Error: fmt.Sprintf("twitch VOD HLS error: %v", err)}, nil
	}

	variant := sp.tw.SelectBestVariant(variants, job.TwitchQuality, sp.cfg.Downloader.MaxVideoResolution)
	if variant == nil {
		return &StreamProcessResult{ShouldDownload: false, IsVod: true, Error: "no suitable HLS quality found for VOD"}, nil
	}

	sp.logger.Info("twitch VOD ready", "vodID", vodID, "quality", variant.Name)

	result := &StreamProcessResult{
		ShouldDownload: true,
		IsVod:          true,
		TwitchVariant:  variant,
	}

	// Create VOD chat downloader if chat download is enabled
	if sp.cfg.Downloader.DownloadChat {
		stagingDir := filepath.Join(sp.cfg.Paths.StagingDirectory, job.ID)
		if sp.cfg.Paths.StagingDirectory == "" {
			stagingDir = filepath.Join("staging", job.ID)
		}
		if err := os.MkdirAll(stagingDir, 0o755); err != nil {
			sp.logger.Warn("failed to create staging dir for VOD chat", "err", err)
		} else {
			chatPath := filepath.Join(stagingDir, "chat.json")
			var vodStartMs int64
			if vodInfo.CreatedAt != "" {
				if t, parseErr := time.Parse(time.RFC3339, vodInfo.CreatedAt); parseErr == nil {
					vodStartMs = t.UnixMilli()
				}
			}

			authToken := ""
			if sp.tw != nil && sp.tw.Auth != nil {
				authToken = sp.tw.Auth.GetAuthToken()
			}

			vodChatDl := twitch.NewVodChatDownloader(sp.tw.API, twitch.VodChatOptions{
				VodID:         vodID,
				ChannelLogin:  vodInfo.ChannelLogin,
				ChannelName:   vodInfo.ChannelDisplayName,
				ChannelID:     vodInfo.ChannelID,
				AuthToken:     authToken,
				OutputPath:    chatPath,
				VodDuration:   vodInfo.Duration,
				VodStartMs:    vodStartMs,
				EmoteResolver: sp.tw.Emotes,
			}, sp.logger)
			result.TwitchVodChatDl = vodChatDl

			sp.db.UpdateJobFields(job.ID, map[string]any{
				"chat_status": "downloading",
			})
		}
	} else {
		sp.db.UpdateJobFields(job.ID, map[string]any{
			"chat_status": "unavailable",
		})
	}

	return result, nil
}

func (sp *StreamProcessor) processTwitchLive(ctx context.Context, job *database.Job, login string) (*StreamProcessResult, error) {
	streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
	if err != nil {
		return nil, fmt.Errorf("twitch stream info: %w", err)
	}

	if streamInfo == nil || !streamInfo.IsLive {
		if job.ManuallyAdded {
			sp.logger.Info("twitch channel is offline, waiting for stream", "channel", login)
			streamInfo, err = sp.waitForTwitchLive(ctx, job, login)
			if err != nil {
				return nil, err
			}
			if streamInfo == nil {
				return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
			}
			// Fall through to existing live handling below
		} else {
			sp.logger.Info("twitch channel is offline", "channel", login)
			return &StreamProcessResult{ShouldDownload: false, Error: "twitch channel is offline"}, nil
		}
	}

	// Update job metadata from stream info
	updates := map[string]any{}
	if streamInfo.Title != "" {
		updates["title"] = streamInfo.ChannelDisplayName + " — " + streamInfo.Title
	}
	if streamInfo.ChannelDisplayName != "" {
		updates["channel_name"] = streamInfo.ChannelDisplayName
	}
	if streamInfo.ThumbnailURL != "" {
		updates["thumbnail_url"] = streamInfo.ThumbnailURL
	}
	if streamInfo.ProfileImageURL != "" {
		updates["channel_avatar_url"] = streamInfo.ProfileImageURL
	}
	if streamInfo.StartedAt != "" && job.StreamStartTime == "" {
		updates["stream_start_time"] = streamInfo.StartedAt
	}
	if streamInfo.GameCategory != "" {
		updates["twitch_category"] = streamInfo.GameCategory
	}
	if len(updates) > 0 {
		sp.db.UpdateJobFields(job.ID, updates)
	}

	// Get HLS variants
	variants, err := sp.tw.GetHLSMasterPlaylist(ctx, login)
	if err != nil {
		return nil, fmt.Errorf("twitch HLS: %w", err)
	}

	variant := sp.tw.SelectBestVariant(variants, job.TwitchQuality, sp.cfg.Downloader.MaxVideoResolution)
	if variant == nil {
		return &StreamProcessResult{ShouldDownload: false, Error: "no suitable HLS quality found"}, nil
	}

	sp.logger.Info("twitch live stream ready",
		"channel", login, "quality", variant.Name,
		"resolution", fmt.Sprintf("%dx%d", variant.Width, variant.Height))

	sp.db.UpdateJobFields(job.ID, map[string]any{
		"status":         database.StatusLive,
		"is_vod":         false,
		"twitch_quality": variant.Name,
	})

	// Send "Twitch Stream Live" notification
	if sp.notifier != nil {
		qualityLabel := variant.Name
		if variant.Height > 0 {
			qualityLabel = fmt.Sprintf("%s (%dp)", variant.Name, variant.Height)
		}
		fields := []notifications.Field{
			{Name: "Channel", Value: streamInfo.ChannelDisplayName, Inline: true},
			{Name: "Quality", Value: qualityLabel, Inline: true},
		}
		if streamInfo.GameCategory != "" {
			fields = append(fields, notifications.Field{Name: "Category", Value: streamInfo.GameCategory, Inline: true})
		}
		sp.notifier.Send("Twitch Stream Live",
			fmt.Sprintf("Now live: %s", job.Title),
			notifications.TypeSuccess,
			fields,
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: streamInfo.ThumbnailURL,
				Event:     "live",
			},
		)
	}

	// Start Twitch IRC chat downloader if chat recording is enabled
	var twitchChatDl *twitch.ChatDownloader
	if sp.cfg.Downloader.DownloadChat && sp.tw != nil {
		stagingBase := sp.cfg.Paths.StagingDirectory
		if stagingBase == "" {
			stagingBase = "./staging"
		}
		chatStagingDir := filepath.Join(stagingBase, job.ID)
		if err := os.MkdirAll(chatStagingDir, 0o755); err == nil {
			chatPath := filepath.Join(chatStagingDir, "chat.json")
			twitchChatDl = twitch.NewChatDownloader(twitch.ChatDownloaderOptions{
				ChannelLogin:    login,
				ChannelDisplay:  streamInfo.ChannelDisplayName,
				ChannelID:       streamInfo.ChannelID,
				StreamID:        streamInfo.StreamID,
				OutputPath:      chatPath,
				StreamStartTime: streamInfo.StartedAt,
				AuthToken:       sp.tw.GetAuthToken(),
				EmoteResolver:   sp.tw.Emotes,
			}, sp.logger)

			sp.db.UpdateJobFields(job.ID, map[string]any{
				"chat_status": "downloading",
			})
		}
	}

	return &StreamProcessResult{
		ShouldDownload:       true,
		IsVod:                false,
		TwitchStreamInfo:     streamInfo,
		TwitchVariant:        variant,
		TwitchChatDownloader: twitchChatDl,
	}, nil
}

// waitForTwitchLive polls a Twitch channel until it goes live or is cancelled.
// Returns (streamInfo, nil) when live, (nil, nil) when cancelled, (nil, err) on fatal error.
func (sp *StreamProcessor) waitForTwitchLive(ctx context.Context, job *database.Job, login string) (*twitch.TwitchStreamInfo, error) {
	sp.db.UpdateJobFields(job.ID, map[string]any{
		"status":          database.StatusUpcoming,
		"progress":        "Waiting for stream...",
		"last_recheck_at": time.Now().UTC().Format(time.RFC3339),
	})

	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
		}

		// Check if job was cancelled by user
		currentJob, err := sp.db.GetJob(job.ID)
		if err == nil && currentJob.Status == database.StatusCancelled {
			return nil, nil
		}

		// Sleep with jitter (15-20s effective)
		jitter := time.Duration(rand.Int63n(int64(twitchPollJitterMax)))
		pollTimer := time.NewTimer(twitchPollInterval + jitter)
		select {
		case <-ctx.Done():
			pollTimer.Stop()
			return nil, nil
		case <-pollTimer.C:
		}

		sp.db.UpdateJobFields(job.ID, map[string]any{
			"last_recheck_at": time.Now().UTC().Format(time.RFC3339),
		})

		streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			consecutiveErrors++
			sp.logger.Warn("twitch poll error", "channel", login, "err", err, "consecutive", consecutiveErrors)
			if consecutiveErrors >= maxConsecutiveProbeErrors {
				return nil, fmt.Errorf("max probe errors: %w", err)
			}
			continue
		}
		consecutiveErrors = 0

		if streamInfo != nil && streamInfo.IsLive {
			sp.logger.Info("twitch channel is now live", "channel", login)
			sp.db.UpdateJobFields(job.ID, map[string]any{
				"progress": "",
			})
			return streamInfo, nil
		}
	}
}

// extractTwitchLoginFromJob extracts the Twitch channel login from a job.
func extractTwitchLoginFromJob(job *database.Job) string {
	// Try URL first
	if job.URL != "" && strings.Contains(job.URL, "twitch.tv/") {
		parts := strings.Split(job.URL, "twitch.tv/")
		if len(parts) >= 2 {
			login := strings.Split(parts[1], "/")[0]
			login = strings.Split(login, "?")[0]
			if login != "" {
				return strings.ToLower(login)
			}
		}
	}

	// Try channel name (but not placeholder "Unknown")
	if job.ChannelName != "" && job.ChannelName != "Unknown" {
		return strings.ToLower(job.ChannelName)
	}

	// Try videoID (tw_manual_{login}_{timestamp}, tw_{login}, or tw_v{vodId})
	id := job.VideoID
	if strings.HasPrefix(id, "tw_manual_") {
		remainder := strings.TrimPrefix(id, "tw_manual_")
		if idx := strings.LastIndex(remainder, "_"); idx > 0 {
			return strings.ToLower(remainder[:idx])
		}
		return strings.ToLower(remainder)
	}
	if strings.HasPrefix(id, "tw_v") {
		return "" // VOD ID, not a login
	}
	if strings.HasPrefix(id, "tw_") {
		return strings.TrimPrefix(id, "tw_")
	}

	return ""
}

package worker

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

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

	if sp.cfgMu != nil {
		sp.cfgMu.RLock()
	}
	vodMaxRes := sp.cfg.Downloader.MaxVideoResolution
	vodDownloadChat := sp.cfg.Downloader.DownloadChat
	vodStagingDir := sp.cfg.Paths.StagingDirectory
	if sp.cfgMu != nil {
		sp.cfgMu.RUnlock()
	}

	variant := sp.tw.SelectBestVariant(variants, job.TwitchQuality, vodMaxRes)
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
	if vodDownloadChat {
		stagingDir := filepath.Join(vodStagingDir, job.ID)
		if vodStagingDir == "" {
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

	if sp.cfgMu != nil {
		sp.cfgMu.RLock()
	}
	liveMaxRes := sp.cfg.Downloader.MaxVideoResolution
	liveDownloadChat := sp.cfg.Downloader.DownloadChat
	liveStagingBase := sp.cfg.Paths.StagingDirectory
	if sp.cfgMu != nil {
		sp.cfgMu.RUnlock()
	}

	variant := sp.tw.SelectBestVariant(variants, job.TwitchQuality, liveMaxRes)
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

	// Start Twitch IRC chat downloader if chat recording is enabled
	var twitchChatDl *twitch.ChatDownloader
	if liveDownloadChat && sp.tw != nil {
		stagingBase := liveStagingBase
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
	if remainder, ok := strings.CutPrefix(id, "tw_manual_"); ok {
		if idx := strings.LastIndex(remainder, "_"); idx > 0 {
			return strings.ToLower(remainder[:idx])
		}
		return strings.ToLower(remainder)
	}
	if strings.HasPrefix(id, "tw_v") {
		return "" // VOD ID, not a login
	}
	if after, ok := strings.CutPrefix(id, "tw_"); ok {
		return after
	}

	return ""
}

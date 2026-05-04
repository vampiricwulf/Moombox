package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TwitchOfflineErrMsg is the literal error string emitted when a non-manual
// Twitch job's GetStreamInfo returns nil/!IsLive. Exported so the monitor's
// auto-recovery predicate can match on it without drifting from the producer.
// Keep these two sites aligned — a regression test in internal/monitor checks
// the predicate accepts exactly this string.
const TwitchOfflineErrMsg = "twitch channel is offline"

// twitchAuthSentinel returns ErrCookiesRequired when err is (or wraps)
// twitch.ErrTwitchAuthExpired so VOD/HLS errors that lost their wrap
// via %v formatting still get classified as auth-required by the
// downstream consumer. Returns nil for non-auth errors so plain
// failures don't get spuriously routed to StatusCookies.
func twitchAuthSentinel(err error) error {
	if errors.Is(err, twitch.ErrTwitchAuthExpired) {
		return ErrCookiesRequired
	}
	return nil
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
		return &StreamProcessResult{
			ShouldDownload: false,
			IsVod:          true,
			Error:          fmt.Sprintf("twitch VOD error: %v", err),
			ErrSentinel:    twitchAuthSentinel(err),
		}, nil
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
		return &StreamProcessResult{
			ShouldDownload: false,
			IsVod:          true,
			Error:          fmt.Sprintf("twitch VOD HLS error: %v", err),
			ErrSentinel:    twitchAuthSentinel(err),
		}, nil
	}

	var vodMaxRes int
	var vodDownloadChat bool
	var vodStagingDir string
	sp.readConfig(func(c *config.MoomboxConfig) {
		vodMaxRes = c.Downloader.MaxVideoResolution
		vodDownloadChat = c.Downloader.DownloadChat
		vodStagingDir = c.Paths.StagingDirectory
	})

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
	// Consume any monitor-stashed hint first — avoids a redundant GetStreamInfo
	// call that exposed us to transient Twitch StreamMetadata flaps where the
	// API briefly returned Stream=nil between two consecutive requests for the
	// same channel. take() is a no-op if no hint exists (manual add, user
	// reinit, app restart, etc.) and we fall back to a fresh fetch.
	streamInfo := sp.twitchHints.take(job.ID)
	if streamInfo != nil && !streamInfo.IsLive {
		// Defense-in-depth: producer should only stash live infos, but if a
		// non-live one slips through we'd rather refetch than log a misleading
		// "using hint" line and fall straight into the offline branch.
		sp.logger.Warn("twitch stashed hint is not live; refetching",
			"jobID", job.ID, "streamID", streamInfo.StreamID, "channel", login)
		streamInfo = nil
	}
	if streamInfo != nil {
		sp.logger.Debug("twitch using stashed monitor hint",
			"jobID", job.ID, "streamID", streamInfo.StreamID, "channel", login)
	}

	if streamInfo == nil {
		fetched, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			return nil, fmt.Errorf("twitch stream info: %w", err)
		}
		streamInfo = fetched
	}

	if streamInfo == nil || !streamInfo.IsLive {
		if job.ManuallyAdded {
			sp.logger.Info("twitch channel is offline, waiting for stream", "channel", login)
			waitInfo, err := sp.waitForTwitchLive(ctx, job, login)
			if err != nil {
				return nil, err
			}
			if waitInfo == nil {
				return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
			}
			streamInfo = waitInfo
			// Fall through to existing live handling below
		} else {
			sp.logger.Info(TwitchOfflineErrMsg, "channel", login)
			return &StreamProcessResult{ShouldDownload: false, Error: TwitchOfflineErrMsg}, nil
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

	var liveMaxRes int
	var liveDownloadChat bool
	var liveStagingBase string
	sp.readConfig(func(c *config.MoomboxConfig) {
		liveMaxRes = c.Downloader.MaxVideoResolution
		liveDownloadChat = c.Downloader.DownloadChat
		liveStagingBase = c.Paths.StagingDirectory
	})

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

		if sp.isOnline != nil && !sp.isOnline() {
			sp.logger.Debug("skipping Twitch probe — device offline", "login", login)
			continue
		}

		streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			consecutiveErrors++
			sp.logger.Warn("twitch poll error", "channel", login, "err", err, "consecutive", consecutiveErrors)
			if consecutiveErrors >= maxConsecutiveProbeErrors {
				// Wrap with ErrNonActionable so worker.setJobError suppresses
				// the user notification — the retry budget exhausted means
				// the stream isn't going to come up regardless of further
				// probes (audit cross-cutting.md C3 follow-up).
				return nil, fmt.Errorf("max probe errors: %w (%w)", err, ErrNonActionable)
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

package worker

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

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
	lastFullFetch := time.Now() // Initial full fetch just happened in Process()

	// B2: Start early chat downloader (only if chat download is enabled)
	if sp.cfgMu != nil {
		sp.cfgMu.RLock()
	}
	downloadChat := sp.cfg.Downloader.DownloadChat
	if sp.cfgMu != nil {
		sp.cfgMu.RUnlock()
	}
	var chatDl *chat.ChatDownloader
	if downloadChat {
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

		// Update local scheduledStartTime for interval calculation
		if probeInfo.ScheduledStartTime != "" {
			scheduledStartTime = probeInfo.ScheduledStartTime
		}
		// Persist any metadata changes (change-detected, zero-cost if nothing differs)
		sp.updateJobMetadata(job, probeInfo, true)

		// Periodic full WEB fetch to catch metadata ANDROID_VR can't see
		// (e.g., rescheduled start times only available in microformat).
		if time.Since(lastFullFetch) >= fullFetchInterval && probeInfo.StreamStatus == youtube.StreamUpcoming {
			fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
			if err == nil {
				sp.updateJobMetadata(job, fullInfo, true)
				if fullInfo.ScheduledStartTime != "" {
					scheduledStartTime = fullInfo.ScheduledStartTime
				}
				sp.logger.Debug("periodic full fetch completed", "videoID", job.VideoID)
			} else {
				sp.logger.Debug("periodic full fetch failed, will retry later", "videoID", job.VideoID, "err", err)
			}
			lastFullFetch = time.Now()
		}

		// B2: Try starting chat if not yet available
		if sp.cfgMu != nil {
			sp.cfgMu.RLock()
		}
		downloadChatRetry := sp.cfg.Downloader.DownloadChat
		if sp.cfgMu != nil {
			sp.cfgMu.RUnlock()
		}
		if chatDl == nil && downloadChatRetry {
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
			sp.logger.Info("stream is now live (probe)", "videoID", job.VideoID)
			fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
			if err != nil {
				sp.stopEarlyChat(chatDl)
				return nil, fmt.Errorf("full fetch on live: %w", err)
			}
			// Cross-check: if full WEB fetch says still upcoming, the probe was
			// fooled by YouTube's waiting room / offline slate. Keep polling.
			if fullInfo.StreamStatus == youtube.StreamUpcoming {
				sp.updateJobMetadata(job, fullInfo, true)
				if fullInfo.ScheduledStartTime != "" {
					scheduledStartTime = fullInfo.ScheduledStartTime
				}
				sp.logger.Info("full fetch says still upcoming — probe saw waiting room, continuing poll",
					"videoID", job.VideoID, "scheduledStart", scheduledStartTime)
				lastFullFetch = time.Now()
				continue
			}
			if fullInfo.ScheduledStartTime == "" && job.StreamStartTime == "" {
				fullInfo.ScheduledStartTime = time.Now().UTC().Format(time.RFC3339)
			}
			sp.updateJobMetadata(job, fullInfo, true)
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
			sp.logger.Info("stream became VOD (probe)", "videoID", job.VideoID)
			fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
			if err != nil {
				sp.stopEarlyChat(chatDl)
				return nil, fmt.Errorf("full fetch on VOD: %w", err)
			}
			// Cross-check: if full WEB fetch says still upcoming, keep polling.
			if fullInfo.StreamStatus == youtube.StreamUpcoming {
				sp.updateJobMetadata(job, fullInfo, true)
				if fullInfo.ScheduledStartTime != "" {
					scheduledStartTime = fullInfo.ScheduledStartTime
				}
				sp.logger.Info("full fetch says still upcoming — probe misclassified, continuing poll",
					"videoID", job.VideoID, "scheduledStart", scheduledStartTime)
				lastFullFetch = time.Now()
				continue
			}
			sp.updateJobMetadata(job, fullInfo, true)
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
					if fullInfo.ScheduledStartTime == "" && job.StreamStartTime == "" {
						fullInfo.ScheduledStartTime = time.Now().UTC().Format(time.RFC3339)
					}
					sp.updateJobMetadata(job, fullInfo, true)
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
					sp.updateJobMetadata(job, fullInfo, true)
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
	if sp.cfgMu != nil {
		sp.cfgMu.RLock()
	}
	stagingBase := sp.cfg.Paths.StagingDirectory
	if sp.cfgMu != nil {
		sp.cfgMu.RUnlock()
	}
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

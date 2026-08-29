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
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// waitForLive polls until a stream goes live or is cancelled.
// B2: Starts early chat downloader during upcoming phase to capture pre-stream chat.
// isTerminalPlayability reports whether a YouTube playability error means the
// video will not become available by waiting — so the upcoming-wait loop should
// give up. Deliberately narrow: members-only / login-required are excluded
// (B1 switches to the authenticated probe) and age-restricted is excluded
// (handled at initial Process / resolvable with auth). Only states that no
// amount of waiting or re-auth can fix are terminal.
func isTerminalPlayability(p youtube.PlayabilityError) bool {
	switch p {
	case youtube.PlayabilityPrivate, youtube.PlayabilityUnavailable, youtube.PlayabilityRegionBlocked:
		return true
	default:
		return false
	}
}

func (sp *StreamProcessor) waitForLive(ctx context.Context, job *database.Job, initialInfo *youtube.VideoInfo) (*StreamProcessResult, error) {
	sp.logger.Info("waiting for stream to go live", "videoID", job.VideoID)

	sp.db.UpdateJobFields(job.ID, map[string]any{
		"status":          database.StatusUpcoming,
		"last_recheck_at": time.Now().UTC().Format(time.RFC3339),
	})

	consecutiveErrors := 0
	var lastOfflineProbe time.Time // zero ⇒ first offline encounter probes immediately
	scheduledStartTime := initialInfo.ScheduledStartTime
	membersOnly := false
	lastFullFetch := time.Now() // Initial full fetch just happened in Process()

	// B2: Start early chat downloader (only if chat download is enabled)
	var downloadChat bool
	sp.readConfig(func(c *config.MoomboxConfig) {
		downloadChat = c.Downloader.DownloadChat
	})
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

	// Per audit reports/worker.md F16 — pass onProgress into tryStartEarlyChat so
	// the wiring is centralized and can't drift between the initial start and
	// the in-loop retry path below.
	var chatDl *chat.ChatDownloader
	if downloadChat {
		chatDl = sp.tryStartEarlyChat(ctx, job, initialInfo, chatProgressFn)
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

		// A5: probe interval with PROPORTIONAL jitter (~10% of the interval)
		// to desync polling. Flat 0-30s jitter previously DOUBLED the tight
		// imminent tier (30s → 30-60s), throwing away the tightening.
		info := &youtube.VideoInfo{ScheduledStartTime: scheduledStartTime}
		interval := sp.calculateProbeInterval(info)
		jitterMax := interval / 10
		if jitterMax < time.Second {
			jitterMax = time.Second
		}
		jitter := time.Duration(rand.Int63n(int64(jitterMax)))

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

		// When the oracle reports offline, still probe occasionally (floor) so
		// a wrongly-offline oracle can't strand a waiting stream. Safe because
		// network-class errors no longer count (see applyProbeError), and a
		// success self-corrects the oracle via reportProbeResult.
		if sp.isOnline != nil && !sp.isOnline() {
			if time.Since(lastOfflineProbe) < offlineProbeFloor {
				sp.logger.Debug("skipping probe — device offline (within floor)", "videoID", job.VideoID)
				continue
			}
			lastOfflineProbe = time.Now()
		}

		// Probe — use lightweight authenticated probe if members-only was detected
		var probeInfo *youtube.VideoInfo
		var probeErr error
		if membersOnly {
			probeInfo, probeErr = sp.yt.ProbeVideoStatusAuthenticated(ctx, job.VideoID)
		} else {
			probeInfo, probeErr = sp.yt.ProbeVideoStatus(ctx, job.VideoID)
		}
		if probeErr != nil {
			newCount, giveUp, report, cancelled := applyProbeError(probeErr, consecutiveErrors)
			switch report {
			case reportFailure:
				reportProbeResult("probe/youtube", true)
			case reportSuccess:
				reportProbeResult("probe/youtube", false)
			}
			if cancelled {
				sp.stopEarlyChat(chatDl)
				return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
			}
			consecutiveErrors = newCount // unchanged for network-class failures
			if report == reportFailure {
				// Network-class failure: the internet/service is unreachable.
				// Keep waiting through the outage — the count did not advance.
				sp.logger.Debug("probe network error — not counting, still waiting", "videoID", job.VideoID, "err", probeErr)
			} else {
				sp.logger.Warn("probe error (definitive)", "videoID", job.VideoID, "err", probeErr, "consecutive", consecutiveErrors)
			}
			if giveUp {
				sp.stopEarlyChat(chatDl)
				// Wrap with ErrNonActionable so worker.setJobError suppresses
				// the user notification — exhausted DEFINITIVE retries mean
				// the stream isn't coming up regardless of further work.
				return nil, fmt.Errorf("max probe errors: %w (%w)", probeErr, ErrNonActionable)
			}
			continue
		}
		consecutiveErrors = 0
		// Successful probe (HTTP 200 reached the service) — feed the oracle so a
		// wrongly-offline passive latch can self-correct (Layer 4).
		reportProbeResult("probe/youtube", false)

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
				// A scheduled stream can be made private / removed / region-blocked
				// while still reporting StreamUpcoming (videoDetails.isUpcoming
				// stays true even as playabilityStatus flips terminal). The
				// authoritative full WEB fetch surfaces that terminal playability —
				// give up rather than polling forever. Decided ONLY on the full
				// fetch (never the lightweight probe) so a misclassification can't
				// wrongly error a waiting stream. Members-only / login are NOT
				// terminal here — B1 above switches to the authenticated probe.
				if isTerminalPlayability(fullInfo.PlayabilityError) {
					sp.stopEarlyChat(chatDl)
					reason := fullInfo.PlayabilityReason
					if reason == "" {
						reason = string(fullInfo.PlayabilityError)
					}
					sp.logger.Info("upcoming stream became unavailable — giving up",
						"videoID", job.VideoID, "playability", fullInfo.PlayabilityError, "reason", reason)
					// The full fetch reached the service (HTTP 200) — report
					// reachability to the oracle even though the video itself is
					// terminal; a terminal playability is an application-level
					// verdict, not a connectivity failure (Layer 4).
					reportProbeResult("probe/youtube", false)
					return &StreamProcessResult{
						VideoInfo:      fullInfo,
						ShouldDownload: false,
						Error:          fmt.Sprintf("stream unavailable: %s", reason),
						ErrSentinel:    ErrNonActionable,
					}, nil
				}
				sp.logger.Debug("periodic full fetch completed", "videoID", job.VideoID)
			} else {
				reportFetchOutcome(err, "probe/youtube")
				sp.logger.Debug("periodic full fetch failed, will retry later", "videoID", job.VideoID, "err", err)
			}
			lastFullFetch = time.Now()
		}

		// B2: Try starting chat if not yet available
		var downloadChatRetry bool
		sp.readConfig(func(c *config.MoomboxConfig) {
			downloadChatRetry = c.Downloader.DownloadChat
		})
		if chatDl == nil && downloadChatRetry {
			// onProgress wired inside tryStartEarlyChat — see F16.
			chatDl = sp.tryStartEarlyChat(ctx, job, probeInfo, chatProgressFn)
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
				// Confirmatory full fetch failed — most likely a transient
				// connectivity blip in the brief window after a successful probe.
				// Do NOT error the stream (that would lose a live recording over a
				// blip); report reachability to the oracle and keep polling — the
				// lightweight probe loop owns the give-up budget, and the next poll
				// re-attempts the fetch once connectivity returns.
				reportFetchOutcome(err, "probe/youtube")
				sp.logger.Debug("full fetch on live failed; will retry next poll", "videoID", job.VideoID, "err", err)
				continue
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
			return sp.completeStreamTransition(job, fullInfo, chatDl), nil

		case youtube.StreamVOD, youtube.StreamPostLive:
			sp.logger.Info("stream became VOD (probe)", "videoID", job.VideoID)
			fullInfo, err := sp.yt.GetVideoInfo(ctx, job.VideoID)
			if err != nil {
				// Same connectivity-resilience as the live case: don't error the
				// stream on a transient confirmatory-fetch failure; report
				// reachability and keep polling (re-attempt on the next poll).
				reportFetchOutcome(err, "probe/youtube")
				sp.logger.Debug("full fetch on VOD failed; will retry next poll", "videoID", job.VideoID, "err", err)
				continue
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
			return sp.completeStreamTransition(job, fullInfo, chatDl), nil

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
					reportFetchOutcome(err, "probe/youtube")
					sp.logger.Warn("full fetch after unclear auth probe failed", "err", err)
					continue // Retry on next iteration
				}
				switch fullInfo.StreamStatus {
				case youtube.StreamLive, youtube.StreamVOD, youtube.StreamPostLive:
					return sp.completeStreamTransition(job, fullInfo, chatDl), nil
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

// completeStreamTransition finalises the probe-resolution flow once fullInfo
// says the stream is Live or VOD/PostLive: applies the metadata update,
// flips the job status (Live gets StatusLive+ScheduledStartTime defaulting;
// VOD/PostLive just flips is_vod), untracks the early chat so the
// orchestrator can own it, and returns the StreamProcessResult. Shared
// between the primary probe path and the auth-probe-unclear fallback
// (audit reports/worker.md F55).
func (sp *StreamProcessor) completeStreamTransition(job *database.Job, fullInfo *youtube.VideoInfo, chatDl *chat.ChatDownloader) *StreamProcessResult {
	isVod := fullInfo.StreamStatus == youtube.StreamVOD || fullInfo.StreamStatus == youtube.StreamPostLive

	if !isVod {
		// Live path: default ScheduledStartTime to now when neither the fetch
		// nor the existing job row carries one.
		if fullInfo.ScheduledStartTime == "" && job.StreamStartTime == "" {
			fullInfo.ScheduledStartTime = time.Now().UTC().Format(time.RFC3339)
		}
	}
	sp.updateJobMetadata(job, fullInfo, true)

	fields := map[string]any{"is_vod": isVod}
	if !isVod {
		fields["status"] = database.StatusLive
	}
	sp.db.UpdateJobFields(job.ID, fields)

	if chatDl != nil {
		sp.untrackChat(chatDl)
	}
	return &StreamProcessResult{
		VideoInfo:      fullInfo,
		ShouldDownload: true,
		IsVod:          isVod,
		ChatDownloader: chatDl,
	}
}

// tryStartEarlyChat attempts to start a chat downloader during the upcoming phase (B2).
// onProgress is wired before Start so callers don't need a follow-up SetOnProgress
// (per audit reports/worker.md F16 — keeps initial+retry paths in sync).
func (sp *StreamProcessor) tryStartEarlyChat(ctx context.Context, job *database.Job, info *youtube.VideoInfo, onProgress func(chat.ChatProgress)) *chat.ChatDownloader {
	// Fetch watch page to get chat continuation token. One-shot call, so a
	// snapshot is correct here; the chat downloader below gets a live getter.
	cookieHeader := ""
	if sp.yt != nil && sp.yt.Auth != nil {
		cookieHeader = sp.yt.Auth.GetCookieHeader()
	}

	watchResult, err := youtube.FetchWatchPage(ctx, job.VideoID, cookieHeader)
	if err != nil {
		sp.logger.Debug("failed to fetch watch page for early chat", "err", err, "videoID", job.VideoID)
		return nil
	}

	continuation := watchResult.ChatContinuation
	isReplay := watchResult.ChatIsReplay
	if continuation == "" {
		sp.logger.Debug("no chat continuation for early chat", "videoID", job.VideoID, "err", watchResult.ChatErr)
		return nil
	}

	visitorData := ""
	if watchResult.Ytcfg != nil {
		visitorData = watchResult.Ytcfg.VisitorData
	}

	// Create staging dir for early chat output (matches TypeScript behavior)
	var stagingBase string
	sp.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.StagingDirectory
	})
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
		IsReplay:            isReplay,
		IsLiveOrUpcoming:    true,
	}
	if sp.yt != nil && sp.yt.Auth != nil {
		opts.GenerateAuth = sp.yt.Auth.GenerateAuthorizationHeader
		// Method value, exactly like GenerateAuth above — re-read per poll so
		// a multi-hour early-chat run tracks the rotating cookie jar. nil Auth
		// leaves the field nil, which the API reads as "no Cookie header".
		opts.CookieHeader = sp.yt.Auth.GetCookieHeader
	}
	if info.ScheduledStartTime != "" {
		opts.StreamStartTime = info.ScheduledStartTime
	}

	dl := chat.NewChatDownloader(opts)
	if onProgress != nil {
		// Wire OnProgress before Start so the surge-detection callback fires
		// from the very first batch (F16).
		dl.SetOnProgress(onProgress)
	}
	dl.OnError = func(err error) {
		sp.logger.Warn("[Chat] Early chat API error", "jobID", job.ID, "err", err)
	}
	// Transition chat_status from "pending" -> "downloading" only when chat
	// actually starts receiving data (matches setupChatDownloader behavior).
	// Previously we wrote "downloading" eagerly before Start, so a chat API
	// that failed immediately would leave the UI stuck at "downloading"
	// (per audit reports/worker.md Finding 22).
	dl.OnStart = func(messageCount int, resuming bool) {
		sp.db.UpdateJobFields(job.ID, map[string]any{
			"chat_status": "downloading",
		})
	}
	sp.trackChat(dl)

	sp.db.UpdateJobFields(job.ID, map[string]any{
		"chat_status": "pending",
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

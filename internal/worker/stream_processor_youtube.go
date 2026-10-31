package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
// amount of waiting or re-auth can fix are terminal — never rate_limited,
// which waiting is exactly what fixes (rate_limit.go).
func isTerminalPlayability(p youtube.PlayabilityError) bool {
	switch p {
	case youtube.PlayabilityPrivate, youtube.PlayabilityUnavailable, youtube.PlayabilityRegionBlocked:
		return true
	default:
		return false
	}
}

// earlyChatNeedsRestart reports whether the upcoming-wait loop should start a
// new early-chat downloader on this probe: either none has been started yet,
// or the one it has has finished its run and lastRestart is at least
// earlyChatMinRestartInterval behind now.
//
// The finished arm is the fix for the waiting-room wipe. YouTube resets a
// waiting-room chat after a period of inactivity; the run then exhausts
// recoverStaleContinuation's ~35-minute budget and leaves. The downloader
// stays non-nil, so a `chatDl == nil` gate never restarted it and nothing
// captured the waiting room again until the process restarted.
//
// finished must be recorded by the goroutine that ran the downloader (see
// runEarlyChat), NOT by IsRunning(): IsRunning is also false in the window
// between NewChatDownloader and that goroutine reaching Start, so it would
// restart a downloader that has not begun yet.
//
// The interval applies ONLY to the finished arm. Having no downloader at all
// is the pre-existing "chat is not available yet" retry, which runs at the
// wait loop's own probe cadence and must not be slowed.
func earlyChatNeedsRestart(chatDl *chat.ChatDownloader, finished bool, lastRestart, now time.Time) bool {
	if chatDl == nil {
		return true
	}
	if !finished {
		return false
	}
	return now.Sub(lastRestart) >= earlyChatMinRestartInterval
}

// runEarlyChat runs one early-chat run to completion and records that it
// ended, however it ended.
//
// The flag is set from THIS defer rather than from ChatDownloader.OnFinish
// because Start's own recovery defer returns before OnFinish is reached: a run
// that panicked never fired it, so the downloader sat non-nil and dead for the
// rest of the wait and was never restarted — the exact shape
// earlyChatNeedsRestart exists to eliminate, reached through a different door.
// Registered before the recover defer so it runs last and cannot be skipped.
func runEarlyChat(finished *atomic.Bool, onPanic func(r any), run func()) {
	defer finished.Store(true)
	defer func() {
		if r := recover(); r != nil {
			onPanic(r)
		}
	}()
	run()
}

// fullFetchSaysRemoved reports whether the waiting loop's full fetch answered
// with YouTube's refusal of a video that is gone, in words the keyword rules
// do not place: playabilityStatus.status ERROR — the status YouTube files a
// removed, terminated or copyright-struck video under, and the one yt-dlp's
// _is_error_response names (_video.py:2912 at b77c9304e) — with a reason
// parsePlayabilityStatus can only call unknown ("This video has been removed
// for violating YouTube's Terms of Service", "… the YouTube account associated
// with this video has been terminated", "… due to a copyright claim", "…
// removed by the uploader"), and no broadcast left to classify.
//
// isTerminalPlayability gives up on the verdicts the rules do place (private,
// unavailable, region_blocked); this is the rest of a gone video. Before G8
// the status probe's own not_a_stream ended such a job on the next poll
// ("unexpected status: not_a_stream"); the probe now calls that refusal
// inconclusive, as blind as the bot wall's (internal/youtube/status_probe.go),
// and the full fetch is the authority left — which, reading only the placed
// verdicts, let the job poll every 30 s past its start for good. yt-dlp ends
// an extraction on any refusal that yields no formats (raise_no_formats,
// _video.py:4084); a loop waiting for a broadcast cannot take an auth wall
// that way, which is why only an ERROR, and only one whose verdict the rules
// left unknown, counts: a sign-in wall, a members-only or an age-gated
// refusal is placed, never unknown, and keeps the loop waiting as before, as
// does an answer that still states a broadcast (any status but not_a_stream).
func fullFetchSaysRemoved(info *youtube.VideoInfo) bool {
	return info != nil && info.StreamStatus == youtube.StreamNotAStream &&
		info.PlayabilityStatus == "ERROR" && info.PlayabilityError == youtube.PlayabilityUnknown
}

// fullFetchSaysGone is the waiting loop's give-up on a full fetch: a verdict
// the keyword rules place as terminal (isTerminalPlayability), or an ERROR
// refusal they cannot place that states no broadcast (fullFetchSaysRemoved).
func fullFetchSaysGone(info *youtube.VideoInfo) bool {
	return isTerminalPlayability(info.PlayabilityError) || fullFetchSaysRemoved(info)
}

// giveUpUnavailable ends the wait on a full fetch that says the stream is
// gone (fullFetchSaysGone): the early chat stopped, the reason logged, and a
// non-actionable result naming it.
func (sp *StreamProcessor) giveUpUnavailable(job *database.Job, fullInfo *youtube.VideoInfo, chatDl *chat.ChatDownloader) *StreamProcessResult {
	sp.stopEarlyChat(chatDl)
	reason := fullInfo.PlayabilityReason
	if reason == "" {
		reason = string(fullInfo.PlayabilityError)
	}
	sp.logger.Info("upcoming stream became unavailable — giving up",
		"videoID", job.VideoID, "playability", fullInfo.PlayabilityError, "reason", reason)
	// The full fetch reached the service (HTTP 200) — report reachability to
	// the oracle even though the video itself is terminal; a terminal
	// playability is an application-level verdict, not a connectivity
	// failure (Layer 4).
	reportProbeResult("probe/youtube", false)
	return &StreamProcessResult{
		VideoInfo:      fullInfo,
		ShouldDownload: false,
		Error:          fmt.Sprintf("stream unavailable: %s", reason),
		ErrSentinel:    ErrNonActionable,
	}
}

// waitingRoomAuthFlip is the waiting-room poll's B1 decision: whether this
// probe answer means the cookieless ANDROID_VR probe can no longer see the
// stream and the poll must switch to the cookied TV one for the rest of the
// wait.
//
// It is isAuthWalledPlayability — the SAME predicate `interruptionSignal.observe`
// and `runLiveStreamDownload`'s requiresAuthProbe use — rather than its own
// disjunction, because the two drifted: B1 listed members_only and
// login_required only, so an AGE-RESTRICTED stream kept polling cookielessly
// for the whole wait and never used the cookies from the age-verified account
// that are the documented remedy (close-review Finding 18). One predicate is
// what stops that happening again.
//
// An answer the cookieless probe's signed-in escalation gave in its place
// (youtube.StatusProbeEscalationTVAuth) switches too, whatever its verdict:
// ANDROID_VR refused with nothing to say about the broadcast — the bot wall,
// above all — and the TV call that saw past it is the cookied probe this
// switch moves to. Before G8 that refusal reached here as ANDROID_VR's own
// login_required and flipped; with the escalation's answer in its place it
// did not, and every poll for the rest of the wait paid ANDROID_VR and TV
// both.
//
// hasCookies is passed in rather than read here so the decision is testable
// without a network-capable *youtube.Service.
func waitingRoomAuthFlip(info *youtube.VideoInfo, hasCookies bool) bool {
	return info != nil && hasCookies &&
		(isAuthWalledPlayability(info.PlayabilityError) || info.StatusProbeEscalation == youtube.StatusProbeEscalationTVAuth)
}

// fetchVideoInfo is the full multi-client fetch, Process's and the waiting
// loop's: yt.GetVideoInfo, or getVideoInfoFn when a test set one.
func (sp *StreamProcessor) fetchVideoInfo(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
	if sp.getVideoInfoFn != nil {
		return sp.getVideoInfoFn(ctx, videoID)
	}
	return sp.yt.GetVideoInfo(ctx, videoID)
}

// probeVideoStatus is the waiting loop's status probe: the cookied one once
// the loop has switched to it (authenticated), the cookieless ANDROID_VR one
// before — or probeStatusFn when a test set one.
//
// The cookieless one does not hand the loop a refusal that says nothing about
// the broadcast (the bot wall, a made-for-kids video): it escalates once, and
// when that settles nothing either it returns a
// *youtube.StatusProbeInconclusiveError, which classifyProbeErr makes
// network-class, so the loop waits through it, its full fetch and schedule
// probe still running (the error branch below). It used to come back as
// not_a_stream, and the switch's default arm ended the job "unexpected
// status: not_a_stream" on the first poll (G8).
func (sp *StreamProcessor) probeVideoStatus(ctx context.Context, videoID string, authenticated bool) (*youtube.VideoInfo, error) {
	if sp.probeStatusFn != nil {
		return sp.probeStatusFn(ctx, videoID, authenticated)
	}
	if authenticated {
		return sp.yt.ProbeVideoStatusAuthenticated(ctx, videoID)
	}
	return sp.yt.ProbeVideoStatus(ctx, videoID)
}

// probeVideoSchedule is the waiting loop's schedule probe: yt.ProbeVideoSchedule,
// or probeScheduleFn when a test set one.
func (sp *StreamProcessor) probeVideoSchedule(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
	if sp.probeScheduleFn != nil {
		return sp.probeScheduleFn(ctx, videoID)
	}
	return sp.yt.ProbeVideoSchedule(ctx, videoID)
}

func (sp *StreamProcessor) waitForLive(ctx context.Context, job *database.Job, initialInfo *youtube.VideoInfo) (*StreamProcessResult, error) {
	sp.logger.Info("waiting for stream to go live", "videoID", job.VideoID)

	// Conditional: an operator's Cancel can land after Process read the row
	// — after a run that took its "Stream Found" sent it — and this write
	// used to turn that Cancelled row back into Upcoming, until
	// handleCancellation wrote Cancelled again. A write it refuses changes
	// nothing: the loop stops on the cancelled ctx, or, while the Cancel has
	// yet to cancel it, on the Cancelled row its first check reads.
	sp.db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{
		"status":          database.StatusUpcoming,
		"last_recheck_at": time.Now().UTC().Format(time.RFC3339),
	})

	consecutiveErrors := 0
	var lastOfflineProbe time.Time // zero ⇒ first offline encounter probes immediately
	scheduledStartTime := initialInfo.ScheduledStartTime
	membersOnly := false
	lastFullFetch := time.Now() // Initial full fetch just happened in Process()
	lastScheduleProbe := lastFullFetch
	var rateLimit loopRateLimit // a rate limit the full fetches met (rate_limit.go)
	defer sp.endRateLimit(job, &rateLimit)

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
	// the in-loop retry path below. chatFinished is set by runEarlyChat's defer
	// when the run goroutine returns, and with chatStartedAt is what
	// earlyChatNeedsRestart consults on each probe; both are REPLACED (never
	// reused) whenever a new downloader is started, so they always describe the
	// current run. chatDl too is reassigned in the loop, so every
	// sp.stopEarlyChat(chatDl) exit below stops whichever downloader is current.
	var chatDl *chat.ChatDownloader
	var chatFinished *atomic.Bool
	var chatStartedAt time.Time
	if downloadChat {
		chatDl, chatFinished = sp.tryStartEarlyChat(ctx, job, initialInfo, chatProgressFn)
		chatStartedAt = time.Now()
	}

	for {
		select {
		case <-ctx.Done():
			sp.stopEarlyChat(chatDl)
			return cancelledResult(), nil
		default:
		}

		// Check if job was cancelled — or deleted: a channel prune removes
		// Upcoming rows in bulk without OnJobDeleted, and GetJob returns
		// (nil, nil) for a missing row, which this used to dereference.
		currentJob, err := sp.db.GetJob(job.ID)
		if err == nil && (currentJob == nil || currentJob.Status == database.StatusCancelled) {
			sp.stopEarlyChat(chatDl)
			return cancelledResult(), nil
		}

		// A5: probe interval with PROPORTIONAL jitter (~10% of the interval)
		// to desync polling. Flat 0-30s jitter previously DOUBLED the tight
		// imminent tier (30s → 30-60s), throwing away the tightening.
		info := &youtube.VideoInfo{ScheduledStartTime: scheduledStartTime}
		interval := sp.calculateProbeInterval(info)
		jitterMax := interval / 10
		jitterMax = max(jitterMax, time.Second)
		jitter := time.Duration(rand.Int63n(int64(jitterMax)))

		// B2: Race sleep against chat surge
		probeTimer := time.NewTimer(interval + jitter)
		select {
		case <-ctx.Done():
			probeTimer.Stop()
			sp.stopEarlyChat(chatDl)
			return cancelledResult(), nil
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
		probeInfo, probeErr := sp.probeVideoStatus(ctx, job.VideoID, membersOnly)
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
				return cancelledResult(), nil
			}
			counted := newCount != consecutiveErrors
			consecutiveErrors = newCount // unchanged for network-class failures
			if !counted {
				// Network-class failure: a transport error, a transient
				// answer (429, 403, 5xx), or a status probe that could not
				// tell (an inconclusive one). Keep waiting through it — the
				// count did not advance. What the oracle heard is probeVote's
				// call.
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
			// A status probe that could not tell (G8) is waited through,
			// not waited out blind: the rest of this tick runs on a stand-in
			// that says nothing but "still waiting", so the calls that can
			// still see the broadcast keep their cadence — the periodic full
			// fetch, the authority that gives up on a stream made private or
			// removed (whose refusal is as blind as the bot wall's, and
			// which the default arm used to end on the probe's
			// not_a_stream), and the WEB schedule probe, which can see the
			// stream begin while ANDROID_VR cannot. A plain continue here
			// would skip both for as long as the wall stood, and a removed
			// stream's job would wait for good.
			if _, blind := errors.AsType[*youtube.StatusProbeInconclusiveError](probeErr); !blind {
				continue
			}
			probeInfo = &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming}
		} else {
			consecutiveErrors = 0
			// Successful probe (HTTP 200 reached the service) — feed the oracle so a
			// wrongly-offline passive latch can self-correct (Layer 4).
			reportProbeResult("probe/youtube", false)
		}

		// Update local scheduledStartTime for interval calculation
		if probeInfo.ScheduledStartTime != "" {
			scheduledStartTime = probeInfo.ScheduledStartTime
		}
		// Persist any metadata changes (change-detected, zero-cost if nothing differs)
		sp.updateJobMetadata(job, probeInfo, true)

		// Periodic full WEB fetch to catch metadata ANDROID_VR can't see
		// (e.g., rescheduled start times only available in microformat).
		if time.Since(lastFullFetch) >= fullFetchInterval && probeInfo.StreamStatus == youtube.StreamUpcoming {
			fullInfo, err := sp.fetchVideoInfo(ctx, job.VideoID)
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
				// Neither is an ERROR refusal the keyword rules could not place,
				// unless it says no broadcast is left (fullFetchSaysRemoved):
				// that is a video gone for a reason they have no word for, which
				// nothing else ends now that the status probe calls its own
				// refusal of it inconclusive (G8).
				if fullFetchSaysGone(fullInfo) {
					return sp.giveUpUnavailable(job, fullInfo, chatDl), nil
				}
				sp.logger.Debug("periodic full fetch completed", "videoID", job.VideoID)
				// It carries the schedule too, so the schedule probe below is
				// not due again for a full interval.
				lastScheduleProbe = time.Now()
				// On this tick the fetch is the WEB view the schedule probe
				// would have been, so a stream it sees begun goes to the switch
				// as the probe's answer would (scheduleProbeOvertakes, below).
				if scheduleProbeOvertakes(fullInfo) {
					sp.logger.Info("periodic full fetch says the stream has begun ahead of the status probe; confirming",
						"videoID", job.VideoID, "status", fullInfo.StreamStatus)
					probeInfo = fullInfo
				}
			} else {
				reportFetchOutcome(err, "probe/youtube")
				sp.logger.Debug("periodic full fetch failed, will retry later", "videoID", job.VideoID, "err", err)
			}
			lastFullFetch = time.Now()
			if err == nil {
				sp.noteFetch(job, &rateLimit, fullInfo, false)
			}
		}

		// Reschedule check. The status probe above carries no schedule, so a
		// start that moved was seen only by the full fetch, every 30 min; one
		// cheap WEB call on its own cadence (scheduleProbeInterval, tighter as
		// the start nears) sees it within minutes. Schedule only, and only
		// while the answer still says upcoming: the full fetch owns every
		// other field; a stream that has just begun carries its ACTUAL start
		// in the same field, which is the go-live path's to write, not a
		// reschedule to announce; and a probe that reports no schedule —
		// refused, or none set — changes nothing. The clock advances before
		// the call, so a failing probe is not retried every 30 s.
		//
		// It asks AFTER the full fetch, whose success resets its clock: asked
		// first, on every tick the fetch came due it read the schedule the
		// fetch then read again — a cookied WEB call every half hour for
		// nothing once the start was six hours out and the probe's cap met
		// the fetch's interval. A fetch that fails leaves it due, so it asks
		// on that tick in the fetch's place.
		//
		// An answer that says the stream has BEGUN is handed to the switch
		// below in the status probe's place (scheduleProbeOvertakes): the WEB
		// view can run ahead of ANDROID_VR, whose waiting-room slate can read
		// as upcoming for a while after go-live, and the live and VOD arms
		// confirm with a full fetch and keep polling when it disagrees — so a
		// wrong answer costs one full fetch per probe, which the status probe
		// itself can already cost every 30 s. Never any other answer: a
		// refused WEB call reads not_a_stream, and the default arm ends the
		// job on that.
		if probeInfo.StreamStatus == youtube.StreamUpcoming {
			if iv := scheduleProbeInterval(scheduledStartTime, time.Now()); iv > 0 && time.Since(lastScheduleProbe) >= iv {
				lastScheduleProbe = time.Now()
				if sched, err := sp.probeVideoSchedule(ctx, job.VideoID); err != nil {
					reportFetchOutcome(err, "probe/youtube")
					sp.logger.Debug("schedule probe failed; next full fetch covers it", "videoID", job.VideoID, "err", err)
				} else if scheduleProbeOvertakes(sched) {
					sp.logger.Info("schedule probe says the stream has begun ahead of the status probe; confirming",
						"videoID", job.VideoID, "status", sched.StreamStatus)
					probeInfo = sched
				} else if sched.IsUpcoming && !sched.IsLive && sched.ScheduledStartTime != "" {
					sp.updateJobMetadata(job, &youtube.VideoInfo{ScheduledStartTime: sched.ScheduledStartTime, IsUpcoming: true}, true)
					scheduledStartTime = sched.ScheduledStartTime
				}
			}
		}

		// B2: Try starting chat if not yet available — or RESTART the one whose
		// run has ended. A waiting-room chat that YouTube reset after a period
		// of inactivity leaves its downloader non-nil but finished; without the
		// restart nothing captures the waiting room again for the rest of the
		// wait. The new run loads the resume sidecar the finished one kept (or
		// adopts chat.json outright) and appends, so no history is lost.
		var downloadChatRetry bool
		sp.readConfig(func(c *config.MoomboxConfig) {
			downloadChatRetry = c.Downloader.DownloadChat
		})
		if downloadChatRetry && earlyChatNeedsRestart(chatDl, chatFinished != nil && chatFinished.Load(), chatStartedAt, time.Now()) {
			if chatDl != nil {
				sp.logger.Info("restarting early chat download after its run ended",
					"videoID", job.VideoID, "messages", chatDl.MessageCount())
				sp.untrackChat(chatDl)
			}
			// onProgress wired inside tryStartEarlyChat — see F16.
			chatDl, chatFinished = sp.tryStartEarlyChat(ctx, job, probeInfo, chatProgressFn)
			chatStartedAt = time.Now()
		}

		// B1: the wall went up mid-wait — switch to the cookied probe.
		if !membersOnly && waitingRoomAuthFlip(probeInfo, sp.yt.Auth.HasAuthCookies()) {
			sp.logger.Info("stream is auth-walled, switching to the authenticated probe",
				"videoID", job.VideoID, "playability", string(probeInfo.PlayabilityError))
			membersOnly = true
		}

		sp.tickRateLimit(job, &rateLimit, time.Now())
		switch probeInfo.StreamStatus {
		case youtube.StreamLive:
			if rateLimit.confirmationHeld() {
				sp.logger.Debug("stream is live (probe); its confirmation waits out a rate limit", "videoID", job.VideoID)
				continue
			}
			sp.logger.Info("stream is now live (probe)", "videoID", job.VideoID)
			fullInfo, err := sp.fetchVideoInfo(ctx, job.VideoID)
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
			// A rate limit says nothing about the stream: keep waiting, and
			// confirm again once it has been waited out (loopRateLimit).
			if sp.noteFetch(job, &rateLimit, fullInfo, true) {
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
			if rateLimit.confirmationHeld() {
				sp.logger.Debug("stream became VOD (probe); its confirmation waits out a rate limit", "videoID", job.VideoID)
				continue
			}
			sp.logger.Info("stream became VOD (probe)", "videoID", job.VideoID)
			fullInfo, err := sp.fetchVideoInfo(ctx, job.VideoID)
			if err != nil {
				// Same connectivity-resilience as the live case: don't error the
				// stream on a transient confirmatory-fetch failure; report
				// reachability and keep polling (re-attempt on the next poll).
				reportFetchOutcome(err, "probe/youtube")
				sp.logger.Debug("full fetch on VOD failed; will retry next poll", "videoID", job.VideoID, "err", err)
				continue
			}
			// A rate limit, as above.
			if sp.noteFetch(job, &rateLimit, fullInfo, true) {
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
				fullInfo, err := sp.fetchVideoInfo(ctx, job.VideoID)
				if err != nil {
					reportFetchOutcome(err, "probe/youtube")
					sp.logger.Warn("full fetch after unclear auth probe failed", "err", err)
					continue // Retry on next iteration
				}
				// The same give-up as the periodic full fetch's: this arm runs a
				// full fetch on every poll the cookied probe cannot read, and a
				// stream made private or removed under it — a members-only one,
				// or, since B1 switches on a TV escalation's answer too, a
				// public one behind a bot wall — was polled here for good, a
				// full fetch every 30 s.
				if fullFetchSaysGone(fullInfo) {
					return sp.giveUpUnavailable(job, fullInfo, chatDl), nil
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
	// The playability verdict the initial Process acts on, re-read on the
	// go-live fetch — but only its credential cases, and only when the fetch
	// came back with no formats to download. A members-only job whose cookies
	// died during a long wait (or whose wall went up mid-wait) used to go on
	// from here to "no download strategy available": a plain Error with no
	// "Authentication Required" alert, which the credential-recovery sweep —
	// it resumes COOKIES? rows only — never looked at again.
	if errMsg, sentinel := sp.checkPlayability(fullInfo); errMsg != "" && credentialVerdict(fullInfo, sentinel) {
		sp.logger.Warn("playability check failed at go-live",
			"videoID", job.VideoID,
			"playability", string(fullInfo.PlayabilityError),
			"sessionAuth", string(fullInfo.SessionAuth),
			"reason", errMsg)
		// What the fetch says about the job still reaches the row, as on the
		// initial Process's park: blanks filled, nothing overwritten (and no
		// schedule notification for a job that is not going to record).
		sp.updateJobMetadata(job, fullInfo, false)
		sp.stopEarlyChat(chatDl)
		return &StreamProcessResult{
			VideoInfo:      fullInfo,
			ShouldDownload: false,
			Error:          errMsg,
			ErrSentinel:    sentinel,
		}
	}

	isVod := fullInfo.StreamStatus == youtube.StreamVOD || fullInfo.StreamStatus == youtube.StreamPostLive

	if !isVod {
		// Live path: default ScheduledStartTime to now when neither the fetch
		// nor the existing job row carries one.
		if fullInfo.ScheduledStartTime == "" && job.StreamStartTime == "" {
			fullInfo.ScheduledStartTime = time.Now().UTC().Format(time.RFC3339)
		}
	}
	sp.updateJobMetadata(job, fullInfo, true)

	// Conditional: the loop's last check read the row before the fetch that
	// confirmed this, and an operator's Cancel that landed since writes the
	// row before it cancels ctx. Live used to be written over it, and the run
	// went on to a download until handleCancellation wrote Cancelled again. A
	// write — Live, or is_vod alone for a stream that is over — refused for
	// the row's outcome stops the run as a cancelled one, its early chat with
	// it; one that failed on a row with none does not (writeUnlessOutcome).
	fields := map[string]any{"is_vod": isVod}
	if !isVod {
		fields["status"] = database.StatusLive
	}
	if writeUnlessOutcome(sp.db, job.ID, fields) {
		sp.stopEarlyChat(chatDl)
		return cancelledResult()
	}

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

// earlyChatEpoch is an early-chat file's epoch: every live message's offset is
// measured from it, and nothing corrects it later — the player re-aligns the
// file by the difference between the job's start and this epoch — so any fixed
// instant serves, and none at all ("") leaves every live message without an
// offset. The schedule when there is one; otherwise the upload date, which is
// what an unscheduled stream's epoch was while ScheduledStartTime still fell
// back to it for upcoming streams.
func earlyChatEpoch(info *youtube.VideoInfo) string {
	if info.ScheduledStartTime != "" {
		return info.ScheduledStartTime
	}
	return info.UploadDate
}

// tryStartEarlyChat attempts to start a chat downloader during the upcoming phase (B2).
// onProgress is wired before Start so callers don't need a follow-up SetOnProgress
// (per audit reports/worker.md F16 — keeps initial+retry paths in sync).
//
// Returns the downloader together with the flag runEarlyChat's defer sets when
// the run ends — the input earlyChatNeedsRestart consults on each probe. Both are nil
// when no downloader could be started. The flag belongs to THIS run: the
// caller replaces it whenever it starts a new one. Once the stream goes live
// and completeStreamTransition hands the downloader to the orchestrator the
// flag has no reader left, which is harmless — the orchestrator never wires
// OnFinish itself.
func (sp *StreamProcessor) tryStartEarlyChat(ctx context.Context, job *database.Job, info *youtube.VideoInfo, onProgress func(chat.ChatProgress)) (*chat.ChatDownloader, *atomic.Bool) {
	// The cookie header is only needed for the FALLBACK watch-page fetch.
	// One-shot call, so a snapshot is correct here; the chat downloader below
	// gets a live getter. The page header, as setupChatDownloader's.
	cookieHeader := ""
	if sp.yt != nil && sp.yt.Auth != nil {
		cookieHeader = sp.yt.Auth.PageCookieHeader()
	}

	// This site is usually called with a ProbeVideoStatus info (ANDROID_VR, no
	// watch page), whose Chat is the zero value — so it normally falls back and
	// behaves exactly as before. When the caller does hold a freshly parsed
	// page (a full fetch's VideoInfo), the carried source saves the second
	// 1-5 MB download.
	src, err := chatSourceFor(ctx, info, job.VideoID, cookieHeader)
	if err != nil {
		sp.logger.Debug("failed to fetch watch page for early chat", "err", err, "videoID", job.VideoID)
		return nil, nil
	}
	if src.Continuation == "" {
		sp.logger.Debug("no chat continuation for early chat", "videoID", job.VideoID, "err", src.Err)
		return nil, nil
	}
	continuation, isReplay, visitorData := src.Continuation, src.IsReplay, src.VisitorData

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
		return nil, nil
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
		opts.PageCookieHeader = sp.yt.Auth.PageCookieHeader
	}
	opts.StreamStartTime = earlyChatEpoch(info)

	dl := chat.NewChatDownloader(opts)
	// Same as setupChatDownloader: without this the downloader's own
	// diagnostics (the mode rule's refusal, the file-epoch adoption, API
	// drift) never reach the log (Arc J D6).
	dl.Logger = sp.logger
	// A waiting room can sit through an outage too: its chat waits for the
	// network rather than spending its error budget on it. The orchestrator
	// installs its own probe again on the handoff.
	dl.SetIsOnline(sp.isOnline)
	if onProgress != nil {
		// Wire OnProgress before Start so the surge-detection callback fires
		// from the very first batch (F16).
		dl.SetOnProgress(onProgress)
	}
	dl.OnError = func(err error) {
		// "downloader", not "API": disk failures (write, append, header,
		// resume state) arrive here too.
		sp.logger.Warn("[Chat] Early chat downloader error", "jobID", job.ID, "err", err)
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
	// The processor-owned end-of-run flag behind earlyChatNeedsRestart, set by
	// runEarlyChat's defer once Start has returned by ANY route — after the
	// run's final flush, header update and sidecar decision, so a restart
	// triggered by it can never race this run's writes to chat.json.
	finished := &atomic.Bool{}
	sp.trackChat(dl)

	sp.db.UpdateJobFields(job.ID, map[string]any{
		"chat_status": "pending",
	})

	// Start in background
	go runEarlyChat(finished, func(r any) {
		sp.logger.Error("panic in early chat downloader", "jobID", job.ID, "panic", fmt.Sprint(r))
	}, func() { dl.Start(ctx) })

	sp.logger.Info("started early chat download for upcoming stream", "videoID", job.VideoID)
	return dl, finished
}

func (sp *StreamProcessor) stopEarlyChat(chatDl *chat.ChatDownloader) {
	if chatDl != nil {
		chatDl.Stop()
		sp.untrackChat(chatDl)
	}
}

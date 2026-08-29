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
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TwitchOfflineErrMsg is the literal error string emitted when a non-manual
// Twitch job's GetStreamInfo returns nil/!IsLive. Exported so the monitor's
// auto-recovery predicate can match on it without drifting from the producer.
// Keep these two sites aligned — a regression test in internal/monitor checks
// the predicate accepts exactly this string.
const TwitchOfflineErrMsg = "twitch channel is offline"

// sameBroadcastStart is THE broadcast-identity rule for Twitch jobs:
// stream_start_time is written once per job and compared against the
// currently-live broadcast's StartedAt with ±1 minute of tolerance (absorbs
// API formatting jitter; distinct broadcasts differ by far more). Missing or
// unparseable values on either side trust the match — the engine's
// resume-identity check still guards the recording itself. Shared by the
// restart-attach guard (processTwitchLive) and the post-outage resume guard
// (ExecuteTwitch) so the two paths can never disagree about what counts as
// "the same broadcast".
func sameBroadcastStart(knownStartISO, currentStartISO string) bool {
	if knownStartISO == "" || currentStartISO == "" {
		return true
	}
	oldStart, errOld := time.Parse(time.RFC3339, knownStartISO)
	newStart, errNew := time.Parse(time.RFC3339, currentStartISO)
	if errOld != nil || errNew != nil {
		return true
	}
	diff := newStart.Sub(oldStart)
	return diff <= time.Minute && diff >= -time.Minute
}

// twitchAuthSentinel returns ErrCookiesRequired when err is (or wraps)
// twitch.ErrTwitchAuthExpired or twitch.ErrSubscriberOnly so VOD/HLS
// errors that lost their wrap via %v formatting still get classified as
// auth-required by the downstream consumer. Subscriber-only restrictions
// route the same way as expired auth: logging into an account that has
// access is the fix, so StatusCookies (not a generic Error) is the right
// resting state. Returns nil for non-auth errors so plain failures don't
// get spuriously routed to StatusCookies.
func twitchAuthSentinel(err error) error {
	if errors.Is(err, twitch.ErrTwitchAuthExpired) || errors.Is(err, twitch.ErrSubscriberOnly) {
		return ErrCookiesRequired
	}
	return nil
}

// twitchChatCredentials returns the getter a live Twitch chat session
// authenticates with, or nil when there is no auth to read from.
//
// ONE getter for the token AND the login, because the handshake is a single
// authenticated-or-anonymous decision over the pair: a real OAuth token beside
// the anonymous `justinfan<random>` nickname is refused or downgraded by
// Twitch, and the symptom is chat that connects normally and never carries
// subscriber-only messages or badges. Wiring the two halves separately could
// produce that state two ways — one half left nil, or both read either side of
// a concurrent cookie Reload — so the pair is never split, here or in the jar
// (see cookies.CookieJar.GetTwitchCredentials, which reads both under one
// RLock).
//
// A method value, not a snapshot: it is re-read on every IRC reconnect, so a
// credential re-imported mid-stream reaches the next session. nil is the
// documented "log in anonymously" signal.
func twitchChatCredentials(auth *twitch.Auth) func() (token, login string) {
	if auth == nil {
		return nil
	}
	return auth.GetCredentials
}

// notifySend is Manager.Send's shape, taken as a value so the downgrade notice
// can be delivered through a recorder in a test. The production value is
// literally the method — nothing reimplements dispatch.
type notifySend func(title, description string, ntype notifications.NotificationType,
	fields []notifications.Field, opts notifications.SendOptions)

// twitchChatDowngradeReason turns one of twitch's fixed AuthDowngrade* tokens
// into the sentence an operator reads.
//
// The mapping lives at THIS end on purpose. internal/twitch reports a state; the
// operator-facing wording, and the remedy attached to it, belong beside the
// other things this process tells an operator to do. The four inputs are a
// closed vocabulary, so the default arm exists only for a future value added
// upstream without a matching arm here — it must still describe the degradation
// rather than say nothing, because a notification that names no problem is
// worse than the log line it was meant to escape.
func twitchChatDowngradeReason(reason string) string {
	switch reason {
	case twitch.AuthDowngradeLoginRefused:
		return "Twitch refused the saved login."
	case twitch.AuthDowngradeLoginUnacknowledged:
		return "Twitch never acknowledged the saved login."
	case twitch.AuthDowngradeNoLoginCookie:
		return "The cookie file has a Twitch auth-token but no login cookie beside it."
	case twitch.AuthDowngradeUnusableLoginCookie:
		return "The Twitch login cookie is not a name that can be sent to chat."
	default:
		return "The saved Twitch login could not be used."
	}
}

// twitchChatDowngradeNotice renders the whole operator-facing payload for one
// chat auth downgrade. Pure, and split from the send so a test can assert on
// what a target would actually receive rather than on the fact that something
// was sent.
//
// The inputs are the reason token, the job, and the channel — and that is the
// leak proof. There is no path from here to the cookie jar, to the credential
// getter, or to a chat line, so no title, description, or field can carry a
// token, a login, or a viewer's message. The check is structural; the test that
// asserts on the rendered payload is the second lock, not the first.
func twitchChatDowngradeNotice(job *database.Job, channel, reason string) (
	title, description string, fields []notifications.Field, opts notifications.SendOptions,
) {
	title = "Twitch chat is anonymous for " + channel
	description = twitchChatDowngradeReason(reason) +
		" Chat is still being recorded, but subscriber-only messages and badges will be missing " +
		"for this job. Re-export cookies from a browser signed in to Twitch, or run R F (Force " +
		"Cookie Refresh)."
	fields = []notifications.Field{
		{Name: "Channel", Value: channel, Inline: true},
		{Name: "Job", Value: job.ID, Inline: true},
		{Name: "Reason", Value: reason, Inline: true},
	}
	opts = notifications.SendOptions{
		URL:       job.URL,
		Thumbnail: job.ThumbnailURL,
		// The same event as the worker's "Authentication Required" and the
		// monitor's auth-loss alerts: an operator filtering for credential
		// trouble is filtering for this too.
		Event: "auth",
	}
	return title, description, fields, opts
}

// sendTwitchChatDowngrade delivers exactly one downgrade notice through send.
//
// TypeWarning, not TypeError: the capture is still running and still producing a
// usable archive. Nothing is lost that a re-export cannot restore for the NEXT
// job, and dressing a degradation as a failure trains an operator to ignore it.
//
// nil send is the no-notifier install and is not an error — every other
// notification site in this package is guarded the same way.
func sendTwitchChatDowngrade(send notifySend, job *database.Job, channel, reason string) {
	if send == nil {
		return
	}
	title, description, fields, opts := twitchChatDowngradeNotice(job, channel, reason)
	send(title, description, notifications.TypeWarning, fields, opts)
}

// twitchChatDowngradeCallback builds the OnAuthDowngrade callback for one live
// chat downloader.
//
// The downloader guarantees at most one call per job, so there is no dedup here
// — and dedup ACROSS jobs is deliberately absent: a second job on the same
// channel an hour later with the same dead cookies must notify again, because
// by then the operator may believe they fixed it.
//
// sp.notifier is read at FIRE time rather than captured into the closure. The
// field is written once, by SetNotifier during worker construction and long
// before any job goroutine exists, so this read races nothing; and a config
// hot-reload swaps the manager's TARGET LIST rather than the pointer, so the
// notice reaches whatever webhooks are configured when it fires rather than
// whatever was configured when the stream started.
func (sp *StreamProcessor) twitchChatDowngradeCallback(job *database.Job, channel string) func(reason string) {
	return func(reason string) {
		if sp.notifier == nil {
			return
		}
		sendTwitchChatDowngrade(sp.notifier.Send, job, channel, reason)
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

			// Method value, not a snapshot: the comment-paging loop below runs
			// for the length of the VOD, and a token captured here would be
			// presented unchanged long after it rotated or died.
			var authToken func() string
			if sp.tw != nil && sp.tw.Auth != nil {
				authToken = sp.tw.Auth.GetAuthToken
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

	// A Downloading job resumed after a restart belongs to a specific
	// broadcast. If the channel is now live with a DIFFERENT broadcast (the
	// old one ended while Moombox was down), do not attach: the engine
	// would discard the old broadcast's resume state and truncate its
	// staging data, and the new broadcast would record under the old job's
	// metadata. stream_start_time is the stable cross-restart identity —
	// it is written once per job (guarded by `job.StreamStartTime == ""`
	// below) for monitor-created and manually-added jobs alike. The minute
	// of tolerance absorbs any API formatting jitter; distinct broadcasts
	// differ by far more. The captured data stays recoverable via the Mux
	// action, and the monitor picks the new broadcast up as its own job.
	if job.Status == database.StatusDownloading && !sameBroadcastStart(job.StreamStartTime, streamInfo.StartedAt) {
		sp.logger.Warn("twitch broadcast changed while job was interrupted; not attaching to the new broadcast",
			"jobID", job.ID, "channel", login,
			"oldStart", job.StreamStartTime, "newStart", streamInfo.StartedAt)
		return &StreamProcessResult{
			ShouldDownload: false,
			Error:          "stream ended while Moombox was offline; a new broadcast is live — captured data can be muxed via the Mux action",
		}, nil
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
			chatCredentials := twitchChatCredentials(sp.tw.Auth)
			chatChannel := streamInfo.ChannelDisplayName
			if chatChannel == "" {
				chatChannel = login
			}
			twitchChatDl = twitch.NewChatDownloader(twitch.ChatDownloaderOptions{
				ChannelLogin:    login,
				ChannelDisplay:  streamInfo.ChannelDisplayName,
				ChannelID:       streamInfo.ChannelID,
				StreamID:        streamInfo.StreamID,
				OutputPath:      chatPath,
				StreamStartTime: streamInfo.StartedAt,
				// Method value, not a snapshot — read fresh on every IRC
				// reconnect so a rotated credential doesn't silently downgrade
				// the rest of the stream to anonymous chat capture.
				Credentials: chatCredentials,
				// And when it downgrades anyway, say so ONCE, where the
				// operator is already looking. Fires only when this job HAD
				// credentials — a cookieless install captures chat anonymously
				// by design and must never be notified about it.
				OnAuthDowngrade: sp.twitchChatDowngradeCallback(job, chatChannel),
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
	var lastOfflineProbe time.Time

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

		// When the oracle reports offline, still probe occasionally (floor) so a
		// wrongly-offline oracle can't strand a waiting stream. Safe because
		// network-class errors no longer count (see applyProbeError), and a
		// success self-corrects the oracle via reportProbeResult.
		if sp.isOnline != nil && !sp.isOnline() {
			if time.Since(lastOfflineProbe) < offlineProbeFloor {
				sp.logger.Debug("skipping Twitch probe — device offline (within floor)", "login", login)
				continue
			}
			lastOfflineProbe = time.Now()
		}

		streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			newCount, giveUp, report, cancelled := applyProbeError(err, consecutiveErrors)
			switch report {
			case reportFailure:
				reportProbeResult("probe/twitch", true)
			case reportSuccess:
				reportProbeResult("probe/twitch", false)
			}
			if cancelled {
				return nil, nil
			}
			consecutiveErrors = newCount // unchanged for network-class failures
			if report == reportFailure {
				sp.logger.Debug("probe network error — not counting, still waiting", "channel", login, "err", err)
			} else {
				sp.logger.Warn("probe error (definitive)", "channel", login, "err", err, "consecutive", consecutiveErrors)
			}
			if giveUp {
				// Wrap with ErrNonActionable so worker.setJobError suppresses
				// the user notification — exhausted DEFINITIVE retries mean
				// the stream isn't coming up regardless of further probes.
				return nil, fmt.Errorf("max probe errors: %w (%w)", err, ErrNonActionable)
			}
			continue
		}
		consecutiveErrors = 0
		// Successful poll reached Twitch — feed the oracle (Layer 4).
		reportProbeResult("probe/twitch", false)

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

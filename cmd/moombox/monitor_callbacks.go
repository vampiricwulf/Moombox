package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// crossMonitorVouchWindow bounds how recently a sibling monitor must have
// succeeded for its success to suppress another monitor's "unhealthy" alert.
// Comfortably longer than DECAPI's seconds-scale cadence and a typical feed
// interval, but short enough that a silently-stalled sibling (frozen at a stale
// success) stops vouching so a genuine outage still surfaces.
const crossMonitorVouchWindow = 20 * time.Minute

// channelHealthReporter is the slice of a monitor's surface needed to
// cross-confirm a channel's reachability across sibling monitors.
type channelHealthReporter interface {
	Health() []monitor.ChannelHealth
}

// siblingReachable reports whether any sibling monitor RECENTLY reached the
// channel successfully. When true the channel is not "not responding" — another
// monitor is still seeing it, so its streams aren't being missed — and the
// failing monitor's unhealthy alert is a false positive to suppress. This is
// the guard against YouTube serving RSS 404/5xx during peak hours while the
// independent DECAPI monitor stays healthy. A sibling vouches only on a FRESH
// success (last check succeeded within crossMonitorVouchWindow); one that is
// itself failing, has never checked the channel, or has gone stale does not.
func siblingReachable(siblings []channelHealthReporter, channelID string, now time.Time) bool {
	for _, sib := range siblings {
		if sib == nil {
			continue
		}
		for _, h := range sib.Health() {
			if h.ChannelID != channelID {
				continue
			}
			if h.ConsecutiveErrors == 0 && h.LastCheckedAt != 0 &&
				now.Sub(time.UnixMilli(h.LastCheckedAt)) <= crossMonitorVouchWindow {
				return true
			}
		}
	}
	return false
}

// wireMonitorCallbacks installs every post-service-startup callback that
// connects the construction graph: cookie recovery / auth-recovered sweep,
// monitor ProbeVideo + OnVideoFound / OnStreamFound job-creation closures,
// monitor OnSchedule (broadcast-all-timers + TUI atomic dispatch), initial
// per-job log tracking, database OnJobUpdate / OnJobsChange subscribers
// (persisted on runState so shutdown can unsubscribe), the log-forwarder
// goroutine, and the connectivity OnStateChange wiring.
//
// Called once between wireRoutes() and the "start services" phase in run().
func (s *runState) wireMonitorCallbacks() {
	// Cooldown for auth-recovery failure notifications: OnRecoveryNeeded can
	// re-fire on every periodic auth check while cookies stay dead, and a
	// broken refresh should page the operator once per window, not per poll.
	// Guarded by a mutex — the recovery attempt runs on its own goroutine.
	var authNotifyMu sync.Mutex
	lastAuthFailNotify := make(map[string]time.Time)
	notifyAuthFailure := func(platform, title, desc string, ntype notifications.NotificationType) {
		authNotifyMu.Lock()
		defer authNotifyMu.Unlock()
		if time.Since(lastAuthFailNotify[platform]) < 30*time.Minute {
			return
		}
		lastAuthFailNotify[platform] = time.Now()
		s.notifyMgr.Send(title, desc, ntype,
			[]notifications.Field{{Name: "Platform", Value: platform, Inline: true}},
			notifications.SendOptions{Event: "auth"},
		)
	}

	s.cookieRefresh.OnRecoveryNeeded = func(platform string) {
		var autoEnabled bool
		s.configStore.Read(func(c *config.MoomboxConfig) {
			autoEnabled = c.Cookies.AutoEnabled
		})
		if !autoEnabled {
			s.log.Debug("Auth lost but auto-cookies disabled, skipping recovery", "platform", platform)
			return
		}
		s.log.Warn("Auth lost, attempting auto-cookie recovery", "platform", platform)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("auto-cookie recovery panic", "panic", r)
				}
			}()
			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer refreshCancel()
			ok, err := s.autoCookieSvc.RefreshCookies(refreshCtx)
			if err != nil {
				s.log.Error("auto-cookie recovery failed", "platform", platform, "err", err)
				// Previously log-only: the operator learned cookies were dead
				// only when a recording actually failed. 30-min per-platform
				// cooldown via notifyAuthFailure.
				notifyAuthFailure(platform, "Cookie Auto-Refresh Failed",
					fmt.Sprintf("Automatic cookie refresh for %s failed — recordings will fail until cookies are refreshed. Re-run cookie setup from Settings.", platform),
					notifications.TypeError)
			} else if ok {
				s.log.Info("auto-cookie recovery succeeded", "platform", platform)
				// Re-check auth status immediately so the UI updates
				s.cookieRefresh.CheckNow(context.Background())
			} else {
				s.log.Warn("auto-cookie recovery did not restore auth", "platform", platform)
				notifyAuthFailure(platform, "Cookie Auto-Refresh Ineffective",
					fmt.Sprintf("Automatic cookie refresh completed but did not restore %s authentication — sign in again via cookie setup in Settings.", platform),
					notifications.TypeWarning)
			}
		}()
	}

	// When a platform transitions from not-authenticated to authenticated,
	// sweep any jobs parked in StatusCookies on that platform back to
	// Upcoming so they get re-probed without manual intervention. Closes
	// audit decision #23 (worker.md Q3).
	s.cookieRefresh.OnAuthRecovered = func(platform string) {
		jobs, err := s.db.GetAllJobs()
		if err != nil {
			s.log.Warn("auth-recovered sweep: GetAllJobs failed", "platform", platform, "err", err)
			return
		}
		resumed := 0
		for _, job := range jobs {
			if job.Status != database.StatusCookies {
				continue
			}
			if job.Platform != platform {
				continue
			}
			s.db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusUpcoming,
				"error":  "",
			})
			resumed++
		}
		if resumed > 0 {
			s.log.Info("auth recovered — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// Event "auth" pairs with the worker's "Authentication Required"
			// emit — an empty Event would bypass every target's allowlist
			// (unfilterable) since the filter only applies when Event != "".
			s.notifyMgr.Send("Authentication Recovered",
				fmt.Sprintf("Resumed %d job(s) waiting on %s cookies", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}

	// ProbeVideo callback for monitors (metadata check before job creation).
	// Uses the caller-supplied ctx so monitor shutdown cancels in-flight
	// probes (per audit reports/cross-cutting.md C4).
	probeVideoFunc := func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := s.ytService.ProbeVideoStatus(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus:       string(meta.StreamStatus),
			Title:              meta.Title,
			ChannelName:        meta.ChannelName,
			PublishedAt:        meta.PublishedAt,
			PublishedPrecision: meta.PublishedPrecision,
			PlayabilityError:   string(meta.PlayabilityError),
		}, nil
	}
	s.feedMon.ProbeVideo = probeVideoFunc
	s.decapiMon.ProbeVideo = probeVideoFunc

	// Authenticated probe for members-only videos: an anonymous probe can't see
	// members-only content, gets no formats, and misclassifies it as "upcoming"
	// (which bypasses include_non_live_content). The TV_DOWNGRADED+cookies probe
	// classifies it correctly (vod/live/upcoming). Only the feed monitor's
	// membership path uses it; RSS/DECAPI stay on the anonymous probe.
	s.feedMon.ProbeVideoAuth = func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := s.ytService.ProbeVideoStatusAuthenticated(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus:       string(meta.StreamStatus),
			Title:              meta.Title,
			ChannelName:        meta.ChannelName,
			PublishedAt:        meta.PublishedAt,
			PublishedPrecision: meta.PublishedPrecision,
			PlayabilityError:   string(meta.PlayabilityError),
		}, nil
	}

	// Membership discovery: authenticated /membership tab scan for members-only
	// videos the RSS feed never lists. Wired on the feed monitor only. The
	// closure adapts youtube.MembershipVideo -> monitor.MembershipVideo (keeping
	// the monitor package decoupled from youtube, like probeVideoFunc does for
	// VideoInfo). MembershipEnabled re-reads the config flag AND cookie state
	// live each cycle, so toggling the setting or acquiring cookies takes effect
	// on the next cycle with no restart.
	s.feedMon.FetchMembership = func(ctx context.Context, channelID string) ([]monitor.MembershipVideo, error) {
		vids, err := s.ytService.FetchMembershipVideos(ctx, channelID)
		if err != nil {
			return nil, err
		}
		out := make([]monitor.MembershipVideo, len(vids))
		for i, v := range vids {
			out[i] = monitor.MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, nil
	}
	s.feedMon.MembershipEnabled = func() bool {
		enabled := true
		s.configStore.Read(func(c *config.MoomboxConfig) {
			enabled = c.Monitors.MembershipDiscoveryEnabled()
		})
		return enabled && s.ytService.HasAuthCookies()
	}

	// createYouTubeJob creates a YouTube job and enqueues it. Stream-status
	// classification is handled by the monitors via ProcessYouTubeVideo.
	createYouTubeJob := func(videoID, title, videoURL string, ch *config.ChannelConfig, source string) {
		s.log.Info("Video found", slog.String("source", source), slog.String("videoID", videoID), slog.String("title", title))

		includeNonLive := ch.IncludeNonLiveContent
		outputDir := resolveOutputDir(ch, s.configStore)
		thumbnailURL := youtubeThumbnailURL(videoID)

		now := time.Now().UTC().Format(time.RFC3339)
		job := &database.Job{
			ID:                videoID,
			VideoID:           videoID,
			URL:               videoURL,
			Title:             title,
			ChannelName:       ch.Name,
			Platform:          "youtube",
			Status:            database.StatusUpcoming,
			ThumbnailURL:      thumbnailURL,
			OutputDirectory:   outputDir,
			AllowNonStream:    includeNonLive,
			QualityPreference: ch.QualityPreference,
			CreatedAt:         now,
			UpdatedAt:         now,
		}

		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add YouTube job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		s.db.AddToHistory(videoID)
		s.dlWorker.EnqueueJob(videoID)
		// AddJob's OnJobAdded handler (wired below) handles the WS
		// broadcast for the new job; no explicit BroadcastJobsUpdate
		// needed here. DECISIONS #21 consumer migration.
		if s.notifyMgr.HasTargets() {
			s.notifyMgr.Send("Stream Found",
				fmt.Sprintf("Found matching stream: %s", title),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Channel", Value: ch.Name, Inline: true},
					{Name: "Video ID", Value: videoID, Inline: true},
				},
				notifications.SendOptions{
					Event:     "found",
					URL:       videoURL,
					Thumbnail: youtubeThumbnailURL(videoID),
				})
		}
	}

	// Monitor -> Worker: create jobs for found videos. Panic recovery
	// prevents a single bad callback from killing the monitor goroutine.
	s.feedMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnVideoFound (feed)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "feed")
	}
	s.decapiMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnVideoFound (decapi)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "decapi")
	}
	s.twitchMon.OnStreamFound = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamFound (twitch)", slog.Any("panic", r))
			}
		}()
		jobID := twitch.BuildJobID(info.StreamID, false)
		s.log.Info("Stream found by Twitch monitor", slog.String("jobID", jobID), slog.String("title", info.Title))

		outputDir := resolveOutputDir(ch, s.configStore)

		now := time.Now().UTC().Format(time.RFC3339)
		title := info.ChannelDisplayName + " — " + info.Title
		if info.Title == "" {
			title = info.ChannelDisplayName + " — " + time.Now().UTC().Format(time.RFC3339)
		}

		job := &database.Job{
			ID:                jobID,
			VideoID:           info.StreamID,
			URL:               "https://twitch.tv/" + info.ChannelLogin,
			Title:             title,
			ChannelName:       info.ChannelDisplayName,
			Platform:          "twitch",
			Status:            database.StatusLive, // Twitch: immediately Live (confirmed by GQL)
			ThumbnailURL:      info.ThumbnailURL,
			ChannelAvatarURL:  info.ProfileImageURL,
			TwitchCategory:    info.GameCategory,
			TwitchQuality:     ch.QualityPreference,
			QualityPreference: ch.QualityPreference,
			StreamStartTime:   info.StartedAt,
			OutputDirectory:   outputDir,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add Twitch job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		// Stash the monitor's fresh streamInfo for processTwitchLive to consume,
		// so it doesn't immediately re-query Twitch GQL (which has been observed
		// to return Stream=nil for ~1s after StreamMetadata reports a stream as
		// live, manifesting as a false "twitch channel is offline" error).
		s.dlWorker.StashTwitchStreamInfo(jobID, info)
		s.db.AddToHistory(jobID)
		s.dlWorker.EnqueueJob(jobID)
		// Same as the YouTube path — AddJob's OnJobAdded handler
		// broadcasts the new job; no explicit BroadcastJobsUpdate
		// needed. DECISIONS #21 consumer migration.
		if s.notifyMgr.HasTargets() {
			twitchFields := []notifications.Field{
				{Name: "Channel", Value: info.ChannelDisplayName, Inline: true},
				{Name: "Stream ID", Value: info.StreamID, Inline: true},
			}
			if info.GameCategory != "" {
				twitchFields = append(twitchFields, notifications.Field{
					Name: "Category", Value: info.GameCategory, Inline: true,
				})
			}
			s.notifyMgr.Send("Twitch Stream Found",
				fmt.Sprintf("Live: %s", title),
				notifications.TypeInfo,
				twitchFields,
				notifications.SendOptions{
					Event:     "found",
					URL:       "https://twitch.tv/" + info.ChannelLogin,
					Thumbnail: info.ThumbnailURL,
				})
		}
	}

	s.twitchMon.OnStreamRecover = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig, jobID string) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamRecover (twitch)", slog.Any("panic", r))
			}
		}()

		// Stash the fresh streamInfo so processTwitchLive consumes it instead
		// of re-querying GQL — same flap-prevention as the OnStreamFound path.
		s.dlWorker.StashTwitchStreamInfo(jobID, info)

		// AutoReinitializeJob increments auto_retry_count, clears state, and
		// re-enqueues. The cap (worker.MaxTwitchAutoRetries) is enforced by
		// the monitor's predicate before we even get here.
		s.dlWorker.AutoReinitializeJob(jobID)

		s.log.Info("auto-recovered twitch job",
			slog.String("jobID", jobID),
			slog.String("channel", info.ChannelDisplayName),
			slog.String("streamID", info.StreamID))
	}

	// Monitor -> WebSocket: broadcast timer updates. TypeScript broadcasts
	// ALL three monitor times on each schedule event so we do the same —
	// read all three monitors' next check times.
	broadcastAllTimers := func() {
		s.wsHub.BroadcastCheckTimers(map[string]any{
			"nextFeedCheck":   s.feedMon.GetNextCheckAt(),
			"nextDecapiCheck": s.decapiMon.GetNextCheckAt(),
			"nextTwitchCheck": s.twitchMon.GetNextCheckAt(),
		})
	}

	// OnSchedule subscriber slots live on runState (atomic.Pointer fields).
	// Set the dispatchers once before monitor.Start(); the TUI wiring later
	// Store()s concrete funcs into the atomic slots. Reassigning the
	// monitor's plain func field while its goroutine is running would race
	// with scheduleNext() reading it; atomic.Pointer keeps the read-side
	// lock-free and the write-side safe.
	s.feedMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.feedTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	s.decapiMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.decapiTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	s.twitchMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.twitchTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}

	// Channel-health notifications: a channel that fails every check for a
	// sustained streak (renamed/banned Twitch login, dead YouTube channel,
	// 404 RSS) previously rotted at Debug level until a stream was missed.
	// One notification per streak, per monitor; the /api/status
	// channelHealth surface shows the live state. platform label is set per
	// monitor so the operator knows which source flagged it.
	unhealthyNotify := func(platform string, siblings ...channelHealthReporter) func(channelID string, consecutive int, lastErr string) {
		return func(channelID string, consecutive int, lastErr string) {
			// Cross-monitor confirmation: a channel is only "not responding" if
			// EVERY monitor covering it has lost it. YouTube serves RSS 404/5xx
			// during peak hours while the independent DECAPI monitor keeps
			// working, so a lone feed-monitor failure is a false positive — its
			// streams are still being seen. Suppress unless no sibling vouches.
			if siblingReachable(siblings, channelID, time.Now()) {
				s.log.Info("channel unhealthy on one monitor but still reachable via another — suppressing alert",
					"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
				return
			}
			s.log.Warn("channel failing monitor checks — verify it still exists",
				"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
			s.notifyMgr.Send("Channel Not Responding",
				fmt.Sprintf("A %s channel has failed %d consecutive monitor checks — it may be renamed, banned, or misconfigured, and its streams are being missed", platform, consecutive),
				notifications.TypeWarning,
				[]notifications.Field{
					{Name: "Channel", Value: channelID, Inline: true},
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Last Error", Value: lastErr},
				},
				notifications.SendOptions{Event: "channel_unhealthy"},
			)
		}
	}
	// YouTube channels are covered by both the RSS feed and DECAPI monitors, so
	// each cross-confirms against the other before alerting. Twitch has a single
	// (reliable GQL) monitor with no sibling to confirm against.
	s.feedMon.SetOnChannelUnhealthy(unhealthyNotify("youtube", s.decapiMon))
	s.decapiMon.SetOnChannelUnhealthy(unhealthyNotify("youtube", s.feedMon))
	s.twitchMon.SetOnChannelUnhealthy(unhealthyNotify("twitch"))

	// Initialize per-job log tracking with existing jobs (matches TS knownJobIds)
	if existingJobs, err := s.db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			s.db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates. Uses the
	// fine-grained OnJobChange API. silentColumns (resume_position,
	// chat_offset) bypass OnJobChange entirely at the writer side,
	// so player-state scrubs don't reach this subscriber.
	//
	// No per-job throttle here: progress writes are already capped to
	// ~60Hz/job upstream by ProgressTracker.maybeUpdate (16ms gate in
	// internal/worker/progress.go), and every other UpdateJobFields
	// caller is event-driven (state transitions, not loops).
	s.unsubWSJobUpdate = s.db.OnJobChange(func(ev *database.JobChange) {
		job := ev.Job
		// Skip broadcasting updates for archived (old finished) jobs — same
		// classification as the list filter, via the shared jobArchivedAt
		// predicate so the two can never disagree about which jobs are
		// archived.
		if job.Status == database.StatusFinished && job.UpdatedAt != "" {
			var hideAgeDays float64
			s.configStore.Read(func(c *config.MoomboxConfig) {
				hideAgeDays = c.Monitors.HideFinishedAgeDays.Value
			})
			if hideAgeDays >= 0 {
				cutoff := time.Now().Add(-time.Duration(hideAgeDays*24) * time.Hour)
				if jobArchivedAt(job, cutoff) {
					return
				}
			}
		}
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnJobAdded subscriber: AddJob no longer fires OnJobsChange (the
	// writer-side dispatch was dropped as part of DECISIONS #21
	// consumer migration). The new-job broadcast goes through
	// BroadcastJobUpdate — frontend's "job_update" handler already
	// has a "job not in array yet — add it and re-render" branch
	// (web/public/app.js around line 1021), so a singular update
	// for an unknown ID is the right wire shape. We also do the
	// per-job log tracking here that the OnJobsChange handler used
	// to do for ALL jobs on every fan-out.
	s.unsubWSJobAdded = s.db.OnJobAdded(func(ev *database.JobAdded) {
		job := ev.Job
		s.db.TrackJobForLogs(job.ID)
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnTrimsChanged subscriber: AddTrim/DeleteTrim no longer fire
	// OnJobsChange (writer-side dispatch dropped per DECISIONS #21).
	// Re-fetch the affected job (so its Trims field reflects the
	// current SQLite state) and broadcast it through BroadcastJobUpdate
	// — frontend's existing job_update handler replaces the in-memory
	// job and re-renders, picking up the new trim list naturally.
	s.unsubWSTrimsChanged = s.db.OnTrimsChanged(func(ev *database.TrimsChanged) {
		job, err := s.db.GetJob(ev.JobID)
		if err != nil || job == nil {
			return
		}
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnJobDeleted subscriber: send a targeted job_deleted WS event so the
	// frontend drops the row immediately. This replaces the prior full-list
	// rebroadcast (jobs_update) which raced against the preceding
	// status=Cancelled job_update and left stale rows visible in the UI.
	// Per-job log buffers are pruned via the active-IDs set derived from the
	// post-delete snapshot (the deleted ID drops out naturally).
	s.unsubWSJobDeleted = s.db.OnJobDeleted(func(ev *database.JobDeleted) {
		jobs := getAllJobsSafe(s.db)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
		}
		// Only the DATABASE per-job log pipeline is live (RouteLogToJobs);
		// the logger's parallel buffers are unwired and permanently empty.
		s.db.PruneJobLogs(activeIDs)
		s.wsHub.BroadcastJobDeleted(ev.JobID)
	})

	s.unsubWSJobsChange = s.db.OnJobsChange(func(jobs []*database.Job) {
		// Keep per-job log tracking in sync (matches TS knownJobIds update)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			s.db.TrackJobForLogs(j.ID)
		}
		s.db.PruneJobLogs(activeIDs)
		s.wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, s.configStore))
	})

	// Logger -> WebSocket: broadcast log lines + route to per-job buffers
	s.logSub = s.log.Subscribe()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("log forwarder panic", "panic", r)
			}
		}()
		for line := range s.logSub {
			s.wsHub.BroadcastLog(line)
			s.db.RouteLogToJobs(line) // Route to per-job buffer (matches TS knownJobIds log routing)
		}
	}()

	// Connectivity -> monitors + WebSocket: kick monitors on reconnect,
	// broadcast state — and notify. The web/TUI broadcasts are ephemeral;
	// an operator who was away during an outage previously had NO trace
	// that monitoring was down (streams silently missed). Transitions are
	// already debounced by the connectivity monitor, so this can't flap-spam.
	// outageStart is guarded by a mutex — OnStateChange serializes today,
	// but this closure must not silently depend on that.
	var connNotifyMu sync.Mutex
	var outageStart time.Time
	s.connMon.OnStateChange(func(online bool) {
		if online {
			s.feedMon.CheckNow()
			s.decapiMon.CheckNow()
			s.twitchMon.CheckNow()
		}
		s.wsHub.BroadcastConnectivity(online)

		connNotifyMu.Lock()
		defer connNotifyMu.Unlock()
		if !online {
			outageStart = time.Now()
			s.notifyMgr.Send("Connectivity Lost",
				"Internet connectivity lost — channel monitoring is paused and active downloads are waiting",
				notifications.TypeWarning, nil,
				notifications.SendOptions{Event: "connectivity_lost"},
			)
			return
		}
		if outageStart.IsZero() {
			return // startup/initial online state — nothing to report
		}
		outage := time.Since(outageStart).Round(time.Second)
		outageStart = time.Time{}
		s.notifyMgr.Send("Connectivity Restored",
			"Internet connectivity restored — monitors re-checking now",
			notifications.TypeSuccess,
			[]notifications.Field{
				{Name: "Outage Duration", Value: outage.String(), Inline: true},
			},
			notifications.SendOptions{Event: "connectivity_restored"},
		)
	})
}

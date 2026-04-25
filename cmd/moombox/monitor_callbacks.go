package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

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
			} else if ok {
				s.log.Info("auto-cookie recovery succeeded", "platform", platform)
				// Re-check auth status immediately so the UI updates
				s.cookieRefresh.CheckNow(context.Background())
			} else {
				s.log.Warn("auto-cookie recovery did not restore auth", "platform", platform)
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
			s.notifyMgr.Send("Authentication Recovered",
				fmt.Sprintf("Resumed %d job(s) waiting on %s cookies", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{},
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
			StreamStatus: string(meta.StreamStatus),
			Title:        meta.Title,
			ChannelName:  meta.ChannelName,
		}, nil
	}
	s.feedMon.ProbeVideo = probeVideoFunc
	s.decapiMon.ProbeVideo = probeVideoFunc

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
		s.wsHub.BroadcastJobsUpdate(filterJobsByAge(getAllJobsSafe(s.db), s.configStore))
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
		s.db.AddToHistory(jobID)
		s.dlWorker.EnqueueJob(jobID)
		s.wsHub.BroadcastJobsUpdate(filterJobsByAge(getAllJobsSafe(s.db), s.configStore))
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

	// Initialize per-job log tracking with existing jobs (matches TS knownJobIds)
	if existingJobs, err := s.db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			s.db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates
	s.unsubWSJobUpdate = s.db.OnJobUpdate(func(job *database.Job) {
		// Skip broadcasting updates for archived (old finished) jobs
		if job.Status == database.StatusFinished && job.UpdatedAt != "" {
			var ageDays int
			s.configStore.Read(func(c *config.MoomboxConfig) {
				ageDays = int(c.Monitors.HideFinishedAgeDays.Value)
			})
			cutoff := time.Now().AddDate(0, 0, -ageDays)
			if t, err := time.Parse(time.RFC3339, job.UpdatedAt); err == nil && t.Before(cutoff) {
				return
			}
		}
		s.wsHub.BroadcastJobUpdate(job.ID, job)
	})
	s.unsubWSJobsChange = s.db.OnJobsChange(func(jobs []*database.Job) {
		// Keep per-job log tracking in sync (matches TS knownJobIds update)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			s.db.TrackJobForLogs(j.ID)
		}
		s.db.PruneJobLogs(activeIDs)
		s.log.PruneJobLogs(activeIDs)
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
	// broadcast state.
	s.connMon.OnStateChange(func(online bool) {
		if online {
			s.feedMon.CheckNow()
			s.decapiMon.CheckNow()
			s.twitchMon.CheckNow()
		}
		s.wsHub.BroadcastConnectivity(online)
	})
}

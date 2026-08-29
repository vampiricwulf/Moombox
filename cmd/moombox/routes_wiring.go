package main

import (
	"log/slog"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// wireRoutes registers every API route on s.r. Returns the importCleanup
// function from ImportRoutes so run() can defer it without this file needing
// to own the lifetime of the helper. The ordering matches the audit scope:
// jobs → formats → status → config → channels → files → trim → stats →
// PoT → setup → ffmpeg → log → import → cookies → yt-dlp → restart →
// update → auth → client-token → watch.
func (s *runState) wireRoutes() func() {
	routes.JobRoutes(
		s.r,
		s.db,
		s.configStore,
		s.dlWorker,
		s.apiRL,
		&twitchMetadataAdapter{svc: s.twService},
		&youtubeMetadataAdapter{svc: s.ytService},
		s.notifyMgr,
	)
	routes.FormatRoutes(s.r, &routes.FormatRoutesDeps{
		DB: s.db,
		YT: &ytFormatAdapter{svc: s.ytService, store: s.configStore},
	})
	routes.StatusRoute(s.r, &routes.StatusRouteDeps{
		Version:            version,
		StartTime:          s.startTime,
		GetActivePlatforms: s.getActivePlatforms,
		// Both wire shapes come from routes' own projections rather than
		// being rebuilt here. Three hand-written copies of the cookieStatus
		// map existed across two packages and a field added to two of them
		// leaves this endpoint — the one the dashboard polls — quietly
		// serving the old meaning.
		GetCookieStatus: func() map[string]any {
			return routes.CookieStatusPayload(s.cookieRefresh.GetStatus())
		},
		GetTwitchAuthStatus: func() map[string]any {
			return routes.TwitchAuthStatusPayload(s.cookieRefresh.GetStatus())
		},
		GetAutoCookieReloginNeeded: func() any {
			// ReloginStatus, not GetStatus: this closure reads nothing but
			// NeedsManualRelogin, and GetStatus's browser/registry detection
			// scan runs on every /api/status poll — the dashboard's most
			// frequent request — for a field this never uses.
			return s.autoCookieSvc.ReloginStatus()
		},
		GetNextFeedCheck:   s.feedMon.GetNextCheckAt,
		GetNextDecapiCheck: s.decapiMon.GetNextCheckAt,
		GetNextTwitchCheck: s.twitchMon.GetNextCheckAt,
		GetChannelHealth: func() map[string]any {
			// Feed and DECAPI both track the same YouTube channel set;
			// merge, preferring whichever has the fresher last-check so
			// the dashboard shows one row per YouTube channel.
			return map[string]any{
				"youtube": mergeChannelHealth(s.feedMon.Health(), s.decapiMon.Health()),
				"twitch":  s.twitchMon.Health(),
			}
		},
	})
	routes.ConfigRoutes(s.r, s.configStore, &routes.ConfigRoutesCallbacks{
		OnLogLevelChange: func(level string) {
			s.log.SetLevel(level)
		},
		OnMaxParallelChange: func(n int) {
			s.dlWorker.SetParallelDownloads(n)
		},
		OnHideFinishedAgeChanged: func() {
			// Send config_update FIRST so the Web UI's hideFinishedAgeDays is
			// already up to date by the time the jobs_update payload (filtered
			// with the new threshold) arrives. Otherwise the per-client FIFO
			// queue would deliver jobs_update first, and the Web UI's archive
			// re-eval would run with the stale threshold and undo the server's
			// widening on a threshold increase.
			// Capture the threshold ONCE and reuse it for both the
			// config_update payload and the job filtering below. Re-reading
			// the store for the filter (via filterJobsByAge) would race a
			// concurrent config change and could broadcast a hideFinishedAgeDays
			// that disagrees with the threshold the jobs_update was filtered by.
			var hideAge float64
			s.configStore.Read(func(c *config.MoomboxConfig) {
				hideAge = c.Monitors.HideFinishedAgeDays.Value
			})
			s.wsHub.Broadcast("config_update", map[string]any{"hideFinishedAgeDays": hideAge})
			jobs, _ := s.db.GetAllJobs()
			s.wsHub.BroadcastJobsUpdate(filterJobsByAgeThreshold(jobs, hideAge))
		},
		OnChannelChange: s.kickMonitors,
		OnNotificationsChange: func() {
			// Hot-reload notification targets so edits apply immediately —
			// previously they silently required a restart nothing asked for.
			s.notifyMgr.Reload(s.configStore.Snapshot())
		},
	})
	routes.NotificationRoutes(s.r, &routes.NotificationRouteDeps{Logger: s.log})
	routes.MonitorRoutes(s.r, &routes.MonitorRouteDeps{CheckNow: func() {
		// Read at call time — kickMonitors is populated in initServices;
		// the closure avoids capturing a nil field at wiring time.
		if s.kickMonitors != nil {
			s.kickMonitors()
		}
	}})
	routes.BackfillRoutes(s.r, &routes.BackfillRouteDeps{Rescan: func() {
		// Read at call time — backfillRescan is populated in initServices;
		// the closure avoids capturing a nil field at wiring time.
		if s.backfillRescan != nil {
			s.backfillRescan()
		}
	}})
	routes.ChannelRoutes(s.r, s.configStore, s.kickMonitors)
	routes.FileRoutes(s.r, &routes.FileRoutesDeps{
		DB:     s.db,
		Store:  s.configStore,
		Logger: s.log,
	})
	routes.HistoryRoutes(s.r, &routes.HistoryRoutesDeps{
		DB:     s.db,
		Logger: s.log,
	})
	routes.TrimRoutes(s.r, s.db, s.trimSvc)
	routes.StatsRoutes(s.r, &routes.StatsRouteDeps{
		DB:     s.db,
		Worker: s.dlWorker,
	})
	routes.PotRoutes(s.r, &routes.PotRoutesDeps{
		PotProvider: s.potProvider,
		StartTime:   s.startTime,
		RateLimit:   s.potRL,
		Logger:      s.log,
	})
	routes.SetupRoutes(s.r, &routes.SetupDeps{
		Auth: s.authSvc,
		OnInstallYtdlp: func(port int, httpsEnabled bool) {
			if err := routes.InstallYtdlpPlugin(port, httpsEnabled); err != nil {
				s.log.Error("Failed to install yt-dlp plugin from setup", slog.String("error", err.Error()))
			} else {
				s.log.Info("yt-dlp plugin installed from setup wizard", slog.Int("port", port))
			}
		},
		OnRestart: func() { s.triggerRestart("setup") },
	}, s.configStore)
	routes.FFmpegRoutes(s.r, &routes.FFmpegDeps{
		Store:     s.configStore,
		RateLimit: s.apiRL,
		Logger:    s.log,
	})
	routes.LogRoutes(s.r, s.log.GetRecentLines)
	importCleanup := routes.ImportRoutes(s.r, s.db, s.configStore)
	routes.CookieRoutes(s.r, s.cookieRefresh, s.autoCookieSvc, s.getActivePlatforms, s.apiRL)
	routes.YtdlpRoutes(s.r, func() int {
		// Per-request port resolution: the listener binds after route wiring,
		// and with auto-pick (port 0) only ActualPort knows the real value.
		if s.webServer != nil && s.webServer.ActualPort > 0 {
			return s.webServer.ActualPort
		}
		var port int
		s.configStore.Read(func(c *config.MoomboxConfig) {
			port = c.Network.Port
		})
		return port
	}, s.cfg.Network.HTTPSEnabled)
	routes.RestartRoute(s.r, func() { s.triggerRestart("API") })
	routes.UpdateRoutes(s.r, &routes.UpdateRouteDeps{
		Updater:   s.upd,
		Version:   version,
		OnRestart: func() { s.triggerRestart("update") },
		OnFound: func(release *updater.ReleaseInfo) {
			s.wsHub.Broadcast("update_available", release)
			select {
			case s.tuiUpdateStatusCh <- tui.UpdateStatusMsg{
				Version:      release.Version,
				TagName:      release.TagName,
				ReleaseNotes: release.ReleaseNotes,
			}:
			default:
			}
		},
	}, s.configStore)
	authDeps := &routes.AuthRoutesDeps{
		Auth:       s.authSvc,
		DB:         s.db,
		LoginRL:    s.loginRL,
		PasswordRL: s.passwordRL,
		Logger:     s.log,
	}
	routes.AuthRoutes(s.r, authDeps, s.configStore)
	routes.ClientTokenRoutes(s.r, authDeps)
	routes.WatchRoutes(s.r, s.db)

	return importCleanup
}

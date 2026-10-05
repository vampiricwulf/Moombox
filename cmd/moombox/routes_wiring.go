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
	// Before any handler exists: the restart, update-apply and setup
	// post-save goroutines report a recovered panic through this. It was
	// never installed, so those panics went to raw stderr — not the log
	// file, and over the TUI's screen when it runs.
	routes.SetPanicLogger(s.log)
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
		DB:        s.db,
		YT:        &ytFormatAdapter{svc: s.ytService, store: s.configStore},
		Logger:    s.log,
		RateLimit: s.apiRL,
	})
	routes.StatusRoute(s.r, &routes.StatusRouteDeps{
		Version:            version,
		StartTime:          s.startTime,
		GetActivePlatforms: s.getActivePlatforms,
		// Both wire shapes come from routes' own projections rather than
		// being rebuilt here. Three hand-written copies of the cookieStatus
		// map existed across two packages and a field added to two of them
		// leaves this endpoint — the one the dashboard reads on every load
		// and reconnect — quietly serving the old meaning.
		GetCookieStatus: func() map[string]any {
			return routes.CookieStatusPayload(s.cookieRefresh.GetStatus())
		},
		GetTwitchAuthStatus: func() map[string]any {
			return routes.TwitchAuthStatusPayload(s.cookieRefresh.GetStatus())
		},
		GetAutoCookieReloginNeeded: func() any {
			// ReloginStatus, not GetStatus: this closure reads nothing but
			// NeedsManualRelogin, and GetStatus would run its browser and
			// registry detection scan for a field that never uses it. The
			// dashboard does NOT poll /api/status on a timer — it fetches it on
			// init, on every WebSocket (re)connect, after a settings save and
			// after an interactive cookie setup — so this is not the hottest
			// route in the server; it is simply a filesystem-and-registry scan
			// nothing here reads, once per open tab per reconnect.
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
		OnLogLevelChange: s.applyConfiguredLogLevel,
		OnMaxParallelChange: func(n int) {
			s.dlWorker.SetParallelDownloads(n)
		},
		OnHideFinishedAgeChanged: s.broadcastHideFinishedAge,
		OnChannelChange:          s.kickMonitors,
		OnMonitorIntervalChange:  s.kickMonitors,
		OnActivePlatformsChange:  s.resendTUICookieStatus,
		OnDiskSettingsChange:     s.requestDiskRecheck,
		OnSegmentWorkersChange:   s.warnSegmentWorkers,
		OnNotificationsChange: func() {
			// Hot-reload notification targets so edits apply immediately —
			// previously they silently required a restart nothing asked for.
			s.notifyMgr.Reload(s.configStore.Snapshot())
		},
		OnGoSoftLimitChange:         s.applyGoSoftLimit,
		OnTrustForwardedProtoChange: s.applyTrustForwardedProto,
		OnFfmpegPathChange:          s.applyFfmpegPath,
		OnReorderBudgetChange:       s.applyReorderBudget,
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
	routes.ChannelRoutes(s.r, s.configStore, s.kickMonitors, s.apiRL)
	routes.FileRoutes(s.r, &routes.FileRoutesDeps{
		DB:     s.db,
		Store:  s.configStore,
		Logger: s.log,
	})
	routes.HistoryRoutes(s.r, &routes.HistoryRoutesDeps{
		DB:     s.db,
		Logger: s.log,
	})
	routes.TrimRoutes(s.r, s.db, s.trimSvc, s.apiRL)
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
		// The same hot reload the config PUT gets. The FFmpeg overlay's
		// "check this path" flow saves and then calls initializeApp() with no
		// restart in between, so without this the muxers keep the boot value
		// that had just failed (WEB-2).
		OnFfmpegPathChange: s.applyFfmpegPath,
	})
	routes.LogRoutes(s.r, s.log.GetRecentLines)
	importCleanup := routes.ImportRoutes(s.r, s.db, s.configStore)
	routes.CookieRoutes(s.r, s.cookieRefresh, s.autoCookieSvc, s.getActivePlatforms, s.apiRL)
	routes.YtdlpRoutes(s.r, s.currentWebPort, s.cfg.Network.HTTPSEnabled)
	routes.RestartRoute(s.r, func() { s.triggerRestart("API") })
	routes.UpdateRoutes(s.r, &routes.UpdateRouteDeps{
		Updater:   s.upd,
		Version:   version,
		Logger:    s.log,
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
		// The dashboard skipped this release, or its check found nothing
		// newer: the TUI's badge and every other open dashboard's must go out
		// too (announceUpdateCleared).
		OnCleared: func(tag string) {
			announceUpdateCleared(s.wsHub, s.tuiUpdateStatusCh, tag)
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

// broadcastHideFinishedAge pushes a hide_finished_age_days change to every
// dashboard: the config_update first, then the re-filtered job list.
//
// Send config_update FIRST so the Web UI's hideFinishedAgeDays is already up
// to date by the time the jobs_update payload (filtered with the new
// threshold) arrives. Otherwise the per-client FIFO queue would deliver
// jobs_update first, and the Web UI's archive re-eval would run with the
// stale threshold and undo the server's widening on a threshold increase.
// Capture the threshold ONCE and reuse it for both the config_update payload
// and the job filtering below. Re-reading the store for the filter (via
// filterJobsByAge) would race a concurrent config change and could broadcast
// a hideFinishedAgeDays that disagrees with the threshold the jobs_update was
// filtered by.
//
// TWO callers, deliberately one method: the Web PUT's
// ConfigRoutesCallbacks.OnHideFinishedAgeChanged above and the TUI's
// OnSaveConfig (tui_wiring.go). Before that pairing a TUI save never reached
// the dashboard at all (CORE-11).
//
// The change gate is here rather than at the call sites for the same reason:
// the Web route gates before it calls (newHideAge != oldHideAge), but the TUI
// cannot — the settings model mutates the live config before OnSaveConfig
// runs, so there is no pre-mutation value on that side. s.hideAgeBroadcast
// carries it instead, written by whichever caller last published, so an
// unrelated TUI save (log level, output directory) costs nothing and a real
// change is never swallowed.
// Serialised end to end by hideAgeBroadcastMu (see runstate.go): the gate is
// a load-compare-broadcast-store sequence whose two callers are independent,
// and interleaving them can leave the memo holding a value no dashboard was
// told. The lock covers the config read, the jobs read and both broadcasts;
// nothing under it writes the config store.
func (s *runState) broadcastHideFinishedAge() {
	s.hideAgeBroadcastMu.Lock()
	defer s.hideAgeBroadcastMu.Unlock()

	var hideAge float64
	s.configStore.Read(func(c *config.MoomboxConfig) {
		hideAge = c.Monitors.HideFinishedAgeDays.Value
	})
	if last := s.hideAgeBroadcast.Load(); last != nil && *last == hideAge {
		return
	}
	s.wsHub.Broadcast("config_update", map[string]any{"hideFinishedAgeDays": hideAge})
	jobs, err := s.db.GetAllJobs()
	if err != nil {
		// Never broadcast the empty slice a failed read returns:
		// jobs_update REPLACES the dashboard's list, so a transient DB
		// error would blank every open dashboard until some unrelated
		// event refilled it. The config_update above is already out, so
		// the clients re-filter the list they hold with the new threshold.
		//
		// The memo is deliberately NOT updated here: a client re-filter
		// cannot widen the list a raised threshold makes longer (only the
		// server holds the archived rows), so the identical save the
		// operator retries must reach this read again rather than be
		// swallowed as "unchanged".
		s.log.Warn("Could not read jobs for the hide_finished_age_days broadcast — dashboards keep their current list",
			slog.String("error", err.Error()))
		return
	}
	s.wsHub.BroadcastJobsUpdate(filterJobsByAgeThreshold(jobs, hideAge))
	// Recorded only once the dashboards really have the new list.
	s.hideAgeBroadcast.Store(&hideAge)
}

// currentWebPort resolves the port this process is actually serving on.
//
// A GETTER, not a value: the listener binds after route wiring, and with
// auto-pick (port 0) only ActualPort knows the real number — a captured value
// would write a plugin file pointing at ":0" forever.
//
// Shared with the TUI's E Y overlay (tui_wiring.go), which asks the same
// question about the same plugin file and must not answer it differently.
func (s *runState) currentWebPort() int {
	if s.webServer != nil && s.webServer.ActualPort() > 0 {
		return s.webServer.ActualPort()
	}
	var port int
	s.configStore.Read(func(c *config.MoomboxConfig) {
		port = c.Network.Port
	})
	return port
}

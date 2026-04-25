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
		s.wsHub,
	)
	routes.FormatRoutes(s.r, &routes.FormatRoutesDeps{
		DB:  s.db,
		Cfg: s.cfg,
		YT:  &ytFormatAdapter{svc: s.ytService, cfg: s.cfg},
	})
	routes.StatusRoute(s.r, &routes.StatusRouteDeps{
		Cfg:                s.cfg,
		Version:            version,
		StartTime:          s.startTime,
		GetActivePlatforms: s.getActivePlatforms,
		GetCookieStatus: func() map[string]any {
			status := s.cookieRefresh.GetStatus()
			return map[string]any{
				"found":         status.HasYouTubeCookies,
				"authenticated": status.YouTubeAuthenticated,
			}
		},
		GetTwitchAuthStatus: func() map[string]any {
			status := s.cookieRefresh.GetStatus()
			return map[string]any{
				"authenticated": status.TwitchAuthenticated,
			}
		},
		GetAutoCookieReloginNeeded: func() any {
			return s.autoCookieSvc.GetStatus().NeedsManualRelogin
		},
		GetNextFeedCheck:   s.feedMon.GetNextCheckAt,
		GetNextDecapiCheck: s.decapiMon.GetNextCheckAt,
		GetNextTwitchCheck: s.twitchMon.GetNextCheckAt,
	})
	routes.ConfigRoutes(s.r, s.configStore, &routes.ConfigRoutesCallbacks{
		OnLogLevelChange: func(level string) {
			s.log.SetLevel(level)
		},
		OnMaxParallelChange: func(n int) {
			s.dlWorker.SetParallelDownloads(n)
		},
		OnHideFinishedAgeChanged: func() {
			// Re-broadcast job list with updated archive threshold
			jobs, _ := s.db.GetAllJobs()
			s.wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, s.configStore))
		},
		OnChannelChange: s.kickMonitors,
	})
	routes.ChannelRoutes(s.r, s.configStore, s.kickMonitors)
	routes.FileRoutes(s.r, &routes.FileRoutesDeps{
		DB:     s.db,
		Cfg:    s.cfg,
		Logger: s.log,
	})
	routes.TrimRoutes(s.r, s.db, s.trimSvc)
	routes.StatsRoutes(s.r, &routes.StatsRouteDeps{
		DB:  s.db,
		Cfg: s.cfg,
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
		Cfg:   s.cfg,
		CfgMu: s.webServer.CfgMu(),
		SaveConfig: func(c *config.MoomboxConfig) error {
			return config.Save(c, s.configPath)
		},
		RateLimit: s.apiRL,
		Logger:    s.log,
	})
	routes.LogRoutes(s.r, s.log.GetRecentLines)
	importCleanup := routes.ImportRoutes(s.r, s.db, s.configStore, s.apiRL)
	routes.CookieRoutes(s.r, s.cookieRefresh, s.autoCookieSvc, s.getActivePlatforms)
	routes.YtdlpRoutes(s.r, s.cfg.Network.Port, s.cfg.Network.HTTPSEnabled)
	routes.RestartRoute(s.r, func() { s.triggerRestart("API") })
	routes.UpdateRoutes(s.r, &routes.UpdateRouteDeps{
		Updater:    s.upd,
		Version:    version,
		Cfg:        s.cfg,
		ConfigPath: s.configPath,
		OnRestart:  func() { s.triggerRestart("update") },
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
	}, s.webServer.CfgMu())
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

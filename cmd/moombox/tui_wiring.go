package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// runTUI starts the BubbleTea TUI, wires every callback (job actions,
// trim service, orphan scanner, client-token management, setup wizard,
// FFmpeg check, cookie controls, update check, etc.), runs the TUI
// event loop, and cleans up afterwards (unsubscribes, drop-count reports,
// stdout restore). Blocks until the TUI is quit via the user or via a
// triggerRestart cancellation.
func (s *runState) runTUI() {
	app := tui.NewApp()
	s.quitTUI = app.QuitTUI // allow API restart to exit TUI

	// Pass config reference, config mutex, and version for settings panel
	app.SetConfig(s.cfg)
	app.SetCfgMu(&s.cfgMu)
	app.SetConfigStore(s.configStore)
	app.SetVersion(version)
	app.SetInternalToken(s.webServer.InternalToken())
	app.IsFirstRun = !s.cfg.ConfigLoaded

	// Wire TUI callbacks
	app.OnAddVideo = func(url string) {
		// Job creation is handled via HTTP POST in addVideoCmd; this is just for logging
		s.log.Info("Add video from TUI", slog.String("url", url))
	}
	app.OnCancelJob = func(jobID string) {
		s.dlWorker.CancelJob(jobID)
	}
	app.OnDeleteJob = func(jobID string) {
		if err := s.db.DeleteJob(jobID); err != nil {
			s.log.Error("Failed to delete job", slog.String("error", err.Error()))
		}
	}
	app.OnResumeJob = func(jobID string) {
		s.dlWorker.ResumeJob(jobID)
	}
	app.OnReinitializeJob = func(jobID string) {
		s.dlWorker.ReinitializeJob(jobID)
	}
	app.OnMuxJob = func(jobID string) error {
		return s.dlWorker.MuxJob(jobID)
	}
	app.HasStagingFiles = func(jobID string) bool {
		s.cfgMu.RLock()
		base := s.cfg.Paths.EffectiveStagingDir()
		s.cfgMu.RUnlock()
		return worker.HasStagingFiles(base, jobID)
	}
	app.HasSegmentFiles = func(jobID string) bool {
		s.cfgMu.RLock()
		base := s.cfg.Paths.EffectiveStagingDir()
		s.cfgMu.RUnlock()
		return worker.HasSegmentFiles(base, jobID)
	}
	app.OnOpenFolder = func(jobID string) {
		job, err := s.db.GetJob(jobID)
		if err != nil || job == nil {
			return
		}

		var dir string
		if job.OutputFile != "" {
			dir = filepath.Dir(job.OutputFile)
		} else {
			// Fall back to staging directory for active jobs
			s.cfgMu.RLock()
			stagingBase := s.cfg.Paths.EffectiveStagingDir()
			s.cfgMu.RUnlock()
			dir = filepath.Join(stagingBase, job.ID)
			if _, err := os.Stat(dir); err != nil {
				return // staging dir doesn't exist yet
			}
		}

		// Open folder in file manager (cross-platform)
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "windows":
			cmd = exec.Command("explorer", dir)
		case "darwin":
			cmd = exec.Command("open", dir)
		default:
			cmd = exec.Command("xdg-open", dir)
		}
		if err := cmd.Start(); err != nil {
			s.log.Debug("Failed to open folder in file manager", slog.String("error", err.Error()))
		}
	}
	app.OnCreateTrim = func(jobID string, startSec, endSec float64, onProgress func(float64)) (string, string) {
		job, err := s.db.GetJob(jobID)
		if err != nil || job == nil {
			s.log.Error("Failed to get job for trim", slog.String("jobID", jobID))
			return "", "Failed to get job"
		}
		record, err := s.trimSvc.CreateTrim(context.Background(), job, startSec, endSec, onProgress)
		if err != nil {
			s.log.Error("Failed to create trim", slog.String("error", err.Error()))
			return "", err.Error()
		}
		return record.Filename, ""
	}
	app.OnDeleteTrim = func(jobID, trimID string) error {
		if err := s.trimSvc.DeleteTrim(jobID, trimID); err != nil {
			s.log.Error("Failed to delete trim", slog.String("error", err.Error()))
			return err
		}
		return nil
	}
	app.OnListOrphans = func() ([]tui.OrphanedFileEntry, error) {
		entries, err := worker.ScanOrphanedFiles(s.db, s.cfg)
		if err != nil {
			return nil, err
		}
		result := make([]tui.OrphanedFileEntry, len(entries))
		for i, e := range entries {
			result[i] = tui.OrphanedFileEntry{
				Path:      e.Path,
				RelPath:   e.RelPath,
				Type:      e.Type,
				Size:      e.Size,
				Modified:  e.Modified,
				JobID:     e.JobID,
				JobTitle:  e.JobTitle,
				JobStatus: e.JobStatus,
			}
		}
		return result, nil
	}
	app.OnDeleteOrphan = func(path string) error {
		return worker.DeleteOrphanedFile(path, s.db, s.cfg)
	}
	app.OnListClientTokens = func() ([]*database.ClientToken, error) {
		return s.db.ListClientTokens()
	}
	app.OnDeleteClientToken = func(id string) error {
		return s.db.DeleteClientToken(id)
	}
	app.OnSaveConfig = func(updatedCfg *config.MoomboxConfig) {
		if err := config.Save(updatedCfg, s.configPath); err != nil {
			s.log.Error("Failed to save config from TUI", slog.String("error", err.Error()))
		} else {
			s.log.Info("Config saved from TUI settings")
		}
		// Hot-reload runtime settings (match TS: refreshLogLevel + setMaxDownloadSlots)
		if updatedCfg.Logs.LogLevel != "" {
			s.log.SetLevel(updatedCfg.Logs.LogLevel)
		}
		if updatedCfg.Downloader.NumParallelDownloads > 0 {
			s.dlWorker.SetParallelDownloads(updatedCfg.Downloader.NumParallelDownloads)
		}
		// Kick monitors so they re-evaluate channels (may have been added/removed)
		s.kickMonitors()
	}
	app.OnRestart = func() { s.triggerRestart("TUI settings") }
	if s.upd != nil {
		app.OnCheckUpdate = func() (*tui.UpdateStatusMsg, error) {
			s.log.Info("Update check requested from TUI")
			release, err := s.upd.CheckForUpdate(context.Background())
			if err != nil {
				return nil, err
			}
			if release == nil {
				return nil, nil
			}
			routes.SharedUpdateInfo.Store(release)
			s.wsHub.Broadcast("update_available", release)
			return &tui.UpdateStatusMsg{
				Version:      release.Version,
				TagName:      release.TagName,
				ReleaseNotes: release.ReleaseNotes,
			}, nil
		}
		app.OnApplyUpdate = func(ver string) string {
			release := routes.SharedUpdateInfo.Load()
			if release == nil {
				return "no update info available"
			}
			s.log.Info("Update requested from TUI", slog.String("version", ver))
			if err := s.upd.ApplyUpdate(context.Background(), release); err != nil {
				s.log.Error("[Updater] Update failed", slog.String("error", err.Error()))
				return err.Error()
			}
			s.triggerRestart("TUI update")
			return ""
		}
		app.OnVerifySignature = func() error {
			return s.upd.VerifyCurrentSignature(context.Background())
		}
	}
	app.OnRecheckCookies = func() (bool, bool) {
		s.log.Info("Cookie recheck requested from TUI")
		s.cookieRefresh.CheckNow(context.Background())
		status := s.cookieRefresh.GetStatus()
		return status.YouTubeAuthenticated, status.TwitchAuthenticated
	}
	if s.cfg.Cookies.AutoEnabled {
		app.OnForceRefreshCookies = func() (bool, error) {
			s.log.Info("Browser cookie refresh requested from TUI")
			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer refreshCancel()
			ok, err := s.autoCookieSvc.RefreshCookies(refreshCtx)
			if err != nil {
				return false, err
			}
			s.cookieRefresh.CheckNow(context.Background())
			return ok, nil
		}
	}

	app.OnHashPassword = func(password string) string {
		hash, err := s.authSvc.HashPassword(password)
		if err != nil {
			s.log.Error("Failed to hash password", slog.String("error", err.Error()))
			return ""
		}
		return hash
	}
	app.OnVerifyPassword = func(password, hash string) bool {
		return s.authSvc.VerifyPassword(password, hash)
	}

	// Wire setup wizard callbacks (OnComplete saves config, OnInstallYtdlp writes plugin)
	app.SetSetupCallbacks(
		func(updatedCfg *config.MoomboxConfig) error {
			s.cfgMu.Lock()
			defer s.cfgMu.Unlock()
			return config.Save(updatedCfg, s.configPath)
		},
		func(port int, httpsEnabled bool) {
			if err := routes.InstallYtdlpPlugin(port, httpsEnabled); err != nil {
				s.log.Error("Failed to install yt-dlp plugin from setup", slog.String("error", err.Error()))
			} else {
				s.log.Info("yt-dlp plugin installed from setup wizard", slog.Int("port", port))
			}
		},
		func(platform string) error {
			if err := s.autoCookieSvc.StartSetup(platform); err != nil {
				s.log.Error("Failed to start auto-cookie setup", slog.String("platform", platform), slog.String("error", err.Error()))
				return err
			}
			return nil
		},
		func() (bool, bool, error) {
			finishCtx, finishCancel := context.WithTimeout(s.ctx, 60*time.Second)
			defer finishCancel()
			yt, tw, err := s.autoCookieSvc.FinishSetup(finishCtx)
			if err != nil {
				s.log.Error("Failed to finish auto-cookie setup", slog.String("error", err.Error()))
				return yt, tw, err
			}
			return yt, tw, nil
		},
		func() {
			s.autoCookieSvc.CancelSetup()
		},
		func() { s.triggerRestart("TUI setup wizard") },
	)

	// Wire setup wizard password hashing
	app.SetupWizHashPassword(func(password string) (string, error) {
		return s.authSvc.HashPassword(password)
	})

	// Wire setup wizard FFmpeg status check
	app.SetupWizFFmpegCheck(func() (bool, string) {
		path := s.cfg.Paths.FfmpegPath
		if path == "" {
			path = "ffmpeg"
		}
		valid, ver, _ := routes.CheckFFmpegCached(path)
		return valid, ver
	})

	// Wire FFmpeg check callbacks for TUI
	app.OnCheckFFmpeg = func(path string) (bool, string, string) {
		if path == "" {
			path = "ffmpeg"
		}
		return routes.CheckFFmpegCached(path)
	}
	app.OnPrepareInstall = routes.PrepareInstall
	app.OnConfirmInstall = routes.ConfirmInstall
	app.OnRejectInstall = routes.RejectInstall
	app.OnCheckPrereqs = func() (bool, bool) {
		chocoAvail := false
		wingetAvail := false
		if _, err := exec.LookPath("choco"); err == nil {
			chocoAvail = true
		}
		if _, err := exec.LookPath("winget"); err == nil {
			wingetAvail = true
		}
		return chocoAvail, wingetAvail
	}

	// Check FFmpeg on startup (after config is loaded)
	if s.cfg.ConfigLoaded {
		ffmpegPath := s.cfg.Paths.FfmpegPath
		if ffmpegPath == "" {
			ffmpegPath = "ffmpeg"
		}
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer checkCancel()
		if err := exec.CommandContext(checkCtx, ffmpegPath, "-version").Run(); err != nil {
			app.ShowFFmpegCheck()
		}
	}

	// Create async update channels for TUI
	jobUpdateCh := make(chan *database.Job, 100)
	jobsUpdateCh := make(chan []*database.Job, 10)
	logCh := make(chan string, 200)
	checkTimersCh := make(chan tui.CheckTimersMsg, 10)
	cookieStatusCh := make(chan tui.CookieStatusMsg, 5)

	app.SetUpdateChannels(jobUpdateCh, jobsUpdateCh, logCh, checkTimersCh, cookieStatusCh, s.tuiDiskStatusCh, s.tuiUpdateStatusCh)

	// Dropped-message counters — track silent drops on TUI channels
	var tuiDroppedJobs, tuiDroppedLogs atomic.Int64

	// Push initial disk status to TUI
	if ds := routes.SharedDiskStatus.Load(); ds != nil {
		select {
		case s.tuiDiskStatusCh <- tui.DiskStatusMsg{Free: ds.Free, UsedPct: ds.UsedPct, Warn: ds.WarnLevel}:
		default:
		}
	}

	// Forward DB events to TUI channels
	unsubTUIJobUpdate := s.db.OnJobUpdate(func(job *database.Job) {
		select {
		case jobUpdateCh <- job:
		default:
			tuiDroppedJobs.Add(1)
		}
	})
	unsubTUIJobsChange := s.db.OnJobsChange(func(jobs []*database.Job) {
		select {
		case jobsUpdateCh <- jobs:
		default:
		}
	})

	// Forward log lines to TUI
	tuiLogSub := s.log.Subscribe()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("[Main] Panic in TUI log forwarder", "panic", fmt.Sprint(r))
			}
		}()
		for line := range tuiLogSub {
			select {
			case logCh <- line:
			default:
				tuiDroppedLogs.Add(1)
			}
		}
	}()

	// Backfill TUI with logs emitted before subscription
	app.BackfillLogs(s.log.GetRecentLines())

	// Forward monitor schedule events to TUI via the atomic pointers installed
	// by monitor_callbacks.wireMonitorCallbacks. Store() is race-free against
	// the monitor goroutines' Load()s.
	makeTUISchedule := func(makeMsg func(time.Time) tui.CheckTimersMsg) func(int64) {
		return func(nextCheckAt int64) {
			t := time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
			select {
			case checkTimersCh <- makeMsg(t):
			default:
			}
		}
	}
	feedFn := makeTUISchedule(func(t time.Time) tui.CheckTimersMsg {
		return tui.CheckTimersMsg{NextFeedCheck: t}
	})
	decapiFn := makeTUISchedule(func(t time.Time) tui.CheckTimersMsg {
		return tui.CheckTimersMsg{NextDecapiCheck: t}
	})
	twitchFn := makeTUISchedule(func(t time.Time) tui.CheckTimersMsg {
		return tui.CheckTimersMsg{NextTwitchCheck: t}
	})
	s.feedTUISchedule.Store(&feedFn)
	s.decapiTUISchedule.Store(&decapiFn)
	s.twitchTUISchedule.Store(&twitchFn)

	// Send initial timer values — monitors fire OnSchedule during Start()
	// before the TUI wrappers above are installed, so the TUI misses those.
	for _, entry := range []struct {
		getNext func() int64
		makeMsg func(time.Time) tui.CheckTimersMsg
	}{
		{s.feedMon.GetNextCheckAt, func(t time.Time) tui.CheckTimersMsg { return tui.CheckTimersMsg{NextFeedCheck: t} }},
		{s.decapiMon.GetNextCheckAt, func(t time.Time) tui.CheckTimersMsg { return tui.CheckTimersMsg{NextDecapiCheck: t} }},
		{s.twitchMon.GetNextCheckAt, func(t time.Time) tui.CheckTimersMsg { return tui.CheckTimersMsg{NextTwitchCheck: t} }},
	} {
		if ms := entry.getNext(); ms > 0 {
			t := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
			select {
			case checkTimersCh <- entry.makeMsg(t):
			default:
			}
		}
	}

	// Wire cookie status to TUI. Parameter `auth` is the incoming status —
	// named differently from `s` (receiver) to avoid the pre-refactor shadow.
	authStatusToTUI := func(auth cookies.AuthStatus) {
		var yt, tw tui.CookieStatus
		switch {
		case auth.YouTubeAuthenticated:
			yt = tui.CookieStatusOK
		case auth.HasYouTubeCookies:
			yt = tui.CookieStatusCookiesOnly
		}
		if auth.TwitchAuthenticated {
			tw = tui.CookieStatusOK
		}
		// Check auto-cookie relogin state
		relogin := s.autoCookieSvc.GetStatus().NeedsManualRelogin
		if relogin.YouTube {
			yt = tui.CookieStatusRelogin
		}
		if relogin.Twitch {
			tw = tui.CookieStatusRelogin
		}
		s.cfgMu.RLock()
		ytActive, twActive := config.GetActivePlatforms(s.cfg)
		s.cfgMu.RUnlock()
		select {
		case cookieStatusCh <- tui.CookieStatusMsg{YT: yt, TW: tw, YTActive: ytActive, TWActive: twActive}:
		default:
		}
	}
	// Store TUI-side callback in the atomic slot; the dispatcher wired to
	// cookieRefresh.OnAuthChange (set before cookieRefresh.Start()) loads it
	// lock-free on each auth change.
	tuiAuthFn := func(auth cookies.AuthStatus) {
		authStatusToTUI(auth)
	}
	s.authChangeTUI.Store(&tuiAuthFn)
	// Send initial cookie status
	authStatusToTUI(s.cookieRefresh.GetStatus())

	// Send initial job list
	if jobs, err := s.db.GetAllJobs(); err == nil {
		select {
		case jobsUpdateCh <- jobs:
		default:
		}
	}

	// Wire connectivity state to TUI (uses program.Send, not a channel)
	unsubConnTUI := s.connMon.OnStateChange(func(online bool) {
		app.Send(tui.ConnectivityMsg{Online: online})
	})

	// Suppress stdout logging while TUI runs — BubbleTea owns the alternate
	// screen, and raw log writes corrupt the display. The TUI log panel
	// receives logs via Subscribe() instead.
	s.log.SuppressStdout()

	// Run TUI (blocks until quit)
	if err := tui.Run(app); err != nil {
		s.log.Error("TUI error", slog.String("error", err.Error()))
	}

	s.log.RestoreStdout()

	// Cleanup TUI channels — cancel first so workers stop sending, then
	// unsubscribe. Don't close channels: non-blocking sends mean no
	// goroutine will block, and GC handles cleanup.
	s.cancel() // TUI quit triggers shutdown
	s.log.Unsubscribe(tuiLogSub)
	unsubTUIJobUpdate()
	unsubTUIJobsChange()
	unsubConnTUI()

	// Report dropped messages (helps diagnose missed TUI updates)
	if n := tuiDroppedJobs.Load(); n > 0 {
		s.log.Warn("TUI dropped job update messages", slog.Int64("count", n))
	}
	if n := tuiDroppedLogs.Load(); n > 0 {
		s.log.Warn("TUI dropped log messages", slog.Int64("count", n))
	}
}

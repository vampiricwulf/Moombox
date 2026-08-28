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

// cookieBadgeFor projects one platform's AuthStatus triple onto the status-bar
// tier. Shared by both platforms rather than written twice: the two arms had
// already diverged — YouTube could reach CookiesOnly and Twitch could not,
// because AuthStatus had no HasTwitchCookies to read.
//
// The order is the contract:
//
//   - authenticated wins outright. It is the only state that lets us do
//     authenticated work, and it implies RefreshOK.
//   - no cookies at all reports NONE, whatever the verdict says. A platform
//     that was never configured is not a platform whose cookies failed, and
//     the check returns a conclusive "not authenticated" for it.
//   - RefreshFailed with cookies present is COOKIES ONLY — the red, always-
//     visible alert. Credentials exist and the site rejected them.
//   - everything else is UNKNOWN: cookies exist and this check learned nothing
//     about them. That is the state that used to fall into CookiesOnly and
//     render a transient network fault as a dead session.
//
// `hasCookies` is the LOOSE predicate on both sides (HasAnyYouTubeAuthCookie /
// HasAnyTwitchAuthCookie), so a half-cleared jar reads as configured rather
// than as never-set-up — see AuthStatus and twitchAuthCookieNames.
func cookieBadgeFor(authenticated, hasCookies bool, verdict cookies.RefreshVerdict) tui.CookieStatus {
	switch {
	case authenticated:
		return tui.CookieStatusOK
	case !hasCookies:
		return tui.CookieStatusNone
	case verdict == cookies.RefreshFailed:
		return tui.CookieStatusCookiesOnly
	default:
		return tui.CookieStatusUnknown
	}
}

// runTUI starts the BubbleTea TUI, wires every callback (job actions,
// trim service, orphan scanner, client-token management, setup wizard,
// FFmpeg check, cookie controls, update check, etc.), runs the TUI
// event loop, and cleans up afterwards (unsubscribes, drop-count reports,
// stdout restore). Blocks until the TUI is quit via the user or via a
// triggerRestart cancellation.
func (s *runState) runTUI() {
	app := tui.NewApp()
	quit := app.QuitTUI
	s.quitTUI.Store(&quit) // allow API restart to exit TUI

	// Pass config reference, config Store, and version for settings panel.
	// SetConfigStore also captures the cfg pointer used for direct-field
	// writes in the settings model — see App.SetConfigStore.
	app.SetConfig(s.cfg)
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
		job, err := s.db.GetJob(jobID)
		if err != nil || job == nil {
			// Already gone; nothing to do.
			return
		}
		switch job.Status {
		case database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusMuxing:
			s.dlWorker.CancelJob(jobID)
			if !s.dlWorker.WaitForJobExit(jobID, 5*time.Second) {
				s.log.Warn("TUI delete: job did not exit within timeout; removing anyway", "jobID", jobID)
			}
		}
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
		var base string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			base = c.Paths.EffectiveStagingDir()
		})
		return worker.HasStagingFiles(base, jobID)
	}
	app.HasSegmentFiles = func(jobID string) bool {
		var base string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			base = c.Paths.EffectiveStagingDir()
		})
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
			var stagingBase string
			s.configStore.Read(func(c *config.MoomboxConfig) {
				stagingBase = c.Paths.EffectiveStagingDir()
			})
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
		// Snapshot, not s.cfg: the scan reads path fields while walking the
		// filesystem, racing configStore.Update on the live struct.
		entries, err := worker.ScanOrphanedFiles(s.db, s.configStore.Snapshot())
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
		return worker.DeleteOrphanedFile(path, s.db, s.configStore.Snapshot())
	}
	app.OnListOrphanedHistory = func() ([]tui.OrphanedHistoryEntry, error) {
		entries, err := s.db.ListOrphanedHistory()
		if err != nil {
			return nil, err
		}
		result := make([]tui.OrphanedHistoryEntry, len(entries))
		for i, e := range entries {
			result[i] = tui.OrphanedHistoryEntry{VideoID: e.VideoID, AddedAt: e.AddedAt}
		}
		return result, nil
	}
	app.OnDeleteHistoryEntry = func(videoID string) error {
		_, err := s.db.DeleteHistoryEntries([]string{videoID})
		return err
	}
	app.OnListClientTokens = func() ([]*database.ClientToken, error) {
		return s.db.ListClientTokens()
	}
	app.OnDeleteClientToken = func(id string) error {
		return s.db.DeleteClientToken(id)
	}
	app.OnSaveConfig = func(updatedCfg *config.MoomboxConfig) {
		// Serialize on the store lock like every other saver (web routes,
		// Store.Update-driven background saves, and the setup-wizard callback
		// below). config.Save writes through a shared temp file and encodes the
		// live cfg struct; without the lock a concurrent background save (e.g.
		// cookie refresh) interleaves into the same temp file and renames a
		// byte-spliced config.toml over the real one.
		mu := s.configStore.RWMutex()
		mu.Lock()
		saveErr := config.Save(updatedCfg, s.configPath)
		mu.Unlock()
		if saveErr != nil {
			s.log.Error("Failed to save config from TUI", slog.String("error", saveErr.Error()))
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
		// Notification targets hot-reload (mirrors the web route's
		// OnNotificationsChange). Unconditional — the rebuild is a few URL
		// parses, cheaper than diffing the section. Snapshot, NOT
		// updatedCfg: that's the live *MoomboxConfig pointer, and reading
		// its Notifications slice unlocked would race a concurrent web
		// PUT /api/config whole-struct store.
		s.notifyMgr.Reload(s.configStore.Snapshot())
		// Kick monitors so they re-evaluate channels (may have been added/removed)
		s.kickMonitors()
	}
	app.OnRestart = func() { s.triggerRestart("TUI settings") }
	app.OnForceCheck = func() {
		if s.kickMonitors != nil {
			s.kickMonitors()
		}
	}
	app.OnBackfillRescan = func() {
		if s.backfillRescan != nil {
			s.backfillRescan()
		}
	}
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
		app.OnFetchReleaseNotes = func(version string) (string, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			info, err := s.upd.FetchReleaseNotes(ctx, version)
			if err != nil {
				return "", "", err
			}
			return info.TagName, info.ReleaseNotes, nil
		}
	}
	app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict) {
		s.log.Info("Cookie recheck requested from TUI")
		s.cookieRefresh.CheckNow(context.Background())
		status := s.cookieRefresh.GetStatus()
		// The verdicts, not the booleans. Both booleans are false for a check
		// that could not reach the site, and the feedback line built on them
		// reported "not authenticated" — a conclusion the check never drew.
		return status.YouTubeVerification, status.TwitchVerification
	}
	if s.cfg.Cookies.AutoEnabled {
		app.OnForceRefreshCookies = func() (cookies.RefreshResult, error) {
			s.log.Info("Browser cookie refresh requested from TUI")
			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer refreshCancel()
			// The whole result, not a flattened bool. R F is a key the operator
			// presses to ask whether the browser refresh works, and three of
			// the four answers it can get are distinct: the cookies on disk
			// still authenticate (Overall), this pass produced them (Renewed),
			// and this pass did any work at all (Ran). Flattening lost the
			// last two — reporting success for a refresh that did nothing, and
			// reporting a verification failure for a pass that never looked.
			result, err := s.autoCookieSvc.RefreshCookiesDetailed(refreshCtx)
			if err != nil {
				return result, err
			}
			s.cookieRefresh.CheckNow(context.Background())
			return result, nil
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
			mu := s.configStore.RWMutex()
			mu.Lock()
			defer mu.Unlock()
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
		// The whole result, not a bool pair. A sign-in the site could not be
		// reached to confirm is ACCEPTED but not verified, and the pair reports
		// it identically to a verified one — so the wizard said "configured"
		// about cookies nothing had checked. See cookies.SetupResult.
		//
		// The 60 s cap is load-bearing beyond this call: the server-side setup
		// grace window is priced against it. Do not raise it to buy a slow
		// finish more time.
		func() (cookies.SetupResult, error) {
			finishCtx, finishCancel := context.WithTimeout(s.ctx, 60*time.Second)
			defer finishCancel()
			result, err := s.autoCookieSvc.FinishSetupDetailed(finishCtx)
			if err != nil {
				s.log.Error("Failed to finish auto-cookie setup", slog.String("error", err.Error()))
				return result, err
			}
			return result, nil
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
	jobUpdateCh := make(chan *database.JobChange, 100)
	jobAddedCh := make(chan *database.JobAdded, 100)
	jobDeletedCh := make(chan *database.JobDeleted, 100)
	jobTrimsChangedCh := make(chan *database.Job, 50)
	jobsUpdateCh := make(chan []*database.Job, 10)
	logCh := make(chan string, 200)
	checkTimersCh := make(chan tui.CheckTimersMsg, 10)
	cookieStatusCh := make(chan tui.CookieStatusMsg, 5)

	app.SetUpdateChannels(jobUpdateCh, jobAddedCh, jobDeletedCh, jobTrimsChangedCh, jobsUpdateCh, logCh, checkTimersCh, cookieStatusCh, s.tuiDiskStatusCh, s.tuiBackfillCh, s.tuiUpdateStatusCh)

	// Dropped-message counters — track silent drops on TUI channels
	var tuiDroppedJobs, tuiDroppedLogs atomic.Int64

	// Push initial disk status to TUI
	if ds := routes.SharedDiskStatus.Load(); ds != nil {
		select {
		case s.tuiDiskStatusCh <- tui.DiskStatusMsg{Free: ds.Free, UsedPct: ds.UsedPct, Warn: ds.WarnLevel}:
		default:
		}
	}

	// Seed the backfill progress snapshot (the disk seed's shape): a scan
	// started before the TUI came up — mid-flight attach is the common case
	// during a long scan (§11) — is visible immediately instead of after the
	// next page event.
	s.backfillMu.Lock()
	for chID, p := range s.backfillProgress {
		select {
		case s.tuiBackfillCh <- tui.BackfillStatusMsg{Channel: chID, Tab: p.Tab, Pages: p.Pages, State: p.State}:
		default:
		}
	}
	s.backfillMu.Unlock()

	// Forward DB events to TUI channels. OnJobChange (not OnJobUpdate)
	// gives us the changed-columns list, which the TUI uses to gate
	// expensive list/detail rebuilds (see hasDisplayChange in
	// app_update.go). DECISIONS #21 / audit tui.md F20.
	unsubTUIJobUpdate := s.db.OnJobChange(func(ev *database.JobChange) {
		select {
		case jobUpdateCh <- ev:
		default:
			tuiDroppedJobs.Add(1)
		}
	})
	// OnJobAdded subscriber: AddJob no longer fires OnJobsChange (the
	// writer-side dispatch was dropped); the TUI now learns of new jobs
	// through this dedicated lifecycle event and the handler appends to
	// the task list instead of clearing + rebuilding from a fresh
	// snapshot. DECISIONS #21 consumer migration.
	unsubTUIJobAdded := s.db.OnJobAdded(func(ev *database.JobAdded) {
		select {
		case jobAddedCh <- ev:
		default:
			tuiDroppedJobs.Add(1)
		}
	})
	// OnJobDeleted subscriber: DeleteJob no longer fires OnJobsChange
	// (writer-side dispatch dropped). The TUI's surgical-removal path
	// (handleJobDeleted) drops just the affected ID from local state
	// instead of clearing+rebuilding from a full-list snapshot.
	// DECISIONS #21.
	unsubTUIJobDeleted := s.db.OnJobDeleted(func(ev *database.JobDeleted) {
		select {
		case jobDeletedCh <- ev:
		default:
			tuiDroppedJobs.Add(1)
		}
	})
	// OnTrimsChanged subscriber: AddTrim/DeleteTrim no longer fire
	// OnJobsChange (writer-side dispatch dropped). Re-fetch the
	// affected job here (so its Trims field is current) and forward
	// the refreshed pointer to the TUI handler. DECISIONS #21.
	unsubTUITrimsChanged := s.db.OnTrimsChanged(func(ev *database.TrimsChanged) {
		job, err := s.db.GetJob(ev.JobID)
		if err != nil || job == nil {
			return
		}
		select {
		case jobTrimsChangedCh <- job:
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
			// -1 = "checking now": forward the TUI's checking sentinel
			// verbatim so the header can render "…" (a lossy time.Unix(-1)
			// conversion would land near the epoch and read as "now").
			var t time.Time
			if nextCheckAt == tui.MonitorCheckingSentinelMs {
				t = tui.MonitorCheckingTime()
			} else {
				t = time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
			}
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
		yt := cookieBadgeFor(auth.YouTubeAuthenticated, auth.HasYouTubeCookies, auth.YouTubeVerification)
		tw := cookieBadgeFor(auth.TwitchAuthenticated, auth.HasTwitchCookies, auth.TwitchVerification)
		// Check auto-cookie relogin state. As of test.36 the relogin map
		// is keyed by lowercase platform name (audit cookies.md #44).
		relogin := s.autoCookieSvc.GetStatus().NeedsManualRelogin
		if relogin["youtube"] {
			yt = tui.CookieStatusRelogin
		}
		if relogin["twitch"] {
			tw = tui.CookieStatusRelogin
		}
		var ytActive, twActive bool
		s.configStore.Read(func(c *config.MoomboxConfig) {
			ytActive, twActive = config.GetActivePlatforms(c)
		})
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
	unsubTUIJobAdded()
	unsubTUIJobDeleted()
	unsubTUITrimsChanged()
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

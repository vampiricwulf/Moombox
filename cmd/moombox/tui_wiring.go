package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/web"
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
//   - an UNREADABLE cookie file reports FILE UNREADABLE, ahead of everything
//     but `authenticated`. It explains every symptom below it, and the two
//     states it used to render as — NONE for a jar that loaded nothing, or
//     UNKNOWN for one holding stale rows — both send the operator to the wrong
//     remedy. `authenticated` still wins: a jar that is doing authenticated
//     work is not a credential problem, whatever a later reload failed to do.
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
func cookieBadgeFor(authenticated, hasCookies, fileUnreadable bool, verdict cookies.RefreshVerdict) tui.CookieStatus {
	switch {
	case authenticated:
		return tui.CookieStatusOK
	case fileUnreadable:
		return tui.CookieStatusFileUnreadable
	case !hasCookies:
		return tui.CookieStatusNone
	case verdict == cookies.RefreshFailed:
		return tui.CookieStatusCookiesOnly
	default:
		return tui.CookieStatusUnknown
	}
}

// newTUIResync builds the replay for a dropped TUI job update. The forwarders
// in runTUI send non-blocking on their 100-slot channels and count the drop;
// nothing ever replayed it, so a dropped Downloading->Finished left a stale
// row for the rest of the session while user-interfaces.md claimed a resync
// existed (CORE-6).
//
// The returned func costs one failed compare-and-swap when nothing was
// dropped. When a drop IS pending it takes the flag, fetches a full snapshot
// and pushes it down the full-list channel the TUI already handles; if either
// the fetch or the push fails the flag is re-armed so the next caller tries
// again — clearing it there would turn one dropped update into a permanent
// divergence. Taking the flag first, and calling this only from a forwarder's
// SUCCESSFUL send, is what makes it ONE refresh per streak of drops rather
// than one per dropped message: it is a catch-up, not a poll, and it leaves
// the ~60 Hz forwarding path exactly as it was.
func newTUIResync(needed *atomic.Bool, jobsCh chan []*database.Job, getAll func() ([]*database.Job, error)) func() {
	return func() {
		if !needed.CompareAndSwap(true, false) {
			return
		}
		jobs, err := getAll()
		if err != nil {
			needed.Store(true)
			return
		}
		select {
		case jobsCh <- jobs:
		default:
			needed.Store(true)
		}
	}
}

// forwardOrDrop hands one DB event to a TUI channel without ever blocking the
// caller — the four job forwarders in runTUI run INLINE on the ~60 Hz
// UpdateJobFields writer goroutine — and carries the CORE-6 replay
// bookkeeping all four of them need identically.
//
// On a SUCCESSFUL send it runs resync, so a drop recorded by an earlier event
// is replayed as one full snapshot by the first event that gets through. On a
// drop it counts the message and arms the flag, warning once per STREAK: the
// compare-and-swap is that gate, and moving the Warn out of it would put one
// line per dropped message into the 200-slot log channel exactly when the TUI
// is already too far behind to drain it.
//
// One generic function rather than the four hand-copied seven-line blocks it
// replaces (the channels carry four different element types): the measured
// pin in tui_resync_test.go then drives THIS body instead of a copy of it,
// which is what lets it catch a warning moved out of the CAS or a coalescing
// check added ahead of the select.
func forwardOrDrop[T any](
	ch chan T,
	ev T,
	jobID string,
	resync func(),
	dropped *atomic.Int64,
	needed *atomic.Bool,
	log interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
) {
	select {
	case ch <- ev:
		resync()
	default:
		dropped.Add(1)
		if needed.CompareAndSwap(false, true) {
			log.Warn("TUI job update dropped — a full refresh is queued",
				slog.String("job", jobID))
		}
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
	app.IsFirstRun = !s.configLoaded()

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
	app.OnSetWatched = func(ids []string, watched bool) error {
		if len(ids) == 1 {
			// Single job: the per-job update path (OnJobUpdate), mirroring
			// the web's POST/DELETE /api/jobs/{id}/watched exactly — both
			// routes clear resume_position, whichever way watched flips.
			watchedVal := 0
			if watched {
				watchedVal = 1
			}
			if s.db.UpdateJobFields(ids[0], map[string]any{
				"watched":         watchedVal,
				"resume_position": nil,
			}) == nil {
				return fmt.Errorf("job %s not found", ids[0])
			}
			return nil
		}
		return s.db.BatchSetWatched(ids, watched)
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

		// Open folder in file manager. The dashboard's open-folder route and
		// this chord are the same action, so they share one switch
		// (web.OpenPathCommand) and one detach rule (web.StartDetached:
		// Windows releases the process handle, Unix reaps the child with Wait
		// — this path used to do neither).
		cmd := web.OpenPathCommand(dir)
		if err := web.StartDetached(cmd); err != nil {
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
	app.OnGetStats = func() (stats.Snapshot, error) {
		js, err := s.db.GetJobStats()
		if err != nil {
			return stats.Snapshot{}, err
		}
		// The same derivation and the same disk reading /api/stats serves.
		snap := stats.Build(js, routes.SharedDiskStatus.Load().Stats())
		snap.Uptime = time.Since(s.startTime)
		return snap, nil
	}
	app.OnSaveConfig = func(updatedCfg *config.MoomboxConfig) error {
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
			// Return before the hot-reload block: applying runtime settings
			// from a config that is not on disk would make the process and
			// the file diverge in the OTHER direction (CORE-4). The caller
			// reports the error and rolls the live struct back.
			return saveErr
		}
		s.log.Info("Config saved from TUI settings")
		// Invalidate the browser-detection caches on every TUI settings
		// save, unconditionally — not gated on browser_path/browser_type
		// actually changing. The settings model mutates the SAME
		// *config.MoomboxConfig the store holds live (Open stores the
		// store's own pointer; applyValues writes straight into it) before
		// this callback ever runs, so by the time updatedCfg reaches here
		// there is no pre-mutation snapshot left to diff against from
		// this side of the package boundary. Invalidating on every save
		// costs one extra detection scan on a rare, human-triggered
		// event; a missed invalidation would instead leave a stale
		// browser list for up to browserDetectCacheTTL, silently. See
		// the validate-browser-path handler in routes/cookies.go for the
		// other, precisely-targeted invalidation site.
		cookies.InvalidateBrowserDetection()
		// A TUI settings save must reach the dashboards too — the Web PUT
		// has always broadcast this, the TUI never did, so a threshold
		// changed in the terminal left every open dashboard filtering on
		// the old one (CORE-11). The same method both sides call, so the
		// two directions cannot drift. Called unconditionally like its
		// neighbours above, for the same reason: there is no pre-mutation
		// snapshot on this side to diff against. The method itself carries
		// the change gate the Web route applies before calling, so a save
		// that did not move the threshold still costs nothing.
		s.broadcastHideFinishedAge()
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
		snap := s.configStore.Snapshot()
		s.notifyMgr.Reload(snap)
		// The four read-once settings the web PUT re-applies via
		// ConfigRoutesCallbacks; applied unconditionally here for the same
		// reason the cache invalidation above is (no pre-mutation snapshot).
		s.applyGoSoftLimit(snap.Memory.GoSoftLimitMB)
		s.applyTrustForwardedProto(snap.Network.TrustForwardedProto)
		s.applyFfmpegPath(snap.Paths.FfmpegPath)
		s.applyReorderBudget(snap.Downloader)
		// Kick monitors so they re-evaluate channels (may have been added/removed)
		s.kickMonitors()
		return nil
	}
	// The FFmpeg overlay's own path applier. OnSaveConfig returns above
	// before the hot-reload block when the write is refused (the settings
	// panel rolls its config back after that return), but the overlay keeps
	// a validated path live for the session — so it re-applies the path
	// itself, whichever way the save went. Pinned by
	// tui_wiring_ffmpeg_callsite_test.go.
	app.OnFfmpegPathChange = s.applyFfmpegPath
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
		app.OnDismissUpdate = func(tag string) error {
			return routes.DismissUpdate(s.configStore, tag)
		}
	}
	app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
		s.log.Info("Cookie recheck requested from TUI")
		// The bool is deliberately ignored, and the feedback line is deliberately
		// unchanged when it is false.
		//
		// R C asks "what is the state of my cookies", and GetStatus answers that
		// whether or not THIS keypress is the pass that produced the answer — a
		// pass colliding with the 30-minute ticker returns the in-flight pass's
		// verdicts, at worst one snapshot old, and every one of them is still a
		// true statement about the credentials. The alternative would be to tell
		// the operator "a refresh was already running", and there is no vocabulary
		// for that here: cookies.RecheckReport renders per-platform VERDICTS and
		// nothing else, and cookies.RefreshDeclinedCauses is the browser
		// refresher's exhaustive, three-consumer-pinned set — not this one's.
		// Adding a cause is a separate change; see CheckNow's own doc.
		s.cookieRefresh.CheckNow(context.Background())
		status := s.cookieRefresh.GetStatus()
		// The verdicts, not the booleans. Both booleans are false for a check
		// that could not reach the site, and the feedback line built on them
		// reported "not authenticated" — a conclusion the check never drew.
		//
		// And the two error strings beside them, which is what the verdict on
		// its own cannot say: RefreshUnknown means "this check learned nothing"
		// and every cause produces the same three words. These fields had no
		// reader anywhere in the tree until Arc 8 Task 12a — the same status
		// object is projected onto the wire by CookieStatusPayload, which now
		// carries them too, so both surfaces answer R C's question with the same
		// two facts.
		//
		// Passed through untouched, and safely: every producer of these strings
		// is status-and-cause only and none interpolates a response body (the
		// rule is stated in full at CookieStatusPayload). The TUI clamps the
		// rendered line to the panel width — see fitFeedback — so length is the
		// renderer's problem, not this closure's.
		return status.YouTubeVerification, status.TwitchVerification,
			status.YouTubeError, status.TwitchError
	}
	// The other half of R C's answer, and it comes off a DIFFERENT SERVICE.
	// AutoCookieStatus.LastError records what the last browser refresh or
	// interactive setup concluded that the operator has to act on; the verdicts
	// above come from the in-process RefreshService's own check. The two can
	// disagree in the direction that matters — cookies.txt still authenticating
	// because the 30-minute session refresh keeps it alive, while the mechanism
	// that RENEWS it has been failing for days — and until now the TUI had no
	// surface for the second fact at all. The field's write policy
	// (internal/cookies/autocookies.go) is what makes it safe to show
	// permanently: one SET funnel, three earned clears, and cleanup() forbidden
	// from clearing.
	//
	// GetStatus, not a narrower accessor, and this is the caller the Task 4 note
	// carves out: the panel that renders the full status keeps GetStatus. Its
	// browser/registry detection scan is behind a 60 s cache and this runs once
	// per R C keypress, not per auth-change dispatch.
	app.OnAutoCookieLastError = func() string {
		if s.autoCookieSvc == nil {
			return ""
		}
		if le := s.autoCookieSvc.GetStatus().LastError; le != nil {
			return *le
		}
		return ""
	}
	// WIRED UNCONDITIONALLY. Do not put a cookies.auto_enabled gate back here,
	// in either shape — not around the assignment, and not as a live read
	// inside the closure.
	//
	// The assignment used to sit behind `if s.cfg.Cookies.AutoEnabled`, read
	// once at process start, and that did far more than skip a refresh. A nil
	// OnForceRefreshCookies does not make the chord inert, it DELETES it:
	// dispatchAction, buildMenuItems and the help overlay all test the field
	// (internal/tui/app_actions.go). So on an install with the flag off, an
	// operator told their cookies were dead had no key to press and no entry
	// naming one — the same hiding the previous arc removed from the Web
	// re-login indicator, on the same flag, one surface over.
	//
	// A live read that refused would only move that hiding behind a different
	// mechanism, and it would be wrong on its own terms. R F is "refresh these
	// cookies by the strongest means available", and with the headless browser
	// switched off the strongest available means is an immediate import from
	// the browser profile — which is exactly what an operator who has just
	// hand-updated that profile is asking for. The flag reaches the pass one
	// level down, where it drops the browser rather than the work; see
	// AutoCookieService.BrowserLaunchAllowed.
	//
	// R C is the other manual trigger — the in-process Go refresh — and is
	// never gated by anything. The two are not substitutes and neither may
	// stand in for the other.
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

		// Same shape as the auto-cookie recovery path in monitor_callbacks.go:
		// the browser pass has just rewritten cookies.txt, and a refresh already
		// in flight read the OLD file, so a skipped re-check leaves the status
		// bar behind until the next tick. R F's own feedback is built from
		// `result`, not from GetStatus, so what the operator is told about the
		// refresh itself is unaffected — the helper's line explains only the
		// badge.
		//
		// Deferred, so the Ran gate is evaluated independently of the error
		// return below: three of the eight refreshAborted() exits happen after
		// cookies.txt was rewritten, and returning on err first skipped exactly
		// the passes whose write nobody had compared.
		defer func() {
			if result.Ran {
				recheckAfterCookieWrite(context.Background(), s.checkNowFn(), s.log, "browser refresh")
			}
		}()

		return result, err
	}

	// R I — import a Netscape cookies.txt the operator exported elsewhere.
	//
	// The browser-free half of R L, and on a headless host the only
	// re-authentication route there is: StartSetup needs a browser it can put
	// on a screen, and this needs a file. It is the same gesture the Web
	// dashboard's import panel makes (POST /api/cookies/import), through the
	// same AutoCookieService.ImportCookies — verified per platform, and rolled
	// back for a platform the paste killed.
	//
	// GATED ON THE SERVICE EXISTING, not on a config flag. A nil callback does
	// not make the chord inert, it DELETES it — see the note on
	// OnForceRefreshCookies above — and that is exactly right here: with no
	// auto-cookie service there is nothing to import into, and offering a path
	// prompt that can only fail is worse than offering nothing. In production
	// the service is always present (initServices §15 constructs it before the
	// TUI is wired), so the chord is always there.
	//
	// THE FILE IS READ HERE, not in the TUI. internal/tui is handed a path and
	// handed back a cookies.ImportResult; no cookie byte reaches a model, a
	// tea.Msg, the screen or a log line. The path is the only thing logged,
	// deliberately — an operator who mistyped it needs to see which file was
	// opened.
	if s.autoCookieSvc != nil {
		app.OnImportCookieFile = func(path string) (cookies.ImportResult, error) {
			s.log.Info("Cookie file import requested from TUI", slog.String("path", path))
			data, err := os.ReadFile(path)
			if err != nil {
				return cookies.ImportResult{}, err
			}
			// 60 s, the wizard-finish budget rather than the route's. The Web
			// import inherits its request's deadline, which has no counterpart
			// here; what the two share is the work — a merge, a write and a
			// live auth check per platform — and that is what
			// FinishSetupDetailed is priced for one call over.
			importCtx, importCancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer importCancel()
			result, err := s.autoCookieSvc.ImportCookies(importCtx, string(data))

			// ImportCookies is the fifth WRITER of cookies.txt, so it ends in a
			// re-check like every other gesture that can (Arc 10 R4). Without
			// it the status-bar cookie badge keeps reporting the credentials
			// this import just replaced, until the 30-minute ticker.
			//
			// Gated on Wrote and DEFERRED, the same shape as the setup-wizard
			// finish and for the same reason: the jar reload after a successful
			// write can fail, and that exit hands back an error over a
			// cookies.txt that has already been replaced — the one case where
			// the re-check is worth most, because refresh's own jar.Reload
			// repairs the stale in-memory jar the error left behind. The defer
			// is what keeps that true if an early error return is ever added
			// above it; see TestEveryCookieWriteRecheckIsDeferred.
			//
			// context.Background rather than importCtx: defers run LIFO, so
			// this one completes before importCancel fires, but the 60 s budget
			// is the IMPORT's and the re-check is not the import's work to
			// spend it on — the same call the other five sites make.
			//
			// The re-check runs before this returns, so the overlay's spinner
			// covers it (≤30 s worst case: two 15 s auth probes); the web route
			// flushes its response first and re-checks after. Kept blocking
			// here on purpose: the AST test pins every cookies.txt write to end
			// in a deferred re-check, and a goroutine would satisfy the test
			// while deleting the property it protects.
			defer func() {
				if result.Wrote {
					recheckAfterCookieWrite(context.Background(), s.checkNowFn(), s.log, "a cookie file import")
				}
			}()

			return result, err
		}
	}

	// R Y — the yt-dlp plugin overlay. Both closures read the SAME port getter
	// and HTTPS flag wireRoutes hands routes.YtdlpRoutes, so the terminal and
	// the dashboard cannot disagree about which port the plugin should point
	// at or whether the one on disk matches.
	app.OnYtdlpPluginStatus = func() (routes.YtdlpPluginInfo, error) {
		return routes.YtdlpPluginStatus(s.currentWebPort(), s.httpsEnabled())
	}
	app.OnInstallYtdlpPlugin = func() error {
		return routes.InstallYtdlpPlugin(s.currentWebPort(), s.httpsEnabled())
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

			// A completed wizard has just written cookies.txt from the browser
			// the operator signed in to — the most deliberate credential change
			// there is, and until Arc 10 the one that told the running process
			// nothing.
			//
			// Gated on Wrote, the setup path's counterpart to
			// RefreshResult.Ran, and deferred so that gate is evaluated
			// independently of the error return: the jar reload after a
			// successful write can fail, and that exit hands back an error over
			// a cookies.txt that has already been replaced.
			//
			// context.Background rather than finishCtx, and NOT because finishCtx
			// is gone: defers run LIFO, so this one runs to completion before
			// finishCancel above fires, and finishCtx is alive throughout it.
			// The reason is that its 60 s budget is the WIZARD's — priced
			// against the server-side setup grace window, as the comment on the
			// timeout says — and the re-check is not the wizard's work to spend
			// it on. Same as the other five sites: none of them wants a
			// fingerprint comparison cancelled by its caller's teardown, and
			// the re-check has to outlive nothing.
			defer func() {
				if result.Wrote {
					recheckAfterCookieWrite(context.Background(), s.checkNowFn(), s.log, "the setup wizard")
				}
			}()

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
		valid, ver, _ := routes.CheckFFmpegCached(s.ffmpegPathOrDefault())
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

	// Check FFmpeg on startup (after config is loaded). Both reads go through
	// the store: ConfigLoaded is not a load-time constant — config.Save sets
	// it — so an unlocked read here races a web handler's save (CORE-24).
	if s.configLoaded() {
		ffmpegPath := s.ffmpegPathOrDefault()
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
	// tuiResyncNeeded arms the full-snapshot replay after a dropped job
	// event (CORE-6). See newTUIResync.
	var tuiResyncNeeded atomic.Bool
	resyncTUIJobs := newTUIResync(&tuiResyncNeeded, jobsUpdateCh, s.db.GetAllJobs)

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
	//
	// The four that can drop share one body, forwardOrDrop, which runs
	// resyncTUIJobs() from its SUCCESSFUL-send branch: a drop recorded by an
	// earlier event is replayed as a full snapshot by the first event that
	// gets through, which is what makes a dropped terminal transition
	// recoverable instead of a stale row for the session (CORE-6). On the way
	// IN it would instead fire once per DROPPED event — a full GetAllJobs on
	// this, the ~60 Hz UpdateJobFields writer goroutine, for every message
	// the stalled TUI could not take, feeding the very backlog it is
	// recovering from. While the channel stays full the 1 s backstop below is
	// the replay path.
	unsubTUIJobUpdate := s.db.OnJobChange(func(ev *database.JobChange) {
		forwardOrDrop(jobUpdateCh, ev, ev.Job.ID, resyncTUIJobs, &tuiDroppedJobs, &tuiResyncNeeded, s.log)
	})
	// OnJobAdded subscriber: AddJob no longer fires OnJobsChange (the
	// writer-side dispatch was dropped); the TUI now learns of new jobs
	// through this dedicated lifecycle event and the handler appends to
	// the task list instead of clearing + rebuilding from a fresh
	// snapshot. DECISIONS #21 consumer migration.
	unsubTUIJobAdded := s.db.OnJobAdded(func(ev *database.JobAdded) {
		forwardOrDrop(jobAddedCh, ev, ev.Job.ID, resyncTUIJobs, &tuiDroppedJobs, &tuiResyncNeeded, s.log)
	})
	// OnJobDeleted subscriber: DeleteJob no longer fires OnJobsChange
	// (writer-side dispatch dropped). The TUI's surgical-removal path
	// (handleJobDeleted) drops just the affected ID from local state
	// instead of clearing+rebuilding from a full-list snapshot.
	// DECISIONS #21.
	unsubTUIJobDeleted := s.db.OnJobDeleted(func(ev *database.JobDeleted) {
		forwardOrDrop(jobDeletedCh, ev, ev.JobID, resyncTUIJobs, &tuiDroppedJobs, &tuiResyncNeeded, s.log)
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
		forwardOrDrop(jobTrimsChangedCh, job, job.ID, resyncTUIJobs, &tuiDroppedJobs, &tuiResyncNeeded, s.log)
	})
	// This one already carries a full list, so its own drop is harmless (a
	// snapshot is queued when a snapshot cannot be queued) and its default
	// stays empty. It neither calls the replay nor CLEARS it: the list it
	// delivers is a time-of-write snapshot taken inside the bulk write, so an
	// UpdateJobFields that lands after that snapshot and is then dropped by a
	// full jobUpdateCh is NOT in it. Clearing the flag there discarded that
	// replay — a delivered list is not assumed newer than a pending one — and
	// the dropped transition was never recovered. Leaving it armed costs one
	// redundant GetAllJobs in the rare coincidence of a bulk write with a
	// pending drop, and the next successful send (or the 1 s backstop) does
	// the catch-up (CORE-6).
	unsubTUIJobsChange := s.db.OnJobsChange(func(jobs []*database.Job) {
		select {
		case jobsUpdateCh <- jobs:
		default:
		}
	})

	// 1 s backstop for the resync: the forwarders above cover the common
	// case (drops happen under event pressure, so more events follow), but a
	// drop whose job then goes quiet would otherwise never be replayed, and
	// neither would one whose channel stays full. One failed compare-and-swap
	// per second when nothing is pending — this is a discovery bound on a
	// pending catch-up, not a poll of the database.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("[Main] Panic in TUI resync backstop", "panic", fmt.Sprint(r))
			}
		}()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				resyncTUIJobs()
			}
		}
	}()

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
		// One cookies.txt holds both platforms' rows, so the sentinel is
		// platform-independent and both badges read the same one.
		unreadable := auth.CookieFileError != ""
		yt := cookieBadgeFor(auth.YouTubeAuthenticated, auth.HasYouTubeCookies, unreadable, auth.YouTubeVerification)
		tw := cookieBadgeFor(auth.TwitchAuthenticated, auth.HasTwitchCookies, unreadable, auth.TwitchVerification)
		// Check auto-cookie relogin state. As of test.36 the relogin map
		// is keyed by lowercase platform name (audit cookies.md #44).
		//
		// Applied unconditionally, and NOT gated on cookies.auto_enabled: the
		// flag means a human has to sign in again, which a manual-cookie
		// install must do by hand. The Web dashboard used to gate its copy of
		// this and now does not — see updateStatusBar in web/public/app.js.
		// ReloginStatus, not GetStatus: this reads nothing but the relogin
		// map, and GetStatus's browser/registry detection scan would run on
		// every auth-change dispatch — including the TUI's, which fires with
		// no cookie dialog open at all.
		relogin := s.autoCookieSvc.ReloginStatus()
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

	// Wire BotGuard sidecar liveness to the TUI (program.Send, like
	// connectivity above).
	//
	// The snapshot that ALREADY exists is seeded into the model first, and not
	// through the subscription's immediate callback: app.Send is a no-op until
	// tui.Run stores the program a few lines below, and initServices has
	// published this health long before now (on a failed first start, and
	// again from the supervisor a moment later). Without the seed the dominant
	// failure — a sidecar that cannot start at all — would draw nothing at
	// all, because the only later publish is the Healthy:true of a restart
	// that never comes. A process with the sidecar disabled published nothing,
	// CurrentHealth reports !ok, and the bar stays quiet, which is right.
	if h, ok := sidecar.CurrentHealth(); ok {
		app.SetSidecarDown(!h.Healthy)
	}
	// Then the subscription for every later transition. Its immediate callback
	// re-sends the snapshot just seeded; that Send is dropped, which is
	// harmless — it carries the same value.
	unsubSidecarTUI := sidecar.SubscribeHealth(func(h sidecar.Health) {
		app.Send(tui.SidecarStatusMsg{Healthy: h.Healthy})
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
	unsubSidecarTUI()

	// Report dropped messages (helps diagnose missed TUI updates). A dropped
	// job event is no longer a silently stale row — each streak of drops was
	// replayed by a full refresh (newTUIResync) — so this count is a
	// pressure signal, not a correctness one.
	if n := tuiDroppedJobs.Load(); n > 0 {
		s.log.Warn("TUI dropped job update messages (each streak replayed by a full refresh)",
			slog.Int64("count", n))
	}
	if n := tuiDroppedLogs.Load(); n > 0 {
		s.log.Warn("TUI dropped log messages", slog.Int64("count", n))
	}
}

// httpsEnabled and ffmpegPathOrDefault read through the config store.
// Closures that outlive wiring must never touch s.cfg's fields directly:
// PUT /api/config assigns *cfg = cfgCopy under the store's lock, so an
// unlocked field read races a whole-struct replacement (CORE-24).
func (s *runState) httpsEnabled() bool {
	enabled := false
	s.configStore.Read(func(c *config.MoomboxConfig) { enabled = c.Network.HTTPSEnabled })
	return enabled
}

// ffmpegPathOrDefault returns the configured FFmpeg path, or "ffmpeg" for
// the PATH lookup when none is set — the fallback every caller applied
// itself.
func (s *runState) ffmpegPathOrDefault() string {
	path := ""
	s.configStore.Read(func(c *config.MoomboxConfig) { path = c.Paths.FfmpegPath })
	if path == "" {
		return "ffmpeg"
	}
	return path
}

// configLoaded reports whether this run has a config file behind it.
//
// It is NOT a load-time constant: config.Save's last statement is
// `cfg.ConfigLoaded = true`, and Store.SaveLocked / Store.Update call Save on
// the very pointer s.cfg holds. The web handlers reach SaveLocked under the
// store's write lock while runTUI's body is still running — the HTTP server is
// up before the TUI starts — so the direct reads this replaced were a genuine
// data race, which the race detector confirms. The transition is only ever
// false -> true, so no caller's decision changes; the lock is what changes.
func (s *runState) configLoaded() bool {
	loaded := false
	s.configStore.Read(func(c *config.MoomboxConfig) { loaded = c.ConfigLoaded })
	return loaded
}

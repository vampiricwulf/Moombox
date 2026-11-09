package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// Each overlay's key handler, reached through the overlay registry
// (overlays.go) only while that overlay is the top visible one. Ctrl+C
// never gets here: handleKey quits ahead of the registry.

// Settings panel intercepts all keys (before normalization to preserve case for text input)
func (a *App) keySettings(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action := a.settings.HandleKey(key)
	// A save that just removed channels with "delete its pending jobs"
	// hands their IDs over here (settings_channel_removal.go).
	prune := a.channelPruneCmd()
	switch action {
	case "close":
		a.afterSettingsClose()
	case "restart":
		if a.OnRestart != nil {
			onRestart := a.OnRestart
			return a, tea.Batch(prune, safeCmd(func() tea.Msg {
				onRestart()
				return tea.QuitMsg{}
			}))
		}
	case "channel_removal_summary":
		return a, a.channelRemovalSummaryCmd(a.settings.ChannelRemovalID())
	case "resolve_channel":
		return a, a.resolveChannelCmd(a.settings.GetChannelResolveInput())
	case "test_notification":
		return a, a.testNotificationCmd(a.settings.SelectedNotificationURL())
	case "open_ffmpeg":
		a.settings.Close()
		a.afterSettingsClose() // a prompted close may have just saved
		a.ffmpegCheck.OnCheckPrereqs = a.OnCheckPrereqs
		a.ffmpegCheck.Open()
		a.ffmpegCheck.SetSize(a.width, a.height)
	}
	return a, prune
}

// Help overlay intercepts all keys (scroll handled by viewport in routeComponentMsg)
func (a *App) keyHelp(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "?", keyEsc:
		a.help.Toggle()
	}
	return a, nil
}

// notesShowPending is THE rule for U and S in the release-notes overlay
// (K27): the notes on screen are a pending update's (pending, set by R N)
// and that release is still the one pending. The footer keys off pending
// alone, which followPendingRelease keeps true to this: every write to
// a.updateAvailable runs it, and it withdraws notes whose release stopped
// being the pending one.
//
// Written once for both keys: in viewer mode U used to close the notes
// being read to print "No update available", and an UpdateStatusMsg landing
// while some other version's notes were open let U apply — or S
// permanently skip — a release whose notes were not on screen.
func (a *App) notesShowPending() bool {
	o := a.releaseNotesPopup
	return o != nil && o.isOpen() && o.pending &&
		a.updateAvailable != nil && a.updateAvailable.TagName == o.tag
}

// canApplyFromNotes reports whether U applies the release on screen.
func (a *App) canApplyFromNotes() bool { return a.notesShowPending() }

// canSkipFromNotes reports whether S skips the release on screen — and
// only once: a second S while the first is out would only be refused, the
// release being skipped already.
func (a *App) canSkipFromNotes() bool {
	return a.notesShowPending() && a.releaseNotesPopup.skipping == "" && a.OnDismissUpdate != nil
}

// Release-notes overlay intercepts keys when open. Scroll is handled by
// routeComponentMsg forwarding to the embedded viewport; here we only
// handle the close/apply bindings.
func (a *App) keyReleaseNotes(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case keyEsc, "q", "Q":
		a.releaseNotesPopup.close()
		return a, nil
	case "u", "U":
		if !a.canApplyFromNotes() {
			return a, nil
		}
		// Shared apply flow (active-downloads confirm included). Close
		// the overlay first so the confirmation/`Updating...` feedback is
		// visible on the main screen; a pending confirm re-arms via the
		// R U chord within the 5s window.
		cmd := a.applyUpdateAction()
		a.releaseNotesPopup.close()
		return a, cmd
	case "s", "S":
		if !a.canSkipFromNotes() {
			return a, nil
		}
		tag := a.updateAvailable.TagName
		fn := a.OnDismissUpdate
		epoch := a.releaseNotesPopup.epoch
		a.releaseNotesPopup.skipping = tag
		a.setFeedback("Skipping " + tag + "...")
		return a, safeCmd(func() tea.Msg { return dismissUpdateResultMsg{Tag: tag, Err: fn(tag), Epoch: epoch} })
	}

	// All other keys (arrows, pgup/pgdn) are forwarded to the viewport via
	// routeComponentMsg — nothing to do here.
	return a, nil
}

// FFmpeg check overlay: Esc here quits (TestEscOnTheFFmpegOverlayStillQuits).
func (a *App) keyFFmpegCheck(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action := a.ffmpegCheck.HandleKey(key)
	switch {
	case action == "skip":
		a.ffmpegCheck.Close()
		return a, nil
	case action == "quit":
		return a, tea.Quit
	case strings.HasPrefix(action, "prepare:"):
		method := strings.TrimPrefix(action, "prepare:")
		return a, tea.Batch(a.ffmpegPrepareCmd(method), spinnerTickCmd(a.ffmpegCheck.spinner))
	case strings.HasPrefix(action, "confirm:"):
		token := strings.TrimPrefix(action, "confirm:")
		return a, tea.Batch(a.ffmpegConfirmCmd(token), spinnerTickCmd(a.ffmpegCheck.spinner))
	case strings.HasPrefix(action, "reject:"):
		token := strings.TrimPrefix(action, "reject:")
		if a.OnRejectInstall != nil {
			a.OnRejectInstall(token)
		}
		a.ffmpegCheck.ShowManual()
	case strings.HasPrefix(action, "cancel:"):
		token := strings.TrimPrefix(action, "cancel:")
		if a.OnRejectInstall != nil {
			a.OnRejectInstall(token)
		}
		a.ffmpegCheck.ShowInstallOptions()
	case strings.HasPrefix(action, "check_custom:"):
		path := strings.TrimPrefix(action, "check_custom:")
		return a, tea.Batch(a.ffmpegCheckCmd(path), spinnerTickCmd(a.ffmpegCheck.spinner))
	case action == "dismiss":
		// Overlay already closed by HandleKey — nothing else to do.
	}
	return a, nil
}

// Setup wizard
func (a *App) keySetupWizard(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action := a.setupWiz.HandleKey(key)
	if action == "save" {
		// Dispatch async config save so the "Saving..." overlay is rendered
		cfg := a.setupWiz.pendingConfig
		installYtdlp := a.setupWiz.pendingYtdlp
		onComplete := a.setupWiz.OnComplete
		onInstall := a.setupWiz.OnInstallYtdlp
		onCarryCookies := a.setupWiz.OnCarryCookies
		store := a.configStore
		return a, safeCmd(func() tea.Msg {
			if cfg == nil {
				return setupSaveResultMsg{Err: "no config to save"}
			}
			// Setup completes once. On a first run in TUI mode the
			// dashboard tab the boot opens shows the Web wizard too,
			// and once it has saved nothing here may run: no
			// directories made, no cookies carried. The Web route's
			// first guard; OnComplete (config.Store.CompleteFirstRun)
			// makes the check again under the store lock.
			if store != nil {
				var loaded bool
				store.Read(func(c *config.MoomboxConfig) { loaded = c.ConfigLoaded })
				if loaded {
					return setupSaveResultMsg{Err: config.ErrSetupCompleted.Error()}
				}
			}
			// The output and staging directories first, before
			// anything is saved: one that cannot be created is
			// reported in the wizard, which stays open to correct the
			// path — the Web wizard's 400 (config.MakeSetupDirs).
			if err := config.MakeSetupDirs(cfg.Paths.OutputDirectory, cfg.Paths.StagingDirectory); err != nil {
				return setupSaveResultMsg{Err: err.Error()}
			}
			// The cookies a login on the Cookie Login step wrote, into
			// the cookie file the setup saves — before it is saved.
			if onCarryCookies != nil {
				if err := onCarryCookies(cfg.Cookies.CookieFile); err != nil {
					return setupSaveResultMsg{Err: "cookies.cookie_file: could not carry the signed-in cookies here: " + err.Error()}
				}
			}
			if onComplete != nil {
				if err := onComplete(cfg); err != nil {
					return setupSaveResultMsg{Err: err.Error()}
				}
			}
			// Post-save: install yt-dlp plugin if requested
			if installYtdlp && onInstall != nil {
				onInstall(cfg.Network.Port, cfg.Network.HTTPSEnabled)
			}
			return setupSaveResultMsg{}
		})
	}
	var cmds []tea.Cmd
	if action == "resolve_channel" {
		cmds = append(cmds, a.resolveChannelCmd(a.setupWiz.GetChannelResolveInput()))
	}
	if action == "finish_cookie" {
		// Run cookie extraction async so TUI doesn't freeze
		platform := a.setupWiz.cookiePlatform
		finishFn := a.setupWiz.OnFinishAutoCookie
		cmds = append(cmds, safeCmd(func() tea.Msg {
			var result cookies.SetupResult
			var errStr string
			if finishFn != nil {
				var err error
				result, err = finishFn()
				if err != nil {
					errStr = err.Error()
				}
			}
			return setupCookieFinishMsg{Platform: platform, Result: result, Err: errStr}
		}))
	}
	if a.setupWiz.cookieActive {
		cmds = append(cmds, spinnerTickCmd(a.setupWiz.spinner))
		// Arm the countdown only when no chain is already ticking —
		// appending one per keypress stacks chains and the countdown
		// would drain N+1 per second.
		if tick := a.setupWiz.armCookieTick(); tick != nil {
			cmds = append(cmds, tick)
		}
	}
	// Deliver pending huh form init cmd immediately (cursor blink, focus)
	if a.setupWiz.advancedInitCmd != nil {
		cmds = append(cmds, a.setupWiz.advancedInitCmd)
		a.setupWiz.advancedInitCmd = nil
	}
	return a, tea.Batch(cmds...)
}

// Action menu intercepts
func (a *App) keyActionMenu(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action := a.actionMenu.HandleKey(key)
	if action == "close" {
		return a, nil
	}
	if action != "" {
		a.actionMenu.Close()
		// Parse "CHORD" or "CHORD:jobID"
		chord, jobID, hasJob := strings.Cut(action, ":")
		var job *database.Job
		if hasJob {
			for _, j := range a.actionMenu.jobs {
				if j.ID == jobID {
					job = j
					break
				}
			}
		}
		return a.dispatchAction(chord, job)
	}
	return a, nil
}

func (a *App) keyImport(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action, data := a.importDlg.HandleKey(key)
	if action == "import" {
		return a, tea.Batch(a.importFileCmd(data), a.importDlg.SpinnerInit())
	}
	return a, nil
}

func (a *App) keyCookieImport(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action, path := a.cookieImportDlg.HandleKey(key)
	if action == "import" {
		a.cookieImportDlg.SetImporting()
		return a, tea.Batch(a.importCookieFileCmd(path), a.cookieImportDlg.SpinnerInit())
	}
	return a, nil
}

func (a *App) keyChapterImport(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	if a.chapterDlg.HandleKey(key) == "save" {
		return a, a.saveChaptersCmd(a.chapterDlg.SetSaving())
	}
	return a, nil
}

func (a *App) keyAddVideo(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action, data := a.addVideo.HandleKey(key)
	switch action {
	case "submit":
		return a, tea.Batch(a.addVideoCmd(data), a.addVideo.SpinnerInit())
	case "fetch_formats":
		return a, tea.Batch(a.fetchFormatsCmd(data), a.addVideo.SpinnerInit())
	}
	return a, nil
}

func (a *App) keyTrim(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action := a.trimDlg.HandleKey(key)
	switch action {
	case "submit":
		if a.OnCreateTrim != nil {
			a.trimDlg.StartProgress()
			a.trimInProgress = true
			a.trimStartedAt = time.Now()
			a.trimProgressMu.Lock()
			a.trimProgressPct = 0
			a.trimProgressMu.Unlock()
			jobID := a.trimDlg.JobID()
			startSec := a.trimDlg.ParsedStartSeconds()
			endSec := a.trimDlg.ParsedEndSeconds()
			// A trim's progress overlay ticks at the fast class — start the loop now
			// (it's demand-driven and may be stopped if all jobs are terminal).
			return a, tea.Batch(a.createTrimCmd(jobID, startSec, endSec), spinnerTickCmd(a.trimDlg.spinner), a.ensureProgressTicking())
		}
	case "background":
		a.setFeedback("Trim encoding in background...")
	case "delete":
		if a.OnDeleteTrim != nil {
			trimID := a.trimDlg.SelectedTrimID()
			if trimID != "" {
				a.trimDlg.SetLoading(true)
				jobID := a.trimDlg.JobID()
				return a, tea.Batch(a.deleteTrimCmd(jobID, trimID), spinnerTickCmd(a.trimDlg.spinner))
			}
		}
	}
	return a, nil
}

func (a *App) keyFiles(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action, data := a.filesDlg.HandleKey(msg)
	switch action {
	case "refresh":
		return a, tea.Batch(a.fetchOrphansCmd(), a.fetchOrphanedHistoryCmd(), a.filesDlg.SpinnerInit())
	case "delete":
		if sel := a.filesDlg.SelectedFile(); sel != nil {
			return a, a.deleteOrphanCmd(sel.Path)
		}
	case "delete-history":
		if sel := a.filesDlg.SelectedHistory(); sel != nil {
			return a, a.deleteHistoryEntryCmd(sel.VideoID)
		}
	case "delete-all-files":
		return a, tea.Batch(a.deleteAllOrphansCmd(data.([]string)), a.filesDlg.SpinnerInit())
	case "delete-all-history":
		return a, a.deleteAllHistoryCmd(data.([]string))
	}
	if cmd, ok := data.(tea.Cmd); ok && cmd != nil {
		return a, cmd
	}
	return a, nil
}

func (a *App) keyClientTokens(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	action, cmd := a.clientTokensDlg.HandleKey(msg)
	switch action {
	case "refresh":
		return a, tea.Batch(a.fetchClientTokensCmd(), a.clientTokensDlg.SpinnerInit())
	case "revoke":
		if sel := a.clientTokensDlg.SelectedToken(); sel != nil {
			return a, a.deleteClientTokenCmd(sel.ID)
		}
	}
	if cmd != nil {
		return a, cmd
	}
	return a, nil
}

// The whole KeyPressMsg for symmetry with the dialogs above, but this one
// owns no component: HandleKey only decides. See YtdlpDialogModel.
func (a *App) keyYtdlp(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch a.ytdlpDlg.HandleKey(msg) {
	case "install":
		// The chord is gated on the STATUS callback, so the overlay can be
		// open with no way to install. Say so rather than swallowing I.
		if a.OnInstallYtdlpPlugin == nil {
			a.ytdlpDlg.SetError("Install is unavailable in this process")
			return a, nil
		}
		a.ytdlpDlg.SetInstalling()
		return a, tea.Batch(a.ytdlpInstallCmd(), a.ytdlpDlg.SpinnerInit())
	case "refresh":
		return a, tea.Batch(a.ytdlpStatusCmd(), a.ytdlpDlg.Open())
	}
	return a, nil
}

func (a *App) keyStats(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch a.statsDlg.HandleKey(key) {
	case "refresh":
		// Re-fetch on the open session's epoch — the chain E T started
		// keeps ticking, and starting a second one here is what made
		// every r press double the polling.
		return a, tea.Batch(a.statsDlg.Open(), a.fetchStatsCmd(a.statsEpoch))
	case "close":
		// HandleKey already hid the overlay (esc/q). Retiring the epoch
		// orphans the session's pending tick and any in-flight fetch.
		a.statsEpoch++
	}
	return a, nil
}

// The O L overlay owns every key while open: its search box, n/N, End
// and the close keys here, the scroll keys already fed to its viewport
// by routeComponentMsg. Nothing reaches the chord system underneath.
func (a *App) keyJobLog(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	cmd, action := a.jobLog.HandleKey(msg)
	if action == "close" {
		// HandleKey already hid the overlay. Retiring the epoch orphans
		// the session's pending tick and any read in flight.
		a.jobLogEpoch++
	}
	return a, cmd
}

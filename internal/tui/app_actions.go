package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// recheckCookiesCmd is the work behind the R C chord: the in-process Go cookie
// refresh, and the per-platform verdicts it reports.
//
// Extracted so R F's no-profile rung can run THE SAME THING rather than a
// second copy of it. R F is a ladder — launch the headless browser, else import
// the browser profile, else fall back to this — and the fallback has to be the
// real refresh, not a message about one. Two hand-written copies would let the
// two triggers drift, which is the defect the shared cookies.RecheckReport was
// introduced to close one level up.
//
// nil when the callback is not wired, so callers keep their own "not available"
// behaviour rather than dispatching a command that does nothing.
func (a *App) recheckCookiesCmd() tea.Cmd {
	if a.OnRecheckCookies == nil {
		return nil
	}
	recheckFn := a.OnRecheckCookies
	// Read off the App here, on the update goroutine, and captured into the
	// closure — the command below runs on a bubbletea worker and must not
	// touch App fields. Same discipline recheckFn is already held to.
	lastErrorFn := a.OnAutoCookieLastError
	return safeCmd(func() tea.Msg {
		yt, tw, ytReason, twReason := recheckFn()
		msg := cookieRecheckResultMsg{
			YouTube: yt, Twitch: tw,
			YouTubeReason: ytReason, TwitchReason: twReason,
		}
		if lastErrorFn != nil {
			msg.LastError = lastErrorFn()
		}
		return msg
	})
}

// cookieAcquisitionMode reads cookies.acquisition from the LIVE store, the same
// way AutoCookieService.AcquisitionMode does in cmd/moombox. A value snapshotted
// at construction would leave R F's sentence describing a mode the operator
// changed twenty minutes ago. Empty (no store, or a config built without
// Defaults) is "auto", which is what the service resolves it to anyway.
func (a *App) cookieAcquisitionMode() string {
	if a.configStore == nil {
		return "auto"
	}
	var mode string
	a.configStore.Read(func(c *config.MoomboxConfig) {
		mode = c.Cookies.Acquisition
	})
	return mode
}

// cookieRefreshFeedback is R F's pre-flight line, and it exists as a function
// so the two sentences can be tested without an event loop.
//
// The default sentence is a claim only one of the two acquisition modes can
// support. Under "profile" the pass launches nothing, so naming the browser
// sends the operator looking for a process that will never start — the same
// unearned cause as telling a gated operator to install a browser they already
// have. The dashboard's twin is cookieRefreshPreflightToast in
// web/public/modules/utils.js; TestRefreshPreflightSentenceAgreesAcrossSurfaces
// pins the two by exact equality. Unlike the ladder's rung-3 pair these do NOT
// diverge by surface, because neither names an affordance.
func cookieRefreshFeedback(mode string) string {
	if mode == "profile" {
		return "Importing cookies from the browser profile..."
	}
	return "Running browser cookie refresh..."
}

// cookieRefreshMechanismLabel is the SUBJECT of every post-flight sentence R F
// renders: the mechanism that actually ran.
//
// cookieRefreshFeedback above names what WILL run, from the mode, because
// before the pass that is all there is. Afterwards the pass knows better —
// RefreshResult.Mechanism is what it chose — and the two disagree wherever the
// HOST decides rather than the setting: a machine with no browser installed
// imports in "auto" mode and always has. That is why every post-flight sentence
// said "Browser cookie refresh ..." after an import (Arc 12c arc-close F2), and
// why this reads the RESULT first.
//
// The mode is the fallback, not the source. An empty Mechanism means the pass
// declined before choosing — a setup in flight, nothing worth refreshing — and
// the mode is then the best answer available AND the one the pre-flight
// sentence already gave, so the two lines agree rather than contradict.
//
// The dashboard's twin is cookieRefreshMechanismLabel in
// web/public/modules/utils.js;
// TestRefreshPostflightMechanismAgreesAcrossSurfaces pins the two by exact
// equality over every combination, the way
// TestRefreshPreflightSentenceAgreesAcrossSurfaces pins the pre-flight pair.
// Like that pair these name no per-surface affordance, so they do not diverge;
// unlike the rung-3 pair, which does and must.
func cookieRefreshMechanismLabel(mechanism, mode string) string {
	switch mechanism {
	case cookies.RefreshMechanismProfileImport:
		return "Browser-profile cookie import"
	case cookies.RefreshMechanismBrowser:
		return "Browser cookie refresh"
	}
	if mode == cookies.AcquisitionProfile {
		return "Browser-profile cookie import"
	}
	return "Browser cookie refresh"
}

// osClipboard is the OS-helper seam the O C case calls. A variable rather
// than a direct call so a test can drive BOTH branches on every platform
// without spawning a real helper or overwriting the developer's own
// clipboard; production always holds the build-tagged osClipboardFallback
// (clipboard_windows.go / clipboard_other.go).
var osClipboard = osClipboardFallback

// clipboardFeedback words the O C result honestly. tea.SetClipboard is OSC
// 52 — the terminal may accept it, ignore it, or be a multiplexer that needs
// `set-clipboard on` first — so on that path the TUI cannot claim a
// completed copy, only that it handed the URL over, which is what every
// press says at the moment it happens. sentViaOSC52 is false only once
// something ELSE has come back reporting that it took the text (clip.exe,
// via osClipboardFallback), which is a copy worth claiming (CORE-15, O-W).
func clipboardFeedback(url string, sentViaOSC52 bool) string {
	if sentViaOSC52 {
		return "Sent to terminal clipboard (OSC 52): " + url
	}
	return "Copied: " + url
}

// dispatchAction executes a chord action. For job-specific actions, job comes from
// the selected task (keyboard chords) or from the menu's job picker (menu flow).
func (a *App) dispatchAction(chord string, job *database.Job) (tea.Model, tea.Cmd) {
	switch chord {
	case "A A":
		a.clearFeedback()
		a.addVideo.SetSize(a.width, a.height)
		a.addVideo.Open()
	case "A Z":
		a.clearFeedback()
		a.importDlg.SetSize(a.width, a.height)
		startDir := filepath.Join(".", "import")
		if cwd, err := os.Getwd(); err == nil {
			startDir = filepath.Join(cwd, "import")
		}
		_ = os.MkdirAll(startDir, 0o755)
		return a, a.importDlg.Open(startDir)
	case "A R":
		if job == nil && a.taskList.SelectedCount() > 0 && a.OnResumeJob != nil {
			count := 0
			for _, id := range a.taskList.SelectedIDs() {
				j := a.taskList.GetJobByID(id)
				if j == nil || j.Platform != "youtube" {
					continue
				}
				if a.HasStagingFiles != nil && !a.HasStagingFiles(id) {
					continue
				}
				if j.Status != database.StatusCancelled && j.Status != database.StatusError && j.Status != database.StatusCookies && !(j.Status == database.StatusFinished && j.IncompleteTail) {
					continue
				}
				a.OnResumeJob(id)
				count++
			}
			a.taskList.ClearSelection()
			a.setFeedback(fmt.Sprintf("Resumed %d jobs", count))
		} else if job != nil && a.OnResumeJob != nil {
			a.OnResumeJob(job.ID)
			a.setFeedback(fmt.Sprintf("Resuming: %s", job.Title))
		}
	case "A I":
		if job == nil && a.taskList.SelectedCount() > 0 && a.OnReinitializeJob != nil {
			count := 0
			for _, id := range a.taskList.SelectedIDs() {
				j := a.taskList.GetJobByID(id)
				if j == nil {
					continue
				}
				if j.Status != database.StatusError && j.Status != database.StatusCancelled && j.Status != database.StatusCookies {
					continue
				}
				a.OnReinitializeJob(id)
				count++
			}
			a.taskList.ClearSelection()
			a.setFeedback(fmt.Sprintf("Reinitialized %d jobs", count))
		} else if job != nil && a.OnReinitializeJob != nil {
			a.OnReinitializeJob(job.ID)
			a.setFeedback(fmt.Sprintf("Reinitializing: %s", job.Title))
		}
	case "A M":
		if job != nil && a.OnMuxJob != nil {
			if err := a.OnMuxJob(job.ID); err != nil {
				a.setFeedback(fmt.Sprintf("Mux failed: %s", err))
			} else {
				a.setFeedback(fmt.Sprintf("Muxing: %s", job.Title))
			}
		}
	case "A S":
		if job != nil && a.OnRecoverAsides != nil {
			if err := a.OnRecoverAsides(job.ID); err != nil {
				a.setFeedback(fmt.Sprintf("Recovery failed: %s", err))
			} else {
				// The recordings are consumed within seconds; drop the memo so
				// the details panel stops offering them.
				a.invalidateAsides(job.ID)
				a.setFeedback(fmt.Sprintf("Recovering set-aside recordings: %s", job.Title))
			}
		}
	case "A C":
		if job == nil && a.taskList.SelectedCount() > 0 && a.OnCancelJob != nil {
			count := 0
			for _, id := range a.taskList.SelectedIDs() {
				j := a.taskList.GetJobByID(id)
				if j == nil {
					continue
				}
				// Filter by cancellable statuses (matches Web UI CANCEL_STATUSES)
				switch j.Status {
				case database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusQueued, database.StatusMuxing, database.StatusCookies:
					// OK
				default:
					continue
				}
				a.OnCancelJob(id)
				count++
			}
			a.taskList.ClearSelection()
			if count > 0 {
				a.setFeedback(fmt.Sprintf("Cancelled %d jobs", count))
			} else {
				a.setFeedback("No cancellable jobs in selection")
			}
		} else if job != nil && a.OnCancelJob != nil {
			a.OnCancelJob(job.ID)
			a.setFeedback(fmt.Sprintf("Cancelled: %s", job.Title))
		}
	case "A D":
		// OnDeleteJob blocks in WaitForJobExit (up to 5s per job) — run the
		// callback(s) off the update loop so the TUI stays responsive. Row
		// removal + selection refresh arrive via the JobDeleted lifecycle
		// event (handleJobDeleted); the result message only sets feedback.
		if job == nil && a.taskList.SelectedCount() > 0 && a.OnDeleteJob != nil {
			// The menu item's JobFilter never reaches here (batch dispatches
			// with a nil job), so the same rule is applied to the selection —
			// the Web's batch bar filters its targets the same way.
			var ids []string
			for _, id := range a.taskList.SelectedIDs() {
				if j := a.taskList.GetJobByID(id); j != nil && isDeletableStatus(j.Status) {
					ids = append(ids, id)
				}
			}
			a.taskList.ClearSelection()
			if len(ids) == 0 {
				a.setFeedback("No deletable jobs in selection")
				return a, nil
			}
			a.setFeedback(fmt.Sprintf("Deleting %d jobs...", len(ids)))
			deleteFn := a.OnDeleteJob
			return a, safeCmd(func() tea.Msg {
				for _, id := range ids {
					deleteFn(id)
				}
				return deleteJobsResultMsg{Count: len(ids)}
			})
		} else if job != nil && a.OnDeleteJob != nil {
			a.setFeedback(fmt.Sprintf("Deleting: %s", job.Title))
			deleteFn := a.OnDeleteJob
			id, title := job.ID, job.Title
			return a, safeCmd(func() tea.Msg {
				deleteFn(id)
				return deleteJobsResultMsg{Count: 1, Title: title}
			})
		}
	case "A W":
		if a.OnSetWatched == nil {
			a.setFeedback("Watched toggling is unavailable")
			return a, nil
		}
		setFn := a.OnSetWatched
		if job == nil && a.taskList.SelectedCount() > 0 {
			ids := a.taskList.SelectedIDs()
			allWatched := true
			finished := ids[:0]
			for _, id := range ids {
				if j := a.taskList.GetJobByID(id); j != nil && j.Status == database.StatusFinished {
					finished = append(finished, id)
					if !j.Watched {
						allWatched = false
					}
				}
			}
			if len(finished) == 0 {
				a.setFeedback("No finished jobs in selection")
				return a, nil
			}
			a.taskList.ClearSelection()
			watched := !allWatched
			return a, safeCmd(func() tea.Msg {
				return setWatchedResultMsg{Count: len(finished), Watched: watched, Err: setFn(finished, watched)}
			})
		} else if job != nil && job.Status == database.StatusFinished {
			watched := !job.Watched
			id := job.ID
			return a, safeCmd(func() tea.Msg {
				return setWatchedResultMsg{Count: 1, Watched: watched, Err: setFn([]string{id}, watched)}
			})
		}
	case "A T":
		if a.trimInProgress {
			a.setFeedback("A trim is already in progress")
			return a, nil
		}
		if job != nil {
			a.openTrimForJob(job)
		}
	case "A O":
		a.filesDlg.SetSize(a.width, a.height)
		a.filesDlg.Open()
		return a, tea.Batch(a.fetchOrphansCmd(), a.fetchOrphanedHistoryCmd(), a.filesDlg.SpinnerInit())
	case "A K":
		if a.OnListClientTokens != nil {
			a.clientTokensDlg.SetSize(a.width, a.height)
			a.clientTokensDlg.Open()
			return a, tea.Batch(a.fetchClientTokensCmd(), a.clientTokensDlg.SpinnerInit())
		}
	case "R B":
		if a.OnBackfillRescan != nil {
			a.setFeedback("Re-scanning feed history...")
			rescanFn := a.OnBackfillRescan
			return a, safeCmd(func() tea.Msg {
				rescanFn()
				return backfillRescanQueuedMsg{}
			})
		}
	case "R C":
		if cmd := a.recheckCookiesCmd(); cmd != nil {
			a.setFeedback("Rechecking cookies...")
			return a, cmd
		}
	case "R F":
		if a.OnForceRefreshCookies != nil {
			a.setFeedback(cookieRefreshFeedback(a.cookieAcquisitionMode()))
			refreshFn := a.OnForceRefreshCookies
			return a, safeCmd(func() tea.Msg {
				result, err := refreshFn()
				return cookieForceRefreshResultMsg{Result: result, Err: err}
			})
		}
	case "E L":
		// Defensive, and unreachable from the keyboard: with no callback the
		// chord is not registered (buildMenuItems below), so processSecondKey
		// reports "Invalid Chord: E L" before this case is consulted. The guard
		// keeps a direct caller from opening an overlay whose every Enter
		// dead-ends. StartSetup's own refusals — service stopped, a setup or
		// refresh already running, no supported browser — arrive on the
		// operator's Enter and are rendered inline by the wizard, which is
		// where the web panel puts them too.
		if a.setupWiz.OnStartAutoCookie == nil {
			a.setFeedback("Cookie login is unavailable — no auto-cookie service is configured")
			return a, nil
		}
		a.setupWiz.SetSize(a.width, a.height)
		// Preselect whatever the status bar is alarming about, so answering a
		// "TW: Re-login" badge does not open on YouTube.
		a.setupWiz.OpenCookieLogin(a.statusBar.ReloginPlatform())
	case "E I":
		// Defensive for the same reason E L's guard is, and unreachable the
		// same way: with no callback the chord is not registered, so
		// processSecondKey never reaches this case. The guard keeps a direct
		// caller from opening an overlay whose Enter dead-ends.
		if a.OnImportCookieFile == nil {
			a.setFeedback("Cookie import is unavailable — no auto-cookie service is configured")
			return a, nil
		}
		a.clearFeedback()
		a.cookieImportDlg.SetSize(a.width, a.height)
		return a, a.cookieImportDlg.Open()
	case "E Y":
		// Defensive for the same reason E I's guard is, and unreachable the
		// same way: with no status callback the chord is not registered, so
		// processSecondKey never reaches this case.
		if a.OnYtdlpPluginStatus == nil {
			a.setFeedback("yt-dlp plugin status is unavailable in this process")
			return a, nil
		}
		a.ytdlpDlg.SetSize(a.width, a.height)
		return a, tea.Batch(a.ytdlpDlg.Open(), a.ytdlpStatusCmd())
	case "E T":
		if a.OnGetStats == nil {
			a.setFeedback("Statistics are unavailable")
			return a, nil
		}
		a.clearFeedback()
		a.statsDlg.SetSize(a.width, a.height)
		// The only place a refresh chain starts. The tick goes LAST in the
		// batch: a consumer that stops at the first real message (the tests'
		// drainer) then never sits on the 60 s timer.
		a.statsEpoch++
		return a, tea.Batch(a.statsDlg.Open(), a.fetchStatsCmd(a.statsEpoch), statsRefreshTick(a.statsEpoch))
	case "R V":
		if a.OnCheckUpdate != nil {
			a.setFeedback("Checking for updates...")
			checkFn := a.OnCheckUpdate
			return a, safeCmd(func() tea.Msg {
				info, err := checkFn()
				if err != nil {
					return updateCheckResultMsg{Err: err.Error()}
				}
				return updateCheckResultMsg{Info: info}
			})
		}
	case "R M":
		if a.OnForceCheck != nil {
			a.OnForceCheck()
			a.setFeedback("Checking all monitors now…")
		}
	case "R N":
		if a.releaseNotesPopup == nil {
			a.releaseNotesPopup = newReleaseNotesOverlay()
		}
		if a.updateAvailable != nil {
			a.releaseNotesPopup.open(
				a.updateAvailable.TagName,
				a.updateAvailable.ReleaseNotes,
				a.width, a.height,
			)
			a.releaseNotesPopup.setPending(true)
			return a, nil
		}
		if a.OnFetchReleaseNotes == nil || a.version == "" {
			a.setFeedback("Cannot fetch release notes (offline build or unconfigured)")
			return a, nil
		}
		// No pending update — fetch current version's notes asynchronously.
		a.releaseNotesPopup.open("v"+a.version, "Loading release notes…", a.width, a.height)
		a.releaseNotesPopup.setPending(false)
		fetchFn := a.OnFetchReleaseNotes
		ver := a.version
		return a, safeCmd(func() tea.Msg {
			tag, notes, err := fetchFn(ver)
			if err != nil {
				return releaseNotesFetchedMsg{Tag: tag, Err: err.Error()}
			}
			return releaseNotesFetchedMsg{Tag: tag, Notes: notes}
		})
	case "R U":
		return a, a.applyUpdateAction()
	case "R S":
		if a.OnVerifySignature != nil {
			a.setFeedback("Verifying signature...")
			verifyFn := a.OnVerifySignature
			return a, safeCmd(func() tea.Msg {
				err := verifyFn()
				if err != nil {
					return signatureVerifyResultMsg{Err: err.Error()}
				}
				return signatureVerifyResultMsg{}
			})
		}
	case "R P":
		if a.OnRestart != nil {
			onRestart := a.OnRestart
			return a, safeCmd(func() tea.Msg {
				onRestart()
				return tea.QuitMsg{}
			})
		}
	case "O F":
		if job != nil && a.OnOpenFolder != nil {
			a.OnOpenFolder(job.ID)
			a.setFeedback(fmt.Sprintf("Opening folder for: %s", job.Title))
		}
	case "O S":
		if job != nil {
			if url := streamURL(job); url != "" {
				openBrowser(url)
				a.setFeedback("Opening: " + url)
			} else {
				a.setFeedback("No stream URL available")
			}
		}
	case "O C":
		if job != nil {
			if url := streamURL(job); url != "" {
				// TWO independent things, and the order matters.
				//
				// The OSC 52 write goes out on every press, on every
				// platform, unconditionally — it is the only mechanism that
				// reaches the terminal the operator is actually sitting at,
				// which over SSH is not the machine Moombox runs on.
				//
				// The OS helper (clip.exe, on a local Windows console) is a
				// CHILD PROCESS, so it runs inside the Cmd rather than here:
				// blocking the update goroutine would freeze rendering and
				// input — the ~60 Hz progress frames included — for as long
				// as the child takes. Its result upgrades the wording when
				// it lands (clipboardResultMsg in app_update.go).
				//
				// The line shown now therefore hedges: at this instant the
				// press has handed the URL over and nothing more
				// (CORE-15, O-W).
				a.setFeedback(clipboardFeedback(url, true))
				return a, tea.Batch(
					tea.SetClipboard(url),
					safeCmd(func() tea.Msg {
						return clipboardResultMsg{URL: url, Copied: osClipboard(url)}
					}),
				)
			}
			a.setFeedback("No URL to copy")
		}
	case "O W":
		scheme := "http"
		if a.configStore != nil {
			a.configStore.Read(func(c *config.MoomboxConfig) {
				if c.Network.HTTPSEnabled {
					scheme = "https"
				}
			})
		}
		url := fmt.Sprintf("%s://localhost:%d", scheme, a.getPort())
		a.setFeedback(fmt.Sprintf("Opening: %s", url))
		openBrowser(url)
	case "O G":
		a.setFeedback("Opening: " + constants.ProjectRepoURL)
		openBrowser(constants.ProjectRepoURL)
	case "F":
		a.handleFilter()
	case "`":
		if a.cfg != nil {
			a.settings.SetSize(a.width, a.height)
			a.settings.OnSave = a.OnSaveConfig
			a.settings.OnRestart = a.OnRestart
			a.settings.OnRestartRequired = func() {
				a.restartPending = true
				// The banner consumes rows above the panels — re-derive
				// panel heights + mouse regions now that it's visible.
				a.recalcLayout()
			}
			a.settings.OnSecurityChanged = func() {
				// Setting a password can clear the security banner — the
				// rows it occupied must go back to the panels, or View()
				// renders a dead row until the next resize.
				a.recalcLayout()
			}
			a.settings.OnHashPassword = a.OnHashPassword
			a.settings.OnVerifyPassword = a.OnVerifyPassword
			a.settings.Open(a.cfg)
		}
	case "?":
		a.help.SetMenuItems(a.buildMenuItems())
		a.help.Toggle()
	case "Q Q":
		return a, tea.Quit
	}
	return a, nil
}

// chordFeedback builds contextual feedback for a chord prefix.
// It derives available options from buildMenuItems() using HintLabel.
func (a *App) chordFeedback(prefix string) string {
	if prefix == "q" {
		return "Quit: Q Confirm (3s)"
	}

	upperPrefix := strings.ToUpper(prefix)
	items := a.buildMenuItems()
	job := a.taskList.SelectedJob()

	var parts []string
	for i := range items {
		item := &items[i]
		// Match items whose chord starts with this prefix (e.g. "A " for prefix "a")
		if !strings.HasPrefix(item.Chord, upperPrefix+" ") {
			continue
		}
		// For NeedsJob items, check if selected job passes the filter
		if item.NeedsJob && (job == nil || (item.JobFilter != nil && !item.JobFilter(job))) {
			continue
		}
		// Extract the second key character from the chord (e.g. "A K" → "K")
		secondKey := item.Chord[len(upperPrefix)+1:]
		parts = append(parts, secondKey+" "+item.HintLabel)
	}

	// Category label from prefix
	var label string
	switch prefix {
	case "a":
		label = "Action"
	case "r":
		label = "Request"
	case "o":
		label = "Open"
	case "e":
		label = "Extras"
	default:
		label = strings.ToUpper(prefix)
	}

	if len(parts) == 0 {
		return label + ": (none available) (3s)"
	}
	return label + ": " + strings.Join(parts, " | ") + " (3s)"
}

// canOpenFolder returns true if the job's folder can be opened.
func canOpenFolder(j *database.Job) bool {
	switch j.Status {
	case database.StatusFinished:
		return j.OutputFile != ""
	case database.StatusUpcoming, database.StatusLive, database.StatusDownloading, database.StatusMuxing:
		return true
	}
	return false
}

// canOpenStream returns true if the job has a stream URL to open.
func canOpenStream(j *database.Job) bool {
	return j.URL != "" || j.VideoID != ""
}

// streamURL returns the stream page URL for a job, or "" if unavailable.
func streamURL(j *database.Job) string {
	if j.URL != "" {
		return j.URL
	}
	if j.VideoID == "" {
		return ""
	}
	if j.Platform == "twitch" {
		if j.IsVod {
			vodID := strings.TrimPrefix(j.VideoID, "tw_v")
			return "https://www.twitch.tv/videos/" + vodID
		}
		if j.ChannelName == "" {
			return ""
		}
		return "https://www.twitch.tv/" + j.ChannelName
	}
	return "https://www.youtube.com/watch?v=" + j.VideoID
}

// openTrimForJob opens the trim dialog for a specific job.
func (a *App) openTrimForJob(job *database.Job) {
	if job.Status == database.StatusFinished && job.OutputFile != "" {
		a.trimDlg.SetSize(a.width, a.height)
		a.trimDlg.Open(job.ID, job.Title)
		var lenSec float64
		var fSize int64
		if job.LengthSeconds != nil {
			lenSec = float64(*job.LengthSeconds)
		}
		if job.FileSize != nil {
			fSize = *job.FileSize
		}
		a.trimDlg.SetJobMetadata(lenSec, fSize)
		a.trimDlg.SetTrims(trimInfosFromJob(job))
	} else {
		a.setFeedback("Trim only available for finished jobs with files")
	}
}

// trimInfosFromJob converts a job's trims to TrimInfo slice for the trim dialog.
func trimInfosFromJob(job *database.Job) []TrimInfo {
	var trimInfos []TrimInfo
	for _, tr := range job.Trims {
		var fs int64
		if tr.FileSize != nil {
			fs = *tr.FileSize
		}
		trimInfos = append(trimInfos, TrimInfo{
			ID:        tr.ID,
			StartTime: tr.StartTime,
			EndTime:   tr.EndTime,
			Duration:  tr.Duration,
			FileSize:  fs,
			Filename:  tr.Filename,
		})
	}
	return trimInfos
}

// refreshTrimList updates the trim dialog's trim list without resetting dialog state.
func (a *App) refreshTrimList(job *database.Job) {
	a.trimDlg.SetTrims(trimInfosFromJob(job))
}

// isDeletableStatus is the Web's DELETE_STATUSES set
// (web/public/modules/utils.js) — the statuses both dashboards offer Delete
// for. Owner ruling R3 (2026-09-15): the TUI hides Delete for an active job
// exactly as the Web's job cards, details dialog and batch bar do. The server
// accepts a delete in any state; this is a UI rule about not pulling a running
// download out from under its worker.
//
// Two readers on purpose: the A D menu item's JobFilter (which gates the
// chord, the job selector and the confirm-window re-validation) and the batch
// arm of dispatchAction, which is dispatched with a nil job and so never sees
// the filter — the same split A C and A W already carry.
//
// It happens to select the same four statuses as isProgressTerminal
// (app_update.go) today, but it is a DIFFERENT rule with a different owner:
// this one tracks the Web's DELETE_STATUSES, that one answers "can this job
// still produce live progress". Keep them separate — never alias one to the
// other or derive it from the other; the Web moving a status in or out of
// DELETE_STATUSES must not silently change what the progress store holds.
func isDeletableStatus(s database.JobStatus) bool {
	return s == database.StatusFinished || s == database.StatusError ||
		s == database.StatusCancelled || s == database.StatusCookies
}

// JobIsActive reports whether a job is actively writing its staging and output
// paths.
//
// The SOURCE OF TRUTH for this list is worker.IsActiveJobStatus
// (internal/worker/orphans.go), which is what the worker and the REST route
// both refuse a set-aside recovery on; internal/tui cannot import that package
// (the import fence), so this is a deliberate twin and cmd/moombox pins the
// two against each other for every status. The third reader, the dashboard's
// literal in web/public/modules/job-details.js, cites the same source and is
// pinned behaviourally by its own jsdom test.
//
// Four statuses, spelled out rather than derived from IsTerminal(): a Queued
// or COOKIES? job is not terminal but its staging dir is not being written
// either, and recovery is perfectly safe there.
//
// Exported only so that parity test can exist.
func JobIsActive(s database.JobStatus) bool {
	return s == database.StatusDownloading || s == database.StatusMuxing ||
		s == database.StatusLive || s == database.StatusUpcoming
}

// asidesFor returns the selected job's set-aside summary, probing the disk at
// most once per job ID. An active job answers empty without a probe: its
// staging dir is mid-write, and nothing in it is recoverable yet.
func (a *App) asidesFor(job *database.Job) AsideSummary {
	if job == nil || a.JobAsides == nil || JobIsActive(job.Status) {
		return AsideSummary{}
	}
	if a.asidesJobID == job.ID {
		return a.asidesCache
	}
	a.asidesJobID = job.ID
	a.asidesCache = a.JobAsides(job.ID)
	return a.asidesCache
}

// invalidateAsides drops the memo for one job, so the next selection re-reads
// the disk. Called when a recovery is dispatched: the recordings it consumes
// are gone within seconds, and a stale panel would keep offering them.
func (a *App) invalidateAsides(jobID string) {
	if a.asidesJobID == jobID {
		a.asidesJobID = ""
		a.asidesCache = AsideSummary{}
	}
}

// buildMenuItems builds context-sensitive action menu items.
// This is the single source of truth for all chords, menu entries, feedback hints, and help text.
func (a *App) buildMenuItems() []ActionMenuItem {
	items := []ActionMenuItem{
		{Chord: "A A", Label: "Add Video", HintLabel: "Add", Category: "Action"},
		{Chord: "A Z", Label: "Import Archive", HintLabel: "Import", Category: "Action"},
		{Chord: "A R", Label: "Resume Job", HintLabel: "Resume", Category: "Action", NeedsJob: true, SupportsBatch: true,
			DisabledReason: "no jobs to resume",
			JobFilter: func(j *database.Job) bool {
				canResume := (j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies || (j.Status == database.StatusFinished && j.IncompleteTail)) &&
					j.Platform == "youtube"
				if canResume && a.HasStagingFiles != nil {
					return a.HasStagingFiles(j.ID)
				}
				return false
			},
			// The menu's "no jobs" question is answered from status alone;
			// the HasStagingFiles probe above runs when A R is chosen and
			// its job selector is built (CORE-9).
			StatusFilter: func(j *database.Job) bool {
				return (j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies || (j.Status == database.StatusFinished && j.IncompleteTail)) &&
					j.Platform == "youtube"
			}},
		{Chord: "A I", Label: "Reinitialize Job", HintLabel: "Reinit", Category: "Action", NeedsJob: true, SupportsBatch: true,
			DisabledReason: "no jobs to reinitialize",
			JobFilter: func(j *database.Job) bool {
				return j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies
			}},
		{Chord: "A M", Label: "Mux Job", HintLabel: "Mux", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			DisabledReason: "no muxable jobs",
			JobFilter: func(j *database.Job) bool {
				canMux := j.Status == database.StatusCancelled || j.Status == database.StatusError
				if canMux && a.HasSegmentFiles != nil {
					return a.HasSegmentFiles(j.ID)
				}
				return false
			},
			// Status-only twin of the filter above — see A R (CORE-9).
			StatusFilter: func(j *database.Job) bool {
				return j.Status == database.StatusCancelled || j.Status == database.StatusError
			}},
		{Chord: "A S", Label: "Recover Set-aside Recordings", HintLabel: "Recover", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			DisabledReason: "no jobs with set-aside recordings",
			JobFilter: func(j *database.Job) bool {
				if JobIsActive(j.Status) {
					return false
				}
				return a.JobAsides != nil && len(a.JobAsides(j.ID).Asides) > 0
			},
			// Status-only twin of the filter above — see A R (CORE-9). The
			// JobAsides probe reads the disk, so it runs when A S is chosen
			// and its job selector is built, never when the menu opens.
			StatusFilter: func(j *database.Job) bool {
				return !JobIsActive(j.Status)
			}},
		{Chord: "A C", Label: "Cancel Job", HintLabel: "Cancel", Category: "Action", NeedsJob: true, NeedsConfirm: true, SupportsBatch: true,
			DisabledReason: "no active jobs",
			JobFilter: func(j *database.Job) bool {
				return j.Status != database.StatusFinished && j.Status != database.StatusCancelled && j.Status != database.StatusError
			}},
		{Chord: "A D", Label: "Delete Job", HintLabel: "Delete", Category: "Action", NeedsJob: true, NeedsConfirm: true, SupportsBatch: true,
			DisabledReason: "no deletable jobs",
			JobFilter:      func(j *database.Job) bool { return isDeletableStatus(j.Status) }},
		{Chord: "A W", Label: "Toggle Watched", HintLabel: "Watched", Category: "Action", NeedsJob: true, SupportsBatch: true,
			DisabledReason: "no finished jobs",
			JobFilter:      func(j *database.Job) bool { return j.Status == database.StatusFinished }},
		{Chord: "A T", Label: "Trim Video", HintLabel: "Trim", Category: "Action", NeedsJob: true,
			DisabledReason: "no finished jobs with files",
			JobFilter: func(j *database.Job) bool {
				return j.Status == database.StatusFinished && j.OutputFile != ""
			}},
		{Chord: "A O", Label: "Browse Orphaned Items", HintLabel: "Orphans", Category: "Action"},
	}

	if a.OnListClientTokens != nil {
		items = append(items, ActionMenuItem{Chord: "A K", Label: "Manage Client Tokens", HintLabel: "Tokens", Category: "Action"})
	}

	// Request — conditional on callbacks being set
	if a.OnBackfillRescan != nil {
		items = append(items, ActionMenuItem{Chord: "R B", Label: "Re-scan Feed History", HintLabel: "Backfill", Category: "Request"})
	}
	if a.OnRecheckCookies != nil {
		items = append(items, ActionMenuItem{Chord: "R C", Label: "Recheck Cookies", HintLabel: "Cookies", Category: "Request"})
	}
	if a.OnForceRefreshCookies != nil {
		items = append(items, ActionMenuItem{Chord: "R F", Label: "Refresh Cookies from Browser", HintLabel: "Refresh Cookies", Category: "Request"})
	}
	if a.OnCheckUpdate != nil {
		items = append(items, ActionMenuItem{Chord: "R V", Label: "Check for Updates", HintLabel: "Version", Category: "Request"})
	}
	if a.OnForceCheck != nil {
		items = append(items, ActionMenuItem{Chord: "R M", Label: "Check Monitors Now", HintLabel: "Monitors", Category: "Request"})
	}
	// R N: View release notes. Pending-update notes when an update is
	// available; otherwise fetches the current running version's notes
	// from GitHub. Registered whenever we have either source available
	// so the chord parser doesn't reject it as invalid.
	if a.updateAvailable != nil {
		items = append(items, ActionMenuItem{Chord: "R N", Label: "View Release Notes " + a.updateAvailable.TagName, HintLabel: "Notes", Category: "Request"})
	} else if a.OnFetchReleaseNotes != nil {
		items = append(items, ActionMenuItem{Chord: "R N", Label: "View Release Notes (current version)", HintLabel: "Notes", Category: "Request"})
	}
	if a.updateAvailable != nil && a.OnApplyUpdate != nil {
		items = append(items, ActionMenuItem{Chord: "R U", Label: "Apply Update " + a.updateAvailable.TagName, HintLabel: "Update", Category: "Request"})
	}
	if a.OnVerifySignature != nil {
		items = append(items, ActionMenuItem{Chord: "R S", Label: "Verify Signature", HintLabel: "Signature", Category: "Request"})
	}
	if a.OnRestart != nil {
		items = append(items, ActionMenuItem{Chord: "R P", Label: "Restart Program", HintLabel: "Restart", Category: "Request", NeedsConfirm: true})
	}

	// Open
	items = append(items,
		ActionMenuItem{Chord: "O F", Label: "Open Folder", HintLabel: "Folder", Category: "Open", NeedsJob: true,
			DisabledReason: "no jobs with folders",
			JobFilter:      func(j *database.Job) bool { return canOpenFolder(j) }},
		ActionMenuItem{Chord: "O S", Label: "Open Stream Page", HintLabel: "Stream", Category: "Open", NeedsJob: true,
			DisabledReason: "no jobs with stream URLs",
			JobFilter:      func(j *database.Job) bool { return canOpenStream(j) }},
		ActionMenuItem{Chord: "O W", Label: "Open Web UI", HintLabel: "Web", Category: "Open"},
		// The label hedges without naming a mechanism. It renders the same
		// on every platform, and each mechanism is missing on some of them:
		// OSC 52 is not what a local Windows console uses, and clip.exe is
		// not what anything else uses. What the press can promise in advance
		// is the attempt; the feedback line names the outcome afterwards
		// (CORE-15, O-W).
		ActionMenuItem{Chord: "O C", Label: "Copy Stream URL (best effort)", HintLabel: "Copy URL", Category: "Open", NeedsJob: true,
			DisabledReason: "no jobs with stream URLs",
			JobFilter:      func(j *database.Job) bool { return canOpenStream(j) }},
		ActionMenuItem{Chord: "O G", Label: "Open GitHub Page", HintLabel: "GitHub", Category: "Open"},
	)

	// Extras — the four side errands that used to crowd Request: none of them
	// acts on a job or asks the running program for anything, and all four are
	// conditional on their own callback being set.
	//
	// Emitted AFTER every Open item on purpose: the action menu heads a group
	// wherever consecutive Category changes (action_menu.go), so an Extras
	// item slipped in earlier would cut Open into two headed halves.
	//
	// E Y: the terminal's half of the dashboard's Integrations card. Gated on
	// the STATUS callback alone — the overlay is worth reading on a host where
	// the install would fail, and I explains itself there.
	if a.OnYtdlpPluginStatus != nil {
		items = append(items, ActionMenuItem{Chord: "E Y", Label: "yt-dlp Plugin", HintLabel: "yt-dlp", Category: "Extras"})
	}
	// E L: open the setup wizard's cookie step alone, so an interactive login
	// is reachable after first run. Gated on the callback for the same reason
	// R F is, and cmd/moombox binds it unconditionally: StartSetup is
	// acquisition and is never gated on cookies.auto_enabled.
	if a.setupWiz.OnStartAutoCookie != nil {
		items = append(items, ActionMenuItem{Chord: "E L", Label: "Cookie Login", HintLabel: "Login", Category: "Extras"})
	}
	// E I: the browser-free half of the same answer E L gives. It imports a
	// Netscape cookies.txt the operator exported elsewhere, through the same
	// verify-and-roll-back path as the Web dashboard's import panel — the one
	// re-authentication route that works on a headless host.
	if a.OnImportCookieFile != nil {
		items = append(items, ActionMenuItem{Chord: "E I", Label: "Import Cookie File", HintLabel: "Import Cookies", Category: "Extras"})
	}
	if a.OnGetStats != nil {
		items = append(items, ActionMenuItem{Chord: "E T", Label: "Statistics", HintLabel: "Stats", Category: "Extras"})
	}

	// Filter + Other
	items = append(items,
		ActionMenuItem{Chord: "F", Label: "Cycle Filter", HintLabel: "Filter", Category: "Filter"},
		ActionMenuItem{Chord: "`", Label: "Settings", HintLabel: "Settings", Category: "Other"},
		ActionMenuItem{Chord: "?", Label: "Help", HintLabel: "Help", Category: "Other"},
		ActionMenuItem{Chord: "Q Q", Label: "Quit", HintLabel: "Quit", Category: "Other"},
	)
	return items
}

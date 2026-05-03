package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// dispatchAction executes a chord action. For job-specific actions, job comes from
// the selected task (keyboard chords) or from the menu's job picker (menu flow).
func (a *App) dispatchAction(chord string, job *database.Job) (tea.Model, tea.Cmd) {
	switch chord {
	case "A A":
		a.feedbackMsg = ""
		a.addVideo.SetSize(a.width, a.height)
		a.addVideo.Open()
	case "A Z":
		a.feedbackMsg = ""
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
				if j.Status != database.StatusCancelled && j.Status != database.StatusError && j.Status != database.StatusCookies {
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
				case database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusMuxing, database.StatusCookies:
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
		if job == nil && a.taskList.SelectedCount() > 0 && a.OnDeleteJob != nil {
			count := 0
			for _, id := range a.taskList.SelectedIDs() {
				a.OnDeleteJob(id)
				count++
			}
			a.taskList.ClearSelection()
			a.setFeedback(fmt.Sprintf("Deleted %d jobs", count))
			a.updateSelectedJob()
		} else if job != nil && a.OnDeleteJob != nil {
			a.OnDeleteJob(job.ID)
			a.setFeedback(fmt.Sprintf("Deleted: %s", job.Title))
			a.taskList.MoveUp()
			a.updateSelectedJob()
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
		return a, tea.Batch(a.fetchOrphansCmd(), a.filesDlg.SpinnerInit())
	case "A K":
		if a.OnListClientTokens != nil {
			a.clientTokensDlg.SetSize(a.width, a.height)
			a.clientTokensDlg.Open()
			return a, tea.Batch(a.fetchClientTokensCmd(), a.clientTokensDlg.SpinnerInit())
		}
	case "R C":
		if a.OnRecheckCookies != nil {
			a.setFeedback("Rechecking cookies...")
			recheckFn := a.OnRecheckCookies
			return a, safeCmd(func() tea.Msg {
				ytAuth, twAuth := recheckFn()
				return cookieRecheckResultMsg{YouTubeAuth: ytAuth, TwitchAuth: twAuth}
			})
		}
	case "R F":
		if a.OnForceRefreshCookies != nil {
			a.setFeedback("Running browser cookie refresh...")
			refreshFn := a.OnForceRefreshCookies
			return a, safeCmd(func() tea.Msg {
				ok, err := refreshFn()
				return cookieForceRefreshResultMsg{Success: ok, Err: err}
			})
		}
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
			return a, nil
		}
		if a.OnFetchReleaseNotes == nil || a.version == "" {
			a.setFeedback("Cannot fetch release notes (offline build or unconfigured)")
			return a, nil
		}
		// No pending update — fetch current version's notes asynchronously.
		a.releaseNotesPopup.open("v"+a.version, "Loading release notes…", a.width, a.height)
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
		if a.updateAvailable != nil && a.OnApplyUpdate != nil {
			a.setFeedback(fmt.Sprintf("Updating to %s...", a.updateAvailable.TagName))
			ver := a.updateAvailable.Version
			applyFn := a.OnApplyUpdate
			return a, safeCmd(func() tea.Msg {
				errStr := applyFn(ver)
				return updateApplyResultMsg{Err: errStr}
			})
		}
		a.setFeedback("No update available — use R V to check")
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
				a.setFeedback("Copied: " + url)
				return a, tea.SetClipboard(url)
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
	case "F":
		a.handleFilter()
	case "`":
		if a.cfg != nil {
			a.settings.SetSize(a.width, a.height)
			a.settings.OnSave = a.OnSaveConfig
			a.settings.OnRestart = a.OnRestart
			a.settings.OnRestartRequired = func() { a.restartPending = true }
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

// buildMenuItems builds context-sensitive action menu items.
// This is the single source of truth for all chords, menu entries, feedback hints, and help text.
func (a *App) buildMenuItems() []ActionMenuItem {
	items := []ActionMenuItem{
		{Chord: "A A", Label: "Add Video", HintLabel: "Add", Category: "Action"},
		{Chord: "A Z", Label: "Import Archive", HintLabel: "Import", Category: "Action"},
		{Chord: "A R", Label: "Resume Job", HintLabel: "Resume", Category: "Action", NeedsJob: true,
			DisabledReason: "no resumable jobs",
			JobFilter: func(j *database.Job) bool {
				canResume := (j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies) &&
					j.Platform == "youtube"
				if canResume && a.HasStagingFiles != nil {
					return a.HasStagingFiles(j.ID)
				}
				return false
			}},
		{Chord: "A I", Label: "Reinitialize Job", HintLabel: "Reinit", Category: "Action", NeedsJob: true,
			DisabledReason: "no retriable jobs",
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
			}},
		{Chord: "A C", Label: "Cancel Job", HintLabel: "Cancel", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			DisabledReason: "no active jobs",
			JobFilter: func(j *database.Job) bool {
				return j.Status != database.StatusFinished && j.Status != database.StatusCancelled && j.Status != database.StatusError
			}},
		{Chord: "A D", Label: "Delete Job", HintLabel: "Delete", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			DisabledReason: "no deletable jobs"},
		{Chord: "A T", Label: "Trim Video", HintLabel: "Trim", Category: "Action", NeedsJob: true,
			DisabledReason: "no finished jobs with files",
			JobFilter: func(j *database.Job) bool {
				return j.Status == database.StatusFinished && j.OutputFile != ""
			}},
		{Chord: "A O", Label: "Browse Orphaned Files", HintLabel: "Orphans", Category: "Action"},
	}

	if a.OnListClientTokens != nil {
		items = append(items, ActionMenuItem{Chord: "A K", Label: "Manage Client Tokens", HintLabel: "Tokens", Category: "Action"})
	}

	// Request — conditional on callbacks being set
	if a.OnRecheckCookies != nil {
		items = append(items, ActionMenuItem{Chord: "R C", Label: "Recheck Cookies", HintLabel: "Cookies", Category: "Request"})
	}
	if a.OnForceRefreshCookies != nil {
		items = append(items, ActionMenuItem{Chord: "R F", Label: "Force Cookie Refresh", HintLabel: "Force Refresh", Category: "Request"})
	}
	if a.OnCheckUpdate != nil {
		items = append(items, ActionMenuItem{Chord: "R V", Label: "Check for Updates", HintLabel: "Version", Category: "Request"})
	}
	if a.updateAvailable != nil {
		items = append(items, ActionMenuItem{Chord: "R N", Label: "View Release Notes " + a.updateAvailable.TagName, HintLabel: "Notes", Category: "Request"})
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
		ActionMenuItem{Chord: "O C", Label: "Copy Stream URL", HintLabel: "Copy URL", Category: "Open", NeedsJob: true,
			DisabledReason: "no jobs with stream URLs",
			JobFilter:      func(j *database.Job) bool { return canOpenStream(j) }},
	)

	// Filter + Other
	items = append(items,
		ActionMenuItem{Chord: "F", Label: "Cycle Filter", HintLabel: "Filter", Category: "Filter"},
		ActionMenuItem{Chord: "`", Label: "Settings", HintLabel: "Settings", Category: "Other"},
		ActionMenuItem{Chord: "?", Label: "Help", HintLabel: "Help", Category: "Other"},
		ActionMenuItem{Chord: "Q Q", Label: "Quit", HintLabel: "Quit", Category: "Other"},
	)
	return items
}

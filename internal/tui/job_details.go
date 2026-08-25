package tui

import (
	"fmt"
	"image/color"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const labelWidth = 14

// JobDetailsModel renders details for a selected job.
type JobDetailsModel struct {
	job             *database.Job
	viewport        viewport.Model
	progress        progress.Model
	width, height   int
	focused         bool
	rows            []detailRow
	hideDescription bool

	// Marquee for scrolling the title value when it overflows.
	marquee Marquee

	// Progress overlay: updated at 100ms by progress store,
	// without rebuilding all rows.
	progressOverlay *ProgressData

	// Version display
	version    string
	updateInfo *UpdateStatusMsg
}

type rowKind int

const (
	rowField rowKind = iota
	rowHeader
	rowSeparator
	rowProgressBar
)

type detailRow struct {
	kind  rowKind
	label string
	value string
	color color.Color
	link  string // OSC 8 hyperlink URL; empty means no hyperlink
}

// NewJobDetailsModel creates a new job details model.
func NewJobDetailsModel() *JobDetailsModel {
	vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(1))
	// Use helpViewportKeyMap so arrow keys work but letter keys (j/k/d/u/f/b)
	// don't conflict with app chord bindings. Mouse scroll is handled
	// explicitly in app.go handleMouse.
	vp.KeyMap = helpViewportKeyMap()
	vp.FillHeight = true

	pb := progress.New(progress.WithoutPercentage())
	pb.Full = '█'
	pb.Empty = '░'
	pb.EmptyColor = ColorGray

	return &JobDetailsModel{viewport: vp, progress: pb}
}

// titleValueWidth computes the available width for the title value column.
func (m *JobDetailsModel) titleValueWidth() int {
	valueW := max(m.width-2-labelWidth, 10)
	return valueW
}

// SetJob updates the displayed job.
func (m *JobDetailsModel) SetJob(job *database.Job) {
	prevID := ""
	prevTitle := ""
	if m.job != nil {
		prevID = m.job.ID
		prevTitle = m.job.Title
	}
	m.job = job
	newID := ""
	if job != nil {
		newID = job.ID
	}
	// Transient view state (progress overlay, scroll position, marquee
	// phase) resets only on a genuine job switch. SetJob is also called as
	// a same-job re-sync whenever ANY job's display column changes — with
	// several active downloads, unconditional resets would blank the
	// overlay and snap the scrolling title back to position 0 constantly.
	if prevID != newID {
		m.progressOverlay = nil
	}
	m.buildRows()
	m.updateViewportContent()
	if prevID != newID {
		m.viewport.GotoTop()
	}
	if job != nil {
		if prevID != newID || prevTitle != job.Title {
			title := job.Title
			if title == "" {
				title = job.VideoID
			}
			m.marquee.Reset(title, m.titleValueWidth())
		}
	} else {
		m.marquee.Reset("", 0)
	}
}

// HasProgress reports whether a progress overlay is currently displayed.
// Used by the progress tick to clear a stale overlay (SetProgress(nil))
// after the store entry was deleted on a terminal status — SetJob no longer
// resets the overlay on same-job refreshes.
func (m *JobDetailsModel) HasProgress() bool {
	return m.progressOverlay != nil
}

// SetProgress updates the progress overlay from the progress store.
// Rebuilds rows so that segment counts, Duration, Starts In, and chat messages
// are recomputed from live data every 100ms (matches TS re-render behavior).
func (m *JobDetailsModel) SetProgress(p *ProgressData) {
	m.progressOverlay = p
	yOffset := m.viewport.YOffset()
	m.buildRows()
	m.updateViewportContent()
	m.viewport.SetYOffset(yOffset)
}

// RefreshMarqueeFrame re-renders the viewport content so the Title row picks
// up the marquee's current offset. The frame is read at renderRow time (not
// stored in m.rows), so no row rebuild is needed — only the content
// re-render. Called from the 150ms marquee tick: without it, the frame baked
// into the viewport string only refreshed on the next SetProgress /
// RefreshRelativeTimes rebuild (500ms–1s), so the scrolling title visibly
// jumped 3–7 positions at a time instead of stepping once per tick (the task
// list marquee never had this problem — its delegate renders live per frame).
// No-op when nothing scrolls; preserves scroll like SetProgress.
func (m *JobDetailsModel) RefreshMarqueeFrame() {
	if m.job == nil || !m.marquee.NeedsScroll() {
		return
	}
	yOffset := m.viewport.YOffset()
	m.updateViewportContent()
	m.viewport.SetYOffset(yOffset)
}

// RefreshRelativeTimes rebuilds the detail rows so wall-clock-derived text
// (the "5m ago" relative suffixes on Created/Updated/DL Started) stays
// current for jobs the progress tick never rebuilds — terminal statuses
// have their progressStore entry deleted, so SetProgress skips them and the
// suffixes would otherwise freeze at whatever the last rebuild computed.
// Called at 1Hz from the app tick; preserves scroll like SetProgress.
func (m *JobDetailsModel) RefreshRelativeTimes() {
	if m.job == nil {
		return
	}
	yOffset := m.viewport.YOffset()
	m.buildRows()
	m.updateViewportContent()
	m.viewport.SetYOffset(yOffset)
}

// SetSize updates the panel dimensions.
func (m *JobDetailsModel) SetSize(w, h int) {
	prevW := m.width
	m.width = w
	m.height = h
	contentH := max(h-3, 1)
	m.viewport.SetWidth(w - 2)
	m.viewport.SetHeight(contentH)
	m.updateViewportContent()
	// Recalculate marquee width when panel width changes (e.g. focus change,
	// or initial WindowSizeMsg arriving after jobs are already loaded).
	if prevW != w && m.job != nil {
		title := m.job.Title
		if title == "" {
			title = m.job.VideoID
		}
		m.marquee.Reset(title, m.titleValueWidth())
	}
}

// SetFocused sets the focus state.
func (m *JobDetailsModel) SetFocused(f bool) {
	m.focused = f
}

// ToggleDescription toggles the description section visibility.
func (m *JobDetailsModel) ToggleDescription() {
	m.hideDescription = !m.hideDescription
	m.buildRows()
	m.updateViewportContent()
}

// ScrollUp scrolls the detail view up by one line.
func (m *JobDetailsModel) ScrollUp() {
	m.viewport.ScrollUp(1)
}

// ScrollDown scrolls the detail view down by one line.
func (m *JobDetailsModel) ScrollDown() {
	m.viewport.ScrollDown(1)
}

// UpdateViewport passes a Bubble Tea message to the viewport for built-in
// key/mouse handling and returns any resulting command.
func (m *JobDetailsModel) UpdateViewport(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return cmd
}

// updateViewportContent rebuilds the viewport's string content from m.rows.
func (m *JobDetailsModel) updateViewportContent() {
	contentW := max(m.width-2, 1)
	if m.job == nil {
		m.viewport.SetContent(DimStyle.Render("Select a job to view details"))
		return
	}
	var lines []string
	for _, r := range m.rows {
		lines = append(lines, m.renderRow(r, contentW))
	}
	m.viewport.SetContentLines(lines)
}

func (m *JobDetailsModel) buildRows() {
	m.rows = nil
	j := m.job
	if j == nil {
		return
	}

	isTwitch := j.Platform == "twitch"
	isFinished := j.Status == database.StatusFinished
	status := string(j.Status)

	// === Basic Info (no header, matching TS) ===
	title := j.Title
	if title == "" {
		title = j.VideoID
	}
	m.addField("Title", title)
	ch := j.ChannelName
	if ch == "" {
		ch = "Unknown"
	}
	m.addField("Channel", ch)
	// Video ID: conditional label for Twitch (J12), fallback to ID (J14)
	vidIDLabel := "Video ID"
	if isTwitch {
		vidIDLabel = "Stream ID"
	}
	vidID := j.VideoID
	if vidID == "" {
		vidID = j.ID
	}
	m.addField(vidIDLabel, vidID)
	// URL only shown if present
	if j.URL != "" {
		m.addFieldLink("URL", j.URL, j.URL)
	}
	m.addFieldColor("Status", status, StatusColor(status))
	// VOD/Live type indicator (matching Web UI)
	if j.IsVod {
		m.addField("Type", "VOD")
	} else {
		m.addField("Type", "Live")
	}
	if isTwitch {
		m.addFieldColor("Platform", "Twitch", ColorTwitch)
		if j.TwitchCategory != "" {
			m.addField("Category", j.TwitchCategory)
		}
		if j.TwitchQuality != "" {
			m.addField("Quality", j.TwitchQuality)
		}
	}

	// === Advanced Options (J2/J3) - separate section, not merged into Media ===
	hasAdvanced := false
	if !isTwitch {
		hasVideoItag := j.SelectedVideoItag != nil
		hasAudioItag := j.SelectedAudioItag != nil
		hasTimeRange := j.StartTime != nil || j.EndTime != nil
		hasAdvanced = hasVideoItag || hasAudioItag || hasTimeRange

		if hasAdvanced {
			m.rows = append(m.rows, detailRow{kind: rowSeparator})
			m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Advanced Options"})

			if hasVideoItag {
				var label string
				if *j.SelectedVideoItag == -1 {
					label = "None (audio only)"
				} else {
					label = fmt.Sprintf("itag %d", *j.SelectedVideoItag)
				}
				m.addFieldColor("Video Format", label, ColorCookies) // magenta
			}
			if hasAudioItag {
				var label string
				if *j.SelectedAudioItag == -1 {
					label = "None (video only)"
				} else {
					label = fmt.Sprintf("itag %d", *j.SelectedAudioItag)
				}
				m.addFieldColor("Audio Format", label, ColorCookies)
			}
			if hasTimeRange {
				startStr := "0:00"
				if j.StartTime != nil {
					startStr = formatTimeSeconds(*j.StartTime)
				}
				endStr := "end"
				if j.EndTime != nil {
					endStr = formatTimeSeconds(*j.EndTime)
				}
				m.addFieldColor("Time Range", startStr+" - "+endStr, ColorCookies)
			}
		}
	}

	// === Parts section — multi-part outputs from quality/gap splits ===
	// Mirrors the Web UI's Parts rows: part number comes from SegmentIndex
	// (matching the " - partN" filenames), not the loop index — short-skipped
	// spans can leave holes in the numbering.
	if len(j.Segments) > 0 {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: fmt.Sprintf("Parts (%d)", len(j.Segments))})
		for _, seg := range j.Segments {
			val := seg.Quality
			if seg.DurationSeconds > 0 {
				val += " - " + utils.FormatDurationHuman(time.Duration(seg.DurationSeconds)*time.Second)
			}
			if seg.FileSize != nil && *seg.FileSize > 0 {
				val += " - " + formatFileSize(*seg.FileSize)
			}
			if seg.VideoWidth != nil && seg.VideoHeight != nil && *seg.VideoWidth > 0 && *seg.VideoHeight > 0 {
				val += fmt.Sprintf(" - %dx%d", *seg.VideoWidth, *seg.VideoHeight)
			}
			if seg.ChatFile != "" {
				val += " - chat"
			}
			m.rows = append(m.rows, detailRow{kind: rowField, label: fmt.Sprintf("Part %d", seg.SegmentIndex+1), value: val, color: ColorFinished})
		}
	}

	// === Trims section (J5/J16) ===
	if len(j.Trims) > 0 {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: fmt.Sprintf("Trims (%d)", len(j.Trims))})
		for _, tr := range j.Trims {
			// Time format: MM:SS (J5)
			label := formatTimeSeconds(tr.StartTime) + " - " + formatTimeSeconds(tr.EndTime)
			// Value: raw seconds (match TS Math.floor(trim.duration) + "s")
			val := fmt.Sprintf("%ds", int(tr.Duration))
			if tr.FileSize != nil && *tr.FileSize > 0 {
				val += ", " + formatFileSize(*tr.FileSize)
			} else {
				val += ", ?" // match TS: shows "?" for unknown file size
			}
			m.rows = append(m.rows, detailRow{kind: rowField, label: label, value: val, color: ColorFinished}) // cyan (J16)
		}
	}

	// === Progress section — shown for active states only, not for Error/Cancelled ===
	isActiveState := !isFinished && !j.IsTerminal()
	if isActiveState {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Progress"})

		// Get progress data from overlay (real-time) with fallback to job
		progress := j.Progress
		percent := j.Percent
		speed := j.Speed
		eta := j.ETA
		if p := m.progressOverlay; p != nil {
			if p.Progress != "" {
				progress = p.Progress
			}
			if p.Percent > 0 {
				percent = p.Percent
			}
			if p.Speed != "" {
				speed = p.Speed
			}
			if p.ETA != "" {
				eta = p.ETA
			}
		}

		if progress != "" {
			m.addField("Progress", progress)
		}
		if percent > 0 {
			m.rows = append(m.rows, detailRow{
				kind:  rowProgressBar,
				value: status, // status string used for gradient mapping
				color: StatusColor(status),
			})
		}
		if speed != "" {
			m.addField("Speed", speed)
		}
		if eta != "" {
			m.addField("ETA", eta)
		}

		// Segment rows during active download (J8) - use progressOverlay for real-time counts
		m.addSegmentRowsWithOverlay(j)

		// Chat status with color coding (J7) - always shown (not gated by hasProgress)
		// Combined line: "status (X messages)" matching Web UI
		chatStatus := j.ChatStatus
		totalChatMsgs := j.TotalChatMessages
		if p := m.progressOverlay; p != nil {
			if p.ChatStatus != "" {
				chatStatus = p.ChatStatus
			}
			if p.TotalChatMessages != nil {
				totalChatMsgs = p.TotalChatMessages
			}
		}
		if chatStatus != "" {
			chatVal := chatStatus
			if totalChatMsgs != nil && *totalChatMsgs > 0 {
				chatVal += fmt.Sprintf(" (%d messages)", *totalChatMsgs)
			}
			chatColor := m.chatStatusColor(chatStatus)
			m.addFieldColor("Chat", chatVal, chatColor)
		}
	}

	// === Media section ===
	hasVideo := j.VideoWidth != nil && *j.VideoWidth > 0
	hasHeight := j.VideoHeight != nil && *j.VideoHeight > 0
	hasSegs := (j.LastVideoSeq != nil && *j.LastVideoSeq > 0) || (j.LastAudioSeq != nil && *j.LastAudioSeq > 0)
	hasChat := j.TotalChatMessages != nil && *j.TotalChatMessages > 0
	hasFileSize := j.FileSize != nil && *j.FileSize > 0
	hasFps := j.VideoFps != nil && *j.VideoFps > 0
	hasGaps := len(j.Gaps) > 0
	hasMediaContent := (hasVideo && hasHeight) || hasFps || hasFileSize || (isFinished && (hasSegs || hasChat || hasGaps))
	if hasMediaContent {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Media"})

		if hasVideo && hasHeight {
			m.addField("Resolution", fmt.Sprintf("%dx%d", *j.VideoWidth, *j.VideoHeight))
		}
		if j.VideoFps != nil && *j.VideoFps > 0 {
			m.addField("FPS", fmt.Sprintf("%d", *j.VideoFps))
		}
		if hasFileSize {
			m.addField("File Size", formatFileSize(*j.FileSize))
		}

		// Segment counters (only for finished jobs - active show in Progress)
		if isFinished {
			m.addSegmentRows(j)
		}
	}

	// === Parse timestamps once for reuse in Timestamps and Duration sections ===
	now := time.Now()
	var parsedStreamStart, parsedCreatedAt, parsedUpdatedAt time.Time
	if j.StreamStartTime != "" {
		parsedStreamStart, _ = time.Parse(time.RFC3339, j.StreamStartTime)
	}
	if j.CreatedAt != "" {
		parsedCreatedAt, _ = time.Parse(time.RFC3339, j.CreatedAt)
	}
	if j.UpdatedAt != "" {
		parsedUpdatedAt, _ = time.Parse(time.RFC3339, j.UpdatedAt)
	}

	// === Timestamps (J11 - human-readable with relative suffix, J13 - Scheduled label) — BEFORE Duration to match TS ===
	m.rows = append(m.rows, detailRow{kind: rowSeparator})
	m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Timestamps"})
	if j.CreatedAt != "" {
		m.addField("Created", formatDateStrRelative(j.CreatedAt, now))
	}
	if j.DownloadStartedAt != "" {
		m.addField("DL Started", formatDateStrRelative(j.DownloadStartedAt, now))
	}
	if j.UpdatedAt != "" {
		m.addField("Updated", formatDateStrRelative(j.UpdatedAt, now))
	}
	if j.StreamStartTime != "" {
		// Show "Scheduled" if upcoming and future (J13)
		label := "Stream Start"
		if j.Status == database.StatusUpcoming && !parsedStreamStart.IsZero() && parsedStreamStart.After(now) {
			label = "Scheduled"
		}
		m.addField(label, formatDateStrRelative(j.StreamStartTime, now))
	}
	if j.StreamEndTime != "" {
		m.addField("Stream End", formatDateStrRelative(j.StreamEndTime, now))
	}
	if j.LengthSeconds != nil && *j.LengthSeconds > 0 {
		m.addField("Video Length", formatDuration(time.Duration(*j.LengthSeconds)*time.Second))
	}

	// === Duration section (J1 - AFTER Timestamps, matching TS) ===
	switch {
	case j.Status == database.StatusLive || j.Status == database.StatusDownloading || j.Status == database.StatusMuxing:
		// Active: show elapsed duration. StatusQueued is deliberately absent —
		// a job waiting for an archive slot has no elapsed recording time.
		startMs := parsedStreamStart
		if startMs.IsZero() {
			startMs = parsedCreatedAt
		}
		if !startMs.IsZero() {
			elapsed := now.Sub(startMs)
			m.addFieldColor("Duration", formatDuration(elapsed), ColorGreen)
		}
	case isFinished:
		// Finished: show "Job Time" only if no lengthSeconds
		if j.LengthSeconds == nil || *j.LengthSeconds <= 0 {
			if !parsedCreatedAt.IsZero() && !parsedUpdatedAt.IsZero() {
				m.addField("Job Time", formatDuration(parsedUpdatedAt.Sub(parsedCreatedAt)))
			}
		}
	case j.Status == database.StatusUpcoming && !parsedStreamStart.IsZero():
		// Upcoming: countdown (J13)
		if parsedStreamStart.After(now) {
			m.addFieldColor("Starts In", formatDuration(parsedStreamStart.Sub(now)), ColorUpcoming)
		}
	}

	// === Error (J6 - word wrapping) ===
	if j.Error != "" {
		// A membership park is not a broken video and not a dead session: the
		// cookies are alive, the account simply lacks the membership, and this
		// job deliberately will not resume on its own until a DIFFERENT account
		// is supplied. Heading it "Error" in red buried that distinction — the
		// text below already names the remedy, so let the heading agree with it.
		header, color := "Error", ColorError
		if j.Status == database.StatusCookies && j.ParkReason == database.ParkReasonMembership {
			header, color = "Not a Member", ColorCookies
		}
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: header})
		contentW := max(m.width-2, 20)
		valueW := contentW - labelWidth
		if valueW < 10 {
			valueW = contentW
		}
		for _, line := range wrapText(j.Error, valueW) {
			m.rows = append(m.rows, detailRow{kind: rowField, value: line, color: color})
		}
	}

	// === File (J15 - label "File", truncate, gate on Filename to match TS) ===
	if j.Filename != "" {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		// Build file:/// hyperlink from the full path (OutputFile) or OutputDirectory+Filename.
		fileLink := ""
		if fullPath := j.OutputFile; fullPath != "" {
			fileLink = (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(fullPath)}).String()
		} else if j.OutputDirectory != "" {
			fullPath = filepath.Join(j.OutputDirectory, j.Filename)
			fileLink = (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(fullPath)}).String()
		}
		m.addFieldLink("File", j.Filename, fileLink)
	}

	// === Description ===
	if j.Description != "" && !m.hideDescription {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Description"})
		// Match TS: wrap to value column width (contentWidth - 14), not full width
		descW := max(m.width-2-labelWidth, 20)
		for _, line := range wrapText(j.Description, descW) {
			m.rows = append(m.rows, detailRow{kind: rowField, value: line})
		}
	}
}

// addSegmentRowsWithOverlay adds segment rows using progressOverlay for real-time counts.
// Used during active download in Progress section (J8).
func (m *JobDetailsModel) addSegmentRowsWithOverlay(j *database.Job) {
	p := m.progressOverlay

	// Use overlay values with fallback to job (match TS pd?.field ?? job.field)
	lastVideoSeq := j.LastVideoSeq
	lastAudioSeq := j.LastAudioSeq
	totalVideoSeq := j.TotalVideoSeq
	totalAudioSeq := j.TotalAudioSeq
	if p != nil {
		if p.LastVideoSeq != nil {
			lastVideoSeq = p.LastVideoSeq
		}
		if p.LastAudioSeq != nil {
			lastAudioSeq = p.LastAudioSeq
		}
		if p.TotalVideoSeq != nil {
			totalVideoSeq = p.TotalVideoSeq
		}
		if p.TotalAudioSeq != nil {
			totalAudioSeq = p.TotalAudioSeq
		}
	}

	m.addSegmentRowsCommon(j, lastVideoSeq, lastAudioSeq, totalVideoSeq, totalAudioSeq, false)
}

// addSegmentRows adds segment counter rows from job data only (for finished jobs in Media section).
func (m *JobDetailsModel) addSegmentRows(j *database.Job) {
	m.addSegmentRowsCommon(j, j.LastVideoSeq, j.LastAudioSeq, j.TotalVideoSeq, j.TotalAudioSeq, true)
}

// addSegmentRowsCommon renders segment counters and gap info. If showChat is true,
// also shows chat message count (used for finished jobs in Media section).
func (m *JobDetailsModel) addSegmentRowsCommon(j *database.Job, lastVideoSeq, lastAudioSeq, totalVideoSeq, totalAudioSeq *int, showChat bool) {
	isTwitch := j.Platform == "twitch"

	// Combined inline segment display: "V: x/y | A: x/y" (matching Web UI)
	if lastVideoSeq != nil {
		vStr := fmt.Sprintf("%d", *lastVideoSeq)
		if totalVideoSeq != nil && *totalVideoSeq > 0 {
			vStr += fmt.Sprintf("/%d", *totalVideoSeq)
		}
		if isTwitch {
			m.addField("Segments", vStr)
		} else {
			aStr := ""
			if lastAudioSeq != nil {
				aStr = fmt.Sprintf("%d", *lastAudioSeq)
				if totalAudioSeq != nil && *totalAudioSeq > 0 {
					aStr += fmt.Sprintf("/%d", *totalAudioSeq)
				}
			}
			if aStr != "" {
				m.addField("Segments", fmt.Sprintf("V: %s | A: %s", vStr, aStr))
			} else {
				m.addField("Segments", fmt.Sprintf("V: %s", vStr))
			}
		}
	}

	// Chat message count (in Media section for finished jobs)
	if showChat && j.TotalChatMessages != nil && *j.TotalChatMessages > 0 {
		m.addField("Chat Msgs", fmt.Sprintf("%d", *j.TotalChatMessages))
	}

	// Gaps with detail breakdown (J4)
	if len(j.Gaps) > 0 {
		videoGaps := 0
		audioGaps := 0
		for _, g := range j.Gaps {
			switch g.Stream {
			case "video":
				videoGaps++
			case "audio":
				audioGaps++
			}
		}
		var detail string
		var parts []string
		if videoGaps > 0 {
			parts = append(parts, fmt.Sprintf("video: %d", videoGaps))
		}
		if audioGaps > 0 {
			parts = append(parts, fmt.Sprintf("audio: %d", audioGaps))
		}
		if len(parts) > 0 {
			detail = " (" + strings.Join(parts, ", ") + ")"
		}
		m.addFieldColor("Gaps", fmt.Sprintf("%d segments%s", len(j.Gaps), detail), ColorWarning)
	}
}

// chatStatusColor returns appropriate color for chat status (J7).
func (m *JobDetailsModel) chatStatusColor(status string) color.Color {
	lower := strings.ToLower(status)
	if strings.Contains(lower, "downloading") || strings.Contains(lower, "running") {
		return ColorGreen
	}
	if strings.Contains(lower, "finished") || strings.Contains(lower, "complete") {
		return ColorFinished // cyan
	}
	if strings.Contains(lower, "error") || strings.Contains(lower, "failed") {
		return ColorError
	}
	return ColorGray // match TS: gray for unrecognized chat status
}

func (m *JobDetailsModel) addField(label, value string) {
	m.rows = append(m.rows, detailRow{kind: rowField, label: label, value: value})
}

func (m *JobDetailsModel) addFieldColor(label, value string, c color.Color) {
	m.rows = append(m.rows, detailRow{kind: rowField, label: label, value: value, color: c})
}

func (m *JobDetailsModel) addFieldLink(label, value, link string) {
	m.rows = append(m.rows, detailRow{kind: rowField, label: label, value: value, link: link})
}

// View renders the job details panel.
func (m *JobDetailsModel) View() string {
	contentW := max(m.width-2, 1)

	// Title and border color: status-colored when focused + job selected (match TS)
	titleColor := ColorCyan
	borderColor := ColorGray
	if m.focused {
		borderColor = ColorCyan
		if m.job != nil {
			sc := StatusColor(string(m.job.Status))
			titleColor = sc
			borderColor = sc
		}
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(titleColor)
	var header string
	if m.job != nil {
		header = titleStyle.Render("Details")
		if m.hideDescription {
			header += " " + DimStyle.Render("[filtered]")
		}
		// Scroll percentage indicator (match TS [XX%])
		// Only append if it fits within contentW to prevent header wrapping
		if m.viewport.TotalLineCount() > m.viewport.Height() {
			pct := int(math.Round(m.viewport.ScrollPercent() * 100))
			suffix := " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
			if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
				header += suffix
			}
		}
	} else {
		header = titleStyle.Render("Details") + DimStyle.Render(" (no job selected)")
	}

	// Version indicator (right-aligned in header)
	if m.version != "" {
		versionText := "v" + m.version
		if m.updateInfo != nil {
			updateStyle := lipgloss.NewStyle().Foreground(ColorGreen).Bold(true)
			versionText = updateStyle.Render("v" + m.version + " ⬆ Update!")
		} else {
			versionText = DimStyle.Render(versionText)
		}
		headerW := lipgloss.Width(header)
		versionW := lipgloss.Width(versionText)
		gap := contentW - headerW - versionW
		if gap > 0 {
			header += strings.Repeat(" ", gap) + versionText
		}
	}

	content := header + "\n" + m.viewport.View()

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor)

	return style.Width(m.width).Height(m.height).Render(content)
}

func (m *JobDetailsModel) renderRow(r detailRow, maxW int) string {
	switch r.kind {
	case rowHeader:
		return HeaderStyle.Render(r.label)

	case rowSeparator:
		// Cap separator at 40 chars (match TS Math.min(contentWidth, 40))
		sepW := min(maxW, 40)
		return SeparatorStyle.Render(strings.Repeat("─", sepW))

	case rowProgressBar:
		// Use progress overlay percent if available (J9: 1 decimal)
		percent := m.job.Percent
		if m.progressOverlay != nil && m.progressOverlay.Percent > 0 {
			percent = m.progressOverlay.Percent
		}
		pctLabel := fmt.Sprintf(" %.1f%%", percent)
		barW := min(maxW-labelWidth-runewidth.StringWidth(pctLabel), 30)
		if barW < 5 {
			// Narrow fallback: plain text percentage
			label := padRight("", labelWidth)
			return label + lipgloss.NewStyle().Foreground(r.color).Render(strings.TrimSpace(pctLabel))
		}
		m.progress.SetWidth(barW)
		// Gradient: blend from the previous phase's color into the current status color
		colorA, colorB := progressGradient(r.value)
		progress.WithColors(colorA, colorB)(&m.progress)
		label := padRight("", labelWidth)
		return label + m.progress.ViewAs(percent/100) + lipgloss.NewStyle().Foreground(r.color).Render(pctLabel)

	case rowField:
		if r.label == "" {
			// Description/error line (no label)
			if r.color != nil {
				return lipgloss.NewStyle().Foreground(r.color).Render(r.value)
			}
			return r.value
		}
		// Label without colon (match TS padEnd(14) with no colon)
		label := lipgloss.NewStyle().Foreground(ColorGray).Render(
			padRight(r.label, labelWidth),
		)
		// Truncate value to fit available width
		valueW := max(maxW-labelWidth, 10)
		val := r.value
		if r.label == "Title" && m.marquee.NeedsScroll() {
			val = m.marquee.View()
		} else {
			val = truncateString(val, valueW)
		}
		valStyle := lipgloss.NewStyle()
		if r.color != nil {
			valStyle = valStyle.Foreground(r.color)
		}
		if r.link != "" {
			valStyle = valStyle.Hyperlink(r.link)
		}
		if r.color != nil || r.link != "" {
			val = valStyle.Render(val)
		}
		return label + val
	}
	return ""
}

// progressGradient returns gradient start/end colors for a progress bar based on job status.
// The bar blends from the previous phase color into the current status color.
func progressGradient(status string) (color.Color, color.Color) {
	switch status {
	case "Downloading", "Live":
		return ColorUpcoming, ColorDownloading
	case "Muxing":
		return ColorDownloading, ColorMuxing
	default:
		c := StatusColor(status)
		return c, c
	}
}

func padRight(s string, w int) string {
	sw := runewidth.StringWidth(s)
	if sw >= w {
		return s
	}
	return s + strings.Repeat(" ", w-sw)
}

// formatDuration formats a duration with spaces like TypeScript (J10).
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSeconds := int(math.Floor(d.Seconds()))
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// formatTimeSeconds formats seconds as M:SS or H:MM:SS for trim/time ranges (J5).
func formatTimeSeconds(seconds float64) string {
	total := int(math.Floor(seconds))
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatDateStrRelative formats a date with a relative suffix, e.g. "2025-01-02 15:04:05 (2h ago)".
func formatDateStrRelative(dateStr string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		return dateStr
	}
	abs := t.Local().Format("2006-01-02 15:04:05")
	d := now.Sub(t)
	if d < 0 {
		d = -d
		return abs + " (in " + formatRelativeDuration(d) + ")"
	}
	if d < time.Second {
		return abs + " (just now)"
	}
	return abs + " (" + formatRelativeDuration(d) + " ago)"
}

func formatRelativeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m > 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
}

var formatFileSize = utils.FormatFileSize

func wrapText(text string, maxW int) []string {
	if maxW <= 0 {
		return []string{text}
	}
	var lines []string
	for paragraph := range strings.SplitSeq(text, "\n") {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		words := strings.Fields(paragraph)
		var line string
		for _, word := range words {
			// Split very long words that exceed maxW (match TS char-level break)
			for runewidth.StringWidth(word) > maxW {
				var prefix []rune
				w := 0
				for _, r := range word {
					rw := runewidth.RuneWidth(r)
					if w+rw > maxW {
						break
					}
					prefix = append(prefix, r)
					w += rw
				}
				if len(prefix) == 0 {
					// At least 1 rune to avoid infinite loop
					runes := []rune(word)
					prefix = runes[:1]
				}
				if line != "" {
					lines = append(lines, line)
					line = ""
				}
				lines = append(lines, string(prefix))
				word = string([]rune(word)[len(prefix):])
			}
			if word == "" {
				continue
			}
			if line == "" {
				line = word
			} else if runewidth.StringWidth(line+" "+word) <= maxW {
				line += " " + word
			} else {
				lines = append(lines, line)
				line = word
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

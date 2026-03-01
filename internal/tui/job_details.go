package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const labelWidth = 14

// JobDetailsModel renders details for a selected job.
type JobDetailsModel struct {
	job           *database.Job
	scrollOffset  int
	width, height int
	focused       bool
	rows          []detailRow

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
	color lipgloss.Color
}

// NewJobDetailsModel creates a new job details model.
func NewJobDetailsModel() *JobDetailsModel {
	return &JobDetailsModel{}
}

// SetJob updates the displayed job.
func (m *JobDetailsModel) SetJob(job *database.Job) {
	// Reset scroll to top on job change (match TS useEffect)
	prevID := ""
	if m.job != nil {
		prevID = m.job.ID
	}
	m.job = job
	m.progressOverlay = nil
	m.buildRows()
	newID := ""
	if job != nil {
		newID = job.ID
	}
	if prevID != newID {
		m.scrollOffset = 0
	} else if m.scrollOffset > m.maxScroll() {
		m.scrollOffset = m.maxScroll()
	}
	// Reset marquee for title field (use m.width - 2 to match renderRow's
	// maxW = contentW = m.width - 2, minus labelWidth for the value column).
	if job != nil {
		valueW := m.width - 2 - labelWidth
		if valueW < 10 {
			valueW = 10
		}
		m.marquee.Reset(job.Title, valueW)
	} else {
		m.marquee.Reset("", 0)
	}
}

// SetProgress updates the progress overlay from the progress store.
// Rebuilds rows so that segment counts, Duration, Starts In, and chat messages
// are recomputed from live data every 100ms (matches TS re-render behavior).
func (m *JobDetailsModel) SetProgress(p *ProgressData) {
	m.progressOverlay = p
	m.buildRows()
}

// SetSize updates the panel dimensions.
func (m *JobDetailsModel) SetSize(w, h int) {
	prevW := m.width
	m.width = w
	m.height = h
	// Recalculate marquee width when panel width changes (e.g. focus change,
	// or initial WindowSizeMsg arriving after jobs are already loaded).
	if prevW != w && m.job != nil {
		valueW := m.width - 2 - labelWidth
		if valueW < 10 {
			valueW = 10
		}
		m.marquee.Reset(m.job.Title, valueW)
	}
}

// SetFocused sets the focus state.
func (m *JobDetailsModel) SetFocused(f bool) {
	m.focused = f
}

// ScrollUp scrolls the detail view up.
func (m *JobDetailsModel) ScrollUp() {
	if m.scrollOffset > 0 {
		m.scrollOffset--
	}
}

// ScrollDown scrolls the detail view down.
func (m *JobDetailsModel) ScrollDown() {
	if m.scrollOffset < m.maxScroll() {
		m.scrollOffset++
	}
}

func (m *JobDetailsModel) contentHeight() int {
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

func (m *JobDetailsModel) maxScroll() int {
	ms := len(m.rows) - m.contentHeight()
	if ms < 0 {
		return 0
	}
	return ms
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
	m.addField("Title", j.Title)
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
		m.addField("URL", j.URL)
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

	// === Progress section (J17 - always shown if !isFinished, matching TS unconditional) ===
	if !isFinished {
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
				value: fmt.Sprintf("%.1f%%", percent), // J9: 1 decimal
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
	hasMediaContent := hasVideo || hasHeight || hasFps || hasFileSize || (isFinished && (hasSegs || hasChat || hasGaps))
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

	// === Timestamps (J11 - human-readable with relative suffix, J13 - Scheduled label) — BEFORE Duration to match TS ===
	now := time.Now()
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
		if j.Status == database.StatusUpcoming {
			if t, err := time.Parse(time.RFC3339, j.StreamStartTime); err == nil && t.After(now) {
				label = "Scheduled"
			}
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
		// Active: show elapsed duration
		var startMs time.Time
		if j.StreamStartTime != "" {
			startMs, _ = time.Parse(time.RFC3339, j.StreamStartTime)
		}
		if startMs.IsZero() && j.CreatedAt != "" {
			startMs, _ = time.Parse(time.RFC3339, j.CreatedAt)
		}
		if !startMs.IsZero() {
			elapsed := now.Sub(startMs)
			m.addFieldColor("Duration", formatDuration(elapsed), ColorGreen)
		}
	case isFinished:
		// Finished: show "Job Time" only if no lengthSeconds
		if j.LengthSeconds == nil || *j.LengthSeconds <= 0 {
			if j.CreatedAt != "" && j.UpdatedAt != "" {
				created, err1 := time.Parse(time.RFC3339, j.CreatedAt)
				updated, err2 := time.Parse(time.RFC3339, j.UpdatedAt)
				if err1 == nil && err2 == nil {
					m.addField("Job Time", formatDuration(updated.Sub(created)))
				}
			}
		}
	case j.Status == database.StatusUpcoming && j.StreamStartTime != "":
		// Upcoming: countdown (J13)
		if startTime, err := time.Parse(time.RFC3339, j.StreamStartTime); err == nil {
			if startTime.After(now) {
				m.addFieldColor("Starts In", formatDuration(startTime.Sub(now)), ColorUpcoming)
			}
		}
	}

	// === Error (J6 - word wrapping) ===
	if j.Error != "" {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Error"})
		contentW := m.width - 2
		if contentW < 20 {
			contentW = 20
		}
		valueW := contentW - labelWidth
		if valueW < 10 {
			valueW = contentW
		}
		for _, line := range wrapText(j.Error, valueW) {
			m.rows = append(m.rows, detailRow{kind: rowField, value: line, color: ColorError})
		}
	}

	// === File (J15 - label "File", truncate, gate on Filename to match TS) ===
	if j.Filename != "" {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.addField("File", j.Filename)
	}

	// === Description ===
	if j.Description != "" {
		m.rows = append(m.rows, detailRow{kind: rowSeparator})
		m.rows = append(m.rows, detailRow{kind: rowHeader, label: "Description"})
		// Match TS: wrap to value column width (contentWidth - 14), not full width
		descW := m.width - 2 - labelWidth
		if descW < 20 {
			descW = 20
		}
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
			if g.Stream == "video" {
				videoGaps++
			} else if g.Stream == "audio" {
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
func (m *JobDetailsModel) chatStatusColor(status string) lipgloss.Color {
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

func (m *JobDetailsModel) addFieldColor(label, value string, color lipgloss.Color) {
	m.rows = append(m.rows, detailRow{kind: rowField, label: label, value: value, color: color})
}

// View renders the job details panel.
func (m *JobDetailsModel) View() string {
	contentW := m.width - 2
	if contentW < 1 {
		contentW = 1
	}

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
		// Scroll percentage indicator (match TS [XX%])
		// Only append if it fits within contentW to prevent header wrapping
		if ms := m.maxScroll(); ms > 0 {
			pct := int(math.Round(float64(m.scrollOffset) * 100 / float64(ms)))
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
			versionText = updateStyle.Render("v"+m.version+" ⬆ Update!")
			if m.focused {
				versionText += " " + DimStyle.Render("UU to update")
			}
		} else {
			versionText = DimStyle.Render(versionText)
			if m.focused {
				versionText += " " + DimStyle.Render("UU to check")
			}
		}
		headerW := lipgloss.Width(header)
		versionW := lipgloss.Width(versionText)
		gap := contentW - headerW - versionW
		if gap > 0 {
			header += strings.Repeat(" ", gap) + versionText
		}
	}

	contentH := m.contentHeight()
	var lines []string

	if m.job == nil {
		lines = append(lines, DimStyle.Render("Select a job to view details"))
	} else {
		end := m.scrollOffset + contentH
		if end > len(m.rows) {
			end = len(m.rows)
		}
		for i := m.scrollOffset; i < end; i++ {
			lines = append(lines, m.renderRow(m.rows[i], contentW))
		}
	}

	for len(lines) < contentH {
		lines = append(lines, strings.Repeat(" ", contentW))
	}

	content := header + "\n" + strings.Join(lines, "\n")

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor)

	return style.Width(contentW).Height(m.height - 2).Render(content)
}

func (m *JobDetailsModel) renderRow(r detailRow, maxW int) string {
	switch r.kind {
	case rowHeader:
		return HeaderStyle.Render(r.label)

	case rowSeparator:
		// Cap separator at 40 chars (match TS Math.min(contentWidth, 40))
		sepW := maxW
		if sepW > 40 {
			sepW = 40
		}
		return SeparatorStyle.Render(strings.Repeat("─", sepW))

	case rowProgressBar:
		// Use progress overlay percent if available (J9: 1 decimal)
		percent := m.job.Percent
		if m.progressOverlay != nil && m.progressOverlay.Percent > 0 {
			percent = m.progressOverlay.Percent
		}
		return renderProgressBar(percent, r.color, maxW)

	case rowField:
		if r.label == "" {
			// Description/error line (no label)
			if r.color != "" {
				return lipgloss.NewStyle().Foreground(r.color).Render(r.value)
			}
			return r.value
		}
		// Label without colon (match TS padEnd(14) with no colon)
		label := lipgloss.NewStyle().Foreground(ColorGray).Render(
			padRight(r.label, labelWidth),
		)
		// Truncate value to fit available width
		valueW := maxW - labelWidth
		if valueW < 10 {
			valueW = 10
		}
		val := r.value
		if r.label == "Title" && m.marquee.NeedsScroll() {
			val = m.marquee.View()
		} else {
			val = truncateString(val, valueW)
		}
		if r.color != "" {
			val = lipgloss.NewStyle().Foreground(r.color).Render(val)
		}
		return label + val
	}
	return ""
}

func renderProgressBar(percent float64, color lipgloss.Color, maxW int) string {
	pctLabel := fmt.Sprintf(" %.1f%%", percent) // J9: 1 decimal, space prefix matches TS
	barW := maxW - labelWidth - len(pctLabel) - 2 // 2 for bar brackets (match TS)
	if barW > 30 {
		barW = 30 // cap at 30 like TypeScript
	}
	if barW < 5 {
		// Narrow fallback: plain text percentage (match TS behavior)
		label := padRight("", labelWidth)
		return label + lipgloss.NewStyle().Foreground(color).Render(strings.TrimSpace(pctLabel))
	}
	filled := int(math.Round(float64(barW) * percent / 100)) // match TS Math.round
	if filled > barW {
		filled = barW
	}
	empty := barW - filled

	bar := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(ColorGray).Render(strings.Repeat("░", empty))

	label := padRight("", labelWidth)

	return label + bar + lipgloss.NewStyle().Foreground(color).Render(pctLabel)
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

// formatTimeSeconds formats seconds as MM:SS for trim ranges (J5).
func formatTimeSeconds(seconds float64) string {
	total := int(math.Floor(seconds))
	m := total / 60
	s := total % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatDateStr converts an ISO 8601 date string to human-readable LOCAL time (J11).
// Match TS: new Date(dateStr) + getFullYear/getHours etc. which returns local time.
func formatDateStr(dateStr string) string {
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		return dateStr // fallback to raw
	}
	return t.Local().Format("2006-01-02 15:04:05")
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
	for _, paragraph := range strings.Split(text, "\n") {
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

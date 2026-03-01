package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Filter represents a task list filter mode.
type Filter int

const (
	FilterAll Filter = iota
	FilterActive
	FilterErrors
	FilterFinished
)

func (f Filter) String() string {
	switch f {
	case FilterActive:
		return "Active"
	case FilterErrors:
		return "Errors"
	case FilterFinished:
		return "Finished"
	default:
		return "All"
	}
}

// Next cycles to the next filter.
func (f Filter) Next() Filter {
	return (f + 1) % 4
}

// virtualItem is either a job or the archive divider.
type virtualItem struct {
	job      *database.Job // nil for divider
	divider  bool
	count    int  // archived count (only for divider)
	archived bool // true if this job is in the archived section
}

// TaskListModel manages the job list panel.
type TaskListModel struct {
	jobs         []*database.Job
	jobIndex     map[string]int // job ID → index in jobs slice (O(1) lookup)
	activeJobs   []*database.Job // non-archived jobs (filtered + sorted)
	archivedJobs []*database.Job // archived finished jobs
	virtualItems []virtualItem   // what we actually render
	selectedIndex int
	scrollOffset  int
	width, height int
	focused       bool
	filter        Filter
	archiveExpanded bool
	hideFinishedAgeDays int // from config, default 30

	// Marquee for scrolling selected item title.
	marquee Marquee

	// Progress store for efficient progress display.
	progressStore *ProgressStore

	// Countdown timers.
	NextFeedCheck   time.Time
	NextDecapiCheck time.Time
	NextTwitchCheck time.Time
}

// NewTaskListModel creates a new task list model.
func NewTaskListModel() *TaskListModel {
	return &TaskListModel{
		hideFinishedAgeDays: 30,
		progressStore:       NewProgressStore(),
	}
}

// SetHideFinishedAgeDays updates the archive threshold.
func (m *TaskListModel) SetHideFinishedAgeDays(days int) {
	m.hideFinishedAgeDays = days
	m.rebuildVirtualList()
}

// SetJobs updates the job list and re-sorts.
func (m *TaskListModel) SetJobs(jobs []*database.Job) {
	m.jobs = jobs
	m.rebuildJobIndex()
	m.rebuildVirtualList()
	m.resetMarquee()
}

// UpdateJob replaces a single job by ID using the index map (O(1) lookup).
// Returns true if the job was found and replaced.
func (m *TaskListModel) UpdateJob(job *database.Job) bool {
	if idx, ok := m.jobIndex[job.ID]; ok && idx < len(m.jobs) {
		m.jobs[idx] = job
		m.rebuildVirtualList()
		return true
	}
	return false
}

func (m *TaskListModel) rebuildJobIndex() {
	m.jobIndex = make(map[string]int, len(m.jobs))
	for i, j := range m.jobs {
		m.jobIndex[j.ID] = i
	}
}

// SelectedJob returns the currently selected job, or nil.
func (m *TaskListModel) SelectedJob() *database.Job {
	if m.selectedIndex < 0 || m.selectedIndex >= len(m.virtualItems) {
		return nil
	}
	item := m.virtualItems[m.selectedIndex]
	if item.divider {
		return nil
	}
	return item.job
}

// SelectedIsDivider returns true if the selected item is the archive divider.
func (m *TaskListModel) SelectedIsDivider() bool {
	if m.selectedIndex < 0 || m.selectedIndex >= len(m.virtualItems) {
		return false
	}
	return m.virtualItems[m.selectedIndex].divider
}

// SetSize updates the panel dimensions.
func (m *TaskListModel) SetSize(w, h int) {
	prevW := m.width
	m.width = w
	m.height = h
	// Recalculate marquee width when panel width changes (e.g. focus change,
	// or initial WindowSizeMsg arriving after jobs are already loaded).
	if prevW != w {
		m.resetMarquee()
	}
}

// SetFocused sets the focus state.
func (m *TaskListModel) SetFocused(f bool) {
	m.focused = f
}

// MoveUp moves the selection up.
func (m *TaskListModel) MoveUp() {
	if m.selectedIndex > 0 {
		m.selectedIndex--
		m.ensureVisible()
		m.resetMarquee()
	}
}

// MoveDown moves the selection down.
func (m *TaskListModel) MoveDown() {
	maxIdx := len(m.virtualItems) - 1
	if m.selectedIndex < maxIdx {
		m.selectedIndex++
		m.ensureVisible()
		m.resetMarquee()
	}
}

// resetMarquee resets the marquee animation for the currently selected job title.
func (m *TaskListModel) resetMarquee() {
	if job := m.SelectedJob(); job != nil {
		title := job.Title
		if title == "" {
			title = job.VideoID
		}
		m.marquee.Reset(title, m.titleWidth(job))
	} else {
		m.marquee.Reset("", 0)
	}
}

// titleWidth computes the available title width for a job in the list.
func (m *TaskListModel) titleWidth(job *database.Job) int {
	contentW := m.width - 2
	if contentW < 1 {
		contentW = 1
	}
	selectorWidth := 2
	iconWidth := 2
	platformTagWidth := 0
	if job.Platform == "twitch" {
		platformTagWidth = 5
	}
	// Include progress width for active jobs
	progressTextWidth := 0
	isActive := job.Status == database.StatusDownloading || job.Status == database.StatusMuxing
	percent := job.Percent
	if isActive {
		if p := m.progressStore.Get(job.ID); p != nil {
			percent = p.Percent
		}
	}
	if isActive && percent > 0 {
		progressTextWidth = len(fmt.Sprintf("%.0f%% ", percent))
	}
	tw := contentW - selectorWidth - iconWidth - progressTextWidth - platformTagWidth
	if tw < 5 {
		tw = 5
	}
	return tw
}

// CycleFilter cycles through filter modes.
func (m *TaskListModel) CycleFilter() {
	m.filter = m.filter.Next()
	m.rebuildVirtualList()
	// Reset selection and scroll on filter change (match TS)
	m.selectedIndex = 0
	m.scrollOffset = 0
	if m.selectedIndex >= len(m.virtualItems) {
		m.selectedIndex = max(0, len(m.virtualItems)-1)
	}
	m.ensureVisible()
	m.resetMarquee()
}

// ToggleArchive toggles the archive divider expansion.
func (m *TaskListModel) ToggleArchive() {
	m.archiveExpanded = !m.archiveExpanded
	m.rebuildVirtualList()
	if m.selectedIndex >= len(m.virtualItems) {
		m.selectedIndex = max(0, len(m.virtualItems)-1)
	}
}

func (m *TaskListModel) ensureVisible() {
	contentH := m.contentHeight()
	if contentH <= 0 {
		return
	}
	if m.selectedIndex < m.scrollOffset {
		m.scrollOffset = m.selectedIndex
	}
	if m.selectedIndex >= m.scrollOffset+contentH {
		m.scrollOffset = m.selectedIndex - contentH + 1
	}
}

func (m *TaskListModel) contentHeight() int {
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

func statusPriority(status database.JobStatus) int {
	switch status {
	case database.StatusError:
		return 0
	case database.StatusCookies:
		return 1
	case database.StatusDownloading:
		return 2
	case database.StatusMuxing:
		return 3
	case database.StatusLive:
		return 4
	case database.StatusUpcoming:
		return 5
	case database.StatusCancelled:
		return 6
	case database.StatusFinished:
		return 7
	default:
		return 8
	}
}

func isTerminalStatus(status database.JobStatus) bool {
	return status == database.StatusFinished || status == database.StatusCancelled
}

// isJobArchived returns true if a finished job should be in the archive section
// based on the hide_finished_age_days setting.
// ageDays == 0: instantly archive all finished jobs.
// ageDays < 0: never archive (all stay active).
// ageDays > 0: archive finished jobs older than N days.
func isJobArchived(j *database.Job, ageDays int, now time.Time) bool {
	if ageDays < 0 || j.Status != database.StatusFinished || j.UpdatedAt == "" {
		return false
	}
	if t, err := time.Parse(time.RFC3339, j.UpdatedAt); err == nil {
		diffDays := int(math.Ceil(now.Sub(t).Hours() / 24))
		return ageDays == 0 || diffDays > ageDays
	}
	return false
}

func (m *TaskListModel) rebuildVirtualList() {
	now := time.Now()
	ageDays := m.hideFinishedAgeDays

	var active, archived []*database.Job
	for _, j := range m.jobs {
		if !m.passesFilter(j) {
			continue
		}

		if isJobArchived(j, ageDays, now) {
			archived = append(archived, j)
			continue
		}
		active = append(active, j)
	}

	// Sort active jobs
	sort.SliceStable(active, func(i, j int) bool {
		pi := statusPriority(active[i].Status)
		pj := statusPriority(active[j].Status)
		if pi != pj {
			return pi < pj
		}
		if pi >= 6 {
			return active[i].UpdatedAt > active[j].UpdatedAt
		}
		return strings.ToLower(active[i].Title) < strings.ToLower(active[j].Title)
	})

	// Sort archived: newest first
	sort.SliceStable(archived, func(i, j int) bool {
		return archived[i].UpdatedAt > archived[j].UpdatedAt
	})

	m.activeJobs = active
	m.archivedJobs = archived

	// Build virtual item list
	m.virtualItems = nil
	for _, j := range active {
		m.virtualItems = append(m.virtualItems, virtualItem{job: j})
	}

	showArchive := len(archived) > 0 && (m.filter == FilterAll || m.filter == FilterFinished)
	if showArchive {
		m.virtualItems = append(m.virtualItems, virtualItem{
			divider: true,
			count:   len(archived),
		})
		if m.archiveExpanded {
			for _, j := range archived {
				m.virtualItems = append(m.virtualItems, virtualItem{job: j, archived: true})
			}
		}
	}

	// Clamp scroll offset when virtual list shrinks (match TS useEffect)
	maxOff := max(0, len(m.virtualItems)-(m.contentHeight()))
	if m.scrollOffset > maxOff {
		m.scrollOffset = maxOff
	}
}

func (m *TaskListModel) passesFilter(j *database.Job) bool {
	switch m.filter {
	case FilterActive:
		return !isTerminalStatus(j.Status) && j.Status != database.StatusError && j.Status != database.StatusCookies
	case FilterErrors:
		return j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies
	case FilterFinished:
		return j.Status == database.StatusFinished
	default:
		return true
	}
}

// View renders the task list panel.
func (m *TaskListModel) View() string {
	contentW := m.width - 2
	if contentW < 1 {
		contentW = 1
	}

	header := m.renderHeader(contentW)

	contentH := m.contentHeight()
	var lines []string

	// Empty state message (T5)
	if len(m.virtualItems) == 0 {
		lines = append(lines, DimStyle.Render("No tasks. Press A to add, or use Web UI."))
	}

	visibleEnd := m.scrollOffset + contentH
	if visibleEnd > len(m.virtualItems) {
		visibleEnd = len(m.virtualItems)
	}

	for i := m.scrollOffset; i < visibleEnd; i++ {
		item := m.virtualItems[i]
		selected := i == m.selectedIndex
		if item.divider {
			lines = append(lines, m.renderDivider(item.count, selected, contentW))
		} else {
			lines = append(lines, m.renderJob(item.job, selected, item.archived, contentW))
		}
	}

	for len(lines) < contentH {
		lines = append(lines, strings.Repeat(" ", contentW))
	}

	content := header + "\n" + strings.Join(lines, "\n")

	style := UnfocusedBorder
	if m.focused {
		style = FocusedBorder
	}

	return style.Width(contentW).Height(m.height - 2).Render(content)
}

func (m *TaskListModel) renderHeader(w int) string {
	// Status summary with icon counts (T1)
	left := "Tasks"
	summary := m.buildStatusSummary()
	if summary != "" {
		left += " (" + summary + ")"
	} else {
		// Fallback: just total count
		total := 0
		for _, item := range m.virtualItems {
			if !item.divider {
				total++
			}
		}
		left += fmt.Sprintf(" (%d)", total)
	}

	if m.filter != FilterAll {
		left += " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("["+m.filter.String()+"]")
	}

	// Countdown timers (T3 - match TS format, colored dots before labels)
	greenDot := lipgloss.NewStyle().Foreground(ColorGreen).Render("\u25CF")
	grayDot := DimStyle.Render("\u25CF")
	var timers []string
	if !m.NextFeedCheck.IsZero() {
		d := time.Until(m.NextFeedCheck)
		dot := grayDot
		if d <= 0 {
			dot = greenDot
		}
		timers = append(timers, dot+DimStyle.Render("F:"+formatCountdown(d)))
	}
	if !m.NextDecapiCheck.IsZero() {
		d := time.Until(m.NextDecapiCheck)
		dot := grayDot
		if d <= 0 {
			dot = greenDot
		}
		timers = append(timers, dot+DimStyle.Render("D:"+formatCountdown(d)))
	}
	if !m.NextTwitchCheck.IsZero() {
		d := time.Until(m.NextTwitchCheck)
		dot := grayDot
		if d <= 0 {
			dot = greenDot
		}
		timers = append(timers, dot+DimStyle.Render("T:"+formatCountdown(d)))
	}

	right := strings.Join(timers, " ")

	// Scrollbar range display (T4)
	if len(m.virtualItems) > m.contentHeight() {
		start := m.scrollOffset + 1
		end := m.scrollOffset + m.contentHeight()
		if end > len(m.virtualItems) {
			end = len(m.virtualItems)
		}
		scrollInfo := fmt.Sprintf("[%d-%d/%d]", start, end, len(m.virtualItems))
		if right != "" {
			right += " "
		}
		right += scrollInfo
	}

	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)

	// If right side doesn't fit, drop it to prevent header from wrapping
	// (wrapping adds an extra line, causing vertical shifting via Height truncation)
	if leftW+1+rightW > w {
		right = ""
		rightW = 0
	}

	padding := w - leftW - rightW
	if padding < 1 {
		padding = 1
	}

	return TitleStyle.Render(left) + strings.Repeat(" ", padding) + DimStyle.Render(right)
}

// buildStatusSummary generates status summary with icons like "3● 2▼ 1⚙" (T1).
// Counts exclude archived finished jobs (match TS which counts from allSortedJobs).
func (m *TaskListModel) buildStatusSummary() string {
	now := time.Now()
	ageDays := m.hideFinishedAgeDays
	counts := make(map[database.JobStatus]int)
	for _, j := range m.jobs {
		if isJobArchived(j, ageDays, now) {
			continue
		}
		counts[j.Status]++
	}

	// Display order matches TypeScript
	order := []database.JobStatus{
		database.StatusLive,
		database.StatusDownloading,
		database.StatusMuxing,
		database.StatusUpcoming,
		database.StatusError,
		database.StatusCookies,
		database.StatusCancelled,
		database.StatusFinished,
	}

	var parts []string
	for _, s := range order {
		c := counts[s]
		if c > 0 {
			icon := StatusIcon(string(s))
			color := StatusColor(string(s))
			parts = append(parts,
				lipgloss.NewStyle().Foreground(color).Render(fmt.Sprintf("%d%s", c, icon)),
			)
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func (m *TaskListModel) renderDivider(count int, selected bool, maxW int) string {
	icon := "\u25b8" // ▸ collapsed
	if m.archiveExpanded {
		icon = "\u25be" // ▾ expanded
	}

	label := fmt.Sprintf("%s Archived (%d)", icon, count)

	ruleLen := (maxW - runewidth.StringWidth(label) - 6) / 2
	if ruleLen < 1 {
		ruleLen = 1
	}
	rule := strings.Repeat("\u2500", ruleLen)

	prefix := "> "
	if !selected {
		prefix = "  "
	}

	line := prefix + rule + " " + label + " " + rule

	color := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true)
	if selected {
		color = color.Background(lipgloss.Color("#333399")).Foreground(ColorWhite)
	}

	rendered := color.Render(line)
	lineW := runewidth.StringWidth(line)
	if lineW < maxW {
		rendered += strings.Repeat(" ", maxW-lineW)
	}
	return rendered
}

func (m *TaskListModel) renderJob(job *database.Job, selected bool, archived bool, maxW int) string {
	statusStr := string(job.Status)
	icon := StatusIcon(statusStr)
	color := StatusColor(statusStr)
	dimmed := archived && !selected

	// Only show progress for Downloading/Muxing (match TS isActive check)
	isActive := job.Status == database.StatusDownloading || job.Status == database.StatusMuxing
	percent := job.Percent
	if isActive {
		if p := m.progressStore.Get(job.ID); p != nil {
			percent = p.Percent
		}
	}
	showProgress := isActive && percent > 0
	progressText := ""
	progressTextWidth := 0
	if showProgress {
		progressText = fmt.Sprintf("%.0f%% ", percent)
		progressTextWidth = len(progressText)
	}

	// Fixed widths for layout (match TS pre-calculation to avoid ANSI measuring)
	selectorWidth := 2 // "> " or "  "
	iconWidth := 2     // icon + space
	platformTagWidth := 0
	if job.Platform == "twitch" {
		platformTagWidth = 5 // "[TW] "
	}

	// Calculate title width with minimum (match TS Math.max(5, ...))
	titleWidth := maxW - selectorWidth - iconWidth - progressTextWidth - platformTagWidth
	if titleWidth < 5 {
		titleWidth = 5
	}

	// Prepare title
	title := job.Title
	if title == "" {
		title = job.VideoID
	}
	if selected && m.marquee.NeedsScroll() {
		title = m.marquee.View()
	} else {
		title = truncateString(title, titleWidth)
	}
	// Pad title to fill remaining width (match TS padEndToWidth)
	tw := runewidth.StringWidth(title)
	if tw < titleWidth {
		title += strings.Repeat(" ", titleWidth-tw)
	}

	// Build styled output - order: selector | icon | progress | [TW] | title (match TS)
	selectedBg := lipgloss.NewStyle().Background(lipgloss.Color("4")).Foreground(ColorWhite)
	var parts []string

	// Selector
	if selected {
		parts = append(parts, selectedBg.Render("> "))
	} else {
		parts = append(parts, "  ")
	}

	// Status icon (retains status color even when selected, per TS)
	iconStyle := lipgloss.NewStyle().Foreground(color)
	if dimmed {
		iconStyle = iconStyle.Faint(true)
	}
	parts = append(parts, iconStyle.Render(icon)+" ")

	// Progress (before platform tag, matching TS order; uses status color not always green)
	if showProgress {
		pctStyle := lipgloss.NewStyle().Foreground(color)
		if dimmed {
			pctStyle = pctStyle.Faint(true)
		}
		parts = append(parts, pctStyle.Render(progressText))
	}

	// Platform tag
	if job.Platform == "twitch" {
		tagStyle := lipgloss.NewStyle().Foreground(ColorTwitch)
		if dimmed {
			tagStyle = tagStyle.Faint(true)
		}
		parts = append(parts, tagStyle.Render("[TW] "))
	}

	// Title (blue bg + white text when selected, status color when not selected, match TS)
	if selected {
		parts = append(parts, selectedBg.Render(title))
	} else if dimmed {
		parts = append(parts, lipgloss.NewStyle().Faint(true).Render(title))
	} else {
		parts = append(parts, lipgloss.NewStyle().Foreground(color).Render(title))
	}

	return strings.Join(parts, "")
}

// formatCountdown formats a duration for display (T3).
func formatCountdown(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func truncateString(s string, maxW int) string {
	if runewidth.StringWidth(s) <= maxW {
		return s
	}
	if maxW <= 1 {
		return "\u2026" // …
	}
	w := 0
	for i, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > maxW-1 {
			return s[:i] + "\u2026"
		}
		w += rw
	}
	return s
}

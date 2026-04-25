package tui

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Package-level styles for task list rendering (avoid alloc per render).
var (
	taskSelectedBgStyle = lipgloss.NewStyle().Background(lipgloss.Color("4")).Foreground(ColorWhite)
	taskBatchCheckStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#2ecc71")) // green ✓ for batch selection
	taskTwitchTagStyle  = lipgloss.NewStyle().Foreground(ColorTwitch)
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
		return "Issues"
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

// taskItem wraps a job or archive divider as a list.Item.
type taskItem struct {
	job      *database.Job // nil for divider
	divider  bool
	count    int  // archived count (only for divider)
	archived bool // true if this job is in the archived section
}

func (t taskItem) FilterValue() string {
	if t.job != nil {
		return t.job.Title
	}
	return ""
}

// taskDelegate renders task list items with status colors and progress.
type taskDelegate struct {
	panel *TaskListModel
}

func (d taskDelegate) Height() int                             { return 1 }
func (d taskDelegate) Spacing() int                            { return 0 }
func (d taskDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d taskDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	ti, ok := item.(taskItem)
	if !ok {
		return
	}
	selected := index == m.Index()
	contentW := m.Width()
	if ti.divider {
		fmt.Fprint(w, d.panel.renderDivider(ti.count, selected, contentW))
	} else {
		fmt.Fprint(w, d.panel.renderJob(ti.job, selected, ti.archived, contentW))
	}
}

// TaskListModel manages the job list panel.
type TaskListModel struct {
	jobs     []*database.Job
	jobIndex map[string]int // job ID → index in jobs slice (O(1) lookup)
	list     list.Model

	width, height       int
	focused             bool
	filter              Filter
	archiveExpanded     bool
	hideFinishedAgeDays int // from config, default 30

	// Batch selection state (Space to toggle, mirrors Web UI batch operations).
	selected map[string]bool // selected job IDs for batch operations

	// Marquee for scrolling selected item title.
	marquee Marquee

	// Progress store for efficient progress display.
	progressStore *ProgressStore

	// Countdown timers.
	NextFeedCheck   time.Time
	NextDecapiCheck time.Time
	NextTwitchCheck time.Time

	// Transient flag: set when setup wizard completes, shown once in empty state.
	JustCompletedSetup bool
}

// NewTaskListModel creates a new task list model.
func NewTaskListModel() *TaskListModel {
	m := &TaskListModel{
		hideFinishedAgeDays: 30,
		progressStore:       NewProgressStore(),
		selected:            make(map[string]bool),
	}
	m.list = m.newTaskList()
	return m
}

func (m *TaskListModel) newTaskList() list.Model {
	delegate := taskDelegate{panel: m}
	l := list.New(nil, delegate, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowFilter(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()

	// Only arrow keys for navigation (letter keys handled by app chord system)
	km := l.KeyMap
	km.CursorUp.SetKeys("up")
	km.CursorDown.SetKeys("down")
	km.NextPage.SetKeys("pgdown")
	km.PrevPage.SetKeys("pgup")
	km.GoToStart.SetKeys("home")
	km.GoToEnd.SetKeys("end")
	l.KeyMap = km

	return l
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

// Jobs returns the current job list (caller must NOT mutate the
// returned slice; it's the live underlying storage). Used by the
// JobAdded lifecycle handler to feed downstream consumers
// (statusBar, actionMenu) without re-fetching from the database.
func (m *TaskListModel) Jobs() []*database.Job {
	return m.jobs
}

// AddJob appends a new job to the list and re-sorts. Used by the
// JobAdded lifecycle handler to apply the database event without a
// full clear+rebuild — the TUI keeps its existing rows + selection
// and just slots the new entry in. DECISIONS #21.
//
// No-ops if a job with the same ID already exists (the SetJobs /
// initial-list path may have already inserted it during an earlier
// snapshot, and AddJob arriving second shouldn't double-insert).
func (m *TaskListModel) AddJob(job *database.Job) {
	if job == nil {
		return
	}
	if _, exists := m.jobIndex[job.ID]; exists {
		return
	}
	m.jobs = append(m.jobs, job)
	m.rebuildJobIndex()
	m.rebuildVirtualList()
	m.resetMarquee()
}

// RemoveJob drops the job with the given ID from the list and
// rebuilds the virtual list. Used by the JobDeleted lifecycle
// handler — the TUI removes the row surgically instead of
// clearing + rebuilding from a fresh full-list snapshot. DECISIONS #21.
//
// No-ops on unknown IDs (a SetJobs snapshot may have already
// excluded the row, or the user may delete via a TUI path that
// doesn't go through the database event chain).
func (m *TaskListModel) RemoveJob(jobID string) {
	idx, ok := m.jobIndex[jobID]
	if !ok || idx >= len(m.jobs) {
		return
	}
	// Remember the previously-selected job (if any) so we can follow
	// it after the rebuild — a removal that ISN'T the selected row
	// should keep the selection on the same job.
	var prevSelectedID string
	if sel := m.SelectedJob(); sel != nil {
		prevSelectedID = sel.ID
	}

	m.jobs = append(m.jobs[:idx], m.jobs[idx+1:]...)
	m.rebuildJobIndex()
	m.rebuildVirtualList()

	if prevSelectedID != "" && prevSelectedID != jobID {
		for i, item := range m.list.Items() {
			if ti, ok := item.(taskItem); ok && ti.job != nil && ti.job.ID == prevSelectedID {
				m.list.Select(i)
				break
			}
		}
	}
	m.resetMarquee()
}

// UpdateJob replaces a single job by ID using the index map (O(1) lookup).
// Returns true if the job was found and replaced.
func (m *TaskListModel) UpdateJob(job *database.Job) bool {
	if idx, ok := m.jobIndex[job.ID]; ok && idx < len(m.jobs) {
		// Remember which job was selected so we can follow it after re-sort.
		var prevSelectedID string
		if sel := m.SelectedJob(); sel != nil {
			prevSelectedID = sel.ID
		}

		m.jobs[idx] = job
		m.rebuildVirtualList()

		// Follow the previously selected job to its new position.
		if prevSelectedID != "" {
			for i, item := range m.list.Items() {
				if ti, ok := item.(taskItem); ok && ti.job != nil && ti.job.ID == prevSelectedID {
					m.list.Select(i)
					break
				}
			}
		}
		m.resetMarquee()
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

// GetJobByID returns a job by ID, or nil if not found.
func (m *TaskListModel) GetJobByID(id string) *database.Job {
	if idx, ok := m.jobIndex[id]; ok && idx < len(m.jobs) {
		return m.jobs[idx]
	}
	return nil
}

// SelectedJob returns the currently selected job, or nil.
func (m *TaskListModel) SelectedJob() *database.Job {
	sel := m.list.SelectedItem()
	if sel == nil {
		return nil
	}
	ti, ok := sel.(taskItem)
	if !ok || ti.divider {
		return nil
	}
	return ti.job
}

// SelectedIsDivider returns true if the selected item is the archive divider.
func (m *TaskListModel) SelectedIsDivider() bool {
	sel := m.list.SelectedItem()
	if sel == nil {
		return false
	}
	ti, ok := sel.(taskItem)
	return ok && ti.divider
}

// ToggleSelection toggles the batch selection state for a job ID.
func (m *TaskListModel) ToggleSelection(jobID string) {
	if m.selected[jobID] {
		delete(m.selected, jobID)
	} else {
		m.selected[jobID] = true
	}
}

// ClearSelection clears all batch selections.
func (m *TaskListModel) ClearSelection() {
	m.selected = make(map[string]bool)
}

// SelectedCount returns the number of batch-selected jobs.
func (m *TaskListModel) SelectedCount() int {
	return len(m.selected)
}

// SelectedIDs returns the IDs of all batch-selected jobs.
func (m *TaskListModel) SelectedIDs() []string {
	ids := make([]string, 0, len(m.selected))
	for id := range m.selected {
		ids = append(ids, id)
	}
	return ids
}

// SetSize updates the panel dimensions.
func (m *TaskListModel) SetSize(w, h int) {
	prevW := m.width
	m.width = w
	m.height = h
	contentW := max(w-2, 1)
	contentH := m.contentHeight()
	m.list.SetSize(contentW, contentH)
	// Recalculate marquee width when panel width changes.
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
	m.list.CursorUp()
	m.resetMarquee()
}

// MoveDown moves the selection down.
func (m *TaskListModel) MoveDown() {
	m.list.CursorDown()
	m.resetMarquee()
}

// SelectAtOffset selects the item at the given Y offset within the visible page.
// Returns true if a valid item was selected.
func (m *TaskListModel) SelectAtOffset(y int) bool {
	perPage := m.list.Paginator.PerPage
	if perPage <= 0 {
		return false
	}
	globalIdx := m.list.Paginator.Page*perPage + y
	if globalIdx < 0 || globalIdx >= len(m.list.Items()) {
		return false
	}
	m.list.Select(globalIdx)
	m.resetMarquee()
	return true
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
	contentW := max(m.width-2, 1)
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
		progressTextWidth = runewidth.StringWidth(fmt.Sprintf("%.0f%% ", percent))
	}
	tw := max(contentW-selectorWidth-iconWidth-progressTextWidth-platformTagWidth, 5)
	return tw
}

// CycleFilter cycles through filter modes.
func (m *TaskListModel) CycleFilter() {
	m.filter = m.filter.Next()
	m.rebuildVirtualList()
	// Reset selection on filter change (match TS)
	m.list.Select(0)
	m.resetMarquee()
}

// ToggleArchive toggles the archive divider expansion.
func (m *TaskListModel) ToggleArchive() {
	m.archiveExpanded = !m.archiveExpanded
	m.rebuildVirtualList()
}

func (m *TaskListModel) contentHeight() int {
	return max(m.height-3, 1)
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

// isCompletedStatus returns true for Finished or Cancelled statuses.
// This differs from Job.IsTerminal() which also includes Error — here we only
// check statuses that represent a completed lifecycle (success or user-cancelled).
func isCompletedStatus(status database.JobStatus) bool {
	return status == database.StatusFinished || status == database.StatusCancelled
}

// isJobArchived returns true if a finished job should be in the archive section
// based on the hide_finished_age_days setting.
// Only Finished jobs are archived — Cancelled jobs stay in the active list since
// they may need user attention (retry, investigate, etc.).
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

	active := make([]*database.Job, 0, len(m.jobs))
	archived := make([]*database.Job, 0, len(m.jobs)/4)
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
	slices.SortStableFunc(active, func(a, b *database.Job) int {
		pa := statusPriority(a.Status)
		pb := statusPriority(b.Status)
		if pa != pb {
			if pa < pb {
				return -1
			}
			return 1
		}
		if pa >= 6 {
			if a.UpdatedAt > b.UpdatedAt {
				return -1
			}
			if a.UpdatedAt < b.UpdatedAt {
				return 1
			}
			return 0
		}
		la := strings.ToLower(a.Title)
		lb := strings.ToLower(b.Title)
		if la < lb {
			return -1
		}
		if la > lb {
			return 1
		}
		return 0
	})

	// Sort archived: newest first
	slices.SortStableFunc(archived, func(a, b *database.Job) int {
		if a.UpdatedAt > b.UpdatedAt {
			return -1
		}
		if a.UpdatedAt < b.UpdatedAt {
			return 1
		}
		return 0
	})

	// Build list items
	items := make([]list.Item, 0, len(active)+len(archived)+1)
	for _, j := range active {
		items = append(items, taskItem{job: j})
	}

	showArchive := len(archived) > 0 && (m.filter == FilterAll || m.filter == FilterFinished)
	if showArchive {
		items = append(items, taskItem{
			divider: true,
			count:   len(archived),
		})
		if m.archiveExpanded {
			for _, j := range archived {
				items = append(items, taskItem{job: j, archived: true})
			}
		}
	}

	m.list.SetItems(items)
}

func (m *TaskListModel) passesFilter(j *database.Job) bool {
	switch m.filter {
	case FilterActive:
		return !isCompletedStatus(j.Status) && j.Status != database.StatusError && j.Status != database.StatusCookies
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
	contentW := max(m.width-2, 1)

	header := m.renderHeader(contentW)

	var listContent string
	if len(m.list.Items()) == 0 {
		if m.JustCompletedSetup {
			listContent = lipgloss.NewStyle().Foreground(lipgloss.Color("#2ecc71")).Render("Setup complete!") + "\n\n" +
				DimStyle.Render("Press ` to open Settings and add channels,") + "\n" +
				DimStyle.Render("or A A to add a video.")
		} else {
			listContent = DimStyle.Render("No tasks. Press A to add, or use Web UI.")
		}
	} else {
		listContent = m.list.View()
	}
	content := header + "\n" + listContent

	style := UnfocusedBorder
	if m.focused {
		style = FocusedBorder
	}

	return style.Width(m.width).Height(m.height).Render(content)
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
		for _, item := range m.list.Items() {
			if ti, ok := item.(taskItem); ok && !ti.divider {
				total++
			}
		}
		left += fmt.Sprintf(" (%d)", total)
	}

	if m.filter != FilterAll {
		left += " " + YellowStyle.Render("["+m.filter.String()+"]")
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

	// Scroll range display
	total := len(m.list.Items())
	contentH := m.contentHeight()
	if total > contentH {
		perPage := m.list.Paginator.PerPage
		if perPage <= 0 {
			perPage = contentH
		}
		start := m.list.Paginator.Page*perPage + 1
		end := min(start+perPage-1, total)
		scrollInfo := fmt.Sprintf("[%d-%d/%d]", start, end, total)
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

	padding := max(w-leftW-rightW, 1)

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

	totalRule := max(maxW-runewidth.StringWidth(label)-6, 2)
	ruleLeft := totalRule / 2
	ruleRight := totalRule - ruleLeft // absorbs odd-width remainder

	prefix := "> "
	if !selected {
		prefix = "  "
	}

	line := prefix + strings.Repeat("\u2500", ruleLeft) + " " + label + " " + strings.Repeat("\u2500", ruleRight)

	color := YellowBoldStyle
	if selected {
		color = color.Background(lipgloss.Color("#333399")).Foreground(ColorWhite)
	}

	// Pad line to full width BEFORE styling so the background color
	// extends to the right edge when selected.
	lineW := runewidth.StringWidth(line)
	if lineW < maxW {
		line += strings.Repeat(" ", maxW-lineW)
	}
	return color.Render(line)
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
	if showProgress {
		progressText = fmt.Sprintf("%.0f%% ", percent)
	}

	// Reuse centralized title width calculation (matches titleWidth method)
	titleWidth := m.titleWidth(job)

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
	var parts []string

	// Selector (with batch selection marker)
	batchSelected := m.selected[job.ID]
	if selected && batchSelected {
		parts = append(parts, taskSelectedBgStyle.Render("\u2713 ")) // ✓ with cursor highlight
	} else if selected {
		parts = append(parts, taskSelectedBgStyle.Render("> "))
	} else if batchSelected {
		parts = append(parts, taskBatchCheckStyle.Render("\u2713 ")) // ✓ green
	} else {
		parts = append(parts, "  ")
	}

	// Status icon (retains status color even when selected, per TS)
	iconStyle := lipgloss.NewStyle().Foreground(color)
	switch job.Status {
	case database.StatusError:
		iconStyle = iconStyle.UnderlineStyle(lipgloss.UnderlineCurly).UnderlineColor(ColorError)
	case database.StatusCookies:
		iconStyle = iconStyle.UnderlineStyle(lipgloss.UnderlineCurly).UnderlineColor(ColorCookies)
	}
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
		tagStyle := taskTwitchTagStyle
		if dimmed {
			tagStyle = tagStyle.Faint(true)
		}
		parts = append(parts, tagStyle.Render("[TW] "))
	}

	// Title (blue bg + white text when selected, status color when not selected, match TS)
	if selected {
		parts = append(parts, taskSelectedBgStyle.Render(title))
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
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
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

package tui

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/sahilm/fuzzy"

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
	// virtualIndex maps job ID → position in the bubbles/list-level items
	// slice (sorted order). Populated by rebuildVirtualList; used by
	// UpdateJob's selection-relocation code path so the post-sort
	// "follow the selected job" walk is O(1) rather than O(N). Audit
	// reports/tui.md #22.
	virtualIndex map[string]int

	// archivedSet records, for every job displayed by the last
	// rebuildVirtualList, whether it landed in the archive bucket. Lets
	// ResweepArchive detect a Finished job aging across the
	// hide_finished_age_days boundary without rebuilding (and without
	// resetting the marquee) when nothing changed.
	archivedSet map[string]bool
	list        list.Model

	width, height       int
	focused             bool
	filter              Filter
	archiveExpanded     bool
	hideFinishedAgeDays int // from config, default 30

	// Batch selection state (Space to toggle, mirrors Web UI batch operations).
	selected map[string]bool // selected job IDs for batch operations

	// Live fuzzy search ("/" in the Tasks panel). searching is true while the
	// input box is open; searchQuery is the applied filter (kept even after
	// the box closes, so a search stays active until explicitly cleared with
	// Esc, mirroring the log panel's search). Empty query = no filtering.
	searching   bool
	searchInput textinput.Model
	searchQuery string

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
	ti := newTextInput()
	ti.Prompt = "/"
	ti.Placeholder = "search titles, channels…"
	ti.CharLimit = 200
	m := &TaskListModel{
		hideFinishedAgeDays: 30,
		progressStore:       NewProgressStore(),
		selected:            make(map[string]bool),
		searchInput:         ti,
	}
	m.list = m.newTaskList()
	return m
}

// IsSearching reports whether the search input box is open (capturing keys).
func (m *TaskListModel) IsSearching() bool { return m.searching }

// StartSearch opens the search input, seeded with any active query so the
// user can refine rather than retype. Returns the input's focus cmd.
func (m *TaskListModel) StartSearch() tea.Cmd {
	m.searching = true
	m.searchInput.SetWidth(max(m.width-4, 8))
	m.searchInput.SetValue(m.searchQuery)
	m.searchInput.CursorEnd()
	m.applyListSize()
	return m.searchInput.Focus()
}

// HandleSearchKey intercepts a key while the box is open. Returns (cmd,
// consumed); consumed=false when the box isn't open so the caller falls
// through to normal handling. Enter applies + closes the box (keeping the
// query live); Esc closes and clears the query entirely. Typing keys are
// consumed here (to keep them off the chord system) but the textinput
// itself is updated in UpdateSearchInput via routeComponentMsg — mirrors
// the log panel's split so the key isn't double-processed. Ctrl+C passes
// through for quit.
func (m *TaskListModel) HandleSearchKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.searching {
		return nil, false
	}
	switch msg.String() {
	case keyCtrlC:
		return nil, false
	case "enter":
		m.searchQuery = strings.TrimSpace(m.searchInput.Value())
		m.searching = false
		m.searchInput.Blur()
		m.applyListSize()
		m.refilterSelectTop()
		return nil, true
	case "esc":
		m.searching = false
		m.searchInput.Blur()
		m.applyListSize()
		if m.searchQuery != "" {
			m.searchQuery = ""
			m.refilterSelectTop()
		}
		return nil, true
	}
	// Consumed; the textinput is fed by UpdateSearchInput.
	return nil, true
}

// refilterSelectTop rebuilds the visible list, jumps the cursor to the top,
// and re-anchors the marquee to the new selection. Shared by the search
// paths so the highlighted row and the scrolling title never disagree after
// the visible set changes.
func (m *TaskListModel) refilterSelectTop() {
	m.rebuildVirtualList()
	m.list.Select(0)
	m.resetMarquee()
}

// UpdateSearchInput feeds a message to the search textinput and re-filters
// live as the query changes. Called from routeComponentMsg on every message
// while the box is open (keys for typing, cursor-blink ticks). No-op when
// the box is closed.
func (m *TaskListModel) UpdateSearchInput(msg tea.Msg) tea.Cmd {
	if !m.searching {
		return nil
	}
	prev := m.searchInput.Value()
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	if m.searchInput.Value() != prev {
		m.searchQuery = strings.TrimSpace(m.searchInput.Value())
		m.refilterSelectTop()
	}
	return cmd
}

// ClearSearch drops any active query and closes the box. Called on Esc from
// the app when nothing else claims it.
func (m *TaskListModel) ClearSearch() bool {
	if !m.searching && m.searchQuery == "" {
		return false
	}
	m.searching = false
	m.searchInput.Blur()
	m.searchInput.SetValue("")
	hadQuery := m.searchQuery != ""
	m.searchQuery = ""
	m.applyListSize()
	m.refilterSelectTop()
	return hadQuery
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
}

// captureSelection returns the currently-selected job's ID. Called at the
// top of rebuildVirtualList, before SetItems replaces the rows.
func (m *TaskListModel) captureSelection() string {
	if sel := m.SelectedJob(); sel != nil {
		return sel.ID
	}
	return ""
}

// restoreSelection re-selects prevID at its post-rebuild position via the
// virtualIndex map (O(1) — audit reports/tui.md #22) and resets the
// marquee. Called from the tail of rebuildVirtualList so EVERY rebuild
// preserves selection by job ID structurally: the bubbles list keeps the
// NUMERIC index across SetItems, so without relocation the highlight
// silently lands on a different job whenever a rebuild re-sorts, filters,
// or re-archives the rows. IDs absent from the rebuilt list (removed job,
// row hidden by a filter or a collapsed archive) are left to the list's
// natural index clamping — the neighbor row inherits the highlight.
// Callers that want a different post-rebuild selection (CycleFilter's
// jump-to-top) Select() after the rebuild returns, which wins.
func (m *TaskListModel) restoreSelection(prevID string) {
	if prevID != "" {
		if newIdx, ok := m.virtualIndex[prevID]; ok {
			m.list.Select(newIdx)
		}
	}
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
	m.jobs = append(m.jobs[:idx], m.jobs[idx+1:]...)
	m.rebuildJobIndex()
	// Removing the selected row itself: its ID is absent from the rebuilt
	// virtualIndex, so the rebuild's restore no-ops and the list's natural
	// neighbor selection stands.
	m.rebuildVirtualList()
}

// UpdateJob replaces a single job by ID using the index map (O(1) lookup).
// Returns true if the job was found and replaced.
//
// Audit reports/tui.md #22 — the previous post-rebuild relocation walked
// m.list.Items() linearly to follow the selected job's new sorted
// position. With 100+ active jobs and 60fps progress ticks that O(N)
// scan was measurable. virtualIndex (built inside rebuildVirtualList)
// turns the relocation into an O(1) map lookup.
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
	m.applyListSize()
	if m.searching {
		m.searchInput.SetWidth(max(w-4, 8))
	}
	// Recalculate marquee width when panel width changes.
	if prevW != w {
		m.resetMarquee()
	}
}

// applyListSize sizes the embedded list, reserving a row for the search box
// while it's open so the list can't overrun the panel border. Called on
// resize and whenever the search box opens/closes.
func (m *TaskListModel) applyListSize() {
	contentW := max(m.width-2, 1)
	contentH := m.contentHeight()
	if m.searching {
		contentH = max(contentH-1, 1)
	}
	m.list.SetSize(contentW, contentH)
}

// SetFocused sets the focus state. Losing focus closes an open search input
// (a click on another panel changes focus without routing through
// HandleSearchKey, which would otherwise strand the box open and dead on the
// unfocused panel) — the applied query survives as a filter, mirroring the
// log viewer's focus-out behavior. Re-focusing + "/" reopens it seeded with
// the query.
func (m *TaskListModel) SetFocused(f bool) {
	m.focused = f
	if !f && m.searching {
		m.searching = false
		m.searchInput.Blur()
		m.applyListSize()
	}
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

// progressCellText returns the text for a row's progress slot — the live
// percent for an actively-delivering download, or a dim wait tag while the
// downloader is in a wait state (verifying end, reconnecting, retrying):
// the percent would otherwise freeze stale during the wait and read as
// hung. The full wait reason always shows in the details panel. isWait
// tells renderJob to style the tag dim rather than in status color. Both
// renderJob and titleWidth derive from this helper so the row layout and
// the rendered cells can never disagree.
func (m *TaskListModel) progressCellText(job *database.Job) (text string, isWait bool) {
	if job.Status != database.StatusDownloading && job.Status != database.StatusMuxing {
		return "", false
	}
	percent := job.Percent
	progress := job.Progress
	if p := m.progressStore.Get(job.ID); p != nil {
		percent = p.Percent
		progress = p.Progress
	}
	if isWaitProgress(progress) {
		return "⋯ wait ", true
	}
	if percent > 0 {
		return fmt.Sprintf("%.0f%% ", percent), false
	}
	return "", false
}

// isWaitProgress reports whether a progress string is a downloader wait
// message rather than a segment counter. Every activity message the worker
// renders (worker.activityMessage) carries the "... (<elapsed>)" tail and no
// counter format does — the contract is pinned on the worker side by
// TestActivityMessagesCarryWaitMarker.
func isWaitProgress(s string) bool {
	return strings.Contains(s, "... (")
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
	progressText, _ := m.progressCellText(job)
	progressTextWidth := runewidth.StringWidth(progressText)
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
	case database.StatusQueued:
		return 6
	case database.StatusCancelled:
		return 7
	case database.StatusFinished:
		return 8
	default:
		return 9
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

// archiveBucketsDirty reports whether any displayed job's archive
// classification differs from the buckets built by the last
// rebuildVirtualList — i.e. a Finished job aged across the
// hide_finished_age_days boundary while the list sat idle. Applies the
// same filter/search gates as the rebuild so hidden jobs can't trigger it.
// One classification pass, no sort — cheap enough for a periodic sweep.
func (m *TaskListModel) archiveBucketsDirty() bool {
	// Only ageDays > 0 has time-driven crossings: 0 archives on the status
	// change itself (which already rebuilds) and <0 never archives.
	if m.hideFinishedAgeDays <= 0 {
		return false
	}
	now := time.Now()
	for _, j := range m.jobs {
		if !m.passesFilter(j) || !m.passesSearch(j) {
			continue
		}
		if isJobArchived(j, m.hideFinishedAgeDays, now) != m.archivedSet[j.ID] {
			return true
		}
	}
	return false
}

// ResweepArchive re-buckets the rows when a Finished job has aged across the
// archive boundary since the last rebuild (the TUI analog of the web UI's
// 60s archive-boundary sweep). Returns true when a rebuild ran. The rebuild
// (with its selection restore + marquee reset) only happens when a job
// actually crossed — the common every-sweep outcome is the cheap dirty check.
func (m *TaskListModel) ResweepArchive() bool {
	if !m.archiveBucketsDirty() {
		return false
	}
	m.rebuildVirtualList()
	return true
}

func (m *TaskListModel) rebuildVirtualList() {
	prevSelectedID := m.captureSelection()

	now := time.Now()
	ageDays := m.hideFinishedAgeDays

	active := make([]*database.Job, 0, len(m.jobs))
	archived := make([]*database.Job, 0, len(m.jobs)/4)
	m.archivedSet = make(map[string]bool, len(m.jobs))
	for _, j := range m.jobs {
		if !m.passesFilter(j) {
			continue
		}
		if !m.passesSearch(j) {
			continue
		}

		if isJobArchived(j, ageDays, now) {
			m.archivedSet[j.ID] = true
			archived = append(archived, j)
			continue
		}
		m.archivedSet[j.ID] = false
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
		// Completed statuses (Cancelled/Finished) sort newest-first; everything
		// else — including the Queued resting state — sorts by title below.
		if pa >= 7 {
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

	// Build list items + virtualIndex side-table for O(1) post-sort
	// relocation in UpdateJob (audit reports/tui.md #22).
	items := make([]list.Item, 0, len(active)+len(archived)+1)
	m.virtualIndex = make(map[string]int, len(active)+len(archived))
	for _, j := range active {
		m.virtualIndex[j.ID] = len(items)
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
				m.virtualIndex[j.ID] = len(items)
				items = append(items, taskItem{job: j, archived: true})
			}
		}
	}

	m.list.SetItems(items)
	m.restoreSelection(prevSelectedID)
}

// passesSearch reports whether a job matches the active fuzzy query. The
// query fuzzy-matches against title, channel name, and video ID (the same
// fields the Web UI's bare-text search covers); an empty query matches
// everything. Matching is case-insensitive subsequence via sahilm/fuzzy, so
// "mchi" finds "Minecraft with Chika".
func (m *TaskListModel) passesSearch(j *database.Job) bool {
	if m.searchQuery == "" {
		return true
	}
	// Space-joined so a query can span fields ("mumei minecraft"); a
	// printable separator also avoids sahilm/fuzzy's NUL-byte indexing bug.
	hay := j.Title + " " + j.ChannelName + " " + j.VideoID
	return len(fuzzy.Find(m.searchQuery, []string{hay})) > 0
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
		switch {
		case m.searchQuery != "":
			listContent = DimStyle.Render(fmt.Sprintf("No tasks match /%s.", truncateString(m.searchQuery, 30)))
		case m.JustCompletedSetup:
			listContent = lipgloss.NewStyle().Foreground(lipgloss.Color("#2ecc71")).Render("Setup complete!") + "\n\n" +
				DimStyle.Render("Press ` to open Settings and add channels,") + "\n" +
				DimStyle.Render("or A A to add a video.")
		default:
			listContent = DimStyle.Render("No tasks. Press A to add, or use Web UI.")
		}
	} else {
		listContent = m.list.View()
	}
	content := header + "\n" + listContent
	// Search box occupies the row reserved by applyListSize while open.
	if m.searching {
		content += "\n" + m.searchInput.View()
	}

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
	// Active-search indicator (when the box is closed but a query is applied).
	if !m.searching && m.searchQuery != "" {
		left += " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#aaaa00")).Render("[/"+truncateString(m.searchQuery, 20)+"]")
	}

	// Countdown timers (T3 - match TS format, colored dots before labels)
	greenDot := lipgloss.NewStyle().Foreground(ColorGreen).Render("\u25CF")
	grayDot := DimStyle.Render("\u25CF")
	// renderTimer formats one monitor countdown, showing "…" with a live
	// (green) dot while a check is actually running (checking sentinel).
	renderTimer := func(label string, next time.Time) (string, bool) {
		if next.IsZero() {
			return "", false
		}
		if isMonitorChecking(next) {
			return greenDot + DimStyle.Render(label+"…"), true
		}
		d := time.Until(next)
		dot := grayDot
		if d <= 0 {
			dot = greenDot
		}
		return dot + DimStyle.Render(label+formatCountdown(d)), true
	}
	var timers []string
	if s, ok := renderTimer("F:", m.NextFeedCheck); ok {
		timers = append(timers, s)
	}
	if s, ok := renderTimer("D:", m.NextDecapiCheck); ok {
		timers = append(timers, s)
	}
	if s, ok := renderTimer("T:", m.NextTwitchCheck); ok {
		timers = append(timers, s)
	}

	timerStr := strings.Join(timers, " ")

	// Scroll range display
	var scrollFull, scrollShort string
	total := len(m.list.Items())
	contentH := m.contentHeight()
	if total > contentH {
		perPage := m.list.Paginator.PerPage
		if perPage <= 0 {
			perPage = contentH
		}
		start := m.list.Paginator.Page*perPage + 1
		end := min(start+perPage-1, total)
		scrollFull = fmt.Sprintf("[%d-%d/%d]", start, end, total)
		scrollShort = fmt.Sprintf("[%d/%d]", end, total)
	}

	// Right-hand content, richest first. The old rule dropped ALL of it the
	// moment it didn't fit, taking the scroll position with it — but that
	// range is navigational (it says where you are in a list you are
	// actively paging through), while the monitor countdowns are
	// informational and repeated in the settings view. So the timers go
	// first, then the range abbreviates, and only then does the side empty.
	rightTiers := []string{
		strings.TrimSpace(timerStr + " " + scrollFull),
		scrollFull,
		scrollShort,
		"",
	}

	leftW := lipgloss.Width(left)
	right := ""
	for _, cand := range rightTiers {
		if leftW+1+lipgloss.Width(cand) <= w {
			right = cand
			break
		}
	}
	padding := max(w-leftW-lipgloss.Width(right), 1)

	header := TitleStyle.Render(left) + strings.Repeat(" ", padding) + DimStyle.Render(right)
	// Hard clamp: the left side alone can exceed w (a long filter or search
	// indicator on a narrow window), and the pre-existing comment here was
	// right about the consequence — a wrapped header adds a line and shifts
	// the whole panel via Height truncation. Dropping the right side never
	// prevented that, since nothing bounded the left. MaxWidth is
	// ANSI-aware, so styled runs are cut without severing escape sequences.
	return lipgloss.NewStyle().MaxWidth(w).Render(header)
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
		database.StatusQueued,
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

	// Only show progress for Downloading/Muxing (match TS isActive check).
	// A downloader wait state renders as a dim "⋯ wait" tag instead of the
	// stale frozen percent — see progressCellText.
	progressText, progressIsWait := m.progressCellText(job)
	showProgress := progressText != ""

	// Reuse centralized title width calculation (matches titleWidth method)
	titleWidth := m.titleWidth(job)

	// Prepare title
	title := job.Title
	if title == "" {
		title = job.VideoID
	}
	if selected && m.marquee.NeedsScroll() {
		// The marquee window was sized at resetMarquee time; the percent
		// text can have grown since (9→10%, 99→100%), shrinking titleWidth.
		// Truncate to the current width or the row wraps and shifts layout.
		title = truncateString(m.marquee.View(), titleWidth)
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
	switch {
	case job.Status == database.StatusError:
		iconStyle = iconStyle.UnderlineStyle(lipgloss.UnderlineCurly).UnderlineColor(ColorError)
	case job.Status == database.StatusCookies:
		iconStyle = iconStyle.UnderlineStyle(lipgloss.UnderlineCurly).UnderlineColor(ColorCookies)
	case job.Status == database.StatusFinished && job.IncompleteTail:
		iconStyle = iconStyle.UnderlineStyle(lipgloss.UnderlineCurly).UnderlineColor(ColorWarning)
	}
	if dimmed {
		iconStyle = iconStyle.Faint(true)
	}
	parts = append(parts, iconStyle.Render(icon)+" ")

	// Progress (before platform tag, matching TS order; uses status color not always green)
	if showProgress {
		pctStyle := lipgloss.NewStyle().Foreground(color)
		if progressIsWait {
			pctStyle = DimStyle // a wait, not delivery — mute it
		}
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

// truncateString clips s to maxW display columns, appending an ellipsis
// when it had to cut. Delegates to x/ansi -- the same helper the Charm
// stack uses internally -- rather than hand-rolling the width walk.
//
// Verified equivalent to the previous implementation across ASCII, wide
// (CJK) runes, pre-existing ellipses and every width from 1 up, with one
// deliberate difference at maxW <= 0: the old code returned an ellipsis
// there, emitting a column of output into a space with no columns to give.
// Empty is the correct answer, and callers do pass non-positive widths on
// narrow terminals (action_menu.go's contentW-17, for one).
//
// The ANSI-awareness is insurance rather than a fix: every current caller
// styles AFTER truncating, so none can sever an escape sequence today --
// but a future caller passing styled text now gets a correct line instead
// of a corrupted one.
func truncateString(s string, maxW int) string {
	return ansi.Truncate(s, maxW, "…")
}

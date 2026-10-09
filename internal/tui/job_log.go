package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// jobLogRefreshInterval is how often the O L overlay re-reads its job's log
// while it is open. The buffer is filled by the database's log router, which
// runs on its own logger subscription, so no event says "this job logged"
// in order with the line itself — a short poll of an in-memory copy of at
// most 200 lines is the honest way to follow it, and it costs nothing when
// nothing moved (SyncLines leaves the display alone on an identical read).
const jobLogRefreshInterval = time.Second

// jobLogTitlePrefix heads the overlay, ahead of the job's title.
const jobLogTitlePrefix = "Job Log — "

// JobLogModel is the O L overlay: the selected job's OWN log lines, the
// terminal's half of the "Job Logs" section of the dashboard's job dialog.
// It reads the same per-job buffer that section reads — the database's
// (db.GetJobLogs, which GET /api/jobs/{id}/logs serves) — through
// App.OnGetJobLogs, and keeps no copy of its own beyond what is on screen.
//
// The body is a LogViewerModel, the log panel's own viewer, so it scrolls,
// pages, searches (/, n/N) and colours levels exactly as the log panel does,
// and a paused view stays on its lines while the job's ring evicts older
// ones (SyncLines, W24-12). It is always "focused": the pause hint and
// [PAUSED] show whenever the reader has scrolled up.
type JobLogModel struct {
	visible       bool
	width, height int
	jobID         string
	// loaded is false until the first read lands; until then the body says
	// "Loading logs...", as the dashboard's section does.
	loaded bool
	log    *LogViewerModel
}

// NewJobLogModel creates the job log overlay.
func NewJobLogModel() *JobLogModel {
	return &JobLogModel{log: NewLogViewerModel()}
}

// IsVisible returns true if the overlay is shown.
func (m *JobLogModel) IsVisible() bool { return m.visible }

// JobID is the job the overlay is showing, "" when closed.
func (m *JobLogModel) JobID() string { return m.jobID }

// Open shows job's log, starting from an empty viewer — no lines, no search,
// following the tail — that says it is loading until SetLines first lands.
func (m *JobLogModel) Open(job *database.Job) {
	m.visible = true
	m.loaded = false
	m.jobID = job.ID
	title := job.Title
	if title == "" {
		title = job.ID
	}
	m.log = NewLogViewerModel()
	m.log.title = jobLogTitlePrefix + title
	m.log.emptyText = "Loading logs..."
	m.log.SetFocused(true)
	m.SetSize(m.width, m.height)
}

// Close hides the overlay.
func (m *JobLogModel) Close() {
	m.visible = false
	m.jobID = ""
}

// SetSize gives the viewer the whole terminal but the footer's row.
func (m *JobLogModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.log.SetSize(w, max(h-1, minPanelH))
}

// SetLines applies a fresh read of the job's log buffer.
func (m *JobLogModel) SetLines(lines []string) {
	first := !m.loaded
	if first {
		m.loaded = true
		// The dashboard section's own words for an empty buffer.
		m.log.emptyText = "No logs for this job yet."
	}
	// An empty first read changes no line, but the body must stop saying
	// "Loading logs...".
	if !m.log.SyncLines(lines) && first {
		m.log.redisplay(0)
	}
}

// UpdateComponents feeds the search box while it is open and the viewport
// otherwise (↑/↓, PgUp/PgDn, Ctrl+U/Ctrl+D) — the log panel's own split in
// routeComponentMsg.
func (m *JobLogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if m.log.IsSearching() {
		return m.log.UpdateSearchInput(msg)
	}
	return m.log.UpdateViewport(msg)
}

// HandleKey processes one keypress after UpdateComponents has seen it, and
// returns the command the search box asked for and "close" when the overlay
// closed. The search keys go first, as on the log panel: while the box is
// open every key is the query's, and with a query applied Esc clears it
// before a second Esc closes the overlay. q closes either way.
func (m *JobLogModel) HandleKey(msg tea.KeyPressMsg) (tea.Cmd, string) {
	if cmd, consumed := m.log.HandleSearchKey(msg); consumed {
		return cmd, ""
	}
	switch msg.String() {
	case "/":
		return m.log.StartSearch(), ""
	case keyEsc, "q", "Q":
		m.Close()
		return nil, "close"
	case keyEnd:
		m.log.ReEnableAutoScroll()
	}
	return nil, ""
}

// Scroll moves the view by n rows (negative is up) — the mouse wheel's path,
// three rows a notch like the panels'.
func (m *JobLogModel) Scroll(n int) {
	for ; n < 0; n++ {
		m.log.ScrollUp()
	}
	for ; n > 0; n-- {
		m.log.ScrollDown()
	}
}

// footerHint is the key line under the viewer, for the state it is in, in
// the longest spelling that fits.
func (m *JobLogModel) footerHint() string {
	var candidates []string
	switch {
	case m.log.IsSearching():
		candidates = []string{"Enter: Find  Esc: Cancel", "Enter find · Esc cancel", "Esc"}
	case m.log.searchQuery != "":
		candidates = []string{
			"↑/↓ PgUp/PgDn: Scroll  n/N: Next/Prev match  End: Follow  Esc: Clear find  Q: Close",
			"↑/↓ PgUp/PgDn · n/N match · End follow · Esc clear · Q close",
			"n/N · Esc clear · Q close",
			"Q close",
		}
	default:
		candidates = []string{
			"↑/↓ PgUp/PgDn: Scroll  /: Find  End: Follow  Esc/Q: Close",
			"↑/↓ PgUp/PgDn · / find · End follow · Esc close",
			"/ find · Esc close",
			"Esc",
		}
	}
	hint := candidates[len(candidates)-1]
	for _, c := range candidates {
		if lipgloss.Width(c)+1 <= m.width { // +1 for the leading space
			hint = c
			break
		}
	}
	return DimStyle.Render(" " + hint)
}

// View renders the overlay: the viewer over the whole terminal, the key line
// under it.
func (m *JobLogModel) View() string {
	if !m.visible {
		return ""
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.log.View(), m.footerHint())
}

package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The compact task list (owner rulings R-C4, R-C7) is the same model drawn a
// second way: the same jobs, cursor, batch selection, filter and embedded
// list, with no border and no header, and each job on one line:
//
//	DOWN Some stream title that is lo…  42%
//	LIVE Another channel live now     1:12:05
//	WAIT Scheduled premiere tomorrow    in 3h
//	DONE An archive from yesterday          •
//
// a four-letter status tag in the status colour, the title cut with an
// ellipsis, and a short figure at the right edge.

// compactTagWidth is the width of a row's status tag, and compactTagGap the
// one cell after it: a space, or the batch selection's ✓.
const (
	compactTagWidth = 4
	compactTagGap   = 1
)

// compactStatusTag is the status tag of a compact row, at most
// compactTagWidth letters. Every status has one; a status this build does not
// know shows its first four letters in capitals.
func compactStatusTag(s database.JobStatus) string {
	switch s {
	case database.StatusDownloading:
		return "DOWN"
	case database.StatusLive:
		return "LIVE"
	case database.StatusUpcoming:
		return "WAIT"
	case database.StatusMuxing:
		return "MUX"
	case database.StatusError:
		return "ERR"
	case database.StatusFinished:
		return "DONE"
	case database.StatusQueued:
		return "QUE"
	case database.StatusCancelled:
		return "CANC"
	case database.StatusCookies:
		return "AUTH" // StatusLabel: "Auth Required"
	}
	return strings.ToUpper(truncateWidth(string(s), compactTagWidth, ""))
}

// setCompact switches the panel between its two drawings. It is a layout
// parameter like the size, pushed by App.recalcLayout from App.layoutMode and
// never decided here.
func (m *TaskListModel) setCompact(compact bool) {
	if m.compact == compact {
		return
	}
	m.compact = compact
	m.applyListSize()
	m.resetMarquee()
	m.invalidate()
}

// compactFigure is the short figure at the right edge of a compact row and
// whether it is dim. For a Downloading, Muxing, Queued, Error or COOKIES? row
// it is the normal row's progress cell (progressCell): the percent, the wait
// tag while the downloader waits, the hold tag, the pending mux or retry, the
// cookie retries used; a capture
// with no percent shows its elapsed time. Its own are the elapsed time while
// live, the countdown while waiting, and the watched dot of a Finished row.
// "" when the row has none.
func (m *TaskListModel) compactFigure(job *database.Job, now time.Time) (text string, dim bool) {
	switch job.Status {
	case database.StatusDownloading, database.StatusMuxing, database.StatusQueued, database.StatusError, database.StatusCookies:
		// The normal row's progress cell (progressCell owns the rule),
		// without its trailing space.
		cell, dim := m.progressCell(job, now)
		cell = strings.TrimSpace(cell)
		switch {
		case cell != "" && !dim:
			// A percent: always four cells, so the title does not shift
			// at 10 and 100.
			return fmt.Sprintf("%4s", cell), false
		case cell != "":
			return cell, true
		case job.Status == database.StatusDownloading || job.Status == database.StatusMuxing:
			return compactElapsed(job, now), false
		}
	case database.StatusLive:
		return compactElapsed(job, now), false
	case database.StatusUpcoming:
		if start := parseJobTime(job.StreamStartTime); start.After(now) {
			return "in " + compactSpan(start.Sub(now)), false
		}
	case database.StatusFinished:
		if job.Watched {
			return watchedGlyph, true
		}
	}
	return "", false
}

// parseJobTime reads one of a job's ISO 8601 timestamps; the zero time when
// it is empty or unreadable.
func parseJobTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// compactElapsed is how long a capture has run, from the stream's start or,
// without one, the job's creation: the Duration field of the details panel.
func compactElapsed(job *database.Job, now time.Time) string {
	start := parseJobTime(job.StreamStartTime)
	if start.IsZero() {
		start = parseJobTime(job.CreatedAt)
	}
	if start.IsZero() || start.After(now) {
		return ""
	}
	d := now.Sub(start)
	h, mnt, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mnt, s)
	}
	return fmt.Sprintf("%d:%02d", mnt, s)
}

// compactSpan is a countdown in its one largest unit: 2d, 3h, 12m, 45s.
func compactSpan(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", max(int(d.Seconds()), 1))
}

// renderCompactJob draws one job as one line exactly w cells wide: the tag,
// one cell, the title, and the figure at the right edge. The cursor's row has
// the normal list's highlight on everything after the tag, which keeps its
// status colour; a batch-selected row has the ✓ in the cell after the tag.
func (m *TaskListModel) renderCompactJob(job *database.Job, selected, archived bool, w int, now time.Time) string {
	dimmed := archived && !selected

	tag := compactStatusTag(job.Status)
	tagPad := strings.Repeat(" ", max(compactTagWidth-ansi.StringWidth(tag), 0))

	figure, figureDim := m.compactFigure(job, now)
	room := max(w-compactTagWidth-compactTagGap, 0)
	figW := ansi.StringWidth(figure)
	// The figure and the cell before it are given up before the title is
	// squeezed under five cells.
	if figW > 0 && room-figW-1 < 5 {
		figure, figW = "", 0
	}
	titleW := room
	if figW > 0 {
		titleW = room - figW - 1
	}

	title := job.Title
	if title == "" {
		title = job.VideoID
	}
	// A tab measures as no cells and is drawn as several, and any other
	// control character has no business on the row: each is one space, so
	// the title is measured as it is drawn.
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, title)
	title = truncateString(title, titleW)
	title += strings.Repeat(" ", max(titleW-ansi.StringWidth(title), 0))

	// The padding is written outside the tag's style: a status style that
	// underlines (Error) would underline the blank cell too.
	var sb strings.Builder
	sb.WriteString(statusMarkStyle(job, dimmed).Render(tag))
	sb.WriteString(tagPad)

	batch := m.selected[job.ID]
	gap := " "
	if batch {
		gap = "✓"
	}
	if selected {
		rest := gap + title
		if figW > 0 {
			rest += " " + figure
		}
		sb.WriteString(taskSelectedBgStyle.Render(rest))
		return sb.String()
	}

	if batch {
		sb.WriteString(taskBatchCheckStyle.Render(gap))
	} else {
		sb.WriteString(gap)
	}
	titleStyle := lipgloss.NewStyle().Foreground(StatusColor(string(job.Status)))
	if dimmed {
		titleStyle = lipgloss.NewStyle().Faint(true)
	}
	sb.WriteString(titleStyle.Render(title))
	if figW > 0 {
		figStyle := lipgloss.NewStyle().Foreground(StatusColor(string(job.Status)))
		if figureDim {
			figStyle = DimStyle
		}
		if dimmed {
			figStyle = figStyle.Faint(true)
		}
		sb.WriteString(" " + figStyle.Render(figure))
	}
	return sb.String()
}

// compactView draws the panel in its compact form: exactly m.height rows of
// exactly m.width cells, the list (or the empty-list line) and, while the "/"
// box is open, the box on the last row.
func (m *TaskListModel) compactView() string {
	rows := m.height
	if m.searching {
		rows = max(rows-1, 1)
	}
	var body string
	if len(m.list.Items()) == 0 {
		msg := "No tasks."
		if len(m.tokens) > 0 {
			msg = "No tasks match [" + truncateString(m.Query(), max(m.width-18, 8)) + "]."
		}
		body = DimStyle.Render(truncateString(msg, m.width))
	} else {
		body = m.list.View()
	}
	out := fitBlock(body, m.width, rows)
	if m.searching {
		out += "\n" + fitBlock(m.searchInput.View(), m.width, 1)
	}
	return out
}

// fitBlock returns s as exactly h lines of exactly w cells: lines past h are
// dropped and missing ones added, a line wider than w is cut and a narrower
// one padded with spaces.
func fitBlock(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	var sb strings.Builder
	for i := range max(h, 0) {
		if i > 0 {
			sb.WriteByte('\n')
		}
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		lw := ansi.StringWidth(line)
		if lw > w {
			line = truncateWidth(line, w, "")
			lw = ansi.StringWidth(line)
		}
		sb.WriteString(line)
		writeSpaces(&sb, w-lw)
	}
	return sb.String()
}

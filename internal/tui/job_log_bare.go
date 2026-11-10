package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// The Job Log's bare form, drawn below the normal layout (overlay_bare.go):
// a header row, the log lines at the full width with no border, and a last
// row that is the key line or, while a search is being typed, the search
// box. The lines get every row but those two in every state, so pausing or
// searching never changes the height.
//
// The normal form says "End to resume" on a row of its own while the view is
// paused; here that row is gone, so the key line names End whenever the view
// is paused.

// bareSearchTitleFloor is how short the job's title is cut in the bare
// header while a search is applied, so the query and its match count stay on
// the row at 30 columns. Without a search the floor is minHeaderTitleWidth.
const bareSearchTitleFloor = 6

// bare reports whether the overlay draws its bare form: its own size is
// below the normal layout. Derived on every call, never stored.
func (m *JobLogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareFooter is the key line of the bare form for the state the viewer is
// in, in the longest spelling that fits. Every rung of a paused view names
// End, and every rung names what Esc does now.
func (m *JobLogModel) bareFooter() string {
	paused := !m.log.autoScroll
	var candidates []string
	switch {
	case m.log.searchQuery != "" && paused:
		candidates = []string{
			"↑/↓ PgUp/PgDn · n/N match · End follow · Esc clear · Q close",
			"n/N · End follow · Esc clear · Q close",
			"n/N · End · Esc clear",
		}
	case m.log.searchQuery != "":
		candidates = []string{
			"↑/↓ PgUp/PgDn · n/N match · Esc clear · Q close",
			"n/N · Esc clear · Q close",
		}
	case paused:
		candidates = []string{
			"↑/↓ PgUp/PgDn · / find · End follow · Esc close",
			"End follow · / find · Esc close",
			"End follow · Esc close",
		}
	default:
		candidates = []string{
			"↑/↓ PgUp/PgDn · / find · Esc close",
			"/ find · Esc close",
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

// bareView draws the bare form: exactly width x height cells.
func (m *JobLogModel) bareView() string {
	last := m.bareFooter()
	if m.log.IsSearching() {
		// The box draws its "/" prompt, the text and a cursor cell. The
		// normal form sets its own width on every frame it draws the box.
		m.log.searchInput.SetWidth(max(m.width-2, 1))
		last = m.log.searchInput.View()
	}
	return m.log.View() + "\n" + fitBlock(last, m.width, 1)
}

// resizeBareViewport is resizeViewport for the bare form: every row of the
// viewer but its header, whatever the state.
func (m *LogViewerModel) resizeBareViewport() {
	m.viewport.SetHeight(max(m.height-1, 1))
	// As in resizeViewport: SetHeight leaves the offset alone.
	m.viewport.SetYOffset(m.viewport.YOffset())
}

// bareHeader is the header row of the bare form: the title without the
// "Job Log — " prefix, the line count, and the suffixes of the normal header
// in the same format. While a search is applied the title gives way down to
// bareSearchTitleFloor and the query is cut before its match count is.
func (m *LogViewerModel) bareHeader() string {
	w := max(m.width, 1)
	count := fmt.Sprintf(" (%d)", m.rawCount)
	countW := lipgloss.Width(count)

	var suffixes []string
	reserve := 0
	floor := minHeaderTitleWidth
	if !m.searching && m.searchQuery != "" {
		floor = bareSearchTitleFloor
		tail := fmt.Sprintf("] (%d matches)", m.matchCount)
		room := w - floor - countW - lipgloss.Width(" [/") - lipgloss.Width(tail)
		query := truncateString(m.searchQuery, max(min(room, 20), 1))
		suffixes = append(suffixes, lipgloss.NewStyle().Foreground(lipgloss.Color("#aaaa00")).Render(" [/"+query+tail))
	}
	if m.level != LogLevelAll {
		suffixes = append(suffixes, " "+YellowStyle.Render("["+m.level.String()+"+]"))
	}
	if !m.autoScroll {
		suffixes = append(suffixes, " "+YellowStyle.Render("[PAUSED]"))
	}
	for _, sfx := range suffixes {
		reserve += lipgloss.Width(sfx)
	}
	if len(m.filtered) > m.viewport.Height() {
		pct := int(m.viewport.ScrollPercent() * 100)
		suffixes = append(suffixes, " "+DimStyle.Render(fmt.Sprintf("[%d%%]", pct)))
		reserve += lipgloss.Width(" [100%]")
	}

	title := strings.TrimPrefix(m.title, jobLogTitlePrefix)
	if title == "" {
		title = "Logs"
	}
	room := w - countW - reserve
	title = truncateString(title, max(room, min(floor, w-countW), 1))
	header := lipgloss.NewStyle().Bold(true).Foreground(ColorCyan).Render(title + count)
	for _, sfx := range suffixes {
		if lipgloss.Width(header)+lipgloss.Width(sfx) <= w {
			header += sfx
		}
	}
	return header
}

// bareView draws the viewer's bare form: the header over the lines, exactly
// width x height cells. Memoised as View is, on the same key; while the
// search box is open the header changes with a field the key lacks, so the
// frame is drawn every time.
func (m *LogViewerModel) bareView() string {
	if !m.searching {
		if k := m.key(); m.renderCache != "" && k == m.cacheKey {
			return m.renderCache
		}
	}
	out := fitBlock(m.bareHeader()+"\n"+m.viewport.View(), m.width, m.height)
	if !m.searching {
		m.renderCache = out
		m.cacheKey = m.key()
	} else {
		m.renderCache = ""
	}
	return out
}

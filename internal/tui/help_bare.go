package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Help's bare form, drawn below the normal layout (overlay_bare.go): a
// one-row header over the key list at the full width, no border. It lists
// only the keys that work at the current size (owner ruling R-C12): a chord
// whose overlay needs a larger terminal is left out by overlayOpensAt, the
// predicate the prefix hint (chordHint) drops it by, so the two key lists
// agree and nothing listed answers "Enlarge the terminal". Its last row says
// the list is partial.

// bareHelpKeyCol is the bare key column: a chord with its confirm key
// ("A X X") and a space. A longer key takes a row of its own.
const bareHelpKeyCol = 6

// bareHelpMore is the last row of the bare key list.
const bareHelpMore = "More keys at 60x20"

// bareHelpKey is one static row of the bare key list. opens names the
// overlay the key opens or belongs to: the row is listed only where that
// overlay opens. Empty: the key works on the compact task list itself.
type bareHelpKey struct {
	key, desc, opens string
}

// The static sections as the compact layout has them: one panel, so no
// focus keys and no Details or Logs halves.
var bareQuickKeys = []bareHelpKey{
	{"F", "Cycle status filter", ""},
	{"M", "Action menu", "action menu"},
	{"Q Q", "Quit program", ""},
	{"Ctrl+C", "Quit immediately", ""},
	{"`", "Open settings", "settings"},
	{"?", "Toggle help", "help"},
}

var bareNavigationKeys = []bareHelpKey{
	{"↑/↓", "Select / Scroll", ""},
	{"PgUp/PgDn", "Page scroll", ""},
	{"Home/End", "Jump to first / last task", ""},
	{"Enter", "Expand/collapse archives", ""},
	{"Space", "Select task for batch actions", ""},
	{"Ctrl+A", "Select every task shown", ""},
	{"Esc", "Clear batch selection · filter · armed chord", ""},
	{"/", "Filter query", ""},
	{"", "status:live type:clip", ""},
	{"", "channel:\"name\"", ""},
	{"", "platform:youtube", ""},
	{"", "-negate a|b \"a phrase\"", ""},
	{"Ctrl+U/Ctrl+D", "Half-page scroll (Job Log)", "job log"},
	{"End", "Resume auto-scroll (Job Log)", "job log"},
	{"/", "Find text (Job Log)", "job log"},
	{"n / N", "Next / previous search match (Job Log)", "job log"},
}

var bareMouseKeys = []bareHelpKey{
	{"Click", "Select task", ""},
	{"Scroll", "Scroll the list, or the overlay on top", ""},
}

// bare reports whether the help draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *HelpModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareOpens reports whether the overlay called name opens at the help's
// size. The empty name is no overlay: a key of the task list.
func (m *HelpModel) bareOpens(name string) bool {
	if name == "" {
		return true
	}
	ok, _, _ := overlayOpensAt(name, m.width, m.height)
	return ok
}

// bareSections is the key list of the bare form: the chord sections without
// the chords refused at this size, then the static sections.
func (m *HelpModel) bareSections() []helpSection {
	var working []ActionMenuItem
	for _, item := range m.menuItems {
		if m.bareOpens(item.Opens) {
			working = append(working, item)
		}
	}
	var sections []helpSection
	if len(working) > 0 {
		chords := HelpModel{menuItems: working}
		sections = chords.sectionsFromMenu()
	}
	for _, static := range []struct {
		title string
		keys  []bareHelpKey
	}{
		{quickKeys.title, bareQuickKeys},
		{navigationKeys.title, bareNavigationKeys},
		{mouseKeys.title, bareMouseKeys},
	} {
		sec := helpSection{title: static.title}
		for _, k := range static.keys {
			if m.bareOpens(k.opens) {
				sec.keys = append(sec.keys, helpKey{key: k.key, desc: k.desc})
			}
		}
		sections = append(sections, sec)
	}
	return sections
}

// buildBareContent fills the viewport for the bare form: the full width,
// every row but the header.
func (m *HelpModel) buildBareContent() {
	descW := max(m.width-bareHelpKeyCol, 1)
	pad := strings.Repeat(" ", bareHelpKeyCol)

	var lines []string
	for _, sec := range m.bareSections() {
		lines = append(lines, HeaderStyle.Render(truncateWidth(sec.title, m.width, "…")))
		for _, k := range sec.keys {
			desc := strings.Split(ansi.Wrap(k.desc, descW, ""), "\n")
			switch kw := ansi.StringWidth(k.key); {
			case kw == 0:
				// A row with no key: under the description column.
			case kw < bareHelpKeyCol:
				lines = append(lines, YellowStyle.Render(k.key)+strings.Repeat(" ", bareHelpKeyCol-kw)+desc[0])
				desc = desc[1:]
			default:
				// A key wider than its column takes a row of its own.
				lines = append(lines, YellowStyle.Render(truncateWidth(k.key, m.width, "…")))
			}
			for _, row := range desc {
				lines = append(lines, pad+row)
			}
		}
	}
	lines = append(lines, DimStyle.Render(truncateWidth(bareHelpMore, m.width, "…")))

	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(max(m.height-1, 1)) // the header row
	m.viewport.SetContent(strings.Join(lines, "\n"))
	m.viewport.GotoTop()
}

// bareView draws the bare form: exactly width x height cells.
func (m *HelpModel) bareView() string {
	left := TitleStyle.Render("Help")
	if m.viewport.TotalLineCount() > m.viewport.Height() {
		left += " " + DimStyle.Render(fmt.Sprintf("%d%%", int(m.viewport.ScrollPercent()*100)))
	}
	return bareSheet{
		left:  left,
		right: DimStyle.Render("?/Esc close"),
		body:  strings.Split(m.viewport.View(), "\n"),
	}.render(m.width, m.height)
}

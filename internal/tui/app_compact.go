package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The TUI draws one of three layouts, decided by the terminal size alone
// (owner rulings R-C4 to R-C7):
//
//   - normal, at or above minTermWidth x minTermHeight: the three panels and
//     every overlay.
//   - compact, below that and at or above compactMinWidth x compactMinHeight:
//     the task list filling the screen, one row a job, then a rule, a
//     one-line status bar and the feedback line. Navigation, batch selection,
//     the "/" filter and the chords that open no overlay work; an action that
//     opens an overlay is refused on the feedback line.
//   - too small, below the compact floor: a notice, and only Ctrl+C works.
//
// An overlay already open when the terminal leaves the normal layout is kept
// with its state and covered by a notice; until the terminal grows back only
// Ctrl+C works, and Esc where the notice says what it does (overlay_held.go).
//
// The decision is layoutMode, derived from the size on every call and never
// stored: View, handleKey, handleMouse and the tick gating all ask it.

// layoutMode is which of the three layouts the terminal size selects.
type layoutMode int

const (
	layoutNormal layoutMode = iota
	layoutCompact
	layoutTooSmall
)

// compactMinWidth and compactMinHeight are the compact layout's floor: the
// tag, a five-cell title and a figure across, and three list rows above the
// rule, the status bar and the feedback line.
const (
	compactMinWidth  = 30
	compactMinHeight = 6
)

// compactChromeRows is the rule, the status bar and the feedback line under
// the compact task list.
const compactChromeRows = 3

// compactRefusal is the feedback line an action that opens an overlay gets
// below the normal layout (owner ruling R-C5).
const compactRefusal = "Enlarge the terminal to 60x20 for this"

// heldOverlayNotice covers an overlay kept open below the normal layout
// (owner ruling R-C6).
const heldOverlayNotice = "Enlarge to 60x20 to continue"

// layoutModeFor is THE size rule. A terminal that has not reported a size yet
// (0x0, before the first WindowSizeMsg) is the normal layout: View shows
// "Initializing..." for it and nothing is refused.
func layoutModeFor(width, height int) layoutMode {
	switch {
	case width == 0 || height == 0:
		return layoutNormal
	case width >= minTermWidth && height >= minTermHeight:
		return layoutNormal
	case width >= compactMinWidth && height >= compactMinHeight:
		return layoutCompact
	}
	return layoutTooSmall
}

// layoutMode is layoutModeFor of the terminal's current size.
func (a *App) layoutMode() layoutMode { return layoutModeFor(a.width, a.height) }

// screenHeld reports whether a notice has replaced the screen: the terminal
// is below the compact floor, or it is below the normal layout with an
// overlay open underneath. While it is held the mouse is ignored, nothing is
// typed into what is hidden, and the progress tick idles.
func (a *App) screenHeld() bool {
	switch a.layoutMode() {
	case layoutTooSmall:
		return true
	case layoutCompact:
		return a.hasActiveOverlay()
	}
	return false
}

// keyReachesComponents reports whether a key press is fed to the embedded
// components (text inputs, viewports) before handleKey decides on it. Not
// while the screen is held, except the Esc a kept overlay still answers
// (heldEscFor).
func (a *App) keyReachesComponents(msg tea.KeyPressMsg) bool {
	if !a.screenHeld() {
		return true
	}
	if a.layoutMode() != layoutCompact || msg.String() != keyEsc {
		return false
	}
	o := a.activeOverlay()
	return o != nil && a.heldEscFor(o) != heldEscOff
}

// refuseOverlay answers an action that would open an overlay. In the normal
// layout it does nothing and reports false. Below it, it disarms the chord,
// writes the refusal on the feedback line and reports true, and the caller
// opens nothing.
func (a *App) refuseOverlay() bool {
	if a.layoutMode() == layoutNormal {
		return false
	}
	a.disarmChord()
	a.setFeedbackWithSeverity(compactRefusal, severityWarning)
	return true
}

// menuItem is the buildMenuItems entry for chord, nil when it has none.
func (a *App) menuItem(chord string) *ActionMenuItem {
	items := a.buildMenuItems()
	for i := range items {
		if items[i].Chord == chord {
			return &items[i]
		}
	}
	return nil
}

// refusedForSize reports whether chord is an action the current layout
// refuses, and refuses it: one that opens an overlay (ActionMenuItem.Opens)
// below the normal layout.
func (a *App) refusedForSize(chord string) bool {
	if a.layoutMode() == layoutNormal {
		return false
	}
	item := a.menuItem(chord)
	return item != nil && item.Opens != "" && a.refuseOverlay()
}

// noticeView is a notice that replaces the screen, cut to the terminal: no
// line wider than it, no more lines than it has rows.
func (a *App) noticeView(lines ...string) tea.View {
	lines = lines[:min(len(lines), a.height)]
	for i := range lines {
		lines[i] = truncateWidth(lines[i], a.width, "")
	}
	return a.viewWithMode(strings.Join(lines, "\n"))
}

// tooSmallView is the notice below the compact floor. Two short lines, not
// one: in the narrow terminals it is shown in, the half that says what to do
// must not be cut off the right edge.
func (a *App) tooSmallView() tea.View {
	return a.noticeView(
		fmt.Sprintf("Terminal too small: %d×%d", a.width, a.height),
		fmt.Sprintf("Moombox needs at least %d×%d", compactMinWidth, compactMinHeight),
	)
}

// heldOverlayView covers an overlay kept open below the normal layout. Its
// Esc line is heldEscFor, the rule the handleKey size guard delivers Esc by.
func (a *App) heldOverlayView(o *overlayEntry) tea.View {
	return a.noticeView(
		heldOverlayNotice,
		"",
		"Open: "+o.title,
		heldEscLine(a.heldEscFor(o)),
		"Ctrl+C quits.",
	)
}

// compactFrame memoises the compact screen as frameComposer does the panel
// frame: the status line under its inputs, and the whole frame under its.
type compactFrame struct {
	barKey compactBarKey
	bar    string

	valid   bool
	key     compactFrameKey
	content string
}

// compactBarKey is everything the compact status line is built from.
type compactBarKey struct {
	width    int
	summary  string
	selected int
	query    string
	status   statusBarKey
	update   bool
	restart  bool
	security bool
}

// compactFrameKey is everything the compact frame is built from. The task
// string is the task list's own memoised frame, so comparing it is a pointer
// check.
type compactFrameKey struct {
	task, bar     string
	width, height int
	fbMsg         string
	fbSev         feedbackSeverity
}

// compactStatusLine is the compact layout's one-line status bar: the job
// counts of the Tasks header on the left, with the batch selection and the
// active filter, and the warnings on the right: offline, sidecar down, a
// disk warning, the cookie alerts, a pending update, a pending restart and
// the passwordless-access warning. The warnings are kept whole; the left
// half is cut to what is left.
func (a *App) compactStatusLine() string {
	key := compactBarKey{
		width:    a.width,
		summary:  a.taskList.statusSummary,
		selected: a.taskList.SelectedCount(),
		query:    a.taskList.queryText,
		status:   a.statusBar.statusBarKey(),
		update:   a.updateAvailable != nil,
		restart:  a.restartPending,
		security: a.securityBannerText() != "",
	}
	if a.compact.bar != "" && a.compact.barKey == key {
		return a.compact.bar
	}

	var left []string
	if key.summary != "" {
		left = append(left, key.summary)
	} else {
		left = append(left, DimStyle.Render("No jobs"))
	}
	if key.selected > 0 {
		left = append(left, statusBarYelStyle.Render(fmt.Sprintf("%d sel", key.selected)))
	}
	if key.query != "" {
		left = append(left, DimStyle.Render("/"+key.query))
	}

	var right []string
	if alerts := strings.TrimSpace(a.statusBar.alerts()); alerts != "" {
		right = append(right, alerts)
	}
	if key.security {
		right = append(right, statusBarRedStyle.Render("NO PW"))
	}
	if key.restart {
		right = append(right, statusBarYelStyle.Render("RESTART"))
	}
	if key.update {
		right = append(right, statusBarGrnStyle.Render("UPD"))
	}

	r := truncateWidth(strings.Join(right, " "), max(a.width-2, 0), "…")
	rw := ansi.StringWidth(r)
	room := a.width - 1 // the leading space
	if rw > 0 {
		room -= rw + 2 // a gap, and the trailing space
	}
	l := truncateWidth(strings.Join(left, " "), max(room, 0), "…")

	var sb strings.Builder
	sb.WriteByte(' ')
	sb.WriteString(l)
	used := 1 + ansi.StringWidth(l)
	if rw > 0 {
		writeSpaces(&sb, a.width-used-rw-1)
		sb.WriteString(r)
		sb.WriteByte(' ')
	} else {
		writeSpaces(&sb, a.width-used)
	}
	bar := statusBarBgStyle.MaxWidth(a.width).Render(sb.String())
	a.compact.barKey, a.compact.bar = key, bar
	return bar
}

// compactView draws the compact layout: the task list, a rule, the status
// line and the feedback line. Exactly a.height rows of at most a.width
// cells.
func (a *App) compactView() tea.View {
	key := compactFrameKey{
		task:   a.taskList.View(),
		bar:    a.compactStatusLine(),
		width:  a.width,
		height: a.height,
		fbMsg:  a.feedback.msg,
		fbSev:  a.feedback.sev,
	}
	if a.compact.valid && a.compact.key == key {
		return a.viewWithMode(a.compact.content)
	}

	// One row, cut with an ellipsis: a confirm prompt leads with the key to
	// press and what it does, so the cut takes the title's tail.
	feedback := ""
	if key.fbMsg != "" {
		feedback = lipgloss.NewStyle().Foreground(feedbackColor(key.fbMsg, key.fbSev)).
			Render(truncateWidth(key.fbMsg, a.width, "…"))
	}
	content := key.task + "\n" +
		DimStyle.Render(strings.Repeat("─", a.width)) + "\n" +
		key.bar + "\n" +
		fitBlock(feedback, a.width, 1)

	a.compact.valid, a.compact.key, a.compact.content = true, key, content
	return a.viewWithMode(content)
}

// handleCompactMouse is the mouse in the compact layout: the wheel moves
// through the list and a click on a row selects it, as in the Tasks panel.
// Nothing else is clickable.
func (a *App) handleCompactMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if dir := wheelDir(msg); dir != 0 {
		for range 3 {
			if dir < 0 {
				a.taskList.MoveUp()
			} else {
				a.taskList.MoveDown()
			}
		}
		a.updateSelectedJob()
		return a, nil
	}
	if isLeftClick(msg) {
		m := msg.Mouse()
		if a.taskRegion.Contains(m.X, m.Y) && a.taskList.SelectAtOffset(m.Y-a.taskRegion.Y) {
			if a.taskList.SelectedIsDivider() {
				a.taskList.ToggleArchive()
			} else {
				a.updateSelectedJob()
			}
		}
	}
	return a, nil
}

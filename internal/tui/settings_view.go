package tui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// --- Rendering ---

// View renders the settings panel.
func (m *SettingsModel) View() string {
	if !m.visible {
		return ""
	}

	// Restart overlay
	if m.showRestartOverlay {
		return m.renderRestartOverlay()
	}

	innerW := m.settingsInnerWidth()
	h := max(m.height-2, 10)

	var content strings.Builder

	// Header: "Settings — General | Downloader | ..."
	content.WriteString(m.renderHeader(innerW))
	content.WriteString("\n")

	// Divider
	content.WriteString(DimStyle.Render(strings.Repeat("\u2500", innerW)))
	content.WriteString("\n")

	sec := sections[m.sectionIndex]

	// Per-section content rows come from settingsContentHeight() — the same
	// helper ensureFieldVisible and mouse hit-testing use, so the rendered
	// window and the scroll/click math can't drift apart.
	contentRows := m.settingsContentHeight()

	// Section content
	switch sec.name {
	case "Network":
		// Network fields + embedded security sub-editor
		if m.secMode != securityStatus {
			content.WriteString(m.renderSecurity(innerW))
		} else {
			content.WriteString(m.renderFields(sec, innerW, contentRows))
			content.WriteString("\n")
			content.WriteString(m.renderSecurityCompact(innerW))
		}
	case "Channels":
		content.WriteString(m.renderChannels(innerW, contentRows))
	case "Integrations":
		content.WriteString(m.renderNotifications(innerW, contentRows))
	default:
		content.WriteString(m.renderFields(sec, innerW, contentRows))
	}

	// Action buttons (always visible).
	// Record the contentY for mouse hit-testing: count newlines so far,
	// subtract 2 for header+divider (which contentY already excludes).
	m.lastButtonContentY = strings.Count(content.String(), "\n") - 2 + 1 // +1 for the \n we're about to add
	content.WriteString("\n")
	content.WriteString(m.renderActionButtons())

	// Status line
	content.WriteString("\n")
	if m.closeConfirm {
		content.WriteString(YellowStyle.Render(
			"Save changes? [Y]es / [N]o / [Esc] Cancel"))
	} else {
		switch m.status {
		case saveSaved:
			content.WriteString(lipgloss.NewStyle().Foreground(ColorGreen).Render("Saved"))
		case saveNotice:
			content.WriteString(lipgloss.NewStyle().Foreground(ColorGreen).Render(m.errorMsg))
		case saveError:
			content.WriteString(lipgloss.NewStyle().Foreground(ColorRed).Render(m.errorMsg))
		default:
			if m.dirty {
				content.WriteString(YellowFaintStyle.Render("Unsaved changes"))
			}
		}
	}

	// Hints
	content.WriteString("\n")
	escHint := "Esc: Close"
	if m.channelRemovalPromptUp() {
		escHint = "Esc: Cancel" // the prompt's Esc closes the prompt, not the overlay
	}
	hintLeft := DimStyle.Render(escHint)
	hintRight := fitSettingsHint(m.renderHintText(), innerW-ansi.StringWidth(escHint)-1)
	hintGap := innerW - ansi.StringWidth(escHint) - ansi.StringWidth(hintRight)
	hintGap = max(hintGap, 1)
	content.WriteString(hintLeft)
	content.WriteString(strings.Repeat(" ", hintGap))
	content.WriteString(DimStyle.Render(hintRight))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(innerW + 2).
		Height(h + 2).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}

// Header layout, shared with handleMouseTabClick.
const (
	settingsHeaderPrefixW = 11 // "Settings" + " ─ "
	settingsTabSepW       = 3  // " │ "
	settingsTabMarkerW    = 2  // "‹ " before the window, " ›" after it
)

// renderHeader draws "Settings ─ <tabs>   N/M". All twelve section names
// need about 140 cells, more than most terminals give, and the single
// truncated line it used to be cut the later sections off — the active one
// included — and the counter with them. The strip now shows the window of
// tabs that fits, slid to keep the active one on screen, with ‹ / › where
// tabs are hidden, and the counter is always drawn at the right edge.
func (m *SettingsModel) renderHeader(w int) string {
	left := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Settings") +
		DimStyle.Render(" \u2500 ")
	right := DimStyle.Render(fmt.Sprintf("%d/%d", m.sectionIndex+1, len(sections)))

	avail := w - settingsHeaderPrefixW - lipgloss.Width(right) - 1
	start, end := settingsTabWindow(m.headerTabStart, m.sectionIndex, avail)
	m.headerTabStart, m.headerTabEnd = start, end

	var tabs strings.Builder
	if start > 0 {
		tabs.WriteString(DimStyle.Render("\u2039 "))
	}
	for i := start; i < end; i++ {
		if i > start {
			tabs.WriteString(DimStyle.Render(" \u2502 "))
		}
		if i == m.sectionIndex {
			tabs.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(sections[i].name))
		} else {
			tabs.WriteString(DimStyle.Render(sections[i].name))
		}
	}
	if end < len(sections) {
		tabs.WriteString(DimStyle.Render(" \u203a"))
	}

	gap := max(w-lipgloss.Width(left)-lipgloss.Width(tabs.String())-lipgloss.Width(right), 1)
	header := left + tabs.String() + strings.Repeat(" ", gap) + right
	return lipgloss.NewStyle().MaxWidth(w).Render(header)
}

// settingsTabStripWidth is the cells sections[start:end] take in the header,
// separators and ‹ / › markers included.
func settingsTabStripWidth(start, end int) int {
	n := 0
	for i := start; i < end; i++ {
		if i > start {
			n += settingsTabSepW
		}
		n += ansi.StringWidth(sections[i].name)
	}
	if start > 0 {
		n += settingsTabMarkerW
	}
	if end < len(sections) {
		n += settingsTabMarkerW
	}
	return n
}

// settingsTabWindow picks the sections [start, end) the header shows in
// avail cells: all of them when they fit, otherwise a window holding the
// active section. The window starts where it last did (prevStart), so moving
// one section slides it by one instead of re-centring it, and it is filled
// from both sides as far as it fits. A window too narrow for even the active
// tab still returns it; renderHeader's MaxWidth cuts the line.
func settingsTabWindow(prevStart, active, avail int) (start, end int) {
	n := len(sections)
	if settingsTabStripWidth(0, n) <= avail {
		return 0, n
	}
	start = min(max(prevStart, 0), active)
	end = start + 1
	for end < n && settingsTabStripWidth(start, end+1) <= avail {
		end++
	}
	if active >= end {
		// Moved past the right edge: the active tab becomes the last one.
		start, end = active, active+1
	}
	for start > 0 && settingsTabStripWidth(start-1, end) <= avail {
		start--
	}
	for end < n && settingsTabStripWidth(start, end+1) <= avail {
		end++
	}
	return start, end
}

// settingsGenericHints are the hints every section shows; they go first when
// the footer is too narrow, so the keys particular to the field or section
// stay on screen.
var settingsGenericHints = []string{
	"Shift+\u2193: Buttons",
	"Shift+\u2190/\u2192: Section",
	"\u2191/\u2193: Navigate",
	"\u2191/\u2193/Tab: Navigate",
}

// fitSettingsHint fits a footer hint ("key: action" entries two spaces
// apart) into room cells. A hint too long used to wrap inside the box,
// splitting an entry across two lines ("D:" / "Delete"). The generic
// entries are dropped first, then whole entries from the end; an entry is
// never cut.
func fitSettingsHint(hint string, room int) string {
	const sep = "  "
	parts := strings.Split(hint, sep)
	fits := func() bool { return ansi.StringWidth(strings.Join(parts, sep)) <= room }
	for _, generic := range settingsGenericHints {
		if fits() {
			break
		}
		if i := slices.Index(parts, generic); i >= 0 && len(parts) > 1 {
			parts = slices.Delete(parts, i, i+1)
		}
	}
	for !fits() && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, sep)
}

func (m *SettingsModel) renderHintText() string {
	sec := sections[m.sectionIndex]
	if sec.name == "Network" && m.secMode != securityStatus {
		return "Esc: Back  \u2191/\u2193/Tab: Navigate  Enter: Save"
	}
	if m.isFieldSection() {
		// Button focus mode
		if m.buttonFocus >= 0 {
			return "\u2190/\u2192: Switch button  \u2191: Back to fields  Enter: Activate"
		}
		field := sec.fields[m.fieldIndex]
		// Shift+↓ is the only key that reaches [ Save & Return ] without
		// leaving through Esc's prompt; unlisted, nobody found it.
		hint := "Shift+\u2190/\u2192: Section  \u2191/\u2193: Navigate  Shift+\u2193: Buttons"
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			hint = "\u2190/\u2192: Toggle  " + hint
		}
		// ` and ~ reach a text or number field as typed characters (a
		// cert path can hold a ~), so the password editor opens from the
		// other rows only — and only those rows advertise it.
		if sec.name == "Network" && field.ftype != fieldText && field.ftype != fieldNumber {
			if m.hasPassword() {
				hint += "  `: Change pw  ~: Remove pw"
			} else {
				hint += "  `: Set pw"
			}
		}
		if sec.name == "Paths" && field.key == "ffmpeg_path" {
			hint += "  Ctrl+O: Install FFmpeg"
		}
		return hint
	}
	if m.channelRemovalPromptUp() {
		return m.channelRemovalHint()
	}
	return "Shift+\u2190/\u2192: Section  \u2191/\u2193: Navigate  A: Add  Enter: Edit  D: Delete"
}

// renderActionButtons renders action buttons at the bottom of the settings panel.
// Always shows "Return" button. Shows "Save & Return" only when dirty.
func (m *SettingsModel) renderActionButtons() string {
	focusedStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("0")).
		Background(ColorCyan).
		Bold(true)
	blurredStyle := lipgloss.NewStyle().
		Foreground(ColorGray)

	if m.dirty {
		saveLabel := "[ Save & Return ]"
		discardLabel := "[ Return Without Saving ]"

		var saveStr, discardStr string
		if m.buttonFocus == 0 {
			saveStr = focusedStyle.Render(saveLabel)
		} else {
			saveStr = blurredStyle.Render(saveLabel)
		}
		if m.buttonFocus == 1 {
			discardStr = focusedStyle.Render(discardLabel)
		} else {
			discardStr = blurredStyle.Render(discardLabel)
		}
		return saveStr + "  " + discardStr
	}

	// Not dirty — just show Return button
	returnLabel := "[ Return ]"
	if m.buttonFocus == 0 {
		return focusedStyle.Render(returnLabel)
	}
	return blurredStyle.Render(returnLabel)
}

func (m *SettingsModel) renderFields(sec settingsSection, w, maxH int) string {
	if sec.fields == nil {
		return DimStyle.Render("No settings in this section")
	}

	// Compute max label width for alignment
	maxLabel := 0
	for _, fd := range sec.fields {
		if len(fd.label) > maxLabel {
			maxLabel = len(fd.label)
		}
	}
	padWidth := maxLabel + 2

	var lines []string
	end := min(m.scrollOffset+maxH, len(sec.fields))

	for i := m.scrollOffset; i < end; i++ {
		fd := sec.fields[i]
		selected := i == m.fieldIndex
		isChanged := m.values[fd.key] != m.originalValues[fd.key]
		needsRestart := isChanged && restartRequiredKeys[fd.key]

		// Prefix: 2-char slot combining selection cursor and change indicator.
		// "  " = normal, "> " = selected, "* " = modified, "*>" = modified+selected
		var prefix string
		var prefixStyle lipgloss.Style
		switch {
		case isChanged && selected:
			prefix = "*>"
			if needsRestart {
				prefixStyle = YellowStyle
			} else {
				prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
			}
		case isChanged:
			prefix = "* "
			if needsRestart {
				prefixStyle = YellowFaintStyle
			} else {
				prefixStyle = lipgloss.NewStyle().Foreground(ColorGreen).Faint(true)
			}
		case selected:
			prefix = "> "
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		default:
			prefix = "  "
			prefixStyle = lipgloss.NewStyle()
		}

		// Label
		labelStr := padRight(fd.label, padWidth)
		labelStyle := lipgloss.NewStyle().Foreground(ColorGray)
		if selected {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}

		// Value
		var value string
		switch fd.ftype {
		case fieldToggle:
			value = renderToggle(m.values[fd.key])
		case fieldCycle:
			value = renderCycleOptions(fd.options, m.values[fd.key], selected, max(w-len(prefix)-padWidth, 5))
		default:
			valueMaxW := max(w-len(prefix)-padWidth, 5)
			if selected {
				m.textInput.SetWidth(valueMaxW - 1) // V2 textinput renders width+1 (cursor block)
				value = m.textInput.View()
			} else {
				value = renderInactiveInput(m.values[fd.key], valueMaxW, ColorWhite)
			}
		}

		line := prefixStyle.Render(prefix) + labelStyle.Render(labelStr) + value
		lines = append(lines, line)

		if fd.previewFn != nil {
			preview := fd.previewFn(m.values[fd.key])
			if preview != "" {
				// One row, as settingsContentHeight budgets: the box would
				// word-wrap a long preview into rows nothing reserved.
				lines = append(lines, "  "+DimStyle.Render(truncateWidth(preview, max(w-2, 1), "…")))
			}
		}
	}

	// Info area: divider + help text for focused field
	if m.fieldIndex < len(sec.fields) {
		fd := sec.fields[m.fieldIndex]
		isChanged := m.values[fd.key] != m.originalValues[fd.key]
		needsRestart := isChanged && restartRequiredKeys[fd.key]

		lines = append(lines, "")
		lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", w)))

		var infoParts []string
		if fd.help != "" {
			infoParts = append(infoParts, fd.help)
		}
		if needsRestart {
			infoParts = append(infoParts, YellowStyle.Render("[restart required]"))
		} else if isChanged {
			infoParts = append(infoParts, lipgloss.NewStyle().Foreground(ColorGreen).Render("[modified]"))
		}

		if len(infoParts) > 0 {
			// Wrapped here rather than by the box, so the rows it takes are
			// rows settingsContentHeight reserved (fieldInfoRows) and the
			// button row's mouse offset counts them.
			info := DimStyle.Render(strings.Join(infoParts, "  "))
			lines = append(lines, strings.Split(ansi.Wrap(info, w, ""), "\n")...)
		}
	}

	return strings.Join(lines, "\n")
}

// settingsInnerWidth is the content width View() lays the box out at.
func (m *SettingsModel) settingsInnerWidth() int {
	boxW := min(max(m.width-4, 40), m.width)
	return boxW - 4
}

// fieldInfoRows is how many rows the focused-field info line can take in this
// section at width w: the longest help text in it, with the longer of its two
// status tags, wrapped the way renderFields wraps it. The worst case over the
// section rather than the focused field's own, so the field window does not
// resize under the operator as the focus moves. At least one row — the one the
// budget always held.
func fieldInfoRows(sec settingsSection, w int) int {
	rows := 1
	for _, fd := range sec.fields {
		tag := "[modified]"
		if restartRequiredKeys[fd.key] {
			tag = "[restart required]"
		}
		text := tag
		if fd.help != "" {
			text = fd.help + "  " + tag
		}
		rows = max(rows, strings.Count(ansi.Wrap(text, max(w, 1), ""), "\n")+1)
	}
	return rows
}

// renderToggle shows both choices with the selected one bracketed, as
// renderCycleOptions does. Colour and the faint unselected half used to be the
// whole signal, and neither survives a NO_COLOR terminal, a colour-blind
// reader or a pasted screenshot: "Yes / No" read the same either way.
// toggleYesWidth is the click geometry that follows from it.
func renderToggle(value string) string {
	if value == "Yes" {
		return lipgloss.NewStyle().Foreground(ColorGreen).Bold(true).Render("[Yes]") + DimStyle.Render(" / No")
	}
	return DimStyle.Render("Yes / ") + lipgloss.NewStyle().Foreground(ColorRed).Bold(true).Render("[No]")
}

// toggleYesWidth is how many columns renderToggle(value) gives its "Yes" half.
func toggleYesWidth(value string) int {
	if value == "Yes" {
		return len("[Yes]")
	}
	return len("Yes")
}

// renderCycleOptions shows every option, the selected one bracketed, when
// that fits maxW columns; otherwise only the selected one, between dim ‹ › so
// the row still reads as a choice. Every field is budgeted one row: the full
// list wrapped — at the 60-column floor "[localhost] / lan / external" left
// "external" alone on the next row, and a channel's fifteen-option quality
// preference wrapped at any width.
func renderCycleOptions(options []string, selected string, focused bool, maxW int) string {
	selStyle := lipgloss.NewStyle().Foreground(ColorWhite).Bold(true)
	if focused {
		selStyle = selStyle.Foreground(ColorCyan)
	}
	var parts []string
	sel := selected
	for _, opt := range options {
		if strings.EqualFold(opt, selected) {
			sel = opt
			parts = append(parts, selStyle.Render("["+opt+"]"))
		} else {
			parts = append(parts, DimStyle.Render(opt))
		}
	}
	full := strings.Join(parts, DimStyle.Render(" / "))
	if ansi.StringWidth(full) <= maxW {
		return full
	}
	return DimStyle.Render("‹ ") + selStyle.Render("["+truncateWidth(sel, max(maxW-6, 1), "…")+"]") + DimStyle.Render(" ›")
}

// listWindowStart returns the first rendered index for a list capped at
// maxH rows, scrolled so the selected index stays visible (mirrors
// renderFields' scrollOffset approach for the channel/notification lists).
func listWindowStart(selected, maxH int) int {
	if maxH > 0 && selected >= maxH {
		return selected - maxH + 1
	}
	return 0
}

func (m *SettingsModel) renderChannels(w, maxH int) string {
	if m.channelMode == "edit" {
		return m.renderChannelEdit(w)
	}

	var lines []string

	// Action bar. The removal prompt replaces the key list rather than
	// trailing it — the two together are wider than a 60-column box — and
	// takes its extra rows from the list's window.
	if m.channelDeleteConf {
		prompt := m.channelRemovalPromptLines(w, maxH)
		lines = append(lines, prompt...)
		maxH = max(maxH-(len(prompt)-1), 1)
	} else {
		lines = append(lines, DimStyle.Render("A: Add  Enter: Edit  D: Delete"))
	}

	if len(m.channels) == 0 {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  No channels configured. Press A to add one."))
		return strings.Join(lines, "\n")
	}

	// Window the list around the selection so navigation past the rendered
	// tail isn't blind.
	start := listWindowStart(m.channelIndex, maxH)
	end := min(start+maxH, len(m.channels))
	for i := start; i < end; i++ {
		ch := m.channels[i]
		selected := i == m.channelIndex

		prefix := "  "
		if selected {
			prefix = "> "
		}

		// Platform icon
		platformIcon := "YT"
		platformColor := ColorRed
		if ch.GetPlatform() == "twitch" {
			platformIcon = "TW"
			platformColor = ColorCookies
		}
		platStr := lipgloss.NewStyle().Foreground(platformColor).Render("[" + platformIcon + "]")

		// Name + ID
		name := ch.Name
		if name == "" {
			name = ch.ID
		}
		nameColor := ColorWhite
		if selected {
			nameColor = ColorCyan
		}
		enabled := ch.IsEnabled()
		nameStyle := lipgloss.NewStyle().Foreground(nameColor)
		if !enabled {
			nameStyle = nameStyle.Faint(true)
		}

		// One row per channel, as listWindowStart and the mouse map assume:
		// the ID gives way first so "(disabled)" stays readable, and the
		// filter, last, is cut at the box edge.
		nameStr := truncateString(name, 20)
		disabledTag := ""
		if !enabled {
			disabledTag = " (disabled)"
		}
		// The last -1 keeps the cell the cut's "…" takes when a filter follows.
		idW := min(24, w-len(prefix)-len("[YT] ")-ansi.StringWidth(nameStr)-1-len(disabledTag)-1)

		line := prefix + platStr + " " + nameStyle.Render(nameStr)
		if idW >= 6 {
			line += " " + DimStyle.Render(truncateString(ch.ID, idW))
		}
		if disabledTag != "" {
			line += YellowFaintStyle.Render(disabledTag)
		}
		terms := ch.Terms.Simple
		if terms != "" {
			line += DimStyle.Render(" filter: " + truncateString(terms, 20))
		}

		lines = append(lines, truncateWidth(line, w, "…"))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderChannelEdit(w int) string {
	var lines []string

	title := "Edit Channel"
	if m.channelIndex >= len(m.channels) {
		title = "Add Channel"
	}
	hint := " (Enter: save, Esc: cancel)"
	if m.channelResolving {
		hint = " (resolving channel...)"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(hint))

	fields := m.visibleChannelFields()
	maxLabel := 0
	for _, f := range fields {
		if len(f.label) > maxLabel {
			maxLabel = len(f.label)
		}
	}
	padW := maxLabel + 2

	for idx, field := range fields {
		isFocused := idx == m.channelEditField
		val := m.channelEditValues[field.key]
		if val == "" && (field.ftype == fieldToggle || field.ftype == fieldCycle) && len(field.options) > 0 {
			val = field.options[0]
		}

		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelStyle := lipgloss.NewStyle().Foreground(ColorGray)
		if isFocused {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		prefixStyle := lipgloss.NewStyle()
		if isFocused {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		var value string
		switch field.ftype {
		case fieldToggle:
			value = renderToggle(val)
		case fieldCycle:
			value = renderCycleOptions(field.options, val, isFocused, max(w-ansi.StringWidth(prefix)-padW-2, 5))
		default:
			valueMaxW := max(w-ansi.StringWidth(prefix)-padW-2, 5)
			if isFocused {
				m.textInput.SetWidth(valueMaxW)
				value = m.textInput.View()
			} else {
				value = renderInactiveInput(val, valueMaxW, ColorWhite)
			}
		}

		line := prefixStyle.Render(prefix) + labelStyle.Render(padRight(field.label, padW)) + value
		if field.help != "" && isFocused {
			line += DimStyle.Render(" (" + field.help + ")")
		}
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderNotifications(w, maxH int) string {
	if m.notifMode == "edit" {
		return m.renderNotifEdit(w, maxH)
	}

	var lines []string

	// The delete prompt replaces the key list — see the Channels twin.
	actionBar := DimStyle.Render("A: Add  Enter: Edit  D: Delete  T: Test")
	if m.notifDeleteConf {
		actionBar = YellowStyle.Render("Press D again to confirm delete")
	}
	lines = append(lines, actionBar)

	if len(m.notifications) == 0 {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  No webhooks configured. Press A to add one."))
		return strings.Join(lines, "\n")
	}

	// Window the list around the selection (the cap counts notification
	// rows, not total lines — the action bar no longer eats a list row).
	start := listWindowStart(m.notifIndex, maxH)
	end := min(start+maxH, len(m.notifications))
	for i := start; i < end; i++ {
		n := m.notifications[i]
		selected := i == m.notifIndex
		prefix := "  "
		if selected {
			prefix = "> "
		}

		nameStyle := lipgloss.NewStyle()
		if selected {
			nameStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		// An empty filter is "all events" everywhere else — the manager, the
		// web card's success tag, operations.md — so it says so here too
		// rather than rendering as "25/25 events".
		filter := " (All events)"
		if len(n.Events) > 0 {
			filter = fmt.Sprintf(" (%d/%d events)", len(n.Events), len(allNotifEvents))
		}

		// The mute is invisible in a URL list otherwise, and a muted target
		// looks identical to a broken one — so it goes straight after the
		// URL, where the row's cut at the box edge never reaches it.
		muted := ""
		if !n.IsEnabled() {
			muted = " Muted"
		}
		urlDisplay := truncateString(n.URL, min(50, max(w-len(prefix)-len(muted)-len(filter)-1, 12)))

		line := prefix + nameStyle.Render(urlDisplay)
		if muted != "" {
			line += YellowStyle.Render(muted)
		}
		line += DimStyle.Render(filter)
		if n.Mention != "" {
			line += DimStyle.Render(" " + n.Mention)
		}
		// Only the opt-in mode is called out: "separate" is what every target
		// has always done, so saying so on every row would be noise.
		if strings.TrimSpace(n.Mode) == "edit" {
			line += DimStyle.Render(" · one message per job")
		}

		// One row per webhook, as listWindowStart and the mouse map assume.
		lines = append(lines, truncateWidth(line, w, "…"))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderNotifEdit(w, maxH int) string {
	var lines []string
	// Rows 0..notifEditEventBase-1 sit on lines 1..notifEditEventBase (the
	// title is pinned at line 0), so a non-event focus maps straight through.
	// An event row overwrites this below.
	focusLine := min(m.notifEditFocus, notifEditEventBase-1) + 1

	title := "Edit Notification"
	if m.notifIndex >= len(m.notifications) {
		title = "Add Notification"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))

	// The two text rows and the toggle between them share one label column.
	const labelW = 16
	textRow := func(focus int, label, value string) string {
		focused := m.notifEditFocus == focus
		prefix := "  "
		prefixColor := ColorWhite
		labelStyle := DimStyle
		if focused {
			prefix = "> "
			prefixColor = ColorCyan
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		maxW := max(w-ansi.StringWidth(prefix)-labelW-2, 10)
		var rendered string
		if focused {
			m.textInput.SetWidth(maxW)
			rendered = m.textInput.View()
		} else {
			rendered = renderInactiveInput(value, maxW, ColorWhite)
		}
		return lipgloss.NewStyle().Foreground(prefixColor).Render(prefix) +
			labelStyle.Render(padRight(label, labelW)) + rendered
	}

	// Webhook URL
	lines = append(lines, textRow(notifEditURLRow, "Webhook URL", m.notifEditURL))

	// Enabled — the mute. The target and its whole filter stay configured;
	// only delivery stops, which is what an operator who wants silence needs
	// now that an empty event filter means "all events".
	enabledFocused := m.notifEditFocus == notifEditEnabledRow
	enabledPrefix := "  "
	enabledPrefixColor := ColorWhite
	enabledLabelStyle := DimStyle
	if enabledFocused {
		enabledPrefix = "> "
		enabledPrefixColor = ColorCyan
		enabledLabelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	}
	enabledMark, enabledWord, enabledColor := " ", "Muted — kept, but delivers nothing", ColorGray
	if m.notifEditEnabled {
		enabledMark, enabledWord, enabledColor = "x", "Delivering", ColorGreen
	}
	enabledLine := lipgloss.NewStyle().Foreground(enabledPrefixColor).Render(enabledPrefix) +
		enabledLabelStyle.Render(padRight("Enabled", labelW)) +
		lipgloss.NewStyle().Foreground(enabledColor).Render("["+enabledMark+"] "+enabledWord)
	if enabledFocused {
		enabledLine += DimStyle.Render("  (Space to toggle)")
	}
	lines = append(lines, enabledLine)

	// Mention
	mentionLine := textRow(notifEditMentionRow, "Mention", m.notifEditMention)
	if m.notifEditFocus == notifEditMentionRow {
		mentionLine += DimStyle.Render(" (<@&ROLE_ID>, <@USER_ID>, @here, @everyone)")
	}
	lines = append(lines, mentionLine)

	// Delivery — the per-target mode. Rendered here, after Mention, because
	// the head rows sit on lines 1..notifEditEventBase and a row's line has to
	// match its focus index (notifEditDeliveryRow = 3 → line 4); the mouse map
	// and the focus arithmetic both read it that way.
	deliveryFocused := m.notifEditFocus == notifEditDeliveryRow
	deliveryPrefix := "  "
	deliveryPrefixColor := ColorWhite
	deliveryLabelStyle := DimStyle
	if deliveryFocused {
		deliveryPrefix = "> "
		deliveryPrefixColor = ColorCyan
		deliveryLabelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	}
	deliveryMark, deliveryWord, deliveryColor := " ", "Separate messages", ColorGray
	if m.notifEditDelivery == "edit" {
		deliveryMark, deliveryWord, deliveryColor = "x", "One message per job", ColorGreen
	}
	deliveryLine := lipgloss.NewStyle().Foreground(deliveryPrefixColor).Render(deliveryPrefix) +
		deliveryLabelStyle.Render(padRight("Delivery", labelW)) +
		lipgloss.NewStyle().Foreground(deliveryColor).Render("["+deliveryMark+"]") +
		lipgloss.NewStyle().Foreground(ColorWhite).Render(" "+deliveryWord)
	if deliveryFocused {
		deliveryLine += DimStyle.Render("  (Space to toggle)")
	}
	lines = append(lines, deliveryLine)

	// Events header
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("  Events (Space to toggle, m: mention @ column):"))

	// Event checkboxes grouped by category
	flatIdx := 0
	for _, group := range notifEventGroups {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  "+group.name))
		for _, event := range group.events {
			isFocused := m.notifEditFocus == flatIdx+notifEditEventBase
			if isFocused {
				focusLine = len(lines)
			}
			isChecked := m.notifEditEvents[event]

			prefix := "  "
			if isFocused {
				prefix = "> "
			}

			checkStr := " "
			checkColor := ColorGray
			if isChecked {
				checkStr = "x"
				checkColor = ColorGreen
			}

			eventStyle := lipgloss.NewStyle().Foreground(ColorWhite)
			if isFocused {
				eventStyle = lipgloss.NewStyle().Foreground(ColorCyan)
			}

			// The mention column, on the same row as the event it belongs to
			// rather than in a second 25-row block: an event's two flags are
			// read together, and one navigation list stays one list. Dim while
			// there is nobody to ping — the flag is kept, it just does nothing.
			mentionColor := ColorGray
			if m.notifEditMentionEvents[event] && strings.TrimSpace(m.notifEditMention) != "" {
				mentionColor = ColorYellow
			}

			lines = append(lines, lipgloss.NewStyle().Foreground(func() color.Color {
				if isFocused {
					return ColorCyan
				}
				return ColorWhite
			}()).Render(prefix)+
				lipgloss.NewStyle().Foreground(checkColor).Render("["+checkStr+"]")+
				eventStyle.Render(" "+padRight(event, notifEventNameWidth))+
				"  "+lipgloss.NewStyle().Foreground(mentionColor).Render("@"))
			flatIdx++
		}
	}

	// Scroll window that follows keyboard focus: the event list outgrew
	// short terminals when the Connectivity group landed, and the renderer's
	// bottom-truncation would otherwise let focus walk onto invisible rows.
	// The title row stays pinned.
	budget := maxH + 1 // renderNotifications' maxH counts list rows; +1 matches its action-bar line
	if budget > 1 && len(lines) > budget {
		body := lines[1:]
		visible := max(budget-1, 3)
		start := 0
		if f := focusLine - 1; f >= visible {
			start = f - visible + 1
		}
		start = min(start, max(len(body)-visible, 0))
		m.notifEditScrollStart = start // record for mouse-click line mapping
		out := append([]string{lines[0]}, body[start:min(start+visible, len(body))]...)
		return strings.Join(out, "\n")
	}

	m.notifEditScrollStart = 0
	return strings.Join(lines, "\n")
}

// renderSecurity draws the open password sub-editor. View calls it only while
// one is open; the status is renderSecurityCompact, under the Network fields.
func (m *SettingsModel) renderSecurity(w int) string {
	if m.secMode == securityRemove {
		return m.renderSecurityRemove(w)
	}
	return m.renderSecuritySet(w)
}

// renderSecurityCompact renders a compact password status below Network fields.
func (m *SettingsModel) renderSecurityCompact(w int) string {
	var lines []string
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", w)))

	status := DimStyle.Render("Not set")
	if m.hasPassword() {
		status = lipgloss.NewStyle().Foreground(ColorGreen).Render("Set")
	}
	lines = append(lines, "  Password: "+status)

	// From a toggle row: on a text or number row the keys type themselves.
	actionLine := DimStyle.Render("  `: Set password (from a toggle row)")
	if m.hasPassword() {
		actionLine = DimStyle.Render("  `: Change  ~: Remove (from a toggle row)")
	}
	lines = append(lines, actionLine)

	if m.secMessage != "" {
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecuritySet(w int) string {
	var lines []string

	title := "Set Password"
	if m.hasPassword() {
		title = "Change Password"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))
	lines = append(lines, "")

	type pwField struct {
		label string
		value string
	}
	var fields []pwField
	if m.hasPassword() {
		fields = append(fields, pwField{"Current password", m.secCurrentPw})
	}
	fields = append(fields, pwField{"New password", m.secNewPw})
	fields = append(fields, pwField{"Confirm password", m.secConfirmPw})

	maxLabel := 0
	for _, f := range fields {
		if len(f.label) > maxLabel {
			maxLabel = len(f.label)
		}
	}
	padW := maxLabel + 2

	for idx, f := range fields {
		isFocused := idx == m.secFieldIndex
		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelStyle := DimStyle
		if isFocused {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		prefixStyle := lipgloss.NewStyle()
		if isFocused {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		var val string
		if isFocused {
			pwMaxW := max(w-len(prefix)-padW-2, 10)
			m.textInput.SetWidth(pwMaxW)
			val = m.textInput.View()
		} else {
			val = renderPasswordDots(f.value)
		}

		lines = append(lines, prefixStyle.Render(prefix)+labelStyle.Render(padRight(f.label, padW))+val)
	}

	// A new password with a space at either end is kept as typed — no
	// surface trims a password — so say so: a warning, not a refusal.
	if config.PasswordHasOuterSpace(m.secNewPw) {
		lines = append(lines, "")
		lines = append(lines, "  "+YellowStyle.Render(config.PasswordOuterSpaceWarning))
	}

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecurityRemove(w int) string {
	var lines []string

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorRed).Bold(true).Render("Remove Password")+
		DimStyle.Render(" (Enter: confirm, Esc: cancel)"))
	lines = append(lines, "")

	isExternal := false
	if m.configStore != nil {
		m.configStore.Read(func(c *config.MoomboxConfig) {
			isExternal = isExternalAccess(c.Network.NetworkAccess)
		})
	}
	if isExternal {
		lines = append(lines, "  "+YellowStyle.Render(
			"Warning: Network access will be reset to localhost"))
	}

	// Password field
	labelStyle := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	pwMaxW := max(w-2-20-2, 10)
	m.textInput.SetWidth(pwMaxW)
	lines = append(lines, "> "+labelStyle.Render(padRight("Current password", 20))+m.textInput.View())

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderRestartOverlay() string {
	w := min(50, m.width-4)
	h := 10

	var content strings.Builder
	content.WriteString(YellowBoldStyle.Render("Restart Required"))
	content.WriteString("\n\n")
	content.WriteString("Some settings require a restart to take effect:\n")
	// Enumerates the CATEGORIES restartRequiredKeys covers. It has to keep pace
	// with that map: an operator who changed only a cookie setting and is shown
	// a list naming four things they did not touch reads this as a prompt about
	// something else and dismisses it — which is the failure the whole entry
	// exists to prevent.
	content.WriteString(DimStyle.Render("port, network access, connectivity probe targets, database path, log settings, cookie settings, sidecar settings"))
	content.WriteString("\n\n")

	content.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render("Enter: Restart now"))
	content.WriteString("  ")
	content.WriteString(DimStyle.Render("Esc: Close without restart"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorYellow).
		Width(w).
		Height(h).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}

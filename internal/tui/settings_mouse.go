package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// HandleMouse processes a mouse event in the settings panel.
func (m *SettingsModel) HandleMouse(msg tea.MouseMsg) {
	if !m.visible {
		return
	}

	// Restart overlay ignores mouse
	if m.showRestartOverlay {
		return
	}

	// Close confirmation ignores mouse
	if m.closeConfirm {
		return
	}

	mo := msg.Mouse()
	x, y := mo.X, mo.Y

	// Calculate box geometry (must match View rendering in settings_view.go).
	// View uses: Width(boxW-2).Height(h+2) — V2 total dimensions.
	// Rendered box: total width = boxW-2, total height = h+2.
	// Inner content: width = boxW-4, height = h.
	boxW := min(max(m.width-4, 40), m.width)
	h := max(m.height-2, 10)
	renderedW := boxW - 2 // V2 Width sets total
	renderedH := h + 2    // V2 Height sets total

	boxX := max((m.width-renderedW)/2, 0)
	boxY := max((m.height-renderedH)/2, 0)

	// Convert to box-relative coordinates (inside borders)
	relX := x - boxX - 1
	relY := y - boxY - 1

	// Bounds check — is the mouse inside the content area?
	innerW := boxW - 4
	innerH := h
	if relX < 0 || relX >= innerW || relY < 0 || relY >= innerH {
		return
	}

	// Scroll wheel: adjust scroll/index regardless of position
	if isScrollUp(msg) || isScrollDown(msg) {
		m.handleMouseScroll(isScrollDown(msg))
		return
	}

	if !isLeftClick(msg) {
		return
	}

	// Click on header tabs (relY == 0)
	if relY == 0 {
		m.handleMouseTabClick(relX)
		return
	}

	// Click in content area (relY >= 2, after header + divider)
	if relY >= 2 {
		m.handleMouseContentClick(relX, relY-2)
	}
}

// handleMouseScroll adjusts scroll offset or list index.
func (m *SettingsModel) handleMouseScroll(down bool) {
	sec := sections[m.sectionIndex]

	switch sec.name {
	case "Channels":
		if m.channelMode == "edit" {
			return // No scrolling in channel edit
		}
		if down {
			if m.channelIndex < len(m.channels)-1 {
				m.channelIndex++
			}
		} else {
			if m.channelIndex > 0 {
				m.channelIndex--
			}
		}
		// Moving the selection invalidates a pending delete confirm —
		// keyboard nav and clicks already clear it.
		m.channelDeleteConf = false
	case "Integrations":
		if m.notifMode == "edit" {
			if down {
				total := 1 + len(allNotifEvents)
				if m.notifEditFocus < total-1 {
					m.notifEditFocus++
					m.updateTextInputForField()
				}
			} else {
				if m.notifEditFocus > 0 {
					m.notifEditFocus--
					m.updateTextInputForField()
				}
			}
			return
		}
		if down {
			if m.notifIndex < len(m.notifications)-1 {
				m.notifIndex++
			}
		} else {
			if m.notifIndex > 0 {
				m.notifIndex--
			}
		}
		// See the Channels branch — confirm must not carry over.
		m.notifDeleteConf = false
	case "Network":
		if m.secMode != securityStatus {
			return // No scrolling in security editor
		}
		m.scrollFields(down, sec)
	default:
		if sec.fields != nil {
			m.scrollFields(down, sec)
		}
	}
}

// scrollFields scrolls through field sections.
func (m *SettingsModel) scrollFields(down bool, sec settingsSection) {
	if down {
		if m.fieldIndex < len(sec.fields)-1 {
			m.fieldIndex++
			m.ensureFieldVisible()
			m.updateTextInputForField()
		}
	} else {
		if m.fieldIndex > 0 {
			m.fieldIndex--
			m.ensureFieldVisible()
			m.updateTextInputForField()
		}
	}
}

// handleMouseTabClick maps a click X position to a section tab.
func (m *SettingsModel) handleMouseTabClick(relX int) {
	// The header is: "Settings ─ Section1 │ Section2 │ ..."
	// Match renderHeader layout: "Settings" (8) + " ─ " (3) = 11 cells prefix.
	pos := 11 // "Settings" + " \u2500 "

	for i, sec := range sections {
		if i > 0 {
			sepLen := 3 // " │ " — 3 cells
			if relX >= pos && relX < pos+sepLen {
				return // Clicked on separator
			}
			pos += sepLen
		}
		nameLen := len(sec.name) // Section names are ASCII
		if relX >= pos && relX < pos+nameLen {
			if i != m.sectionIndex {
				m.switchSection(i)
				m.updateTextInputForField()
			}
			return
		}
		pos += nameLen
	}
}

// handleMouseContentClick handles a click in the content area below the header.
// relX is X relative to inner content, contentY is 0-based from start of content.
func (m *SettingsModel) handleMouseContentClick(relX, contentY int) {
	sec := sections[m.sectionIndex]

	// Check for button clicks — position recorded during View() rendering.
	buttonY := m.lastButtonContentY
	if contentY == buttonY {
		m.handleMouseButtonClick(relX)
		return
	}

	switch sec.name {
	case "Channels":
		m.handleMouseChannelClick(contentY)
	case "Integrations":
		m.handleMouseNotifClick(contentY)
	case "Network":
		if m.secMode != securityStatus {
			return
		}
		m.handleMouseFieldClick(relX, contentY, sec)
	default:
		if sec.fields != nil {
			m.handleMouseFieldClick(relX, contentY, sec)
		}
	}
}

// handleMouseFieldClick handles clicking on a field row.
func (m *SettingsModel) handleMouseFieldClick(relX, contentY int, sec settingsSection) {
	// Each field is 1 line. Fields with previewFn have an extra preview line.
	// Map contentY to field index accounting for scroll offset and preview lines.
	fieldIdx := m.contentYToFieldIndex(contentY, sec)
	if fieldIdx < 0 || fieldIdx >= len(sec.fields) {
		return
	}

	m.fieldIndex = fieldIdx
	m.buttonFocus = -1
	m.ensureFieldVisible()
	m.updateTextInputForField()

	// For toggle/cycle fields, determine which option was clicked
	fd := sec.fields[fieldIdx]
	labelWidth := m.maxLabelWidth(sec)
	switch fd.ftype {
	case fieldToggle:
		m.handleToggleClick(fd, relX, labelWidth)
	case fieldCycle:
		m.handleCycleClick(fd, relX, labelWidth)
	}
}

// maxLabelWidth returns the max label width for a section's fields.
func (m *SettingsModel) maxLabelWidth(sec settingsSection) int {
	maxLabel := 0
	for _, fd := range sec.fields {
		if len(fd.label) > maxLabel {
			maxLabel = len(fd.label)
		}
	}
	return maxLabel
}

// handleCycleClick determines which cycle option was clicked and sets it directly.
func (m *SettingsModel) handleCycleClick(fd fieldDef, relX int, labelWidth int) {
	// Prefix is 2 chars ("  " or "> "), then label padded to labelWidth+2, then value
	valueX := relX - 2 - labelWidth - 2
	if valueX < 0 {
		return
	}

	currentVal := m.values[fd.key]
	pos := 0
	for _, opt := range fd.options {
		// Calculate this option's visual width
		optW := len(opt) // ASCII option names
		if strings.EqualFold(opt, currentVal) {
			optW += 2 // brackets [...]
		}
		if valueX >= pos && valueX < pos+optW {
			m.values[fd.key] = opt
			m.recheckDirty()
			m.status = saveIdle
			return
		}
		pos += optW + 3 // " / " separator
	}
}

// handleToggleClick determines if Yes or No was clicked and sets it directly.
func (m *SettingsModel) handleToggleClick(fd fieldDef, relX int, labelWidth int) {
	// Prefix is 2 chars, label padded to labelWidth+2, then "Yes / No"
	valueX := relX - 2 - labelWidth - 2
	if valueX < 0 {
		return
	}
	// "Yes" is 3 chars, " / " is 3 chars, "No" is 2 chars
	if valueX < 3 {
		m.values[fd.key] = "Yes"
	} else if valueX >= 6 {
		m.values[fd.key] = "No"
	}
	m.recheckDirty()
	m.status = saveIdle
}

// handleMouseButtonClick handles clicking on action buttons.
func (m *SettingsModel) handleMouseButtonClick(relX int) {
	if m.dirty {
		// Layout: "[ Save & Return ]  [ Return Without Saving ]"
		saveLen := 17 // len("[ Save & Return ]")
		gapLen := 2
		discardLen := 25 // len("[ Return Without Saving ]")

		if relX >= 0 && relX < saveLen {
			m.saveAndClose()
			return
		}
		if relX >= saveLen+gapLen && relX < saveLen+gapLen+discardLen {
			m.discardAndClose()
			return
		}
	} else {
		// Layout: "[ Return ]"
		returnLen := 10 // len("[ Return ]")
		if relX >= 0 && relX < returnLen {
			m.discardAndClose()
			return
		}
	}
}

// contentYToFieldIndex maps a content area Y offset to a field index,
// accounting for scroll offset and preview lines.
func (m *SettingsModel) contentYToFieldIndex(contentY int, sec settingsSection) int {
	if sec.fields == nil {
		return -1
	}

	contentH := m.settingsContentHeight()
	end := min(m.scrollOffset+contentH, len(sec.fields))

	lineCount := 0
	for i := m.scrollOffset; i < end; i++ {
		// This field starts at lineCount
		if contentY == lineCount {
			return i
		}
		lineCount++ // The field line itself

		// Preview line
		if sec.fields[i].previewFn != nil {
			preview := sec.fields[i].previewFn(m.values[sec.fields[i].key])
			if preview != "" {
				if contentY == lineCount {
					return i // Click on preview line selects the field
				}
				lineCount++
			}
		}
	}
	return -1
}

// handleMouseChannelClick handles clicking in the channel list/edit view.
func (m *SettingsModel) handleMouseChannelClick(contentY int) {
	if m.channelMode == "edit" {
		// In edit mode: line 0 is title, then fields start at line 1
		fieldY := contentY - 1
		if fieldY < 0 {
			return
		}
		fields := m.visibleChannelFields()
		if fieldY >= 0 && fieldY < len(fields) {
			m.channelEditField = fieldY
			m.updateTextInputForField()

			// Toggle/cycle on click
			field := fields[fieldY]
			switch field.ftype {
			case fieldToggle:
				m.cycleChannelOption(field, 1)
			case fieldCycle:
				m.cycleChannelOption(field, 1)
			}
		}
		return
	}

	// List mode: line 0 is action bar, then channels start at line 1.
	// The list renders a window around the selection — offset by its start.
	chIdx := listWindowStart(m.channelIndex, m.settingsContentHeight()) + contentY - 1
	if contentY >= 1 && chIdx < len(m.channels) {
		m.channelIndex = chIdx
		m.channelDeleteConf = false
	}
}

// handleMouseNotifClick handles clicking in the notification list/edit view.
func (m *SettingsModel) handleMouseNotifClick(contentY int) {
	if m.notifMode == "edit" {
		// Edit mode layout:
		// Line 0: title
		// Line 1: URL field
		// Line 2: empty
		// Line 3: "Events (Space to toggle):"
		// Line 4+: event groups with headers and checkboxes
		if contentY == 1 {
			m.notifEditFocus = 0
			m.updateTextInputForField()
			return
		}
		// Map event lines: we need to account for group headers and blank lines
		if contentY >= 4 {
			eventLine := contentY - 4
			m.clickNotifEvent(eventLine)
		}
		return
	}

	// List mode: line 0 is action bar, then notifications start at line 1.
	// The list renders a window around the selection — offset by its start.
	nIdx := listWindowStart(m.notifIndex, m.settingsContentHeight()) + contentY - 1
	if contentY >= 1 && nIdx < len(m.notifications) {
		m.notifIndex = nIdx
		m.notifDeleteConf = false
	}
}

// clickNotifEvent maps a line offset in the event area to a specific event
// and toggles its selection + sets focus.
func (m *SettingsModel) clickNotifEvent(eventLine int) {
	// The event area has groups. Each group has:
	// - blank line
	// - group header line
	// - event lines (one per event)
	line := 0
	flatIdx := 0
	for _, group := range notifEventGroups {
		// Blank line before group
		if eventLine == line {
			return // Clicked on blank line
		}
		line++
		// Group header
		if eventLine == line {
			return // Clicked on group header
		}
		line++
		// Events in this group
		for _, event := range group.events {
			if eventLine == line {
				m.notifEditFocus = flatIdx + 1
				m.notifEditEvents[event] = !m.notifEditEvents[event]
				m.updateTextInputForField()
				return
			}
			line++
			flatIdx++
		}
	}
}

package tui

import (
	"maps"
	"strings"
)

// HandleKey processes key input in the settings panel.
func (m *SettingsModel) HandleKey(key string) (action string) {
	// Restart overlay
	if m.showRestartOverlay {
		switch key {
		case keyEnter:
			m.Close()
			return "restart"
		case keyEsc:
			m.showRestartOverlay = false
			m.Close()
			return "close"
		}
		return ""
	}

	// Close confirmation prompt
	if m.closeConfirm {
		switch key {
		case "y", "Y":
			m.closeConfirm = false
			return m.saveAndClose()
		case "n", "N":
			m.closeConfirm = false
			return m.discardAndClose()
		case keyEsc:
			m.closeConfirm = false
		}
		return ""
	}

	// Clear error on meaningful input (not pure navigation)
	if m.status == saveError {
		switch key {
		case keyUp, keyDown, keyLeft, keyRight, keyTab, "shift+tab", "shift+left", "shift+right":
			// Navigation keys — don't clear error
		default:
			m.status = saveIdle
			m.errorMsg = ""
		}
	}

	sec := sections[m.sectionIndex]

	// Route to sub-editors
	switch sec.name {
	case "Channels":
		return m.handleChannelKey(key)
	case "Integrations":
		return m.handleNotifKey(key)
	case "Network":
		// Security sub-editor is embedded in Network section
		if m.secMode != securityStatus {
			return m.handleSecurityKey(key)
		}
		// Backtick/tilde for security mode — skip intercept when editing a text/number field
		field := sec.fields[m.fieldIndex]
		if (key == "`" || key == "~") && (field.ftype == fieldText || field.ftype == fieldNumber) {
			return m.handleFieldKey(key)
		}
		if key == "`" {
			m.secMode = securitySet
			m.secCurrentPw = ""
			m.secNewPw = ""
			m.secConfirmPw = ""
			m.secFieldIndex = 0
			m.secMessage = ""
			m.updateTextInputForField()
			return ""
		}
		if key == "~" && m.hasPassword() {
			m.secMode = securityRemove
			m.secRemovePw = ""
			m.secMessage = ""
			m.updateTextInputForField()
			return ""
		}
		return m.handleFieldKey(key)
	}

	// Paths section: "I" shortcut to open FFmpeg installer (only on ffmpeg_path field).
	// The "i" key is suppressed in UpdateComponents so it doesn't reach the text input.
	if sec.name == "Paths" && sec.fields[m.fieldIndex].key == "ffmpeg_path" && key == "i" {
		return "open_ffmpeg"
	}

	// Field section handling (inline editing - no separate edit mode)
	return m.handleFieldKey(key)
}

func (m *SettingsModel) handleFieldKey(key string) string {
	sec := sections[m.sectionIndex]
	if sec.fields == nil {
		return ""
	}

	// Button focus mode
	if m.buttonFocus >= 0 {
		return m.handleButtonKey(key)
	}

	field := sec.fields[m.fieldIndex]

	switch key {
	case keyEsc:
		return m.handleClose()
	case keyUp:
		if m.fieldIndex > 0 {
			m.fieldIndex--
			m.ensureFieldVisible()
			m.updateTextInputForField()
		}
		return ""
	case keyDown:
		if m.fieldIndex < len(sec.fields)-1 {
			m.fieldIndex++
			m.ensureFieldVisible()
			m.updateTextInputForField()
		}
		return ""
	case "shift+down":
		// Jump to action buttons from any field
		m.buttonFocus = 0
		m.textInput.Blur()
		return ""
	case keyLeft:
		switch field.ftype {
		case fieldToggle:
			m.toggleField(field)
		case fieldCycle:
			m.cycleFieldReverse(field)
		}
		return ""
	case keyRight:
		switch field.ftype {
		case fieldToggle:
			m.toggleField(field)
		case fieldCycle:
			m.cycleFieldForward(field)
		}
		return ""
	case "shift+left":
		if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
			m.updateTextInputForField()
		}
		return ""
	case "shift+right":
		if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
			m.updateTextInputForField()
		}
		return ""
	case keyTab:
		m.switchSection((m.sectionIndex + 1) % len(sections))
		m.updateTextInputForField()
		return ""
	}
	return ""
}

// handleButtonKey processes key input when action buttons are focused.
func (m *SettingsModel) handleButtonKey(key string) string {
	switch key {
	case keyEsc:
		return m.handleClose()
	case "shift+up":
		// Return to fields
		m.buttonFocus = -1
		m.updateTextInputForField()
		return ""
	case keyUp:
		// Return to fields
		m.buttonFocus = -1
		m.updateTextInputForField()
		return ""
	case keyLeft:
		if m.dirty && m.buttonFocus > 0 {
			m.buttonFocus--
		}
		return ""
	case keyRight:
		if m.dirty && m.buttonFocus < 1 {
			m.buttonFocus++
		}
		return ""
	case keyEnter:
		if !m.dirty {
			// Single "Return" button — just close
			return m.discardAndClose()
		}
		if m.buttonFocus == 0 {
			return m.saveAndClose()
		}
		return m.discardAndClose()
	case keyTab:
		m.buttonFocus = -1
		m.switchSection((m.sectionIndex + 1) % len(sections))
		m.updateTextInputForField()
		return ""
	case "shift+left":
		m.buttonFocus = -1
		if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
			m.updateTextInputForField()
		}
		return ""
	case "shift+right":
		m.buttonFocus = -1
		if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
			m.updateTextInputForField()
		}
		return ""
	}
	return ""
}

func (m *SettingsModel) handleClose() string {
	if m.dirty && m.status != saveError {
		m.closeConfirm = true
		return ""
	}
	m.Close()
	return "close"
}

// saveAndClose applies changes, saves config, and closes.
func (m *SettingsModel) saveAndClose() string {
	if m.dirty && m.status != saveError {
		m.applyValues()
		if m.status == saveError {
			return "" // Validation failed, show error
		}
		if m.OnSave != nil {
			m.OnSave(m.cfg)
		}
		m.status = saveSaved
		m.dirty = false
		m.structDirty = false
		needsRestart := m.hasRestartChanges()
		m.originalValues = make(map[string]string, len(m.values))
		maps.Copy(m.originalValues, m.values)
		if needsRestart {
			// Surface a persistent banner so dismissing the modal with
			// Esc still leaves a visual reminder that the on-disk config
			// no longer matches the running process. Audit reports/tui.md
			// #26.
			if m.OnRestartRequired != nil {
				m.OnRestartRequired()
			}
			m.showRestartOverlay = true
			return ""
		}
	}
	m.Close()
	return "close"
}

// discardAndClose resets values to originals and closes without saving.
func (m *SettingsModel) discardAndClose() string {
	maps.Copy(m.values, m.originalValues)
	m.dirty = false
	m.Close()
	return "close"
}

func (m *SettingsModel) switchSection(idx int) {
	if idx >= 0 && idx < len(sections) {
		m.sectionIndex = idx
		m.fieldIndex = 0
		m.scrollOffset = 0
		m.buttonFocus = -1
	}
}

func (m *SettingsModel) toggleField(fd fieldDef) {
	cur := m.values[fd.key]
	if cur == "Yes" {
		m.values[fd.key] = "No"
	} else {
		m.values[fd.key] = "Yes"
	}
	m.recheckDirty()
	m.status = saveIdle
}

func (m *SettingsModel) cycleFieldForward(fd fieldDef) {
	cur := m.values[fd.key]
	for i, opt := range fd.options {
		if strings.EqualFold(opt, cur) {
			m.values[fd.key] = fd.options[(i+1)%len(fd.options)]
			m.recheckDirty()
			m.status = saveIdle
			return
		}
	}
	if len(fd.options) > 0 {
		m.values[fd.key] = fd.options[0]
		m.recheckDirty()
		m.status = saveIdle
	}
}

func (m *SettingsModel) cycleFieldReverse(fd fieldDef) {
	cur := m.values[fd.key]
	for i, opt := range fd.options {
		if strings.EqualFold(opt, cur) {
			idx := (i - 1 + len(fd.options)) % len(fd.options)
			m.values[fd.key] = fd.options[idx]
			m.recheckDirty()
			m.status = saveIdle
			return
		}
	}
	if len(fd.options) > 0 {
		m.values[fd.key] = fd.options[len(fd.options)-1]
		m.recheckDirty()
		m.status = saveIdle
	}
}

func (m *SettingsModel) ensureFieldVisible() {
	contentH := m.settingsContentHeight()
	if m.fieldIndex < m.scrollOffset {
		m.scrollOffset = m.fieldIndex
	}
	if m.fieldIndex >= m.scrollOffset+contentH {
		m.scrollOffset = m.fieldIndex - contentH + 1
	}
}

// settingsContentHeight returns the number of content rows the current
// section actually renders. Single source of truth shared by View()'s
// renderFields/renderChannels/renderNotifications calls, ensureFieldVisible,
// and mouse hit-testing — if the scroll-keeper assumed a taller window than
// View renders, the focused field could sit permanently off-screen.
func (m *SettingsModel) settingsContentHeight() int {
	h := max(m.height-2, 10) // matches View()'s box height
	buttonLine := 1
	if sections[m.sectionIndex].name == "Network" {
		// Network reserves 4 extra lines for the compact security block.
		return max(h-12-buttonLine, 1)
	}
	return max(h-8-buttonLine, 1)
}

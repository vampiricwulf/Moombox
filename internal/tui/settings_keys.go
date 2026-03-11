package tui

import "strings"

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

	// Field section handling (inline editing - no separate edit mode)
	return m.handleFieldKey(key)
}

func (m *SettingsModel) handleFieldKey(key string) string {
	sec := sections[m.sectionIndex]
	if sec.fields == nil {
		return ""
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
	case keyLeft:
		if field.ftype == fieldToggle {
			m.toggleField(field)
		} else if field.ftype == fieldCycle {
			m.cycleFieldReverse(field)
		}
		return ""
	case keyRight:
		if field.ftype == fieldToggle {
			m.toggleField(field)
		} else if field.ftype == fieldCycle {
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

func (m *SettingsModel) handleClose() string {
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
		needsRestart := m.hasRestartChanges()
		m.originalValues = make(map[string]string)
		for k, v := range m.values {
			m.originalValues[k] = v
		}
		if needsRestart {
			m.showRestartOverlay = true
			return ""
		}
	}
	m.Close()
	return "close"
}

func (m *SettingsModel) switchSection(idx int) {
	if idx >= 0 && idx < len(sections) {
		m.sectionIndex = idx
		m.fieldIndex = 0
		m.scrollOffset = 0
	}
}

func (m *SettingsModel) toggleField(fd fieldDef) {
	cur := m.values[fd.key]
	if cur == "Yes" {
		m.values[fd.key] = "No"
	} else {
		m.values[fd.key] = "Yes"
	}
	m.dirty = true
	m.status = saveIdle
}

func (m *SettingsModel) cycleFieldForward(fd fieldDef) {
	cur := m.values[fd.key]
	for i, opt := range fd.options {
		if strings.EqualFold(opt, cur) {
			m.values[fd.key] = fd.options[(i+1)%len(fd.options)]
			m.dirty = true
			m.status = saveIdle
			return
		}
	}
	if len(fd.options) > 0 {
		m.values[fd.key] = fd.options[0]
		m.dirty = true
		m.status = saveIdle
	}
}

func (m *SettingsModel) cycleFieldReverse(fd fieldDef) {
	cur := m.values[fd.key]
	for i, opt := range fd.options {
		if strings.EqualFold(opt, cur) {
			idx := (i - 1 + len(fd.options)) % len(fd.options)
			m.values[fd.key] = fd.options[idx]
			m.dirty = true
			m.status = saveIdle
			return
		}
	}
	if len(fd.options) > 0 {
		m.values[fd.key] = fd.options[len(fd.options)-1]
		m.dirty = true
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

func (m *SettingsModel) settingsContentHeight() int {
	h := m.height - 10 // borders, header, tabs, status, footer
	if h < 5 {
		h = 5
	}
	return h
}

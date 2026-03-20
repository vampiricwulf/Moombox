package tui

// --- Security sub-editor ---

func (m *SettingsModel) handleSecurityKey(key string) string {
	switch m.secMode {
	case securitySet:
		return m.handleSecuritySetKey(key)
	case securityRemove:
		return m.handleSecurityRemoveKey(key)
	}
	return ""
}

func (m *SettingsModel) hasPassword() bool {
	if m.cfg == nil {
		return false
	}
	if m.cfgMu != nil {
		m.cfgMu.RLock()
	}
	hash := m.cfg.Network.PasswordHash
	if m.cfgMu != nil {
		m.cfgMu.RUnlock()
	}
	return hash != ""
}

func (m *SettingsModel) handleSecuritySetKey(key string) string {
	fieldCount := 2
	if m.hasPassword() {
		fieldCount = 3
	}

	switch key {
	case keyEsc:
		m.secMode = securityStatus
		m.updateTextInputForField()
		return ""
	case keyEnter:
		m.handleSetPassword()
		return ""
	case keyUp, "shift+tab":
		if m.secFieldIndex > 0 {
			m.secFieldIndex--
			m.updateTextInputForField()
		}
		return ""
	case keyDown, keyTab:
		if m.secFieldIndex < fieldCount-1 {
			m.secFieldIndex++
			m.updateTextInputForField()
		}
		return ""
	}
	return ""
}

func (m *SettingsModel) secActiveField() *string {
	if m.hasPassword() {
		switch m.secFieldIndex {
		case 0:
			return &m.secCurrentPw
		case 1:
			return &m.secNewPw
		case 2:
			return &m.secConfirmPw
		}
	} else {
		switch m.secFieldIndex {
		case 0:
			return &m.secNewPw
		case 1:
			return &m.secConfirmPw
		}
	}
	return nil
}

func (m *SettingsModel) handleSetPassword() {
	// Verify current password if exists
	if m.hasPassword() {
		if m.secCurrentPw == "" {
			m.secMessage = "Current password required"
			m.secMessageColor = ColorRed
			return
		}
		if m.cfgMu != nil {
			m.cfgMu.RLock()
		}
		currentHash := m.cfg.Network.PasswordHash
		if m.cfgMu != nil {
			m.cfgMu.RUnlock()
		}
		if m.OnVerifyPassword != nil && !m.OnVerifyPassword(m.secCurrentPw, currentHash) {
			m.secMessage = "Current password is incorrect"
			m.secMessageColor = ColorRed
			return
		}
	}

	if m.secNewPw == "" {
		m.secMessage = "New password required"
		m.secMessageColor = ColorRed
		return
	}
	if m.secNewPw != m.secConfirmPw {
		m.secMessage = "Passwords do not match"
		m.secMessageColor = ColorRed
		return
	}

	// Hash and save
	if m.OnHashPassword != nil {
		hash := m.OnHashPassword(m.secNewPw)
		if hash == "" {
			m.secMessage = "Failed to hash password"
			m.secMessageColor = ColorRed
			return
		}
		if m.cfgMu != nil {
			m.cfgMu.Lock()
		}
		m.cfg.Network.PasswordHash = hash
		if m.cfgMu != nil {
			m.cfgMu.Unlock()
		}
		if m.OnSave != nil {
			m.OnSave(m.cfg)
		}
		m.secMessage = "Password set successfully"
		m.secMessageColor = ColorGreen
		m.secMode = securityStatus
	} else {
		m.secMessage = "Password hashing not available"
		m.secMessageColor = ColorRed
	}
}

func (m *SettingsModel) handleSecurityRemoveKey(key string) string {
	switch key {
	case keyEsc:
		m.secMode = securityStatus
		m.updateTextInputForField()
		return ""
	case keyEnter:
		m.handleRemovePassword()
		return ""
	}
	return ""
}

func (m *SettingsModel) handleRemovePassword() {
	if !m.hasPassword() {
		m.secMessage = "No password is set"
		m.secMessageColor = ColorRed
		return
	}

	if m.cfgMu != nil {
		m.cfgMu.RLock()
	}
	pwHash := m.cfg.Network.PasswordHash
	if m.cfgMu != nil {
		m.cfgMu.RUnlock()
	}
	if m.OnVerifyPassword != nil && !m.OnVerifyPassword(m.secRemovePw, pwHash) {
		m.secMessage = "Current password is incorrect"
		m.secMessageColor = ColorRed
		return
	}

	// Remove password
	if m.cfgMu != nil {
		m.cfgMu.Lock()
	}
	networkReset := m.cfg.Network.NetworkAccess == "external"
	m.cfg.Network.PasswordHash = ""
	if networkReset {
		m.cfg.Network.NetworkAccess = "localhost"
	}
	if m.cfgMu != nil {
		m.cfgMu.Unlock()
	}
	if networkReset {
		m.values["network_access"] = "localhost"
		m.dirty = true
		m.structDirty = true
	}
	if m.OnSave != nil {
		m.OnSave(m.cfg)
	}
	if networkReset {
		m.secMessage = "Password removed, network access reset to localhost"
	} else {
		m.secMessage = "Password removed"
	}
	m.secMessageColor = ColorGreen
	m.secMode = securityStatus
}

package tui

import "github.com/vampiricwulf/Moombox/internal/config"

// --- Security sub-editor ---

// isExternalAccess reports whether a network_access value exposes the
// dashboard beyond the local network. "external" and "public" are documented
// synonyms — "public" marks a deployment sitting behind an authenticating
// reverse proxy — and every runtime consumer treats them identically. Config
// validation used to normalise a hand-edited "public" back to "localhost" at
// load, which hid the alias from guards that only checked "external"; it is a
// persistable value now, so every passwordless-external guard goes through
// here rather than re-spelling the pair.
func isExternalAccess(access string) bool {
	return access == "external" || access == "public"
}

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
	if m.configStore == nil {
		return false
	}
	var hash string
	m.configStore.Read(func(c *config.MoomboxConfig) {
		hash = c.Network.PasswordHash
	})
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
		var currentHash string
		m.configStore.Read(func(c *config.MoomboxConfig) {
			currentHash = c.Network.PasswordHash
		})
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
		// Snapshot before the write for the same reason saveAndClose does:
		// a refused save would otherwise leave the process authenticating
		// against a hash config.toml does not have (CORE-4).
		snapshot := m.snapshotConfig()
		mu := m.configStore.RWMutex()
		mu.Lock()
		m.cfg.Network.PasswordHash = hash
		mu.Unlock()
		if m.OnSave != nil {
			if err := m.OnSave(m.cfg); err != nil {
				m.restoreConfig(snapshot)
				m.secMessage = "Save failed: " + err.Error()
				m.secMessageColor = ColorRed
				return
			}
		}
		if m.OnSecurityChanged != nil {
			m.OnSecurityChanged()
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

	var pwHash string
	m.configStore.Read(func(c *config.MoomboxConfig) {
		pwHash = c.Network.PasswordHash
	})
	if m.OnVerifyPassword != nil && !m.OnVerifyPassword(m.secRemovePw, pwHash) {
		m.secMessage = "Current password is incorrect"
		m.secMessageColor = ColorRed
		return
	}

	// Remove password
	snapshot := m.snapshotConfig()
	prevDirty, prevStructDirty := m.dirty, m.structDirty
	mu := m.configStore.RWMutex()
	mu.Lock()
	// Removing the password while external/public would leave the dashboard
	// open to every reachable IP — the exact state block-set exists to
	// prevent — so drop back to localhost in the same write.
	networkReset := isExternalAccess(m.cfg.Network.NetworkAccess)
	m.cfg.Network.PasswordHash = ""
	if networkReset {
		m.cfg.Network.NetworkAccess = "localhost"
	}
	mu.Unlock()
	if networkReset {
		m.values["network_access"] = "localhost"
		m.dirty = true
		m.structDirty = true
	}
	if m.OnSave != nil {
		if err := m.OnSave(m.cfg); err != nil {
			// Roll the whole commit back — the cleared hash, the access
			// reset that rode along with it, and the panel's own field map
			// (which the next save would otherwise write through).
			m.restoreConfig(snapshot)
			if networkReset {
				m.values["network_access"] = snapshot.Network.NetworkAccess
				m.dirty, m.structDirty = prevDirty, prevStructDirty
			}
			m.secMessage = "Save failed: " + err.Error()
			m.secMessageColor = ColorRed
			return
		}
	}
	if m.OnSecurityChanged != nil {
		m.OnSecurityChanged()
	}
	if networkReset {
		// The reset is on disk now, so it is the field's saved value, not an
		// unsaved edit: left out of originalValues it showed "Unsaved
		// changes" and a "*" on Network access, and Esc offered to save what
		// was already saved. The form's own dirty state is what it was.
		m.originalValues["network_access"] = "localhost"
		m.structDirty = prevStructDirty
		m.recheckDirty()
		// network_access is restart-required (the listener's bind address
		// is read at boot): say so the way a save of it does.
		if m.OnRestartRequired != nil {
			m.OnRestartRequired()
		}
		m.showRestartOverlay = true
		m.secMessage = "Password removed, network access reset to localhost"
	} else {
		m.secMessage = "Password removed"
	}
	m.secMessageColor = ColorGreen
	m.secMode = securityStatus
}

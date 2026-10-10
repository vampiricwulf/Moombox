package tui

import (
	"maps"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
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
			return m.thenAfterClose(m.saveAndClose())
		case "n", "N":
			m.closeConfirm = false
			return m.thenAfterClose(m.discardAndClose())
		case keyEsc:
			m.closeConfirm = false
			m.afterClose = ""
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

	// Paths section: Ctrl+O opens the FFmpeg installer (only on the
	// ffmpeg_path field). A control key, not a letter: the field is edited
	// inline, and the old "i" binding made every path with an i in it —
	// /usr/local/bin/ffmpeg, C:\ffmpeg\bin\ffmpeg.exe — impossible to type.
	if sec.name == "Paths" && sec.fields[m.fieldIndex].key == "ffmpeg_path" && key == keyCtrlO {
		// The installer replaces this panel, and closing it silently threw
		// every unsaved edit away — the one close path without the Save
		// changes? prompt. With edits pending it asks first, and opens the
		// installer once the answer is Save or Discard.
		if m.dirty {
			m.closeConfirm = true
			m.afterClose = "open_ffmpeg"
			return ""
		}
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
		case fieldNumber:
			// A number row that declares options is a picker: the arrows step
			// the presets, typing still enters anything else. The text input
			// has to follow, or the next keystroke syncs its stale text back
			// over the value we just set.
			if len(field.options) > 0 {
				cycleNumberPreset(m.values, field.key, field.options, -1)
				m.recheckDirty()
				m.status = saveIdle
				m.updateTextInputForField()
			}
		}
		return ""
	case keyRight:
		switch field.ftype {
		case fieldToggle:
			m.toggleField(field)
		case fieldCycle:
			m.cycleFieldForward(field)
		case fieldNumber:
			if len(field.options) > 0 {
				cycleNumberPreset(m.values, field.key, field.options, 1)
				m.recheckDirty()
				m.status = saveIdle
				m.updateTextInputForField()
			}
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

// thenAfterClose swaps a completed prompted close for the action that asked
// for the prompt (afterClose). A save that failed or needs a restart returns
// "" and the pending action is dropped with it.
func (m *SettingsModel) thenAfterClose(action string) string {
	after := m.afterClose
	m.afterClose = ""
	if action == "close" && after != "" {
		return after
	}
	return action
}

func (m *SettingsModel) handleClose() string {
	if m.dirty && m.status != saveError {
		m.closeConfirm = true
		return ""
	}
	m.Close()
	return "close"
}

// snapshotConfig copies the live config under the store's read lock, for the
// callers that mutate it before a save that can be refused. (applyValues
// takes the same copy itself, under the write lock its writes hold:
// settingsWrite.before.)
//
// The copy is SHALLOW — the same shape config.Store.Update's own rollback
// takes, for the same reason: every writer here REPLACES the slice fields it
// touches (Channels, Notifications, TrustedProxies, ProbeTargets,
// ActivePlatforms) rather than mutating elements in place, so the headers in
// the copy still point at the pre-save backing arrays. A writer that ever
// needs to mutate a slice element in place must deep-copy it first.
func (m *SettingsModel) snapshotConfig() config.MoomboxConfig {
	if m.cfg == nil {
		return config.MoomboxConfig{}
	}
	if m.configStore != nil {
		mu := m.configStore.RWMutex()
		mu.RLock()
		defer mu.RUnlock()
		return *m.cfg
	}
	return *m.cfg
}

// restoreConfig puts the live config back to a snapshotConfig copy, under the
// store's write lock so a concurrent reader never observes the half-rolled
// struct.
//
// It restores the WHOLE struct rather than the fields the caller happens to
// have typed, deliberately: a hand-maintained undo list would drift the
// first time a field was added to writeSettingsField and not to it.
//
// Neither list aliases the model's own: mergeChannelEdits and
// mergeNotificationEdits build new slices (the latter copying each target it
// writes), and a save re-copies the editors' lists from the saved ones
// (resyncFromLive), so the notification editor's in-place writes never reach
// the live config or a snapshot.
//
// Known window: a background writer (cookie refresh, a Web PUT) can commit
// through config.Store.Update between the snapshot — applyValues takes it
// under the same lock as its writes — and this restore; the whole-struct
// write then reverts that change in memory while it is already on disk.
// Closing it would mean holding the store's write lock across applyValues →
// OnSave → restore, which deadlocks: OnSaveConfig takes that same lock
// (around config.Save, then Snapshot's RLock). It is the identical caveat
// Store.Update's own rollback documents, and the window is one refused save
// concurrent with a background Update.
func (m *SettingsModel) restoreConfig(snapshot config.MoomboxConfig) {
	if m.cfg == nil {
		return
	}
	if m.configStore != nil {
		mu := m.configStore.RWMutex()
		mu.Lock()
		*m.cfg = snapshot
		mu.Unlock()
		return
	}
	*m.cfg = snapshot
}

// saveAndClose applies changes, saves config, and closes.
//
// Only what the overlay changed is written (applyValues), and a save whose
// changes leave the live config as it was — a field typed to what the
// dashboard already saved, an Enter on a target that changed nothing, a
// removal the dashboard made first — writes nothing: OnSave is not called,
// so config.toml is not rewritten and nothing is hot-reloaded. Such a save
// can still ask for the restart: a restart-required field typed away from
// its Open value is on disk at a value the process was not started with,
// whoever wrote it there (settingsWrite.restart).
func (m *SettingsModel) saveAndClose() string {
	if m.dirty && m.status != saveError {
		// applyValues writes straight into the live *MoomboxConfig the store
		// holds (Open stores the store's own pointer), and hands back the
		// config as its write found it: without that a refused save leaves
		// the running process on values that are not on disk while the
		// overlay says "Saved" (CORE-4).
		w, ok := m.applyValues()
		if !ok {
			return "" // Validation failed, show error; nothing was written
		}
		if w.changed && m.OnSave != nil {
			if err := m.OnSave(m.cfg); err != nil {
				m.restoreConfig(w.before)
				m.errorMsg = "Save failed: " + err.Error()
				m.status = saveError
				// dirty stays set: the typed values are still in m.values,
				// so the user can fix the cause and press Save again.
				return ""
			}
		}
		m.status = saveSaved
		m.dirty = false
		m.structDirty = false
		m.handOverChannelPrunes()
		m.resyncFromLive()
		if w.restart {
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
	// renderFields emits a SECOND line under every field that carries a
	// previewFn, so a window measured in field units overruns the box by the
	// number of preview rows the section can show. Reserve them here rather
	// than clipping inside renderFields: a clip would draw fewer fields than
	// ensureFieldVisible budgeted for, and the focused row at the foot of the
	// window would simply vanish. The count is the section's previewFn fields,
	// not the ones currently rendering a non-empty preview, so the window does
	// not resize under the operator as they type.
	previewRows := 0
	for i := range sections[m.sectionIndex].fields {
		if sections[m.sectionIndex].fields[i].previewFn != nil {
			previewRows++
		}
	}
	// The focused field's help line is one of the eight rows below; a long
	// one wraps (up to ~170 characters of help at 50-odd columns), and those
	// extra rows used to push the header off the top of the screen.
	infoRows := fieldInfoRows(sections[m.sectionIndex], m.settingsInnerWidth()) - 1
	if sections[m.sectionIndex].name == "Network" {
		// Network reserves 4 extra lines for the compact security block.
		return max(h-12-buttonLine-previewRows-infoRows, 1)
	}
	return max(h-8-buttonLine-previewRows-infoRows, 1)
}

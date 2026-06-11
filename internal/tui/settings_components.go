package tui

import (
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// updateTextInputForField configures the textInput based on the current state.
func (m *SettingsModel) updateTextInputForField() {
	sec := sections[m.sectionIndex]

	// Security sub-editor
	if sec.name == "Network" && m.secMode == securitySet {
		m.textInput.EchoMode = textinput.EchoPassword
		m.textInput.Validate = nil
		target := m.secActiveField()
		if target != nil {
			m.textInput.SetValue(*target)
		}
		m.textInput.Focus()
		return
	}
	if sec.name == "Network" && m.secMode == securityRemove {
		m.textInput.EchoMode = textinput.EchoPassword
		m.textInput.Validate = nil
		m.textInput.SetValue(m.secRemovePw)
		m.textInput.Focus()
		return
	}

	// Channel edit mode
	if sec.name == "Channels" && m.channelMode == "edit" {
		fields := m.visibleChannelFields()
		if m.channelEditField < len(fields) {
			field := fields[m.channelEditField]
			if field.ftype == fieldText {
				m.textInput.EchoMode = textinput.EchoNormal
				m.textInput.Validate = nil
				m.textInput.SetValue(m.channelEditValues[field.key])
				m.textInput.Focus()
				return
			}
		}
		m.textInput.Blur()
		return
	}

	// Notification edit mode - URL field
	if sec.name == "Integrations" && m.notifMode == "edit" && m.notifEditFocus == 0 {
		m.textInput.EchoMode = textinput.EchoNormal
		m.textInput.Validate = nil
		m.textInput.SetValue(m.notifEditURL)
		m.textInput.Focus()
		return
	}

	// Normal field sections
	if sec.fields != nil {
		field := sec.fields[m.fieldIndex]
		if field.ftype == fieldText || field.ftype == fieldNumber {
			m.textInput.EchoMode = textinput.EchoNormal
			if field.ftype == fieldNumber {
				m.textInput.Validate = validateDigitsOnly
			} else {
				m.textInput.Validate = nil
			}
			m.textInput.SetValue(m.values[field.key])
			m.textInput.Focus()
			return
		}
	}

	m.textInput.Blur()
}

// UpdateComponents routes tea.Msg to the embedded textinput and syncs.
func (m *SettingsModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible || !m.textInput.Focused() {
		return nil
	}
	// While the close-confirm prompt or restart overlay is up, HandleKey
	// owns every key (Y/N/Enter/Esc) — routing them into the focused text
	// input too would append the answer to the field value before saving.
	if m.closeConfirm || m.showRestartOverlay {
		return nil
	}
	// Suppress "i" key on ffmpeg_path field — it opens the FFmpeg installer
	// and must not be typed into the text input.
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && keyMsg.String() == "i" {
		sec := sections[m.sectionIndex]
		if sec.name == "Paths" && sec.fields != nil && m.fieldIndex < len(sec.fields) &&
			sec.fields[m.fieldIndex].key == "ffmpeg_path" {
			return nil
		}
	}
	prev := m.textInput.Value()
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prev {
		m.syncFromTextInput()
	}
	return cmd
}

func (m *SettingsModel) syncFromTextInput() {
	val := m.textInput.Value()
	sec := sections[m.sectionIndex]

	// Security sub-editor
	if sec.name == "Network" && m.secMode == securitySet {
		if target := m.secActiveField(); target != nil {
			*target = val
		}
		return
	}
	if sec.name == "Network" && m.secMode == securityRemove {
		m.secRemovePw = val
		return
	}

	// Channel edit
	if sec.name == "Channels" && m.channelMode == "edit" {
		fields := m.visibleChannelFields()
		if m.channelEditField < len(fields) {
			field := fields[m.channelEditField]
			if field.ftype == fieldText {
				m.channelEditValues[field.key] = val
				if field.key == "id" {
					m.autoDetectPlatform()
				}
			}
		}
		return
	}

	// Notification edit - URL
	if sec.name == "Integrations" && m.notifMode == "edit" && m.notifEditFocus == 0 {
		m.notifEditURL = val
		return
	}

	// Normal fields
	if sec.fields != nil {
		field := sec.fields[m.fieldIndex]
		if field.ftype == fieldText || field.ftype == fieldNumber {
			m.values[field.key] = val
			m.recheckDirty()
			m.status = saveIdle
		}
	}
}

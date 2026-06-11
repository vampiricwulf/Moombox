package tui

import (
	"cmp"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// --- Channel sub-editor ---

func (m *SettingsModel) visibleChannelFields() []channelFieldDef {
	return filterChannelFieldsByPlatform(channelFields, m.channelEditValues)
}

func channelToValues(ch config.ChannelConfig) map[string]string {
	terms := ch.Terms.Simple
	enabled := "Yes"
	if ch.Enabled != nil && !*ch.Enabled {
		enabled = "No"
	}
	return map[string]string{
		"id":                 ch.ID,
		"name":               ch.Name,
		"platform":           ch.GetPlatform(),
		"enabled":            enabled,
		"terms":              terms,
		"include_non_live":   boolToDisplay(ch.IncludeNonLiveContent),
		"quality_preference": cmp.Or(ch.QualityPreference, "best"),
	}
}

func valuesToChannel(vals map[string]string) config.ChannelConfig {
	ch := config.ChannelConfig{
		ID:       strings.TrimSpace(vals["id"]),
		Name:     strings.TrimSpace(vals["name"]),
		Platform: vals["platform"],
	}
	switch vals["enabled"] {
	case "No":
		boolFalse := false
		ch.Enabled = &boolFalse
	case "Yes":
		boolTrue := true
		ch.Enabled = &boolTrue
	}
	if vals["terms"] != "" {
		ch.Terms = config.ChannelTerms{Simple: vals["terms"]}
	}
	if vals["platform"] == "youtube" && vals["include_non_live"] == "Yes" {
		ch.IncludeNonLiveContent = true
	}
	if vals["quality_preference"] != "" && vals["quality_preference"] != "best" {
		ch.QualityPreference = vals["quality_preference"]
	}
	return ch
}

func (m *SettingsModel) handleChannelKey(key string) string {
	if m.channelMode == "edit" {
		return m.handleChannelEditKey(key)
	}

	// List mode
	if m.channelDeleteConf {
		if key == "d" || key == "D" {
			if m.channelIndex < len(m.channels) {
				m.channels = append(m.channels[:m.channelIndex], m.channels[m.channelIndex+1:]...)
				if m.channelIndex >= len(m.channels) && m.channelIndex > 0 {
					m.channelIndex--
				}
				m.dirty = true
				m.structDirty = true
			}
			m.channelDeleteConf = false
		} else {
			m.channelDeleteConf = false
		}
		return ""
	}

	switch key {
	case keyEsc:
		return m.handleClose()
	case keyUp:
		if m.channelIndex > 0 {
			m.channelIndex--
		}
	case keyDown:
		if m.channelIndex < len(m.channels)-1 {
			m.channelIndex++
		}
	case keyEnter:
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			m.channelMode = "edit"
			m.channelEditValues = channelToValues(m.channels[m.channelIndex])
			m.channelEditField = 0
			m.updateTextInputForField()
		}
	case "a", "A":
		m.channelEditValues = map[string]string{
			"id": "", "name": "", "platform": "youtube",
			"enabled": "Yes", "terms": "",
			"include_non_live": "No", "quality_preference": "best",
		}
		m.channelEditField = 0
		m.channelIndex = len(m.channels) // Will be new index
		m.channelMode = "edit"
		m.updateTextInputForField()
	case "d", "D":
		if len(m.channels) > 0 {
			m.channelDeleteConf = true
		}
	case keyTab:
		m.switchSection((m.sectionIndex + 1) % len(sections))
		m.updateTextInputForField()
	case "shift+left":
		if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
			m.updateTextInputForField()
		}
	case "shift+right":
		if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
			m.updateTextInputForField()
		}
	}
	return ""
}

func (m *SettingsModel) handleChannelEditKey(key string) string {
	fields := m.visibleChannelFields()
	if len(fields) == 0 {
		return ""
	}
	if m.channelEditField >= len(fields) {
		m.channelEditField = len(fields) - 1
	}
	field := fields[m.channelEditField]

	switch key {
	case keyEsc:
		m.channelMode = "list"
		// "a" set channelIndex = len(channels) for the pending add — clamp
		// it back or list-mode Enter indexes out of range (mirrors the
		// setup wizard's handleChannelEditKey Esc).
		if m.channelIndex >= len(m.channels) && len(m.channels) > 0 {
			m.channelIndex = len(m.channels) - 1
		} else if len(m.channels) == 0 {
			m.channelIndex = 0
		}
		m.channelResolving = false
		m.textInput.Blur()
		return ""
	case keyEnter:
		if m.channelResolving {
			return ""
		}
		id := strings.TrimSpace(m.channelEditValues["id"])
		if id == "" {
			m.errorMsg = "Channel ID is required"
			return ""
		}
		if strings.Contains(id, "youtube.com/") || strings.Contains(id, "youtu.be/") || strings.Contains(id, "twitch.tv/") {
			m.channelResolving = true
			return "resolve_channel"
		}
		m.saveCurrentChannel()
		return ""
	case keyUp:
		if m.channelEditField > 0 {
			m.channelEditField--
			m.updateTextInputForField()
		}
		return ""
	case keyDown:
		if m.channelEditField < len(fields)-1 {
			m.channelEditField++
			m.updateTextInputForField()
		}
		return ""
	case keyLeft:
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelOption(field, -1)
		}
		return ""
	case keyRight:
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelOption(field, 1)
		}
		return ""
	}
	return ""
}

// autoDetectPlatform checks the ID field value and auto-switches the platform if it contains a known domain.
func (m *SettingsModel) autoDetectPlatform() {
	id := m.channelEditValues["id"]
	if strings.Contains(id, "youtube.com/") || strings.Contains(id, "youtu.be/") {
		m.channelEditValues["platform"] = "youtube"
	} else if strings.Contains(id, "twitch.tv/") {
		m.channelEditValues["platform"] = "twitch"
	}
}

// saveCurrentChannel saves the current channel edit values to the channel list.
func (m *SettingsModel) saveCurrentChannel() {
	ch := valuesToChannel(m.channelEditValues)
	if m.channelIndex < len(m.channels) {
		m.channels[m.channelIndex] = ch
	} else {
		m.channels = append(m.channels, ch)
	}
	m.dirty = true
	// structDirty keeps recheckDirty from clearing the dirty flag — channel
	// edits aren't reflected in m.values, so a value-level recheck would
	// otherwise silently discard the add/edit on close.
	m.structDirty = true
	m.status = saveIdle
	m.channelMode = "list"
}

// GetChannelResolveInput returns the current channel ID being resolved.
func (m *SettingsModel) GetChannelResolveInput() string {
	if m.channelEditValues == nil {
		return ""
	}
	return strings.TrimSpace(m.channelEditValues["id"])
}

// HandleChannelResolved processes the result of an async channel resolution.
// If the user cancelled (Esc) before the result arrived, the result is silently discarded.
func (m *SettingsModel) HandleChannelResolved(id, name, platform string, err error) {
	if !m.channelResolving {
		return // User cancelled or navigated away — discard stale result
	}
	m.channelResolving = false
	if err != nil {
		m.errorMsg = "Resolve failed: " + err.Error()
		m.status = saveError
		return
	}
	if id != "" {
		m.channelEditValues["id"] = id
	}
	if name != "" && m.channelEditValues["name"] == "" {
		m.channelEditValues["name"] = name
	}
	if platform != "" {
		m.channelEditValues["platform"] = platform
	}
	m.saveCurrentChannel()
}

func (m *SettingsModel) cycleChannelOption(field channelFieldDef, direction int) {
	cycleFieldOption(m.channelEditValues, field.key, field.options, direction)
}

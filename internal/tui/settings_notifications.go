package tui

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// --- Notification sub-editor ---

func (m *SettingsModel) handleNotifKey(key string) string {
	if m.notifMode == "edit" {
		return m.handleNotifEditKey(key)
	}

	// List mode
	if m.notifDeleteConf {
		if key == "d" || key == "D" {
			if m.notifIndex < len(m.notifications) {
				m.notifications = append(m.notifications[:m.notifIndex], m.notifications[m.notifIndex+1:]...)
				if m.notifIndex >= len(m.notifications) && m.notifIndex > 0 {
					m.notifIndex--
				}
				m.dirty = true
				m.structDirty = true
			}
			m.notifDeleteConf = false
		} else {
			m.notifDeleteConf = false
		}
		return ""
	}

	switch key {
	case keyEsc:
		return m.handleClose()
	case keyUp:
		if m.notifIndex > 0 {
			m.notifIndex--
		}
	case keyDown:
		if m.notifIndex < len(m.notifications)-1 {
			m.notifIndex++
		}
	case keyEnter:
		if len(m.notifications) > 0 && m.notifIndex < len(m.notifications) {
			n := m.notifications[m.notifIndex]
			m.notifEditURL = n.URL
			m.notifEditEvents = make(map[string]bool)
			if len(n.Events) == 0 {
				// All events
				for _, e := range allNotifEvents {
					m.notifEditEvents[e] = true
				}
			} else {
				for _, e := range n.Events {
					m.notifEditEvents[e] = true
				}
			}
			m.notifEditFocus = 0
			m.notifMode = "edit"
			m.updateTextInputForField()
		}
	case "a", "A":
		m.notifEditURL = ""
		m.notifEditEvents = make(map[string]bool)
		for _, e := range allNotifEvents {
			m.notifEditEvents[e] = true
		}
		m.notifEditFocus = 0
		m.notifIndex = len(m.notifications)
		m.notifMode = "edit"
		m.updateTextInputForField()
	case "d", "D":
		if len(m.notifications) > 0 {
			m.notifDeleteConf = true
		}
	case "t", "T":
		// Async test-send of the highlighted target's URL — the app layer
		// dispatches the HTTP call (testNotificationCmd) and routes the
		// result back via SetNotifTestResult.
		if len(m.notifications) > 0 && m.notifIndex < len(m.notifications) {
			m.errorMsg = "Sending test notification..."
			m.status = saveNotice
			return "test_notification"
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

// SelectedNotificationURL returns the URL of the highlighted notification
// target in list mode, or "" when none is selected. Used by the app layer
// to dispatch the async test-send.
func (m *SettingsModel) SelectedNotificationURL() string {
	if m.notifIndex < len(m.notifications) {
		return m.notifications[m.notifIndex].URL
	}
	return ""
}

// SetNotifTestResult surfaces an async test-notification outcome in the
// settings status line (the settings overlay covers the main feedback
// line, so the result must render inside the overlay).
func (m *SettingsModel) SetNotifTestResult(errMsg string) {
	if errMsg == "" {
		m.errorMsg = "Test notification delivered ✓"
		m.status = saveNotice
		return
	}
	m.errorMsg = "Test failed: " + errMsg
	m.status = saveError
}

func (m *SettingsModel) handleNotifEditKey(key string) string {
	totalItems := 1 + len(allNotifEvents)

	switch key {
	case keyEsc:
		m.notifMode = "list"
		// "a" set notifIndex = len(notifications) for the pending add —
		// clamp it back or list-mode Enter indexes out of range (mirrors
		// the setup wizard's channel-edit Esc clamp).
		if m.notifIndex >= len(m.notifications) && len(m.notifications) > 0 {
			m.notifIndex = len(m.notifications) - 1
		} else if len(m.notifications) == 0 {
			m.notifIndex = 0
		}
		m.textInput.Blur()
		return ""
	case keyEnter:
		if strings.TrimSpace(m.notifEditURL) == "" {
			return ""
		}
		// Save-time URL validation — a broken paste previously round-tripped
		// to config with no error and was silently warn-skipped at startup.
		if err := notifications.ValidateURL(strings.TrimSpace(m.notifEditURL)); err != nil {
			m.errorMsg = err.Error()
			m.status = saveError
			return ""
		}
		var events []string
		enabledCount := 0
		for _, e := range allNotifEvents {
			if m.notifEditEvents[e] {
				enabledCount++
				events = append(events, e)
			}
		}
		// Zero selected events would store Events = nil, which the
		// notifications manager treats as "all events" — reject instead.
		if enabledCount == 0 {
			m.errorMsg = "Select at least one event (Space to toggle)"
			m.status = saveError
			return ""
		}
		n := config.NotificationConfig{URL: strings.TrimSpace(m.notifEditURL)}
		if enabledCount < len(allNotifEvents) {
			n.Events = events
		}
		if m.notifIndex < len(m.notifications) {
			m.notifications[m.notifIndex] = n
		} else {
			m.notifications = append(m.notifications, n)
		}
		m.dirty = true
		m.structDirty = true
		m.status = saveIdle
		m.notifMode = "list"
		m.textInput.Blur()
		return ""
	case keyUp:
		if m.notifEditFocus > 0 {
			m.notifEditFocus--
			m.updateTextInputForField()
		}
		return ""
	case keyDown:
		if m.notifEditFocus < totalItems-1 {
			m.notifEditFocus++
			m.updateTextInputForField()
		}
		return ""
	case " ":
		if m.notifEditFocus > 0 {
			eventIdx := m.notifEditFocus - 1
			if eventIdx < len(allNotifEvents) {
				event := allNotifEvents[eventIdx]
				m.notifEditEvents[event] = !m.notifEditEvents[event]
			}
		}
		return ""
	}
	return ""
}

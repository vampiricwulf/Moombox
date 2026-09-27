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
			// Absent enabled means delivering, exactly as the card reads it.
			m.notifEditEnabled = n.IsEnabled()
			m.notifEditMention = n.Mention
			// An ABSENT mention_events shows the shipped defaults; an explicit
			// (possibly empty) list shows itself. The distinction is the whole
			// three-way rule, so it is read off the pointer rather than through
			// ResolveMentionEvents, which folds the no-mention case to nil and
			// would blank a stored list the moment a mention was cleared.
			m.notifEditMentionTouched = false
			if n.MentionEvents == nil {
				m.seedMentionDefaults()
			} else {
				m.notifEditMentionEvents = make(map[string]bool, len(*n.MentionEvents))
				for _, e := range *n.MentionEvents {
					m.notifEditMentionEvents[e] = true
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
		m.notifEditEnabled = true
		m.notifEditMention = ""
		m.notifEditMentionTouched = false
		m.seedMentionDefaults()
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

// seedMentionDefaults lights the @ column with the shipped default mention
// filter (config.DefaultMentionEvents). Called when the editor opens a target
// whose mention_events is absent, so the column shows what WOULD be pinged
// without the operator ever having written the list down. It deliberately does
// not set notifEditMentionTouched: only the operator's own `m` keypress makes
// the list explicit.
func (m *SettingsModel) seedMentionDefaults() {
	defaults := config.DefaultMentionEvents()
	m.notifEditMentionEvents = make(map[string]bool, len(defaults))
	for _, e := range defaults {
		m.notifEditMentionEvents[e] = true
	}
}

func (m *SettingsModel) handleNotifEditKey(key string) string {
	totalItems := notifEditEventBase + len(allNotifEvents)

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
		// Save-time mention validation, for the same reason the URL gets one:
		// config.validateOrNormalize CLEARS an unparseable mention instead of
		// refusing the save, so a typo would round-trip to a target that pings
		// nobody with nothing said about it.
		mention, _, _, err := config.ParseMention(m.notifEditMention)
		if err != nil {
			m.errorMsg = err.Error()
			m.status = saveError
			return ""
		}
		var events []string
		selected := 0
		for _, e := range allNotifEvents {
			if m.notifEditEvents[e] {
				selected++
				events = append(events, e)
			}
		}
		// Copy the stored target rather than building a fresh one: this editor
		// owns five fields, and a from-scratch literal silently dropped every
		// other per-target key the moment anyone fixed a typo in a URL.
		var n config.NotificationConfig
		if m.notifIndex < len(m.notifications) {
			n = m.notifications[m.notifIndex]
		}
		n.URL = strings.TrimSpace(m.notifEditURL)
		// An empty selection stores Events = nil, which the manager, the web
		// card and operations.md all read as "all events". Silence is the
		// Enabled toggle, not an empty filter.
		n.Events = nil
		if selected > 0 && selected < len(allNotifEvents) {
			n.Events = events
		}
		// Written explicitly either way, never deleted back to absent, so the
		// state an operator chose reads the same in config.toml as it does
		// here — the same rule the web switch commits under.
		enabled := m.notifEditEnabled
		n.Enabled = &enabled
		n.Mention = mention
		// The three-way rule: an untouched column leaves mention_events exactly
		// as it was stored (absent stays absent, so the target keeps following
		// the shipped list), and the first toggle makes the whole list
		// explicit — including the all-off case, which is an empty list
		// ("never"), not an absent key ("the default six").
		if m.notifEditMentionTouched {
			mentionEvents := []string{}
			for _, e := range allNotifEvents {
				if m.notifEditMentionEvents[e] {
					mentionEvents = append(mentionEvents, e)
				}
			}
			n.MentionEvents = &mentionEvents
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
		// Both arms are guarded on their own row, because the two text rows
		// (URL and Mention) own their own spaces: UpdateComponents routes the
		// key into the focused input, and this arm must not also act on it.
		if m.notifEditFocus == notifEditEnabledRow {
			m.notifEditEnabled = !m.notifEditEnabled
			return ""
		}
		if eventIdx := m.notifEditFocus - notifEditEventBase; eventIdx >= 0 && eventIdx < len(allNotifEvents) {
			event := allNotifEvents[eventIdx]
			m.notifEditEvents[event] = !m.notifEditEvents[event]
		}
		return ""
	case "m", "M":
		// The @ column's key. Guarded on an event row so an `m` typed into the
		// URL or Mention field only reaches the text input.
		if eventIdx := m.notifEditFocus - notifEditEventBase; eventIdx >= 0 && eventIdx < len(allNotifEvents) {
			event := allNotifEvents[eventIdx]
			if m.notifEditMentionEvents == nil {
				m.notifEditMentionEvents = make(map[string]bool, len(allNotifEvents))
			}
			m.notifEditMentionEvents[event] = !m.notifEditMentionEvents[event]
			m.notifEditMentionTouched = true
		}
		return ""
	}
	return ""
}

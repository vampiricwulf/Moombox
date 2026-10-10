package tui

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
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
				if m.notifIndex < len(m.notifFrom) {
					m.notifFrom = slices.Delete(m.notifFrom, m.notifIndex, m.notifIndex+1)
				}
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
			m.notifEditEventsTouched = false
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
				m.notifEditMentionExtras = notifMentionIDsWithNoRow(*n.MentionEvents)
			}
			// Anything that is not the opt-in "edit" reads as separate — an
			// absent key included, which is what every config written before
			// this key looks like.
			m.notifEditDelivery = "edit"
			if strings.TrimSpace(n.Mode) != "edit" {
				m.notifEditDelivery = "separate"
			}
			m.notifEditFocus = 0
			m.notifMode = "edit"
			m.updateTextInputForField()
		}
	case "a", "A":
		m.notifEditURL = ""
		m.notifEditEvents = make(map[string]bool)
		m.notifEditEventsTouched = false
		for _, e := range allNotifEvents {
			m.notifEditEvents[e] = true
		}
		m.notifEditEnabled = true
		m.notifEditMention = ""
		m.notifEditMentionTouched = false
		m.seedMentionDefaults()
		m.notifEditDelivery = "separate"
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
	m.notifEditMentionExtras = notifMentionIDsWithNoRow(defaults)
}

// notifMentionIDsWithNoRow returns the ids in a resolved mention list that
// this build's EventGroups has no row for, in their original order.
func notifMentionIDsWithNoRow(ids []string) []string {
	known := make(map[string]bool, len(allNotifEvents))
	for _, e := range allNotifEvents {
		known[e] = true
	}
	var out []string
	for _, e := range ids {
		if !known[e] {
			out = append(out, e)
		}
	}
	return out
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
		// Written explicitly either way, "separate" included, so the two
		// editors round-trip the same value — the web card commits under the
		// same rule.
		n.Mode = m.notifEditDelivery
		// An empty selection stores Events = nil, which the manager, the web
		// card and operations.md all read as "all events". Silence is the
		// Enabled toggle, not an empty filter. Untouched rows leave the stored
		// filter as it is (notifEditEventsTouched); an added target has none,
		// which is every event — the rows "a" ticks.
		if m.notifEditEventsTouched {
			n.Events = nil
			if selected > 0 && selected < len(allNotifEvents) {
				n.Events = events
			}
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
		// With no mention there is nobody to ping, so the list is inert: the
		// stored one is left exactly as it is, and none is invented. Both are
		// what settings.js does (toggleMentionEvent opens with
		// `if (!notif || !notif.mention) return`, and setNotificationMention
		// deliberately leaves the list alone when the mention is cleared), so
		// an operator who retypes a mention gets their own filter back rather
		// than today's defaults frozen into their config file.
		if m.notifEditMentionTouched && mention != "" {
			mentionEvents := []string{}
			for _, e := range allNotifEvents {
				if m.notifEditMentionEvents[e] {
					mentionEvents = append(mentionEvents, e)
				}
			}
			// Ids with no row cannot be unticked, so they ride along rather
			// than being dropped by a toggle elsewhere — settings.js keeps
			// them the same way, through its resolved list. The map lookup
			// still honours an explicit false.
			for _, e := range m.notifEditMentionExtras {
				if m.notifEditMentionEvents[e] {
					mentionEvents = append(mentionEvents, e)
				}
			}
			n.MentionEvents = &mentionEvents
		}
		if m.notifIndex < len(m.notifications) {
			m.notifications[m.notifIndex] = n
		} else {
			// An added target has no Open copy: the save adds it rather than
			// merging it into a live target.
			m.notifFrom = append(m.notifFrom[:min(len(m.notifFrom), len(m.notifications))], -1)
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
	case keySpace:
		// Both arms are guarded on their own row, because the two text rows
		// (URL and Mention) own their own spaces: UpdateComponents routes the
		// key into the focused input, and this arm must not also act on it.
		if m.notifEditFocus == notifEditEnabledRow {
			m.notifEditEnabled = !m.notifEditEnabled
			return ""
		}
		if m.notifEditFocus == notifEditDeliveryRow {
			if m.notifEditDelivery == "edit" {
				m.notifEditDelivery = "separate"
			} else {
				m.notifEditDelivery = "edit"
			}
			return ""
		}
		if eventIdx := m.notifEditFocus - notifEditEventBase; eventIdx >= 0 && eventIdx < len(allNotifEvents) {
			event := allNotifEvents[eventIdx]
			m.notifEditEvents[event] = !m.notifEditEvents[event]
			m.notifEditEventsTouched = true
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

// notifFormValues is a target as the notification editor's Enter writes
// it, each field in a form that tells apart exactly what the manager tells
// apart: the URL as stored (the manager refuses a padded one, so trimming
// it is an edit), the mute as IsEnabled reads it, the event filter as a set
// (notifFormEvents), the canonical mention, the mention filter as a set (or
// "default", absent), and the delivery mode with absent reading separate.
// Two targets with equal form values differ only in spelling, so a save
// that compared the stored structs would read an Enter that changed
// nothing — which spells an absent enabled out as true — as an edit; and a
// form that hid a difference the manager acts on would drop a real edit as
// none.
func notifFormValues(n config.NotificationConfig) map[string]string {
	mention := strings.TrimSpace(n.Mention)
	if canonical, _, _, err := config.ParseMention(n.Mention); err == nil {
		mention = canonical
	}
	mentionEvents := "default"
	if n.MentionEvents != nil {
		list := slices.Clone(*n.MentionEvents)
		slices.Sort(list)
		mentionEvents = "[" + strings.Join(slices.Compact(list), ",") + "]"
	}
	mode := "separate"
	if strings.TrimSpace(n.Mode) == "edit" {
		mode = "edit"
	}
	return map[string]string{
		"url":            n.URL,
		"enabled":        boolToDisplay(n.IsEnabled()),
		"events":         notifFormEvents(n.Events),
		"mention":        mention,
		"mention_events": mentionEvents,
		"mode":           mode,
	}
}

// notifEventsSorted is the editor's event vocabulary, sorted.
var notifEventsSorted = func() []string {
	out := slices.Clone(allNotifEvents)
	slices.Sort(out)
	return out
}()

// notifFormEvents is an event filter as notifFormValues compares it: "*"
// for every event — no filter, or one naming each vocabulary event and
// nothing else — and otherwise the names it holds, as a sorted set. A
// filter naming only events outside the vocabulary is its own value, not
// "*": the manager reads it as a filter (one naming a retired key matches
// that key's alias; connectivity_lost matches nothing), so turning it into
// "every event" is an edit.
func notifFormEvents(events []string) string {
	if len(events) == 0 {
		return "*"
	}
	set := slices.Clone(events)
	slices.Sort(set)
	set = slices.Compact(set)
	if slices.Equal(set, notifEventsSorted) {
		return "*"
	}
	return fmt.Sprintf("%q", set)
}

// writeNotifField writes the form field key — one of notifFormValues'
// keys — from src onto dst, and reports false for a key it does not know.
// The slices and pointers are copied, never shared: dst goes into the live
// config, src stays in the editor.
func writeNotifField(dst *config.NotificationConfig, src config.NotificationConfig, key string) bool {
	switch key {
	case "url":
		dst.URL = src.URL
	case "enabled":
		dst.Enabled = nil
		if src.Enabled != nil {
			on := *src.Enabled
			dst.Enabled = &on
		}
	case "events":
		dst.Events = slices.Clone(src.Events)
	case "mention":
		dst.Mention = src.Mention
	case "mention_events":
		dst.MentionEvents = nil
		if src.MentionEvents != nil {
			list := append([]string{}, *src.MentionEvents...)
			dst.MentionEvents = &list
		}
	case "mode":
		dst.Mode = src.Mode
	default:
		return false
	}
	return true
}

// cloneNotification copies n with nothing shared.
func cloneNotification(n config.NotificationConfig) config.NotificationConfig {
	out := n
	for k := range notifFormValues(n) {
		writeNotifField(&out, n, k)
	}
	return out
}

// notifIdentities names each target of list by its trimmed URL and how many
// earlier entries carry the same one: the URL is all a target has to be
// known by, and a webhook listed twice (which the manager posts to once) is
// still two entries to the editors.
func notifIdentities(list []config.NotificationConfig) []string {
	seen := make(map[string]int, len(list))
	ids := make([]string, len(list))
	for i, n := range list {
		url := strings.TrimSpace(n.URL)
		ids[i] = url + "#" + strconv.Itoa(seen[url])
		seen[url]++
	}
	return ids
}

// mergeNotificationEdits applies the Settings editor's own target changes to
// the target list as it stands at save time — mergeChannelEdits' rule, with
// a target's identity in place of a channel ID. base is the list Open
// copied, edited the list the editor holds now, from the base index each
// edited entry was opened from (-1, or past its end: added in the editor),
// live the store's list now. The result is a new slice (never live's array,
// which Snapshot readers and a rollback share), and changed is false — live
// returned as is — when the editor changed nothing.
//
// A target is known by the URL it had at Open (notifIdentities), so one
// whose URL the editor changed is still the same target. A base target the
// editor deleted is removed from live. One it changed — compared by
// notifFormValues, which tells apart what the manager does — is merged
// field by field into live's target
// with that identity: only the fields the editor changed are written, so a
// dashboard mute or mention on the same target survives a TUI edit of its
// events, and a field both changed takes the editor's value. An edit of a
// target the dashboard removed meanwhile is added back, the operator having
// saved it on purpose. A target the editor added replaces one the dashboard
// added with the same URL while the overlay was open — one webhook, listed
// once — and is appended otherwise. Everything else in live — a target the
// dashboard added, muted or edited, and one it removed that the editor did
// not touch — stays exactly as live has it.
func mergeNotificationEdits(base, edited []config.NotificationConfig, from []int, live []config.NotificationConfig) ([]config.NotificationConfig, bool) {
	baseIDs := notifIdentities(base)
	type upsert struct {
		n, was  config.NotificationConfig
		id      string // the base identity; unused for an added target
		hasBase bool
	}
	kept := make([]bool, len(base))
	var upserts []upsert
	for i, n := range edited {
		f := -1
		if i < len(from) {
			f = from[i]
		}
		if f < 0 || f >= len(base) || kept[f] {
			upserts = append(upserts, upsert{n: n})
			continue
		}
		kept[f] = true
		if maps.Equal(notifFormValues(base[f]), notifFormValues(n)) {
			continue
		}
		upserts = append(upserts, upsert{n: n, was: base[f], id: baseIDs[f], hasBase: true})
	}
	removed := make(map[string]bool)
	inBase := make(map[string]bool, len(base))
	for i, id := range baseIDs {
		inBase[id] = true
		if !kept[i] {
			removed[id] = true
		}
	}
	if len(removed) == 0 && len(upserts) == 0 {
		return live, false
	}

	// outIDs follows out: each kept live target's identity, "" once an added
	// target has replaced it or for one appended, so two added targets with
	// one URL do not both land on the same dashboard-added entry.
	out := make([]config.NotificationConfig, 0, len(live)+len(upserts))
	outIDs := make([]string, 0, cap(out))
	for i, id := range notifIdentities(live) {
		if !removed[id] {
			out = append(out, live[i])
			outIDs = append(outIDs, id)
		}
	}
	for _, u := range upserts {
		i := -1
		if u.hasBase {
			i = slices.Index(outIDs, u.id)
		} else {
			url := strings.TrimSpace(u.n.URL)
			for j, id := range outIDs {
				if id != "" && !inBase[id] && strings.TrimSpace(out[j].URL) == url {
					i = j
					break
				}
			}
		}
		switch {
		case i >= 0 && u.hasBase:
			out[i] = mergeNotifFields(out[i], u.was, u.n)
		case i >= 0:
			out[i] = cloneNotification(u.n)
			outIDs[i] = ""
		default:
			out = append(out, cloneNotification(u.n))
			outIDs = append(outIDs, "")
		}
	}
	return out, true
}

// mergeNotifFields is the three-way merge of one target: live as it stands
// at save time, with each form field the editor changed — where edited
// differs from base, the copy Open took, compared by notifFormValues —
// taken from edited. A field the editor left alone keeps live's value,
// whatever the dashboard set it to meanwhile.
func mergeNotifFields(live, base, edited config.NotificationConfig) config.NotificationConfig {
	was, now := notifFormValues(base), notifFormValues(edited)
	for k := range was {
		if was[k] != now[k] {
			writeNotifField(&live, edited, k)
		}
	}
	return live
}

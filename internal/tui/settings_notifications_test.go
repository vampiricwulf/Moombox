package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// sectionIndexByName resolves a settings section to its index. internal/tui
// has no such helper today (settings_channels_test.go:240 inlines the loop);
// this is that loop, named once.
func sectionIndexByName(t *testing.T, name string) int {
	t.Helper()
	for i, s := range sections {
		if s.name == name {
			return i
		}
	}
	t.Fatalf("no settings section named %q", name)
	return -1
}

func newNotifEditModel(t *testing.T, existing []config.NotificationConfig) *SettingsModel {
	t.Helper()
	m := &SettingsModel{
		values:        map[string]string{},
		textInput:     textinput.New(),
		notifications: existing,
		notifMode:     "list",
	}
	m.sectionIndex = sectionIndexByName(t, "Integrations")
	return m
}

// newSettingsModelForSave builds a model the pre-save gate can actually run
// against. internal/tui has no such helper either; this mirrors
// settings_save_error_test.go's inline four lines. The configStore is not
// optional: applyValues reads PasswordHash through it (settings.go:606)
// before it reaches any field validation, and a nil store panics there.
func newSettingsModelForSave(t *testing.T) *SettingsModel {
	t.Helper()
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.OnSave = func(*config.MoomboxConfig) error { return nil }
	m.Open(cfg)
	return m
}

// TestNotifEditAcceptsEmptyFilter removes the last place the two UIs
// disagreed about what an empty event list means. The manager has always read
// a nil/empty filter as "all events" and operations.md has always documented
// that; the TUI's refusal was the outlier, and it left an operator who wanted
// silence deleting the webhook. The mute is the Enabled toggle now.
func TestNotifEditAcceptsEmptyFilter(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditURL = "discord://1/aaa"
	m.notifEditEvents = map[string]bool{} // every box unticked
	m.handleNotifEditKey(keyEnter)

	if m.status == saveError {
		t.Fatalf("saving an empty filter reported %q — an empty filter means ALL events, not an error", m.errorMsg)
	}
	if len(m.notifications) != 1 {
		t.Fatalf("the target was not saved (%d stored)", len(m.notifications))
	}
	if m.notifications[0].Events != nil {
		t.Errorf("Events = %v, want nil so the manager reads it as all events", m.notifications[0].Events)
	}
}

// TestNotifListShowsAllEventsLabel pins the wording parity with the web card.
func TestNotifListShowsAllEventsLabel(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{{URL: "discord://1/aaa"}})
	out := m.renderNotifications(80, 10)
	if !strings.Contains(out, "All events") {
		t.Errorf("the list line for an unfiltered target did not say \"All events\":\n%s", out)
	}
}

// TestNotifEditEnabledToggleRoundTrips: the mute must survive edit → save →
// re-edit, or an operator who opens a muted target and presses Enter silently
// un-mutes it.
func TestNotifEditEnabledToggleRoundTrips(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{{URL: "discord://1/aaa"}})
	m.handleNotifKey(keyEnter) // open the editor
	if !m.notifEditEnabled {
		t.Fatal("an existing target with no enabled key opened as disabled")
	}
	m.notifEditFocus = 1
	m.handleNotifEditKey(" ")
	if m.notifEditEnabled {
		t.Fatal("Space on the Enabled row did not toggle it")
	}
	m.handleNotifEditKey(keyEnter)
	if m.notifications[0].IsEnabled() {
		t.Fatal("enabled = false was not saved")
	}

	m.handleNotifKey(keyEnter)
	if m.notifEditEnabled {
		t.Error("re-opening a muted target showed it as enabled — pressing Enter would silently un-mute it")
	}
}

// TestNotifEditMentionDefaultsStayImplicit is the three-way rule at the TUI
// edge. Typing a mention must NOT write mention_events: the target then
// follows the shipped default list, and a later release that adds a key to
// that list reaches them. Writing it out would freeze today's six.
func TestNotifEditMentionDefaultsStayImplicit(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditURL = "discord://1/aaa"
	m.notifEditMention = "@here"
	m.handleNotifEditKey(keyEnter)

	if m.notifications[0].Mention != "@here" {
		t.Fatalf("mention = %q", m.notifications[0].Mention)
	}
	if m.notifications[0].MentionEvents != nil {
		t.Errorf("mention_events = %v, want absent so the default list keeps applying",
			m.notifications[0].MentionEvents)
	}
}

// TestNotifEditMentionToggleWritesExplicitList: the moment the operator
// disagrees with a default, the whole list becomes explicit — including the
// all-off case, which must be an explicit EMPTY list ("never"), not an absent
// key ("the default six").
func TestNotifEditMentionToggleWritesExplicitList(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditURL = "discord://1/aaa"
	m.notifEditMention = "@here"
	m.seedMentionDefaults() // what the editor does when a mention is first entered

	// Untick every default.
	for _, e := range config.DefaultMentionEvents() {
		m.notifEditMentionEvents[e] = false
	}
	m.notifEditMentionTouched = true
	m.handleNotifEditKey(keyEnter)

	got := m.notifications[0].MentionEvents
	if got == nil {
		t.Fatal("unticking every mention toggle left mention_events absent — the operator switched the " +
			"pings off and would keep receiving the default six")
	}
	if len(*got) != 0 {
		t.Errorf("mention_events = %v, want an explicit empty list", *got)
	}
}

// TestNotifEditMKeyTogglesTheFocusedEventsMention pins the column's key.
func TestNotifEditMKeyTogglesTheFocusedEventsMention(t *testing.T) {
	first := allNotifEvents[0]
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditMention = "@here"
	m.seedMentionDefaults()
	m.notifEditFocus = notifEditEventBase // the first event row
	before := m.notifEditMentionEvents[first]
	m.handleNotifEditKey("m")
	if m.notifEditMentionEvents[first] == before {
		t.Errorf("m on the first event row did not toggle its mention flag")
	}
	if !m.notifEditMentionTouched {
		t.Error("toggling a mention flag did not mark the list explicit")
	}
}

// TestHandleMouseNotifClickMapsThroughTheNewRows guards the index shift: the
// edit form gained two rows above the event list, so every hard-coded offset
// in the mouse map had to move with it.
func TestHandleMouseNotifClickMapsThroughTheNewRows(t *testing.T) {
	firstEvent := notifEventGroups[0].events[0]
	m := newNotifEditModel(t, nil)
	m.notifMode = "edit"
	m.notifEditEvents = map[string]bool{}
	m.notifEditMentionEvents = map[string]bool{}
	m.notifEditScrollStart = 0
	// Unscrolled layout: 0 title, 1 URL, 2 Enabled, 3 Mention, 4 blank,
	// 5 "Events", 6 group0 blank, 7 group0 header, 8 group0 event0.
	m.handleMouseNotifClick(8)
	if !m.notifEditEvents[firstEvent] {
		t.Errorf("a click on the first event row (contentY=8) did not toggle %q — the mouse map still "+
			"assumes the pre-N2b row offsets", firstEvent)
	}
}

// TestNotifEditPreservesFieldsTheEditorDoesNotShow: the Enter arm built a
// fresh NotificationConfig from the rows on screen, so editing a webhook's
// URL from the TUI erased every key the form has no row for. N3's `mode` is
// the next one, and it would go the first time anyone fixes a typo.
//
// The load-bearing assertion is MentionEvents, the one key today that the
// editor stores without rebuilding: an UNTOUCHED @ column must leave the
// stored list exactly as it was, so a copy-then-overwrite keeps it while a
// rebuild-from-scratch drops it back to nil and silently returns the target to
// the default six. Events is asserted beside it as the documented no-widening
// case — it round-trips through the checkbox seed either way, so on its own it
// would not catch the rebuild. When N3 lands, add a `mode` assertion here
// rather than replacing these.
func TestNotifEditPreservesFieldsTheEditorDoesNotShow(t *testing.T) {
	stored := []string{"finished"}
	m := newNotifEditModel(t, []config.NotificationConfig{
		{URL: "discord://1/aaa", Events: []string{"finished"}, Mention: "@here", MentionEvents: &stored},
	})
	m.handleNotifKey(keyEnter)         // open — seeds the checkboxes from Events
	m.notifEditURL = "discord://1/bbb" // the operator only fixed the URL
	m.handleNotifEditKey(keyEnter)

	got := m.notifications[0]
	if got.URL != "discord://1/bbb" {
		t.Fatalf("URL = %q", got.URL)
	}
	if len(got.Events) != 1 || got.Events[0] != "finished" {
		t.Errorf("events = %v — a URL edit widened the filter; the Enter arm rebuilt the target "+
			"instead of copying it", got.Events)
	}
	if got.MentionEvents == nil {
		t.Fatal("a URL edit erased mention_events — the Enter arm rebuilt the target instead of " +
			"copying it, and the operator's explicit ping filter silently became the default six")
	}
	if len(*got.MentionEvents) != 1 || (*got.MentionEvents)[0] != "finished" {
		t.Errorf("mention_events = %v, want the stored list untouched", *got.MentionEvents)
	}
}

// TestPublicURLFieldRoundTrips and its save path.
func TestPublicURLFieldRoundTrips(t *testing.T) {
	found := false
	for _, s := range sections {
		if s.name != "Network" {
			continue
		}
		for _, f := range s.fields {
			if f.key == "public_url" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the Network section has no public_url row — the web UI offers one and the TUI must too")
	}

	cfg := config.Defaults()
	cfg.Network.PublicURL = "https://x.example"
	m := &SettingsModel{values: map[string]string{}, textInput: textinput.New()}
	m.loadValues(cfg)
	if m.values["public_url"] != "https://x.example" {
		t.Errorf("loadValues did not populate public_url (%q)", m.values["public_url"])
	}
}

// TestSaveRejectsUnusablePublicURL mirrors the trusted_proxies gate: config.Save
// runs Validate and REFUSES a failing config, so without a pre-save check one
// typo makes the whole save fail while saveAndClose still reports "Saved" and
// every other change in that save is lost.
func TestSaveRejectsUnusablePublicURL(t *testing.T) {
	m := newSettingsModelForSave(t) // the package's existing save-test helper
	m.values["public_url"] = "moombox.example.com"
	// saveAndClose short-circuits on a clean form; a typed value is what makes
	// it dirty in the real flow (syncFromTextInput calls recheckDirty), and
	// settings_save_error_test.go marks its models dirty for the same reason.
	m.recheckDirty()
	m.saveAndClose()
	if m.status != saveError {
		t.Fatal("an unusable public_url was accepted; config.Save would then refuse the whole config " +
			"and every other edit in this save would be lost")
	}
	if !strings.Contains(m.errorMsg, "public_url") && !strings.Contains(m.errorMsg, "Public dashboard URL") {
		t.Errorf("the error did not name the field: %q", m.errorMsg)
	}
}

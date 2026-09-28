package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestNotifEditLoadsDeliveryMode: opening a target for edit must show the mode
// it is actually in, or the operator toggles from a wrong starting point.
func TestNotifEditLoadsDeliveryMode(t *testing.T) {
	m := &SettingsModel{
		notifMode:  "list",
		textInput:  textinput.New(),
		notifIndex: 0,
		notifications: []config.NotificationConfig{
			{URL: "discord://1/a", Mode: "edit"},
		},
	}
	m.handleNotifKey(keyEnter)
	if m.notifEditDelivery != "edit" {
		t.Errorf("notifEditDelivery = %q, want edit", m.notifEditDelivery)
	}

	m2 := &SettingsModel{
		notifMode:     "list",
		textInput:     textinput.New(),
		notifications: []config.NotificationConfig{{URL: "discord://1/a"}},
	}
	m2.handleNotifKey(keyEnter)
	if m2.notifEditDelivery != "separate" {
		t.Errorf("an unset mode must load as separate, got %q", m2.notifEditDelivery)
	}
}

// TestNotifEditTogglesDeliveryMode: Space on the Delivery row flips it.
func TestNotifEditTogglesDeliveryMode(t *testing.T) {
	m := &SettingsModel{
		notifMode:         "edit",
		textInput:         textinput.New(),
		notifEditEvents:   map[string]bool{},
		notifEditDelivery: "separate",
		notifEditFocus:    notifEditDeliveryRow,
	}
	m.handleNotifEditKey(" ")
	if m.notifEditDelivery != "edit" {
		t.Fatalf("after one toggle = %q, want edit", m.notifEditDelivery)
	}
	m.handleNotifEditKey(" ")
	if m.notifEditDelivery != "separate" {
		t.Fatalf("after two toggles = %q, want separate", m.notifEditDelivery)
	}
}

// TestNotifEditSavePreservesMode is the one that matters: the Enter path
// copies the stored target and overwrites only the fields the editor owns, so
// a field it never assigns is silently dropped on the next save.
func TestNotifEditSavePreservesMode(t *testing.T) {
	m := &SettingsModel{
		notifMode:         "edit",
		textInput:         textinput.New(),
		notifEditURL:      "discord://1/a",
		notifEditDelivery: "edit",
		notifEditEvents:   map[string]bool{},
		notifIndex:        0,
		notifications:     []config.NotificationConfig{{URL: "discord://1/a"}},
	}
	for _, e := range allNotifEvents {
		m.notifEditEvents[e] = true
	}
	m.handleNotifEditKey(keyEnter)

	if len(m.notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(m.notifications))
	}
	if m.notifications[0].Mode != "edit" {
		t.Errorf("saved Mode = %q, want edit — the Enter path dropped it", m.notifications[0].Mode)
	}
	if !m.dirty || !m.structDirty {
		t.Error("the save did not mark the form dirty")
	}
}

// TestNotifEditSaveWritesSeparateExplicitly: storing "" would be a second
// spelling of the default that the Web card does not use — the two editors
// must round-trip the same value.
func TestNotifEditSaveWritesSeparateExplicitly(t *testing.T) {
	m := &SettingsModel{
		notifMode:         "edit",
		textInput:         textinput.New(),
		notifEditURL:      "discord://1/a",
		notifEditDelivery: "separate",
		notifEditEvents:   map[string]bool{},
		notifIndex:        0,
		notifications:     []config.NotificationConfig{{URL: "discord://1/a", Mode: "edit"}},
	}
	for _, e := range allNotifEvents {
		m.notifEditEvents[e] = true
	}
	m.handleNotifEditKey(keyEnter)
	if m.notifications[0].Mode != "separate" {
		t.Errorf("saved Mode = %q, want separate", m.notifications[0].Mode)
	}
}

// TestNotifEditModeSurvivesAUrlOnlyEdit closes the round trip the four tests
// above only cover in halves: open a stored edit-mode target, change nothing
// but the URL, save. This is the shape TestNotifEditPreservesFieldsTheEditor
// DoesNotShow pins for the keys the editor has no row for — Mode now has a
// row, and a seed that forgot to read n.Mode would silently demote every
// edit-mode target the moment someone fixed a typo in its URL.
func TestNotifEditModeSurvivesAUrlOnlyEdit(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{
		{URL: "discord://1/aaa", Mode: "edit"},
	})
	m.handleNotifKey(keyEnter) // open
	m.notifEditURL = "discord://1/bbb"
	m.handleNotifEditKey(keyEnter)

	if got := m.notifications[0]; got.Mode != "edit" {
		t.Errorf("Mode = %q after a URL-only edit, want edit — the open seeded the wrong mode "+
			"or the save dropped it", got.Mode)
	}
}

// TestNotifEditHeadRowOrderMatchesTheFocusIndices pins the ORDER the four
// head rows render in. The mouse map, the focus marker and the scroll window
// all address a head row by line number, so a row's screen line must equal
// its focus index + 1 (the title is pinned at line 0).
//
// MUTANT: move the Delivery block above Mention in renderNotifEdit. Every
// other test in the package stays green while a click on Mention toggles the
// mode and the focus marker sits one row off.
func TestNotifEditHeadRowOrderMatchesTheFocusIndices(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{{URL: "discord://1/a"}})
	m.handleNotifKey(keyEnter) // open the editor, seeding every row
	m.notifEditScrollStart = 0

	ls := strings.Split(m.renderNotifEdit(100, 1000), "\n")
	for _, row := range []struct {
		index int
		label string
	}{
		{notifEditURLRow, "Webhook URL"},
		{notifEditEnabledRow, "Enabled"},
		{notifEditMentionRow, "Mention"},
		{notifEditDeliveryRow, "Delivery"},
	} {
		line := row.index + 1
		if line >= len(ls) {
			t.Fatalf("the editor rendered %d lines, too few to hold row %d (%s)", len(ls), row.index, row.label)
		}
		if !strings.Contains(ls[line], row.label) {
			t.Errorf("line %d = %q, want the %s row (focus index %d + 1)", line, ls[line], row.label, row.index)
		}
	}
}

// TestNotifEditNewTargetDefaultsToSeparate: `a` opens a blank form, and the
// new key is opt-in — a target added from the TUI must not arrive in edit mode.
func TestNotifEditNewTargetDefaultsToSeparate(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	if m.notifEditDelivery != "separate" {
		t.Errorf("a new target opened with notifEditDelivery = %q, want separate", m.notifEditDelivery)
	}
}

package tui

import (
	"testing"

	"charm.land/bubbles/v2/textinput"
)

// TestHandleMouseNotifClickAccountsForScroll guards the edit-mode event-click
// path against the focus-following scroll window in renderNotifEdit: when the
// event list has scrolled, the on-screen row must be mapped back through the
// scroll offset before resolving which event was clicked. Without the offset a
// scrolled click toggles the wrong event.
func TestHandleMouseNotifClickAccountsForScroll(t *testing.T) {
	firstEvent := notifEventGroups[0].events[0]

	// Original (unscrolled) line layout in renderNotifEdit:
	//   0 title, 1 URL, 2 Enabled, 3 Mention, 4 blank, 5 "Events:",
	//   6 group0 blank, 7 group0 header, 8 group0 event0, ...
	// With the view scrolled by 2 body lines, group0 event0 (original line 8)
	// renders on screen at contentY = 8 - 2 = 6.
	m := &SettingsModel{
		notifMode:            "edit",
		notifEditEvents:      map[string]bool{},
		textInput:            textinput.New(),
		notifEditScrollStart: 2,
	}

	m.handleMouseNotifClick(6)

	if !m.notifEditEvents[firstEvent] {
		t.Errorf("scrolled click (contentY=6, scrollStart=2) should toggle the first event %q; "+
			"it did not — the handler ignored the scroll offset", firstEvent)
	}
}

// TestHandleMouseNotifClickNoScrollUnchanged confirms the unscrolled path still
// maps clicks the same as before (regression guard for the offset addition).
func TestHandleMouseNotifClickNoScrollUnchanged(t *testing.T) {
	firstEvent := notifEventGroups[0].events[0]
	m := &SettingsModel{
		notifMode:            "edit",
		notifEditEvents:      map[string]bool{},
		textInput:            textinput.New(),
		notifEditScrollStart: 0,
	}
	// Unscrolled: group0 event0 is at original line 8 = contentY 8.
	m.handleMouseNotifClick(8)
	if !m.notifEditEvents[firstEvent] {
		t.Errorf("unscrolled click (contentY=8) should toggle the first event %q", firstEvent)
	}
}

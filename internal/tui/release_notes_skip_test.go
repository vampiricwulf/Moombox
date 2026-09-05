package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestReleaseNotesSkipKey: S skips the pending version through
// OnDismissUpdate, clears updateAvailable and closes the overlay; with no
// update pending (notes of the current version) S does nothing and the
// footer does not offer it.
func TestReleaseNotesSkipKey(t *testing.T) {
	app := NewApp()
	skipped := ""
	app.OnDismissUpdate = func(tag string) error { skipped = tag; return nil }
	app.updateAvailable = &UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9", ReleaseNotes: "notes"}
	app.releaseNotesPopup.open("v9.9.9", "notes", 80, 24)
	app.releaseNotesPopup.setPending(true)
	if !strings.Contains(app.releaseNotesPopup.View(), "S: Skip") {
		t.Fatal("footer must offer S while an update is pending")
	}
	_, cmd := app.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd != nil {
		cmd()
	}
	if skipped != "v9.9.9" {
		t.Fatalf("OnDismissUpdate got %q", skipped)
	}
	// the result arm
	app.Update(dismissUpdateResultMsg{Tag: "v9.9.9"})
	if app.updateAvailable != nil || app.releaseNotesPopup.isOpen() {
		t.Fatal("skip must clear updateAvailable and close the overlay")
	}

	app.updateAvailable = nil
	skipped = ""
	app.releaseNotesPopup.open("v1.0.0", "current notes", 80, 24)
	if strings.Contains(app.releaseNotesPopup.View(), "S: Skip") {
		t.Fatal("footer must not offer S without a pending update")
	}
	app.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if skipped != "" {
		t.Fatal("S without a pending update must not call OnDismissUpdate")
	}
}

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
	app.releaseNotesPopup.setPending(false)
	if strings.Contains(app.releaseNotesPopup.View(), "S: Skip") {
		t.Fatal("footer must not offer S without a pending update")
	}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd != nil {
		cmd()
	}
	if skipped != "" {
		t.Fatal("S without a pending update must not call OnDismissUpdate")
	}
	// The race the guard exists for: a periodic check lands WHILE the current
	// version's notes are open. updateAvailable is non-nil again, but the
	// footer still does not offer S (pending is false), so S must not skip a
	// release nobody has read yet — skipping is permanent. The command has to
	// be RUN: OnDismissUpdate is called inside it, not by handleKey.
	app.updateAvailable = &UpdateStatusMsg{Version: "2.0.0", TagName: "v2.0.0", ReleaseNotes: "unread"}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd != nil {
		cmd()
	}
	if skipped != "" {
		t.Fatalf("S skipped %q while showing another version's notes with no S in the footer", skipped)
	}
}

// TestReleaseNotesApplyKeyOnlyBesideAPendingUpdate: R N with no update
// pending opens the running version's notes, whose footer still offered
// "U: Apply update"; U then closed the notes being read to say "No update
// available". And a periodic check landing while those notes were open let U
// apply a release whose notes were not on screen. U is now offered and
// handled only beside the pending update's own notes — the S gate.
//
// Mutants: put U back in the viewer footer (the first check fails); drop the
// key's pending/tag gate (U closes the viewer and calls OnApplyUpdate).
func TestReleaseNotesApplyKeyOnlyBesideAPendingUpdate(t *testing.T) {
	app := NewApp()
	applied := false
	app.OnApplyUpdate = func(string) string { applied = true; return "" }

	app.releaseNotesPopup.open("v1.0.0", "current notes", 80, 24)
	app.releaseNotesPopup.setPending(false)
	if strings.Contains(app.releaseNotesPopup.View(), "U") {
		t.Fatalf("viewer footer offers U with nothing to apply:\n%s", app.releaseNotesPopup.View())
	}
	// A check lands while the current version's notes are open.
	app.updateAvailable = &UpdateStatusMsg{Version: "2.0.0", TagName: "v2.0.0", ReleaseNotes: "unread"}
	_, cmd := app.handleKey(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if cmd != nil {
		cmd()
	}
	if applied || !app.releaseNotesPopup.isOpen() {
		t.Fatalf("U in viewer mode: applied=%v open=%v; want neither applied nor closed", applied, app.releaseNotesPopup.isOpen())
	}

	// Beside the pending update's own notes U is offered and closes the overlay.
	app.releaseNotesPopup.open("v2.0.0", "unread", 80, 24)
	app.releaseNotesPopup.setPending(true)
	if !strings.Contains(app.releaseNotesPopup.View(), "U: Apply update") {
		t.Fatal("footer must offer U beside a pending update")
	}
	app.handleKey(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if app.releaseNotesPopup.isOpen() {
		t.Error("U beside a pending update should hand over to the apply flow and close the notes")
	}
}

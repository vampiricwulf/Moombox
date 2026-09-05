package tui

import (
	"strings"
	"testing"
)

// TestUpdateStatusMsgEmptyClearsBadge: the periodic-check channel carries an
// UpdateStatusMsg with a Version to LIGHT the badge, and an EMPTY one to clear
// it. The empty message is what a Web-side POST /api/update/dismiss sends
// (cmd/moombox/routes_wiring.go OnDismissed) so the dashboard's dismiss is not
// contradicted by a TUI still advertising the release.
func TestUpdateStatusMsgEmptyClearsBadge(t *testing.T) {
	app := NewApp()
	app.SetVersion("2.6.0-test")
	app.details.SetSize(80, 24)

	app.Update(UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9", ReleaseNotes: "notes"})
	if app.updateAvailable == nil || app.updateAvailable.TagName != "v9.9.9" {
		t.Fatalf("a versioned UpdateStatusMsg must set the badge, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo == nil {
		t.Fatal("a versioned UpdateStatusMsg must set details.updateInfo")
	}
	if !strings.Contains(app.details.View(), "Update!") {
		t.Fatal("the details header must render the update glyph while an update is pending")
	}

	app.Update(UpdateStatusMsg{})
	if app.updateAvailable != nil {
		t.Errorf("an empty UpdateStatusMsg must clear updateAvailable, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo != nil {
		t.Errorf("an empty UpdateStatusMsg must clear details.updateInfo, got %#v", app.details.updateInfo)
	}
	if strings.Contains(app.details.View(), "Update!") {
		t.Error("the details header must not render the update glyph after a clear")
	}
}

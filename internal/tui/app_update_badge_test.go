package tui

import (
	"errors"
	"strings"
	"testing"
)

// lightBadge puts a pending release in front of the app the way the periodic
// check does, and fails the test if the badge and the details glyph are not
// both lit — everything below is about what CLEARS them.
func lightBadge(t *testing.T, app *App, tag string) {
	t.Helper()
	app.Update(UpdateStatusMsg{Version: strings.TrimPrefix(tag, "v"), TagName: tag, ReleaseNotes: "notes"})
	if app.updateAvailable == nil || app.updateAvailable.TagName != tag {
		t.Fatalf("a versioned UpdateStatusMsg must set the badge, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo == nil {
		t.Fatal("a versioned UpdateStatusMsg must set details.updateInfo")
	}
	if !strings.Contains(app.details.View(), "Update!") {
		t.Fatal("the details header must render the update glyph while an update is pending")
	}
}

func newBadgeApp(t *testing.T) *App {
	t.Helper()
	app := NewApp()
	app.SetVersion("2.6.0-test")
	app.details.SetSize(80, 24)
	return app
}

// TestUpdateStatusMsgEmptyClearsBadge: the periodic-check channel carries an
// UpdateStatusMsg with a Version to LIGHT the badge, and one with an empty
// Version to clear it. The cleared message is what a Web-side
// POST /api/update/dismiss sends (cmd/moombox/routes_wiring.go OnDismissed) so
// the dashboard's dismiss is not contradicted by a TUI still advertising the
// release — and it carries the TAG that was skipped.
func TestUpdateStatusMsgEmptyClearsBadge(t *testing.T) {
	app := newBadgeApp(t)
	lightBadge(t, app, "v9.9.9")

	app.Update(UpdateStatusMsg{TagName: "v9.9.9"})
	if app.updateAvailable != nil {
		t.Errorf("a cleared UpdateStatusMsg naming the pending tag must clear updateAvailable, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo != nil {
		t.Errorf("a cleared UpdateStatusMsg must clear details.updateInfo, got %#v", app.details.updateInfo)
	}
	if strings.Contains(app.details.View(), "Update!") {
		t.Error("the details header must not render the update glyph after a clear")
	}
}

// A dismiss can race a newly-found release: the Web skips v9.9.9 while the
// periodic check has already replaced the pending release with v9.9.10. The
// clear then names the OLD tag, and blanking the badge on it would hide an
// update nobody skipped.
func TestUpdateStatusMsgClearForAnotherTagKeepsTheBadge(t *testing.T) {
	app := newBadgeApp(t)
	lightBadge(t, app, "v9.9.10")

	app.Update(UpdateStatusMsg{TagName: "v9.9.9"})
	if app.updateAvailable == nil || app.updateAvailable.TagName != "v9.9.10" {
		t.Errorf("a clear naming a DIFFERENT tag must leave the badge alone, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo == nil {
		t.Error("a clear naming a different tag must leave details.updateInfo alone")
	}
	if !strings.Contains(app.details.View(), "Update!") {
		t.Error("the details header must still render the update glyph")
	}
}

// The TUI's OWN skip (S in the release-notes overlay) has to clear exactly what
// the Web dismiss clears. It used to drop only updateAvailable, leaving the
// details header's glyph lit for a release the operator had just skipped
// (review F2).
func TestDismissUpdateResultClearsBadgeAndGlyph(t *testing.T) {
	app := newBadgeApp(t)
	lightBadge(t, app, "v9.9.9")

	app.Update(dismissUpdateResultMsg{Tag: "v9.9.9"})
	if app.updateAvailable != nil {
		t.Errorf("the S skip must clear updateAvailable, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo != nil {
		t.Errorf("the S skip must clear details.updateInfo too, got %#v", app.details.updateInfo)
	}
	if strings.Contains(app.details.View(), "Update!") {
		t.Error("the details header must not render the update glyph after the operator skipped the release")
	}
}

// A skip that FAILED to persist changes nothing: the release is still pending,
// so the badge and the glyph must both survive.
func TestDismissUpdateResultFailureKeepsTheBadge(t *testing.T) {
	app := newBadgeApp(t)
	lightBadge(t, app, "v9.9.9")

	app.Update(dismissUpdateResultMsg{Tag: "v9.9.9", Err: errors.New("disk full")})
	if app.updateAvailable == nil || app.details.updateInfo == nil {
		t.Errorf("a failed skip must leave the pending release in place, got %#v / %#v",
			app.updateAvailable, app.details.updateInfo)
	}
}

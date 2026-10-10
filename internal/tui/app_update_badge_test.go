package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
// POST /api/update/dismiss sends (cmd/moombox/routes_wiring.go OnCleared) so
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

// A placed update is restart-pending: the updater refuses a second apply
// until the process restarts, so the badge and R U have nothing left to
// offer. A failed apply leaves both, since installing is still possible.
//
// Mutant: the success branch leaving updateAvailable set.
func TestAnAppliedUpdateClearsTheBadge(t *testing.T) {
	app := newBadgeApp(t)
	lightBadge(t, app, "v9.9.9")

	app.Update(updateApplyResultMsg{Err: "download failed: boom"})
	if app.updateAvailable == nil || app.details.updateInfo == nil {
		t.Fatal("a failed apply must leave the pending release offered")
	}

	app.Update(updateApplyResultMsg{})
	if app.updateAvailable != nil {
		t.Errorf("an applied update must clear updateAvailable, got %#v", app.updateAvailable)
	}
	if app.details.updateInfo != nil {
		t.Error("an applied update must clear details.updateInfo")
	}
	if strings.Contains(app.details.View(), "Update!") {
		t.Error("the details header must not render the update glyph after the update is applied")
	}
}

// rvCheck presses R V against a check that answers answer, and returns the
// result message for the test to deliver when it chooses: whatever reaches
// the TUI before it arrived during the check's round trip.
func rvCheck(t *testing.T, app *App, answer *UpdateStatusMsg) tea.Msg {
	t.Helper()
	app.OnCheckUpdate = func() (*UpdateStatusMsg, error) { return answer, nil }
	_, cmd := app.dispatchAction("R V", nil)
	if cmd == nil {
		t.Fatal("R V started no check")
	}
	return cmd()
}

// R V answering "Already up to date" means a release this TUI still offers
// was pulled: its download no longer exists, and R U would fetch a dead
// asset.
//
// Mutant: the up-to-date branch leaving updateAvailable set.
func TestAnUpToDateCheckClearsTheBadge(t *testing.T) {
	app := newBadgeApp(t)
	lightBadge(t, app, "v9.9.9")

	app.Update(rvCheck(t, app, nil))
	if app.updateAvailable != nil || app.details.updateInfo != nil {
		t.Errorf("an up-to-date check left the pulled release offered: %#v", app.updateAvailable)
	}
	if !strings.Contains(app.feedback.msg, "Already up to date") {
		t.Errorf("feedback = %q", app.feedback.msg)
	}
}

// An up-to-date answer drops the badge only while it shows the release it
// showed when R V was pressed. A release that reached the TUI during the
// round trip — another check found it after this one's answer was decided —
// is still pending on the server, and every dashboard offers it; R V used to
// drop it, after which R U said "No update available".
//
// Mutants: R V not capturing the badge (`Seen` left nil) — the release
// already shown is not cleared; the result clearing whatever is shown — the
// release found during the round trip goes.
func TestAnUpToDateCheckKeepsAReleaseFoundDuringIt(t *testing.T) {
	for _, before := range []string{"", "v9.9.1"} {
		app := newBadgeApp(t)
		if before != "" {
			lightBadge(t, app, before)
		}
		result := rvCheck(t, app, nil)
		app.Update(UpdateStatusMsg{Version: "9.9.2", TagName: "v9.9.2", ReleaseNotes: "n"})
		app.Update(result)
		if app.updateAvailable == nil || app.updateAvailable.TagName != "v9.9.2" || app.details.updateInfo == nil {
			t.Errorf("badge %q before R V: an up-to-date answer dropped v9.9.2, found during it (badge %#v)", before, app.updateAvailable)
			continue
		}
		if !strings.Contains(app.feedback.msg, "v9.9.2") {
			t.Errorf("badge %q before R V: feedback %q, want it to name the release still pending", before, app.feedback.msg)
		}
	}
}

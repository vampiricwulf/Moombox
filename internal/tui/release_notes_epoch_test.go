package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// S, Esc, R N again before the skip answers: the answer belongs to the
// session S was pressed in, so it no longer closes the notes reopened
// since (K27). The release's own state still follows it: the badge and
// glyph clear, and the reopened notes — on the release just skipped —
// stop offering U and S.
//
// Mutant: dismissUpdateResultMsg closing the overlay whatever its epoch —
// the reopened notes are closed.
func TestReleaseNotesLateSkipAnswerLeavesReopenedNotesOpen(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.OnDismissUpdate = func(string) error { return nil }
	a.updateAvailable = &UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9", ReleaseNotes: "notes"}
	a.details.updateInfo = a.updateAvailable

	a.dispatchAction("R N", nil)
	_, skip := a.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if skip == nil {
		t.Fatal("S sent nothing")
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	a.dispatchAction("R N", nil)
	if !a.releaseNotesPopup.isOpen() || !a.releaseNotesPopup.pending {
		t.Fatal("R N did not reopen the pending notes")
	}

	a.Update(runCmd(t, skip))
	if !a.releaseNotesPopup.isOpen() {
		t.Fatal("the late skip answer closed the notes reopened since")
	}
	if a.updateAvailable != nil || a.details.updateInfo != nil {
		t.Error("the skip answer must still clear the badge and the glyph")
	}
	if a.releaseNotesPopup.pending || !strings.Contains(a.releaseNotesPopup.notice, "skipped") {
		t.Errorf("reopened notes on the skipped release: pending=%v notice=%q, want withdrawn",
			a.releaseNotesPopup.pending, a.releaseNotesPopup.notice)
	}
	if !strings.HasPrefix(a.feedback.msg, "Skipped v9.9.9") {
		t.Errorf("feedback = %q", a.feedback.msg)
	}
}

// A late skip answer for a session that was closed and not reopened still
// clears the release; and S in the reopened session works.
func TestReleaseNotesSkipInTheReopenedSessionStillCloses(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.OnDismissUpdate = func(string) error { return nil }
	a.updateAvailable = &UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9", ReleaseNotes: "notes"}

	a.dispatchAction("R N", nil)
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	a.dispatchAction("R N", nil)
	_, skip := a.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	a.Update(runCmd(t, skip))
	if a.releaseNotesPopup.isOpen() || a.updateAvailable != nil {
		t.Fatal("S's own answer must close its notes and clear the release")
	}
}

// R N, Esc, R N again: the first fetch's error lands after the second's
// notes loaded and used to replace them with "Failed to fetch" (K27).
//
// Mutant: releaseNotesFetchedMsg without the epoch match — the notes read
// "Failed to fetch release notes".
func TestReleaseNotesLateFetchErrorLeavesLoadedNotesAlone(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.SetVersion("1.0.0")
	calls := 0
	a.OnFetchReleaseNotes = func(string) (string, string, error) {
		calls++
		if calls == 1 {
			return "", "", errors.New("timeout")
		}
		return "v1.0.0", "good notes", nil
	}
	_, first := a.dispatchAction("R N", nil)
	firstMsg := runCmd(t, first) // the fetch fails, but its answer is slow to land
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, second := a.dispatchAction("R N", nil)
	a.Update(runCmd(t, second))
	if a.releaseNotesPopup.rawNotes != "good notes" {
		t.Fatalf("the second fetch did not load: %q", a.releaseNotesPopup.rawNotes)
	}

	a.Update(firstMsg)
	if a.releaseNotesPopup.rawNotes != "good notes" {
		t.Errorf("a late fetch error replaced the loaded notes: %q", a.releaseNotesPopup.rawNotes)
	}
}

// The viewer's own fetch is applied, error included, and the error keeps
// the tag R N opened with.
func TestReleaseNotesOwnFetchIsApplied(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.SetVersion("1.0.0")
	a.OnFetchReleaseNotes = func(string) (string, string, error) { return "", "", errors.New("timeout") }
	_, cmd := a.dispatchAction("R N", nil)
	a.Update(runCmd(t, cmd))
	if !strings.HasPrefix(a.releaseNotesPopup.rawNotes, "Failed to fetch release notes: timeout") ||
		a.releaseNotesPopup.tag != "v1.0.0" {
		t.Errorf("own fetch error not shown: tag=%q notes=%q", a.releaseNotesPopup.tag, a.releaseNotesPopup.rawNotes)
	}
}

// Each open of a session and each close names a new epoch.
func TestReleaseNotesEpochBumpsOnOpenAndClose(t *testing.T) {
	o := newReleaseNotesOverlay()
	e0 := o.epoch
	o.start("v1", "n", 80, 24)
	e1 := o.epoch
	o.open("v1", "re-rendered", 80, 24) // a fetch answer re-renders in place
	if o.epoch != e1 {
		t.Error("re-rendering the notes must not start a new session")
	}
	o.close()
	if e1 == e0 || o.epoch == e1 {
		t.Errorf("epochs %d → %d → %d, want each open and close to bump", e0, e1, o.epoch)
	}
}

// U and S share one rule: the notes on screen are the pending release's.
func TestReleaseNotesApplyAndSkipRule(t *testing.T) {
	a := NewApp()
	a.OnDismissUpdate = func(string) error { return nil }
	a.updateAvailable = &UpdateStatusMsg{TagName: "v2"}
	a.releaseNotesPopup.start("v2", "n", 80, 24)
	a.releaseNotesPopup.setPending(true)
	if !a.canApplyFromNotes() || !a.canSkipFromNotes() {
		t.Fatal("pending notes of the pending release offer both")
	}
	a.releaseNotesPopup.skipping = "v2"
	if !a.canApplyFromNotes() || a.canSkipFromNotes() {
		t.Error("a skip in flight refuses a second S only")
	}
	a.releaseNotesPopup.skipping = ""
	a.updateAvailable = &UpdateStatusMsg{TagName: "v3"}
	if a.canApplyFromNotes() || a.canSkipFromNotes() {
		t.Error("another release pending: neither")
	}
	a.updateAvailable = &UpdateStatusMsg{TagName: "v2"}
	a.releaseNotesPopup.setPending(false)
	if a.canApplyFromNotes() || a.canSkipFromNotes() {
		t.Error("viewer notes: neither")
	}
}

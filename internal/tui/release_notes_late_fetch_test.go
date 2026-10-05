package tui

import "testing"

// TestReleaseNotesLateFetchLeavesPendingNotesAlone: R N with no update fetches
// the running version's notes; close it, an update arrives, R N again shows
// the pending update's notes with U/S — and the first fetch's late answer
// replaced them with v1.0.0's, under a footer still offering U and S, both of
// which then refused on the tag guard.
//
// Mutant: dropping the pending gate — the overlay shows v1.0.0.
func TestReleaseNotesLateFetchLeavesPendingNotesAlone(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.SetVersion("1.0.0")
	a.OnFetchReleaseNotes = func(string) (string, string, error) { return "v1.0.0", "old notes", nil }
	a.dispatchAction("R N", nil)
	a.releaseNotesPopup.close()
	a.updateAvailable = &UpdateStatusMsg{Version: "2.0.0", TagName: "v2.0.0", ReleaseNotes: "new notes"}
	a.dispatchAction("R N", nil)

	a.Update(releaseNotesFetchedMsg{Tag: "v1.0.0", Notes: "old notes"})
	if a.releaseNotesPopup.tag != "v2.0.0" || !a.releaseNotesPopup.pending {
		t.Errorf("overlay is tag=%q pending=%v, want the pending v2.0.0 notes untouched",
			a.releaseNotesPopup.tag, a.releaseNotesPopup.pending)
	}

	// The viewer it was meant for still gets its answer.
	a.releaseNotesPopup.close()
	a.updateAvailable = nil
	a.dispatchAction("R N", nil)
	a.Update(releaseNotesFetchedMsg{Tag: "v1.0.0", Notes: "old notes"})
	if a.releaseNotesPopup.tag != "v1.0.0" {
		t.Errorf("the viewer's own fetch was not applied: tag %q", a.releaseNotesPopup.tag)
	}
}

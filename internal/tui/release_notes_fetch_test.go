package tui

import (
	"errors"
	"strings"
	"testing"
)

// TestReleaseNotesOverlayKeepsItsVersionWhenTheFetchFails: R N with no update
// pending opens the overlay titled with the running version and fetches its
// notes; the wiring's OnFetchReleaseNotes returns ("", "", err) on failure,
// and the result handler reopened the overlay with that empty tag, blanking
// the title to "Release Notes — " above the error. The tag the overlay was
// opened with is kept whenever the result carries none.
//
// Mutant: pass msg.Tag straight through — the title loses its version.
func TestReleaseNotesOverlayKeepsItsVersionWhenTheFetchFails(t *testing.T) {
	app := NewApp()
	app.width, app.height = 100, 40
	app.SetVersion("1.2.3")
	app.OnFetchReleaseNotes = func(string) (string, string, error) {
		return "", "", errors.New("rate limited")
	}

	_, cmd := app.dispatchAction("R N", nil)
	if app.releaseNotesPopup == nil || !app.releaseNotesPopup.isOpen() || app.releaseNotesPopup.tag != "v1.2.3" {
		t.Fatalf("premise lost: R N did not open the overlay for v1.2.3 (%+v)", app.releaseNotesPopup)
	}

	msg := runCmd(t, cmd)
	fm, ok := msg.(releaseNotesFetchedMsg)
	if !ok || fm.Err == "" || fm.Tag != "" {
		t.Fatalf("premise lost: a failed fetch is expected to carry an error and no tag, got %#v", msg)
	}
	app.Update(msg)

	if got := app.releaseNotesPopup.tag; got != "v1.2.3" {
		t.Errorf("the overlay's tag after the failed fetch is %q, want v1.2.3", got)
	}
	v := stripANSI(app.releaseNotesPopup.View())
	if !strings.Contains(v, "Release Notes — v1.2.3") {
		t.Errorf("the title lost its version:\n%s", v)
	}
	if !strings.Contains(v, "rate limited") {
		t.Errorf("the fetch error is not shown:\n%s", v)
	}
}

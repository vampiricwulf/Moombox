package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestStatusLabelMatchesTheWebUI: COOKIES? reads "Auth Required" — the label
// the Web UI already shows (web/public/app.js, statusLabel) — and every other
// status is its own name. Colour and icon key off the raw status, so a wrong
// label here would not change them; this pins the text.
func TestStatusLabelMatchesTheWebUI(t *testing.T) {
	if got := StatusLabel(string(database.StatusCookies)); got != "Auth Required" {
		t.Fatalf("StatusLabel(COOKIES?) = %q, want %q", got, "Auth Required")
	}
	for _, s := range []database.JobStatus{
		database.StatusUpcoming, database.StatusLive, database.StatusDownloading, database.StatusMuxing,
		database.StatusFinished, database.StatusError, database.StatusCancelled, database.StatusQueued,
	} {
		if got := StatusLabel(string(s)); got != string(s) {
			t.Errorf("StatusLabel(%s) = %q, want the status itself", s, got)
		}
	}
	if got := StatusLabel("Whatever"); got != "Whatever" {
		t.Errorf("unknown status must pass through, got %q", got)
	}
}

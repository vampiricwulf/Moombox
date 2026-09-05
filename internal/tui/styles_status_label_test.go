package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
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

// TestActionMenuAndHintWording pins the operator-facing strings this arc
// reworded so a later edit cannot drift them apart from the docs.
func TestActionMenuAndHintWording(t *testing.T) {
	app := NewApp()
	app.OnForceRefreshCookies = func() (cookies.RefreshResult, error) { return cookies.RefreshResult{}, nil }
	want := map[string][2]string{ // chord → {Label or DisabledReason, HintLabel}
		"R F": {"Refresh Cookies from Browser", "Refresh Cookies"},
	}
	reasons := map[string]string{
		"A R": "no jobs to resume",
		"A I": "no jobs to reinitialize",
	}
	seen := map[string]bool{}
	for _, it := range app.buildMenuItems() {
		if w, ok := want[it.Chord]; ok {
			seen[it.Chord] = true
			if it.Label != w[0] || it.HintLabel != w[1] {
				t.Errorf("%s: Label/HintLabel = %q/%q, want %q/%q", it.Chord, it.Label, it.HintLabel, w[0], w[1])
			}
		}
		if r, ok := reasons[it.Chord]; ok {
			seen[it.Chord] = true
			if it.DisabledReason != r {
				t.Errorf("%s: DisabledReason = %q, want %q", it.Chord, it.DisabledReason, r)
			}
		}
	}
	for _, c := range []string{"R F", "A R", "A I"} {
		if !seen[c] {
			t.Errorf("chord %s missing from buildMenuItems()", c)
		}
	}
}

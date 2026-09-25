package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestActiveJobStatusListsAgree pins the one list that decides whether a job's
// staging directory is being written, and therefore whether set-aside recovery
// may touch it.
//
// worker.IsActiveJobStatus (internal/worker/orphans.go) is the source of
// truth: the worker and the REST route both refuse a recovery on it.
// tui.JobIsActive is a deliberate re-declaration, because internal/tui cannot
// import internal/worker (the import fence) — and nothing was comparing them.
// The dashboard's literal in web/public/modules/job-details.js is the third
// reader; its own jsdom test walks all four statuses plus Finished.
//
// Mutant: adding a status to one side, or dropping one — this names it.
func TestActiveJobStatusListsAgree(t *testing.T) {
	all := []database.JobStatus{
		database.StatusQueued,
		database.StatusUpcoming,
		database.StatusLive,
		database.StatusDownloading,
		database.StatusMuxing,
		database.StatusFinished,
		database.StatusError,
		database.StatusCancelled,
		database.StatusCookies,
	}
	active := 0
	for _, s := range all {
		w, ui := worker.IsActiveJobStatus(s), tui.JobIsActive(s)
		if w != ui {
			t.Errorf("status %q: worker.IsActiveJobStatus = %v but tui.JobIsActive = %v", s, w, ui)
		}
		if w {
			active++
		}
	}
	// A guard against the guard: if the worker's set ever emptied, every row
	// above would agree vacuously.
	if active != 4 {
		t.Errorf("worker.IsActiveJobStatus reports %d active statuses over the full list, want 4 (Upcoming, Live, Downloading, Muxing) — "+
			"if this list genuinely changed, update the dashboard literal in web/public/modules/job-details.js too", active)
	}
}

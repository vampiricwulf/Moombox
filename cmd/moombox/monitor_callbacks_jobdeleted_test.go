package main

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// TestJobDeletionPrunesOnlyThatJobsLogBuffer pins the fix for a delete that
// walked the whole jobs table to prune one buffer — and, when that read came
// back empty (the old helper swallowed the error and returned an empty
// slice), deleted EVERY buffer instead of one.
//
// The second buffer here belongs to an ID the jobs table does not hold: that is
// precisely the shape a failed GetAllJobs produces for every job at once, and
// the only shape in which the old and new implementations disagree.
//
// Mutant: restoring the old activeIDs-derived PruneJobLogs body wipes "ghost"
// too and fails this.
func TestJobDeletionPrunesOnlyThatJobsLogBuffer(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.AddJob(&database.Job{
		ID: "doomed", VideoID: "doomed", URL: "https://example.invalid/doomed",
		Platform: "youtube", Status: database.StatusFinished,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	db.TrackJobForLogs("doomed")
	db.AddJobLog("doomed", "line for doomed")
	db.TrackJobForLogs("ghost")
	db.AddJobLog("ghost", "line for ghost")

	s := &runState{db: db, wsHub: web.NewWebSocketHub(sweepTestLogger{})}
	s.onJobDeleted("doomed")

	if got := db.GetJobLogs("doomed"); len(got) != 0 {
		t.Errorf("the deleted job's buffer must be cleared, got %v", got)
	}
	if got := db.GetJobLogs("ghost"); len(got) != 1 {
		t.Errorf("another job's buffer was collateral damage: got %v, want the one line it had", got)
	}
}

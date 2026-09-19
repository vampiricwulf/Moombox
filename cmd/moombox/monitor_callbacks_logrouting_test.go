package main

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// logRoutingFixture opens a real database and wires the two production
// subscriber bodies that decide per-job log routing — nothing is replayed or
// re-implemented here, so a mutation of either one fails these tests.
func logRoutingFixture(t *testing.T) (*database.Database, *runState) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := &runState{db: db}
	t.Cleanup(db.OnJobAdded(func(ev *database.JobAdded) { s.syncJobLogRouting(ev.Job) }))
	t.Cleanup(db.OnJobChange(s.syncJobLogRoutingOnChange))
	return db, s
}

func addJob(t *testing.T, db *database.Database, id string, status database.JobStatus) {
	t.Helper()
	if _, err := db.AddJob(&database.Job{
		ID: id, VideoID: id, URL: "https://example.invalid/" + id,
		Platform: "youtube", Status: status,
	}); err != nil {
		t.Fatalf("AddJob(%s): %v", id, err)
	}
}

// A job that LEAVES a terminal state must be routed to again. /retry
// (ReinitializeJob), /resume (ResumeJob) and auto-retry all resurrect a job
// with a plain UpdateJobFields(status=…), which fires only OnJobChange — and
// that subscriber used to untrack and never re-track, so the whole re-run
// produced no per-job log lines until a restart (B-1). Before this fix the
// buffer held 1 line; it holds 3 now.
//
// Mutant: dropping the track branch from syncJobLogRouting (untrack only) —
// the retried job collects nothing and this fails at 1 line.
func TestRetryReRoutesLogsToTheRevivedJob(t *testing.T) {
	db, _ := logRoutingFixture(t)
	addJob(t, db, "vid0000001", database.StatusDownloading)

	db.RouteLogToJobs("2026-09-17 12:00:00 INFO vid0000001 download started")

	if got := db.UpdateJobFields("vid0000001", map[string]any{"status": database.StatusError}); got == nil {
		t.Fatal("UpdateJobFields(status=Error) returned nil")
	}
	db.RouteLogToJobs("2026-09-17 12:00:01 ERROR vid0000001 download failed")
	if got := db.GetJobLogs("vid0000001"); len(got) != 1 {
		t.Fatalf("a terminal job must stop collecting, holds %d lines", len(got))
	}

	// The retry: exactly what ReinitializeJob writes.
	if got := db.UpdateJobFields("vid0000001", map[string]any{"status": database.StatusDownloading}); got == nil {
		t.Fatal("UpdateJobFields(status=Downloading) returned nil")
	}
	db.RouteLogToJobs("2026-09-17 12:00:02 INFO vid0000001 download started (retry)")
	db.RouteLogToJobs("2026-09-17 12:00:03 INFO vid0000001 segment 1")

	got := db.GetJobLogs("vid0000001")
	if len(got) != 3 {
		t.Errorf("the revived job holds %d lines, want 3 (the first plus the two from the re-run): %v", len(got), got)
	}
	if len(got) > 0 && got[0] != "2026-09-17 12:00:00 INFO vid0000001 download started" {
		t.Errorf("the buffer from before the failure must survive the retry, first line is %q", got[0])
	}
}

// The ZIP archive import (internal/web/routes/import_routes.go) really does
// AddJob a Finished job, and OnJobAdded used to track every added row — so
// every import seeded a terminal ID into the routed set that only a restart
// removed (B-3). The add path uses the same predicate as the change path.
//
// Mutant: OnJobAdded tracking unconditionally (s.db.TrackJobForLogs(job.ID))
// — the imported job collects a line and this fails.
func TestImportedFinishedJobIsNotLogRouted(t *testing.T) {
	db, _ := logRoutingFixture(t)
	addJob(t, db, "imported01", database.StatusFinished)

	db.RouteLogToJobs("2026-09-17 12:00:00 INFO imported01 something happened")

	if got := db.GetJobLogs("imported01"); got != nil {
		t.Errorf("an imported Finished job must not be routed to, holds %v", got)
	}
}

// OnJobChange also fires for every ~60 Hz progress write, so the routing
// decision is gated on the status column actually having been written: the
// progress pipeline must not touch jobLogsMu at all. A status write still
// re-routes immediately.
//
// Mutant: dropping the ev.Changes gate — the progress write re-creates the
// cleared buffer and the first assertion fails.
func TestProgressWritesDoNotTouchLogRouting(t *testing.T) {
	db, _ := logRoutingFixture(t)
	addJob(t, db, "vid0000002", database.StatusDownloading)
	db.ClearJobLogs("vid0000002")

	if got := db.UpdateJobFields("vid0000002", map[string]any{"progress": "V:12 A:12"}); got == nil {
		t.Fatal("UpdateJobFields(progress) returned nil")
	}
	db.RouteLogToJobs("2026-09-17 12:00:00 INFO vid0000002 progress line")
	if got := db.GetJobLogs("vid0000002"); got != nil {
		t.Errorf("a progress-only write must not re-enter the routed set, holds %v", got)
	}

	if got := db.UpdateJobFields("vid0000002", map[string]any{"status": database.StatusMuxing}); got == nil {
		t.Fatal("UpdateJobFields(status=Muxing) returned nil")
	}
	db.RouteLogToJobs("2026-09-17 12:00:01 INFO vid0000002 muxing")
	if got := db.GetJobLogs("vid0000002"); len(got) != 1 {
		t.Errorf("a status write must route again, holds %d lines", len(got))
	}
}

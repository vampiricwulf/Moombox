package database

import (
	"sync/atomic"
	"testing"
)

// casFixture opens a database holding one job in status, and counts the
// OnJobUpdate and OnJobChange notifications that follow.
func casFixture(t *testing.T, id string, status JobStatus) (*Database, *atomic.Int32) {
	t.Helper()
	db := newTestDB(t)
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&Job{ID: id, VideoID: id, URL: "u", Status: status}); err != nil {
		t.Fatal(err)
	}
	var notified atomic.Int32
	db.OnJobUpdate(func(j *Job) {
		if j.ID == id {
			notified.Add(1)
		}
	})
	db.OnJobChange(func(c *JobChange) {
		if c.Job != nil && c.Job.ID == id {
			notified.Add(1)
		}
	})
	return db, &notified
}

// TestUpdateJobFieldsIfIsACompareAndSet: the write applies only while the
// row's status is the expected one, says whether it did, and a write that
// did not apply leaves the row as it was and tells no subscriber anything.
// A row that does not exist is not written either.
//
// Mutants: drop the condition from the statement — the write lands on a row
// that left the expected status; drop the RowsAffected check — the missed
// write still reports applied and notifies.
func TestUpdateJobFieldsIfIsACompareAndSet(t *testing.T) {
	t.Parallel()
	db, notified := casFixture(t, "cas", StatusQueued)

	if !db.UpdateJobFieldsIf("cas", StatusQueued, map[string]any{"status": StatusUpcoming}) {
		t.Fatal("the write on a Queued row did not apply")
	}
	if row, _ := db.GetJob("cas"); row.Status != StatusUpcoming {
		t.Fatalf("status = %s after an applied write, want Upcoming", row.Status)
	}
	if n := notified.Load(); n != 2 {
		t.Fatalf("notifications after an applied write = %d, want one per subscriber (2)", n)
	}

	// An operator's Cancel lands; the stale admission must not undo it.
	db.UpdateJobFields("cas", map[string]any{"status": StatusCancelled})
	notified.Store(0)
	if db.UpdateJobFieldsIf("cas", StatusQueued, map[string]any{"status": StatusUpcoming, "progress": "admitted"}) {
		t.Error("the write on a Cancelled row reported applied")
	}
	row, _ := db.GetJob("cas")
	if row.Status != StatusCancelled || row.Progress != "" {
		t.Errorf("row = %s %q after a write that did not apply, want Cancelled and untouched", row.Status, row.Progress)
	}
	if n := notified.Load(); n != 0 {
		t.Errorf("a write that did not apply notified %d subscribers", n)
	}

	if db.UpdateJobFieldsIf("missing", StatusQueued, map[string]any{"status": StatusUpcoming}) {
		t.Error("a write on a row that does not exist reported applied")
	}
}

// TestUpdateJobFieldsUnlessSparesOneStatus: the write applies over any status
// but the one named, which it leaves alone.
//
// Mutant: compare with = instead of <> — the write lands only on the status
// it was meant to spare.
func TestUpdateJobFieldsUnlessSparesOneStatus(t *testing.T) {
	t.Parallel()
	db, notified := casFixture(t, "unless", StatusDownloading)

	if !db.UpdateJobFieldsUnless("unless", StatusCancelled, map[string]any{"status": StatusError, "error": "boom"}) {
		t.Fatal("the write on a Downloading row did not apply")
	}
	if row, _ := db.GetJob("unless"); row.Status != StatusError || row.Error != "boom" {
		t.Fatalf("row = %s %q, want Error \"boom\"", row.Status, row.Error)
	}

	db.UpdateJobFields("unless", map[string]any{"status": StatusCancelled, "error": ""})
	notified.Store(0)
	if db.UpdateJobFieldsUnless("unless", StatusCancelled, map[string]any{"status": StatusError, "error": "late"}) {
		t.Error("the write on a Cancelled row reported applied")
	}
	if row, _ := db.GetJob("unless"); row.Status != StatusCancelled || row.Error != "" {
		t.Errorf("row = %s %q, want the Cancel left standing", row.Status, row.Error)
	}
	if n := notified.Load(); n != 0 {
		t.Errorf("a write that did not apply notified %d subscribers", n)
	}
}

// TestUpdateJobFieldsUnlessTerminalSparesEveryOutcome: the write applies over
// every status a job is still on its way through and over none it ends in —
// Finished, Error, Cancelled, the statuses IsTerminal reports — and one it
// spares tells no subscriber anything.
//
// Mutants: drop a status from terminalStatuses — the write lands on that
// outcome; compare with IN instead of NOT IN — it lands only on outcomes.
func TestUpdateJobFieldsUnlessTerminalSparesEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status   JobStatus
		terminal bool
	}{
		{StatusQueued, false}, {StatusUpcoming, false}, {StatusLive, false}, {StatusDownloading, false},
		{StatusMuxing, false}, {StatusCookies, false},
		{StatusFinished, true}, {StatusError, true}, {StatusCancelled, true},
	} {
		st := tc.status
		if got := (&Job{Status: st}).IsTerminal(); got != tc.terminal {
			t.Errorf("%s: IsTerminal = %v, want %v", st, got, tc.terminal)
		}
		db, notified := casFixture(t, "t", st)
		applied := db.UpdateJobFieldsUnlessTerminal("t", map[string]any{"status": StatusCancelled, "progress": "cancelled"})
		if applied == tc.terminal {
			t.Errorf("%s: applied = %v, want %v", st, applied, !tc.terminal)
		}
		row, _ := db.GetJob("t")
		if tc.terminal && (row.Status != st || row.Progress != "" || notified.Load() != 0) {
			t.Errorf("%s: row = %s %q with %d notifications, want it untouched", st, row.Status, row.Progress, notified.Load())
		}
		if !tc.terminal && row.Status != StatusCancelled {
			t.Errorf("%s: status = %s after an applied write, want Cancelled", st, row.Status)
		}
	}
}

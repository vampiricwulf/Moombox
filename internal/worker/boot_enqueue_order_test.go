package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestBootOffersInterruptedCapturesBeforeWaiters: the queue's backlog cap
// (100 pending) is checked before priority, and the boot pass used to offer
// rows in the order the database returns them (most recently updated first).
// A Live or Downloading capture the restart interrupted, sitting behind a
// hundred fresher Upcoming rows, was dropped until the first heartbeat a
// minute later. The pass offers Live, then Downloading, then Upcoming, so the
// cap can only ever delay an Upcoming waiter.
//
// Mutant: offering rows in read order — the Live and Downloading rows, the
// oldest here, are the ones past the cap and are not pending.
func TestBootOffersInterruptedCapturesBeforeWaiters(t *testing.T) {
	w, db := testWorkerSetup(t)
	add := func(id string, st database.JobStatus) {
		t.Helper()
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	// Oldest first: the read is newest first, so these two come last.
	add("downloading", database.StatusDownloading)
	add("live", database.StatusLive)
	for i := range 150 {
		add(jobIDf(i), database.StatusUpcoming)
		// Touch the row so its updated_at is not older than the captures'.
		db.UpdateJobFields(jobIDf(i), map[string]any{"title": "waiter"})
	}
	jobs, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for i, j := range jobs {
		if (j.ID == "live" || j.ID == "downloading") && i < 100 {
			t.Fatalf("%s read at index %d: the fixture did not put the captures past the cap", j.ID, i)
		}
	}

	w.enqueueExistingJobs()

	if got := w.queue.PendingCount(); got != 100 {
		t.Fatalf("PendingCount = %d, want the cap of 100", got)
	}
	for _, id := range []string{"live", "downloading"} {
		if !w.queue.isPending(id) {
			t.Errorf("%s row was dropped at boot behind Upcoming waiters", id)
		}
	}
	if id, _, ok := w.queue.Dequeue(t.Context()); !ok || id != "live" {
		t.Errorf("first Dequeue = %q, %v; want the Live row", id, ok)
	}
}

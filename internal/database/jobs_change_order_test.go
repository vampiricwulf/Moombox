package database

import (
	"testing"
	"time"
)

// TestJobsChangeDropsASnapshotOlderThanOneDelivered: dispatchJobsChange hands
// each full list to subscribers on its own goroutine, and those are not
// ordered — two bulk writes in quick succession (a backfill pruning several
// departed channels) could deliver the OLDER list last, and every subscriber
// replaces its whole job list from it, so pruned jobs reappeared. A snapshot
// older than one already delivered is now dropped.
//
// Mutant: drop the `snap.seq <= db.jobsChangeDelivered` check — the stale
// one-job list is delivered after the empty one.
func TestJobsChangeDropsASnapshotOlderThanOneDelivered(t *testing.T) {
	db := newTestDB(t)
	got := make(chan int, 4)
	unsub := db.OnJobsChange(func(jobs []*Job) { got <- len(jobs) })
	defer unsub()

	if _, err := db.AddJob(&Job{ID: "a", VideoID: "a", URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	older := db.snapshotJobsChange() // one job
	db.mu.Unlock()
	if _, err := db.db.Exec(`DELETE FROM jobs WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	newer := db.snapshotJobsChange() // empty
	db.mu.Unlock()

	db.dispatchJobsChange(newer)
	select {
	case n := <-got:
		if n != 0 {
			t.Fatalf("first delivery had %d jobs, want the newer, empty list", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the newer snapshot was never delivered")
	}
	db.dispatchJobsChange(older)
	select {
	case n := <-got:
		t.Fatalf("the older %d-job snapshot was delivered after the newer one", n)
	case <-time.After(200 * time.Millisecond):
	}
}

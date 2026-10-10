package database

import (
	"slices"
	"testing"
)

// The channel-removal tests (W25-09). Removing a channel keeps its jobs
// unless the operator chooses to delete the pending ones, while the
// departure prune still deletes the channel's feed_items — so a kept Queued
// row has to be admissible with no partner, and the delete has to spare the
// rows the caller names.

// seedChannelJob adds a job of channel ch with its history row, as the host
// creates every channel job (monitor_callbacks: AddJob, then AddToHistory).
func seedChannelJob(t *testing.T, db *Database, id string, status JobStatus, ch *string, createdAt string) {
	t.Helper()
	added, err := db.AddJob(&Job{ID: id, VideoID: id, URL: "u", Title: "title " + id, Status: status,
		ChannelID: ch, QueuePriority: 1, CreatedAt: createdAt, UpdatedAt: createdAt})
	if err != nil || !added {
		t.Fatalf("AddJob(%s): added=%v err=%v", id, added, err)
	}
	if err := db.AddToHistory(id); err != nil {
		t.Fatalf("AddToHistory(%s): %v", id, err)
	}
}

// TestNextQueuedJobsAdmitsKeptRowsWithNoFeedPartner: a removed channel's kept
// backlog has lost its feed_items partners to the departure prune, and the
// scheduler must still admit it — after every partnered row (a NULL published
// sorts last), newest created first — and, once a re-added channel's rescan
// writes the partners back, return each row exactly once.
//
// Mutants killed: the INNER JOIN back (the partnerless rows are never
// returned); the created_at tie-break dropped (the partnerless rows come back
// in insertion order, oldest first).
func TestNextQueuedJobsAdmitsKeptRowsWithNoFeedPartner(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	ch := "UC_removed"
	seedChannelJob(t, db, "kept_old", StatusQueued, &ch, "2026-07-01T00:00:00Z")
	seedChannelJob(t, db, "kept_new", StatusQueued, &ch, "2026-07-02T00:00:00Z")
	seedChannelJob(t, db, "partnered", StatusQueued, &ch, "2026-06-01T00:00:00Z")
	if _, err := db.UpsertFeedItem(fi(ch, "partnered", "2026-05-01T00:00:00Z", "exact", "videos", "vod", 1)); err != nil {
		t.Fatal(err)
	}

	got, err := db.NextQueuedJobs(ch, 10)
	if err != nil {
		t.Fatalf("NextQueuedJobs: %v", err)
	}
	if want := []string{"partnered", "kept_new", "kept_old"}; !slices.Equal(got, want) {
		t.Errorf("NextQueuedJobs = %v, want %v", got, want)
	}

	// The channel is added back and its rescan restores the partners.
	for i, row := range []struct{ id, published string }{
		{"kept_old", "2026-07-03T00:00:00Z"},
		{"kept_new", "2026-07-04T00:00:00Z"},
	} {
		if _, err := db.UpsertFeedItem(fi(ch, row.id, row.published, "exact", "videos", "vod", 2+i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err = db.NextQueuedJobs(ch, 10)
	if err != nil {
		t.Fatalf("NextQueuedJobs after the re-add: %v", err)
	}
	if want := []string{"kept_new", "kept_old", "partnered"}; !slices.Equal(got, want) {
		t.Errorf("NextQueuedJobs after the re-add = %v, want %v (each row once, by published)", got, want)
	}
}

// TestDeleteJobsAndHistoryForChannelSparesKeep: the rows named in keep — a
// removal's parked recordings with footage — survive with their history,
// while the rest of the channel's matching rows go with theirs.
//
// Mutant killed: the `id NOT IN (keep)` clause dropped (the footage row is
// deleted).
func TestDeleteJobsAndHistoryForChannelSparesKeep(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	ch := "UC_spare"
	seedChannelJob(t, db, "parked_footage", StatusCookies, &ch, "")
	seedChannelJob(t, db, "parked_empty", StatusCookies, &ch, "")
	seedChannelJob(t, db, "queued", StatusQueued, &ch, "")

	n, err := db.DeleteJobsAndHistoryForChannel(ch, []JobStatus{StatusQueued, StatusUpcoming, StatusCookies}, []string{"parked_footage"})
	if err != nil {
		t.Fatalf("DeleteJobsAndHistoryForChannel: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}
	for id, want := range map[string]bool{"parked_footage": true, "parked_empty": false, "queued": false} {
		job, _ := db.GetJob(id)
		has, _ := db.HasProcessed(id)
		if (job != nil) != want || has != want {
			t.Errorf("%s: job kept = %v, history kept = %v; want both %v", id, job != nil, has, want)
		}
	}
}

// TestListChannelJobs: every job of the channel, whatever its status, oldest
// first — and never another channel's, nor a job with no channel at all.
//
// Mutant killed: the ORDER BY dropped (the rows, inserted newest first, come
// back in insertion order).
func TestListChannelJobs(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	ch, other := "UC_list", "UC_other"
	seedChannelJob(t, db, "finished", StatusFinished, &ch, "2026-07-03T00:00:00Z")
	seedChannelJob(t, db, "downloading", StatusDownloading, &ch, "2026-07-02T00:00:00Z")
	seedChannelJob(t, db, "queued", StatusQueued, &ch, "2026-07-01T00:00:00Z")
	seedChannelJob(t, db, "elsewhere", StatusQueued, &other, "2026-07-01T00:00:00Z")
	seedChannelJob(t, db, "manual", StatusQueued, nil, "2026-07-01T00:00:00Z")

	jobs, err := db.ListChannelJobs(ch)
	if err != nil {
		t.Fatalf("ListChannelJobs: %v", err)
	}
	want := []ChannelJob{
		{"queued", "title queued", StatusQueued},
		{"downloading", "title downloading", StatusDownloading},
		{"finished", "title finished", StatusFinished},
	}
	if !slices.Equal(jobs, want) {
		t.Errorf("ListChannelJobs = %+v, want %+v", jobs, want)
	}
}

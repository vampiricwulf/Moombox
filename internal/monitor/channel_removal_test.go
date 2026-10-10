package monitor

import (
	"slices"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestChannelReAdd_KeptJobsAreNeitherLostNorDuplicated walks a channel's jobs
// through a removal that keeps them and a re-add (W25-09, owner decision).
// The departure prune deletes only the channel's feed history: the kept
// Queued backlog row stays admissible with its feed_items partner gone
// (NextQueuedJobs LEFT-JOINs), and the re-added channel's first cycle, which
// lists the same videos again, jobs neither of them a second time — the
// Queued row is still active, the finished one still has its history row —
// while the rescan restores the Queued row's partner.
//
// Mutants killed: the sweep deleting the channel's pending jobs again (the
// kept Queued row is gone); NextQueuedJobs' INNER JOIN back (the kept row is
// not admissible between the removal and the re-add).
func TestChannelReAdd_KeptJobsAreNeitherLostNorDuplicated(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const ch = "UC1"
	chID := ch
	for _, row := range []struct {
		id     string
		status database.JobStatus
	}{{"keptQueued", database.StatusQueued}, {"keptDone", database.StatusFinished}} {
		if added, err := db.AddJob(&database.Job{ID: row.id, VideoID: row.id, URL: "u", Title: row.id,
			Status: row.status, ChannelID: &chID, QueuePriority: 1}); err != nil || !added {
			t.Fatalf("AddJob(%s): added=%v err=%v", row.id, added, err)
		}
		if err := db.AddToHistory(row.id); err != nil {
			t.Fatal(err)
		}
		seedRow(t, db, ch, row.id, now.Add(-24*time.Hour), "day", "rss", "vod")
	}

	// Removed: the next sweep no longer carries the channel.
	bw := newTestBackfillWorker(t, db)
	startWorker(t, bw)
	bw.Sweep(nil, false)
	if it, err := db.GetFeedItem(ch, "keptQueued"); err != nil || it != nil {
		t.Fatalf("precondition: the departure prune left feed row %+v (err %v)", it, err)
	}
	assertKept := func(when string) {
		t.Helper()
		for id, want := range map[string]database.JobStatus{"keptQueued": database.StatusQueued, "keptDone": database.StatusFinished} {
			job, err := db.GetJob(id)
			if err != nil || job == nil || job.Status != want {
				t.Fatalf("%s: job %s = %+v (err %v), want it kept %s", when, id, job, err, want)
			}
		}
		if ids, err := db.NextQueuedJobs(ch, 10); err != nil || !slices.Equal(ids, []string{"keptQueued"}) {
			t.Errorf("%s: NextQueuedJobs = %v (err %v), want [keptQueued] — the kept backlog stays admissible", when, ids, err)
		}
	}
	assertKept("after the removal")

	// Added back: the first cycle lists both videos again.
	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"keptQueued": 1, "keptDone": 1}, &order)
	published := now.Add(-24 * time.Hour).Format(time.RFC3339)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "keptQueued", Title: "keptQueued", Published: published},
			rssItem{ID: "keptDone", Title: "keptDone", Published: published})),
		withMembership(membWith()), withProbe(probe), withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, ch)

	if len(*found) != 0 {
		t.Errorf("the re-added channel's cycle jobbed %v again, want none — the kept jobs already stand for them", *found)
	}
	mustGetFeedItem(t, db, ch, "keptQueued") // the rescan restored the partner
	assertKept("after the re-add")
}

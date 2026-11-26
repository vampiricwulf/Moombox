package monitor

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// A YouTube video still upcoming whose job was Cancelled (or ended in Error)
// is handled: the feed cycle neither probes it nor offers it as found, where
// it used to spend a player probe and log "Video found" every cycle for a
// job insert that was always refused (owner ruling R4-4). A Finished job is
// still offered, so an incomplete-tail job can resume on re-detection.
//
// Mutant: the walk and archive gates back on HasActiveJob — the Cancelled
// and Error cases probe and emit.
func TestFeed_ASettledJobHoldsItsBroadcastBack(t *testing.T) {
	for _, tc := range []struct {
		status database.JobStatus
		held   bool
	}{
		{database.StatusCancelled, true},
		{database.StatusError, true},
		{database.StatusFinished, false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			db := newTestDB(t)
			now := fixedNow()
			seedRow(t, db, "UC1", "up000000001", now, "exact", "rss", "upcoming")
			if _, err := db.AddJob(&database.Job{ID: "up000000001", VideoID: "up000000001", URL: "u", Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			probes := 0
			fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()),
				withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
					probes++
					return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
				}), withNow(now))
			found := recordVideoFound(fm)
			fm.runCycleForTest(t, "UC1")

			if tc.held {
				if probes != 0 || len(*found) != 0 {
					t.Fatalf("probes = %d, found = %v; a %s job must hold the video back", probes, *found, tc.status)
				}
				return
			}
			if probes != 1 || len(*found) != 1 || (*found)[0].d != DispositionBroadcast {
				t.Fatalf("probes = %d, found = %v; a Finished job must stay eligible for re-detection", probes, *found)
			}
		})
	}
}

// The DECAPI half of the same rule: its newest video, upcoming, with a
// Cancelled job is not probed every 15 s, and with a Finished job it is.
func TestDecapi_ASettledJobHoldsItsBroadcastBack(t *testing.T) {
	for _, tc := range []struct {
		status database.JobStatus
		held   bool
	}{
		{database.StatusCancelled, true},
		{database.StatusError, true},
		{database.StatusFinished, false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			db := newTestDB(t)
			if _, err := db.AddJob(&database.Job{ID: "vidDecHld01", VideoID: "vidDecHld01", URL: "u", Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			probes := 0
			dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
				probes++
				return &VideoProbeResult{StreamStatus: "upcoming", Title: "free chat"}, nil
			})
			found := recordDecapiVideoFound(dm)
			ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
			if err := dm.processResponse(context.Background(), decapiBody("vidDecHld01", "free chat"), ch); err != nil {
				t.Fatal(err)
			}
			if tc.held {
				if probes != 0 || len(*found) != 0 {
					t.Fatalf("probes = %d, found = %v; a %s job must hold the video back", probes, *found, tc.status)
				}
				return
			}
			if probes != 1 || len(*found) != 1 {
				t.Fatalf("probes = %d, found = %v; a Finished job must stay eligible", probes, *found)
			}
		})
	}
}

// addSkipHistoryRow writes the kind of history row no code writes any more:
// one DECAPI recorded for a video it skipped or gave up on, with no job ever
// created — job_created 0, as the v28 upgrade leaves every orphaned row. It
// goes in through a second connection to the test database's file, because
// AddToHistory and DeleteJobLeavingHistory write only a job's row.
func addSkipHistoryRow(t *testing.T, db *database.Database, videoID string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db.File()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO history (video_id, added_at, job_created) VALUES (?, '2026-01-01T00:00:00Z', 0)`, videoID); err != nil {
		t.Fatalf("seed skip history row %s: %v", videoID, err)
	}
}

// A YouTube job deleted while its stream is upcoming or live is not created
// again by the feed cycle or DECAPI: the history row the Delete left holds it
// back, without a probe, until the operator removes that row from the
// Orphaned list, which reopens it on the next cycle (owner ruling R4-3). The
// skip-row half — a history row with no job ever created holds nothing back —
// is history_never_gates_live (archive_test.go).
//
// Mutant: HasJobBlockingRedetection without its history clause — the deleted
// stream is probed and found again every cycle.
func TestFeed_ADeletedJobsStreamIsNotFoundAgain(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const id = "del00000001"
	seedRow(t, db, "UC1", id, now, "exact", "rss", "upcoming")
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Status: database.StatusCancelled}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddToHistory(id); err != nil { // written at creation, as the host does
		t.Fatal(err)
	}
	if err := db.DeleteJob(id); err != nil {
		t.Fatal(err)
	}
	probes := 0
	probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
	}
	fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()), withProbe(probe), withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")
	if probes != 0 || len(*found) != 0 {
		t.Fatalf("probes = %d, found = %v; a deleted job's stream must not be found again", probes, *found)
	}

	if n, err := db.DeleteHistoryEntries([]string{id}); err != nil || n != 1 {
		t.Fatalf("DeleteHistoryEntries = (%d, %v), want (1, nil)", n, err)
	}
	fm2 := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()), withProbe(probe), withNow(now.Add(10*time.Minute)))
	found2 := recordVideoFound(fm2)
	fm2.runCycleForTest(t, "UC1")
	if len(*found2) != 1 || (*found2)[0].d != DispositionBroadcast {
		t.Fatalf("found = %v after the orphaned row was removed, want the stream found again", *found2)
	}
}

// The DECAPI half: the newest video, upcoming, whose job was deleted, is not
// probed or found until its orphaned history row is removed. This job was
// added by URL, so nothing wrote its history row at creation: the operator's
// Delete (DeleteJobLeavingHistory) writes it.
func TestDecapi_ADeletedJobsStreamIsNotFoundAgain(t *testing.T) {
	db := newTestDB(t)
	const id = "vidDecDel01"
	job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusUpcoming, ManuallyAdded: true}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteJobLeavingHistory(job); err != nil {
		t.Fatal(err)
	}
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "upcoming", Title: "free chat"}, nil
	})
	found := recordDecapiVideoFound(dm)
	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody(id, "free chat")
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatal(err)
	}
	if probes != 0 || len(*found) != 0 {
		t.Fatalf("probes = %d, found = %v; a deleted job's stream must not be found again", probes, *found)
	}
	if n, err := db.DeleteHistoryEntries([]string{id}); err != nil || n != 1 {
		t.Fatalf("DeleteHistoryEntries = (%d, %v), want (1, nil)", n, err)
	}
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatal(err)
	}
	if probes != 1 || len(*found) != 1 || (*found)[0].d != DispositionBroadcast {
		t.Fatalf("probes = %d, found = %v after the orphaned row was removed, want it found on the next cycle", probes, *found)
	}
}

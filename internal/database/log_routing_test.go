package database

import (
	"fmt"
	"strings"
	"testing"
)

// newLogRoutingDB is the two-map pair RouteLogToJobs works on, with no SQLite
// behind it: every assertion here is about the in-memory routing set, and the
// bare struct literal keeps the benchmark honest about what it measures.
func newLogRoutingDB() *Database {
	return &Database{jobLogs: map[string][]string{}, logRouted: map[string]struct{}{}}
}

// Every job the database has ever held was tracked for log routing, so each
// log line did an O(jobs) substring scan under the write lock — 47 µs per
// line at 5,000 jobs, with years-old Finished rows making up the bulk
// (CORE-12). Tracking follows only non-terminal jobs now; the BUFFER stays
// readable, because the operator reads a failed job's log right after it
// fails.
//
// Mutant: implementing UntrackJobForLogs as ClearJobLogs — the preserved
// lines assertion fails.
func TestUntrackStopsRoutingButKeepsTheBuffer(t *testing.T) {
	db := newLogRoutingDB()

	db.TrackJobForLogs("job-1")
	db.RouteLogToJobs("2026-09-17 12:00:00 INFO job-1 started")
	if got := db.GetJobLogs("job-1"); len(got) != 1 {
		t.Fatalf("tracked job holds %d lines, want 1", len(got))
	}

	db.UntrackJobForLogs("job-1")
	db.RouteLogToJobs("2026-09-17 12:00:01 INFO job-1 finished")

	got := db.GetJobLogs("job-1")
	if len(got) != 1 {
		t.Errorf("an untracked job must stop receiving lines, holds %d", len(got))
	}
	if len(got) == 1 && !strings.Contains(got[0], "started") {
		t.Errorf("the buffer must survive untracking, holds %q", got[0])
	}

	db.ClearJobLogs("job-1")
	if got := db.GetJobLogs("job-1"); got != nil {
		t.Errorf("ClearJobLogs must drop the buffer, holds %v", got)
	}
}

// SyncJobLogTracking is the policy of cmd/moombox's boot seed over every
// existing job (the OnJobsChange fan-out called it too, once). A live job
// is tracked — that is the whole point of the boot seed — and a terminal one
// is not, which is what stops RouteLogToJobs scanning years of Finished rows
// per log line (CORE-12).
//
// Mutants: dropping the track branch (the boot loop no longer tracks — the
// live job loses its log); dropping the untrack branch (the finished job
// keeps collecting); untracking with ClearJobLogs (the finished job's buffer
// disappears out from under the operator reading it).
func TestSyncJobLogTrackingFollowsLiveJobsOnly(t *testing.T) {
	db := newLogRoutingDB()

	live := &Job{ID: "live-1", Status: StatusDownloading}
	waiting := &Job{ID: "wait-1", Status: StatusUpcoming}
	jobs := []*Job{
		live,
		waiting,
		{ID: "done-1", Status: StatusFinished},
		{ID: "err-1", Status: StatusError},
		{ID: "gone-1", Status: StatusCancelled},
	}
	db.SyncJobLogTracking(jobs)

	for _, id := range []string{"live-1", "wait-1"} {
		db.RouteLogToJobs("2026-09-17 12:00:00 INFO " + id + " segment 1")
		if got := db.GetJobLogs(id); len(got) != 1 {
			t.Errorf("a non-terminal job must be routed to, %s holds %d lines", id, len(got))
		}
	}
	for _, id := range []string{"done-1", "err-1", "gone-1"} {
		db.RouteLogToJobs("2026-09-17 12:00:00 INFO " + id + " segment 1")
		if got := db.GetJobLogs(id); len(got) != 0 {
			t.Errorf("a terminal job must not be routed to, %s holds %v", id, got)
		}
	}

	// The transition an operator watches: the job finishes, routing stops,
	// and the lines it already collected are still there to read.
	live.Status = StatusFinished
	db.SyncJobLogTracking(jobs)
	db.RouteLogToJobs("2026-09-17 12:00:02 INFO live-1 muxed")
	got := db.GetJobLogs("live-1")
	if len(got) != 1 {
		t.Errorf("the finished job holds %d lines, want the 1 it collected while live", len(got))
	}
	if len(got) == 1 && !strings.Contains(got[0], "segment 1") {
		t.Errorf("the buffer must survive the terminal transition, holds %q", got[0])
	}
}

// ClearJobLogs and clearJobLogsOf (its list form, which the bulk channel
// prune drops its rows' logs through) own BOTH maps: a routed ID left behind
// after its buffer is gone is a job RouteLogToJobs keeps scanning for forever,
// which is the leak CORE-12's set would otherwise introduce.
//
// Mutant: clearJobLogsOf deleting only from jobLogs — the routed job
// resurrects a buffer on the next matching line.
func TestClearJobLogsDropsTheRoutedIDToo(t *testing.T) {
	db := newLogRoutingDB()
	db.TrackJobForLogs("cleared")
	db.TrackJobForLogs("pruned")
	db.TrackJobForLogs("kept")

	db.ClearJobLogs("cleared")
	db.clearJobLogsOf([]string{"pruned", "never-tracked"})

	db.RouteLogToJobs("2026-09-17 12:00:00 INFO cleared line")
	db.RouteLogToJobs("2026-09-17 12:00:00 INFO pruned line")
	db.RouteLogToJobs("2026-09-17 12:00:00 INFO kept line")

	if got := db.GetJobLogs("cleared"); got != nil {
		t.Errorf("a cleared job must stop being scanned for, holds %v", got)
	}
	if got := db.GetJobLogs("pruned"); got != nil {
		t.Errorf("a pruned job must stop being scanned for, holds %v", got)
	}
	if got := db.GetJobLogs("kept"); len(got) != 1 {
		t.Errorf("the surviving job holds %d lines, want 1", len(got))
	}
}

// BenchmarkRouteLogToJobs measures the per-line cost at the scale CORE-12
// reports: 5,000 rows in the database, a handful of them live. The line
// matches nothing, which is the worst case — every routed ID is scanned.
// Before the routed set, all 5,000 were tracked and every log line paid for
// the whole history under the write lock.
func BenchmarkRouteLogToJobs(b *testing.B) {
	db := newLogRoutingDB()
	jobs := make([]*Job, 0, 5000)
	for i := range 5000 {
		status := StatusFinished
		if i < 5 {
			status = StatusDownloading
		}
		jobs = append(jobs, &Job{ID: fmt.Sprintf("job-%06d", i), Status: status})
	}
	db.SyncJobLogTracking(jobs)

	line := "2026-09-17 12:00:00 INFO a line that names no job at all"
	b.ReportAllocs()
	for b.Loop() {
		db.RouteLogToJobs(line)
	}
}

// finishJobWithLine tracks id, routes one line to it and untracks it: the log
// life of a job that ran and reached a terminal status.
func finishJobWithLine(db *Database, id string) {
	db.TrackJobForLogs(id)
	db.RouteLogToJobs("2026-11-12 12:00:00 INFO " + id + " done")
	db.UntrackJobForLogs(id)
}

// Finished jobs kept their buffers until their row was deleted, so a process
// working through a backlog held 100 to 200 lines for every job it had ever
// finished. Only the newest finishedJobLogsKept finished jobs keep theirs
// (owner ruling R2-N2); an older one reads as empty, as it does after a
// restart.
//
// Mutants: never freeing (the oldest still holds its line); freeing the newest
// instead of the oldest; an off-by-one bound (job 1 is freed too, or job 0 is
// kept).
func TestOnlyTheNewestFinishedJobsKeepTheirLogs(t *testing.T) {
	db := newLogRoutingDB()
	id := func(i int) string { return fmt.Sprintf("fin-%04d", i) }

	for i := range finishedJobLogsKept + 1 {
		finishJobWithLine(db, id(i))
	}

	if got := db.GetJobLogs(id(0)); got != nil {
		t.Errorf("the oldest finished job still holds %d lines, want its buffer freed", len(got))
	}
	for _, i := range []int{1, finishedJobLogsKept} {
		if got := db.GetJobLogs(id(i)); len(got) != 1 {
			t.Errorf("%s is among the newest %d finished jobs and holds %d lines, want 1", id(i), finishedJobLogsKept, len(got))
		}
	}
}

// A finished job that is routed to again (Resume, Reinitialize, auto-retry, a
// worker bracket) is active: however many jobs finish while it runs, its
// buffer stays, old lines included. One whose buffer was already freed gets a
// working empty buffer, and when it finishes again it is the newest kept.
//
// Mutants: trackForLogsLocked not taking the ID out of logKept (the resumed
// job's buffer is freed mid-run); eviction deleting a routed job's buffer;
// track not recreating a freed buffer's entry (no lines after the resume).
func TestAJobRoutedAgainIsNeverFreed(t *testing.T) {
	db := newLogRoutingDB()
	id := func(i int) string { return fmt.Sprintf("fin-%04d", i) }

	finishJobWithLine(db, "resumed-a")
	finishJobWithLine(db, "freed-b")
	db.TrackJobForLogs("resumed-a") // resumed before anything pushed it out
	for i := range finishedJobLogsKept {
		finishJobWithLine(db, id(i))
	}
	if got := db.GetJobLogs("freed-b"); got != nil {
		t.Fatalf("freed-b holds %d lines, want it freed: the test needs a freed buffer", len(got))
	}

	db.RouteLogToJobs("2026-11-12 12:05:00 INFO resumed-a still going")
	if got := db.GetJobLogs("resumed-a"); len(got) != 2 {
		t.Errorf("the resumed job holds %d lines, want its first run's line and the new one", len(got))
	}

	db.TrackJobForLogs("freed-b")
	db.RouteLogToJobs("2026-11-12 12:06:00 INFO freed-b retried")
	db.UntrackJobForLogs("freed-b")
	finishJobWithLine(db, "one-more")
	if got := db.GetJobLogs("freed-b"); len(got) != 1 || !strings.Contains(got[0], "retried") {
		t.Errorf("a job retried after its log was freed holds %v, want the retry's one line", got)
	}
}

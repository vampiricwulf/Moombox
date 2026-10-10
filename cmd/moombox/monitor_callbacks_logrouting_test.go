package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/web"
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
// The fixture wires the POLICY function, so what this kills is the policy:
// Mutant: syncJobLogRouting treating a terminal job as routable (drop the
// IsTerminal branch, or call TrackJobForLogs unconditionally) — the imported
// job collects a line and this fails. That the two one-line call sites really
// do delegate to it is verified by reading them (monitor_callbacks.go's
// OnJobAdded subscriber and syncJobLogRoutingOnChange), not by this test.
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

// The status gate reads a column list that today's only emitter always fills
// (UpdateJobFields). An emitter that omits it must re-sync rather than
// silently disable re-routing for the rest of the session: nil is "I did not
// say what changed", not "nothing that matters changed".
//
// Driven through the production function directly, because the gate is what
// is under test and no production caller can produce a nil Changes.
//
// Mutant: slices.Contains(ev.Changes, "status") without the nil fallback —
// the untracked live job is never re-tracked and collects nothing.
func TestAChangeWithoutAColumnListStillReRoutes(t *testing.T) {
	db, s := logRoutingFixture(t)
	addJob(t, db, "vid0000003", database.StatusDownloading)

	// The job goes terminal and stops collecting, the ordinary way.
	if got := db.UpdateJobFields("vid0000003", map[string]any{"status": database.StatusError}); got == nil {
		t.Fatal("UpdateJobFields(status=Error) returned nil")
	}
	db.RouteLogToJobs("2026-09-17 12:00:00 ERROR vid0000003 failed")
	if got := db.GetJobLogs("vid0000003"); len(got) != 0 {
		t.Fatalf("a terminal job must stop collecting, holds %d lines", len(got))
	}

	// A hypothetical emitter that says a job changed without saying which
	// column: the routing decision must be re-taken.
	s.syncJobLogRoutingOnChange(&database.JobChange{
		Job:     &database.Job{ID: "vid0000003", Status: database.StatusDownloading},
		Changes: nil,
	})
	db.RouteLogToJobs("2026-09-17 12:00:01 INFO vid0000003 retrying")
	if got := db.GetJobLogs("vid0000003"); len(got) != 1 {
		t.Errorf("a change with no column list left the revived job unrouted, holds %d lines — nil "+
			"Changes means \"unspecified\", not \"nothing relevant\"", len(got))
	}
}

// TestTheLineBeforeATerminalWriteReachesTheJobsOwnLog is W24-11, through the
// production wiring: a real logger, wireLogForwarding, and the real routing
// subscribers. setJobError logs "job error" and then writes status=Error; the
// write untracks the job synchronously, inside UpdateJobFields. Routing used
// to be a step of the log forwarder — a goroutine behind a Subscribe channel
// — so it ran against a routed set the untrack had often already changed, and
// the failed job's own log, the one an operator opens next, lacked the line
// that says why (8 of 100 jobs at GOMAXPROCS=4, all of them at 1). Each job
// is checked right after its write with nothing drained: the line has to be
// in the buffer already.
//
// That race is lost only sometimes, so the second half takes the forwarder
// out of it altogether, the way a forwarder that has fallen behind does: the
// logger drops every line its full channel cannot take. Routing must not
// care.
//
// Mutants this kills:
//   - RouteLogToJobs moved back into the forwarder loop (and the
//     SetLineRouter call dropped): the second half routes nothing, every
//     time; the first half misses some jobs on most runs.
//   - the SetLineRouter call dropped alone: nothing is routed at all.
func TestTheLineBeforeATerminalWriteReachesTheJobsOwnLog(t *testing.T) {
	db, s := logRoutingFixture(t)
	log, err := logger.New(filepath.Join(t.TempDir(), "moombox.log"), "info", 1<<20, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)
	s.log = log
	s.wsHub = web.NewWebSocketHub(log)

	s.wireLogForwarding()
	stopForwarder := sync.OnceFunc(func() {
		log.UnsubscribeLines(s.logSub)
		close(s.logSubDone)
	})
	// What shutdown does with them.
	t.Cleanup(func() {
		log.SetLineRouter(nil)
		stopForwarder()
	})

	failJob := func(id string) bool {
		t.Helper()
		addJob(t, db, id, database.StatusDownloading)

		// setJobError's order: the line, then the terminal write.
		log.Error("job error", "jobID", id, "err", "boom")
		if !db.UpdateJobFieldsUnless(id, database.StatusCancelled, map[string]any{
			"status": database.StatusError, "error": "boom",
		}) {
			t.Fatalf("the Error write for %s did not apply", id)
		}
		routed := strings.Contains(strings.Join(db.GetJobLogs(id), "\n"), "job error")

		// Control: the terminal write still ends the routing.
		log.Info("after the terminal write", "jobID", id)
		if logs := db.GetJobLogs(id); len(logs) > 0 && strings.Contains(logs[len(logs)-1], "after the terminal write") {
			t.Fatalf("%s is still routed to after its Error write — a terminal ID left in the routed set is CORE-12", id)
		}
		return routed
	}

	const jobs = 50
	missing := 0
	for i := range jobs {
		if !failJob(fmt.Sprintf("vid%07d", i)) {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d failed jobs lack their own \"job error\" line — routing must happen inside the log call", missing, jobs)
	}

	stopForwarder()
	if !failJob("vidlagging") {
		t.Error("with the forwarder out of the picture the failed job's own log lacks its \"job error\" line — " +
			"per-job routing must not depend on a subscriber keeping up")
	}
}

// A terminal job's routing that a worker bracket holds open — RecoverAsides
// for a whole recovery, cleanupStagingAfterMux for the staging outcome — must
// outlast what happens to OTHER jobs meanwhile. Routing follows each job's own
// events: OnJobAdded (syncJobLogRouting), a status write through OnJobChange
// (syncJobLogRoutingOnChange) and OnJobDeleted (ClearJobLogs), none of which
// touches another job's. The bulk writers' OnJobsChange fan-out was the
// exception: it re-ran SyncJobLogTracking over the whole table, which untracks
// every terminal job, so a Mark Watched on any rows (BatchSetWatched — the
// dashboard's watched routes and the TUI's A W) or a channel's removal ended
// a running recovery's routing, and the rest of its progress reached no job's
// log. Neither bulk writer writes a status; the fan-out now leaves routing to
// the per-job events, and the bulk delete drops its own rows' logs itself.
//
// Mutants: onJobsChange calling SyncJobLogTracking again — the held job loses
// the line after the Watched toggle; DeleteJobsAndHistoryForChannel without
// its clearJobLogsOf — the removed row keeps its routing and buffer.
func TestABulkWriteLeavesAHeldJobsLogRouting(t *testing.T) {
	db, s := logRoutingFixture(t)
	s.wsHub = web.NewWebSocketHub(sweepTestLogger{})
	s.configStore = config.NewStore(config.Defaults(), "")
	delivered := make(chan struct{}, 4)
	t.Cleanup(db.OnJobsChange(func(jobs []*database.Job) {
		s.onJobsChange(jobs)
		delivered <- struct{}{}
	}))
	waitDelivered := func() {
		t.Helper()
		select {
		case <-delivered:
		case <-time.After(10 * time.Second):
			t.Fatal("the bulk write's OnJobsChange never arrived")
		}
	}
	routedTo := func(id, line string) bool {
		t.Helper()
		db.RouteLogToJobs(line)
		logs := db.GetJobLogs(id)
		return len(logs) > 0 && logs[len(logs)-1] == line
	}

	addJob(t, db, "vidrecover1", database.StatusFinished)
	addJob(t, db, "vidwatched1", database.StatusFinished)
	addJob(t, db, "vidlive0001", database.StatusDownloading)
	db.TrackJobForLogs("vidrecover1") // RecoverAsides' bracket opens

	// A stream discovered meanwhile: AddJob routes the new job and only it.
	addJob(t, db, "viddiscov01", database.StatusUpcoming)
	if !routedTo("vidrecover1", "recovering, job vidrecover1, after a new job") {
		t.Fatal("an AddJob ended the held job's routing")
	}

	// An operator marks another job watched while the recovery runs.
	if err := db.BatchSetWatched([]string{"vidwatched1"}, true); err != nil {
		t.Fatalf("BatchSetWatched: %v", err)
	}
	waitDelivered()
	if !routedTo("vidrecover1", "recovering, job vidrecover1, after a Watched toggle") {
		t.Error("a Mark Watched on another job ended the held job's routing — the recovery's remaining lines reach no job's log")
	}
	if !routedTo("vidlive0001", "segment, job vidlive0001") {
		t.Error("a live job stopped being routed to after the bulk write")
	}
	if routedTo("vidwatched1", "a line naming vidwatched1") {
		t.Error("the bulk write started routing to a Finished job nothing holds — a terminal ID left in the routed set is CORE-12")
	}

	// A bulk delete still drops what it removed — by the time it returns.
	ch := "UC_departed"
	if _, err := db.AddJob(&database.Job{
		ID: "vidgone0001", VideoID: "vidgone0001", URL: "https://example.invalid/vidgone0001",
		Platform: "youtube", Status: database.StatusQueued, ChannelID: &ch,
	}); err != nil {
		t.Fatalf("AddJob(vidgone0001): %v", err)
	}
	db.RouteLogToJobs("last line, job vidgone0001")
	if n, err := db.DeleteJobsAndHistoryForChannel(ch, []database.JobStatus{database.StatusQueued}, nil); err != nil || n != 1 {
		t.Fatalf("DeleteJobsAndHistoryForChannel = %d, %v; want 1, nil", n, err)
	}
	if logs := db.GetJobLogs("vidgone0001"); len(logs) != 0 {
		t.Errorf("a row the bulk write removed keeps its buffer: %v", logs)
	}
	if routedTo("vidgone0001", "a later line, job vidgone0001") {
		t.Error("a row the bulk write removed is still routed to")
	}
	waitDelivered()
	if !routedTo("vidrecover1", "recovering, job vidrecover1, after a channel's removal") {
		t.Error("a channel's removal ended the held job's routing")
	}
}

// A bulk write's OnJobsChange list is read under db.mu at commit and reaches
// the subscribers later, on a goroutine of its own. A job AddJob creates in
// that window is missing from the list without having been deleted, and its
// own OnJobAdded has already routed it. onJobsChange used to prune the routed
// set and the buffers down to the list, so it dropped that job's routing and
// buffer — a stream a monitor found, or a Queued row a backfill scan added,
// while an operator marked a job watched or removed a channel logged nothing
// to its own log until its next status write: hours, for an Upcoming one.
//
// The dispatch is held at a subscriber registered ahead of onJobsChange, the
// stand-in for that goroutine not having got there yet. Both bulk writers
// are driven.
//
// Mutant: onJobsChange pruning the log maps down to its list again (the
// PruneJobLogs it used to call, restored) — the new job holds no line at all.
func TestABulkWritesOlderListLeavesALaterJobsLogRouting(t *testing.T) {
	ch := "UC_bulkrace"
	for _, bulk := range []struct {
		name  string
		write func(t *testing.T, db *database.Database)
	}{
		{"BatchSetWatched", func(t *testing.T, db *database.Database) {
			addJob(t, db, "vidwatched1", database.StatusFinished)
			if err := db.BatchSetWatched([]string{"vidwatched1"}, true); err != nil {
				t.Fatalf("BatchSetWatched: %v", err)
			}
		}},
		{"DeleteJobsAndHistoryForChannel", func(t *testing.T, db *database.Database) {
			if _, err := db.AddJob(&database.Job{
				ID: "vidpruned01", VideoID: "vidpruned01", URL: "https://example.invalid/vidpruned01",
				Platform: "youtube", Status: database.StatusQueued, ChannelID: &ch,
			}); err != nil {
				t.Fatalf("AddJob(vidpruned01): %v", err)
			}
			if n, err := db.DeleteJobsAndHistoryForChannel(ch, []database.JobStatus{database.StatusQueued}, nil); err != nil || n != 1 {
				t.Fatalf("DeleteJobsAndHistoryForChannel = %d, %v; want 1, nil", n, err)
			}
		}},
	} {
		t.Run(bulk.name, func(t *testing.T) {
			db, s := logRoutingFixture(t)
			s.wsHub = web.NewWebSocketHub(sweepTestLogger{})
			s.configStore = config.NewStore(config.Defaults(), "")

			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			released := sync.OnceFunc(func() { close(release) })
			t.Cleanup(released)
			t.Cleanup(db.OnJobsChange(func([]*database.Job) {
				entered <- struct{}{}
				<-release
			}))
			delivered := make(chan struct{}, 1)
			t.Cleanup(db.OnJobsChange(func(jobs []*database.Job) {
				s.onJobsChange(jobs)
				delivered <- struct{}{}
			}))
			wait := func(ch <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(10 * time.Second):
					t.Fatal(what)
				}
			}

			bulk.write(t, db)
			wait(entered, "the bulk write's OnJobsChange never arrived")

			// A stream a monitor finds while the list is on its way.
			addJob(t, db, "vidnewlive1", database.StatusUpcoming)
			db.RouteLogToJobs("waiting for vidnewlive1 to start")
			released()
			wait(delivered, "onJobsChange never ran")
			db.RouteLogToJobs("vidnewlive1 still upcoming")

			if logs := db.GetJobLogs("vidnewlive1"); len(logs) != 2 {
				t.Errorf("the job added while the bulk write's list was on its way holds %d lines, want 2: %q — "+
					"a list read before the job existed must not end its routing", len(logs), logs)
			}
		})
	}
}

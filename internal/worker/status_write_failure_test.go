package worker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/sqliteuri"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A conditional status write reports false for a statement that failed as
// well as for a row that already holds an outcome. The go-live writes read
// every false as a Cancel, so a stream that went live while the database
// refused a write — another process holding its write lock past
// busy_timeout, an I/O error — was not recorded: the run stopped as a
// cancelled one with no Cancel set, handleCancellation left the row as it
// was, and the next heartbeat's run met the same wall. These tests make the
// write fail with a trigger and expect the run to go on.

// failJobWritesWhere makes every UPDATE of the jobs table for which cond
// holds (over OLD and NEW) fail, as a locked or failing database fails it.
// The trigger is created over a connection of its own, closed at once.
func failJobWritesWhere(t *testing.T, path, cond string) {
	t.Helper()
	raw, err := sql.Open("sqlite", sqliteuri.FileURI(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_job_writes BEFORE UPDATE ON jobs WHEN ` + cond +
		` BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
}

// writeFailFixture is an Upcoming YouTube job in a database of its own, a
// processor over it, and the database file's path.
func writeFailFixture(t *testing.T, status database.JobStatus) (*StreamProcessor, *database.Database, *database.Job, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "write-fail.db")
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	job := &database.Job{ID: "wf1", VideoID: "wf1", Platform: "youtube",
		Title: "T", ChannelName: "C", Status: status}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob(job.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetJob: %v", err)
	}
	return &StreamProcessor{db: db, cfg: &config.MoomboxConfig{}, logger: discardLogger{}}, db, stored, path
}

// rowStatus is the row's status, or "" for a row that is gone.
func rowStatus(t *testing.T, db *database.Database, id string) database.JobStatus {
	t.Helper()
	row, err := db.GetJob(id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row == nil {
		return ""
	}
	return row.Status
}

// writeUnlessOutcome reports an outcome only for a row that holds one: a
// terminal row, which it leaves as it is, or one that is gone. A write that
// applies reports none, and so does one that failed on a row with no outcome.
//
// Mutants: the re-read dropped (every refusal an outcome: the failed write
// reports one); `cur == nil` dropped (the deleted row reports none);
// IsTerminal narrowed to Cancelled (the Error row reports none and is
// written).
func TestWriteUnlessOutcome(t *testing.T) {
	live := map[string]any{"status": database.StatusLive}

	t.Run("applied", func(t *testing.T) {
		_, db, job, _ := writeFailFixture(t, database.StatusUpcoming)
		if writeUnlessOutcome(db, job.ID, live) {
			t.Error("a write that applied reported an outcome")
		}
		if got := rowStatus(t, db, job.ID); got != database.StatusLive {
			t.Errorf("row status = %q, want %q", got, database.StatusLive)
		}
	})

	t.Run("failed on a row with no outcome", func(t *testing.T) {
		_, db, job, path := writeFailFixture(t, database.StatusUpcoming)
		failJobWritesWhere(t, path, `NEW.status = 'Live'`)
		if writeUnlessOutcome(db, job.ID, live) {
			t.Error("a write that failed on an Upcoming row reported an outcome")
		}
		if got := rowStatus(t, db, job.ID); got != database.StatusUpcoming {
			t.Errorf("row status = %q, want the failed write's row still %q", got, database.StatusUpcoming)
		}
	})

	for _, st := range []database.JobStatus{database.StatusCancelled, database.StatusError, database.StatusFinished} {
		t.Run(string(st), func(t *testing.T) {
			_, db, job, _ := writeFailFixture(t, st)
			if !writeUnlessOutcome(db, job.ID, live) {
				t.Errorf("a %s row reported no outcome", st)
			}
			if got := rowStatus(t, db, job.ID); got != st {
				t.Errorf("row status = %q, want the outcome %q left as it is", got, st)
			}
		})
	}

	t.Run("deleted", func(t *testing.T) {
		_, db, job, _ := writeFailFixture(t, database.StatusUpcoming)
		if err := db.DeleteJob(job.ID); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		if !writeUnlessOutcome(db, job.ID, live) {
			t.Error("a deleted row reported no outcome")
		}
	})
}

// wantDownload fails t unless res hands the job to a download and the row is
// still at want: the go-live write failed and nobody cancelled anything.
func wantDownload(t *testing.T, db *database.Database, id string, want database.JobStatus, res *StreamProcessResult) {
	t.Helper()
	if res == nil || !res.ShouldDownload {
		t.Errorf("a stream whose go-live write failed was not recorded: %+v", res)
	}
	if got := rowStatus(t, db, id); got != want {
		t.Errorf("row status = %q, want the failed write's row still %q", got, want)
	}
}

// Each go-live write that fails on a row with no outcome lets the run go on
// to its download, as it did while the write was unconditional: the live arm
// of handleStreamStatus, completeStreamTransition for a stream that is live
// and for one that is over (its is_vod write), and the Twitch live start.
//
// Mutant: each call site back on the bare refusal
// (!UpdateJobFieldsUnlessTerminal; startTwitchVariant returning its false as
// the outcome) — that subtest's run stops as a cancelled one.
func TestAFailedGoLiveWriteIsNotACancel(t *testing.T) {
	t.Run("handleStreamStatus live", func(t *testing.T) {
		sp, db, job, path := writeFailFixture(t, database.StatusUpcoming)
		failJobWritesWhere(t, path, `NEW.status = 'Live'`)
		res, err := sp.handleStreamStatus(context.Background(), job, liveFetch())
		if err != nil {
			t.Fatalf("handleStreamStatus: %v", err)
		}
		wantDownload(t, db, job.ID, database.StatusUpcoming, res)
	})

	t.Run("completeStreamTransition live", func(t *testing.T) {
		sp, db, job, path := writeFailFixture(t, database.StatusUpcoming)
		failJobWritesWhere(t, path, `NEW.status = 'Live'`)
		wantDownload(t, db, job.ID, database.StatusUpcoming, sp.completeStreamTransition(job, liveFetch(), nil))
	})

	t.Run("completeStreamTransition ended", func(t *testing.T) {
		sp, db, job, path := writeFailFixture(t, database.StatusUpcoming)
		failJobWritesWhere(t, path, `NEW.is_vod = 1`)
		ended := &youtube.VideoInfo{Title: "T", StreamStatus: youtube.StreamPostLive,
			ScheduledStartTime: "2026-10-16T18:00:00Z", PlayabilityError: youtube.PlayabilityOK}
		res := sp.completeStreamTransition(job, ended, nil)
		wantDownload(t, db, job.ID, database.StatusUpcoming, res)
		if res != nil && !res.IsVod {
			t.Errorf("the ended stream's run returned IsVod false: %+v", res)
		}
	})

	t.Run("Twitch live start", func(t *testing.T) {
		sp, db, job, path := twitchCancelFixtureAt(t, database.StatusUpcoming)
		failJobWritesWhere(t, path, `NEW.status = 'Live'`)
		sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, ChannelLogin: "streamer",
			StreamID: "1", StartedAt: "2026-10-16T18:00:00Z"})
		sp.hlsMasterFn = func(context.Context, string) ([]twitch.TwitchHLSVariant, bool, error) {
			return sixtyBesideThirty(), false, nil
		}
		res, err := sp.processTwitchLive(context.Background(), job, "streamer")
		if err != nil {
			t.Fatalf("processTwitchLive: %v", err)
		}
		wantDownload(t, db, job.ID, database.StatusUpcoming, res)
	})
}

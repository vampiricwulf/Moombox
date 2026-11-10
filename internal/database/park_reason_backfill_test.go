package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenBackfillsTwitchOfflineParkReasons pins the boot backfill (owner
// ruling R-I1): an Error row an earlier release wrote with one of the two
// Twitch-offline error texts and no park_reason is given the matching reason
// when the database is opened, at the same schema version, and nothing else
// on any row moves.
//
// Mutants: the backfill not run on Open (the two legacy rows stay unmarked); the
// status filter dropped (the Cancelled and Muxing rows that still carry the
// text are marked); the empty-park_reason filter dropped (the row another
// reason claimed is rewritten); a LIKE in place of the exact match (the
// reworded row is marked); the two reasons crossed.
func TestOpenBackfillsTwitchOfflineParkReasons(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rows := []struct {
		id     string
		status JobStatus
		errMsg string
		reason ParkReason
		want   ParkReason
	}{
		{"flap", StatusError, LegacyTwitchOfflineError, ParkReasonNone, ParkReasonTwitchOffline},
		{"ended", StatusError, LegacyTwitchEndedOfflineError, ParkReasonNone, ParkReasonTwitchEndedOffline},
		{"other", StatusError, "twitch HLS: 500", ParkReasonNone, ParkReasonNone},
		{"reworded", StatusError, LegacyTwitchOfflineError + " (again)", ParkReasonNone, ParkReasonNone},
		{"claimed", StatusError, LegacyTwitchEndedOfflineError, ParkReasonMuxAfterCancel, ParkReasonMuxAfterCancel},
		{"unconfirmed", StatusError, "twitch HLS: 500", ParkReasonTwitchEndUnconfirmed, ParkReasonTwitchEndUnconfirmed},
		{"cancelled", StatusCancelled, LegacyTwitchEndedOfflineError, ParkReasonNone, ParkReasonNone},
		{"muxing", StatusMuxing, LegacyTwitchEndedOfflineError, ParkReasonNone, ParkReasonNone},
		{"parked", StatusCookies, LegacyTwitchOfflineError, ParkReasonNone, ParkReasonNone},
		{"already", StatusError, LegacyTwitchOfflineError, ParkReasonTwitchOffline, ParkReasonTwitchOffline},
	}
	updated := map[string]string{}
	for _, r := range rows {
		if _, err := db.AddJob(&Job{ID: r.id, VideoID: r.id, Platform: "twitch", Status: StatusUpcoming}); err != nil {
			t.Fatalf("AddJob %s: %v", r.id, err)
		}
		// Written as an earlier release's rows are on disk: raw, so the
		// row's updated_at is known not to move with the backfill.
		if _, err := db.db.Exec(`UPDATE jobs SET status = ?, error = ?, park_reason = ?, updated_at = ? WHERE id = ?`,
			r.status, r.errMsg, r.reason, "2026-01-02T03:04:05Z", r.id); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
		updated[r.id] = "2026-01-02T03:04:05Z"
	}
	db.Close()

	for pass := 1; pass <= 2; pass++ { // idempotent: every boot runs it
		db, err = Open(dbPath)
		if err != nil {
			t.Fatalf("reopen %d: %v", pass, err)
		}
		if v, err := db.readUserVersion(); err != nil || v != schemaVersion {
			t.Fatalf("pass %d: user_version = %d (%v), want %d: the backfill carries no schema bump", pass, v, err, schemaVersion)
		}
		for _, r := range rows {
			job, err := db.GetJob(r.id)
			if err != nil || job == nil {
				t.Fatalf("pass %d: GetJob %s: %v", pass, r.id, err)
			}
			if job.ParkReason != r.want {
				t.Errorf("pass %d: %s: park_reason = %q, want %q", pass, r.id, job.ParkReason, r.want)
			}
			if job.Status != r.status || job.Error != r.errMsg || job.UpdatedAt != updated[r.id] {
				t.Errorf("pass %d: %s: the backfill moved more than park_reason: status %q error %q updated_at %q",
					pass, r.id, job.Status, job.Error, job.UpdatedAt)
			}
		}
		// The end-unconfirmed sweep selects by its own reason alone.
		jobs, err := db.TwitchEndUnconfirmedJobs()
		if err != nil || len(jobs) != 1 || jobs[0].ID != "unconfirmed" {
			t.Errorf("pass %d: TwitchEndUnconfirmedJobs = %v (%v), want the one end-unconfirmed row", pass, jobs, err)
		}
		db.Close()
	}
}

// warnRecorder records what an Open warns about.
type warnRecorder struct{ warns []string }

func (*warnRecorder) Debug(string, ...any)         {}
func (*warnRecorder) Info(string, ...any)          {}
func (r *warnRecorder) Warn(msg string, _ ...any)  { r.warns = append(r.warns, msg) }
func (r *warnRecorder) Error(msg string, _ ...any) { r.warns = append(r.warns, msg) }

// TestOpenWithNothingToBackfillTakesNoWriteLock: the backfill runs on every
// Open, and one of those is the `moombox add` side process on a running
// daemon's live database. With no row to convert — every Open but the first
// after the update — it only reads: it neither waits for the daemon's writer
// nor takes the write lock from it.
//
// Another connection holds the write lock for the whole Open, as the daemon
// does mid-transaction.
//
// Mutant: the UPDATE run unconditionally (the Open waits out busy_timeout
// behind the writer and warns that the backfill failed).
func TestOpenWithNothingToBackfillTakesNoWriteLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.AddJob(&Job{ID: "tw_1", VideoID: "1", Platform: "twitch", Status: StatusUpcoming}); err != nil {
		t.Fatal(err)
	}
	// An Error row the backfill has nothing to say about, and one already
	// converted.
	if _, err := db.db.Exec(`UPDATE jobs SET status = ?, error = ?, park_reason = ? WHERE id = ?`,
		StatusError, LegacyTwitchOfflineError, ParkReasonTwitchOffline, "tw_1"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	writer, err := sql.Open("sqlite", openDSN(dbPath, true))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	conn, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	rec := &warnRecorder{}
	start := time.Now()
	db, err = Open(dbPath, rec)
	if err != nil {
		t.Fatalf("Open beside a writer: %v", err)
	}
	defer db.Close()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Open took %s beside a writer: it waited for the write lock with nothing to backfill", took.Round(time.Millisecond))
	}
	if len(rec.warns) != 0 {
		t.Errorf("Open warned %q with nothing to backfill", rec.warns)
	}
}

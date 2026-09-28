package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV20 mirrors TestMigrationV19: newTestDB runs createSchema at
// the current schemaVersion, so this pins the fresh-install side — a
// legacy-shaped row (INSERT omitting notification_msgs) must read back NULL.
//
//	notification_msgs TEXT   (nullable — no default)
func TestMigrationV20(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy20','legacy20','u','t','Finished','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	// Read narrowly, as TestMigrationV19 does: a legacy-shaped row leaves
	// stream_start_time NULL, and Job.StreamStartTime is a plain string, so
	// GetJob cannot scan such a row at all (pre-existing, and not what this
	// test is about). The scanJobRow decode path is covered by
	// TestNotificationMsgsRoundTrip's real AddJob row.
	if got := db.NotificationMsgs("legacy20"); got != nil {
		t.Fatalf("legacy row NotificationMsgs = %v, want nil", got)
	}
}

// TestMigrationV20Idempotent: re-running the guarded ALTER on an
// already-migrated DB must be a no-op, not an error — a crash mid-block
// re-runs the whole block on next startup, because user_version is last.
func TestMigrationV20Idempotent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV20(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.migrateV20(); err != nil {
		t.Fatalf("third run: %v", err)
	}
}

// TestMigrationV20UpgradesAV19Database pins the UPGRADE half, which
// TestFreshSchemaMatchesMigratedSchema cannot: it builds both of its databases
// through createSchema, so the column is already there and a deleted ALTER
// still passes. This test presents migrateV20 with a table that lacks the
// column — the shape every existing install has.
func TestMigrationV20UpgradesAV19Database(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v19.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&Job{ID: "yt_up", VideoID: "up", URL: "u", Status: StatusFinished}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE jobs DROP COLUMN notification_msgs`); err != nil {
		t.Fatalf("make a v19-shaped table: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 19`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db2, err := Open(path) // replays the version < 20 block
	if err != nil {
		t.Fatalf("v19 -> v20 upgrade failed: %v", err)
	}
	defer db2.Close()
	if v, _ := db2.readUserVersion(); v != 20 {
		t.Errorf("user_version = %d, want 20", v)
	}
	j, err := db2.GetJob("yt_up")
	if err != nil {
		t.Fatalf("existing row unreadable after upgrade: %v", err)
	}
	if j.NotificationMsgs != nil {
		t.Errorf("upgraded row = %v, want nil", j.NotificationMsgs)
	}
	if !db2.UpdateNotificationMsgs("yt_up", map[string]string{"k": "1"}) {
		t.Error("the upgraded column is not writable")
	}
}

// TestNotificationMsgsRoundTrip pins the whole column: the silent write, both
// read paths (the narrow single-column reader the notifier uses on its hot
// path, and the full scan every UI read goes through), and the no-op that a
// nil map must be.
func TestNotificationMsgsRoundTrip(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm1", VideoID: "nm1", URL: "https://youtube.com/watch?v=nm1", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if got := db.NotificationMsgs("yt_nm1"); got != nil {
		t.Fatalf("fresh job NotificationMsgs = %v, want nil", got)
	}

	want := map[string]string{"3f6a1b2c9d0e4f57": "1234567890123456789"}
	if !db.UpdateNotificationMsgs("yt_nm1", want) {
		t.Fatal("UpdateNotificationMsgs returned false for an existing job")
	}

	got := db.NotificationMsgs("yt_nm1")
	if len(got) != 1 || got["3f6a1b2c9d0e4f57"] != "1234567890123456789" {
		t.Fatalf("NotificationMsgs = %v, want %v", got, want)
	}

	full, err := db.GetJob("yt_nm1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if full.NotificationMsgs["3f6a1b2c9d0e4f57"] != "1234567890123456789" {
		t.Fatalf("GetJob NotificationMsgs = %v, want %v", full.NotificationMsgs, want)
	}

	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatalf("GetAllJobs: %v", err)
	}
	var seen bool
	for _, j := range all {
		if j.ID == "yt_nm1" {
			seen = true
			if j.NotificationMsgs["3f6a1b2c9d0e4f57"] != "1234567890123456789" {
				t.Fatalf("GetAllJobs NotificationMsgs = %v, want %v", j.NotificationMsgs, want)
			}
		}
	}
	if !seen {
		t.Fatal("GetAllJobs did not return yt_nm1")
	}
}

// TestUpdateNotificationMsgsSilent: the ids feed the notifier and no UI reads
// them, so the write must not bump updated_at (which would put a frame on the
// WebSocket and the TUI for every job that reaches an edit-mode target).
//
// All THREE job subscribers are watched, not just OnJobUpdate:
// updateSingleColumnSilent bypasses OnJobChange and OnJobsChange too, and those
// are the richer paths a future edit is likelier to wire up by accident.
func TestUpdateNotificationMsgsSilent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm2", VideoID: "nm2", URL: "u", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	before, err := db.GetJob("yt_nm2")
	if err != nil {
		t.Fatal(err)
	}

	fired := make(chan struct{}, 8)
	db.OnJobUpdate(func(*Job) { fired <- struct{}{} })
	db.OnJobChange(func(*JobChange) { fired <- struct{}{} })
	db.OnJobsChange(func([]*Job) { fired <- struct{}{} })

	db.UpdateNotificationMsgs("yt_nm2", map[string]string{"k": "1"})

	after, err := db.GetJob("yt_nm2")
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved from %q to %q — the write must be silent", before.UpdatedAt, after.UpdatedAt)
	}
	select {
	case <-fired:
		t.Error("a job subscriber fired — the write must wake none of them")
	default:
	}
}

// TestUpdateNotificationMsgsUnknownJob (Review Focus 1): the job row can be
// deleted between the POST and its response. The write must answer false and
// leave the caller free to carry on — never error, never retry.
func TestUpdateNotificationMsgsUnknownJob(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if db.UpdateNotificationMsgs("yt_gone", map[string]string{"k": "1"}) {
		t.Error("UpdateNotificationMsgs returned true for a job that does not exist")
	}
	if got := db.NotificationMsgs("yt_gone"); got != nil {
		t.Errorf("NotificationMsgs for a missing job = %v, want nil", got)
	}
}

// TestUpdateNotificationMsgsEmptyStoresNULL: an empty map is "this job has no
// lifecycle message anywhere", which is SQL NULL, not the string "{}".
func TestUpdateNotificationMsgsEmptyStoresNULL(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm3", VideoID: "nm3", URL: "u", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	db.UpdateNotificationMsgs("yt_nm3", map[string]string{"k": "1"})
	db.UpdateNotificationMsgs("yt_nm3", nil)

	var raw any
	if err := db.db.QueryRow(`SELECT notification_msgs FROM jobs WHERE id='yt_nm3'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Errorf("notification_msgs = %v, want NULL", raw)
	}
}

// TestDecodeNotificationMsgsGarbage (Review Focus 4): a half-written or
// hand-mangled value must read as nil so the job simply posts a new message.
// A scan that failed here would break EVERY read of that job — the dashboard,
// the TUI and the worker all go through scanJobRow.
func TestDecodeNotificationMsgsGarbage(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm4", VideoID: "nm4", URL: "u", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for _, bad := range []string{`{"k":`, `[]`, `"not an object"`, ``, `   `} {
		if _, err := db.db.Exec(`UPDATE jobs SET notification_msgs = ? WHERE id = 'yt_nm4'`, bad); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetJob("yt_nm4")
		if err != nil {
			t.Fatalf("GetJob with stored %q: %v", bad, err)
		}
		if got.NotificationMsgs != nil {
			t.Errorf("stored %q decoded to %v, want nil", bad, got.NotificationMsgs)
		}
		if narrow := db.NotificationMsgs("yt_nm4"); narrow != nil {
			t.Errorf("stored %q read narrowly as %v, want nil", bad, narrow)
		}
	}
}

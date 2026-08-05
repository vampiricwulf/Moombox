package database

import "testing"

// TestMigrationV17 mirrors TestMigrationV16's shape: newTestDB already runs
// createSchema at the current schemaVersion, so this pins the fresh-install
// side — a legacy-shaped row (INSERT omitting incomplete_tail) must read
// back the column's DEFAULT 0.
func TestMigrationV17(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy17','legacy17','u','t','Finished','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var it int
	if err := db.db.QueryRow(`SELECT incomplete_tail FROM jobs WHERE id='legacy17'`).Scan(&it); err != nil {
		t.Fatal(err)
	}
	if it != 0 {
		t.Fatalf("incomplete_tail default = %d, want 0", it)
	}
}

// TestMigrationV17Idempotent mirrors TestMigrationV16Idempotent: re-running
// the guarded ALTER block on an already-migrated DB must be a no-op, not an
// error (a crash mid-block re-runs the whole block on next startup).
func TestMigrationV17Idempotent(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV17(); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

// TestMigrateV17IncompleteTail pins the brief's behavioral contract end to
// end: a fresh job defaults IncompleteTail=false, UpdateJobFields can flip
// it, and GetJob round-trips the change.
func TestMigrateV17IncompleteTail(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_tail1", VideoID: "tail1", URL: "https://youtube.com/watch?v=tail1", Status: StatusFinished}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	got, err := db.GetJob("yt_tail1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.IncompleteTail {
		t.Fatal("fresh job should default IncompleteTail=false")
	}

	if updated := db.UpdateJobFields("yt_tail1", map[string]any{"incomplete_tail": true}); updated == nil {
		t.Fatal("UpdateJobFields(incomplete_tail) returned nil")
	}

	got, err = db.GetJob("yt_tail1")
	if err != nil || got == nil || !got.IncompleteTail {
		t.Fatalf("IncompleteTail not persisted: job=%+v err=%v", got, err)
	}
}

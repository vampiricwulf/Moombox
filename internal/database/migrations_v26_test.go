package database

import (
	"testing"
)

// TestMigrationV26 is owner decision CJ-3's column through a real upgrade: a
// v25 database (the fresh schema with join_offset_seconds dropped and
// user_version put back) is migrated, and its existing row reads the new
// column as NULL — no backfill guess — while a write and both read paths
// carry a value. Re-running the block is a no-op (a crash mid-block re-runs
// it, user_version being written last).
//
// Mutants: the v26 block removed (the upgraded database has no column, and
// GetJob's SELECT fails); join_offset_seconds left out of fieldToColumn (the
// write is ignored); the scan dropping it (GetAllJobs reads nil).
func TestMigrationV26(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "legacy26", VideoID: "legacy26", URL: "u", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN join_offset_seconds`); err != nil {
		t.Fatalf("make a v25 database: %v", err)
	}
	if err := db.writeUserVersion(25); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion != 26 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 26", v, schemaVersion)
	}
	var cols int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = 'join_offset_seconds'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("after the upgrade jobs has %d join_offset_seconds column(s) (%v), want 1", cols, err)
	}
	if err := db.migrateV26(); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	got, err := db.GetJob("legacy26")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.JoinOffsetSeconds != nil {
		t.Errorf("an existing row's join_offset_seconds = %v, want NULL (no backfill)", *got.JoinOffsetSeconds)
	}

	db.UpdateJobFields("legacy26", map[string]any{"join_offset_seconds": 5025.0})
	got, _ = db.GetJob("legacy26")
	if got.JoinOffsetSeconds == nil || *got.JoinOffsetSeconds != 5025 {
		t.Errorf("GetJob join_offset_seconds = %v, want 5025", got.JoinOffsetSeconds)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy26" && (j.JoinOffsetSeconds == nil || *j.JoinOffsetSeconds != 5025) {
			t.Errorf("GetAllJobs join_offset_seconds = %v, want 5025", j.JoinOffsetSeconds)
		}
	}
}

package database

import (
	"testing"
)

// TestMigrationV28 is owner ruling R2-W1's column through a real upgrade: a
// v27 database (the fresh schema with prior_output_file dropped and
// user_version put back) is migrated, its existing row reads the new column
// as "", and a write is read back by both read paths. Re-running the block is
// a no-op.
//
// Mutants: the v28 block removed (GetJob's SELECT fails on the missing
// column); prior_output_file left out of fieldToColumn (the write is
// ignored); the scan not assigning it (both reads answer "").
func TestMigrationV28(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "legacy28", VideoID: "legacy28", URL: "u", Status: StatusError}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN prior_output_file`); err != nil {
		t.Fatalf("make a v27 database: %v", err)
	}
	if err := db.writeUserVersion(27); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion < 28 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 28 or later", v, schemaVersion)
	}
	if err := db.migrateV28(); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	got, err := db.GetJob("legacy28")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.PriorOutputFile != "" {
		t.Errorf("an existing row's prior_output_file = %q, want none", got.PriorOutputFile)
	}

	const prior = `D:\out\Channel\Stream.mp4`
	db.UpdateJobFields("legacy28", map[string]any{"prior_output_file": prior})
	if got, _ = db.GetJob("legacy28"); got.PriorOutputFile != prior {
		t.Errorf("GetJob prior_output_file = %q, want %q", got.PriorOutputFile, prior)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy28" && j.PriorOutputFile != prior {
			t.Errorf("GetAllJobs prior_output_file = %q, want %q", j.PriorOutputFile, prior)
		}
	}
}

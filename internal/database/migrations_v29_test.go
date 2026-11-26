package database

import "testing"

// TestMigrationV29 is owner ruling RV-6's column through a real upgrade: a
// v28 database (the fresh schema with twitch_stream_id dropped and
// user_version put back) is migrated, and its existing row reads the new
// column as "" (NULL, no backfill), while a write reaches both read paths.
// Re-running the block is a no-op.
//
// Mutants: the v29 block removed (GetJob's SELECT fails on the missing
// column); twitch_stream_id left out of fieldToColumn (the write is
// ignored); the scan not assigning it (both reads return "").
func TestMigrationV29(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "legacy29", VideoID: "legacy29", URL: "u", Platform: "twitch", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN twitch_stream_id`); err != nil {
		t.Fatalf("make a v28 database: %v", err)
	}
	if err := db.writeUserVersion(28); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion < 29 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 29 or later", v, schemaVersion)
	}
	if err := db.migrateV29(); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	got, err := db.GetJob("legacy29")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.TwitchStreamID != "" {
		t.Errorf("an existing row's twitch_stream_id = %q, want none (no backfill)", got.TwitchStreamID)
	}

	db.UpdateJobFields("legacy29", map[string]any{"twitch_stream_id": "900"})
	if got, _ = db.GetJob("legacy29"); got.TwitchStreamID != "900" {
		t.Errorf("GetJob twitch_stream_id = %q, want 900", got.TwitchStreamID)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy29" && j.TwitchStreamID != "900" {
			t.Errorf("GetAllJobs twitch_stream_id = %q, want 900", j.TwitchStreamID)
		}
	}
}

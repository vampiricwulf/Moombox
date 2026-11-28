package database

import "testing"

// TestMigrationV31MissingAudio: a v30 database gains missing_audio, its
// existing row reads false (never checked, no backfill), and a write reaches
// both read paths.
//
// Mutants: the v31 block removed (the SELECT fails on the missing column);
// missing_audio left out of fieldToColumn (the write is ignored); the scan
// not assigning it.
func TestMigrationV31MissingAudio(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	if _, err := db.AddJob(&Job{ID: "legacy31", VideoID: "legacy31", URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN missing_audio`); err != nil {
		t.Fatalf("make a v30 database: %v", err)
	}
	if err := db.writeUserVersion(30); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion < 31 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 31 or later", v, schemaVersion)
	}
	got, err := db.GetJob("legacy31")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.MissingAudio {
		t.Error("an existing row reads missing_audio, want false (never checked)")
	}
	db.UpdateJobFields("legacy31", map[string]any{"missing_audio": true})
	if got, _ = db.GetJob("legacy31"); !got.MissingAudio {
		t.Error("GetJob missing_audio = false after the write")
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy31" && !j.MissingAudio {
			t.Error("GetAllJobs missing_audio = false after the write")
		}
	}
}

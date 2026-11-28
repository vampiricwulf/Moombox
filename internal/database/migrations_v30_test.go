package database

import "testing"

// TestMigrationV30 is owner ruling RV-19's two columns through a real
// upgrade: a v29 database (the fresh schema with both dropped and
// user_version put back) is migrated, its existing row reads
// media_start_time as "" (NULL, no backfill), and a write reaches both read
// paths. latency_mode is only checked to exist and start NULL. Re-running
// the block is a no-op.
//
// Mutants: the v30 block removed (GetJob's SELECT fails on the missing
// column); media_start_time left out of fieldToColumn (the write is
// ignored); the scan not assigning it (both reads return "").
func TestMigrationV30(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "legacy30", VideoID: "legacy30", URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for _, col := range []string{"media_start_time", "latency_mode"} {
		if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN ` + col); err != nil {
			t.Fatalf("make a v29 database: %v", err)
		}
	}
	if err := db.writeUserVersion(29); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion < 30 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 30 or later", v, schemaVersion)
	}
	if err := db.migrateV30(); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	got, err := db.GetJob("legacy30")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.MediaStartTime != "" {
		t.Errorf("an existing row's media_start_time = %q, want none (no backfill)", got.MediaStartTime)
	}
	var latency *string
	if err := db.db.QueryRow(`SELECT latency_mode FROM jobs WHERE id = 'legacy30'`).Scan(&latency); err != nil {
		t.Fatalf("latency_mode after the upgrade: %v", err)
	}
	if latency != nil {
		t.Errorf("an existing row's latency_mode = %q, want NULL", *latency)
	}

	const anchor = "2026-11-27T22:56:50.408302Z"
	db.UpdateJobFields("legacy30", map[string]any{"media_start_time": anchor})
	if got, _ = db.GetJob("legacy30"); got.MediaStartTime != anchor {
		t.Errorf("GetJob media_start_time = %q, want %s", got.MediaStartTime, anchor)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy30" && j.MediaStartTime != anchor {
			t.Errorf("GetAllJobs media_start_time = %q, want %s", j.MediaStartTime, anchor)
		}
	}
	db.UpdateJobFields("legacy30", map[string]any{"media_start_time": nil})
	if got, _ = db.GetJob("legacy30"); got.MediaStartTime != "" {
		t.Errorf("media_start_time after a nil write = %q, want none", got.MediaStartTime)
	}
}

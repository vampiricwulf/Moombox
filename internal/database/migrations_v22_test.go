package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV22Idempotent: re-running the guarded ALTER on an
// already-migrated DB must be a no-op, not an error — a crash mid-block
// re-runs the whole block on next startup, because user_version is last.
func TestMigrationV22Idempotent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV22(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.migrateV22(); err != nil {
		t.Fatalf("third run: %v", err)
	}
}

// TestMigrationV22UpgradesAV21Database presents migrateV22 with a table that
// lacks the column — the shape every existing install has. An existing row
// reads as already announced (the default 0): nothing the old code created
// is owed a "Stream Found" by this one.
//
// Mutant: the ALTER dropped from migrateV22 (every read fails on the missing
// column).
func TestMigrationV22UpgradesAV21Database(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v21.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&Job{ID: "yt_old", VideoID: "old", URL: "u", Status: StatusUpcoming}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE jobs DROP COLUMN announce_found`); err != nil {
		t.Fatalf("make a v21-shaped table: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 21`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db2, err := Open(path) // replays the version < 22 block
	if err != nil {
		t.Fatalf("v21 -> v22 upgrade failed: %v", err)
	}
	defer db2.Close()
	if v, _ := db2.readUserVersion(); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	j, err := db2.GetJob("yt_old")
	if err != nil || j == nil {
		t.Fatalf("existing row unreadable after upgrade: %v", err)
	}
	if j.AnnounceFound {
		t.Error("an existing row came out of the upgrade owed a Stream Found")
	}
}

// AddJob stores the flag, and TakeAnnounceFound hands it out exactly once.
//
// Mutants: announce_found left out of the INSERT (the take finds nothing);
// the take as an unconditional UPDATE (the second take reports true too).
func TestTakeAnnounceFoundIsOnce(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.AddJob(&Job{ID: "yt_new", VideoID: "new", URL: "u", Status: StatusUpcoming, AnnounceFound: true}); err != nil {
		t.Fatal(err)
	}
	if j, _ := db.GetJob("yt_new"); j == nil || !j.AnnounceFound {
		t.Fatalf("stored row = %+v, want AnnounceFound", j)
	}
	if !db.TakeAnnounceFound("yt_new") {
		t.Fatal("the first take found no flag")
	}
	if db.TakeAnnounceFound("yt_new") {
		t.Error("the second take found the flag again")
	}
	if db.TakeAnnounceFound("no_such_job") {
		t.Error("a take on an unknown job reported a flag")
	}
}

package database

import (
	"testing"
)

// historyJobCreated reads one history row's job_created flag.
func historyJobCreated(t *testing.T, db *Database, id string) int {
	t.Helper()
	var v int
	if err := db.db.QueryRow(`SELECT job_created FROM history WHERE video_id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("read history %s: %v", id, err)
	}
	return v
}

// TestMigrationV28 is owner ruling R4-3's upgrade: a v27 database (history
// without job_created) is migrated, and a row whose video has a job is
// marked as a job's row, while an orphaned row stays unmarked — nothing tells
// a deleted job's row from a skip row DECAPI once wrote, and an unmarked row
// never holds back a live stream. Re-running the block is a no-op.
//
// Mutants: the backfill removed (the jobbed row stays 0); the backfill
// marking every row (the orphan reads 1).
func TestMigrationV28(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "jobbed00001", VideoID: "jobbed00001", URL: "u", Status: StatusFinished}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`ALTER TABLE history DROP COLUMN job_created`); err != nil {
		t.Fatalf("make a v27 database: %v", err)
	}
	for _, id := range []string{"jobbed00001", "orphan00001"} {
		if _, err := db.db.Exec(`INSERT INTO history (video_id, added_at) VALUES (?, '2026-01-01T00:00:00Z')`, id); err != nil {
			t.Fatal(err)
		}
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
	if got := historyJobCreated(t, db, "jobbed00001"); got != 1 {
		t.Errorf("the row of a video with a job: job_created = %d, want 1", got)
	}
	if got := historyJobCreated(t, db, "orphan00001"); got != 0 {
		t.Errorf("an orphaned row: job_created = %d, want 0 (treated as a skip row)", got)
	}

	// A job created later for the unmarked orphan's video marks its row, so
	// deleting that job leaves a row that holds the stream back.
	if _, err := db.AddJob(&Job{ID: "orphan00001", VideoID: "orphan00001", URL: "u", Status: StatusUpcoming}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddToHistory("orphan00001"); err != nil {
		t.Fatal(err)
	}
	if got := historyJobCreated(t, db, "orphan00001"); got != 1 {
		t.Errorf("AddToHistory over an unmarked row: job_created = %d, want 1", got)
	}
}

// The history clause of HasJobBlockingRedetection (owner ruling R4-3): the
// row a deleted job left holds the video back until it is removed from the
// Orphaned list; an unmarked (skip) row never does; and a Finished job's own
// marked row does not, so the incomplete-tail resume still sees it.
func TestHasJobBlockingRedetectionDeletedJob(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	const id = "deleted0001"

	if _, err := db.AddJob(&Job{ID: id, VideoID: id, URL: "u", Status: StatusFinished}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddToHistory(id); err != nil {
		t.Fatal(err)
	}
	if got, err := db.HasJobBlockingRedetection(id); err != nil || got {
		t.Fatalf("Finished job with its history row: got %v, %v; want false", got, err)
	}
	if err := db.DeleteJob(id); err != nil {
		t.Fatal(err)
	}
	if got, err := db.HasJobBlockingRedetection(id); err != nil || !got {
		t.Fatalf("after Delete: got %v, %v; want true — the row a Delete leaves holds the video back", got, err)
	}
	if n, err := db.DeleteHistoryEntries([]string{id}); err != nil || n != 1 {
		t.Fatalf("DeleteHistoryEntries = (%d, %v), want (1, nil)", n, err)
	}
	if got, err := db.HasJobBlockingRedetection(id); err != nil || got {
		t.Fatalf("after removing the orphan: got %v, %v; want false", got, err)
	}

	if _, err := db.db.Exec(`INSERT INTO history (video_id, added_at, job_created) VALUES ('skiprow0001', '2026-01-01T00:00:00Z', 0)`); err != nil {
		t.Fatal(err)
	}
	if got, err := db.HasJobBlockingRedetection("skiprow0001"); err != nil || got {
		t.Fatalf("an unmarked (skip) row: got %v, %v; want false", got, err)
	}
}

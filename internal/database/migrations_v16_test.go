package database

import (
	"path/filepath"
	"testing"
)

// newTestDB creates a test database with fresh schema and returns it.
// The caller must defer db.Close().
func newTestDB(t *testing.T) *Database {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func TestMigrationV16(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Tables and indexes exist.
	for _, name := range []string{"feed_items", "channel_state"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", name, n, err)
		}
	}
	for _, name := range []string{"idx_feed_items_window", "idx_feed_items_status"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing", name)
		}
	}

	// last_videos is gone.
	var n int
	db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='last_videos'`).Scan(&n)
	if n != 0 {
		t.Fatal("last_videos still exists")
	}

	// jobs gained channel_id (NULL) and queue_priority (DEFAULT 1) — insert a
	// legacy-shaped row without either column and read the defaults back.
	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy1','legacy1','u','t','Finished','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var qp int
	var chID any
	if err := db.db.QueryRow(`SELECT queue_priority, channel_id FROM jobs WHERE id='legacy1'`).Scan(&qp, &chID); err != nil {
		t.Fatal(err)
	}
	if qp != 1 {
		t.Fatalf("queue_priority default = %d, want 1 (spec §6: DEFAULT 1 is fail-closed for the M count)", qp)
	}
	if chID != nil {
		t.Fatalf("channel_id = %v, want NULL on legacy rows", chID)
	}
}

func TestMigrationV16Idempotent(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Re-running the block must be a no-op, not an error (crash-mid-block re-runs it).
	if err := db.migrateV16(); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

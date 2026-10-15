package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV23Idempotent: re-running the guarded ALTERs and the index on
// an already-migrated DB must be a no-op, not an error — a crash mid-block
// re-runs the whole block on next startup, because user_version is last.
func TestMigrationV23Idempotent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV23(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.migrateV23(); err != nil {
		t.Fatalf("third run: %v", err)
	}
}

// TestMigrationV23UpgradesAV22Database presents migrateV23 with a feed_items
// table that lacks the re-check columns and index — the shape every existing
// install has. An existing row comes out settled: nothing pending.
//
// Mutants: either ALTER dropped from migrateV23 (every read of the row fails
// on the missing column); the index left out of it (no
// idx_feed_items_recheck after the upgrade).
func TestMigrationV23UpgradesAV22Database(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v22.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertFeedItem(fi("UC1", "old", "2026-07-10T00:00:00Z", "exact", "rss", "unknown", 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyProbeToFeedItem("UC1", "old", "vod", "", "", "", FeedRecheck{}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX idx_feed_items_recheck`,
		`ALTER TABLE feed_items DROP COLUMN recheck_at`,
		`ALTER TABLE feed_items DROP COLUMN recheck_step`,
		`PRAGMA user_version = 22`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("make a v22-shaped table (%s): %v", q, err)
		}
	}
	raw.Close()

	db2, err := Open(path) // replays the version < 23 block
	if err != nil {
		t.Fatalf("v22 -> v23 upgrade failed: %v", err)
	}
	defer db2.Close()
	if v, _ := db2.readUserVersion(); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	it, err := db2.GetFeedItem("UC1", "old")
	if err != nil || it == nil {
		t.Fatalf("existing row unreadable after upgrade: %v", err)
	}
	if it.Status != "vod" || it.RecheckAt != "" || it.RecheckStep != 0 {
		t.Errorf("existing row came out %q re-check %q step %d, want vod and nothing pending", it.Status, it.RecheckAt, it.RecheckStep)
	}
	var n int
	if err := db2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_feed_items_recheck'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("idx_feed_items_recheck after the upgrade: count %d err %v, want 1", n, err)
	}
}

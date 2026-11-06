package database

import (
	"testing"
)

// TestMigrationV27 is owner decision PR-R2's column through a real upgrade: a
// v26 database (the fresh schema with live_edge_reason dropped and
// user_version put back) is migrated, and its existing row reads the new
// column as "" (NULL, no backfill), while a write and both read paths carry
// a value and a nil write clears it. Re-running the block is a no-op.
//
// Mutants: the v27 block removed (GetJob's SELECT fails on the missing
// column); live_edge_reason left out of fieldToColumn (the write is
// ignored); the scan not assigning it (GetAllJobs reads "").
func TestMigrationV27(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "legacy27", VideoID: "legacy27", URL: "u", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN live_edge_reason`); err != nil {
		t.Fatalf("make a v26 database: %v", err)
	}
	if err := db.writeUserVersion(26); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion < 27 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 27 or later", v, schemaVersion)
	}
	if err := db.migrateV27(); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	got, err := db.GetJob("legacy27")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.LiveEdgeReason != "" {
		t.Errorf("an existing row's live_edge_reason = %q, want none (no backfill)", got.LiveEdgeReason)
	}

	const reason = "the stream had been live longer than YouTube keeps its segments (~120 h)"
	db.UpdateJobFields("legacy27", map[string]any{"live_edge_reason": reason})
	got, _ = db.GetJob("legacy27")
	if got.LiveEdgeReason != reason {
		t.Errorf("GetJob live_edge_reason = %q, want %q", got.LiveEdgeReason, reason)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy27" && j.LiveEdgeReason != reason {
			t.Errorf("GetAllJobs live_edge_reason = %q, want %q", j.LiveEdgeReason, reason)
		}
	}
	db.UpdateJobFields("legacy27", map[string]any{"live_edge_reason": nil})
	if got, _ = db.GetJob("legacy27"); got.LiveEdgeReason != "" {
		t.Errorf("after a nil write live_edge_reason = %q, want it cleared", got.LiveEdgeReason)
	}
}

package database

import (
	"path/filepath"
	"testing"
)

// TestGapFillRows is the v25 table's accessors: a save replaces the job's
// row, DueGapFills returns only the rows due by then (soonest first), and a
// delete removes it.
//
// Mutants: the upsert a plain INSERT (the second save fails); the due filter
// dropped (the later row returned early).
func TestGapFillRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SaveGapFill(GapFill{JobID: "a", Plan: "{}", NextCheckAt: "2026-10-01T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveGapFill(GapFill{JobID: "a", Plan: `{"x":1}`, NextCheckAt: "2026-10-01T11:00:00Z"}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if err := db.SaveGapFill(GapFill{JobID: "b", Plan: "{}", NextCheckAt: "2026-10-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	due, err := db.DueGapFills("2026-10-01T12:00:00Z")
	if err != nil || len(due) != 1 || due[0].JobID != "a" || due[0].Plan != `{"x":1}` {
		t.Fatalf("due = %+v, %v; want only a, as last saved", due, err)
	}
	if err := db.DeleteGapFill("a"); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetGapFill("a"); g != nil {
		t.Errorf("a = %+v after delete", g)
	}
	if g, _ := db.GetGapFill("b"); g == nil {
		t.Error("b is gone")
	}
}

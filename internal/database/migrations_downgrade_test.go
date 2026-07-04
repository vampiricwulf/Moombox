package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateRefusesNewerSchema pins the downgrade guard: a database whose
// user_version is HIGHER than this binary's schemaVersion must refuse to
// open (previously every `version < N` block was false and migrate()
// silently accepted the unknown schema for writing).
func TestMigrateRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Create a normal DB at the current schema, then bump user_version past it.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if _, err := Open(dbPath); err == nil {
		t.Fatal("expected Open to refuse a newer-schema database, got nil error")
	} else if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("expected downgrade-guard message, got: %v", err)
	}

	// Sanity: an equal-version DB still opens fine (the guard is strictly >).
	raw2, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw2.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		t.Fatal(err)
	}
	raw2.Close()
	db3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("equal-version DB must open: %v", err)
	}
	db3.Close()
}

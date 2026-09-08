package database

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenDSNSyncsInProductionAndNotUnderGoTest: the production DSN never
// weakens SQLite's synchronous level (the durability ruling of 2026-07-03
// stands: FULL sync stays), while the DSN used under `go test` turns it off,
// because every test opens its own database and fsyncs on every commit
// were most of a 205-second database package on the Windows CI runner.
func TestOpenDSNSyncsInProductionAndNotUnderGoTest(t *testing.T) {
	t.Parallel()
	prod := openDSN("/data/moombox.db", false)
	if strings.Contains(prod, "synchronous") {
		t.Errorf("production DSN must not touch synchronous: %q", prod)
	}
	for _, want := range []string{"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		if !strings.Contains(prod, want) {
			t.Errorf("production DSN lost %s: %q", want, prod)
		}
	}

	test := openDSN("/data/moombox.db", true)
	if !strings.Contains(test, "_pragma=synchronous(OFF)") {
		t.Errorf("test DSN must turn synchronous off: %q", test)
	}
	for _, want := range []string{"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		if !strings.Contains(test, want) {
			t.Errorf("test DSN lost %s: %q", want, test)
		}
	}
}

// TestOpenUnderGoTestRunsWithoutSyncing proves the switch reaches the
// engine: a database opened by this test reports synchronous = 0 (OFF).
func TestOpenUnderGoTestRunsWithoutSyncing(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var level int
	if err := db.db.QueryRow("PRAGMA synchronous").Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != 0 {
		t.Errorf("PRAGMA synchronous = %d under go test, want 0 (OFF)", level)
	}
}

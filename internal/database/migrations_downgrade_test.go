package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateRefusesNewerSchema pins the downgrade guard: a database whose
// user_version is HIGHER than this binary's schemaVersion must refuse to
// open (previously every `version < N` block was false and migrate()
// silently accepted the unknown schema for writing).
//
// It also pins C L17: the rollback artifacts are named from the running
// binary, and the false `.old` clause is gone.
func TestMigrateRefusesNewerSchema(t *testing.T) {
	t.Parallel()
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

	_, refusal := Open(dbPath)
	if refusal == nil {
		t.Fatal("expected Open to refuse a newer-schema database, got nil error")
	}
	msg := refusal.Error()
	if !strings.Contains(msg, "newer than this binary") {
		t.Fatalf("expected downgrade-guard message, got: %v", refusal)
	}

	// C L17: the artifact names are built from the RUNNING binary, because
	// the launcher builds every rollback artifact path from its own exePath.
	// The message used to hard-code the Windows spelling, which sends a Linux
	// operator hunting for a moombox.exe.failed that cannot exist there. The
	// running binary here is the test binary (database.test / database.test.exe)
	// — whatever it is called, the message must say so, which is a stronger
	// check than any fixed name could be.
	//
	// MUTANTS THIS KILLS — both of them about the NAME, which is what this
	// test owns:
	//   - hard-code "moombox.exe" back into rollbackArtifactBase -> both
	//     Contains fail here, on this host and on Linux (executed: "the
	//     refusal does not name \"database.test.exe.failed\"");
	//   - restore the "; a manual downgrade leaves <exe>.old from the update
	//     swap" clause -> the .old assertion fails. An automatic rollback
	//     renames the artifact BACK to the plain name, so there is no .old to
	//     point at in the case this message is printed in.
	//
	// A MUTANT THIS DELIBERATELY DOES NOT KILL: changing either suffix
	// constant. This test reads the suffixes through RollbackArtifactSuffixes,
	// the same source the message formats from, so the two move together and
	// every Contains still holds (executed: green with .failed -> .broken AND
	// with .update-failed -> .update-broken). Catching that drift is
	// TestDowngradeRefusalNamesTheLauncherArtifacts's whole job, in
	// cmd/moombox, which is the only package that can see the launcher's own
	// values — that is why the parity test exists.
	exe := defaultBinaryName
	if self, exeErr := os.Executable(); exeErr == nil {
		exe = filepath.Base(self)
	}
	failed, marker := RollbackArtifactSuffixes()
	for _, want := range []string{exe + failed, exe + marker} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q; it reads: %s", want, msg)
		}
	}
	if strings.Contains(msg, ".old") {
		t.Errorf("the refusal still names a .old artifact — after an automatic rollback the newer "+
			"binary is only ever %q or a fresh download: %s", exe+failed, msg)
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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRestrictFileToOwner swaps the restrictFileToOwner seam for one test.
func fakeRestrictFileToOwner(t *testing.T, fn func(path string) error) {
	t.Helper()
	real := restrictFileToOwner
	t.Cleanup(func() { restrictFileToOwner = real })
	restrictFileToOwner = fn
}

// TestSaveRestrictsTheTempFileBeforeTheRename pins where Save's per-file
// restriction runs: on the temp file beside config.toml, while config.toml
// still holds what it held. config.toml carries the password hash; restricted
// only after the rename it would sit under the folder's inherited permissions
// in between, and for good when the restriction fails.
//
// Mutants: restrict `path` after utils.ReplaceFile -> the path seen is
// config.toml and it already holds the new settings; drop the call -> nothing
// is seen.
func TestSaveRestrictsTheTempFileBeforeTheRename(t *testing.T) {
	dir := configDirFixture(t, "output/")
	target := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	var targetAtCall string
	fakeRestrictFileToOwner(t, func(path string) error {
		seen = append(seen, path)
		b, _ := os.ReadFile(target)
		targetAtCall = string(b)
		return nil
	})

	if err := Save(Defaults(), target); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("the file restriction ran on %v, want exactly one file", seen)
	}
	if seen[0] == target || filepath.Dir(seen[0]) != dir || !strings.HasSuffix(seen[0], ".tmp") {
		t.Errorf("the file restriction ran on %q, want the temp file beside %q", seen[0], target)
	}
	if targetAtCall != "old" {
		t.Errorf("config.toml held %q when the restriction ran, want its old bytes: it must run before the rename", targetAtCall)
	}
}

// TestSaveSurvivesAFailedFileRestriction: a volume that refuses the DACL still
// gets its settings saved.
//
// Mutant: return the restriction's error -> Save fails and nothing is written.
func TestSaveSurvivesAFailedFileRestriction(t *testing.T) {
	dir := configDirFixture(t, "output/")
	target := filepath.Join(dir, "config.toml")
	fakeRestrictFileToOwner(t, func(string) error { return errors.New("this volume has no ACLs") })

	if err := Save(Defaults(), target); err != nil {
		t.Fatalf("Save failed because the file restriction did: %v", err)
	}
	if _, err := Load(target); err != nil {
		t.Errorf("the saved config does not load: %v", err)
	}
}

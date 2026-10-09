//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func seedRollback(t *testing.T) string {
	t.Helper()
	exePath := filepath.Join(t.TempDir(), "moombox-test")
	if err := os.WriteFile(exePath, []byte("BROKEN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+".old", []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exePath
}

// The rollback used to move the broken binary aside and then rename the
// previous one into the freed name, so a kill between the two left no binary
// at the plain name and nothing that could start to repair it. The broken
// binary is now kept by a hard link and the previous one replaces it in one
// rename.
//
// Mutant: keepAsideByLink reporting false — the exe is moved away first.
func TestAutoRollbackNeverEmptiesThePlainName(t *testing.T) {
	exePath := seedRollback(t)
	orig := rollbackRename
	t.Cleanup(func() { rollbackRename = orig })
	rollbackRename = func(from, to string) error {
		if from == exePath {
			t.Errorf("the broken binary was moved away to %s", to)
		}
		if to == exePath {
			if _, err := os.Stat(exePath); err != nil {
				t.Errorf("the plain name was empty when the previous binary was restored: %v", err)
			}
		}
		return orig(from, to)
	}

	if !attemptAutoRollback(exePath, exePath+".old", 1) {
		t.Fatal("attemptAutoRollback must succeed")
	}
	if !bytesEqualFile(t, exePath, "PREVIOUS") {
		t.Error("the previous binary must be back at the plain name")
	}
	if !bytesEqualFile(t, exePath+failedBinarySuffix, "BROKEN") {
		t.Error("the broken binary must be kept at .failed")
	}
}

// A restore that fails leaves the broken binary at the plain name — the
// install still starts, and preserveUpdateRollback's instructions take over —
// and drops the link, so no .failed names a binary that is still current.
//
// Mutant: the link kept on a failed restore — .failed survives.
func TestAFailedRestoreLeavesTheBrokenBinaryInPlace(t *testing.T) {
	exePath := seedRollback(t)
	orig := rollbackRename
	t.Cleanup(func() { rollbackRename = orig })
	rollbackRename = func(from, to string) error {
		if to == exePath {
			return errors.New("injected restore failure")
		}
		return orig(from, to)
	}

	if attemptAutoRollback(exePath, exePath+".old", 1) {
		t.Fatal("attemptAutoRollback reported success through a failed restore")
	}
	if !bytesEqualFile(t, exePath, "BROKEN") {
		t.Error("the plain name must still hold the binary it had")
	}
	if !bytesEqualFile(t, exePath+".old", "PREVIOUS") {
		t.Error("the rollback artifact must survive for the manual instructions")
	}
	if _, err := os.Stat(exePath + failedBinarySuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".failed left behind: %v", err)
	}
	if _, err := os.Stat(exePath + ".update-failed"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a marker was written for a rollback that did not happen: %v", err)
	}
}

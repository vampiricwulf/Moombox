package cookies

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

// TestWriteFileAtomicRestrictsTheTempFileBeforeTheRename pins WHERE and WHEN
// the per-file restriction runs: on the temp file, in the target's folder,
// while that temp file is still empty and the target still holds its old
// bytes. Restricting the target after the rename would leave the new cookies
// readable through the folder's inherited permissions for as long as that
// takes, and for good when it fails; restricting the temp file after the write
// does the same to the temp file, and a kill in between leaves it so.
//
// Mutants: call it after utils.ReplaceFile, or on `path` -> the path seen is
// the target and the target already holds the new bytes; call it after the
// Write -> the temp file already holds the cookies.
func TestWriteFileAtomicRestrictsTheTempFileBeforeTheRename(t *testing.T) {
	dir := cookieDirFixture(t, "output/")
	target := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	var targetAtCall string
	tempAtCall, tempErr := []byte("never read"), error(nil)
	fakeRestrictFileToOwner(t, func(path string) error {
		seen = append(seen, path)
		b, _ := os.ReadFile(target)
		targetAtCall = string(b)
		tempAtCall, tempErr = os.ReadFile(path)
		return nil
	})

	if err := writeFileAtomic(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("the file restriction ran on %v, want exactly one file", seen)
	}
	if seen[0] == target || filepath.Dir(seen[0]) != dir || !strings.HasSuffix(seen[0], ".tmp") {
		t.Errorf("the file restriction ran on %q, want the temp file beside %q", seen[0], target)
	}
	if targetAtCall != "old" {
		t.Errorf("the target held %q when the restriction ran, want the old bytes: it must run before the rename", targetAtCall)
	}
	if tempErr != nil || len(tempAtCall) != 0 {
		t.Errorf("the temp file held %q (read error %v) when the restriction ran, want it empty: it must run before any cookie byte is written", tempAtCall, tempErr)
	}
}

// TestWriteFileAtomicSurvivesAFailedFileRestriction: the restriction is
// hardening, the cookies are the session. A file system that refuses the DACL
// (a FAT or network volume) must still get its cookie file.
//
// Mutant: return the restriction's error -> the write fails and the target is
// never written.
func TestWriteFileAtomicSurvivesAFailedFileRestriction(t *testing.T) {
	dir := cookieDirFixture(t, "output/")
	target := filepath.Join(dir, "cookies.txt")
	fakeRestrictFileToOwner(t, func(string) error { return errors.New("this volume has no ACLs") })

	if err := writeFileAtomic(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic failed because the file restriction did: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Errorf("target = %q, %v; want the new bytes", got, err)
	}
}

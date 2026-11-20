package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// mkTree builds a directory holding exactly the named entries; a name ending
// in "/" becomes a subdirectory, anything else an empty file.
func mkTree(t *testing.T, entries ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e))
		if len(e) > 0 && e[len(e)-1] == '/' {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDirHoldsSharedData is the content test the startup notice reads. The
// Docker image's /data, and a default install folder, hold cookies.txt BESIDE
// output/, staging/, the database and the log. A directory that holds nothing
// but Moombox's secrets is a different thing.
//
// Mutants:
//   - drop the directory-name check -> rows 3 and 4 answer false and /data is
//     not called the data folder.
//   - match only the exact default basenames (moombox.db / moombox.log) ->
//     rows 6 and 7 answer false; paths.database_path and paths.log_file_path
//     are operator-settable.
//   - drop the unreadable-directory arm -> row 9 answers false, and a
//     directory we cannot even list is assumed to hold only the secrets.
//   - treat io.EOF from ReadDir as a failed listing -> the "empty" row answers
//     true.
//   - test the marker NAMES only inside the IsDir() branch -> the "a FILE named
//     output" row answers false. `output` is a marker whatever its type: on a
//     bind mount an unreadable or dangling `output` entry can present as a
//     non-directory.
func TestDirHoldsSharedData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		want    bool
	}{
		{"empty", nil, false},
		{"secrets only", []string{"cookies.txt", "config.toml", "browser-profile/"}, false},
		{"holds output/", []string{"cookies.txt", "output/"}, true},
		{"holds staging/", []string{"cookies.txt", "staging/"}, true},
		{"holds logs/", []string{"cookies.txt", "logs/"}, true},
		{"holds the default database", []string{"cookies.txt", "moombox.db"}, true},
		{"holds a renamed database", []string{"cookies.txt", "archive.sqlite3"}, true},
		{"holds a renamed log", []string{"cookies.txt", "archiver.log"}, true},
		{"holds a WAL sidecar only", []string{"cookies.txt", "moombox.db-wal"}, true},
		{"case-insensitive directory name", []string{"cookies.txt", "Output/"}, true},
		{"a temp sibling of cookies.txt is not shared data", []string{"cookies.txt", "cookies.txt.1234.tmp"}, false},
		{"a FILE named output", []string{"cookies.txt", "output"}, true},
		{"a FILE named Staging", []string{"cookies.txt", "Staging"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirHoldsSharedData(mkTree(t, tc.entries...)); got != tc.want {
				t.Errorf("DirHoldsSharedData = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a directory that cannot be listed counts as shared", func(t *testing.T) {
		if got := DirHoldsSharedData(filepath.Join(t.TempDir(), "does-not-exist")); !got {
			t.Error("DirHoldsSharedData = false for an unlistable directory — a folder that cannot be inspected is not called a secrets-only one")
		}
	})

	// A DANGLING symlink named `output` is the shape a bind mount produces
	// when the target is not mounted yet: ReadDir reports it, IsDir() is
	// false, and the entry is still the operator's output tree by every name
	// that matters. Creating a symlink needs Developer Mode or an elevated
	// shell on Windows, so a failure to create one is a skip, not a verdict.
	t.Run("a symlink named output", func(t *testing.T) {
		dir := mkTree(t, "cookies.txt")
		if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "output")); err != nil {
			t.Skipf("cannot create a symlink on this host: %v", err)
		}
		if !DirHoldsSharedData(dir) {
			t.Error("DirHoldsSharedData = false for a directory holding a symlink named output — " +
				"the name is the marker, not the inode type")
		}
	})

	// sharedDataScanLimit, stated by execution at both sides of the edge:
	// one entry under it is still judged on its contents, and hitting it
	// disqualifies whatever else the directory holds.
	//
	// Mutant: raise the limit, or truncate instead of disqualifying -> the
	// "at the limit" row answers false and a directory with hundreds of
	// entries is called a secrets-only directory.
	t.Run("the scan limit disqualifies rather than truncating", func(t *testing.T) {
		fill := func(t *testing.T, n int) string {
			t.Helper()
			dir := t.TempDir()
			for i := range n {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("harmless-%03d.txt", i)), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return dir
		}
		if got := DirHoldsSharedData(fill(t, sharedDataScanLimit-1)); got {
			t.Errorf("DirHoldsSharedData = true at %d harmless entries, one under the %d-entry limit — "+
				"nothing here is another Moombox surface", sharedDataScanLimit-1, sharedDataScanLimit)
		}
		if got := DirHoldsSharedData(fill(t, sharedDataScanLimit)); !got {
			t.Errorf("DirHoldsSharedData = false at the %d-entry limit — a directory with that many "+
				"entries is not one Moombox keeps only its secrets in, whatever the listing shows",
				sharedDataScanLimit)
		}
	})
}

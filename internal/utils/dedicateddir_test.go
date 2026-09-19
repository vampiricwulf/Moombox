package utils

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestDirHoldsSharedData is the content half of owner decision O-K. The Docker
// image's /data holds cookies.txt BESIDE output/, staging/, the database and
// the log, and chmodding it 0700 takes the operator's archives away from their
// own host user. A directory that holds nothing but Moombox's secrets is a
// different thing and still earns the hardening.
//
// Mutants:
//   - drop the directory-name check -> rows 3 and 4 answer false and /data is
//     chmodded 0700 again (the shipped bug).
//   - match only the exact default basenames (moombox.db / moombox.log) ->
//     rows 6 and 7 answer false; paths.database_path and paths.log_file_path
//     are operator-settable.
//   - drop the unreadable-directory arm -> row 9 answers false, and a
//     directory we cannot even list is assumed dedicated.
//   - treat io.EOF from ReadDir as a failed listing -> the "empty" row answers
//     true, and a brand-new secrets directory never earns the hardening.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirHoldsSharedData(mkTree(t, tc.entries...)); got != tc.want {
				t.Errorf("DirHoldsSharedData = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a directory that cannot be listed is not assumed dedicated", func(t *testing.T) {
		if got := DirHoldsSharedData(filepath.Join(t.TempDir(), "does-not-exist")); !got {
			t.Error("DirHoldsSharedData = false for an unlistable directory — a dir we cannot inspect must never be chmodded 0700")
		}
	})
}

// TestDirTighteningAllowedIsPOSIXOnly pins the OS half of O-K: "Windows icacls
// unchanged". The Windows tightening writes inheritable ACEs and takes nothing
// away from a sibling service that a shared POSIX 0700 does.
//
// Mutant: apply the content test on Windows too -> the first assertion fails on
// the Windows CI leg and every default install (exe dir holds output/, staging/,
// the DB and the log) silently loses its cookie-directory hardening.
func TestDirTighteningAllowedIsPOSIXOnly(t *testing.T) {
	shared := mkTree(t, "cookies.txt", "output/", "moombox.db")
	dedicated := mkTree(t, "cookies.txt")

	if !DirTighteningAllowed(dedicated) {
		t.Error("a dedicated secrets directory must still be tightened on every OS")
	}
	wantShared := runtime.GOOS == "windows"
	if got := DirTighteningAllowed(shared); got != wantShared {
		t.Errorf("DirTighteningAllowed(shared) = %v on %s, want %v — Windows icacls is unchanged by O-K; POSIX must not 0700 the data directory",
			got, runtime.GOOS, wantShared)
	}
}

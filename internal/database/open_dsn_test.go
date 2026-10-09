package database

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestOpenTakesTheDatabasePathLiterally opens a database under a directory
// whose name holds a character SQLite's URI parser reads ('#', '?', '%'), and
// one whose path starts with "//". Each must land at exactly that path, with
// the DSN's pragmas applied, and FileSchemaVersion must read the same file.
// Before the path was escaped, '#' and '?' cut it short (the database went to
// a sibling file, and after a '?' busy_timeout was lost into the query), "%41"
// was decoded so the open failed, and "//tmp/..." was refused as a URI
// authority.
//
// MUTANTS: drop any one of the three escapes from sqliteURIPathEscaper (that
// row fails); drop the "//" authority prefix (the "//" row fails to open);
// build openDSN or FileSchemaVersion from the raw path again (the rows fail
// in Open, or in the FileSchemaVersion check).
func TestOpenTakesTheDatabasePathLiterally(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, dir string
		unixOnly  bool
	}{
		{"hash", "a#b", false},
		{"question", "a?b", true}, // not a legal Windows file name
		{"percent", "a%41b", false},
		{"percent escape of a percent", "a%25b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.unixOnly && runtime.GOOS == "windows" {
				t.Skip("not a legal Windows path")
			}
			base := t.TempDir()
			p := filepath.Join(base, tc.dir, "moombox.db")
			checkOpensAt(t, p, p)
			ents, _ := os.ReadDir(base)
			if len(ents) != 1 || ents[0].Name() != tc.dir {
				var names []string
				for _, e := range ents {
					names = append(names, e.Name())
				}
				t.Errorf("the open left %v beside %q -- a stray database from a truncated path", names, tc.dir)
			}
		})
	}
	t.Run("leading double slash", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("a leading // is a UNC path on Windows")
		}
		p := filepath.Join(t.TempDir(), "moombox.db")
		checkOpensAt(t, "/"+p, p)
	})
}

// checkOpensAt opens the database through openPath and asserts that it is the
// file at want, with WAL, foreign keys and the busy timeout all applied, and
// that FileSchemaVersion reads it through openPath too.
func checkOpensAt(t *testing.T, openPath, want string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open(openPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", openPath, err)
	}
	var seq int
	var name, file string
	if err := db.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	var journal string
	var fk, busy int
	db.db.QueryRow("PRAGMA journal_mode").Scan(&journal)
	db.db.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	db.db.QueryRow("PRAGMA busy_timeout").Scan(&busy)
	db.Close()

	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatalf("no database at %q (SQLite opened %q): %v", want, file, err)
	}
	if gotInfo, err := os.Stat(file); err != nil || !os.SameFile(gotInfo, wantInfo) {
		t.Errorf("SQLite opened %q, want %q", file, want)
	}
	if journal != "wal" || fk != 1 || busy != 5000 {
		t.Errorf("pragmas journal_mode=%q foreign_keys=%d busy_timeout=%d, want wal/1/5000 -- the DSN's query was not read as one",
			journal, fk, busy)
	}
	if v, err := FileSchemaVersion(openPath); err != nil || v != schemaVersion {
		t.Errorf("FileSchemaVersion(%q) = %d, %v; want %d", openPath, v, err, schemaVersion)
	}
}

// TestSQLiteFileURIChangesOnlyWhatSQLiteWouldMisread pins the other half of
// the escaping: a path with none of '%', '?', '#' and no leading "//" is
// passed through untouched, so the relative default and Windows paths (drive
// letter, backslashes), which cannot be opened on this runner, read exactly
// as they always have.
//
// MUTANT: url.PathEscape-style escaping of the whole path (the space, the
// colon or the backslashes change).
func TestSQLiteFileURIChangesOnlyWhatSQLiteWouldMisread(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"./moombox.db":                   "file:./moombox.db",
		`C:\Users\me\Moombox\moombox.db`: `file:C:\Users\me\Moombox\moombox.db`,
		"/srv/My Moombox/moombox.db":     "file:/srv/My Moombox/moombox.db",
		"/srv/Moombox #2/moombox.db":     "file:/srv/Moombox %232/moombox.db",
		"/srv/a?b/100%/moombox.db":       "file:/srv/a%3Fb/100%25/moombox.db",
		"//srv/moombox.db":               "file:////srv/moombox.db",
	} {
		if got := sqliteFileURI(in); got != want {
			t.Errorf("sqliteFileURI(%q) = %q, want %q", in, got, want)
		}
	}
}

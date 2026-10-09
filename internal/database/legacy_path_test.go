package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// legacyOpenDSN is openDSN as every release before sqliteFileURI built it:
// the path pasted into the URI raw.
func legacyOpenDSN(dbPath string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", dbPath)
}

// legacyResolvedFile opens dbPath the way an earlier release did and returns
// the file SQLite reports it opened ("" for a temporary database), or the
// open's error.
func legacyResolvedFile(t *testing.T, dbPath string) (string, error) {
	t.Helper()
	raw, err := sql.Open("sqlite", legacyOpenDSN(dbPath))
	if err != nil {
		return "", err
	}
	defer raw.Close()
	var seq int
	var name, file string
	if err := raw.QueryRow("PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		return "", err
	}
	return file, nil
}

// TestLegacySQLitePathIsWhereSQLiteOpenedTheRawPath checks legacySQLitePath
// against SQLite itself: for each path shape, the file an earlier release's
// raw DSN opened is the file legacySQLitePath names, and a DSN SQLite refused
// is "".
//
// MUTANTS: drop the '?'/'#' cut, the %HH decode, the %00 stop, or the
// authority handling (an empty or "localhost" authority, any other refused);
// each fails its rows.
func TestLegacySQLitePathIsWhereSQLiteOpenedTheRawPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		dir      string // the directory under base the configured path sits in
		openDir  string // the directory the raw DSN needs to exist, "" for none
		unixOnly bool
	}{
		{"plain", "plain dir", "plain dir", false},
		{"hash", "a#b", "", false},
		{"question", "a?b", "", true},
		{"percent escape", "a%41b", "aAb", false},
		{"percent escape of a percent", "a%2541", "a%41", false},
		{"percent zero", "x%00y", "", false},
		{"percent without hex", "a%4g", "a%4g", false},
		{"percent at the end", "a%4", "a%4", false},
		{"slash escape", "a%2Fb", "a/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.unixOnly && runtime.GOOS == "windows" {
				t.Skip("not a legal Windows path")
			}
			base := t.TempDir()
			if tc.openDir != "" {
				if err := os.MkdirAll(filepath.Join(base, tc.openDir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			p := filepath.Join(base, tc.dir, "moombox.db")
			checkLegacyPath(t, p)
		})
	}
	t.Run("authorities", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("a leading // is a UNC path on Windows")
		}
		p := filepath.Join(t.TempDir(), "moombox.db")
		checkLegacyPath(t, "//"+p)          // "//" + "/tmp/...": an empty authority
		checkLegacyPath(t, "//localhost"+p) // the one named authority SQLite takes
		if got := legacySQLitePath("//example" + p); got != "" {
			t.Errorf("legacySQLitePath(%q) = %q, want \"\": SQLite refused that authority", "//example"+p, got)
		}
		if _, err := legacyResolvedFile(t, "//example"+p); err == nil {
			t.Errorf("SQLite opened %q -- the test's premise is gone", "//example"+p)
		}
	})
	t.Run("nothing left of the path", func(t *testing.T) {
		t.Parallel()
		for _, p := range []string{"#moombox.db", "%00moombox.db"} {
			if got := legacySQLitePath(p); got != "" {
				t.Errorf("legacySQLitePath(%q) = %q, want \"\"", p, got)
			}
			if file, err := legacyResolvedFile(t, p); err != nil || file != "" {
				t.Errorf("SQLite opened %q as %q (err %v), want a temporary database", p, file, err)
			}
		}
	})
}

// checkLegacyPath asserts that legacySQLitePath(p) is the file the raw DSN
// for p opened.
func checkLegacyPath(t *testing.T, p string) {
	t.Helper()
	opened, err := legacyResolvedFile(t, p)
	if err != nil {
		t.Fatalf("raw DSN for %q: %v", p, err)
	}
	got := legacySQLitePath(p)
	openedInfo, err := os.Stat(opened)
	if err != nil {
		t.Fatalf("SQLite opened %q for %q, which is not there: %v", opened, p, err)
	}
	if gotInfo, err := os.Stat(got); err != nil || !os.SameFile(gotInfo, openedInfo) {
		t.Errorf("legacySQLitePath(%q) = %q, but SQLite opened %q", p, got, opened)
	}
}

type legacyCapLog struct {
	mu    sync.Mutex
	warns []string
}

func (l *legacyCapLog) Debug(string, ...any) {}
func (l *legacyCapLog) Info(string, ...any)  {}
func (l *legacyCapLog) Error(string, ...any) {}
func (l *legacyCapLog) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+fmt.Sprint(args...))
}

// seedLegacyInstall gives the file an earlier release opened for configured
// the life that release lived in it: a finished job and its history row. It
// returns that file.
func seedLegacyInstall(t *testing.T, configured string) string {
	t.Helper()
	legacy, err := legacyResolvedFile(t, configured)
	if err != nil || legacy == "" {
		t.Fatalf("raw DSN for %q: %q, %v", configured, legacy, err)
	}
	old, err := Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.AddJob(&Job{ID: "kept", VideoID: "kept", URL: "u", Status: StatusFinished, Platform: "youtube"}); err != nil {
		t.Fatal(err)
	}
	if err := old.AddToHistory("kept"); err != nil {
		t.Fatal(err)
	}
	old.Close()
	return legacy
}

// TestOpenKeepsTheDatabaseAnEarlierReleaseKept is the upgrade of an install
// whose database_path holds '#', '?' or a %HH escape: every earlier release
// kept its jobs and history in the file the raw URI resolved to, so the
// literal path does not exist. Open must keep using that file — not create an
// empty database at the literal path, whose empty history would let the
// monitors and the backfill queue every archived video again — and warn,
// naming both paths; FileSchemaVersion, which `moombox add` reads first, must
// read the same file.
//
// MUTANTS: legacyDatabaseFile always returning "" (an empty database at the
// literal path); FileSchemaVersion reading dbPath instead of the file it
// resolved (no such file); the Warn dropped.
func TestOpenKeepsTheDatabaseAnEarlierReleaseKept(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, dir, legacyDir string
		unixOnly             bool
	}{
		{"hash", "Moombox #2", "", false},
		{"question", "Moombox?2", "", true},
		{"percent escape", "Moombox%41", "MoomboxA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.unixOnly && runtime.GOOS == "windows" {
				t.Skip("not a legal Windows path")
			}
			base := t.TempDir()
			configured := filepath.Join(base, tc.dir, "moombox.db")
			for _, d := range []string{tc.dir, tc.legacyDir} {
				if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			legacy := seedLegacyInstall(t, configured)

			if v, err := FileSchemaVersion(configured); err != nil || v != schemaVersion {
				t.Errorf("FileSchemaVersion(%q) = %d, %v; want %d, read from %q", configured, v, err, schemaVersion, legacy)
			}
			log := &legacyCapLog{}
			db, err := Open(configured, log)
			if err != nil {
				t.Fatalf("Open(%q): %v", configured, err)
			}
			job, _ := db.GetJob("kept")
			seen, _ := db.HasProcessed("kept")
			db.Close()
			if job == nil || !seen {
				t.Errorf("Open(%q) holds job %v, HasProcessed(kept)=%v; the install's jobs and history are in %q", configured, job, seen, legacy)
			}
			if _, err := os.Stat(configured); err == nil {
				t.Errorf("Open created an empty database at %q beside the install's own in %q", configured, legacy)
			}
			if len(log.warns) != 1 || !strings.Contains(log.warns[0], configured) || !strings.Contains(log.warns[0], legacy) {
				t.Errorf("warnings %q, want one naming %q and %q", log.warns, configured, legacy)
			}
		})
	}
}

// TestOpenCreatesTheConfiguredPathWhenNoEarlierDatabaseIsThere covers the
// files at the raw URI's path that were never this install's database: one
// that is not an SQLite database, an SQLite database with no jobs table, and
// a directory. Each leaves the literal path to be created, silently. A
// literal path that already exists is opened even when an earlier release's
// database is still there.
//
// MUTANTS: drop the "literal path exists" check (the last row opens the
// legacy file); drop the header check (the text file's read fails, so Open
// fails); drop the jobs-table check (the empty database is taken).
func TestOpenCreatesTheConfiguredPathWhenNoEarlierDatabaseIsThere(t *testing.T) {
	t.Parallel()
	setup := map[string]func(t *testing.T, legacy string){
		"a text file": func(t *testing.T, legacy string) {
			// Longer than a page: SQLite reads a shorter file as an empty
			// database, and this one as "file is not a database".
			if err := os.WriteFile(legacy, []byte(strings.Repeat("notes\n", 1000)), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"a database without jobs": func(t *testing.T, legacy string) {
			raw, err := sql.Open("sqlite", sqliteFileURI(legacy))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec("CREATE TABLE notes (n TEXT)"); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, legacy string) {
			if err := os.Mkdir(legacy, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range setup {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			configured := filepath.Join(base, "m #2", "moombox.db")
			if err := os.MkdirAll(filepath.Dir(configured), 0o755); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(base, "m ")
			prepare(t, legacy)
			log := &legacyCapLog{}
			db, err := Open(configured, log)
			if err != nil {
				t.Fatalf("Open(%q) beside %s: %v", configured, name, err)
			}
			db.Close()
			if _, err := os.Stat(configured); err != nil {
				t.Errorf("Open did not create %q: %v", configured, err)
			}
			if len(log.warns) != 0 {
				t.Errorf("warnings %q for %s, which was never this install's database", log.warns, name)
			}
		})
	}
	t.Run("the configured path exists", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		configured := filepath.Join(base, "m #2", "moombox.db")
		if err := os.MkdirAll(filepath.Dir(configured), 0o755); err != nil {
			t.Fatal(err)
		}
		seedLegacyInstall(t, configured)
		// A file at the configured path is the database from now on, even
		// an empty one, and the earlier release's file is no longer read.
		if err := os.WriteFile(configured, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		log := &legacyCapLog{}
		db, err := Open(configured, log)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if job, _ := db.GetJob("kept"); job != nil {
			t.Errorf("Open(%q) read the earlier release's file although the configured one exists", configured)
		}
		if len(log.warns) != 0 {
			t.Errorf("warnings %q although the configured path was opened", log.warns)
		}
	})
}

// TestOpenRefusesAnEarlierDatabaseItCannotRead: a file at the raw URI's path
// that starts like an SQLite database but cannot be read is neither taken nor
// skipped. Open fails, naming both paths, and creates nothing at the
// configured path, so a database it could not inspect is never left behind
// for an empty one.
//
// MUTANT: treat a read error as "no earlier database" (Open creates the
// literal path and succeeds).
func TestOpenRefusesAnEarlierDatabaseItCannotRead(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	configured := filepath.Join(base, "m #2", "moombox.db")
	if err := os.MkdirAll(filepath.Dir(configured), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(base, "m ")
	if err := os.WriteFile(legacy, []byte(sqliteHeader+strings.Repeat("\xff", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(configured)
	if err == nil {
		db.Close()
		t.Fatalf("Open(%q) succeeded beside an unreadable database at %q", configured, legacy)
	}
	if !strings.Contains(err.Error(), configured) || !strings.Contains(err.Error(), legacy) {
		t.Errorf("Open error %q does not name %q and %q", err, configured, legacy)
	}
	if _, statErr := os.Stat(configured); statErr == nil {
		t.Errorf("Open created %q although it refused", configured)
	}
	if _, err := FileSchemaVersion(configured); err == nil {
		t.Errorf("FileSchemaVersion(%q) succeeded beside an unreadable database", configured)
	}
}

// TestAddDuringTheUpdateWindowReachesTheRunningDaemonsDatabase: a pre-fix
// daemon still runs on the earlier release's file while the upgraded binary,
// already on disk, serves a `moombox add`. FileSchemaVersion and Open must
// both reach the file the daemon writes, so the job lands where the daemon
// reads it.
//
// MUTANTS: FileSchemaVersion or Open reading the literal path (no such file:
// FileSchemaVersion fails, or Open creates an empty database the daemon never
// reads).
func TestAddDuringTheUpdateWindowReachesTheRunningDaemonsDatabase(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	configured := filepath.Join(base, "m #2", "moombox.db")
	if err := os.MkdirAll(filepath.Dir(configured), 0o755); err != nil {
		t.Fatal(err)
	}
	seedLegacyInstall(t, configured)
	daemon, err := sql.Open("sqlite", legacyOpenDSN(configured))
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if err := daemon.Ping(); err != nil {
		t.Fatal(err)
	}

	if v, err := FileSchemaVersion(configured); err != nil || v != schemaVersion {
		t.Fatalf("FileSchemaVersion(%q) = %d, %v while the daemon runs; want %d", configured, v, err, schemaVersion)
	}
	add, err := Open(configured)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := add.AddJob(&Job{ID: "added", VideoID: "added", URL: "u", Status: StatusUpcoming, Platform: "youtube"}); err != nil {
		t.Fatal(err)
	}
	add.Close()

	var n int
	if err := daemon.QueryRow("SELECT count(*) FROM jobs WHERE id = 'added'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the running daemon sees %d rows for the added job (err %v), want 1", n, err)
	}
}

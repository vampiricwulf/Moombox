package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// isolateDaemonLock points this process's lock directory (HOME on Linux,
// LOCALAPPDATA on Windows) at a fresh temp directory and returns the record's
// path there, so no test sees or touches a real daemon's lock. The home
// directory moves with it (USERPROFILE on Windows), so the config search's
// ~/.config/moombox/ finds nothing either.
func isolateDaemonLock(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", home)
	return filepath.Join(daemonLockDir(), daemonRecordName)
}

// holdDaemonLock takes the single-instance lock the way a launcher does and
// releases it at cleanup. On Windows the session mutex comes with it, so a
// Moombox running in this session skips the test rather than failing it.
func holdDaemonLock(t *testing.T) {
	t.Helper()
	if err := acquireSingleInstanceLock(); err != nil {
		t.Skipf("cannot take the single-instance lock here: %v", err)
	}
	t.Cleanup(releaseSingleInstanceLock)
}

// putDaemonRecord writes rec at path as a daemon's child would.
func putDaemonRecord(t *testing.T, path string, rec daemonRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := utils.WriteFileAtomic(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// canonicalTempDir is t.TempDir() spelled as canonicalFile spells what is
// under it — Windows runners' temp directory sits under an 8.3 short name
// (RUNNER~1) that filepath.EvalSymlinks expands — so a test can compare the
// paths add resolves with strings.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// touch creates an empty file at path.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestResolveAddDatabase pins which database `moombox add` writes to. A
// relative database_path was opened as given, against the CLI's own working
// directory, so `moombox -config <the daemon's config> add` run from another
// directory missed the running daemon's database — or wrote into a stray one
// there and reported success. While a daemon runs, the database it recorded
// is the answer: a relative path in the daemon's own config resolves against
// the daemon's working directory, one in any other config against this
// directory, and a config that resolves elsewhere is refused naming both.
//
// Mutants: resolve a relative path against cwd whatever the config (the
// daemon's-config rows are refused); compare the config files' strings, not
// the files (the spelt-another-way row is refused); return the record's
// database without the comparison (the other-install and absolute-elsewhere
// rows are accepted); treat a running daemon with no record as no daemon
// (that row resolves); the no-record refusal advises a restart of the daemon
// again, which the dashboard's Restart does not give it (the starting row);
// canonicalFile returns its path unchanged (the no-daemon relative row is
// <cwd>/./moombox.db).
func TestResolveAddDatabase(t *testing.T) {
	root := canonicalTempDir(t)
	daemonDir := filepath.Join(root, "daemon")
	elsewhere := filepath.Join(root, "elsewhere")
	for _, d := range []string{daemonDir, elsewhere} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	daemonDB := filepath.Join(daemonDir, "moombox.db")
	daemonCfg := filepath.Join(daemonDir, "config.toml")
	otherCfg := filepath.Join(elsewhere, "config.toml")
	for _, f := range []string{daemonDB, daemonCfg, otherCfg, filepath.Join(elsewhere, "moombox.db")} {
		touch(t, f)
	}
	// The daemon's config through elsewhere/.. — joined by hand, since
	// filepath.Join would clean it into the same string.
	sep := string(filepath.Separator)
	unclean := elsewhere + sep + ".." + sep + "daemon" + sep + "config.toml"
	running := daemonState{running: true, record: &daemonRecord{
		PID: 4242, DatabasePath: daemonDB, ConfigPath: daemonCfg, WorkingDir: daemonDir,
	}}
	for _, tc := range []struct {
		name             string
		dbPath, cfg, cwd string
		d                daemonState
		want             string   // the database, when accepted
		refusalMentions  []string // the refusal names these, when refused
	}{
		{"no daemon, relative", "./moombox.db", otherCfg, elsewhere, daemonState{}, filepath.Join(elsewhere, "moombox.db"), nil},
		{"no daemon, absolute", daemonDB, otherCfg, elsewhere, daemonState{}, daemonDB, nil},
		{"daemon starting", "./moombox.db", daemonCfg, daemonDir, daemonState{running: true}, "", []string{"has not recorded", "stop Moombox and start it again", "keeps the supervisor"}},
		{"the daemon's config from elsewhere", "./moombox.db", daemonCfg, elsewhere, running, daemonDB, nil},
		{"the daemon's config spelt another way", "./moombox.db", unclean, elsewhere, running, daemonDB, nil},
		{"another install's config, here", "./moombox.db", otherCfg, elsewhere, running, "", []string{daemonDB, filepath.Join(elsewhere, "moombox.db"), daemonCfg, "4242"}},
		{"another install's config from the daemon's directory", "./moombox.db", otherCfg, daemonDir, running, daemonDB, nil},
		{"absolute, the daemon's", daemonDB, otherCfg, elsewhere, running, daemonDB, nil},
		{"absolute, elsewhere", filepath.Join(elsewhere, "moombox.db"), daemonCfg, daemonDir, running, "", []string{daemonDB, filepath.Join(elsewhere, "moombox.db")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAddDatabase(tc.dbPath, tc.cfg, tc.cwd, tc.d)
			if tc.refusalMentions == nil {
				if err != nil || got != tc.want {
					t.Fatalf("= %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("= %q, want a refusal", got)
			}
			for _, s := range tc.refusalMentions {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("the refusal does not name %q: %v", s, err)
				}
			}
		})
	}
}

// TestADaemonRecordIsTrustedOnlyWhileTheLockIsHeld walks the record through a
// launcher's life. A record with no lock held is a dead daemon's and is never
// read; the launcher removes one it finds when it takes the lock and before
// each spawn, so a crashed child's record cannot stand for the next child; the
// child's record is read while the lock is held; and the release removes it.
//
// Mutants: findRunningDaemon ignores daemonLockHeld (the stale record before
// the lock reads as a running daemon); acquireSingleInstanceLock keeps the
// record it finds (it reads as the new launcher's); childDaemonRecordEnv
// removes nothing (the crashed child's record survives the respawn);
// releaseSingleInstanceLock leaves the record (it is still on disk); either
// platform's daemonLockHeld answering false while held (the child's record is
// never found).
func TestADaemonRecordIsTrustedOnlyWhileTheLockIsHeld(t *testing.T) {
	recPath := isolateDaemonLock(t)
	dir := t.TempDir()
	stale := daemonRecord{PID: 1, DatabasePath: filepath.Join(dir, "dead.db"), WorkingDir: dir}

	if d := findRunningDaemon(); d.running {
		t.Fatalf("no lock file, but a daemon reads as running: %+v", d)
	}
	putDaemonRecord(t, recPath, stale)
	if d := findRunningDaemon(); d.running || d.record != nil {
		t.Fatalf("a dead daemon's record was trusted: %+v", d)
	}

	holdDaemonLock(t)
	if d := findRunningDaemon(); !d.running || d.record != nil {
		t.Fatalf("after the lock was taken: %+v, want running with no record", d)
	}

	env := childDaemonRecordEnv()
	if len(env) != 1 || env[0] != daemonRecordEnv+"="+recPath {
		t.Fatalf("childDaemonRecordEnv() = %q, want the record's path", env)
	}
	t.Chdir(dir)
	writeDaemonRecord(recPath, "moombox.db", "", &nopLogger{})
	d := findRunningDaemon()
	if !d.running || d.record == nil || d.record.PID != os.Getpid() || !samePath(d.record.DatabasePath, filepath.Join(dir, "moombox.db")) {
		t.Fatalf("the child's record: %+v", d)
	}

	putDaemonRecord(t, recPath, stale) // the child crashed; its record stays
	childDaemonRecordEnv()             // the respawn
	if _, err := os.Stat(recPath); err == nil {
		t.Fatal("the crashed child's record survived the respawn")
	}

	putDaemonRecord(t, recPath, stale)
	releaseSingleInstanceLock()
	if _, err := os.Stat(recPath); err == nil {
		t.Error("the release left the record")
	}
	if d := findRunningDaemon(); d.running {
		t.Errorf("released, but a daemon reads as running: %+v", d)
	}
	if env := childDaemonRecordEnv(); len(env) != 1 || env[0] != daemonRecordEnv+"=" {
		t.Errorf("no lock held: childDaemonRecordEnv() = %q, want the variable set to no path", env)
	}
}

// TestChildDaemonRecordPath pins where a child records its database. A
// launcher from before the record names no path — the supervisor keeps
// running the binary it started from across every in-place update — so after
// an in-app update into this release the child recorded nothing, and `add`
// refused every add, from the daemon's own directory too, until the whole
// process was stopped and started; the in-app Restart kept the same launcher.
// The child now finds the record's place beside the lock itself when a
// launcher spawned it naming nothing and the lock is held, removing the
// record a crashed predecessor left there, and `add` then finds its record.
// A launcher of this release that holds no lock names no path, and a child
// run by hand (no _MOOMBOX_LAUNCHER_VERSION) records nothing beside a lock
// another process holds.
//
// Mutants: read daemonRecordEnv with os.Getenv (an empty name takes the
// fallback); drop the _MOOMBOX_LAUNCHER_VERSION gate (the hand-run child
// removes and replaces the daemon's record); drop the daemonLockHeld check
// (the fallback answers with no lock held); drop the removeDaemonRecord in
// the fallback (the crashed predecessor's record stands while the database
// opens); initServices reads os.Getenv(daemonRecordEnv) again
// (TestTheDaemonRecordIsWrittenWhereTheDatabaseOpens).
func TestChildDaemonRecordPath(t *testing.T) {
	recPath := isolateDaemonLock(t)
	dir := t.TempDir()
	stale := daemonRecord{PID: 1, DatabasePath: filepath.Join(dir, "dead.db"), WorkingDir: dir}
	named := filepath.Join(dir, "named.json")
	// unset leaves name unset for the rest of the test, restored at cleanup.
	unset := func(name string) {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	unset(daemonRecordEnv)
	unset("_MOOMBOX_LAUNCHER_VERSION")

	// No lock held: nothing, whoever spawned this child.
	t.Setenv("_MOOMBOX_LAUNCHER_VERSION", "2.7.2")
	unset(daemonRecordEnv)
	if got := childDaemonRecordPath(); got != "" {
		t.Errorf("no lock held: %q, want none", got)
	}

	holdDaemonLock(t)
	putDaemonRecord(t, recPath, stale)

	// A child run by hand records nothing, and leaves the daemon's record.
	unset("_MOOMBOX_LAUNCHER_VERSION")
	if got := childDaemonRecordPath(); got != "" {
		t.Errorf("a child run by hand: %q, want none", got)
	}
	if _, err := os.Stat(recPath); err != nil {
		t.Errorf("a child run by hand removed the daemon's record: %v", err)
	}

	// A launcher of this release names the path, or names none.
	t.Setenv("_MOOMBOX_LAUNCHER_VERSION", version)
	t.Setenv(daemonRecordEnv, named)
	if got := childDaemonRecordPath(); got != named {
		t.Errorf("a named path: %q, want %q", got, named)
	}
	t.Setenv(daemonRecordEnv, "")
	if got := childDaemonRecordPath(); got != "" {
		t.Errorf("a launcher that named no path: %q, want none", got)
	}

	// A launcher from before the record: the place beside the lock, the
	// crashed predecessor's record gone before the database opens.
	t.Setenv("_MOOMBOX_LAUNCHER_VERSION", "2.9.5")
	unset(daemonRecordEnv)
	got := childDaemonRecordPath()
	if got != recPath {
		t.Fatalf("under a launcher from before the record: %q, want %q", got, recPath)
	}
	if _, err := os.Stat(recPath); err == nil {
		t.Error("the crashed predecessor's record stands while the database opens")
	}
	if d := findRunningDaemon(); !d.running || d.record != nil {
		t.Fatalf("while the database opens: %+v, want running with no record", d)
	}
	t.Chdir(dir)
	writeDaemonRecord(got, "moombox.db", "", &nopLogger{})
	d := findRunningDaemon()
	if !d.running || d.record == nil || d.record.PID != os.Getpid() || !samePath(d.record.DatabasePath, filepath.Join(dir, "moombox.db")) {
		t.Fatalf("add does not find the child's record: %+v", d)
	}
}

// TestWriteDaemonRecordResolvesAgainstTheWorkingDirectory: the child records
// the database file it opened and its config as the OS resolved them —
// against its own working directory, which it records too — spelled
// canonically, and writes nothing when no path is named.
//
// Mutants: record the file as given (the relative row is not absolute, and
// readDaemonRecord refuses it); canonicalFile returns its path unchanged (the
// recorded database keeps its "./").
func TestWriteDaemonRecordResolvesAgainstTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("data", 0o700); err != nil {
		t.Fatal(err)
	}
	touch(t, "config.toml")
	db, err := database.Open("./data/moombox.db")
	if err != nil {
		t.Fatal(err)
	}
	file := db.File()
	db.Close()
	recPath := filepath.Join(t.TempDir(), daemonRecordName)
	writeDaemonRecord(recPath, file, "config.toml", &nopLogger{})
	rec := readDaemonRecord(recPath)
	if rec == nil {
		t.Fatal("no record was written")
	}
	if rec.PID != os.Getpid() || rec.WorkingDir != wd {
		t.Errorf("record = %+v, want pid %d in %s", *rec, os.Getpid(), wd)
	}
	for _, f := range []struct{ name, got, want string }{
		{"database", rec.DatabasePath, filepath.Join(wd, "data", "moombox.db")},
		{"config", rec.ConfigPath, filepath.Join(wd, "config.toml")},
	} {
		if !samePath(f.got, f.want) || f.got != filepath.Clean(f.got) {
			t.Errorf("the record's %s is %q, want %q spelled canonically", f.name, f.got, f.want)
		}
	}

	before, _ := os.ReadDir(dir)
	writeDaemonRecord("", "moombox.db", "", &nopLogger{}) // no path named
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Errorf("a record was written with no path named: %v", after)
	}
}

// TestDotDotThroughASymlinkedWorkingDirectory: the daemon runs from a
// directory reached through a symlink (links/mb -> real/mb, the working
// directory a shell's cd gives, which os.Getwd returns), and its
// database_path is "../db/moombox.db". Unix resolves that ".." in the
// symlink's target, so SQLite opens real/db/moombox.db; the record and `add`
// resolved it lexically, with filepath.Join, to links/db/moombox.db. So add
// from the daemon's own directory failed with "unable to open database
// file", or, with a moombox.db of the current schema at that lexical
// spelling, wrote the job there and reported it added — and with the daemon
// stopped it was refused there with "No database at" the lexical spelling.
// The record, add with the daemon's config from its directory and from
// another, and add with no daemon now all name the file SQLite opened.
//
// Mutant: joinUnder joins with filepath.Join on Unix too (every row names
// links/db/moombox.db).
func TestDotDotThroughASymlinkedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves .. lexically, as filepath.Join does")
	}
	root := t.TempDir()
	for _, d := range []string{"real/mb", "real/db", "links/db", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	daemonDir := filepath.Join(root, "links", "mb")
	if err := os.Symlink(filepath.Join(root, "real", "mb"), daemonDir); err != nil {
		t.Skipf("cannot make a symlink here: %v", err)
	}
	elsewhere := filepath.Join(root, "elsewhere")
	daemonDB := filepath.Join(root, "real", "db", "moombox.db")
	strayDB := filepath.Join(root, "links", "db", "moombox.db")
	touch(t, filepath.Join(root, "real", "mb", "config.toml"))
	touch(t, strayDB) // a file at the lexical spelling
	const dbPath = "../db/moombox.db"

	t.Chdir(daemonDir)
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	file := db.File()
	db.Close()
	if info, err := os.Stat(strayDB); err != nil || info.Size() != 0 {
		t.Fatalf("SQLite opened the lexical spelling, not the file beside the symlink's target — the test's premise is gone (%v)", err)
	}
	recPath := filepath.Join(t.TempDir(), daemonRecordName)
	writeDaemonRecord(recPath, file, "config.toml", &nopLogger{})
	rec := readDaemonRecord(recPath)
	if rec == nil || !samePath(rec.DatabasePath, daemonDB) {
		t.Fatalf("the record is %+v, want the database SQLite opened, %s", rec, daemonDB)
	}

	running := daemonState{running: true, record: rec}
	for _, tc := range []struct {
		name, cwd, cfg string
		d              daemonState
	}{
		{"the daemon's directory", daemonDir, "config.toml", running},
		{"another directory, the daemon's -config", elsewhere, filepath.Join(daemonDir, "config.toml"), running},
		{"no daemon, the daemon's directory", daemonDir, "config.toml", daemonState{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAddDatabase(dbPath, resolveUnder(tc.cwd, tc.cfg), tc.cwd, tc.d)
			if err != nil || !samePath(got, daemonDB) {
				t.Errorf("= %q, %v; want %s, the database SQLite opened (not %s)", got, err, daemonDB, strayDB)
			}
		})
	}
}

// TestAddFindsTheDatabaseAnEarlierReleaseKept: a database_path of
// "//localhost/<abs>", which every earlier release opened as <abs> and
// database.Open still opens as <abs> while nothing is at the literal path.
// absUnder cleaned it to "/localhost/<abs>", so add with no daemon failed
// with "unable to open database file" where the previous release's add
// worked, and the daemon, which had <abs> open, recorded "/localhost/<abs>"
// and add failed on that. The record names the file Open opened, and add
// resolves database_path as Open does.
//
// Mutants: joinUnder cleans an absolute path (the no-daemon row names
// /localhost/...); databaseFileUnder skips database.ResolveFile (both rows
// name the literal path); the record names database_path instead of the
// file Open opened (TestTheDaemonRecordIsWrittenWhereTheDatabaseOpens).
func TestAddFindsTheDatabaseAnEarlierReleaseKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a leading // is a UNC path on Windows")
	}
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy", "moombox.db")
	if err := os.Mkdir(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(legacy) // the install's database, with its jobs table
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	configured := "//localhost" + legacy

	t.Chdir(dir)
	db, err = database.Open(configured)
	if err != nil {
		t.Fatal(err)
	}
	file := db.File()
	db.Close()
	if !samePath(file, legacy) {
		t.Fatalf("Open(%q) opened %q, not the earlier release's %s — the test's premise is gone", configured, file, legacy)
	}
	recPath := filepath.Join(t.TempDir(), daemonRecordName)
	writeDaemonRecord(recPath, file, "", &nopLogger{})
	rec := readDaemonRecord(recPath)
	if rec == nil || !samePath(rec.DatabasePath, legacy) {
		t.Fatalf("the record is %+v, want the database Open opened, %s", rec, legacy)
	}

	got, err := resolveAddDatabase(configured, "", dir, daemonState{})
	if err != nil || !samePath(got, legacy) {
		t.Errorf("no daemon: = %q, %v; want %s", got, err, legacy)
	}
	if v, err := database.FileSchemaVersion(got); err != nil || v != database.CurrentSchemaVersion() {
		t.Errorf("FileSchemaVersion(%q) = %d, %v", got, v, err)
	}
	got, err = resolveAddDatabase(configured, "", dir, daemonState{running: true, record: rec})
	if err != nil || got != rec.DatabasePath {
		t.Errorf("the daemon running: = %q, %v; want its record's %s", got, err, rec.DatabasePath)
	}
}

// TestSamePathNeverMatchesAMissingFileToAnExistingOne: a path that does not
// exist is not the file another path names, though utils.CanonicalPath,
// which cleans ".." lexically before it resolves anything, spells x/sub/..
// as x when sub is a symlink — and the OS reads x/sub/.. as the parent of
// sub's target.
//
// Mutant: drop the one-exists check (the missing path matches).
func TestSamePathNeverMatchesAMissingFileToAnExistingOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves .. lexically, so the two spellings name one file there")
	}
	root := t.TempDir()
	for _, d := range []string{"x/nodir", "y/z"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "y", "z"), filepath.Join(root, "x", "sub")); err != nil {
		t.Skipf("cannot make a symlink here: %v", err)
	}
	existing := filepath.Join(root, "x", "nodir", "moombox.db")
	touch(t, existing)
	sep := string(filepath.Separator)
	missing := filepath.Join(root, "x", "sub") + sep + ".." + sep + "nodir" + sep + "moombox.db" // y/nodir/moombox.db
	if _, err := os.Stat(missing); err == nil {
		t.Fatalf("%s exists — the test's premise is gone", missing)
	}
	if samePath(missing, existing) || samePath(existing, missing) {
		t.Errorf("samePath matched the missing %s to the existing %s", missing, existing)
	}
	if !samePath(existing, filepath.Join(root, "x", "sub")+sep+".."+sep+".."+sep+"x"+sep+"nodir"+sep+"moombox.db") {
		t.Error("samePath does not match two spellings of one existing file")
	}
}

// TestTheDaemonRecordIsWrittenWhereTheDatabaseOpens pins the two call sites
// only a real launcher and child reach: the launcher names the record's path
// in every child's environment, and initServices takes its record's path from
// childDaemonRecordPath before the database opens — not from the launcher's
// variable alone, which a launcher from before the record never sets — writes
// the record after the database opens, naming the file Open opened
// (db.File(), not database_path, which an earlier release may have read as
// another file), and removes it when the database closes.
//
// Mutants: drop the childDaemonRecordEnv append from the launcher loop; take
// the path from os.Getenv(daemonRecordEnv) again, or call
// childDaemonRecordPath after database.Open; drop the writeDaemonRecord call,
// or move it above database.Open; record cfg.Paths.DatabasePath again; drop
// the removeDaemonRecord from closeDB.
func TestTheDaemonRecordIsWrittenWhereTheDatabaseOpens(t *testing.T) {
	_, body := funcBody(t, "launcher.go", "launchAndSupervise")
	if !callsNamed(body, "childDaemonRecordEnv") {
		t.Error("the launcher no longer names a record to its children")
	}

	fset, body := funcBody(t, "services.go", "initServices")
	var openAt, pathAt, writeAt int
	var closeRemoves, readsEnv, recordsOpenedFile bool
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Open" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "database" && openAt == 0 {
					openAt = fset.Position(n.Pos()).Offset
				}
			}
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "writeDaemonRecord" {
				writeAt = fset.Position(n.Pos()).Offset
				if len(n.Args) > 1 {
					if c, ok := n.Args[1].(*ast.CallExpr); ok {
						if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "File" {
							recordsOpenedFile = true
						}
					}
				}
			}
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "childDaemonRecordPath" {
				pathAt = fset.Position(n.Pos()).Offset
			}
			for _, arg := range n.Args {
				if id, ok := arg.(*ast.Ident); ok && id.Name == "daemonRecordEnv" {
					readsEnv = true
				}
			}
		case *ast.AssignStmt:
			if sel, ok := n.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "closeDB" {
				closeRemoves = callsNamed(n.Rhs[0], "removeDaemonRecord")
			}
		}
		return true
	})
	if openAt == 0 || writeAt <= openAt {
		t.Errorf("initServices does not write the record after database.Open (open at %d, write at %d)", openAt, writeAt)
	}
	if pathAt == 0 || pathAt >= openAt {
		t.Errorf("initServices does not take the record's path from childDaemonRecordPath before database.Open (path at %d, open at %d)", pathAt, openAt)
	}
	if !recordsOpenedFile {
		t.Error("initServices does not record the file database.Open opened (db.File())")
	}
	if readsEnv {
		t.Error("initServices reads the launcher's record variable itself, which a launcher from before the record never sets")
	}
	if !closeRemoves {
		t.Error("closeDB does not remove the record")
	}
}

// callsNamed reports whether node calls the function named name.
func callsNamed(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestCLIAddWritesToTheRunningDaemonsDatabase runs main() in a child process
// from a directory other than the daemon's, with the daemon's config holding
// the default, relative database_path — the audit's case. This process plays
// the daemon: it holds the single-instance lock and has recorded its
// database. The cwd holds a current-schema moombox.db of its own, which is
// where the job used to land, with "Added", exit 0 and Job Added.
//
//  1. -config <the daemon's config>: the job lands in the daemon's database.
//  2. Another install's config found in the cwd: refused, naming both
//     databases.
//  3. No config found at all: refused, naming the config the daemon loaded.
//  4. The daemon gone, its record left behind: the record is not trusted, and
//     the relative path resolves against the cwd, whose database exists.
//  5. No database in the cwd either: refused, naming the absolute path tried.
//
// Mutants: findRunningDaemon trusts a record without the lock (row 4 lands in
// the daemon's database); addVideo opens cfg.Paths.DatabasePath as given
// again (row 1 lands in the cwd's); resolveAddDatabase drops the comparison
// (row 2 lands in the daemon's); the no-config refusal drops the daemon's
// config (row 3); the no-daemon relative refusal reverts to "Start the
// Moombox daemon first." (row 5's text); canonicalFile drops its fallback to
// the file's directory (row 5 names <cwd>/./moombox.db).
func TestCLIAddWritesToTheRunningDaemonsDatabase(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Split(os.Getenv("MOOMBOX_CLI_ARGS"), "\x1f")...)
		main()
		os.Exit(0)
	}
	recPath := isolateDaemonLock(t)
	root := t.TempDir()
	daemonDir := filepath.Join(root, "daemon")
	elsewhere := filepath.Join(root, "elsewhere")
	for _, d := range []string{daemonDir, elsewhere} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	daemonCfg := filepath.Join(daemonDir, "config.toml")
	touch(t, daemonCfg) // no [paths]: database_path is the default "./moombox.db"
	daemonDB := filepath.Join(daemonDir, "moombox.db")
	strayDB := filepath.Join(elsewhere, "moombox.db")
	for _, p := range []string{daemonDB, strayDB} {
		db, err := database.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	daemon := daemonRecord{PID: 4242, DatabasePath: daemonDB, ConfigPath: daemonCfg, WorkingDir: daemonDir}

	add := func(t *testing.T, wantCode int, wantOut []string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIAddWritesToTheRunningDaemonsDatabase$")
		cmd.Dir = elsewhere
		cmd.Env = append(os.Environ(),
			"MOOMBOX_CLI_HELPER=1",
			"MOOMBOX_CLI_ARGS="+strings.Join(args, "\x1f"),
			"_MOOMBOX_CHILD=1", // a wrong dispatch boots run() here, not a launcher
			"MOOMBOX_NO_TUI=1",
		)
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("the command did not exit:\n%s", out)
		}
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != wantCode {
			t.Errorf("%q: exit %d, want %d:\n%s", args, code, wantCode, out)
		}
		for _, s := range wantOut {
			if !strings.Contains(string(out), s) {
				t.Errorf("%q: the output does not name %q:\n%s", args, s, out)
			}
		}
	}
	has := func(t *testing.T, path, id string) bool {
		t.Helper()
		db, err := database.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		return db.JobExists(id)
	}

	holdDaemonLock(t)
	putDaemonRecord(t, recPath, daemon)

	add(t, 0, []string{"Added AbCdEfGhIj1 to queue."}, "-config", daemonCfg, "add", "AbCdEfGhIj1")
	if !has(t, daemonDB, "AbCdEfGhIj1") || has(t, strayDB, "AbCdEfGhIj1") {
		t.Error("row 1: the job is not in the daemon's database alone")
	}

	otherCfg := filepath.Join(elsewhere, "config.toml")
	touch(t, otherCfg)
	add(t, 1, []string{daemonDB, filepath.Join("elsewhere", "moombox.db"), "pid 4242"}, "add", "AbCdEfGhIj2")
	if has(t, daemonDB, "AbCdEfGhIj2") || has(t, strayDB, "AbCdEfGhIj2") {
		t.Error("row 2: another install's config queued the job")
	}
	if err := os.Remove(otherCfg); err != nil {
		t.Fatal(err)
	}

	add(t, 1, []string{"pass the -config the Moombox daemon runs with", "The running Moombox daemon (pid 4242) loaded " + daemonCfg}, "add", "AbCdEfGhIj5")

	releaseSingleInstanceLock()
	putDaemonRecord(t, recPath, daemon) // a dead daemon's record
	add(t, 0, []string{"Added AbCdEfGhIj3 to queue."}, "-config", daemonCfg, "add", "AbCdEfGhIj3")
	if has(t, daemonDB, "AbCdEfGhIj3") || !has(t, strayDB, "AbCdEfGhIj3") {
		t.Error("row 4: a dead daemon's record was trusted")
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(strayDB + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	add(t, 1, []string{"No database at", filepath.Join("elsewhere", "moombox.db"), "is relative"}, "-config", daemonCfg, "add", "AbCdEfGhIj4")
	if _, err := os.Stat(strayDB); err == nil {
		t.Error("row 5: a database was created in the cwd")
	}
}

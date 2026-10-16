package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"os"
	"os/exec"
	"path/filepath"
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
// (that row resolves).
func TestResolveAddDatabase(t *testing.T) {
	root := t.TempDir()
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
		{"daemon starting", "./moombox.db", daemonCfg, daemonDir, daemonState{running: true}, "", []string{"has not recorded"}},
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
	if env := childDaemonRecordEnv(); env != nil {
		t.Errorf("no lock held, but childDaemonRecordEnv() = %q", env)
	}
}

// TestWriteDaemonRecordResolvesAgainstTheWorkingDirectory: the child records
// database_path and its config as it resolved them — against its own working
// directory, which it records too — and writes nothing when no launcher named
// a record.
//
// Mutant: record database_path as given (the relative row is not absolute,
// and readDaemonRecord refuses it).
func TestWriteDaemonRecordResolvesAgainstTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	recPath := filepath.Join(t.TempDir(), daemonRecordName)
	writeDaemonRecord(recPath, "./data/moombox.db", "config.toml", &nopLogger{})
	rec := readDaemonRecord(recPath)
	if rec == nil {
		t.Fatal("no record was written")
	}
	want := daemonRecord{
		PID:          os.Getpid(),
		DatabasePath: filepath.Join(wd, "data", "moombox.db"),
		ConfigPath:   filepath.Join(wd, "config.toml"),
		WorkingDir:   wd,
	}
	if *rec != want {
		t.Errorf("record = %+v, want %+v", *rec, want)
	}

	writeDaemonRecord("", "moombox.db", "", &nopLogger{}) // no launcher named one
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a record was written with no path named: %v", entries)
	}
}

// TestTheDaemonRecordIsWrittenWhereTheDatabaseOpens pins the two call sites
// only a real launcher and child reach: the launcher names the record's path
// in every child's environment, and initServices writes the record after the
// database opens and removes it when the database closes.
//
// Mutants: drop the childDaemonRecordEnv append from the launcher loop; drop
// the writeDaemonRecord call, or move it above database.Open; drop the
// removeDaemonRecord from closeDB.
func TestTheDaemonRecordIsWrittenWhereTheDatabaseOpens(t *testing.T) {
	_, body := funcBody(t, "launcher.go", "launchAndSupervise")
	if !callsNamed(body, "childDaemonRecordEnv") {
		t.Error("the launcher no longer names a record to its children")
	}

	fset, body := funcBody(t, "services.go", "initServices")
	var openAt, writeAt int
	var closeRemoves bool
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
// Moombox daemon first." (row 5's text).
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

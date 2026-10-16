package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The daemon record: which database the running daemon has open, kept beside
// the single-instance lock so `moombox add` writes to that database and no
// other. A relative database_path resolves against the working directory of
// whoever opens it, and `add` used to open it as given — so `moombox -config
// <the daemon's config> add <id>` run from another directory refused with
// "Start the Moombox daemon first." while the daemon ran, or, when that
// directory held a moombox.db of the current schema, wrote the job into it,
// printed "Added", exited 0 and sent Job Added for a video the daemon never
// saw.
//
// The launcher holds the lock and the child opens the database, so the
// launcher names the record's path to each child it spawns
// (daemonRecordEnv), after removing whatever an earlier child left there,
// and the child writes the record once its database is open
// (writeDaemonRecord, initServices) and removes it when it closes the
// database; the launcher removes it too when it takes the lock and when it
// releases it. A launcher from before the record names nothing — the
// supervisor keeps running the binary it started from across every in-place
// update — so a child it spawns finds the record's place beside the lock
// itself (childDaemonRecordPath). A record is trusted only while the lock is
// held (findRunningDaemon): a daemon that died without removing its record
// left the lock with it, kernel-released, so its record is never read as a
// running daemon's. The lock is per user — %LOCALAPPDATA% on Windows,
// ~/.local/share on Linux — so a daemon another account runs is invisible
// here, and `add` takes the no-daemon rule for it.

// singleInstanceLockName is the lock file's name (single_instance_unix.go,
// single_instance_windows.go); daemonRecordName is the record's, beside it.
const (
	singleInstanceLockName = "moombox.lock"
	daemonRecordName       = "moombox-daemon.json"
)

// daemonRecordEnv names, in a child's environment, the file it records its
// database in: every launcher since the record sets it, empty when it holds
// no lock to put the record beside. A child run by hand (or a test's) has
// none, and writes no record another process would read
// (childDaemonRecordPath).
const daemonRecordEnv = "_MOOMBOX_DAEMON_RECORD"

// daemonRecord is the record's content. Every path in it is absolute.
type daemonRecord struct {
	// PID is the child that has the database open.
	PID int `json:"pid"`
	// DatabasePath is database_path as the daemon resolved it.
	DatabasePath string `json:"database_path"`
	// ConfigPath is the config file the daemon loaded, "" when it runs on
	// defaults (a first run).
	ConfigPath string `json:"config_path,omitempty"`
	// WorkingDir is the daemon's working directory, which its config's
	// relative paths resolve against.
	WorkingDir string `json:"working_dir"`
}

// childDaemonRecordEnv is the launcher's half, run before each spawn: when
// this process holds the lock it removes the record an earlier child left —
// one that crashed removes nothing, and its record must not stand for the
// child about to boot — and returns the environment entry that names the
// record's path to the next one. The entry names no path when this process
// holds no lock with a directory (the Windows session mutex alone, when the
// cross-session lock failed open): it is still set, so the child does not
// take its launcher for one from before the record and write beside a lock
// another process holds (childDaemonRecordPath).
func childDaemonRecordEnv() []string {
	path := heldDaemonRecordPath()
	removeDaemonRecord(path)
	return []string{daemonRecordEnv + "=" + path}
}

// childDaemonRecordPath is the child's record path: the one its launcher
// named (daemonRecordEnv, "" for none), or — when the launcher named nothing
// at all, being a release from before the record — the record's place beside
// the lock, while a lock is held there and a launcher spawned this child
// (_MOOMBOX_LAUNCHER_VERSION, which every launcher since the version
// handshake sets and a child run by hand does not). The supervisor keeps
// running the binary it started from across every in-place update until a
// full stop/start (launcherStalenessNote), so after an in-app update into
// this release the launcher that respawns the child is the old one, and
// without this the child recorded nothing and `add` refused every add until
// the whole process was stopped — the dashboard's and the TUI's Restart
// respawn the child under that same launcher. Such a launcher removes no
// record either, so the one a crashed predecessor left is removed here, as
// the launcher would have before the spawn; initServices calls this before
// the database opens. Until then — the crash backoff — that record stands
// for the child the same launcher is about to start, from the same directory
// and config file. A launcher from before the handshake as well is not
// told from a child run by hand: its child records nothing, and `add` refuses
// until a full stop/start.
func childDaemonRecordPath() string {
	if path, named := os.LookupEnv(daemonRecordEnv); named {
		return path
	}
	if os.Getenv("_MOOMBOX_LAUNCHER_VERSION") == "" {
		return ""
	}
	dir := daemonLockDir()
	if dir == "" {
		return ""
	}
	if held, err := daemonLockHeld(dir); err != nil || !held {
		return ""
	}
	path := filepath.Join(dir, daemonRecordName)
	removeDaemonRecord(path)
	return path
}

// heldDaemonRecordPath is the record's path beside the lock this process
// holds, "" when it holds none.
func heldDaemonRecordPath() string {
	if heldLockDir == "" {
		return ""
	}
	return filepath.Join(heldLockDir, daemonRecordName)
}

// writeDaemonRecord is the child's half: it records, at path, the database
// it opened from dbPath and the config it loaded from configPath (both as the
// config spells them, resolved here against this process's working
// directory). A no-op when path is "" (childDaemonRecordPath found none). A
// failure is a Warn and nothing more: without the record `add` refuses while
// this daemon runs, and a database that opened is no reason to stop.
func writeDaemonRecord(path, dbPath, configPath string, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) {
	if path == "" {
		return
	}
	rec, err := newDaemonRecord(dbPath, configPath)
	if err == nil {
		var data []byte
		if data, err = json.MarshalIndent(rec, "", "  "); err == nil {
			err = utils.WriteFileAtomic(path, append(data, '\n'), 0o600)
		}
	}
	if err != nil {
		log.Warn("Could not record the database for `moombox add` — it refuses while this daemon runs",
			slog.String("record", path), slog.String("error", err.Error()))
	}
}

// newDaemonRecord is this process's record for dbPath and configPath.
func newDaemonRecord(dbPath, configPath string) (daemonRecord, error) {
	wd, err := os.Getwd()
	if err != nil {
		return daemonRecord{}, err
	}
	rec := daemonRecord{PID: os.Getpid(), WorkingDir: wd, DatabasePath: absUnder(wd, dbPath)}
	if configPath != "" {
		rec.ConfigPath = absUnder(wd, configPath)
	}
	return rec, nil
}

// removeDaemonRecord removes the record at path; "" and a record already gone
// are no-ops.
func removeDaemonRecord(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// daemonState is what `add` knows about the daemon this user runs.
type daemonState struct {
	// running: a process holds the single-instance lock.
	running bool
	// record is what it recorded, nil while it has recorded nothing (it is
	// still starting, or it predates the record).
	record *daemonRecord
}

// findRunningDaemon probes the lock and, while it is held, reads the record
// beside it. A lock that cannot be probed counts as not held: `add` then
// takes the no-daemon rule, which refuses a relative database_path that does
// not resolve to a database, as it does for a daemon another account runs.
func findRunningDaemon() daemonState {
	dir := daemonLockDir()
	if dir == "" {
		return daemonState{}
	}
	held, err := daemonLockHeld(dir)
	if err != nil || !held {
		return daemonState{}
	}
	return daemonState{running: true, record: readDaemonRecord(filepath.Join(dir, daemonRecordName))}
}

// readDaemonRecord reads the record at path; nil when there is none, or none
// that names an absolute database.
func readDaemonRecord(path string) *daemonRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rec daemonRecord
	if json.Unmarshal(data, &rec) != nil || !filepath.IsAbs(rec.DatabasePath) {
		return nil
	}
	return &rec
}

// noDaemonRecordRefusal is why `add` refuses while a daemon holds the lock
// with no record beside it. Mostly the daemon is still starting, and a moment
// later it has one. One that stays without a record is a supervisor from
// before the version handshake, a child from before the record, or a record
// the child could not write (its Warn says why) — and a Restart from the
// dashboard or the TUI respawns the child under the same supervisor, so the
// remedy it names is a full stop and start.
const noDaemonRecordRefusal = "a Moombox daemon is running but has not recorded its database — retry in a moment if it is still starting; " +
	"if it stays unrecorded, stop Moombox and start it again (a Restart from the dashboard or the TUI keeps the supervisor process, which can be a release that records none), " +
	"and see the daemon's log for a record it could not write"

// resolveAddDatabase is the database file `add` writes to, for a config whose
// database_path is dbPath, loaded from configFile ("" for none), run from cwd.
//
// While a daemon runs, it is the database that daemon recorded, and `add`
// refuses when the config resolves elsewhere, naming both. A relative
// database_path resolves the way the daemon resolved it when the config is
// the very file the daemon loaded — against the daemon's working directory,
// which is what makes `moombox -config <the daemon's config> add` work from
// any directory — and against cwd otherwise, so a second install's config
// with the same "./moombox.db" is refused rather than read as the running
// one's. A daemon that holds the lock but has recorded nothing is refused too
// (noDaemonRecordRefusal): a database guessed while it starts is the one this
// record exists to stop.
//
// With no daemon running, a relative database_path resolves against cwd, and
// the caller refuses it when no database is there (addVideo).
func resolveAddDatabase(dbPath, configFile, cwd string, d daemonState) (string, error) {
	if !d.running {
		return absUnder(cwd, dbPath), nil
	}
	rec := d.record
	if rec == nil {
		return "", errors.New(noDaemonRecordRefusal)
	}
	base := cwd
	if configFile != "" && rec.ConfigPath != "" && samePath(configFile, rec.ConfigPath) {
		base = rec.WorkingDir
	}
	want := absUnder(base, dbPath)
	if samePath(want, rec.DatabasePath) {
		return rec.DatabasePath, nil
	}
	which := "this config"
	if configFile != "" {
		which = configFile
	}
	daemonConfig := ""
	if rec.ConfigPath != "" {
		daemonConfig = " (" + rec.ConfigPath + ")"
	}
	return "", fmt.Errorf("the running Moombox daemon (pid %d) uses the database %s, but the database_path %q in %s resolves to %s — pass the -config the daemon runs with%s, so the job lands in the daemon's database",
		rec.PID, rec.DatabasePath, dbPath, which, want, daemonConfig)
}

// absUnder is path made absolute against base when it is relative.
func absUnder(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// samePath reports whether a and b name one file: the same file when both
// exist (a symlink, an 8.3 name or another case of one on Windows), otherwise
// the same canonical spelling.
func samePath(a, b string) bool {
	if fa, err := os.Stat(a); err == nil {
		if fb, err := os.Stat(b); err == nil {
			return os.SameFile(fa, fb)
		}
	}
	ca, errA := utils.CanonicalPath(a)
	cb, errB := utils.CanonicalPath(b)
	if errA != nil || errB != nil {
		ca, cb = filepath.Clean(a), filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

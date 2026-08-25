package cookies

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Browser-free cookie import from a mounted Firefox profile.
//
// Moombox's Docker image ships no browser at all (the runtime stage installs
// ffmpeg, ca-certificates and tzdata and nothing else), so the browser-launch
// refresh path can never run there. What DOES work in a container is reading
// a Firefox profile directory the operator bind-mounts in — cookies.sqlite is
// plain SQLite and the pure-Go modernc driver opens it on every platform we
// ship. This file is that path: no browser process, and no writes into the
// profile (the browser path drops a user.js in there; a mounted profile is
// treated as strictly read-only input).
//
// Honest scope, so nobody oversells this in the docs: importing refreshes
// whatever the PROFILE holds. It does not renew SAPISID/LOGIN_INFO — YouTube
// only rotates those through a JS challenge we cannot execute — and a profile
// still in use by a live browser session will have its exported cookies
// invalidated by that session's own use.

const (
	// firefoxCookieDBName is the profile-relative cookie database.
	firefoxCookieDBName = "cookies.sqlite"
)

// firefoxCookieDBSidecars are the WAL companions that must travel WITH
// cookies.sqlite whenever it is copied.
//
// This is the whole reason a "just copy the db" implementation is wrong, and
// it was measured rather than assumed. Against a WAL-mode cookies.sqlite
// holding 25 uncheckpointed rows:
//
//	copy cookies.sqlite only          -> opens fine, 0 rows, NO error
//	copy cookies.sqlite + -wal + -shm -> all 25 rows
//
// A silently-empty cookie jar is the single worst outcome for this feature,
// so the sidecars are non-negotiable. yt-dlp gets this wrong today
// (yt_dlp/cookies.py copies the main file alone); do not mirror it.
//
// Note also that `immutable=1` is the WRONG DSN for a live WAL database — it
// tells SQLite the file cannot change, so the -wal is ignored and stale (or
// empty) data comes back. queryFirefoxCookieDB's
// `mode=ro&_pragma=busy_timeout(2000)` is correct against the writable temp
// copy this file produces.
var firefoxCookieDBSidecars = []string{"-wal", "-shm"}

// snapshotFirefoxCookieDB copies cookies.sqlite and its -wal/-shm sidecars
// into a private temp directory and returns that directory plus a cleanup
// func the caller MUST defer (the copy contains the user's live session
// cookies).
//
// Copying rather than opening in place buys two things:
//
//  1. SQLite never writes into the user's profile — not even the WAL-index
//     recovery a read-write open can perform.
//  2. It reads a database a running Firefox has locked. Firefox's exclusive
//     lock is a byte-range lock at SQLite's 1GB lock page, far past end of
//     file, so a plain byte copy succeeds where a second SQLite connection is
//     refused outright.
//
// The temp dir is created 0o700 by os.MkdirTemp; on Windows %TEMP% is already
// per-user, so no extra ACL shell-out is worth the 30-80ms it costs here.
func snapshotFirefoxCookieDB(profileDir string) (string, func(), error) {
	srcDB := filepath.Join(profileDir, firefoxCookieDBName)
	if _, err := os.Stat(srcDB); err != nil {
		return "", func() {}, fmt.Errorf("stat %s: %w", firefoxCookieDBName, err)
	}

	tmpDir, err := os.MkdirTemp("", "moombox-cookiedb-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create cookie snapshot dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	if err := copyFile(srcDB, filepath.Join(tmpDir, firefoxCookieDBName)); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("copy %s: %w", firefoxCookieDBName, err)
	}
	// Sidecars are optional: a cleanly-checkpointed profile has no -wal at
	// all. Their ABSENCE is fine; failing to copy one that exists is not,
	// because that is precisely the silently-empty case.
	for _, suffix := range firefoxCookieDBSidecars {
		src := srcDB + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, filepath.Join(tmpDir, firefoxCookieDBName+suffix)); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("copy %s%s: %w", firefoxCookieDBName, suffix, err)
		}
	}
	return tmpDir, cleanup, nil
}

// copyFile copies src to dst with 0o600 permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// firefoxCookieDBExists reports whether profileDir looks like it holds an
// importable Firefox cookie database. Cheap enough for a startup gate.
func firefoxCookieDBExists(profileDir string) bool {
	if profileDir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(profileDir, firefoxCookieDBName))
	return err == nil && !info.IsDir()
}

// countNetscapeCookieRows counts the DATA rows in a Netscape cookie file.
// `#HttpOnly_`-prefixed lines are data despite the leading '#'; every other
// comment and blank line is not.
func countNetscapeCookieRows(content string) int {
	n := 0
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "#HttpOnly_") {
			continue
		}
		n++
	}
	return n
}

// lockedDBErrorMarkers are the substrings that identify "someone else holds
// this file". The SQLite ones come from the driver; the Windows one comes
// from the file copy when an AV or backup agent has the db open.
var lockedDBErrorMarkers = []string{
	"database is locked",
	"sqlite_busy",
	"being used by another process",
	"sharing violation",
	"resource temporarily unavailable",
}

// isLockedDBError reports whether err looks like lock contention rather than
// a broken database.
func isLockedDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range lockedDBErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// classifyCookieDBError turns a raw read failure into one of the two
// actionable sentinels, keeping the driver's message as context. A locked
// database and a corrupt one need opposite responses from the operator, so
// they must not arrive as the same error.
func classifyCookieDBError(err error) error {
	if err == nil {
		return nil
	}
	if isLockedDBError(err) {
		return fmt.Errorf("%w — stop Firefox (or copy the profile while it is closed) and try again: %v",
			ErrCookieDBLocked, err)
	}
	return fmt.Errorf("%w — the file may be truncated, corrupt, or not a Firefox cookie database: %v",
		ErrCookieDBUnreadable, err)
}

// importProfileCookies reads cookies straight out of the configured browser
// profile directory without launching anything, and returns them in Netscape
// format.
//
// Every failure below is deliberately distinguishable, because the operator's
// next step differs for each one: fix the mount, point at the profile itself
// rather than its parent, stop Firefox, or re-export the profile. A generic
// "import failed" would be useless in a container where there is no UI to
// poke at.
func (s *AutoCookieService) importProfileCookies() (string, error) {
	if s.profileDirErr != nil {
		return "", s.profileDirErr
	}
	if s.profileDir == "" {
		return "", fmt.Errorf("no browser_profile_dir configured: %w", ErrProfileNotFound)
	}

	info, err := os.Stat(s.profileDir)
	if err != nil {
		return "", fmt.Errorf("browser profile dir %q is not readable: %w", s.profileDir, ErrProfileNotFound)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("browser profile dir %q is a file: %w", s.profileDir, ErrProfileNotADirectory)
	}
	dbPath := filepath.Join(s.profileDir, firefoxCookieDBName)
	switch dbFile, statErr := os.Open(dbPath); {
	case statErr == nil:
		dbFile.Close()
	case errors.Is(statErr, fs.ErrNotExist):
		return "", fmt.Errorf("no %s in %q — mount the single Firefox profile directory (the one containing prefs.js), not its parent: %w",
			firefoxCookieDBName, s.profileDir, ErrCookieDBNotFound)
	case errors.Is(statErr, fs.ErrPermission):
		// The common shape of this in Docker: the bind-mounted profile is
		// owned by the host user while the container runs as another uid
		// (compose's `user:` key). Naming the file makes that fixable.
		return "", fmt.Errorf("%s in %q is not readable by this process — check ownership/permissions on the mounted profile: %w",
			firefoxCookieDBName, s.profileDir, ErrCookieDBUnreadable)
	default:
		// Anything else — notably a Windows sharing violation from an AV or
		// backup agent holding the file — goes through the classifier so
		// "someone else has it open" is reported as locked, not corrupt.
		return "", classifyCookieDBError(fmt.Errorf("open %s in %q: %w", firefoxCookieDBName, s.profileDir, statErr))
	}

	netscape, err := readFirefoxCookies(s.profileDir)
	if err != nil {
		return "", err
	}

	s.logger.Info("imported cookies from browser profile without launching a browser",
		"profile_dir", s.profileDir, "cookies", countNetscapeCookieRows(netscape))
	return netscape, nil
}

// shouldSeedFromProfileAtStartup reports whether the service should run one
// import immediately instead of waiting a full refresh interval.
//
// It fires only in the browserless case. On a desktop with a browser
// installed the normal refresh path already owns the profile, and an
// unsolicited startup pass there would just launch a browser nobody asked
// for. In a container there is no browser and the periodic interval defaults
// to six hours — an operator who just mounted a profile (or restarted after
// their cookies died) should not wait that long for it to be read.
func (s *AutoCookieService) shouldSeedFromProfileAtStartup() bool {
	if s.profileDirErr != nil || s.profileDir == "" {
		return false
	}
	if s.resolvedBrowser() != nil {
		return false
	}
	return firefoxCookieDBExists(s.profileDir)
}

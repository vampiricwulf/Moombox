package cookies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
// whose schema is checkpointed but whose rows are not (the shape of any
// long-lived profile):
//
//	copy cookies.sqlite only   -> opens fine, 0 rows, NO error
//	copy cookies.sqlite + -wal -> every row
//
// A silently-empty cookie jar is the single worst outcome for this feature,
// so the -wal is non-negotiable. yt-dlp gets this wrong today
// (yt_dlp/cookies.py copies the main file alone); do not mirror it.
//
// -shm is deliberately NOT copied. It is pure derived state — the WAL index —
// which SQLite rebuilds from the -wal, and copying it could only hurt: taken
// after the -wal it can describe more frames than the copied log contains,
// which is the one way a snapshot could serve stale data. The
// "main plus wal without shm" case in TestSnapshotCopiesWALSidecars is the
// measurement that says leaving it behind loses nothing.
//
// Rebuilding that index is also why the copy goes into a WRITABLE temp dir.
// `mode=ro` stops SQLite writing to the DATABASE, not to the directory, so it
// can still create the -shm it needs. `immutable=1` would be the wrong DSN
// here for the same reason the main file alone is wrong: it tells SQLite the
// file cannot change, so the -wal is ignored and stale or empty data comes
// back.
var firefoxCookieDBSidecars = []string{"-wal"}

// errSnapshotTorn reports that the profile's cookie database changed while it
// was being copied, so the copy may pair a main file and a -wal that
// disagree. Retryable: the fix is another copy, not another query of this one.
var errSnapshotTorn = errors.New("cookie database changed during snapshot")

// snapshotMaxAge bounds how long an abandoned snapshot directory may survive
// before the sweep reclaims it. Comfortably longer than any read, so a
// concurrent snapshot is never yanked out from under its own reader.
const snapshotMaxAge = time.Hour

// snapshotDirPrefix is both the os.MkdirTemp pattern and what the sweep
// matches on, so the two can never drift apart.
const snapshotDirPrefix = "moombox-cookiedb-"

var snapshotSweepOnce sync.Once

// snapshotFirefoxCookieDB copies cookies.sqlite and its -wal sidecar into a
// private temp directory and returns that directory plus a cleanup func the
// caller MUST defer (the copy contains the user's live session cookies).
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
// The copy is not atomic against a live Firefox, so the database is
// fingerprinted before and after; a change means the pair may be inconsistent
// and errSnapshotTorn asks the caller to try again. Without that check a torn
// pair yields a PARTIAL cookie set with no error at all, which no retry loop
// would ever retry.
//
// The temp dir is created 0o700 by os.MkdirTemp; on Windows %TEMP% is already
// per-user, so no extra ACL shell-out is worth the 30-80ms it costs here.
func snapshotFirefoxCookieDB(profileDir string) (string, func(), error) {
	// Reclaim snapshots abandoned by a previous run that was SIGKILLed
	// mid-read. Each one holds live session cookies at 0600.
	snapshotSweepOnce.Do(func() { sweepStaleCookieSnapshots(os.TempDir(), snapshotMaxAge) })

	srcDB := filepath.Join(profileDir, firefoxCookieDBName)
	if _, err := os.Stat(srcDB); err != nil {
		return "", func() {}, fmt.Errorf("stat %s: %w", firefoxCookieDBName, err)
	}
	before := fingerprintCookieDB(srcDB)

	tmpDir, err := os.MkdirTemp("", snapshotDirPrefix)
	if err != nil {
		return "", func() {}, fmt.Errorf("create cookie snapshot dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	if err := copySnapshotFile(srcDB, filepath.Join(tmpDir, firefoxCookieDBName)); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("copy %s: %w", firefoxCookieDBName, err)
	}
	// A sidecar's ABSENCE is fine — a cleanly-checkpointed profile has no
	// -wal at all. A sidecar that exists and cannot be read is NOT fine, and
	// must never be quietly skipped: the main file may still hold a stale
	// checkpointed set, which would then come back as a perfectly valid
	// looking cookie jar full of dead credentials. Only fs.ErrNotExist means
	// absence; a permission error (uid mismatch on a bind mount, an SELinux
	// or AppArmor label, a restrictive mode left by docker cp / rsync) is a
	// hard failure. Falling back to reading the live database would not help
	// either — SQLite hits the same unreadable -wal.
	//
	// That reasoning covers the SOURCE only. A failure on the destination
	// side of the copy is our own temp directory misbehaving and is reported
	// as such, so the caller degrades to the live database instead of
	// blaming a profile that is perfectly readable.
	for _, suffix := range firefoxCookieDBSidecars {
		src := srcDB + suffix
		if _, err := os.Stat(src); err != nil {
			if isMissingSidecar(err) {
				continue
			}
			cleanup()
			return "", func() {}, fmt.Errorf("%w: %s%s exists but cannot be read (%v) — its contents would be silently missing from the import",
				ErrCookieDBUnreadable, firefoxCookieDBName, suffix, err)
		}
		if err := copySnapshotFile(src, filepath.Join(tmpDir, firefoxCookieDBName+suffix)); err != nil {
			cleanup()
			// Which END failed decides the whole response. A failure writing
			// OUR copy into OUR temp dir (no space, a transient error on the
			// temp filesystem) says nothing about the profile: the live
			// fallback is safe, it is what the same failure on the main file
			// already did, and it is what this function's doc comment
			// promises. Only a failure READING the sidecar means the profile
			// itself is unreadable, and that one must keep refusing the
			// fallback — SQLite would hit the same -wal and could hand back a
			// stale checkpointed set as if it were current.
			if isDestinationCopyFault(err) {
				return "", func() {}, fmt.Errorf("could not write %s%s into the snapshot dir %q: %v",
					firefoxCookieDBName, suffix, tmpDir, err)
			}
			return "", func() {}, fmt.Errorf("%w: %s%s exists but could not be read (%v) — its contents would be silently missing from the import",
				ErrCookieDBUnreadable, firefoxCookieDBName, suffix, err)
		}
	}

	if fingerprintsDiffer(before, fingerprintCookieDB(srcDB)) {
		cleanup()
		return "", func() {}, errSnapshotTorn
	}
	return tmpDir, cleanup, nil
}

// isMissingSidecar reports whether a sidecar stat error means "there is no
// such file" as opposed to "there is one and I could not look at it". Only the
// former is safe to skip.
func isMissingSidecar(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// fileStamp is the cheap change-detector for one file: presence, size and
// mtime. Enough to notice a Firefox flush landing mid-copy.
type fileStamp struct {
	exists bool
	size   int64
	mod    time.Time
}

// cookieDBFingerprint stamps the database and its -wal together, since a torn
// snapshot is precisely a disagreement between the two.
type cookieDBFingerprint struct {
	main fileStamp
	wal  fileStamp
}

func stampFile(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, size: info.Size(), mod: info.ModTime()}
}

// equal compares two stamps of the same file. The mtime goes through
// time.Equal rather than `==`: struct equality on a time.Time compares the
// wall clock, the monotonic reading AND the Location pointer, so two stamps
// of the same instant can compare unequal. Both stamps happen to come from
// os.Stat today (no monotonic reading, same Location), which is exactly what
// makes the trap easy to spring later — a false "torn" verdict costs a retry
// and, on the last attempt, a fall back to the live database.
func (a fileStamp) equal(b fileStamp) bool {
	return a.exists == b.exists && a.size == b.size && a.mod.Equal(b.mod)
}

func fingerprintCookieDB(dbPath string) cookieDBFingerprint {
	return cookieDBFingerprint{main: stampFile(dbPath), wal: stampFile(dbPath + "-wal")}
}

func fingerprintsDiffer(a, b cookieDBFingerprint) bool {
	return !a.main.equal(b.main) || !a.wal.equal(b.wal)
}

// sweepStaleCookieSnapshots removes snapshot directories left behind by a
// process that died mid-read. Only directories carrying our own prefix and
// older than maxAge are touched, so a snapshot a concurrent read is still
// using is never removed. Best-effort: every error is ignored, because this
// is housekeeping and must never fail a cookie read.
func sweepStaleCookieSnapshots(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), snapshotDirPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

// copyFault says which END of a file copy failed.
//
// The distinction is load-bearing for the cookie snapshot: a failure on the
// SOURCE means the user's profile cannot be read, which must never be
// papered over; a failure on the DESTINATION means our own temp directory is
// full or flaky, which says nothing at all about the profile and must not be
// reported as one.
type copyFault struct {
	dest bool // true: the failure happened writing our copy, not reading theirs
	err  error
}

func (c *copyFault) Error() string {
	if c.dest {
		return "write copy: " + c.err.Error()
	}
	return "read source: " + c.err.Error()
}

func (c *copyFault) Unwrap() error { return c.err }

// isDestinationCopyFault reports whether a copy failed on the destination
// (our temp dir) rather than on the source file.
func isDestinationCopyFault(err error) bool {
	var fault *copyFault
	return errors.As(err, &fault) && fault.dest
}

// writeFaultRecorder notes whether io.Copy's error came out of the WRITE
// half. io.Copy collapses both directions into one error value, and the
// caller has to be able to tell them apart. Wrapping the destination in a
// plain io.Writer also keeps io.Copy off the fd-to-fd fast paths, which is
// what makes the attribution reliable.
type writeFaultRecorder struct {
	w   io.Writer
	err error
}

func (r *writeFaultRecorder) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	if err != nil {
		r.err = err
	}
	return n, err
}

// copySnapshotFile is the copy the cookie-database snapshot goes through.
// A package variable so tests can inject the failure modes that matter here
// (a full temp filesystem, an unreadable sidecar) without needing a
// filesystem that can actually produce them on every supported platform.
var copySnapshotFile = copyFile

// copyFile copies src to dst with 0o600 permissions. Every error it returns
// is a *copyFault, so callers can tell a source-side failure from a
// destination-side one.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return &copyFault{err: err}
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return &copyFault{dest: true, err: err}
	}
	recorder := &writeFaultRecorder{w: out}
	if _, err := io.Copy(recorder, in); err != nil {
		out.Close()
		os.Remove(dst)
		return &copyFault{dest: recorder.err != nil, err: err}
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return &copyFault{dest: true, err: err}
	}
	return nil
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

// cookieRowPlatform returns which platform a Netscape row belongs to
// ("youtube", "twitch") or "" for anything else. Google rows count as YouTube
// because that is where the shared auth cookies (SID/HSID/SAPISID) live and
// they must always be restored or replaced as one set with youtube.com's.
func cookieRowPlatform(row string) string {
	trimmed := strings.TrimPrefix(row, "#HttpOnly_")
	fields := strings.Split(trimmed, "\t")
	if len(fields) < 7 {
		return ""
	}
	domain := fields[0]
	switch {
	case isYouTubeDomain(domain) || isGoogleDomain(domain):
		return "youtube"
	case isTwitchDomain(domain):
		return "twitch"
	default:
		return ""
	}
}

// netscapeDataRows returns the data rows of a Netscape cookie file.
// `#HttpOnly_` lines are data despite the leading '#'.
func netscapeDataRows(content string) []string {
	var rows []string
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		rows = append(rows, line)
	}
	return rows
}

// restorePlatformRows rebuilds a cookie file from `merged`, swapping every row
// belonging to a platform in `restore` back to the rows `previous` held.
//
// Per-platform rather than whole-file, because the platforms are entirely
// independent domains: an import that fixes YouTube and breaks Twitch should
// keep the YouTube fix. The unit of restore is the platform's whole row set,
// never individual cookies, so a platform's credentials stay internally
// coherent instead of mixing generations.
func restorePlatformRows(merged, previous string, restore map[string]bool) string {
	out := []string{
		"# Netscape HTTP Cookie File",
		"# Extracted by Moombox auto-cookie service",
		"",
	}
	for _, row := range netscapeDataRows(merged) {
		if !restore[cookieRowPlatform(row)] {
			out = append(out, row)
		}
	}
	now := time.Now().Unix()
	for _, row := range netscapeDataRows(previous) {
		// Same expiry prune mergeCookieFiles applies, so a restore cannot
		// resurrect rows the merge had already dropped as expired.
		if restore[cookieRowPlatform(row)] && !rowExpired(row, now) {
			out = append(out, row)
		}
	}
	return strings.Join(out, "\n") + "\n"
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

	info, err := statProfileDir(s.profileDir)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		// Genuinely absent. This IS the manual ladder's bottom rung, and it
		// keeps ErrProfileNotFound. Normally unreachable from a refresh —
		// RefreshCookiesDetailed's own gate catches it first — but the two
		// stats are not atomic, and a profile deleted between them must not be
		// reported as a permissions problem.
		return "", fmt.Errorf("browser profile dir %q does not exist: %w", s.profileDir, ErrProfileNotFound)
	default:
		// Everything else: EACCES, ENOTDIR, an over-long path. The profile IS
		// there and this process cannot look at it, which is a different state
		// with a different remedy — so NOT ErrProfileNotFound, which is what
		// this returned for every stat failure before. That put the compose
		// uid-mismatch case on the ladder's bottom rung: R F and the
		// dashboard's shift+click both reported "No browser profile found",
		// ran a plain recheck instead, and threw away the one sentence that
		// would have fixed it — on the exact path a container operator uses.
		return "", fmt.Errorf("browser profile dir %q is not readable by this process (%v) — check "+
			"ownership and permissions on the mounted profile, and that every parent directory is "+
			"traversable: %w", s.profileDir, err, ErrProfileDirUnreadable)
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

	netscape, stats, err := readFirefoxCookies(s.profileDir)
	s.logFirefoxReadStats(stats)
	if err != nil {
		return "", err
	}

	s.logger.Info("imported cookies from browser profile without launching a browser",
		"profile_dir", s.profileDir, "cookies", countNetscapeCookieRows(netscape))
	return netscape, nil
}

// verificationState is the outcome of one platform auth check. The third
// state is the point: "the check errored" is NOT the same as "the credentials
// are dead", and conflating them is how a network blip ends up telling the
// user to sign in again — or, worse, how an unevaluated cookie set gets
// committed over a working one.
type verificationState int

const (
	verifyUnknown verificationState = iota // callback errored — we learned nothing
	verifyFailed                           // conclusively not authenticated
	verifyOK                               // conclusively authenticated
)

// platformAuth pairs "are there credentials on disk for this platform" with
// what verifying them concluded.
//
// attempted splits verifyUnknown in two, because the two halves license
// different things. "The site could not answer" (a 429, a dropped connection)
// says nothing about a credential and a caller may reasonably accept a login
// over it; "we could not form the question" — a jar with no cookie header or
// no SAPISIDHASH to build, ErrAuthCheckNotAttempted — means no request was ever
// made, so there is no answer to accept. Both stay verifyUnknown: neither is a
// finding about the credentials, which is what the state records.
type platformAuth struct {
	hasCookies bool
	state      verificationState
	attempted  bool
}

func (p platformAuth) ok() bool { return p.state == verifyOK }

// checkPlatformAuth verifies both platforms against the CURRENT jar contents.
//
// The bool projection (`state == verifyOK`) is exactly what RefreshCookies
// computed inline before, including the "no verify callback wired" contract:
// presence is then the only signal available, so it is reported as success
// with a warning rather than counted as a verification failure.
//
// hasCookies asks whether the platform was ever CONFIGURED, not whether the
// credential set is complete. The complete-set predicates conflate "there is
// nothing here" with "there is something here and part of it is missing", and
// only the first of those licenses skipping the check: a jar holding SAPISID
// with LOGIN_INFO cleared can still authenticate, and only the site can say.
// Reading it strictly reported verifyFailed on a check that never ran, which
// (a) told a container operator "Moombox now holds no youtube cookies at all"
// about a file plainly holding SAPISID, contradicting the dashboard's own
// HasYouTubeCookies at the same instant, and (b) left platformsToRestore
// unable to see either a regression or an unknown, so a destructive import
// went in over a working session with no rollback.
func (s *AutoCookieService) checkPlatformAuth(ctx context.Context) (yt, tw platformAuth) {
	vctx, cancel := context.WithTimeout(ctx, authVerifyTimeout)
	defer cancel()

	check := func(hasCookies bool, verify func(context.Context) (bool, error), platform string) platformAuth {
		if !hasCookies {
			// No credential of any kind for this platform. verifyFailed is a
			// conclusion, not an assumption: there is nothing to send, so no
			// request can be authenticated, and the callback would answer the
			// same after a round trip it does not need to make. Everything
			// downstream reads hasCookies=false as "nothing to protect / do
			// not blame the user", so this stays silent rather than alarming.
			return platformAuth{hasCookies: false, state: verifyFailed}
		}
		if verify == nil {
			s.logger.Warn(platform + " auth verification callback not wired — reporting based on cookie presence alone")
			return platformAuth{hasCookies: true, state: verifyOK}
		}
		verified, err := verify(vctx)
		switch {
		case err != nil:
			// ErrAuthCheckNotAttempted means the callback gave up before any
			// request left the process. Still unknown — nothing was learned
			// about the credentials either way — but not the same unknown as a
			// site that would not answer, and FinishSetup has to tell them
			// apart before it reports a completed sign-in.
			return platformAuth{
				hasCookies: true,
				state:      verifyUnknown,
				attempted:  !errors.Is(err, ErrAuthCheckNotAttempted),
			}
		case verified:
			return platformAuth{hasCookies: true, state: verifyOK, attempted: true}
		default:
			return platformAuth{hasCookies: true, state: verifyFailed, attempted: true}
		}
	}

	yt = check(s.jar.HasAnyYouTubeAuthCookie(), s.VerifyYouTubeAuth, "YouTube")
	tw = check(s.jar.HasAnyTwitchAuthCookie(), s.VerifyTwitchAuth, "Twitch")
	return yt, tw
}

// platformsToRestore decides which platforms an import must give back.
//
// Two independent reasons, both scoped to a single platform so an import that
// helps one and harms the other is not judged as a whole:
//
//   - REGRESSION: it verified before the import and does not after. This is
//     the case that silently destroys a working credential, because
//     mergeCookieFiles lets the imported value win by name+domain and a
//     sibling platform verifying can mask the loss entirely.
//   - INCONCLUSIVE: the post-import check could not reach the network for a
//     platform that had credentials. We have learned nothing, so committing a
//     set we could not evaluate over one that may be fine is a bet with no
//     upside.
//
// A platform that was already dead is deliberately NOT restored: replacing
// dead cookies with other dead cookies costs nothing, and the fresher set is
// the better guess for the next attempt.
//
// Both arms depend on checkPlatformAuth asking the CONFIGURED question rather
// than the complete-set one. A half-cleared but still working session used to
// arrive here as {hasCookies:false, state:verifyFailed} — neither ok() nor
// hasCookies — so a stale import was committed straight over it and nothing
// said so. It now arrives as {true, verifyOK} and hits the REGRESSION arm when
// the import is dead, or the INCONCLUSIVE arm when the post-check cannot
// complete. A jar with no credential at all is still {false, verifyFailed} and
// still restores nothing, which is what keeps seeding a fresh container from
// being treated as a loss.
func platformsToRestore(pre, post map[string]platformAuth) map[string]bool {
	restore := map[string]bool{}
	for platform, before := range pre {
		after := post[platform]
		switch {
		case before.ok() && after.state == verifyFailed:
			restore[platform] = true
		case before.hasCookies && after.state == verifyUnknown:
			restore[platform] = true
		}
	}
	return restore
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

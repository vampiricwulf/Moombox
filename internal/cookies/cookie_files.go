package cookies

// cookie_files.go — cookie file hygiene: the atomic write, sweeping orphaned
// temp files, tightening a cookie directory's ACL/permissions, and carrying
// the cookie file into the one a first-run setup saves.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// writeCookieFile is the cookie-file write RefreshCookies goes through, as a
// package variable so tests can exercise the branches that only exist for a
// FAILED write — notably a rollback that cannot put the previous credentials
// back, which decides what the operator is told is on disk.
var writeCookieFile = writeFileAtomic

// readCookieFile is the read FinishSetup and RefreshCookiesDetailed go
// through before merging freshly-extracted cookies into an existing
// cookies.txt, as a package variable so tests can exercise a read that fails
// for a reason OTHER than "file does not exist" (a permission blip, a locked
// file, an I/O error) without needing to make a real fixture file
// unreadable — mirrors writeCookieFile above. See the callers for why that
// distinction matters: os.IsNotExist is the normal first-run case, and every
// other error must abort rather than silently proceed as if there were
// nothing to merge.
var readCookieFile = os.ReadFile

// statProfileDir is os.Stat, behind a seam, for the three places that ask
// whether the configured browser profile directory can be looked at: the
// missing-profile gate in RefreshCookiesDetailed, the import in
// importProfileCookies, and the periodic loop's per-tick precondition in
// periodicRefreshHasSource. The first two are the reason it is a seam at all:
// a test needs to drive the real sequence — the gate sees a non-ENOENT error
// and proceeds, and the import classifies it. The third goes through it for
// consistency, so all three answer the same way when a test does substitute;
// TestPeriodicLoopPicksUpAProfileThatAppearsAtRuntime does not substitute, and
// creates a real directory mid-test instead.
//
// A seam rather than a fixture because the states that matter are not portably
// constructible: EACCES needs a chmod that means nothing on Windows, and the
// ENOTDIR shape (a file in the middle of the path) surfaces as ERROR_PATH_NOT_
// FOUND there, which os.IsNotExist reports as true. Building the failure by
// hand would pin the case on one platform and skip it on the other, and the one
// it would skip is the one this seam exists to test.
var statProfileDir = os.Stat

// cookieTempFileMaxAge bounds how long an orphaned writeFileAtomic temp file
// may survive before the sweep below reclaims it. A write completes in
// milliseconds; this is three orders of magnitude of margin, so nothing but
// a genuinely abandoned temp file — left behind by a crash, a kill, or a
// panic between os.CreateTemp and the rename — is ever old enough to match.
// Do not lower this "to be thorough": age is the only guard against
// sweeping a write in progress.
const cookieTempFileMaxAge = time.Hour

// cookieTempFileSweepOnce fires sweepStaleCookieTempFiles exactly once per
// process. Package-level state, same shape as snapshotSweepOnce in
// autocookies_profile.go — different root (the cookie file's own directory,
// not os.TempDir()) and a different secret (the whole cookie file, not a
// browser DB snapshot), so it stays a sibling rather than merging with it.
//
// Wired at service construction (NewAutoCookieService), which is the one
// place in the package that always holds the REAL cookie file path up
// front. writeFileAtomic itself is a generic temp-then-rename helper shared
// with meta.go's cookies.meta.json sidecar writes and refresh.go's own
// cookies.txt rewrite, so keying the "once" off whichever path happens to
// call writeFileAtomic first would risk sweeping with the wrong base name
// if call order ever changed.
var cookieTempFileSweepOnce sync.Once

// writeFileAtomic writes data to a temp file then renames it to the target path,
// preventing corruption on partial failure. Applies
// utils.ApplyUserOnlyDACL to the parent directory (successful attempts are
// memoised per directory; see tightenCookieDirOnce) so the highest-value
// secret in the app (auth-token + SAPISID for the user's session) doesn't
// sit on disk with a parent-inherited world-readable ACL when the cookie
// file lives outside the config dir (e.g. default `./cookies.txt` in the
// project root). The DACL is applied to the parent dir rather than the file
// because (a) icacls /inheritance:r on individual files has corner cases
// where the new ACL ends up over-restrictive, and (b) propagating from the
// dir covers any future writes (rotated cookies, side-files) without
// per-write icacls latency. Real hardening on non-Windows too — see
// utils.ApplyUserOnlyDACL's non-Windows implementation, which chmods to
// 0700/0600 rather than no-op'ing — not just Windows; idempotent once
// applied.
//
// CONDITIONAL on POSIX since owner decision O-K: tightenCookieDirOnce asks
// utils.DirTighteningAllowed first, and a parent that also holds the output
// tree, the staging tree, the database or the log is left alone there (the
// Docker image's /data). Windows is unchanged. The FILE is 0600 either way —
// that is the Chmod on the temp file below, which no gate touches — so what a
// shared directory gives up is only the untraversable parent.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	// Unique temp name (os.CreateTemp): the RefreshService rewrites the same
	// cookies.txt through its own temp file, and a shared fixed ".tmp" name
	// would let two concurrent writers interleave into a corrupt file.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cookie file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp cookie file: %w", err)
	}
	// fsync before rename (matches ResumeStore.Save / WriteChatFileAtomic /
	// config.Save): without it a crash can journal the rename while the data
	// pages never hit disk, leaving a zero-length/torn cookies.txt that Load
	// silently trusts — dropping auth cookies until a full re-login.
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync temp cookie file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp cookie file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp cookie file: %w", err)
	}
	if err := utils.ReplaceFile(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp cookie file: %w", err)
	}
	tightenCookieDirOnce(filepath.Dir(path))
	return nil
}

// sweepCookieTempFilesOnce runs sweepStaleCookieTempFiles through the given
// *sync.Once. Production code always passes &cookieTempFileSweepOnce; tests
// pass a local one so the process-lifetime package var doesn't make every
// test but the first one in the binary a no-op.
func sweepCookieTempFilesOnce(once *sync.Once, dir, base string, maxAge time.Duration) {
	once.Do(func() { sweepStaleCookieTempFiles(dir, base, maxAge) })
}

// sweepStaleCookieTempFiles removes orphaned writeFileAtomic temp files left
// beside the cookie file: a crash, a kill, or a panic between os.CreateTemp
// and the rename leaves `<base>.<random>.tmp` on disk forever, and each one
// is a full copy of cookies.txt — the highest-value secret in the app —
// under a name nothing reads and nothing else removes.
//
// Matches ONLY <base>.<anything>.tmp in dir — the exact shape
// os.CreateTemp(dir, base+".*.tmp") produces in writeFileAtomic. The prefix
// check is anchored on base+"." (not on the directory as a whole), so this
// can never match base itself (no .tmp suffix), never matches an operator
// file like cookies.txt.bak, and never matches an unrelated file's temp
// (other.txt.NNN.tmp).
//
// Age (see cookieTempFileMaxAge) is the guard against sweeping a live
// write. Best-effort — every failure is logged at Debug, with the path only
// and never content or size, and left for the next process start to retry;
// this is housekeeping, not correctness, and must never fail a caller over
// a stale file it couldn't remove.
func sweepStaleCookieTempFiles(dir, base string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Debug("cookie temp file sweep: could not read directory", "dir", dir, "err", err)
		return
	}
	prefix := base + "."
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			slog.Debug("cookie temp file sweep: could not remove orphaned temp file", "path", path, "err", err)
			continue
		}
		slog.Debug("cookie temp file sweep: removed orphaned temp file", "path", path)
	}
}

// applyUserOnlyDACL is utils.ApplyUserOnlyDACL behind a seam, so
// tightenCookieDirOnce's retry-on-failure memoisation (below) can be tested
// with a fake that fails once then succeeds, instead of shelling out to a
// real icacls (Windows) or depending on real chmod failure modes (Linux) in
// CI. Mirrors writeCookieFile/readCookieFile/statProfileDir above.
var applyUserOnlyDACL = utils.ApplyUserOnlyDACL

// dirTighteningAllowed is utils.DirTighteningAllowed behind a seam, for the
// same reason as applyUserOnlyDACL above: the real predicate answers true for
// EVERY directory on Windows — owner decision O-K leaves icacls unchanged — so
// on a Windows host nothing but a substituted verdict can drive the refusal arm
// of tightenCookieDirOnce. config.Save holds a seam over the same function for
// its own twin; both are references to one rule, not two opinions.
var dirTighteningAllowed = utils.DirTighteningAllowed

// dirTightenState is tightenCookieDirOnce's per-directory memo. Two states,
// not one boolean, because "an apply is running right now" and "an apply
// already succeeded" must be told apart: the first still says "don't spawn
// another", the second says "there is nothing left to do, ever". A dir
// absent from the map is the implicit third state, not started.
type dirTightenState int

const (
	dirTighteningInFlight dirTightenState = iota
	dirTighteningDone
)

// tightenCookieDirOnce applies utils.ApplyUserOnlyDACL to the given parent
// dir. Memoised on SUCCESS, not on attempt: icacls (Windows) / chmod
// (Linux — see utils.ApplyUserOnlyDACL's non-Windows implementation, which
// really chmods to 0700/0600 there, not a no-op) is a ~30-80ms shell-out/
// syscall that would otherwise fire on every cookie write, but a transient
// failure (an AV scanner holding the dir, a first-write race with the dir's
// own creation) must not disable hardening for the rest of the process just
// because the failure is demoted to a Debug log line.
//
// Three states per dir (dirTightenState above, plus "absent = not
// started"):
//   - not started: this call marks the dir in flight, synchronously, before
//     spawning the goroutine that runs the apply — so a second write
//     landing during the 30-80ms shell-out sees "in flight" and returns
//     without spawning a second one.
//   - in flight: return; the goroutine already running will resolve it.
//   - done: return; already applied, nothing left to do.
//
// A failure — or a panic mid-apply — deletes the entry, putting the dir
// back to "not started" so the NEXT cookie write retries rather than
// leaving it stuck in flight or falsely memoised as done.
//
// Cost if icacls/chmod fails permanently on a given host: one extra
// ~30-80ms shell-out per cookie write instead of one per process. Writes
// happen on the refresh cadence (30 min) and on imports, so the bound is a
// handful of shell-outs an hour. No failure cap, no backoff — either would
// be a mechanism to contain a mechanism, and nothing has profiled the plain
// retry as costing anything.
//
// GATED on dirTighteningAllowed (owner decision O-K): on POSIX a directory
// that also holds the output tree, the staging tree, the database or the log
// is left alone, so a refused directory never appears in the memo at all —
// do not go looking for an entry for /data that will never be written.
var (
	tightenedCookieDirsMu sync.Mutex
	tightenedCookieDirs   = make(map[string]dirTightenState)
)

func tightenCookieDirOnce(dir string) {
	// Owner decision O-K: on POSIX the 0700 applies only to a DEDICATED
	// directory. In the Docker image this parent is /data — the bind mount
	// holding output/, staging/, the database and the log — and the first
	// cookie write used to undo the Dockerfile's deliberate `chmod 777 /data`.
	// Windows is unchanged. Checked ahead of the memo claim rather than inside
	// the goroutine so a refused directory is never recorded as in-flight; the
	// listing costs a readdir per cookie write, which happens on the 30-minute
	// refresh cadence and on imports. Files stay 0600 either way —
	// writeFileAtomic chmods the temp file before the rename.
	if !dirTighteningAllowed(dir) {
		return
	}

	tightenedCookieDirsMu.Lock()
	if _, ok := tightenedCookieDirs[dir]; ok {
		tightenedCookieDirsMu.Unlock()
		return // in flight or already done
	}
	tightenedCookieDirs[dir] = dirTighteningInFlight
	tightenedCookieDirsMu.Unlock()

	// Read the seam here, on the caller's goroutine: a test swaps it without
	// a lock, and a goroutine an earlier test left running would race it.
	apply := applyUserOnlyDACL
	go func() {
		succeeded := false
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("cookie dir DACL tightening panicked", "dir", dir, "panic", fmt.Sprint(r))
			}
			if !succeeded {
				// Covers both a returned error and a recovered panic: either
				// way the apply did not finish successfully, so the memo
				// goes back to "not started" for the next write to retry.
				tightenedCookieDirsMu.Lock()
				delete(tightenedCookieDirs, dir)
				tightenedCookieDirsMu.Unlock()
			}
		}()
		// This is the hardening for the auth-cookie file (highest-value
		// secret in the app). Demoted to Debug (matches the config +
		// sidecar + profile dir sites): the common failure is ACCESS_DENIED
		// on a dir created under an elevated/admin context, and on the
		// single-user host this app targets nobody else can read it anyway.
		// Raise the log level to Debug to surface the miss. A failure here
		// is retried on the NEXT cookie write, not memoised — see the
		// doc comment above.
		if err := apply(dir); err != nil {
			slog.Debug("could not restrict cookie dir to current user", "dir", dir, "err", err)
			return
		}
		tightenedCookieDirsMu.Lock()
		tightenedCookieDirs[dir] = dirTighteningDone
		tightenedCookieDirsMu.Unlock()
		succeeded = true
	}()
}

// CarryCookieFileTo carries the cookies this service writes — at the cookie
// file it was built with, the boot-time cookies.cookie_file — into newPath,
// the cookie file a first-run setup is about to save. Both wizards call it
// before they save (the Web one's POST /api/setup/complete, the TUI one's
// save command): a browser login run in the wizard wrote to the boot-time
// path, the Advanced step lets the operator name another cookie file beside
// that login, and the restart then loaded an empty jar from the new path
// although the wizard had reported the login Done. One rule for both: the
// cookies the run was using are the cookies the restart loads.
//
// The cookies are merged into newPath when a file is already there, the
// carried ones winning a clash (they are what the wizard just signed in to),
// the same merge every cookie writer uses; an unreadable newPath is refused
// rather than overwritten (ErrCookieFileUnreadable). The refresh sidecar goes
// with them when it is newer than newPath's own. The boot-time file is left
// where it is. Nothing to carry — no boot-time file, an empty one, or newPath
// naming the same file — is not an error.
func (s *AutoCookieService) CarryCookieFileTo(newPath string) error {
	from := s.cookiePath
	if from == "" || newPath == "" || sameFilePath(from, newPath) {
		return nil
	}
	data, err := readCookieFile(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the cookies to carry from %s: %w", from, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return fmt.Errorf("create the directory for %s: %w", newPath, err)
	}
	merged := string(data)
	existing, err := readCookieFile(newPath)
	switch {
	case err == nil:
		if strings.TrimSpace(string(existing)) != "" {
			merged = mergeCookieFiles(string(existing), merged)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("%w — refusing to overwrite %s (%w)", ErrCookieFileUnreadable, newPath, err)
	}
	if err := writeCookieFile(newPath, []byte(merged), 0o600); err != nil {
		return err
	}
	fromMeta, err := LoadMeta(from)
	if err != nil || fromMeta == nil {
		return nil // the sidecar is advisory; the next refresh writes one
	}
	if toMeta, err := LoadMeta(newPath); err == nil && toMeta != nil && !fromMeta.LastRefresh.After(toMeta.LastRefresh) {
		return nil
	}
	return SaveMeta(newPath, *fromMeta)
}

// sameFilePath reports whether a and b name the same file: the same absolute
// path, or — both existing — the same file by os.SameFile.
func sameFilePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil && absA == absB {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

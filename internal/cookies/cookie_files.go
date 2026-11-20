package cookies

// cookie_files.go — cookie file hygiene: the atomic write, sweeping orphaned
// temp files, tightening a cookie directory's ACL/permissions, and carrying
// the cookie file into the one a first-run setup saves.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
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
// preventing corruption on partial failure. The highest-value secret in the
// app (auth-token + SAPISID for the user's session) must not sit on disk with
// permissions inherited from wherever the file happens to live, so the FILE is
// restricted on every write: the Chmod to 0600 on the temp file below, and
// restrictFileToOwner on it (on Windows a protected DACL for the current user,
// SYSTEM and Administrators; nothing elsewhere). The rename carries both to
// the target.
//
// The file's FOLDER is never touched here (owner ruling, 2026-11-19, which
// supersedes owner decision O-K): Moombox sets permissions on the files it
// writes and on a folder at the moment Moombox itself creates it, and never
// changes the permissions of a folder that already existed. Whoever needs the
// folder first creates it with createCookieDir.
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
	// Hardening, not a condition of the write: a volume without ACLs (FAT,
	// some network shares) still gets its cookie file.
	if err := restrictFileToOwner(tmpPath); err != nil {
		slog.Debug("could not restrict cookie file to its owner", "path", path, "err", err)
	}
	if err := utils.ReplaceFile(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp cookie file: %w", err)
	}
	return nil
}

// createCookieDir makes sure the folder a cookie file (or its meta sidecar)
// is about to be written into exists. A folder that is already there is left
// exactly as it is. One Moombox has to create is created for its owner only:
// mode 0700 on POSIX, for the folder and for any missing parent created with
// it. On Windows the mode means nothing and no ACL call is made: the new folder
// inherits its parent's permissions, and the per-file restriction in
// writeFileAtomic is what protects the secret.
func createCookieDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
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

// restrictFileToOwner is utils.RestrictFileToOwner behind a seam, so a test
// can see which file writeFileAtomic restricts and when, and can make it fail.
// The real one starts no process (Win32 calls on Windows, nothing elsewhere).
var restrictFileToOwner = utils.RestrictFileToOwner

// CarryCookieFileTo carries the cookies the operator supplied in this run —
// a browser login FinishSetupDetailed accepted, an import ImportCookies
// installed, both written at the cookie file this service was built with, the
// boot-time cookies.cookie_file — into newPath, the cookie file a first-run
// setup is about to save. Both wizards call it before they save (the Web one's
// POST /api/setup/complete, the TUI one's save command): the Advanced step
// lets the operator name another cookie file beside the login, and the
// restart then loaded an empty jar from the new path although the wizard had
// reported the login Done. One rule for both: what was signed in to in this
// run is what the restart loads.
//
// Only the rows of the platforms supplied in this run (suppliedThisRun) are
// carried, never the boot-time file as a whole: it can hold another install's
// session — an earlier install's ./cookies.txt, a second instance started
// with -config in the same folder — and a setup that ran no login, or signed
// in to one platform only, must not put that over the cookies the operator's
// own file holds. Nothing supplied is nothing carried: newPath is left exactly
// as it is.
//
// The carried rows are merged into newPath when a file is already there, the
// carried ones winning a clash (they are what the wizard just signed in to),
// the same merge every cookie writer uses; an unreadable newPath is refused
// rather than overwritten (ErrCookieFileUnreadable). The refresh sidecar goes
// with them when it is newer than newPath's own, naming as verified only the
// carried platforms the boot-time sidecar names and newPath's own for the
// rest. The boot-time file is left where it is. Nothing to carry — no
// boot-time file, no row of a supplied platform in it, or newPath naming the
// same file — is not an error.
func (s *AutoCookieService) CarryCookieFileTo(newPath string) error {
	from := s.cookiePath
	s.mu.Lock()
	supplied := maps.Clone(s.suppliedThisRun)
	s.mu.Unlock()
	if len(supplied) == 0 || from == "" || newPath == "" || sameFilePath(from, newPath) {
		return nil
	}
	data, err := readCookieFile(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the cookies to carry from %s: %w", from, err)
	}
	var carried []string
	for _, row := range netscapeDataRows(string(data)) {
		if supplied[cookieRowPlatform(row)] {
			carried = append(carried, row)
		}
	}
	if len(carried) == 0 {
		return nil
	}
	if err := createCookieDir(filepath.Dir(newPath)); err != nil {
		return fmt.Errorf("create the directory for %s: %w", newPath, err)
	}
	existing, err := readCookieFile(newPath)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		existing = nil
	default:
		return fmt.Errorf("%w — refusing to overwrite %s (%w)", ErrCookieFileUnreadable, newPath, err)
	}
	merged := mergeCookieFiles(string(existing), strings.Join(carried, "\n"))
	if err := writeCookieFile(newPath, []byte(merged), 0o600); err != nil {
		return err
	}
	fromMeta, err := LoadMeta(from)
	if err != nil || fromMeta == nil {
		return nil // the sidecar is advisory; the next refresh writes one
	}
	toMeta, err := LoadMeta(newPath)
	if err != nil {
		toMeta = nil
	}
	if toMeta != nil && !fromMeta.LastRefresh.After(toMeta.LastRefresh) {
		return nil
	}
	meta := CookieMeta{LastRefresh: fromMeta.LastRefresh}
	for _, platform := range fromMeta.Platforms {
		if supplied[strings.ToLower(platform)] {
			meta.Platforms = append(meta.Platforms, platform)
		}
	}
	if toMeta != nil {
		for _, platform := range toMeta.Platforms {
			if !supplied[strings.ToLower(platform)] {
				meta.Platforms = append(meta.Platforms, platform)
			}
		}
	}
	return SaveMeta(newPath, meta)
}

// noteSuppliedLocked records platform as one whose cookies the operator
// supplied in this run (suppliedThisRun). Caller holds s.mu.
func (s *AutoCookieService) noteSuppliedLocked(platform string) {
	if s.suppliedThisRun == nil {
		s.suppliedThisRun = map[string]bool{}
	}
	s.suppliedThisRun[platform] = true
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

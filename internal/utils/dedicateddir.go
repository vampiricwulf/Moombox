package utils

// dedicateddir.go — the shared "is this a dedicated secrets directory" rule
// behind owner decision O-K (2026-09-17), extended to Windows on 2026-11-19: the
// parent directory lockdown (POSIX chmod 0700, Windows icacls) applies only
// when the secret's parent holds nothing else of Moombox's.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// dedicatedDirScanLimit bounds the listing. A directory Moombox keeps only its
// secrets in has a handful of entries; one with hundreds is, whatever else it
// is, not that — so hitting the limit disqualifies rather than truncating.
const dedicatedDirScanLimit = 512

// sharedDataDirNames are the names that mark a directory as Moombox's DATA
// directory rather than a dedicated secrets directory. Matched
// case-insensitively because Windows and macOS filesystems are, and matched
// against EVERY entry rather than against subdirectories only: the name is the
// marker, and an `output` that presents as a file or a dangling symlink (an
// unmounted bind target, an entry that cannot be stat'd) is still the
// operator's output tree.
var sharedDataDirNames = map[string]bool{
	"output":  true,
	"staging": true,
	"logs":    true,
}

// sharedDataFileExts are the file extensions that mark the same thing. The
// database and the log are both operator-settable (paths.database_path,
// paths.log_file_path), so matching the default basenames alone would miss a
// renamed one — and a renamed database in /data is exactly as much a reason not
// to chmod /data 0700 as moombox.db is. SQLite's -wal/-shm sidecars are
// stripped before the extension is taken.
//
// config.toml is deliberately NOT here: it is a SECRET of the same family as
// cookies.txt (it carries the password hash), it sits beside cookies.txt in
// every dedicated layout, and config.Save is itself one of the two callers —
// counting it as shared data would mean no config directory anywhere was ever
// tightened, which is the opposite of what O-K asks for.
var sharedDataFileExts = map[string]bool{
	".db":      true,
	".sqlite":  true,
	".sqlite3": true,
	".log":     true,
}

// DirHoldsSharedData reports whether dir holds a Moombox surface OTHER than its
// secrets — the output tree, the staging tree, the database, or the log.
//
// It is the content test behind owner decision O-K (2026-09-17). In the Docker
// image cookies.txt lives at /data/cookies.txt, so its parent is the bind mount
// that also holds output/, staging/, the database and the log; chmodding that
// 0700 undoes the Dockerfile's deliberate `chmod 777 /data` and takes the
// operator's archives away from their own host user. A directory holding
// nothing but cookies.txt, config.toml and a browser profile is a different
// thing and still earns the hardening.
//
// CONTENT, not config, for three reasons: internal/config cannot import
// internal/cookies and internal/cookies deliberately does not import
// internal/config, so a shared config-driven predicate would need a new home
// and a new injection site; the `moombox add` side process wires no config
// store at all; and the answer has to be right for a hand-edited layout the
// running config has never seen.
//
// The three boundary answers, all deliberate:
//   - An EMPTY directory holds no other surface, so it is DEDICATED. This is
//     the first-write case — the directory Moombox just created for its own
//     cookies.txt — and it is exactly the one worth tightening.
//   - A directory holding only cookies.txt and its own family (the
//     cookies.txt.<random>.tmp writeFileAtomic leaves mid-write, an operator's
//     cookies.txt.bak, cookies.meta.json) is DEDICATED: none of those names is
//     another Moombox surface.
//   - A directory that cannot be LISTED answers true — SHARED. Erring toward
//     "shared" means the worst case is a missed hardening on a host where the
//     files are already 0600; erring the other way reinstates the container bug
//     on exactly the deployments whose directories are the most unusual.
func DirHoldsSharedData(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()

	// io.EOF is not a failure here: File.ReadDir(n>0) reports it whenever the
	// directory holds fewer than n entries, which includes the empty directory
	// this rule calls dedicated. Any OTHER error means the listing we are about
	// to judge is incomplete, and an incomplete listing can only under-report
	// the shared surfaces — so it fails safe.
	entries, err := f.ReadDir(dedicatedDirScanLimit)
	if err != nil && !errors.Is(err, io.EOF) {
		return true
	}
	if len(entries) == dedicatedDirScanLimit {
		// More entries than any secrets directory has. Whatever this is, it is
		// not a directory Moombox keeps only its own secrets in.
		return true
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		// The NAME is the marker, whatever the entry turns out to be. Tested
		// before the IsDir() branch on purpose: a dangling symlink at a bind
		// mount whose target is not mounted yet, or an entry ReadDir cannot
		// stat, presents as a non-directory while still being the operator's
		// output tree by every name that matters — and the fail-safe direction
		// here is SHARED (a missed hardening on a host whose files are already
		// 0600, against reinstating the container bug).
		if sharedDataDirNames[name] {
			return true
		}
		if e.IsDir() {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, "-wal"), "-shm")
		if sharedDataFileExts[filepath.Ext(base)] {
			return true
		}
	}
	return false
}

// DirTighteningAllowed reports whether ApplyUserOnlyDACL may be applied to dir.
//
// It is the dedicated-directory test on every platform. What the lockdown
// takes away differs, and it is too much for a shared folder either way:
//   - POSIX: chmod 0700 makes the parent untraversable to everyone else,
//     which on a shared data volume is the operator's own host user.
//   - Windows: `icacls /inheritance:r /grant:r <user>:(OI)(CI)F` REMOVES every
//     inherited entry, SYSTEM and Administrators included, from the folder and
//     from everything under it. With the default cookie_file = "./cookies.txt"
//     that folder is the install folder, holding output, staging, the database
//     and the log. (Windows used to be exempt here on the reasoning that icacls
//     only adds inheritable entries; /inheritance:r is what removes the rest.)
//
// The secret FILES do not depend on this: every cookie and config write
// restricts its own temp file before the rename (0600, and RestrictFileToOwner
// on Windows), whatever folder it is in. What a shared folder gives up is only
// the restricted parent.
func DirTighteningAllowed(dir string) bool {
	return !DirHoldsSharedData(dir)
}

// restrictedDirs is FirstDirRestriction's memo.
var (
	restrictedDirsMu sync.Mutex
	restrictedDirs   = make(map[string]struct{})
)

// FirstDirRestriction reports whether this is the first time in this process
// that dir is reported as locked down (ApplyUserOnlyDACL succeeded on it). The
// cookie writer and config.Save each lock down their own file's folder, which
// in a dedicated layout is the same folder, and each logs
// "restricted folder to the current user" only when this answers true, so the
// operator reads it once per folder. Keyed on the absolute path, which is also
// returned for the log line: the cookie writer's default folder is spelled ".".
func FirstDirRestriction(dir string) (absDir string, first bool) {
	absDir = dir
	if abs, err := filepath.Abs(dir); err == nil {
		absDir = abs
	}
	absDir = filepath.Clean(absDir)
	key := absDir
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	restrictedDirsMu.Lock()
	defer restrictedDirsMu.Unlock()
	if _, seen := restrictedDirs[key]; seen {
		return absDir, false
	}
	restrictedDirs[key] = struct{}{}
	return absDir, true
}

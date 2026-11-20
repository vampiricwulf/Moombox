package utils

// dedicateddir.go — the content test that tells Moombox's DATA folder (output,
// staging, the database, the log) from a folder that holds only its secrets.
//
// It decides nothing about permissions any more. Owner decision O-K
// (2026-09-17) used it to gate a lockdown of the cookie and config folders;
// the owner ruling of 2026-11-19 supersedes that: Moombox sets permissions on
// the files it writes and on a folder at the moment Moombox itself creates it,
// and never changes the permissions of a folder that already existed. What is
// left is one reader, the startup notice (cmd/moombox/folder_permissions.go),
// which names a data folder an earlier version stripped of inheritance.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
// to call /data the data folder as moombox.db is. SQLite's -wal/-shm sidecars are
// stripped before the extension is taken.
//
// config.toml is deliberately NOT here: it is a SECRET of the same family as
// cookies.txt (it carries the password hash) and sits beside cookies.txt in
// every secrets-only layout.
var sharedDataFileExts = map[string]bool{
	".db":      true,
	".sqlite":  true,
	".sqlite3": true,
	".log":     true,
}

// DirHoldsSharedData reports whether dir holds a Moombox surface OTHER than its
// secrets — the output tree, the staging tree, the database, or the log.
//
// The two layouts it tells apart: the Docker image keeps cookies.txt and
// config.toml in /data, the bind mount that also holds output/, staging/, the
// database and the log, and a default Windows install keeps them in the
// install folder beside the same things; a folder holding nothing but
// cookies.txt, config.toml and a browser profile is a different thing.
//
// CONTENT, not config: the answer has to be right for a hand-edited layout the
// running config has never seen, and for a database or log the operator
// renamed.
//
// The three boundary answers, all deliberate:
//   - An EMPTY directory holds no other surface, so it answers false.
//   - A directory holding only cookies.txt and its own family (the
//     cookies.txt.<random>.tmp writeFileAtomic leaves mid-write, an operator's
//     cookies.txt.bak, cookies.meta.json) answers false: none of those names
//     is another Moombox surface.
//   - A directory that cannot be LISTED answers true — SHARED.
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
	// the shared surfaces, so it answers shared.
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
		// output tree by every name that matters.
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

package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ReplaceFile renames tmp over path — the last step of the atomic writes that
// go through it (cookies.txt, chat files, resume sidecars, config.toml, the
// worker's downloaded assets and its post-mux single-part video/chat-sidecar
// rename, and now WriteFileAtomic). On Windows
// an antivirus or indexer briefly holds a freshly written file open and the
// replace is refused with ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION even
// though nothing is wrong with either file; those two are retried with a
// growing pause (about a second in total) before the last error is returned.
// Every other error is returned on the first attempt, and elsewhere than
// Windows this is exactly os.Rename.
//
// A successful replace also fsyncs path's directory (syncDir): the callers
// fsync the file's bytes, but the entry that names it lives in the directory,
// and without that a power loss shortly after could bring back the old file —
// or, for the muxed archive, leave the database pointing at a name the rename
// never made durable.
func ReplaceFile(tmp, path string) error {
	delay := replaceFileFirstDelay
	for attempt := 1; ; attempt++ {
		err := renameFile(tmp, path)
		if err == nil {
			syncDirectory(filepath.Dir(path))
			return nil
		}
		if attempt >= replaceFileAttempts || !isTransientReplaceError(err) {
			return err
		}
		replaceFileSleep(delay)
		if delay < replaceFileMaxDelay {
			delay *= 2
		}
	}
}

const (
	// replaceFileAttempts bounds the retries; with the pauses below the
	// worst case waits just over one second, short enough for a cookie
	// rollback the operator is watching.
	replaceFileAttempts   = 8
	replaceFileFirstDelay = 10 * time.Millisecond
	replaceFileMaxDelay   = 400 * time.Millisecond
)

// syncDir fsyncs a directory so the entries renamed into it are durable.
// Best-effort: a filesystem that refuses a directory fsync changes nothing
// the rename already did. A no-op on Windows, where a directory cannot be
// opened for an fsync.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	f.Close()
}

// Seams for the tests: the rename itself, the platform classifier, the pause
// and the directory sync. Production never reassigns them.
var (
	renameFile              = os.Rename
	isTransientReplaceError = transientReplaceError
	replaceFileSleep        = time.Sleep
	syncDirectory           = syncDir
)

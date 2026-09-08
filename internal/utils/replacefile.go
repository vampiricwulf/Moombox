package utils

import (
	"os"
	"time"
)

// ReplaceFile renames tmp over path — the last step of every atomic write in
// Moombox (cookies.txt, chat files, resume sidecars, config.toml). On Windows
// an antivirus or indexer briefly holds a freshly written file open and the
// replace is refused with ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION even
// though nothing is wrong with either file; those two are retried with a
// growing pause (about a second in total) before the last error is returned.
// Every other error is returned on the first attempt, and elsewhere than
// Windows this is exactly os.Rename.
func ReplaceFile(tmp, path string) error {
	delay := replaceFileFirstDelay
	for attempt := 1; ; attempt++ {
		err := renameFile(tmp, path)
		if err == nil || attempt >= replaceFileAttempts || !isTransientReplaceError(err) {
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

// Seams for the tests: the rename itself, the platform classifier and the
// pause. Production never reassigns them.
var (
	renameFile              = os.Rename
	isTransientReplaceError = transientReplaceError
	replaceFileSleep        = time.Sleep
)

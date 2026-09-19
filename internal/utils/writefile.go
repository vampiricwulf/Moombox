package utils

import (
	"fmt"
	"os"
	"path/filepath"
)

// syncFile is a test seam; production = (*os.File).Sync.
var syncFile = func(f *os.File) error { return f.Sync() }

// WriteFileAtomic writes data to path via a uniquely named temp file in the
// same directory, fsyncs it, chmods it to perm, and replaces path with it.
// It is the one home for a pattern three packages had drifted copies of.
//
// Two properties the drifted copies were missing, each of which cost a real
// failure mode:
//
//   - A UNIQUE temp name (os.CreateTemp, not path+".tmp"): two writers aiming
//     at the same target would otherwise interleave into one temp file and
//     rename a corrupt result into place. internal/cookies/cookie_files.go
//     documents this; internal/cipher/player_cache.go did not do it.
//   - fsync BEFORE the rename: without it a crash can journal the rename
//     while the data pages never reach disk, leaving a zero-length or torn
//     file that the next read silently trusts.
//
// The replace goes through ReplaceFile, so the Windows AV/indexer sharing
// window is retried rather than reported as a hard failure.
//
// Every failure path shares one deferred cleanup of the temp file instead of
// a per-branch os.Remove; done flips true only after ReplaceFile has moved
// the temp into place, so a successful call leaves nothing behind to remove.
//
// Callers that need more than this (the cookie writer's DACL tightening, the
// chat file's in-place header rewrite) keep their own writers; this helper is
// for the plain "replace this file's whole contents" case.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := syncFile(tmpFile); err != nil {
		tmpFile.Close()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := ReplaceFile(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	done = true
	return nil
}

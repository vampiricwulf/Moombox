package utils

import (
	"fmt"
	"os"
	"path/filepath"
)

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
// Callers that need more than this (the cookie writer's DACL tightening, the
// chat file's in-place header rewrite) keep their own writers; this helper is
// for the plain "replace this file's whole contents" case.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := ReplaceFile(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

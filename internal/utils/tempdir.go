package utils

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoveStaleTempEntries removes the entries of os.TempDir() — files or
// directories — whose names start with one of prefixes and that were last
// modified more than maxAge ago. It is the startup sweep for temp files a
// hard abort (OS kill, power loss, a panic elsewhere) left behind before
// their deferred removal ran; maxAge must exceed the longest operation that
// can own such an entry, so an in-flight one is never touched. An entry that
// cannot be read or removed is skipped.
func RemoveStaleTempEntries(maxAge time.Duration, prefixes ...string) (removed int, err error) {
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, ent := range entries {
		name := ent.Name()
		if !hasAnyPrefix(name, prefixes) {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(dir, name)) == nil {
			removed++
		}
	}
	return removed, nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

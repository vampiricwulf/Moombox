package cookies

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeAgedFile creates path with the given content and backdates its mtime
// by age, so the sweep's age guard can be exercised deterministically
// without a fake clock (sweepStaleCookieSnapshots's own test uses the same
// os.Chtimes technique — see autocookies_profile_rollback_test.go).
func writeAgedFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestSweepStaleCookieTempFiles is the junction-defect check: "the tmp file
// is gone" is satisfied even by a sweep that deletes everything in the
// directory, so the four SURVIVORS are the assertion, not just the one
// removal.
func TestSweepStaleCookieTempFiles(t *testing.T) {
	dir := t.TempDir()

	real := filepath.Join(dir, "cookies.txt")
	staleTemp := filepath.Join(dir, "cookies.txt.111.tmp")
	freshTemp := filepath.Join(dir, "cookies.txt.222.tmp")
	backup := filepath.Join(dir, "cookies.txt.bak")
	otherTemp := filepath.Join(dir, "other.txt.333.tmp")

	writeAgedFile(t, real, 0)
	writeAgedFile(t, staleTemp, 2*time.Hour)
	writeAgedFile(t, freshTemp, time.Minute)
	writeAgedFile(t, backup, 2*time.Hour)
	writeAgedFile(t, otherTemp, 2*time.Hour)

	sweepStaleCookieTempFiles(dir, "cookies.txt", time.Hour)

	if _, err := os.Stat(staleTemp); !os.IsNotExist(err) {
		t.Errorf("stale temp file survived sweep: stat err = %v, want IsNotExist", err)
	}
	for _, survivor := range []string{real, freshTemp, backup, otherTemp} {
		if _, err := os.Stat(survivor); err != nil {
			t.Errorf("survivor %s was removed by sweep: %v", survivor, err)
		}
	}
}

// TestSweepStaleCookieTempFilesMissingDir mirrors
// sweepStaleCookieSnapshots's own contract: a directory that can't be read
// (never written to yet) is not an error the sweep surfaces.
func TestSweepStaleCookieTempFilesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	sweepStaleCookieTempFiles(dir, "cookies.txt", time.Hour) // must not panic
}

// TestSweepCookieTempFilesOnceFiresOnlyOnce asserts the sweep runs once
// across two triggers: a stale temp file removed by the first call must
// survive an identical second call through the same *sync.Once, because the
// second call's body should never run at all.
func TestSweepCookieTempFilesOnceFiresOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "cookies.txt.111.tmp")
	writeAgedFile(t, tmpPath, 2*time.Hour)

	var once sync.Once
	sweepCookieTempFilesOnce(&once, dir, "cookies.txt", time.Hour)
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("first trigger: expected temp file removed, stat err = %v", err)
	}

	// Recreate the same stale file. If the second trigger actually ran the
	// sweep body again, this would be removed too; it must not be.
	writeAgedFile(t, tmpPath, 2*time.Hour)
	sweepCookieTempFilesOnce(&once, dir, "cookies.txt", time.Hour)
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("second trigger re-ran the sweep (Once did not gate it): %v", err)
	}
}

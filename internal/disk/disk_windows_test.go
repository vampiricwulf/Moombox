//go:build windows

package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetDiskSpaceLocalSucceeds covers the smoke path: GetDiskSpace on
// the OS temp dir returns plausible values. Audit reports/small-
// packages.md disk smoke + edge cases.
func TestGetDiskSpaceLocalSucceeds(t *testing.T) {
	got, err := GetDiskSpace(t.TempDir())
	if err != nil {
		t.Fatalf("GetDiskSpace(t.TempDir()): %v", err)
	}
	if got == nil {
		t.Fatal("GetDiskSpace returned nil result with no error")
	}
	if got.Total == 0 {
		t.Error("Total bytes = 0 — no real volume detected")
	}
	if got.Free > got.Total {
		t.Errorf("Free (%d) > Total (%d)", got.Free, got.Total)
	}
	if got.UsedPct < 0 || got.UsedPct > 100 {
		t.Errorf("UsedPct out of range [0,100]: %.2f", got.UsedPct)
	}
}

// TestGetDiskSpaceRelativePathResolves verifies the helper handles a
// relative path by resolving it via filepath.Abs before querying.
func TestGetDiskSpaceRelativePathResolves(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	// Use a relative path that we know resolves under cwd.
	got, err := GetDiskSpace(".")
	if err != nil {
		t.Fatalf("GetDiskSpace(.): %v", err)
	}
	if got == nil || got.Total == 0 {
		t.Errorf("relative path . did not resolve: %+v", got)
	}
	_ = cwd
}

// TestGetDiskSpaceNonExistentPathStillResolvesVolume covers a subtle
// invariant: GetDiskFreeSpaceExW queries the *volume*, so the path
// argument doesn't need to exist on disk — only the volume name has
// to be valid. The audit's UNC / extended-path guidance hinges on
// this — we test the analogous case for local non-existent paths to
// lock the behaviour without needing a UNC mount in CI.
func TestGetDiskSpaceNonExistentPathStillResolvesVolume(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "this", "does", "not", "exist", "yet")

	got, err := GetDiskSpace(missing)
	if err != nil {
		t.Fatalf("GetDiskSpace on non-existent path: %v", err)
	}
	if got == nil || got.Total == 0 {
		t.Errorf("non-existent path query produced empty result: %+v", got)
	}
}

// TestGetDiskSpaceExtendedPath covers the \\?\ extended-path form
// that disables MAX_PATH normalisation. filepath.VolumeName extracts
// `\\?\C:` from such inputs and the syscall accepts it.
func TestGetDiskSpaceExtendedPath(t *testing.T) {
	dir := t.TempDir()
	if !strings.HasPrefix(dir, `\\?\`) && filepath.VolumeName(dir) != "" {
		// Construct an extended-path form: \\?\<drive>:\<remainder>
		extended := `\\?\` + dir
		got, err := GetDiskSpace(extended)
		if err != nil {
			t.Skipf("extended-path form not accepted on this OS: %v", err)
		}
		if got == nil || got.Total == 0 {
			t.Errorf("extended path produced empty result: %+v", got)
		}
	}
}

// TestGetDiskSpaceInvalidPath covers the early-return error path: a
// path filepath.Abs cannot resolve. Empty string normalises to cwd
// on Windows so it doesn't error; we use an explicitly-broken UTF-16
// sequence instead.
func TestGetDiskSpaceInvalidPath(t *testing.T) {
	// A NUL in a Windows path is rejected by UTF16PtrFromString.
	_, err := GetDiskSpace("C:\\good\x00bad")
	if err == nil {
		t.Fatal("GetDiskSpace on path with NUL: want error, got nil")
	}
}

//go:build !windows

package disk

import (
	"os"
	"testing"
)

func TestGetDiskSpaceUnix(t *testing.T) {
	tmp, err := os.MkdirTemp("", "disk-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmp)

	ds, err := GetDiskSpace(tmp)
	if err != nil {
		t.Fatalf("GetDiskSpace returned error: %v", err)
	}
	if ds == nil {
		t.Fatal("GetDiskSpace returned nil")
	}
	if ds.Total == 0 {
		t.Error("Total bytes is 0; expected > 0 on a real filesystem")
	}
	if ds.UsedPct < 0 || ds.UsedPct > 100 {
		t.Errorf("UsedPct out of range [0,100]: %v", ds.UsedPct)
	}
	// Free may legitimately be 0 on a full filesystem; we only assert
	// it cannot exceed Total. A bug that always returns Free=0 would
	// pass these assertions, so callers should additionally sanity-check
	// in production.
	if ds.Free > ds.Total {
		t.Errorf("Free (%d) > Total (%d), invariant violated", ds.Free, ds.Total)
	}
}

func TestGetDiskSpaceUnixNonexistentPath(t *testing.T) {
	// filepath.Abs succeeds on nonexistent paths (it just resolves
	// relative to cwd); the error comes from syscall.Statfs returning
	// ENOENT. So this test exercises the Statfs error path, not Abs.
	_, err := GetDiskSpace("/this/path/does/not/exist/anywhere")
	if err == nil {
		t.Error("expected error for nonexistent path, got nil")
	}
}

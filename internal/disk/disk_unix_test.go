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
	// Mirrors the Windows invariant (TestGetDiskSpaceNonExistentPathStillResolvesVolume):
	// the path argument doesn't need to exist on disk — Statfs walks up to the
	// nearest existing ancestor, so a not-yet-created output directory still
	// reports the volume it would land on instead of killing disk monitoring.
	ds, err := GetDiskSpace("/this/path/does/not/exist/anywhere")
	if err != nil {
		t.Fatalf("expected nearest-ancestor fallback, got error: %v", err)
	}
	if ds.Total == 0 {
		t.Error("Total bytes is 0; expected > 0 from ancestor volume")
	}
	if ds.Free > ds.Total {
		t.Errorf("Free (%d) > Total (%d), invariant violated", ds.Free, ds.Total)
	}
}

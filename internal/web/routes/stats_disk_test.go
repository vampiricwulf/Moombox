package routes

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

// TestDiskStatusStats pins the one conversion both dashboards go through
// (the /api/stats handler and cmd/moombox's OnGetStats closure): no reading
// yet means no reading out, and every field crosses unchanged.
func TestDiskStatusStats(t *testing.T) {
	var none *DiskStatus
	if got := none.Stats(); got != nil {
		t.Errorf("a nil reading must convert to nil, got %+v", *got)
	}
	ds := &DiskStatus{Free: 250 << 30, Total: 1000 << 30, UsedPct: 75, WarnLevel: "warn"}
	want := stats.Disk{Free: 250 << 30, Total: 1000 << 30, UsedPct: 75, WarnLevel: "warn"}
	got := ds.Stats()
	if got == nil {
		t.Fatal("a reading must convert to a reading, got nil")
	}
	if *got != want {
		t.Errorf("Stats() = %+v, want %+v", *got, want)
	}
}

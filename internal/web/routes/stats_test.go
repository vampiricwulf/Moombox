package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// resetSharedDiskStatus clears the package atomic between cases so the
// assertion-on-defaults paths aren't polluted by state from a prior
// test that seeded a value.
func resetSharedDiskStatus(t *testing.T) {
	t.Helper()
	SharedDiskStatus.Store(nil)
	t.Cleanup(func() { SharedDiskStatus.Store(nil) })
}

func newStatsFixture(t *testing.T) (chi.Router, *database.Database) {
	t.Helper()
	resetSharedDiskStatus(t)

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	r := chi.NewRouter()
	StatsRoutes(r, &StatsRouteDeps{DB: db})
	return r, db
}

// --- GET /api/stats ---

func TestStatsEmptyDatabase(t *testing.T) {
	r, _ := newStatsFixture(t)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stats: want 200, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	disk, ok := resp["disk"].(map[string]any)
	if !ok {
		t.Fatalf("disk: want object, got %T", resp["disk"])
	}
	// No SharedDiskStatus → defaults
	if disk["warnLevel"] != "ok" {
		t.Errorf("disk.warnLevel: want ok by default, got %v", disk["warnLevel"])
	}
	if disk["free"] != float64(0) {
		t.Errorf("disk.free: want 0 with no shared status, got %v", disk["free"])
	}

	storage, _ := resp["storage"].(map[string]any)
	if storage["jobCount"] != float64(0) {
		t.Errorf("storage.jobCount: want 0 on empty DB, got %v", storage["jobCount"])
	}
	if storage["totalSize"] != float64(0) {
		t.Errorf("storage.totalSize: want 0 on empty DB, got %v", storage["totalSize"])
	}
}

func TestStatsSurfacesSharedDiskStatus(t *testing.T) {
	// SharedDiskStatus is updated by main.go's stats ticker; the route
	// just reads + serialises it. This test confirms field round-trip
	// (free, total, usedPct, warnLevel) and the override of the empty
	// defaults.
	r, _ := newStatsFixture(t)

	SharedDiskStatus.Store(&DiskStatus{
		Free:      1_000_000,
		Total:     10_000_000,
		UsedPct:   90.0,
		WarnLevel: "warn",
	})

	req := httptest.NewRequest("GET", "/api/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stats: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	disk := resp["disk"].(map[string]any)
	if disk["free"] != float64(1_000_000) {
		t.Errorf("disk.free: want 1_000_000, got %v", disk["free"])
	}
	if disk["total"] != float64(10_000_000) {
		t.Errorf("disk.total: want 10_000_000, got %v", disk["total"])
	}
	if disk["usedPct"] != float64(90.0) {
		t.Errorf("disk.usedPct: want 90, got %v", disk["usedPct"])
	}
	if disk["warnLevel"] != "warn" {
		t.Errorf("disk.warnLevel: want warn, got %v", disk["warnLevel"])
	}
}

func TestStatsAggregatesByPlatformAndStatus(t *testing.T) {
	// Seed enough jobs to exercise the per-platform + per-status
	// aggregations. The route trusts GetJobStats; we trust the DB
	// layer is tested separately. This test just confirms the field
	// shape exposed to the frontend matches the documented contract.
	r, db := newStatsFixture(t)

	// AddJob doesn't insert file_size (the column is bumped via UpdateJobFields
	// later in the production flow), so the test does the same: insert, then
	// patch in the size for the aggregation to see it.
	type seed struct {
		id, plat string
		status   database.JobStatus
		size     int64
	}
	seeds := []seed{
		{"yt_a", "youtube", database.StatusFinished, 100},
		{"yt_b", "youtube", database.StatusFinished, 200},
		{"tw_c", "twitch", database.StatusFinished, 50},
		{"yt_d", "youtube", database.StatusError, 10},
	}
	for _, s := range seeds {
		j := &database.Job{ID: s.id, VideoID: s.id, URL: "u", Platform: s.plat, Status: s.status}
		if _, err := db.AddJob(j); err != nil {
			t.Fatalf("AddJob %s: %v", s.id, err)
		}
		// file_size is patched via UpdateJobFields, mirroring the worker's
		// post-mux update path. Without this the GetJobStats SUM(file_size)
		// returns 0 even though the rows are present.
		if got := db.UpdateJobFields(s.id, map[string]any{"file_size": s.size}); got == nil {
			t.Fatalf("UpdateJobFields %s: nil result", s.id)
		}
	}

	req := httptest.NewRequest("GET", "/api/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stats: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)

	storage := resp["storage"].(map[string]any)
	byPlatform := storage["byPlatform"].(map[string]any)
	if byPlatform["youtube"] != float64(310) {
		t.Errorf("byPlatform.youtube: want 310 (100+200+10), got %v", byPlatform["youtube"])
	}
	if byPlatform["twitch"] != float64(50) {
		t.Errorf("byPlatform.twitch: want 50, got %v", byPlatform["twitch"])
	}

	byStatus := storage["byStatus"].(map[string]any)
	if byStatus["finished"] != float64(350) {
		t.Errorf("byStatus.finished: want 350 (100+200+50), got %v", byStatus["finished"])
	}
	if byStatus["error"] != float64(10) {
		t.Errorf("byStatus.error: want 10, got %v", byStatus["error"])
	}

	if storage["jobCount"] != float64(4) {
		t.Errorf("jobCount: want 4, got %v", storage["jobCount"])
	}

	activity := resp["activity"].(map[string]any)
	if activity["totalFinished"] != float64(3) {
		t.Errorf("activity.totalFinished: want 3, got %v", activity["totalFinished"])
	}
}

// --- ComputeWarnLevel unit ---

func TestComputeWarnLevel(t *testing.T) {
	cfg := &config.MoomboxConfig{}
	cfg.Disk.WarnPercent = 80
	cfg.Disk.CriticalPercent = 95

	tests := []struct {
		usedPct float64
		want    string
	}{
		{0.0, "ok"},
		{50.0, "ok"},
		{79.999, "ok"},
		{80.0, "warn"}, // boundary — equal to threshold trips
		{90.0, "warn"},
		{94.999, "warn"},
		{95.0, "critical"}, // boundary
		{99.9, "critical"},
		{100.0, "critical"},
	}
	for _, tt := range tests {
		got := ComputeWarnLevel(tt.usedPct, cfg)
		if got != tt.want {
			t.Errorf("ComputeWarnLevel(%v): want %q, got %q", tt.usedPct, tt.want, got)
		}
	}
}

func TestComputeWarnLevelZeroThresholdsDisabled(t *testing.T) {
	// Threshold == 0 disables that level (matches the `> 0` guard in
	// the implementation). Use case: a user who only wants critical
	// alerts can set warn=0 and only get the red banner.
	cfg := &config.MoomboxConfig{}
	cfg.Disk.WarnPercent = 0 // disabled
	cfg.Disk.CriticalPercent = 95

	if got := ComputeWarnLevel(80.0, cfg); got != "ok" {
		t.Errorf("WarnPercent=0 should disable warn; at 80%%: want ok, got %q", got)
	}
	if got := ComputeWarnLevel(95.0, cfg); got != "critical" {
		t.Errorf("CriticalPercent=95 at 95%%: want critical, got %q", got)
	}
}

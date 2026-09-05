package routes

import (
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// DiskStatus holds the latest disk space reading and warn level.
// Updated periodically by the memory stats ticker in main.go.
type DiskStatus struct {
	Free      uint64  `json:"free"`
	Total     uint64  `json:"total"`
	UsedPct   float64 `json:"usedPct"`
	WarnLevel string  `json:"warnLevel"` // "ok", "warn", "critical"
}

// SharedDiskStatus is the package-level atomic pointer updated by main.go
// and read by both the status route and stats route.
var SharedDiskStatus atomic.Pointer[DiskStatus]

// ComputeWarnLevel returns "ok", "warn", or "critical" for a given usage percent.
func ComputeWarnLevel(usedPct float64, cfg *config.MoomboxConfig) string {
	if cfg.Disk.CriticalPercent > 0 && usedPct >= float64(cfg.Disk.CriticalPercent) {
		return "critical"
	}
	if cfg.Disk.WarnPercent > 0 && usedPct >= float64(cfg.Disk.WarnPercent) {
		return "warn"
	}
	return "ok"
}

// UpdateDiskStatus queries disk space and stores the result in
// SharedDiskStatus. The Store's Read serialises threshold-field access with
// concurrent config Updates. Returns the DiskStatus, or nil on disk-read
// error.
func UpdateDiskStatus(outputDir string, store *config.Store) *DiskStatus {
	ds, err := disk.GetDiskSpace(outputDir)
	if err != nil {
		return nil
	}

	var warnLevel string
	store.Read(func(c *config.MoomboxConfig) {
		warnLevel = ComputeWarnLevel(ds.UsedPct, c)
	})

	status := &DiskStatus{
		Free:      ds.Free,
		Total:     ds.Total,
		UsedPct:   ds.UsedPct,
		WarnLevel: warnLevel,
	}
	SharedDiskStatus.Store(status)
	return status
}

// StatsRouteDeps holds dependencies for the stats route.
type StatsRouteDeps struct {
	DB     *database.Database
	Worker *worker.DownloadWorker // optional; surfaces Twitch hint cache hit/miss
}

// StatsRoutes registers the /api/stats endpoint.
func StatsRoutes(r chi.Router, deps *StatsRouteDeps) {
	r.Get("/api/stats", func(rw http.ResponseWriter, req *http.Request) {
		var js *database.JobStats
		if got, err := deps.DB.GetJobStats(); err == nil {
			js = got
		}
		var diskReading *stats.Disk
		if ds := SharedDiskStatus.Load(); ds != nil {
			diskReading = &stats.Disk{Free: ds.Free, Total: ds.Total, UsedPct: ds.UsedPct, WarnLevel: ds.WarnLevel}
		}
		snap := stats.Build(js, diskReading)
		warn := snap.Disk.WarnLevel
		if warn == "" {
			warn = "ok" // the route's historical default when no reading exists yet
		}
		diskResp := map[string]any{
			"free": snap.Disk.Free, "total": snap.Disk.Total, "usedPct": snap.Disk.UsedPct, "warnLevel": warn,
		}
		storageResp := map[string]any{
			"totalSize":  snap.TotalSize,
			"byPlatform": snap.SizeByPlatform,
			"byStatus":   snap.SizeByStatus,
			"jobCount":   snap.JobCount,
		}
		activityResp := map[string]any{
			"totalFinished":     snap.TotalFinished,
			"totalDuration":     snap.TotalDuration,
			"totalChatMessages": snap.TotalChatMessages,
			"activeDownloads":   snap.ActiveDownloads,
			"activeMuxing":      snap.ActiveMuxing,
			"byPlatform":        snap.CountByPlatform,
		}

		resp := map[string]any{
			"disk":     diskResp,
			"storage":  storageResp,
			"activity": activityResp,
		}

		// Twitch hint-cache observability — surfaces whether the monitor →
		// processor pass-through is firing in production. A drop to zero hits
		// after a refactor would otherwise be invisible.
		if deps.Worker != nil {
			hs := deps.Worker.TwitchHintStats()
			resp["twitchHints"] = map[string]uint64{
				"hits":   hs.Hits,
				"misses": hs.Misses,
			}
		}

		jsonResponse(rw, resp)
	})
}

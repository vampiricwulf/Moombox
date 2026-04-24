package routes

import (
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
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
	DB  *database.Database
	Cfg *config.MoomboxConfig
}

// StatsRoutes registers the /api/stats endpoint.
func StatsRoutes(r chi.Router, deps *StatsRouteDeps) {
	r.Get("/api/stats", func(rw http.ResponseWriter, req *http.Request) {
		// Disk status from shared atomic
		diskResp := map[string]any{
			"free": 0, "total": 0, "usedPct": 0.0, "warnLevel": "ok",
		}
		if ds := SharedDiskStatus.Load(); ds != nil {
			diskResp["free"] = ds.Free
			diskResp["total"] = ds.Total
			diskResp["usedPct"] = ds.UsedPct
			diskResp["warnLevel"] = ds.WarnLevel
		}

		// Job stats from database
		storageResp := map[string]any{
			"totalSize":  int64(0),
			"byPlatform": map[string]int64{"youtube": 0, "twitch": 0},
			"byStatus":   map[string]int64{"finished": 0, "error": 0, "cancelled": 0},
			"jobCount":   0,
		}
		activityResp := map[string]any{
			"totalFinished":     0,
			"totalDuration":     int64(0),
			"totalChatMessages": int64(0),
			"activeDownloads":   0,
			"activeMuxing":      0,
			"byPlatform":        map[string]int{"youtube": 0, "twitch": 0},
		}

		if stats, err := deps.DB.GetJobStats(); err == nil {
			storageResp["totalSize"] = stats.FinishedSize + stats.ErrorSize + stats.CancelledSize
			storageResp["byPlatform"] = map[string]int64{
				"youtube": stats.YouTubeSize,
				"twitch":  stats.TwitchSize,
			}
			storageResp["byStatus"] = map[string]int64{
				"finished":  stats.FinishedSize,
				"error":     stats.ErrorSize,
				"cancelled": stats.CancelledSize,
			}
			storageResp["jobCount"] = stats.FinishedCount + stats.ErrorCount + stats.CancelledCount +
				stats.ActiveCount + stats.MuxingCount

			activityResp["totalFinished"] = stats.FinishedCount
			activityResp["totalDuration"] = stats.TotalDuration
			activityResp["totalChatMessages"] = stats.TotalChatMessages
			activityResp["activeDownloads"] = stats.ActiveCount
			activityResp["activeMuxing"] = stats.MuxingCount
			activityResp["byPlatform"] = map[string]int{
				"youtube": stats.YouTubeCount,
				"twitch":  stats.TwitchCount,
			}
		}

		jsonResponse(rw, map[string]any{
			"disk":     diskResp,
			"storage":  storageResp,
			"activity": activityResp,
		})
	})
}

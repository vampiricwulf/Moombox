package routes

import (
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// ChannelRemovalRoutesDeps holds the channel-removal routes' dependencies.
type ChannelRemovalRoutesDeps struct {
	DB    *database.Database
	Store *config.Store
	// OnChannelChange re-seeds the monitors once the channel is gone
	// (kickMonitors) — the sweep it runs prunes the channel's feed history.
	OnChannelChange func()
	Logger          interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// ChannelRemovalRoutes registers the removal of a monitored channel and the
// counts its confirmation shows (W25-09, owner decision). Removing a channel
// asks what to do with its jobs: keep them all — the default, and what every
// other way a channel leaves the config does — or delete the pending ones
// (worker.DeletePendingChannelJobs: Queued, Upcoming and COOKIES?, less any
// whose staging holds footage). The departure prune that follows deletes the
// channel's feed history only (monitor.BackfillWorker.CancelAndPrune).
func ChannelRemovalRoutes(r chi.Router, deps *ChannelRemovalRoutesDeps) {
	stagingDir := func() string {
		var dir string
		deps.Store.Read(func(c *config.MoomboxConfig) { dir = c.Paths.EffectiveStagingDir() })
		return dir
	}

	// GET /api/config/channels/{id}/removal — what removing the channel
	// would do to its jobs: {total, pending, footage: [{id, title, status}],
	// active}. Answers for any ID, configured or not.
	r.Get("/api/config/channels/{id}/removal", func(rw http.ResponseWriter, req *http.Request) {
		sum, err := worker.SummarizeChannelRemoval(deps.DB, stagingDir(), pathParam(req, "id"))
		if err != nil {
			jsonError(rw, "failed to read the channel's jobs", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, sum)
	})

	// DELETE /api/config/channels/{id}?jobs=keep|delete — remove the
	// channel. jobs=delete also deletes its pending jobs; absent, or keep,
	// keeps every job. The choice is checked before anything changes.
	r.Delete("/api/config/channels/{id}", func(rw http.ResponseWriter, req *http.Request) {
		channelID := pathParam(req, "id")
		var deletePending bool
		switch req.URL.Query().Get("jobs") {
		case "", "keep":
		case "delete":
			deletePending = true
		default:
			jsonError(rw, `jobs must be "keep" or "delete"`, http.StatusBadRequest)
			return
		}

		// Copy-on-write: Store.Snapshot() readers share the previous backing
		// array, and compacting elements inside it in place raced them.
		mu := deps.Store.RWMutex()
		cfg := deps.Store.Config()
		mu.Lock()
		oldChannels := cfg.Channels
		idx := slices.IndexFunc(cfg.Channels, func(ch config.ChannelConfig) bool { return ch.ID == channelID })
		if idx < 0 {
			mu.Unlock()
			jsonError(rw, "channel not found", http.StatusNotFound)
			return
		}
		cfg.Channels = slices.Delete(slices.Clone(cfg.Channels), idx, idx+1)

		// Persist to disk; restore on save failure so in-memory and disk stay in sync.
		if err := deps.Store.SaveLocked(); err != nil {
			cfg.Channels = oldChannels
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}
		mu.Unlock()

		// The delete runs once the channel is out of the config: from then
		// on no monitor cycle creates a job for it (createYouTubeJob asks
		// the live config), so nothing recreates what this deletes.
		resp := map[string]any{"success": true}
		var deleteErr error
		if deletePending {
			n, kept, err := worker.DeletePendingChannelJobs(deps.DB, stagingDir(), channelID)
			if err != nil {
				deleteErr = err
				deps.Logger.Error("channel removed, but deleting its pending jobs failed; they are kept",
					"channel", channelID, "err", err)
			} else {
				deps.Logger.Info("channel removed with its pending jobs",
					"channel", channelID, "jobsDeleted", n, "footageKept", len(kept))
				resp["jobsDeleted"] = n
				resp["footageKept"] = kept
			}
		} else {
			deps.Logger.Info("channel removed; its jobs are kept", "channel", channelID)
		}

		if deps.OnChannelChange != nil {
			deps.OnChannelChange()
		}
		if deleteErr != nil {
			jsonError(rw, "channel removed, but deleting its pending jobs failed — they are kept", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, resp)
	})
}

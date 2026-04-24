package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// youtubeThumbnailURL returns the maxres thumbnail URL for a YouTube video.
// Centralised here so a future host/quality change is one place, not N
// (per audit reports/cmd-moombox.md D-7).
func youtubeThumbnailURL(videoID string) string {
	return fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", videoID)
}

// resolveOutputDir returns the channel-specific output directory if set,
// otherwise falls back to the global default under cfgMu.RLock (per audit
// reports/cmd-moombox.md D-5).
func resolveOutputDir(ch *config.ChannelConfig, cfg *config.MoomboxConfig, cfgMu *sync.RWMutex) string {
	if ch.OutputDirectory != "" {
		return ch.OutputDirectory
	}
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.Paths.OutputDirectory
}

// nopLogger is a no-op logger for CLI commands where full logging isn't needed.
type nopLogger struct{}

func (n *nopLogger) Debug(_ string, _ ...any) {}
func (n *nopLogger) Info(_ string, _ ...any)  {}
func (n *nopLogger) Warn(_ string, _ ...any)  {}
func (n *nopLogger) Error(_ string, _ ...any) {}

func getAllJobsSafe(db *database.Database) []*database.Job {
	jobs, err := db.GetAllJobs()
	if err != nil {
		return []*database.Job{}
	}
	return jobs
}

// checkAndBroadcastUpdate checks for a new release and broadcasts the result.
func checkAndBroadcastUpdate(
	ctx context.Context,
	upd *updater.Updater,
	wsHub *web.WebSocketHub,
	notifyMgr *notifications.Manager,
	tuiCh chan<- tui.UpdateStatusMsg,
	log *logger.Logger,
) {
	release, err := upd.CheckForUpdate(ctx)
	if err != nil {
		log.Warn("[Updater] Check failed", slog.String("error", err.Error()))
		return
	}
	if release == nil {
		return // already up to date
	}

	routes.SharedUpdateInfo.Store(release)
	wsHub.Broadcast("update_available", release)

	select {
	case tuiCh <- tui.UpdateStatusMsg{
		Version:      release.Version,
		TagName:      release.TagName,
		ReleaseNotes: release.ReleaseNotes,
	}:
	default:
	}

	if notifyMgr.HasTargets() {
		notifyMgr.Send("Update Available",
			"Moombox "+release.TagName+" is available",
			notifications.TypeInfo, nil,
			notifications.SendOptions{Event: "update_available"},
		)
	}
}

func extractWSIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// filterJobsByAge removes finished jobs older than hide_finished_age_days from
// the slice. When ageDays is 0, the cutoff is "now" so all finished jobs are
// hidden (any finished job's updated_at will be before the current moment).
// Returns the original slice unchanged when no jobs are filtered out.
func filterJobsByAge(jobs []*database.Job, cfg *config.MoomboxConfig, cfgMu *sync.RWMutex) []*database.Job {
	cfgMu.RLock()
	ageDays := int(cfg.Monitors.HideFinishedAgeDays.Value)
	cfgMu.RUnlock()
	cutoff := time.Now().AddDate(0, 0, -ageDays)
	anyFiltered := false
	for _, j := range jobs {
		if j.Status == database.StatusFinished && j.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, j.UpdatedAt); err == nil {
				if t.Before(cutoff) {
					anyFiltered = true
					break
				}
			}
		}
	}
	if !anyFiltered {
		return jobs
	}
	filtered := make([]*database.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.Status == database.StatusFinished && j.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, j.UpdatedAt); err == nil {
				if t.Before(cutoff) {
					continue
				}
			}
		}
		filtered = append(filtered, j)
	}
	return filtered
}

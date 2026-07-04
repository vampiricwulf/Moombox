package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	isatty "github.com/mattn/go-isatty"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// waitForKeypress waits for a keypress before exiting (prevents .exe window
// from vanishing on Windows when the process hit a startup error). Matches
// the TS waitForKeypress() in index.ts — only blocks on a TTY so scripted
// runs / CI aren't held up.
func waitForKeypress() {
	fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	reader.ReadByte()
}

// youtubeThumbnailURL returns the maxres thumbnail URL for a YouTube video.
// Centralised here so a future host/quality change is one place, not N
// (per audit reports/cmd-moombox.md D-7).
func youtubeThumbnailURL(videoID string) string {
	return fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", videoID)
}

// resolveOutputDir returns the channel-specific output directory if set,
// otherwise falls back to the global default under store.Read (per audit
// reports/cmd-moombox.md D-5).
func resolveOutputDir(ch *config.ChannelConfig, store *config.Store) string {
	if ch.OutputDirectory != "" {
		return ch.OutputDirectory
	}
	var dir string
	store.Read(func(c *config.MoomboxConfig) {
		dir = c.Paths.OutputDirectory
	})
	return dir
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
//
// configStore is re-read AFTER the network check so a "Skip this version" /
// auto-check disable that landed while the check was in flight still takes
// effect. lastNotifiedTag (single check-goroutine state) suppresses the
// daily re-ping: one notification per NEW version, not one per 24h tick
// while the same update sits pending.
func checkAndBroadcastUpdate(
	ctx context.Context,
	upd *updater.Updater,
	wsHub *web.WebSocketHub,
	notifyMgr *notifications.Manager,
	tuiCh chan<- tui.UpdateStatusMsg,
	log *logger.Logger,
	configStore *config.Store,
	lastNotifiedTag *string,
) {
	release, err := upd.CheckForUpdate(ctx)
	if err != nil {
		log.Warn("[Updater] Check failed", slog.String("error", err.Error()))
		return
	}
	if release == nil {
		return // already up to date
	}

	var enabled bool
	var skipped string
	configStore.Read(func(c *config.MoomboxConfig) {
		enabled = c.Updates.AutoCheckUpdates
		skipped = c.Updates.SkippedVersion
	})
	if !enabled {
		return // disabled while the check was in flight
	}

	if release.TagName == skipped {
		// Skipped version: the automatic path surfaces NOTHING — storing it
		// would leak straight back into the web badge via the /api/status
		// poll on the next page load, defeating the skip within a day. The
		// operator's explicit override is the manual "Check for updates"
		// button (POST /api/update/check), which stores unconditionally.
		log.Debug("[Updater] Suppressing user-skipped version", slog.String("tag", release.TagName))
		return
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

	if *lastNotifiedTag == release.TagName {
		return // already pinged the operator about this exact version
	}
	if notifyMgr.HasTargets() {
		// Link the embed title to the GitHub release page and carry the
		// version pair — this was the only lifecycle notification with no
		// clickable link and no detail fields.
		notifyMgr.Send("Update Available",
			"Moombox "+release.TagName+" is available",
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Current Version", Value: "v" + upd.CurrentVersion(), Inline: true},
				{Name: "New Version", Value: release.TagName, Inline: true},
			},
			notifications.SendOptions{Event: "update_available", URL: release.ReleaseURL},
		)
	}
	*lastNotifiedTag = release.TagName
}

func extractWSIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// filterJobsByAge removes finished jobs older than hide_finished_age_days from
// the slice, reading the threshold from the config store. Convenience wrapper
// around filterJobsByAgeThreshold for callers that don't already hold the value.
func filterJobsByAge(jobs []*database.Job, store *config.Store) []*database.Job {
	var hideAgeDays float64
	store.Read(func(c *config.MoomboxConfig) {
		hideAgeDays = c.Monitors.HideFinishedAgeDays.Value
	})
	return filterJobsByAgeThreshold(jobs, hideAgeDays)
}

// filterJobsByAgeThreshold removes finished jobs older than hideAgeDays from the
// slice using the same float-precision, strict-greater-than semantics as the
// REST archived filter (internal/web/routes/jobs.go filterJobsByAge) and the
// Web UI (_evaluateArchiveBoundary in app.js), so all three classify a given
// job identically:
//   - hideAgeDays < 0 : never archive (returns the slice unchanged)
//   - hideAgeDays == 0: archive every finished job whose updated_at is strictly
//     in the past (cutoff == now)
//   - hideAgeDays > 0 : archive finished jobs whose age exceeds the threshold
//
// Callers that broadcast hideFinishedAgeDays alongside the filtered list MUST
// pass the same captured value here rather than re-reading the store, so the
// config_update and jobs_update payloads can never disagree. Returns the
// original slice unchanged when no jobs are filtered out.
func filterJobsByAgeThreshold(jobs []*database.Job, hideAgeDays float64) []*database.Job {
	// Negative means never hide finished jobs — they all stay active.
	if hideAgeDays < 0 {
		return jobs
	}
	cutoff := time.Now().Add(-time.Duration(hideAgeDays*24) * time.Hour)
	anyFiltered := false
	for _, j := range jobs {
		if jobArchivedAt(j, cutoff) {
			anyFiltered = true
			break
		}
	}
	if !anyFiltered {
		return jobs
	}
	filtered := make([]*database.Job, 0, len(jobs))
	for _, j := range jobs {
		if jobArchivedAt(j, cutoff) {
			continue
		}
		filtered = append(filtered, j)
	}
	return filtered
}

// jobArchivedAt is the single-job archive classification shared by
// filterJobsByAgeThreshold and the WS job_update broadcast gate. Keeping one
// predicate prevents the two from drifting (an earlier int-truncated copy of
// this logic suppressed broadcasts for jobs the list filter still showed).
func jobArchivedAt(j *database.Job, cutoff time.Time) bool {
	if j.Status != database.StatusFinished || j.UpdatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return false
	}
	return t.Before(cutoff)
}

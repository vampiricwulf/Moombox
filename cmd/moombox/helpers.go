package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	isatty "github.com/mattn/go-isatty"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// mergeChannelHealth combines the feed and DECAPI health lists (both track
// the same YouTube channel set) into one row per channel, keeping the entry
// with the more recent last-check so the dashboard shows a single, freshest
// status per channel.
func mergeChannelHealth(lists ...[]monitor.ChannelHealth) []monitor.ChannelHealth {
	byID := make(map[string]monitor.ChannelHealth)
	for _, list := range lists {
		for _, h := range list {
			if prev, ok := byID[h.ChannelID]; !ok || h.LastCheckedAt >= prev.LastCheckedAt {
				byID[h.ChannelID] = h
			}
		}
	}
	out := make([]monitor.ChannelHealth, 0, len(byID))
	for _, h := range byID {
		out = append(out, h)
	}
	return out
}

// effectiveLogLevel picks the level the logger starts at: the -log-level
// override when one was given, otherwise the configured level. Deliberately
// a pure function of the two strings so the "the override never reaches the
// config struct" rule can be asserted without a boot (CORE-10).
func effectiveLogLevel(configured, override string) string {
	if override != "" {
		return override
	}
	return configured
}

// waitForKeypress waits for a keypress before exiting (prevents .exe window
// from vanishing on Windows when the process hit a startup error). Matches
// the TS waitForKeypress() in index.ts — only blocks on a TTY so scripted
// runs / CI aren't held up.
func waitForKeypress() {
	// The prompt comes after the TTY check: under Docker or systemd it would
	// be a false line in the very log an operator reads after the crash.
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return
	}
	fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
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

// loadConfig loads the configuration for the -config flag's value and returns
// it with the path every later save must target. An empty flagPath runs
// config.Load's search (cwd, ./config/, ~/.config/moombox/); a named one is
// the only file considered.
func loadConfig(flagPath string) (*config.MoomboxConfig, string, error) {
	cfg, err := config.Load(flagPath)
	if err != nil {
		return nil, "", err
	}
	return cfg, storePathFor(flagPath, cfg), nil
}

// storePathFor is the file every later save must target: the one config.Load
// actually read, and — only when nothing was found anywhere — the path that was
// asked for (the -config flag), else <cwd>/config.toml. So a config found in
// ./config/ is written back to ./config/ instead of being forked into a fresh
// ./config.toml that shadows it on the next boot, while a -config path that
// does not exist yet is still CREATED where it was named.
func storePathFor(flagPath string, cfg *config.MoomboxConfig) string {
	if cfg.LoadedFrom != "" {
		return cfg.LoadedFrom
	}
	if flagPath != "" {
		return flagPath
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to get working directory: %v\n", err)
	}
	return filepath.Join(cwd, "config.toml")
}

// updateCheckTiming is runUpdateCheckLoop's schedule.
type updateCheckTiming struct {
	initialDelay time.Duration // before the boot check, so it does not slow startup
	period       time.Duration // between scheduled checks
	poll         time.Duration // how often the toggle is re-read for a false→true flip
}

// runUpdateCheckLoop runs the auto-update check: once shortly after boot, then
// every period, each only while enabled() — re-read every time, so disabling
// the toggle in either settings UI stops an armed schedule without a restart.
// It also re-reads the toggle every poll and checks at once when it has
// turned ON: enabling it at runtime used to do nothing until the next daily
// tick, up to a day later. Polling the store rather than wiring a callback
// covers every writer of the flag — the web Settings save
// (config_routes.go) and the TUI's — and any added later. (The dismiss route
// is not one: it records SkippedVersion and leaves the toggle alone.)
// Returns when ctx ends.
func runUpdateCheckLoop(ctx context.Context, enabled func() bool, check func(), t updateCheckTiming) {
	select {
	case <-time.After(t.initialDelay):
	case <-ctx.Done():
		return
	}
	wasEnabled := enabled()
	if wasEnabled {
		check()
	}
	ticker := time.NewTicker(t.period)
	defer ticker.Stop()
	poll := time.NewTicker(t.poll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if wasEnabled = enabled(); wasEnabled {
				check()
			}
		case <-poll.C:
			on := enabled()
			if on && !wasEnabled {
				check()
			}
			wasEnabled = on
		}
	}
}

// announceUpdateCleared tells both UIs that the pending release tagged tag is
// withdrawn — skipped, or found to be no newer than the running version — so
// they drop the badge their own copy of it lights.
//
// The TAG travels with the clear, as an UpdateStatusMsg with an empty Version
// for the TUI and an update_cleared event for the dashboards. Each holds its
// own copy of the pending release and drops it only when the clear names the
// release it is showing — otherwise a clear racing a newly-found release would
// blank the badge for an update that is still there. This is the ONLY
// producer of either, so the tag is always set.
func announceUpdateCleared(wsHub *web.WebSocketHub, tuiCh chan<- tui.UpdateStatusMsg, tag string) {
	if wsHub != nil {
		wsHub.Broadcast("update_cleared", map[string]string{"tagName": tag})
	}
	select {
	case tuiCh <- tui.UpdateStatusMsg{TagName: tag}:
	default:
	}
}

// checkForUpdate is (*updater.Updater).CheckForUpdate, a seam for the tests of
// the periodic and TUI checks, which need a check to answer without GitHub.
var checkForUpdate = (*updater.Updater).CheckForUpdate

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
	notifyMgr notifications.Notifier,
	tuiCh chan<- tui.UpdateStatusMsg,
	log *logger.Logger,
	configStore *config.Store,
	lastNotifiedTag *string,
) {
	seen := routes.SharedUpdateInfo.Load() // what an up-to-date answer may withdraw
	release, err := checkForUpdate(upd, ctx)
	if err != nil {
		// A check cut short by shutdown is not a failure worth a warning.
		if ctx.Err() != nil {
			log.Debug("[Updater] Check cancelled", slog.String("error", err.Error()))
			return
		}
		log.Warn("[Updater] Check failed", slog.String("error", err.Error()))
		return
	}
	if release == nil {
		// Already up to date — and so a release still pending names one that
		// was pulled.
		if tag := routes.ClearPendingUpdate(seen); tag != "" {
			announceUpdateCleared(wsHub, tuiCh, tag)
		}
		return
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

// filterJobsByAgeThreshold removes finished jobs older than hideAgeDays from
// the slice using jobfilter.ArchiveCutoff + jobfilter.IsArchived — the one
// predicate the REST archived filter, the WS broadcast gate, the TUI list
// and the Web UI's _evaluateArchiveBoundary all classify by, so a given job
// is archived in all four or in none:
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
	cutoff := jobfilter.ArchiveCutoff(time.Now(), hideAgeDays)
	anyFiltered := false
	for _, j := range jobs {
		if jobfilter.IsArchived(j, cutoff) {
			anyFiltered = true
			break
		}
	}
	if !anyFiltered {
		return jobs
	}
	filtered := make([]*database.Job, 0, len(jobs))
	for _, j := range jobs {
		if jobfilter.IsArchived(j, cutoff) {
			continue
		}
		filtered = append(filtered, j)
	}
	return filtered
}

// launcherStalenessNote renders the INFO-level suggestion logged when the
// supervisor process is running a different version than this child — the
// launcher keeps executing the binary it was originally started from across
// every in-place update, so launcher-side improvements (crash supervision,
// auto-rollback, …) only take effect after a full stop/start. Deliberately a
// light suggestion, never a warning: nothing is broken in the meantime.
// Returns "" when the versions match (nothing to log). An empty
// launcherVersion means the launcher predates the version handshake itself.
func launcherStalenessNote(launcherVersion, currentVersion string) string {
	if launcherVersion == currentVersion {
		return ""
	}
	supervisor := "an earlier version"
	if launcherVersion != "" {
		supervisor = "v" + launcherVersion
	}
	return fmt.Sprintf("Supervisor is %s, this process is v%s — a full restart refreshes it (optional, at your leisure).",
		supervisor, currentVersion)
}

// shouldSkipPendingVersion decides whether a .update-pending breadcrumb (the
// release tag ApplyUpdate was updating TO) should be recorded as the skipped
// version on this boot. True only for the auto-rollback shape: the pending
// tag names a DIFFERENT version than the one running (we are the restored
// previous binary, not the update that landed) AND a failed-update marker is
// present (the launcher documented the rollback). currentVersion is the bare
// semver ("2.7.1"); the tag carries the "v" prefix, matching SkippedVersion's
// comparison against release.TagName.
func shouldSkipPendingVersion(pendingTag, currentVersion string, failureMarkerPresent bool) bool {
	return pendingTag != "" &&
		pendingTag != "v"+currentVersion &&
		failureMarkerPresent
}

// cookieFilePath returns the Netscape cookie file the services use — the
// jar's, falling back to the setting — for operator-facing messages, or a
// short prose stand-in when none is set.
//
// Auth-failure guidance used to say only "re-run cookie setup from Settings",
// which is a dead end wherever the interactive browser login cannot run — it
// needs a headed browser and a person at it, and the setup endpoints are
// loopback-gated so a remote dashboard cannot reach them either. Naming the
// actual path makes the advice concrete in every deployment without having to
// guess at the environment: a Docker operator reads "/data/cookies.txt" and
// knows which host file to replace.
func (s *runState) cookieFilePath() string {
	// The jar's path first: the services read and write the file they loaded
	// at boot, and cookies.cookie_file is restart-required — saved without
	// the restart it names a file nothing touches, so advice to replace it
	// sent the operator to the wrong file.
	var path string
	if s.jar != nil {
		path = s.jar.GetFilePath()
	}
	if path == "" && s.configStore != nil {
		s.configStore.Read(func(c *config.MoomboxConfig) { path = c.Cookies.CookieFile })
	}
	if path == "" {
		return "the file named by cookies.cookie_file (currently unset)"
	}
	return path
}

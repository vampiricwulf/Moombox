package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	isatty "github.com/mattn/go-isatty"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

var (
	version = "2.6.0-test.24"
	commit  = ""
)

// exitCodeRestart is the exit code the child uses to signal the launcher
// that it should respawn. Used for both config restarts and update restarts.
const exitCodeRestart = 42

// createNoWindow prevents a console window from appearing when spawning
// detached processes on Windows (passed to SysProcAttr.CreationFlags).
const createNoWindow = 0x08000000

func init() {
	// Strip "v" prefix from version if set via -ldflags (tag name includes it)
	version = strings.TrimPrefix(version, "v")

	if commit != "" {
		return
	}
	// Resolve commit from Go build info (populated by `go build` in a git repo)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				commit = s.Value[:7]
				return
			}
		}
	}
	commit = "unknown"
}

// envDisablesTUI reports whether the MOOMBOX_NO_TUI value is one of the
// truthy strings (case-insensitive). Replaces a regex with a tiny switch —
// faster, no init-time compile, easier to scan (per audit
// reports/cmd-moombox.md QI-4).
func envDisablesTUI(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// Per-minute rate-limit ceilings for the four web rate-limiter slots.
// Pulled out of NewRateLimiter call sites so a tuning change is one place
// (per audit reports/cmd-moombox.md QI-6). Each is paired with time.Minute
// so the constant name encodes the window.
const (
	rateLimitAPIPerMinute      = 20
	rateLimitPOTPerMinute      = 10
	rateLimitLoginPerMinute    = 5
	rateLimitPasswordPerMinute = 3
)

func main() {
	// Subcommands (like `moombox add <url>`) do not need the launcher/child
	// split — they run briefly in-process and exit. Checking for them before
	// the `_MOOMBOX_CHILD` gate avoids spawning an unnecessary child process
	// (saves ~100ms and prevents a silent ghost spawn on CLI add commands).
	if len(os.Args) > 1 && os.Args[1] == "add" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: moombox add <video_id_or_url>")
			os.Exit(1)
		}
		addVideo(os.Args[2])
		return
	}

	// Launcher/supervisor: if we're not already a child process, act as the
	// launcher. The launcher spawns moombox as a child, waits for it, and
	// respawns when the child exits with exitCodeRestart (config change or
	// update applied). This keeps one stable parent holding the console so
	// the child's TUI restores terminal state cleanly on exit, and avoids
	// process chain buildup across multiple restarts.
	if os.Getenv("_MOOMBOX_CHILD") != "1" {
		launchAndSupervise()
		return
	}

	configPath := flag.String("config", "", "Path to config file")
	logLevel := flag.String("log-level", "", "Override log level (DEBUG, INFO, WARN, ERROR)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	headless := flag.Bool("headless", false, "Run without TUI (web-only mode)")
	noTUI := flag.Bool("no-tui", false, "Run without TUI (web-only mode)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("moombox %s (%s)\n", version, commit)
		os.Exit(0)
	}

	// TTY detection: only use TUI if both stdin/stdout are terminals
	isTTY := isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
	envNoTUI := envDisablesTUI(os.Getenv("MOOMBOX_NO_TUI"))
	useTUI := isTTY && !*headless && !*noTUI && !envNoTUI

	if !useTUI {
		fmt.Println("Moombox - YouTube/Twitch Live Stream Archiver")
		fmt.Println("==============================================")
		fmt.Println()
	}

	// Resolve config path
	cfgPath := *configPath
	if cfgPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to get working directory: %v\n", err)
		}
		cfgPath = filepath.Join(cwd, "config.toml")
	}

	// Run the application. If a restart is requested (config change or
	// update), exit with exitCodeRestart so the launcher respawns us.
	if run(cfgPath, *logLevel, useTUI) {
		os.Exit(exitCodeRestart)
	}
}

// run initializes and runs the full application. Returns true if a restart was
// requested (config change, update applied, or via web API).
func run(configPath string, logLevelOverride string, useTUI bool) bool {
	// Graceful shutdown context (also stored on runState so every extracted
	// phase sees the same cancellation).
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s := &runState{
		ctx:        ctx,
		cancel:     cancel,
		configPath: configPath,
		useTUI:     useTUI,
	}

	// Initialise all 16 service sections (config, logger, updater, database,
	// connectivity, cookies, platform services, worker, monitors, cookie
	// refresh, web server, rate limiters, auth, shared closures). On a fatal
	// startup failure, skip deferred cleanup — matches the pre-refactor
	// behaviour where os.Exit(1) jumps past any defers that may not even
	// have been registered yet.
	if err := s.initServices(logLevelOverride); err != nil {
		fmt.Fprintf(os.Stderr, "Startup error: %v\n", err)
		waitForKeypress()
		os.Exit(1)
	}

	defer s.closeLog()
	defer s.closeDB()
	defer s.connMon.Stop()
	defer s.closeLimiters()

	// Alias runState fields to local names so the not-yet-extracted sections
	// below (route wiring, WS wiring, monitor callbacks, TUI, shutdown)
	// continue to compile unchanged. Subsequent SP-* commits will move those
	// sections into *runState methods and shrink this block to nothing.
	var (
		cfg               = s.cfg
		cfgMu             = &s.cfgMu
		log               = s.log
		upd               = s.upd
		db                = s.db
		connMon           = s.connMon
		ytService         = s.ytService
		notifyMgr         = s.notifyMgr
		dlWorker          = s.dlWorker
		trimSvc           = s.trimSvc
		feedMon           = s.feedMon
		decapiMon         = s.decapiMon
		twitchMon         = s.twitchMon
		cookieRefresh     = s.cookieRefresh
		autoCookieSvc     = s.autoCookieSvc
		browserProfileDir = s.browserProfileDir
		webServer         = s.webServer
		wsHub             = s.wsHub
		authSvc           = s.authSvc
		kickMonitors      = s.kickMonitors
		triggerRestart    = s.triggerRestart
		tuiUpdateStatusCh = s.tuiUpdateStatusCh
		tuiDiskStatusCh   = s.tuiDiskStatusCh
	)
	authChangeTUI := &s.authChangeTUI

	// Register all routes. See routes_wiring.go.
	importCleanup := s.wireRoutes()
	defer importCleanup()

	// WebSocket wiring: upgrade handler, AuthMiddleware client-token fallback,
	// upgrade-time WS auth check, InitialState, OpenBrowser + static files.
	// See ws_wiring.go.
	s.wireWebSocket()

	// =========================================================================
	// Wire cookie recovery callback
	// =========================================================================
	cookieRefresh.OnRecoveryNeeded = func(platform string) {
		cfgMu.RLock()
		autoEnabled := cfg.Cookies.AutoEnabled
		cfgMu.RUnlock()
		if !autoEnabled {
			log.Debug("Auth lost but auto-cookies disabled, skipping recovery", "platform", platform)
			return
		}
		log.Warn("Auth lost, attempting auto-cookie recovery", "platform", platform)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("auto-cookie recovery panic", "panic", r)
				}
			}()
			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer refreshCancel()
			ok, err := autoCookieSvc.RefreshCookies(refreshCtx)
			if err != nil {
				log.Error("auto-cookie recovery failed", "platform", platform, "err", err)
			} else if ok {
				log.Info("auto-cookie recovery succeeded", "platform", platform)
				// Re-check auth status immediately so the UI updates
				cookieRefresh.CheckNow(context.Background())
			} else {
				log.Warn("auto-cookie recovery did not restore auth", "platform", platform)
			}
		}()
	}

	// When a platform transitions from not-authenticated to authenticated, sweep
	// any jobs parked in StatusCookies on that platform back to Upcoming so they
	// get re-probed without manual intervention. Closes audit decision #23
	// (worker.md Q3).
	cookieRefresh.OnAuthRecovered = func(platform string) {
		jobs, err := db.GetAllJobs()
		if err != nil {
			log.Warn("auth-recovered sweep: GetAllJobs failed", "platform", platform, "err", err)
			return
		}
		resumed := 0
		for _, job := range jobs {
			if job.Status != database.StatusCookies {
				continue
			}
			if job.Platform != platform {
				continue
			}
			db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusUpcoming,
				"error":  "",
			})
			resumed++
		}
		if resumed > 0 {
			log.Info("auth recovered — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			notifyMgr.Send("Authentication Recovered",
				fmt.Sprintf("Resumed %d job(s) waiting on %s cookies", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{},
			)
		}
	}

	// =========================================================================
	// Wire up event callbacks
	// =========================================================================

	// Wire ProbeVideo callback for monitors (metadata check before job creation).
	// Returns VideoProbeResult with stream status so monitors can skip non-streams.
	// Uses the caller-supplied ctx so monitor shutdown cancels in-flight probes
	// (per audit reports/cross-cutting.md C4).
	probeVideoFunc := func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := ytService.ProbeVideoStatus(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus: string(meta.StreamStatus),
			Title:        meta.Title,
			ChannelName:  meta.ChannelName,
		}, nil
	}
	feedMon.ProbeVideo = probeVideoFunc
	decapiMon.ProbeVideo = probeVideoFunc

	// createYouTubeJob creates a YouTube job and enqueues it.
	// Stream status classification is now handled by the monitors via ProcessYouTubeVideo.
	createYouTubeJob := func(videoID, title, videoURL string, ch *config.ChannelConfig, source string) {
		log.Info("Video found", slog.String("source", source), slog.String("videoID", videoID), slog.String("title", title))

		includeNonLive := ch.IncludeNonLiveContent

		outputDir := resolveOutputDir(ch, cfg, cfgMu)
		thumbnailURL := youtubeThumbnailURL(videoID)

		now := time.Now().UTC().Format(time.RFC3339)
		job := &database.Job{
			ID:                videoID,
			VideoID:           videoID,
			URL:               videoURL,
			Title:             title,
			ChannelName:       ch.Name,
			Platform:          "youtube",
			Status:            database.StatusUpcoming,
			ThumbnailURL:      thumbnailURL,
			OutputDirectory:   outputDir,
			AllowNonStream:    includeNonLive,
			QualityPreference: ch.QualityPreference,
			CreatedAt:         now,
			UpdatedAt:         now,
		}

		added, err := db.AddJob(job)
		if err != nil {
			log.Error("Failed to add YouTube job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		db.AddToHistory(videoID)
		dlWorker.EnqueueJob(videoID)
		wsHub.BroadcastJobsUpdate(filterJobsByAge(getAllJobsSafe(db), cfg, webServer.CfgMu()))
		if notifyMgr.HasTargets() {
			notifyMgr.Send("Stream Found",
				fmt.Sprintf("Found matching stream: %s", title),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Channel", Value: ch.Name, Inline: true},
					{Name: "Video ID", Value: videoID, Inline: true},
				},
				notifications.SendOptions{
					Event:     "found",
					URL:       videoURL,
					Thumbnail: youtubeThumbnailURL(videoID),
				})
		}
	}

	// Monitor -> Worker: create jobs for found videos.
	// Wrapped with panic recovery to prevent a single bad callback from crashing the monitor goroutine.
	feedMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Panic in OnVideoFound (feed)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "feed")
	}

	decapiMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Panic in OnVideoFound (decapi)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "decapi")
	}

	twitchMon.OnStreamFound = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Panic in OnStreamFound (twitch)", slog.Any("panic", r))
			}
		}()
		jobID := twitch.BuildJobID(info.StreamID, false)
		log.Info("Stream found by Twitch monitor", slog.String("jobID", jobID), slog.String("title", info.Title))

		outputDir := resolveOutputDir(ch, cfg, cfgMu)

		now := time.Now().UTC().Format(time.RFC3339)
		title := info.ChannelDisplayName + " — " + info.Title
		if info.Title == "" {
			title = info.ChannelDisplayName + " — " + time.Now().UTC().Format(time.RFC3339)
		}

		job := &database.Job{
			ID:                jobID,
			VideoID:           info.StreamID,
			URL:               "https://twitch.tv/" + info.ChannelLogin,
			Title:             title,
			ChannelName:       info.ChannelDisplayName,
			Platform:          "twitch",
			Status:            database.StatusLive, // Twitch: immediately Live (confirmed by GQL)
			ThumbnailURL:      info.ThumbnailURL,
			ChannelAvatarURL:  info.ProfileImageURL,
			TwitchCategory:    info.GameCategory,
			TwitchQuality:     ch.QualityPreference,
			QualityPreference: ch.QualityPreference,
			StreamStartTime:   info.StartedAt,
			OutputDirectory:   outputDir,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		added, err := db.AddJob(job)
		if err != nil {
			log.Error("Failed to add Twitch job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		db.AddToHistory(jobID)
		dlWorker.EnqueueJob(jobID)
		wsHub.BroadcastJobsUpdate(filterJobsByAge(getAllJobsSafe(db), cfg, webServer.CfgMu()))
		if notifyMgr.HasTargets() {
			twitchFields := []notifications.Field{
				{Name: "Channel", Value: info.ChannelDisplayName, Inline: true},
				{Name: "Stream ID", Value: info.StreamID, Inline: true},
			}
			if info.GameCategory != "" {
				twitchFields = append(twitchFields, notifications.Field{
					Name: "Category", Value: info.GameCategory, Inline: true,
				})
			}
			notifyMgr.Send("Twitch Stream Found",
				fmt.Sprintf("Live: %s", title),
				notifications.TypeInfo,
				twitchFields,
				notifications.SendOptions{
					Event:     "found",
					URL:       "https://twitch.tv/" + info.ChannelLogin,
					Thumbnail: info.ThumbnailURL,
				})
		}
	}

	// Monitor -> WebSocket: broadcast timer updates
	// TypeScript broadcasts ALL three monitor times on each schedule event,
	// so we do the same — read all three monitors' next check times.
	broadcastAllTimers := func() {
		wsHub.BroadcastCheckTimers(map[string]any{
			"nextFeedCheck":   feedMon.GetNextCheckAt(),
			"nextDecapiCheck": decapiMon.GetNextCheckAt(),
			"nextTwitchCheck": twitchMon.GetNextCheckAt(),
		})
	}

	// OnSchedule subscriber slots — set once before monitor.Start() and only
	// mutated via atomic.Pointer.Store thereafter. Reassigning the monitor's
	// plain func field while its goroutine is running would race with
	// scheduleNext() reading it; funneling subscribers through an atomic
	// pointer keeps the read-side lock-free and the write-side safe.
	var (
		feedTUISchedule   atomic.Pointer[func(int64)]
		decapiTUISchedule atomic.Pointer[func(int64)]
		twitchTUISchedule atomic.Pointer[func(int64)]
	)
	feedMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := feedTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	decapiMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := decapiTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	twitchMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := twitchTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}

	// Initialize per-job log tracking with existing jobs (matches TS knownJobIds)
	if existingJobs, err := db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates
	s.unsubWSJobUpdate = db.OnJobUpdate(func(job *database.Job) {
		// Skip broadcasting updates for archived (old finished) jobs
		if job.Status == database.StatusFinished && job.UpdatedAt != "" {
			cfgMu.RLock()
			ageDays := int(cfg.Monitors.HideFinishedAgeDays.Value)
			cfgMu.RUnlock()
			cutoff := time.Now().AddDate(0, 0, -ageDays)
			if t, err := time.Parse(time.RFC3339, job.UpdatedAt); err == nil && t.Before(cutoff) {
				return
			}
		}
		wsHub.BroadcastJobUpdate(job.ID, job)
	})
	s.unsubWSJobsChange = db.OnJobsChange(func(jobs []*database.Job) {
		// Keep per-job log tracking in sync (matches TS knownJobIds update)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			db.TrackJobForLogs(j.ID)
		}
		db.PruneJobLogs(activeIDs)
		log.PruneJobLogs(activeIDs)

		wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, cfg, webServer.CfgMu()))
	})

	// Logger -> WebSocket: broadcast log lines + route to per-job buffers
	s.logSub = log.Subscribe()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("log forwarder panic", "panic", r)
			}
		}()
		for line := range s.logSub {
			wsHub.BroadcastLog(line)
			db.RouteLogToJobs(line) // Route to per-job buffer (matches TS knownJobIds log routing)
		}
	}()

	// Connectivity -> monitors + WebSocket: kick monitors on reconnect, broadcast state
	connMon.OnStateChange(func(online bool) {
		if online {
			feedMon.CheckNow()
			decapiMon.CheckNow()
			twitchMon.CheckNow()
		}
		wsHub.BroadcastConnectivity(online)
	})

	// =========================================================================
	// Start services (consumers first)
	// =========================================================================

	log.Info("Moombox initialized",
		slog.Int("port", cfg.Network.Port),
		slog.String("network_access", cfg.Network.NetworkAccess),
	)

	// Start monitors
	feedMon.Start(ctx)
	decapiMon.Start(ctx)
	twitchMon.Start(ctx)

	// Start download worker (runs in goroutine — Start() blocks on job queue)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("download worker panic", "panic", r)
			}
		}()
		dlWorker.Start(ctx)
	}()

	// Start cookie refresh (only if a cookie file is configured, like TS)
	if cfg.Cookies.CookieFile != "" {
		// Seed expected auth from persisted platforms so auth loss is detected on restart
		if cfg.Cookies.AutoEnabled && len(cfg.Cookies.Platforms) > 0 {
			cookieRefresh.SetExpectedPlatforms(cfg.Cookies.Platforms)
		}
		cookieRefresh.Start(ctx)
	} else {
		log.Debug("[CookieRefresh] No cookie file configured, skipping refresh service")
	}

	// Start auto-cookie periodic refresh if enabled and profile exists
	if cfg.Cookies.AutoEnabled {
		if _, err := os.Stat(browserProfileDir); err == nil {
			interval := cfg.Cookies.RefreshInterval.AsDuration(time.Minute)
			if interval > 0 {
				autoCookieSvc.StartPeriodicRefresh(ctx, interval)
			}
		}
	}

	// Start web server — wait for initial binding result before continuing.
	// Matches TS: exits on web server failure in non-TUI mode.
	webErrCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("web server panic", "panic", r)
				// webErrCh is buffered to 1. If webServer.Start already
				// sent a nil value (fast graceful return followed by the
				// deferred recover firing — extremely rare but possible),
				// a plain send would block forever. Use non-blocking send.
				select {
				case webErrCh <- fmt.Errorf("web server panic: %v", r):
				default:
				}
			}
		}()
		webErrCh <- webServer.Start(ctx)
	}()

	// Give the server a moment to bind (port binding is synchronous in Start).
	// If it fails quickly, exit; otherwise proceed.
	select {
	case err := <-webErrCh:
		if err != nil {
			if !useTUI {
				log.Error("Web server failed to start", slog.String("error", err.Error()))
				fmt.Fprintf(os.Stderr, "\nError: %s\n", err.Error())
				fmt.Fprintln(os.Stderr, "Web dashboard is unavailable.")
				waitForKeypress()
				os.Exit(1)
			}
			log.Warn("Web server failed, continuing in TUI-only mode", slog.String("error", err.Error()))
		}
	case <-time.After(500 * time.Millisecond):
		// Server is binding/listening successfully, drain errors in background
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("web error drainer panic", "panic", r)
				}
			}()
			if err := <-webErrCh; err != nil {
				log.Error("Web server error", slog.String("error", err.Error()))
			}
		}()
	}

	// Expose actual bound port for TUI and other components (matches TS: process.env.MOOMBOX_PORT)
	if actualPort := webServer.ActualPort; actualPort > 0 {
		cfgMu.Lock()
		cfg.Network.Port = actualPort
		cfgMu.Unlock()
	}

	// Initial disk space check (populate immediately so status bar has data).
	cfgMu.RLock()
	initialOutputDir := cfg.Paths.OutputDirectory
	cfgMu.RUnlock()
	routes.UpdateDiskStatus(initialOutputDir, cfg, webServer.CfgMu())

	// Auto-update check: initial check + daily ticker
	if upd != nil && cfg.Updates.AutoCheckUpdates {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("auto-update check panic", "panic", r)
				}
			}()
			// Initial check (slight delay to avoid slowing startup)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			checkAndBroadcastUpdate(ctx, upd, wsHub, notifyMgr, tuiUpdateStatusCh, log)

			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					checkAndBroadcastUpdate(ctx, upd, wsHub, notifyMgr, tuiUpdateStatusCh, log)
				}
			}
		}()
	}

	// Periodic memory + disk usage logging (every 2 minutes) for diagnostics.
	// Matches TS: process.memoryUsage() logging with heap delta tracking.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("stats ticker panic", "panic", r)
			}
		}()
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		var prevHeapMB float64
		var lastDiskNotify time.Time
		var lastDiskLevel string
		diskCheckCounter := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				heapMB := float64(m.HeapAlloc) / 1048576
				delta := ""
				if prevHeapMB > 0 {
					diff := heapMB - prevHeapMB
					sign := "+"
					if diff < 0 {
						sign = ""
					}
					delta = fmt.Sprintf(" (%s%.1f)", sign, diff)
				}
				prevHeapMB = heapMB
				log.Debug(fmt.Sprintf(
					"[Memory] Sys: %.1fMB, Heap: %.1f%s/%.1fMB, Stack: %.1fMB, GC: %d",
					float64(m.Sys)/1048576,
					heapMB, delta,
					float64(m.HeapSys)/1048576,
					float64(m.StackInuse)/1048576,
					m.NumGC,
				))

				// Disk space check every ~5th tick (~10 minutes at 2min interval,
				// close enough to the 5-min target while reusing the existing ticker).
				diskCheckCounter++
				if diskCheckCounter%3 == 0 { // every 3 ticks = ~6 minutes
					cfgMu.RLock()
					diskOutputDir := cfg.Paths.OutputDirectory
					cfgMu.RUnlock()
					if ds := routes.UpdateDiskStatus(diskOutputDir, cfg, webServer.CfgMu()); ds != nil {
						// Broadcast to web clients
						wsHub.Broadcast("disk_status", map[string]any{
							"free":      ds.Free,
							"total":     ds.Total,
							"usedPct":   ds.UsedPct,
							"warnLevel": ds.WarnLevel,
						})

						// Push to TUI
						select {
						case tuiDiskStatusCh <- tui.DiskStatusMsg{
							Free: ds.Free, UsedPct: ds.UsedPct, Warn: ds.WarnLevel,
						}:
						default:
						}

						// Notification with 30-minute cooldown
						if ds.WarnLevel != "ok" {
							canNotify := lastDiskLevel != ds.WarnLevel ||
								time.Since(lastDiskNotify) >= 30*time.Minute
							if canNotify && notifyMgr.HasTargets() {
								freeGB := float64(ds.Free) / (1024 * 1024 * 1024)
								level := "Warning"
								ntype := notifications.TypeWarning
								if ds.WarnLevel == "critical" {
									level = "Critical"
									ntype = notifications.TypeError
								}
								notifyMgr.Send(
									fmt.Sprintf("Disk Space %s", level),
									fmt.Sprintf("%.1f%% used — %.1f GB free on output drive", ds.UsedPct, freeGB),
									ntype, nil, notifications.SendOptions{Event: "disk_warning"},
								)
								lastDiskNotify = time.Now()
								lastDiskLevel = ds.WarnLevel
							}
						} else {
							lastDiskLevel = "" // Reset cooldown when back to ok
						}
					}
				}
			}
		}
	}()

	// =========================================================================
	// TUI or headless mode
	// =========================================================================
	if !useTUI {
		if !isatty.IsTerminal(os.Stdout.Fd()) {
			log.Info("Non-interactive terminal detected, running in headless mode")
		} else {
			log.Info("Running in headless mode (web-only)")
		}
		<-ctx.Done()
	} else {
		// Start TUI
		app := tui.NewApp()
		s.quitTUI = app.QuitTUI // allow API restart to exit TUI

		// Pass config reference, config mutex, and version for settings panel
		app.SetConfig(cfg)
		app.SetCfgMu(cfgMu)
		app.SetVersion(version)
		app.SetInternalToken(webServer.InternalToken())
		app.IsFirstRun = !cfg.ConfigLoaded

		// Wire TUI callbacks
		app.OnAddVideo = func(url string) {
			// Job creation is handled via HTTP POST in addVideoCmd; this is just for logging
			log.Info("Add video from TUI", slog.String("url", url))
		}
		// OnOpenFolder is handled by TUI callbacks below
		app.OnCancelJob = func(jobID string) {
			dlWorker.CancelJob(jobID)
		}
		app.OnDeleteJob = func(jobID string) {
			if err := db.DeleteJob(jobID); err != nil {
				log.Error("Failed to delete job", slog.String("error", err.Error()))
			}
		}
		app.OnResumeJob = func(jobID string) {
			dlWorker.ResumeJob(jobID)
		}
		app.OnReinitializeJob = func(jobID string) {
			dlWorker.ReinitializeJob(jobID)
		}
		app.OnMuxJob = func(jobID string) error {
			return dlWorker.MuxJob(jobID)
		}
		app.HasStagingFiles = func(jobID string) bool {
			cfgMu.RLock()
			base := cfg.Paths.EffectiveStagingDir()
			cfgMu.RUnlock()
			return worker.HasStagingFiles(base, jobID)
		}
		app.HasSegmentFiles = func(jobID string) bool {
			cfgMu.RLock()
			base := cfg.Paths.EffectiveStagingDir()
			cfgMu.RUnlock()
			return worker.HasSegmentFiles(base, jobID)
		}
		app.OnOpenFolder = func(jobID string) {
			job, err := db.GetJob(jobID)
			if err != nil || job == nil {
				return
			}

			var dir string
			if job.OutputFile != "" {
				dir = filepath.Dir(job.OutputFile)
			} else {
				// Fall back to staging directory for active jobs
				cfgMu.RLock()
				stagingBase := cfg.Paths.EffectiveStagingDir()
				cfgMu.RUnlock()
				dir = filepath.Join(stagingBase, job.ID)
				if _, err := os.Stat(dir); err != nil {
					return // staging dir doesn't exist yet
				}
			}

			// Open folder in file manager (cross-platform)
			var cmd *exec.Cmd
			switch runtime.GOOS {
			case "windows":
				cmd = exec.Command("explorer", dir)
			case "darwin":
				cmd = exec.Command("open", dir)
			default:
				cmd = exec.Command("xdg-open", dir)
			}
			if err := cmd.Start(); err != nil {
				log.Debug("Failed to open folder in file manager", slog.String("error", err.Error()))
			}
		}
		app.OnCreateTrim = func(jobID string, startSec, endSec float64, onProgress func(float64)) (string, string) {
			job, err := db.GetJob(jobID)
			if err != nil || job == nil {
				log.Error("Failed to get job for trim", slog.String("jobID", jobID))
				return "", "Failed to get job"
			}
			record, err := trimSvc.CreateTrimWithProgress(context.Background(), job, startSec, endSec, onProgress)
			if err != nil {
				log.Error("Failed to create trim", slog.String("error", err.Error()))
				return "", err.Error()
			}
			return record.Filename, ""
		}
		app.OnDeleteTrim = func(jobID, trimID string) error {
			if err := trimSvc.DeleteTrim(jobID, trimID); err != nil {
				log.Error("Failed to delete trim", slog.String("error", err.Error()))
				return err
			}
			return nil
		}
		app.OnListOrphans = func() ([]tui.OrphanedFileEntry, error) {
			entries, err := worker.ScanOrphanedFiles(db, cfg)
			if err != nil {
				return nil, err
			}
			result := make([]tui.OrphanedFileEntry, len(entries))
			for i, e := range entries {
				result[i] = tui.OrphanedFileEntry{
					Path:      e.Path,
					RelPath:   e.RelPath,
					Type:      e.Type,
					Size:      e.Size,
					Modified:  e.Modified,
					JobID:     e.JobID,
					JobTitle:  e.JobTitle,
					JobStatus: e.JobStatus,
				}
			}
			return result, nil
		}
		app.OnDeleteOrphan = func(path string) error {
			return worker.DeleteOrphanedFile(path, db, cfg)
		}
		app.OnListClientTokens = func() ([]*database.ClientToken, error) {
			return db.ListClientTokens()
		}
		app.OnDeleteClientToken = func(id string) error {
			return db.DeleteClientToken(id)
		}
		app.OnSaveConfig = func(updatedCfg *config.MoomboxConfig) {
			if err := config.Save(updatedCfg, configPath); err != nil {
				log.Error("Failed to save config from TUI", slog.String("error", err.Error()))
			} else {
				log.Info("Config saved from TUI settings")
			}
			// Hot-reload runtime settings (match TS: refreshLogLevel + setMaxDownloadSlots)
			if updatedCfg.Logs.LogLevel != "" {
				log.SetLevel(updatedCfg.Logs.LogLevel)
			}
			if updatedCfg.Downloader.NumParallelDownloads > 0 {
				dlWorker.SetParallelDownloads(updatedCfg.Downloader.NumParallelDownloads)
			}
			// Kick monitors so they re-evaluate channels (may have been added/removed)
			kickMonitors()
		}
		app.OnRestart = func() { triggerRestart("TUI settings") }
		if upd != nil {
			app.OnCheckUpdate = func() (*tui.UpdateStatusMsg, error) {
				log.Info("Update check requested from TUI")
				release, err := upd.CheckForUpdate(context.Background())
				if err != nil {
					return nil, err
				}
				if release == nil {
					return nil, nil
				}
				routes.SharedUpdateInfo.Store(release)
				wsHub.Broadcast("update_available", release)
				return &tui.UpdateStatusMsg{
					Version:      release.Version,
					TagName:      release.TagName,
					ReleaseNotes: release.ReleaseNotes,
				}, nil
			}
			app.OnApplyUpdate = func(ver string) string {
				release := routes.SharedUpdateInfo.Load()
				if release == nil {
					return "no update info available"
				}
				log.Info("Update requested from TUI", slog.String("version", ver))
				if err := upd.ApplyUpdate(context.Background(), release); err != nil {
					log.Error("[Updater] Update failed", slog.String("error", err.Error()))
					return err.Error()
				}
				triggerRestart("TUI update")
				return ""
			}
			app.OnVerifySignature = func() error {
				return upd.VerifyCurrentSignature(context.Background())
			}
		}
		app.OnRecheckCookies = func() (bool, bool) {
			log.Info("Cookie recheck requested from TUI")
			cookieRefresh.CheckNow(context.Background())
			status := cookieRefresh.GetStatus()
			return status.YouTubeAuthenticated, status.TwitchAuthenticated
		}
		if cfg.Cookies.AutoEnabled {
			app.OnForceRefreshCookies = func() (bool, error) {
				log.Info("Browser cookie refresh requested from TUI")
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				ok, err := autoCookieSvc.RefreshCookies(ctx)
				if err != nil {
					return false, err
				}
				cookieRefresh.CheckNow(context.Background())
				return ok, nil
			}
		}

		app.OnHashPassword = func(password string) string {
			hash, err := authSvc.HashPassword(password)
			if err != nil {
				log.Error("Failed to hash password", slog.String("error", err.Error()))
				return ""
			}
			return hash
		}
		app.OnVerifyPassword = func(password, hash string) bool {
			return authSvc.VerifyPassword(password, hash)
		}

		// Wire setup wizard callbacks (OnComplete saves config, OnInstallYtdlp writes plugin)
		app.SetSetupCallbacks(
			func(updatedCfg *config.MoomboxConfig) error {
				cfgMu.Lock()
				defer cfgMu.Unlock()
				return config.Save(updatedCfg, configPath)
			},
			func(port int, httpsEnabled bool) {
				if err := routes.InstallYtdlpPlugin(port, httpsEnabled); err != nil {
					log.Error("Failed to install yt-dlp plugin from setup", slog.String("error", err.Error()))
				} else {
					log.Info("yt-dlp plugin installed from setup wizard", slog.Int("port", port))
				}
			},
			func(platform string) error {
				if err := autoCookieSvc.StartSetup(platform); err != nil {
					log.Error("Failed to start auto-cookie setup", slog.String("platform", platform), slog.String("error", err.Error()))
					return err
				}
				return nil
			},
			func() (bool, bool, error) {
				finishCtx, finishCancel := context.WithTimeout(ctx, 60*time.Second)
				defer finishCancel()
				yt, tw, err := autoCookieSvc.FinishSetup(finishCtx)
				if err != nil {
					log.Error("Failed to finish auto-cookie setup", slog.String("error", err.Error()))
					return yt, tw, err
				}
				return yt, tw, nil
			},
			func() {
				autoCookieSvc.CancelSetup()
			},
			func() { triggerRestart("TUI setup wizard") },
		)

		// Wire setup wizard password hashing
		app.SetupWizHashPassword(func(password string) (string, error) {
			return authSvc.HashPassword(password)
		})

		// Wire setup wizard FFmpeg status check
		app.SetupWizFFmpegCheck(func() (bool, string) {
			path := cfg.Paths.FfmpegPath
			if path == "" {
				path = "ffmpeg"
			}
			valid, version, _ := routes.CheckFFmpegCached(path)
			return valid, version
		})

		// Wire FFmpeg check callbacks for TUI
		app.OnCheckFFmpeg = func(path string) (bool, string, string) {
			if path == "" {
				path = "ffmpeg"
			}
			return routes.CheckFFmpegCached(path)
		}
		app.OnPrepareInstall = routes.PrepareInstall
		app.OnConfirmInstall = routes.ConfirmInstall
		app.OnRejectInstall = routes.RejectInstall
		app.OnCheckPrereqs = func() (bool, bool) {
			chocoAvail := false
			wingetAvail := false
			if _, err := exec.LookPath("choco"); err == nil {
				chocoAvail = true
			}
			if _, err := exec.LookPath("winget"); err == nil {
				wingetAvail = true
			}
			return chocoAvail, wingetAvail
		}

		// Check FFmpeg on startup (after config is loaded)
		if cfg.ConfigLoaded {
			ffmpegPath := cfg.Paths.FfmpegPath
			if ffmpegPath == "" {
				ffmpegPath = "ffmpeg"
			}
			checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer checkCancel()
			if err := exec.CommandContext(checkCtx, ffmpegPath, "-version").Run(); err != nil {
				app.ShowFFmpegCheck()
			}
		}

		// Create async update channels for TUI
		jobUpdateCh := make(chan *database.Job, 100)
		jobsUpdateCh := make(chan []*database.Job, 10)
		logCh := make(chan string, 200)
		checkTimersCh := make(chan tui.CheckTimersMsg, 10)
		cookieStatusCh := make(chan tui.CookieStatusMsg, 5)

		app.SetUpdateChannels(jobUpdateCh, jobsUpdateCh, logCh, checkTimersCh, cookieStatusCh, tuiDiskStatusCh, tuiUpdateStatusCh)

		// Dropped-message counters — track silent drops on TUI channels
		var tuiDroppedJobs, tuiDroppedLogs atomic.Int64

		// Push initial disk status to TUI
		if ds := routes.SharedDiskStatus.Load(); ds != nil {
			select {
			case tuiDiskStatusCh <- tui.DiskStatusMsg{Free: ds.Free, UsedPct: ds.UsedPct, Warn: ds.WarnLevel}:
			default:
			}
		}

		// Forward DB events to TUI channels
		unsubTUIJobUpdate := db.OnJobUpdate(func(job *database.Job) {
			select {
			case jobUpdateCh <- job:
			default:
				tuiDroppedJobs.Add(1)
			}
		})
		unsubTUIJobsChange := db.OnJobsChange(func(jobs []*database.Job) {
			select {
			case jobsUpdateCh <- jobs:
			default:
			}
		})

		// Forward log lines to TUI
		tuiLogSub := log.Subscribe()
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("[Main] Panic in TUI log forwarder", "panic", fmt.Sprint(r))
				}
			}()
			for line := range tuiLogSub {
				select {
				case logCh <- line:
				default:
					tuiDroppedLogs.Add(1)
				}
			}
		}()

		// Backfill TUI with logs emitted before subscription
		app.BackfillLogs(log.GetRecentLines())

		// Forward monitor schedule events to TUI. We store a TUI-only callback
		// in the atomic slot defined earlier; the dispatcher wired to
		// feedMon/decapiMon/twitchMon.OnSchedule (set before monitor.Start())
		// loads it lock-free. Reassignment here is race-free because the
		// monitor goroutine never reads the field directly.
		makeTUISchedule := func(makeMsg func(time.Time) tui.CheckTimersMsg) func(int64) {
			return func(nextCheckAt int64) {
				t := time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
				select {
				case checkTimersCh <- makeMsg(t):
				default:
				}
			}
		}
		feedFn := makeTUISchedule(func(t time.Time) tui.CheckTimersMsg {
			return tui.CheckTimersMsg{NextFeedCheck: t}
		})
		decapiFn := makeTUISchedule(func(t time.Time) tui.CheckTimersMsg {
			return tui.CheckTimersMsg{NextDecapiCheck: t}
		})
		twitchFn := makeTUISchedule(func(t time.Time) tui.CheckTimersMsg {
			return tui.CheckTimersMsg{NextTwitchCheck: t}
		})
		feedTUISchedule.Store(&feedFn)
		decapiTUISchedule.Store(&decapiFn)
		twitchTUISchedule.Store(&twitchFn)

		// Send initial timer values — monitors fire OnSchedule during Start()
		// before the TUI wrappers above are installed, so the TUI misses those.
		for _, entry := range []struct {
			getNext func() int64
			makeMsg func(time.Time) tui.CheckTimersMsg
		}{
			{feedMon.GetNextCheckAt, func(t time.Time) tui.CheckTimersMsg { return tui.CheckTimersMsg{NextFeedCheck: t} }},
			{decapiMon.GetNextCheckAt, func(t time.Time) tui.CheckTimersMsg { return tui.CheckTimersMsg{NextDecapiCheck: t} }},
			{twitchMon.GetNextCheckAt, func(t time.Time) tui.CheckTimersMsg { return tui.CheckTimersMsg{NextTwitchCheck: t} }},
		} {
			if ms := entry.getNext(); ms > 0 {
				t := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
				select {
				case checkTimersCh <- entry.makeMsg(t):
				default:
				}
			}
		}

		// Wire cookie status to TUI
		authStatusToTUI := func(s cookies.AuthStatus) {
			var yt, tw tui.CookieStatus
			switch {
			case s.YouTubeAuthenticated:
				yt = tui.CookieStatusOK
			case s.HasYouTubeCookies:
				yt = tui.CookieStatusCookiesOnly
			}
			if s.TwitchAuthenticated {
				tw = tui.CookieStatusOK
			}
			// Check auto-cookie relogin state
			relogin := autoCookieSvc.GetStatus().NeedsManualRelogin
			if relogin.YouTube {
				yt = tui.CookieStatusRelogin
			}
			if relogin.Twitch {
				tw = tui.CookieStatusRelogin
			}
			cfgMu.RLock()
			ytActive, twActive := config.GetActivePlatforms(cfg)
			cfgMu.RUnlock()
			select {
			case cookieStatusCh <- tui.CookieStatusMsg{YT: yt, TW: tw, YTActive: ytActive, TWActive: twActive}:
			default:
			}
		}
		// Store TUI-side callback in the atomic slot; the dispatcher wired to
		// cookieRefresh.OnAuthChange (set before cookieRefresh.Start()) loads it
		// lock-free on each auth change. Store is race-free with the refresh
		// goroutine's Load — unlike the previous field reassignment.
		tuiAuthFn := func(s cookies.AuthStatus) {
			authStatusToTUI(s)
		}
		authChangeTUI.Store(&tuiAuthFn)
		// Send initial cookie status
		authStatusToTUI(cookieRefresh.GetStatus())

		// Send initial job list
		if jobs, err := db.GetAllJobs(); err == nil {
			select {
			case jobsUpdateCh <- jobs:
			default:
			}
		}

		// Wire connectivity state to TUI (uses program.Send, not a channel)
		unsubConnTUI := connMon.OnStateChange(func(online bool) {
			app.Send(tui.ConnectivityMsg{Online: online})
		})

		// Suppress stdout logging while TUI runs — BubbleTea owns the
		// alternate screen, and raw log writes corrupt the display.
		// The TUI log panel receives logs via Subscribe() instead.
		log.SuppressStdout()

		// Run TUI (blocks until quit)
		if err := tui.Run(app); err != nil {
			log.Error("TUI error", slog.String("error", err.Error()))
		}

		log.RestoreStdout()

		// Cleanup TUI channels — cancel first so workers stop sending,
		// then unsubscribe. Don't close channels: non-blocking sends
		// mean no goroutine will block, and GC handles cleanup.
		cancel() // TUI quit triggers shutdown
		log.Unsubscribe(tuiLogSub)
		unsubTUIJobUpdate()
		unsubTUIJobsChange()
		unsubConnTUI()

		// Report dropped messages (helps diagnose missed TUI updates)
		if n := tuiDroppedJobs.Load(); n > 0 {
			log.Warn("TUI dropped job update messages", slog.Int64("count", n))
		}
		if n := tuiDroppedLogs.Load(); n > 0 {
			log.Warn("TUI dropped log messages", slog.Int64("count", n))
		}
	}

	return s.shutdown()
}

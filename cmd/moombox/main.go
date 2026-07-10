package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	isatty "github.com/mattn/go-isatty"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

var (
	version = "2.7.0"
	commit  = ""
)

// exitCodeRestart is the exit code the child uses to signal the launcher
// that it should respawn. Used for both config restarts and update restarts.
const exitCodeRestart = 42

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

	configPath := flag.String("config", "", "Path to config file")
	logLevel := flag.String("log-level", "", "Override log level (DEBUG, INFO, WARN, ERROR)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	headless := flag.Bool("headless", false, "Run without TUI (web-only mode)")
	noTUI := flag.Bool("no-tui", false, "Run without TUI (web-only mode)")

	// Read-only diagnostics must not pass through the launcher: they would
	// trip the single-instance lock ("another instance is already running")
	// whenever the daemon is up, and pointlessly spawn a supervised child
	// when it isn't.
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-version", "--version":
			fmt.Printf("moombox %s (%s)\n", version, commit)
			return
		case "-h", "-help", "--help":
			flag.Usage()
			return
		}
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
	startPprofIfRequested()

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

	// Remaining aliases the not-yet-extracted startup / ticker code still uses.
	// Everything not accessed outside of a *runState method lives on s directly.
	var (
		cfg               = s.cfg
		log               = s.log
		upd               = s.upd
		notifyMgr         = s.notifyMgr
		dlWorker          = s.dlWorker
		feedMon           = s.feedMon
		decapiMon         = s.decapiMon
		twitchMon         = s.twitchMon
		cookieRefresh     = s.cookieRefresh
		autoCookieSvc     = s.autoCookieSvc
		browserProfileDir = s.browserProfileDir
		webServer         = s.webServer
		wsHub             = s.wsHub
		tuiUpdateStatusCh = s.tuiUpdateStatusCh
		tuiDiskStatusCh   = s.tuiDiskStatusCh
	)

	// Register all routes. See routes_wiring.go.
	importCleanup := s.wireRoutes()
	defer importCleanup()

	// WebSocket wiring: upgrade handler, AuthMiddleware client-token fallback,
	// upgrade-time WS auth check, InitialState, OpenBrowser + static files.
	// See ws_wiring.go.
	s.wireWebSocket()

	// Cookie-recovery, monitor callbacks, DB/log subscribers, connectivity
	// wiring — see monitor_callbacks.go.
	s.wireMonitorCallbacks()

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
	webBindFailed := false
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
			webBindFailed = true
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

	// Expose actual bound port for TUI and other components (matches TS:
	// process.env.MOOMBOX_PORT). When the user configured `network.port =
	// 0` (auto-pick), persist the OS-assigned port back to disk so the
	// next launch reuses it (predictable port across restarts; users can
	// discover the port from the config file). Audit cmd-moombox.md Q2.
	if actualPort := webServer.ActualPort; actualPort > 0 {
		var configuredPort int
		s.configStore.Read(func(c *config.MoomboxConfig) {
			configuredPort = c.Network.Port
		})
		mu := s.configStore.RWMutex()
		mu.Lock()
		cfg.Network.Port = actualPort
		mu.Unlock()
		// Only write back when the user requested auto-pick (0). Don't
		// rewrite a configured fixed port the user explicitly set.
		if configuredPort == 0 {
			if err := s.configStore.SaveLocked(); err != nil {
				log.Warn("could not persist actualPort to config",
					slog.Int("port", actualPort), slog.String("err", err.Error()))
			} else {
				log.Info("persisted auto-assigned port to config",
					slog.Int("port", actualPort))
			}
		}
	}

	// First-successful-boot milestone: the database opened (initServices)
	// and the web-server bind resolved above — this binary has proven it
	// can run, so NOW sweep the previous version's .old binary. Doing this
	// at the top of initServices deleted the only rollback artifact before
	// a boot-crashing update could fail (updater review 2026-07, A1). In
	// headless mode a bind failure exits above, preserving .old; TUI mode
	// continues without the web server by design, which still counts as a
	// healthy boot.
	if upd != nil {
		upd.CleanupOldBinary()
	}

	// Initial disk space check (populate immediately so status bar has data).
	var initialOutputDir string
	s.configStore.Read(func(c *config.MoomboxConfig) {
		initialOutputDir = c.Paths.OutputDirectory
	})
	routes.UpdateDiskStatus(initialOutputDir, s.configStore)

	// Update-applied detection: if this boot runs a different version than
	// the previous run (self-update restart OR manual binary swap), confirm
	// it — the update_available embed otherwise never gets a "landed
	// successfully" counterpart, and a boot-time check also covers applies
	// the process didn't survive to report. LastRunVersion == "" means
	// pre-feature config or first run: persist silently, don't announce.
	//
	// Gated on ConfigLoaded (mirrors the NeedsAutoPersist gate in
	// services.go): on a true first run the config file doesn't exist yet,
	// and writing one here would make the NEXT boot see ConfigLoaded=true —
	// permanently skipping the setup wizard. The version stamp simply waits
	// until the boot after setup has created the file.
	var lastRunVersion string
	var configLoaded bool
	s.configStore.Read(func(c *config.MoomboxConfig) {
		lastRunVersion = c.Updates.LastRunVersion
		configLoaded = c.ConfigLoaded
	})
	if configLoaded && lastRunVersion != version {
		if lastRunVersion != "" {
			// Reflect whether the dashboard actually came back: in TUI mode
			// a failed web bind logs-and-continues above, and "restarted
			// successfully" on the operator's phone while the dashboard is
			// dead would be a lie. FAILED only on a CONFIRMED bind error —
			// a slow bind (>500ms select timeout, e.g. AV-scanned first
			// boot) leaves ActualPort briefly 0, and claiming failure then
			// would be a false alarm on exactly the boots that matter.
			fields := []notifications.Field{
				{Name: "Previous Version", Value: "v" + lastRunVersion, Inline: true},
				{Name: "Current Version", Value: "v" + version, Inline: true},
			}
			ntype := notifications.TypeSuccess
			desc := fmt.Sprintf("Moombox updated from v%s to v%s and restarted successfully", lastRunVersion, version)
			switch {
			case webBindFailed:
				fields = append(fields, notifications.Field{Name: "Web Dashboard", Value: "FAILED — check logs", Inline: true})
				ntype = notifications.TypeWarning
				desc = fmt.Sprintf("Moombox updated from v%s to v%s and restarted — but the web dashboard failed to start", lastRunVersion, version)
			case webServer.ActualPort > 0:
				fields = append(fields, notifications.Field{Name: "Web Dashboard", Value: "OK", Inline: true})
			}
			notifyMgr.Send("Update Applied", desc, ntype, fields,
				notifications.SendOptions{Event: "update_applied"},
			)
		}
		if err := s.configStore.Update(func(c *config.MoomboxConfig) {
			c.Updates.LastRunVersion = version
		}); err != nil {
			log.Warn("failed to persist last-run version", slog.String("error", err.Error()))
		}
	}

	// Crash-recovery notice: the launcher sets _MOOMBOX_CRASH_RESPAWN when
	// this boot exists because the previous process died abnormally and
	// supervision respawned it. Tell the operator — an unattended recorder
	// that silently crash-cycles hides a problem that needs a look.
	if respawnCode := os.Getenv("_MOOMBOX_CRASH_RESPAWN"); respawnCode != "" {
		log.Error("moombox was restarted by the launcher after an abnormal exit",
			slog.String("previousExitCode", respawnCode))
		notifyMgr.Send("Recovered From Crash",
			fmt.Sprintf("Moombox exited abnormally (code %s) and was automatically restarted — check the logs for the cause", respawnCode),
			notifications.TypeWarning, nil,
			notifications.SendOptions{Event: "crash_recovered"},
		)
	}

	// Failed-update marker sweep: both breadcrumbs were previously
	// WRITE-ONLY — ApplyUpdate's catastrophic .update-broken marker even
	// documents "so the launcher / next run has a clear breadcrumb", but
	// nothing ever read either. Runs regardless of configLoaded (the
	// marker's relevance is independent of setup-wizard state); the
	// dedupe stamp only persists once a config file exists.
	if exeSelf, exeErr := os.Executable(); exeErr == nil {
		var markerSeen string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			markerSeen = c.Updates.LastFailureMarkerSeen
		})
		// The stamp field holds ALL currently-present markers ("a;b") —
		// a single-marker stamp would alternate re-announcements forever
		// when both markers exist (each boot sees the other as unseen).
		var stamps []string
		for _, marker := range []string{exeSelf + ".update-broken", exeSelf + ".update-failed"} {
			fi, statErr := os.Stat(marker)
			if statErr != nil {
				continue
			}
			stamp := filepath.Base(marker) + "|" + fmt.Sprint(fi.ModTime().Unix())
			stamps = append(stamps, stamp)
			if strings.Contains(markerSeen, stamp) {
				continue // already announced this exact failure
			}
			content, _ := os.ReadFile(marker)
			if len(content) > 600 {
				content = content[:600] // markers are ASCII; plain byte cut is safe
			}
			log.Error("previous self-update failure marker present — manual attention needed",
				slog.String("marker", marker), slog.String("details", string(content)))
			notifyMgr.Send("Previous Update Failed",
				"A marker from a failed self-update is present — verify the running binary, then delete the marker file",
				notifications.TypeError,
				[]notifications.Field{
					{Name: "Marker", Value: marker},
					{Name: "Details", Value: string(content)},
				},
				notifications.SendOptions{Event: "update_failed"},
			)
		}
		if combined := strings.Join(stamps, ";"); configLoaded && combined != markerSeen {
			if err := s.configStore.Update(func(c *config.MoomboxConfig) {
				c.Updates.LastFailureMarkerSeen = combined
			}); err != nil {
				log.Warn("failed to persist failure-marker stamp", slog.String("error", err.Error()))
			}
		}

		// .update-pending breadcrumb: ApplyUpdate wrote the tag it updated
		// TO right before restarting. Resolve it now, ahead of the auto-check
		// goroutine below. Our own version means the update landed — just
		// remove it. A DIFFERENT version alongside a failure marker means the
		// launcher auto-rolled back to this binary: mark that tag skipped so
		// automatic checks stop offering a release that just proved broken (a
		// manual "Check for updates" still retries it deliberately). A
		// different version with NO marker (manual binary swap, marker
		// deleted early) is treated conservatively: delete without skipping.
		pendingPath := exeSelf + updater.PendingVersionSuffix
		if raw, readErr := os.ReadFile(pendingPath); readErr == nil {
			pendingTag := strings.TrimSpace(string(raw))
			if shouldSkipPendingVersion(pendingTag, version, len(stamps) > 0) && configLoaded {
				if err := s.configStore.Update(func(c *config.MoomboxConfig) {
					c.Updates.SkippedVersion = pendingTag
				}); err != nil {
					log.Warn("failed to persist skipped version after rollback",
						slog.String("version", pendingTag), slog.String("error", err.Error()))
				} else {
					log.Warn("failed update rolled back — version marked skipped for automatic checks",
						slog.String("version", pendingTag))
				}
			}
			if rmErr := os.Remove(pendingPath); rmErr != nil {
				log.Warn("failed to remove pending-version breadcrumb",
					slog.String("path", pendingPath), slog.String("error", rmErr.Error()))
			}
		}
	}

	// Auto-update check: initial check + daily ticker. The goroutine runs
	// whenever the updater exists and re-reads auto_check_updates on EVERY
	// iteration — previously the flag was consulted once at boot, so
	// disabling checks (the dismiss route / settings toggle) couldn't stop
	// an armed ticker until restart, and enabling the toggle did nothing
	// without one.
	if upd != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("auto-update check panic", "panic", r)
				}
			}()
			checkEnabled := func() bool {
				var on bool
				s.configStore.Read(func(c *config.MoomboxConfig) {
					on = c.Updates.AutoCheckUpdates
				})
				return on
			}
			// One notification per pending version, not one per tick —
			// state lives here because this goroutine is the only caller.
			var lastNotifiedTag string

			// Initial check (slight delay to avoid slowing startup)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			if checkEnabled() {
				checkAndBroadcastUpdate(ctx, upd, wsHub, notifyMgr, tuiUpdateStatusCh, log, s.configStore, &lastNotifiedTag)
			}

			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if checkEnabled() {
						checkAndBroadcastUpdate(ctx, upd, wsHub, notifyMgr, tuiUpdateStatusCh, log, s.configStore, &lastNotifiedTag)
					}
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
		diskReadFailing := false
		// Consecutive read failures. The low-disk safety net silently dying
		// (volume offline, I/O error) is itself alert-worthy — notified on
		// the 2nd consecutive failure (~12 min) so a transient SMB blip or
		// flapping network volume doesn't page the operator.
		diskReadFailCount := 0
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
				// Build the base Go-runtime line.
				line := fmt.Sprintf(
					"[Memory] Sys: %.1fMB, Heap: %.1f%s/%.1fMB, Stack: %.1fMB, GC: %d",
					float64(m.Sys)/1048576,
					heapMB, delta,
					float64(m.HeapSys)/1048576,
					float64(m.StackInuse)/1048576,
					m.NumGC,
				)

				// Append sidecar memory if it's running and reachable. Brief
				// 1s timeout — if the sidecar is hung or dead we don't want
				// the every-2-minutes memory log to wedge on it. When the
				// sidecar is configured but unreachable, append a marker so
				// the log distinguishes "sidecar disabled" (no suffix) from
				// "sidecar present but not responding" (suffix says so).
				if s.bgSidecar != nil && s.bgSidecar.IsHealthy() {
					sideCtx, sideCancel := context.WithTimeout(ctx, 1*time.Second)
					sideStats, sideErr := s.bgSidecar.MemoryStats(sideCtx)
					sideCancel()
					if sideErr == nil {
						line += fmt.Sprintf(
							" | Sidecar: RSS %.1fMB (V8 Heap %.1f/%.1fMB)",
							float64(sideStats.RSS)/1048576,
							float64(sideStats.HeapUsed)/1048576,
							float64(sideStats.HeapTotal)/1048576,
						)
						// Soft sidecar memory limit: V8 has no native soft
						// cap, so when RSS exceeds the configured threshold
						// we ask the sidecar to run global.gc(). Combined
						// with --max-old-space-size as a hard ceiling, this
						// gives soft-limit semantics without OOM aborts.
						var sidecarSoftMB int
						s.configStore.Read(func(c *config.MoomboxConfig) {
							sidecarSoftMB = c.Memory.SidecarSoftLimitMB
						})
						if sidecarSoftMB > 0 && sideStats.RSS >= int64(sidecarSoftMB)<<20 {
							gcCtx, gcCancel := context.WithTimeout(ctx, 5*time.Second)
							gcResult, gcErr := s.bgSidecar.TriggerGC(gcCtx)
							gcCancel()
							if gcErr != nil {
								log.Debug("[Memory] sidecar TriggerGC failed", slog.String("err", gcErr.Error()))
							} else {
								reclaimed := gcResult.Before.RSS - gcResult.After.RSS
								log.Info("[Memory] sidecar soft limit hit; ran GC",
									slog.Int("threshold_mb", sidecarSoftMB),
									slog.Float64("reclaimed_mb", float64(reclaimed)/1048576),
								)
							}
						}
					} else {
						line += " | Sidecar: stats unavailable"
					}
				}

				log.Debug(line)

				// Disk space check every 3rd tick (~6 minutes at the 2min ticker
				// interval — close enough to the 5-min target while reusing the
				// existing ticker).
				diskCheckCounter++
				if diskCheckCounter%3 == 0 { // every 3 ticks = ~6 minutes
					var diskOutputDir string
					s.configStore.Read(func(c *config.MoomboxConfig) {
						diskOutputDir = c.Paths.OutputDirectory
					})
					if ds := routes.UpdateDiskStatus(diskOutputDir, s.configStore); ds != nil {
						diskReadFailing = false
						diskReadFailCount = 0
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
								// Critical gets its own event so targets can route
								// it separately (e.g. a high-priority channel);
								// eventAliases keeps plain "disk_warning" filters
								// receiving it too.
								event := "disk_warning"
								if ds.WarnLevel == "critical" {
									level = "Critical"
									ntype = notifications.TypeError
									event = "disk_critical"
								}
								// Name the directory: an operator with several
								// machines (or several volumes) can't act on
								// "output drive" alone. Absolute path — the
								// config default is the relative "./output".
								dirShown := diskOutputDir
								if abs, absErr := filepath.Abs(diskOutputDir); absErr == nil {
									dirShown = abs
								}
								notifyMgr.Send(
									fmt.Sprintf("Disk Space %s", level),
									fmt.Sprintf("%.1f%% used — %.1f GB free on output drive", ds.UsedPct, freeGB),
									ntype,
									[]notifications.Field{
										{Name: "Output Directory", Value: dirShown},
									},
									notifications.SendOptions{Event: event},
								)
								lastDiskNotify = time.Now()
								lastDiskLevel = ds.WarnLevel
							}
						} else {
							lastDiskLevel = "" // Reset cooldown when back to ok
						}
					} else {
						// GetDiskSpace failed (volume offline, I/O error). Warn
						// once per failure streak — until this recovers, the
						// dashboard disk gauge and low-disk notifications are
						// frozen at the last good reading.
						diskReadFailCount++
						if !diskReadFailing {
							log.Warn("[Disk] disk space check failed; gauge and low-disk alerts frozen until it recovers",
								slog.String("outputDir", diskOutputDir))
							diskReadFailing = true
						}
						// The safety net dying is itself alert-worthy: with
						// monitoring frozen, the drive can fill unnoticed.
						// Fires once per streak, on the 2nd consecutive
						// failure (~12 min) to ride out transient blips.
						if diskReadFailCount == 2 {
							notifyMgr.Send("Disk Monitoring Failed",
								"Disk space checks are failing (volume offline or I/O error) — low-disk alerts are suspended until monitoring recovers",
								notifications.TypeError,
								[]notifications.Field{
									{Name: "Output Directory", Value: diskOutputDir},
								},
								notifications.SendOptions{Event: "disk_warning"},
							)
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
		s.runTUI()
	}

	return s.shutdown()
}

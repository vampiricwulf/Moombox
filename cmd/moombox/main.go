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
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

var (
	version = "2.6.0-test.31"
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

	// Initial disk space check (populate immediately so status bar has data).
	var initialOutputDir string
	s.configStore.Read(func(c *config.MoomboxConfig) {
		initialOutputDir = c.Paths.OutputDirectory
	})
	routes.UpdateDiskStatus(initialOutputDir, s.configStore)

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
					var diskOutputDir string
					s.configStore.Read(func(c *config.MoomboxConfig) {
						diskOutputDir = c.Paths.OutputDirectory
					})
					if ds := routes.UpdateDiskStatus(diskOutputDir, s.configStore); ds != nil {
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
		s.runTUI()
	}

	return s.shutdown()
}

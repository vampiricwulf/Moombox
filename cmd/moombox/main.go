package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	isatty "github.com/mattn/go-isatty"
	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
	"github.com/vampiricwulf/Moombox/internal/worker"
	"github.com/vampiricwulf/Moombox/internal/youtube"
	webpublic "github.com/vampiricwulf/Moombox/web"
)

var (
	version = "2.5.2"
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

var noTUIEnvRe = regexp.MustCompile(`^(?i:1|true|yes)$`)

// waitForKeypress waits for a keypress before exiting (prevents .exe window from vanishing).
// Matches TS waitForKeypress() in index.ts — only blocks on a TTY.
func waitForKeypress() {
	fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	reader.ReadByte()
}

func main() {
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

	// Check for subcommands before flag parsing
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
	flag.Parse()

	if *showVersion {
		fmt.Printf("moombox %s (%s)\n", version, commit)
		os.Exit(0)
	}

	// TTY detection: only use TUI if both stdin/stdout are terminals
	isTTY := isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
	envNoTUI := noTUIEnvRe.MatchString(strings.TrimSpace(os.Getenv("MOOMBOX_NO_TUI")))
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
func run(configPath string, logLevelOverride string, useTUI bool) (restart bool) {
	var restartRequested atomic.Bool
	var quitTUI func() // set when TUI is running; called on restart to unblock Run()

	if !useTUI {
		fmt.Println("Loading configuration...")
	}

	// Graceful shutdown context
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// =========================================================================
	// 1. Load config
	// =========================================================================
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Startup error: Failed to load config: %v\n", err)
		waitForKeypress()
		os.Exit(1)
	}

	if logLevelOverride != "" {
		cfg.Logs.LogLevel = logLevelOverride
	}

	// =========================================================================
	// 2. Initialize logger
	// =========================================================================
	log, err := logger.New(cfg.Paths.LogFilePath, cfg.Logs.LogLevel, cfg.Logs.LogMaxFileSize, cfg.Logs.LogMaxFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Startup error: Failed to initialize logger: %v\n", err)
		waitForKeypress()
		os.Exit(1)
	}
	defer log.Close()

	log.Info("Starting Moombox", slog.String("version", version), slog.String("commit", commit))

	// Updater: create instance and clean up .old binary from previous update
	upd, updErr := updater.New(version, log)
	if updErr != nil {
		log.Warn("Updater unavailable", slog.String("error", updErr.Error()))
	} else {
		upd.CleanupOldBinary()
	}

	// Auto-convert plaintext password to scrypt hash (matches TS ConfigManager.load)
	if cfg.Network.PasswordHash != "" && !web.IsScryptHash(cfg.Network.PasswordHash) {
		log.Info("[Config] Plaintext password detected, converting to secure hash")
		tempAuth := web.NewAuthService()
		hash, err := tempAuth.HashPassword(cfg.Network.PasswordHash)
		if err == nil {
			cfg.Network.PasswordHash = hash
			if saveErr := config.Save(cfg, configPath); saveErr != nil {
				log.Warn("Failed to save auto-hashed password", slog.String("error", saveErr.Error()))
			}
		} else {
			log.Error("Failed to hash plaintext password", slog.String("error", err.Error()))
		}
	}

	// =========================================================================
	// 3. Open database
	// =========================================================================
	if !useTUI {
		fmt.Println("Initializing database...")
	}
	db, err := database.Open(cfg.Paths.DatabasePath, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Startup error: Failed to open database: %v\n", err)
		waitForKeypress()
		os.Exit(1)
	}
	// Database is closed explicitly in the shutdown sequence below (stopService "Database").
	// No defer db.Close() here to avoid double-close.

	// =========================================================================
	// 3b. Connectivity monitor
	// =========================================================================
	connMon := connectivity.NewMonitor(log)
	connMon.Start(ctx)
	defer connMon.Stop()
	utils.SetConnectivityReporter(connMon)
	engine.SetConnectivityReporter(connMon)

	// =========================================================================
	// 4. Load cookies
	// =========================================================================
	if !useTUI {
		fmt.Println("Starting services...")
	}
	jar := cookies.NewCookieJar()
	if cfg.Cookies.CookieFile != "" {
		if err := jar.Load(cfg.Cookies.CookieFile); err != nil {
			log.Warn("Failed to load cookies", slog.String("error", err.Error()))
		} else {
			log.Info("Cookies loaded", slog.Bool("hasAuth", jar.HasYouTubeAuthCookies()))
			// Auto-detect platforms from cookie file when not already set
			if len(cfg.Cookies.Platforms) == 0 && len(cfg.Cookies.ActivePlatforms) == 0 {
				var detected []string
				if jar.HasYouTubeAuthCookies() {
					detected = append(detected, "youtube")
				}
				if jar.HasTwitchAuthCookies() {
					detected = append(detected, "twitch")
				}
				if len(detected) > 0 {
					cfg.Cookies.Platforms = detected
					if err := config.Save(cfg, configPath); err != nil {
						log.Warn("Failed to persist detected cookie platforms", slog.String("error", err.Error()))
					} else {
						log.Info("Detected cookie platforms from cookie file", slog.Any("platforms", detected))
					}
				}
			}
		}
	}

	// =========================================================================
	// 5. YouTube service
	// =========================================================================
	ytService := youtube.NewService(jar, log)
	ytService.Init(ctx) // Fetch homepage for visitor data and API key

	// =========================================================================
	// 6. Twitch service
	// =========================================================================
	twService := twitch.NewService(jar, log)
	// Log auth status at startup (matches TS TwitchService.init())
	if twService.Auth.HasAuthToken() {
		log.Info("[Twitch] Authenticated (auth-token found in cookies)")
	} else {
		log.Debug("[Twitch] No auth-token found, using anonymous access")
	}

	// =========================================================================
	// 7. PO Token provider
	// =========================================================================
	potProvider := bgutils.NewPotProvider(&bgutils.BgConfig{}, log)

	// =========================================================================
	// 8. Cipher solver
	// =========================================================================
	cacheDir := filepath.Join(os.TempDir(), "yt-cipher")
	cipherSolver, err := cipher.NewSolver(cacheDir, log)
	if err != nil {
		log.Warn("Failed to init cipher solver, will retry on demand", slog.String("error", err.Error()))
	}

	// Wire cipher solver to YouTube service for format decryption
	if cipherSolver != nil {
		ytService.PlayerAPI.SetCipherSolver(cipherSolver)
	}

	// Wire PO token provider into Innertube player requests (audit youtube.md C1).
	ytService.PlayerAPI.SetPotProvider(potProvider)

	// =========================================================================
	// 9. Notification manager
	// =========================================================================
	notifyMgr := notifications.NewManager(cfg, log)

	// =========================================================================
	// 10. Download worker
	// =========================================================================
	dlWorker := worker.NewDownloadWorker(db, ytService, cfg, log, &worker.DownloadWorkerDeps{
		CipherSolver:         cipherSolver,
		PotProvider:          potProvider,
		TwitchService:        twService,
		Notifier:             notifyMgr,
		IsOnline:             connMon.IsOnline,
		OnConnectivityChange: connMon.OnStateChange,
	})

	// =========================================================================
	// 11. Trim service
	// =========================================================================
	trimSvc := worker.NewTrimService(db, cfg.Paths.FfmpegPath, log)
	if notifyMgr != nil {
		trimSvc.SetNotifier(notifyMgr)
	}

	// =========================================================================
	// 12. Feed monitor (YouTube RSS)
	// =========================================================================
	feedMon := monitor.NewFeedMonitor(cfg, db, log)

	// =========================================================================
	// 13. DECAPI monitor
	// =========================================================================
	decapiMon := monitor.NewDecapiMonitor(cfg, db, log)

	// =========================================================================
	// 14. Twitch monitor
	// =========================================================================
	twitchMon := monitor.NewTwitchMonitor(cfg, db, twService, log)

	// Wire connectivity to monitors so they skip polls when offline
	feedMon.IsOnline = connMon.IsOnline
	decapiMon.IsOnline = connMon.IsOnline
	twitchMon.IsOnline = connMon.IsOnline

	// =========================================================================
	// 15. Cookie refresh service
	// =========================================================================
	cookieRefresh := cookies.NewRefreshService(jar, 0, log)

	// =========================================================================
	// 15b. Auto-cookie service
	// =========================================================================
	browserProfileDir := cfg.Cookies.BrowserProfileDir
	if browserProfileDir == "" {
		browserProfileDir = "./browser-profile"
	}
	autoCookieSvc := cookies.NewAutoCookieService(
		browserProfileDir,
		cfg.Cookies.CookieFile,
		jar,
		log,
	)
	// Wire auth verification callbacks so AutoCookieService can verify via real API
	autoCookieSvc.VerifyYouTubeAuth = cookieRefresh.CheckYouTubeAuth
	autoCookieSvc.VerifyTwitchAuth = cookieRefresh.CheckTwitchAuth
	// Wire persistPlatforms callback: saves verified platforms to config
	// so we can detect auth loss after restart (matches TS persistPlatforms)
	var cfgMu sync.RWMutex
	dlWorker.SetCfgMu(&cfgMu)
	autoCookieSvc.PersistPlatforms = func(youtubeVerified, twitchVerified bool) {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		// During first-run setup, the config file doesn't exist yet. Don't
		// create it prematurely — the setup wizard's POST /api/setup/complete
		// will save everything (including platforms) when the user finishes.
		if !cfg.ConfigLoaded {
			return
		}
		existing := make(map[string]bool)
		for _, p := range cfg.Cookies.Platforms {
			existing[p] = true
		}
		if youtubeVerified {
			existing["youtube"] = true
		}
		if twitchVerified {
			existing["twitch"] = true
		}
		platforms := make([]string, 0, len(existing))
		for p := range existing {
			platforms = append(platforms, p)
		}
		slices.Sort(platforms)
		cfg.Cookies.Platforms = platforms
		if err := config.Save(cfg, configPath); err != nil {
			log.Warn("Failed to persist auto-cookie platforms", slog.String("error", err.Error()))
		} else {
			log.Debug("Persisted auto-cookie platforms", slog.Any("platforms", platforms))
		}
	}

	// Wire auto-cookie refresh into download worker (attempts refresh on auth failure)
	dlWorker.OnCookieRefreshNeeded = func() bool {
		cfgMu.RLock()
		autoEnabled := cfg.Cookies.AutoEnabled
		cfgMu.RUnlock()
		if !autoEnabled {
			return false
		}
		refreshCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ok, err := autoCookieSvc.RefreshCookies(refreshCtx)
		if err != nil {
			log.Warn("auto cookie refresh error", slog.String("error", err.Error()))
			return false
		}
		return ok
	}

	// =========================================================================
	// 16. Web server
	// =========================================================================
	startTime := time.Now()
	webServer := web.NewServer(cfg, &cfgMu, log)
	webServer.SetCommit(commit)
	wsHub := webServer.WebSocket()
	r := webServer.Router()

	// Rate limiters
	apiRL := web.NewRateLimiter(20, time.Minute)
	defer apiRL.Close()
	potRL := web.NewRateLimiter(10, time.Minute)
	defer potRL.Close()

	// Auth service
	authSvc := web.NewAuthService()
	authSvc.Start()
	defer authSvc.Stop()

	// Auth rate limiters
	loginRL := web.NewRateLimiter(5, time.Minute)
	defer loginRL.Close()
	passwordRL := web.NewRateLimiter(3, time.Minute)
	defer passwordRL.Close()

	// Wire auth middleware for external connections
	webServer.SetAuth(authSvc)
	r.Use(webServer.AuthMiddleware)

	// Shared closure: determines which platforms are active for cookie status display.
	getActivePlatforms := func() map[string]bool {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		yt, tw := config.GetActivePlatforms(cfg)
		return map[string]bool{"youtube": yt, "twitch": tw}
	}

	// Shared callback to kick all monitors when channels change.
	// Wakes monitors that went idle when they had no channels of their type.
	kickMonitors := func() {
		feedMon.CheckNow()
		decapiMon.CheckNow()
		twitchMon.CheckNow()
	}

	// Register all routes
	routes.JobRoutes(r, db, cfg, webServer.CfgMu(), dlWorker, apiRL, &twitchMetadataAdapter{svc: twService}, &youtubeMetadataAdapter{svc: ytService}, notifyMgr, wsHub)
	routes.FormatRoutes(r, &routes.FormatRoutesDeps{
		DB:  db,
		Cfg: cfg,
		YT:  &ytFormatAdapter{svc: ytService, cfg: cfg},
	})
	routes.StatusRoute(r, &routes.StatusRouteDeps{
		Cfg:       cfg,
		Version:   version,
		StartTime: startTime,
		GetActivePlatforms: getActivePlatforms,
		GetCookieStatus: func() map[string]any {
			status := cookieRefresh.GetStatus()
			return map[string]any{
				"found":         status.HasYouTubeCookies,
				"authenticated": status.YouTubeAuthenticated,
			}
		},
		GetTwitchAuthStatus: func() map[string]any {
			status := cookieRefresh.GetStatus()
			return map[string]any{
				"authenticated": status.TwitchAuthenticated,
			}
		},
		GetAutoCookieReloginNeeded: func() any {
			if autoCookieSvc != nil {
				return autoCookieSvc.GetStatus().NeedsManualRelogin
			}
			return cookies.AutoCookieReloginRequired{}
		},
		GetNextFeedCheck:   feedMon.GetNextCheckAt,
		GetNextDecapiCheck: decapiMon.GetNextCheckAt,
		GetNextTwitchCheck: twitchMon.GetNextCheckAt,
	})
	routes.ConfigRoutes(r, cfg, webServer.CfgMu(), func(c *config.MoomboxConfig) error {
		return config.Save(c, configPath)
	}, &routes.ConfigRoutesCallbacks{
		OnLogLevelChange: func(level string) {
			log.SetLevel(level)
		},
		OnMaxParallelChange: func(n int) {
			dlWorker.SetParallelDownloads(n)
		},
		OnHideFinishedAgeChanged: func() {
			// Re-broadcast job list with updated archive threshold
			jobs, _ := db.GetAllJobs()
			wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, cfg, webServer.CfgMu()))
		},
		OnChannelChange: kickMonitors,
	})
	routes.ChannelRoutes(r, cfg, webServer.CfgMu(), func(c *config.MoomboxConfig) error {
		return config.Save(c, configPath)
	}, kickMonitors)
	routes.FileRoutes(r, &routes.FileRoutesDeps{
		DB:     db,
		Cfg:    cfg,
		Logger: log,
	})
	routes.TrimRoutes(r, db, trimSvc)
	routes.StatsRoutes(r, &routes.StatsRouteDeps{
		DB:  db,
		Cfg: cfg,
	})
	routes.PotRoutes(r, &routes.PotRoutesDeps{
		PotProvider: potProvider,
		StartTime:   startTime,
		RateLimit:   potRL,
		Logger:      log,
	})
	triggerRestart := func(source string) {
		log.Info("Restart requested", slog.String("source", source))
		restartRequested.Store(true)
		cancel()
		if quitTUI != nil {
			quitTUI()
		}
	}
	routes.SetupRoutes(r, &routes.SetupDeps{
		Cfg:  cfg,
		Auth: authSvc,
		SaveConfig: func(c *config.MoomboxConfig) error {
			return config.Save(c, configPath)
		},
		OnInstallYtdlp: func(port int, httpsEnabled bool) {
			if err := routes.InstallYtdlpPlugin(port, httpsEnabled); err != nil {
				log.Error("Failed to install yt-dlp plugin from setup", slog.String("error", err.Error()))
			} else {
				log.Info("yt-dlp plugin installed from setup wizard", slog.Int("port", port))
			}
		},
		OnRestart: func() { triggerRestart("setup") },
	}, webServer.CfgMu())
	routes.FFmpegRoutes(r, &routes.FFmpegDeps{
		Cfg:   cfg,
		CfgMu: webServer.CfgMu(),
		SaveConfig: func(c *config.MoomboxConfig) error {
			return config.Save(c, configPath)
		},
		RateLimit: apiRL,
		Logger:    log,
	})
	routes.LogRoutes(r, log.GetRecentLines)
	importCleanup := routes.ImportRoutes(r, db, cfg, webServer.CfgMu(), apiRL)
	defer importCleanup()
	routes.CookieRoutes(r, cookieRefresh, autoCookieSvc, getActivePlatforms)
	routes.YtdlpRoutes(r, cfg.Network.Port, cfg.Network.HTTPSEnabled)
	routes.RestartRoute(r, func() { triggerRestart("API") })
	// TUI update status channel (created here so the OnFound closure can reference it)
	tuiUpdateStatusCh := make(chan tui.UpdateStatusMsg, 2)
	routes.UpdateRoutes(r, &routes.UpdateRouteDeps{
		Updater:    upd,
		Version:    version,
		Cfg:        cfg,
		ConfigPath: configPath,
		OnRestart:  func() { triggerRestart("update") },
		OnFound: func(release *updater.ReleaseInfo) {
			wsHub.Broadcast("update_available", release)
			select {
			case tuiUpdateStatusCh <- tui.UpdateStatusMsg{
				Version:      release.Version,
				TagName:      release.TagName,
				ReleaseNotes: release.ReleaseNotes,
			}:
			default:
			}
		},
	}, webServer.CfgMu())
	authDeps := &routes.AuthRoutesDeps{
		Cfg:        cfg,
		Auth:       authSvc,
		DB:         db,
		LoginRL:    loginRL,
		PasswordRL: passwordRL,
		SaveConfig: func(c *config.MoomboxConfig) error {
			return config.Save(c, configPath)
		},
		Logger: log,
	}
	routes.AuthRoutes(r, authDeps, webServer.CfgMu())
	routes.ClientTokenRoutes(r, authDeps)
	routes.WatchRoutes(r, db)

	// WebSocket upgrade handler — register on the router before static file mounting.
	// TS uses noServer mode which upgrades on any path; frontend connects to ws://host/ (root).
	webServer.SetWebSocketHandler(wsHub.HandleUpgrade)

	// Wire persistent client token check for AuthMiddleware fallback
	webServer.ClientTokenCheck = func(rawToken, ip string) (bool, string) {
		prefix := web.TokenPrefix(rawToken)
		ct, err := db.GetClientTokenByPrefix(prefix)
		if err != nil || ct == nil {
			return false, ""
		}
		if !web.VerifyToken(rawToken, ct.TokenHash) {
			return false, ""
		}
		sessionToken, err := authSvc.CreateSession()
		if err != nil {
			return false, ""
		}
		// Fire-and-forget usage update
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("client token usage update panic", "panic", r)
				}
			}()
			db.UpdateClientTokenUsage(ct.ID, ip)
		}()
		return true, sessionToken
	}

	// Wire WebSocket auth check for external connections
	wsHub.AuthCheck = func(r *http.Request) bool {
		cfgMu.RLock()
		networkAccess := cfg.Network.NetworkAccess
		passwordHash := cfg.Network.PasswordHash
		cfgMu.RUnlock()
		if !web.IsAuthRequired(networkAccess, passwordHash) {
			return true
		}
		// Check session cookie
		if cookie, err := r.Cookie("moombox_session"); err == nil {
			if authSvc.ValidateSession(cookie.Value) {
				return true
			}
		}
		// Fallback: check persistent client token (can't set cookies on WS upgrade, just allow)
		if cookie, err := r.Cookie("moombox_client"); err == nil && cookie.Value != "" {
			prefix := web.TokenPrefix(cookie.Value)
			if ct, err := db.GetClientTokenByPrefix(prefix); err == nil && ct != nil {
				if web.VerifyToken(cookie.Value, ct.TokenHash) {
					go func() {
						defer func() {
							if r := recover(); r != nil {
								log.Error("client token usage update panic", "panic", r)
							}
						}()
						db.UpdateClientTokenUsage(ct.ID, extractWSIP(r))
					}()
					return true
				}
			}
		}
		return false
	}

	// Wire initial state provider for WebSocket connections
	wsHub.InitialState = func() map[string]any {
		jobs, err := db.GetAllJobs()
		if err != nil {
			jobs = []*database.Job{} // Send empty array, not null
		}
		jobs = filterJobsByAge(jobs, cfg, webServer.CfgMu())
		return map[string]any{
			"jobs":             jobs,
			"logs":             log.GetRecentLines(),
			"nextFeedCheck":    feedMon.GetNextCheckAt(),
			"nextDecapiCheck":  decapiMon.GetNextCheckAt(),
			"nextTwitchCheck":  twitchMon.GetNextCheckAt(),
			"connectivity":    connMon.IsOnline(),
		}
	}

	// Open browser to dashboard URL on start (matches TS openBrowser = true default)
	webServer.OpenBrowser = true

	// Serve embedded static files (web dashboard) with SPA fallback
	staticFS, _ := fs.Sub(webpublic.PublicFS, "public")
	webServer.MountStaticFiles(staticFS)

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
			if notifyMgr != nil {
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
	}

	// =========================================================================
	// Wire up event callbacks
	// =========================================================================

	// Wire ProbeVideo callback for monitors (metadata check before job creation).
	// Returns VideoProbeResult with stream status so monitors can skip non-streams.
	probeVideoFunc := func(videoID string) (*monitor.VideoProbeResult, error) {
		ctx := context.Background()
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

		// Determine output directory (channel-specific > global > default)
		outputDir := ch.OutputDirectory
		if outputDir == "" {
			cfgMu.RLock()
			outputDir = cfg.Paths.OutputDirectory
			cfgMu.RUnlock()
		}

		thumbnailURL := fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", videoID)

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
					Thumbnail: fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", videoID),
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

		// Determine output directory
		outputDir := ch.OutputDirectory
		if outputDir == "" {
			cfgMu.RLock()
			outputDir = cfg.Paths.OutputDirectory
			cfgMu.RUnlock()
		}

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
	feedMon.OnSchedule = func(_ int64) { broadcastAllTimers() }
	decapiMon.OnSchedule = func(_ int64) { broadcastAllTimers() }
	twitchMon.OnSchedule = func(_ int64) { broadcastAllTimers() }

	// Initialize per-job log tracking with existing jobs (matches TS knownJobIds)
	if existingJobs, err := db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates
	unsubWSJobUpdate := db.OnJobUpdate(func(job *database.Job) {
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
	unsubWSJobsChange := db.OnJobsChange(func(jobs []*database.Job) {
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
	logSub := log.Subscribe()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("log forwarder panic", "panic", r)
			}
		}()
		for line := range logSub {
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
			interval := time.Duration(cfg.Cookies.RefreshInterval.Minutes()) * time.Minute
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
				webErrCh <- fmt.Errorf("web server panic: %v", r)
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

	// TUI disk status channel (created early so the goroutine can reference it;
	// only receives messages when TUI mode is active).
	tuiDiskStatusCh := make(chan tui.DiskStatusMsg, 5)

	// tuiUpdateStatusCh created earlier (before route registration)

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
		quitTUI = app.QuitTUI // allow API restart to exit TUI

		// Pass config reference, config mutex, and version for settings panel
		app.SetConfig(cfg)
		app.SetCfgMu(&cfgMu)
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
				if autoCookieSvc == nil {
					return fmt.Errorf("auto-cookie service not available")
				}
				if err := autoCookieSvc.StartSetup(platform); err != nil {
					log.Error("Failed to start auto-cookie setup", slog.String("platform", platform), slog.String("error", err.Error()))
					return err
				}
				return nil
			},
			func() (bool, bool, error) {
				if autoCookieSvc != nil {
					finishCtx, finishCancel := context.WithTimeout(ctx, 60*time.Second)
					defer finishCancel()
					yt, tw, err := autoCookieSvc.FinishSetup(finishCtx)
					if err != nil {
						log.Error("Failed to finish auto-cookie setup", slog.String("error", err.Error()))
						return yt, tw, err
					}
					return yt, tw, nil
				}
				return false, false, fmt.Errorf("auto-cookie service not available")
			},
			func() {
				if autoCookieSvc != nil {
					autoCookieSvc.CancelSetup()
				}
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

		// Forward monitor schedule events to TUI
		wrapOnSchedule := func(orig func(int64), makeMsg func(time.Time) tui.CheckTimersMsg) func(int64) {
			return func(nextCheckAt int64) {
				if orig != nil {
					orig(nextCheckAt)
				}
				t := time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
				select {
				case checkTimersCh <- makeMsg(t):
				default:
				}
			}
		}
		feedMon.OnSchedule = wrapOnSchedule(feedMon.OnSchedule, func(t time.Time) tui.CheckTimersMsg {
			return tui.CheckTimersMsg{NextFeedCheck: t}
		})
		decapiMon.OnSchedule = wrapOnSchedule(decapiMon.OnSchedule, func(t time.Time) tui.CheckTimersMsg {
			return tui.CheckTimersMsg{NextDecapiCheck: t}
		})
		twitchMon.OnSchedule = wrapOnSchedule(twitchMon.OnSchedule, func(t time.Time) tui.CheckTimersMsg {
			return tui.CheckTimersMsg{NextTwitchCheck: t}
		})

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
			if autoCookieSvc != nil {
				relogin := autoCookieSvc.GetStatus().NeedsManualRelogin
				if relogin.YouTube {
					yt = tui.CookieStatusRelogin
				}
				if relogin.Twitch {
					tw = tui.CookieStatusRelogin
				}
			}
			cfgMu.RLock()
			ytActive, twActive := config.GetActivePlatforms(cfg)
			cfgMu.RUnlock()
			select {
			case cookieStatusCh <- tui.CookieStatusMsg{YT: yt, TW: tw, YTActive: ytActive, TWActive: twActive}:
			default:
			}
		}
		origOnAuthChange := cookieRefresh.OnAuthChange
		cookieRefresh.OnAuthChange = func(s cookies.AuthStatus) {
			if origOnAuthChange != nil {
				origOnAuthChange(s)
			}
			authStatusToTUI(s)
		}
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

	log.Info("Shutdown signal received, shutting down gracefully...")

	// 10-second force-exit timer
	forceExit := time.AfterFunc(10*time.Second, func() {
		log.Error("Graceful shutdown timed out, forcing exit")
		log.Close() // flush buffered logs before force exit
		os.Exit(1)
	})
	defer forceExit.Stop()

	// =========================================================================
	// Shutdown order: consumers first, flush data, infrastructure last
	// Each service stop is isolated (like TS stopService pattern) to prevent
	// one failing service from blocking shutdown of others.
	// =========================================================================

	// stopService stops a named service with panic isolation and logging.
	stopService := func(name string, fn func()) {
		defer func() {
			if r := recover(); r != nil {
				log.Error(fmt.Sprintf("[Moombox] Error stopping %s: %v", name, r))
			}
		}()
		fn()
		log.Debug(fmt.Sprintf("[Moombox] Stopped %s", name))
	}

	// 1. Stop monitors
	stopService("TwitchMonitor", twitchMon.Stop)
	stopService("DecapiMonitor", decapiMon.Stop)
	stopService("FeedMonitor", feedMon.Stop)

	// 2. Stop worker (waits for active downloads to save state)
	stopService("DownloadWorker", dlWorker.Stop)

	// 3. Flush in-flight notifications (may have been fired during worker stop)
	stopService("Notifications", notifyMgr.Wait)

	// 4. Stop cookie refresh and auto-cookie service
	stopService("CookieRefresh", cookieRefresh.Stop)
	stopService("AutoCookies", autoCookieSvc.Stop)

	// 5. Cleanup PO token provider
	stopService("PotProvider", potProvider.Cleanup)

	// 6. Stop web server
	stopService("WebServer", webServer.Stop)

	// 7. Unsubscribe log forwarder and DB event subscribers
	log.Unsubscribe(logSub)
	unsubWSJobUpdate()
	unsubWSJobsChange()

	// 8. Flush database
	stopService("Database", func() { db.Close() })

	if restartRequested.Load() {
		log.Info("Shutdown complete, restarting...")
	} else {
		log.Info("Shutdown complete")
	}

	return restartRequested.Load()
}

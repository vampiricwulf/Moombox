package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"io/fs"
	"net/http"
	"os/exec"

	isatty "github.com/mattn/go-isatty"
	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
	"github.com/vampiricwulf/Moombox/internal/worker"
	"github.com/vampiricwulf/Moombox/internal/youtube"
	webpublic "github.com/vampiricwulf/Moombox/web"
)

var (
	version = "2.1.1"
	commit  = ""

	// updateRestart is set when an update has been applied and the process
	// should exec the new binary instead of re-entering run().
	updateRestart atomic.Bool
)

func init() {
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

	// Resolve config path once before the run loop
	cfgPath := *configPath
	if cfgPath == "" {
		cwd, _ := os.Getwd()
		cfgPath = filepath.Join(cwd, "config.toml")
	}

	// Run loop: restart re-initializes everything within the same process.
	// On restart, run() returns true and the loop starts a fresh iteration.
	for run(cfgPath, *logLevel, useTUI) {
		// After an update, the new binary is on disk but this process is still
		// the old code. Launch the new binary as a separate process and exit.
		if updateRestart.Load() {
			execNewBinary()
			return // execNewBinary calls os.Exit, but return as a safeguard
		}
		if !useTUI {
			fmt.Println("\nRestarting...")
		}
	}
}

// run initializes and runs the full application. Returns true if a restart was
// requested (by TUI settings or web API), in which case the caller should loop.
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
	defer db.Close()

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
			log.Info("Cookies loaded", slog.Bool("hasAuth", jar.HasAuthCookies()))
			// Auto-detect platforms from cookie file when not already set
			if len(cfg.Cookies.Platforms) == 0 && len(cfg.Cookies.ActivePlatforms) == 0 {
				var detected []string
				if jar.HasAuthCookies() {
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

	// =========================================================================
	// 9. Notification manager
	// =========================================================================
	notifyMgr := notifications.NewManager(cfg, log)

	// =========================================================================
	// 10. Download worker
	// =========================================================================
	dlWorker := worker.NewDownloadWorker(db, ytService, cfg, log, &worker.DownloadWorkerDeps{
		CipherSolver:  cipherSolver,
		PotProvider:   potProvider,
		TwitchService: twService,
		Notifier:      notifyMgr,
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
	autoCookieSvc.PersistPlatforms = func(youtubeVerified, twitchVerified bool) {
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
		cfg.Cookies.Platforms = platforms
		if err := config.Save(cfg, configPath); err != nil {
			log.Warn("Failed to persist auto-cookie platforms", slog.String("error", err.Error()))
		} else {
			log.Debug("Persisted auto-cookie platforms", slog.Any("platforms", platforms))
		}
	}

	// Wire auto-cookie refresh into download worker (attempts refresh on auth failure)
	dlWorker.OnCookieRefreshNeeded = func() bool {
		if !cfg.Cookies.AutoEnabled {
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
	webServer := web.NewServer(cfg, log)
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
	routes.JobRoutes(r, db, cfg, dlWorker, apiRL, &twitchMetadataAdapter{svc: twService}, &youtubeMetadataAdapter{svc: ytService}, notifyMgr)
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
	routes.ConfigRoutes(r, cfg, func(c *config.MoomboxConfig) error {
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
			jobs, _ := db.GetAllJobs(false)
			wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, cfg))
		},
		OnChannelChange: kickMonitors,
	})
	routes.ChannelRoutes(r, cfg, func(c *config.MoomboxConfig) error {
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
	restartFunc := func() {
		log.Info("Restart requested via setup")
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
		OnChannelChange: kickMonitors,
		OnRestart:       restartFunc,
	})
	routes.FFmpegRoutes(r, &routes.FFmpegDeps{
		Cfg: cfg,
		SaveConfig: func(c *config.MoomboxConfig) error {
			return config.Save(c, configPath)
		},
		Logger: log,
	})
	routes.LogRoutes(r, log.GetRecentLines)
	routes.ImportRoutes(r, db, cfg, apiRL)
	routes.CookieRoutes(r, cookieRefresh, autoCookieSvc, getActivePlatforms)
	routes.YtdlpRoutes(r, cfg.Network.Port, cfg.Network.HTTPSEnabled)
	routes.RestartRoute(r, func() {
		log.Info("Restart requested via API")
		restartRequested.Store(true)
		cancel()
		if quitTUI != nil {
			quitTUI()
		}
	})
	routes.UpdateRoutes(r, &routes.UpdateRouteDeps{
		Updater:          upd,
		Version:          version,
		Cfg:              cfg,
		ConfigPath:       configPath,
		RestartRequested: &restartRequested,
		UpdateRestart:    &updateRestart,
		CancelFunc:       cancel,
		QuitTUI:          &quitTUI,
	})
	routes.AuthRoutes(r, &routes.AuthRoutesDeps{
		Cfg:        cfg,
		Auth:       authSvc,
		LoginRL:    loginRL,
		PasswordRL: passwordRL,
		SaveConfig: func(c *config.MoomboxConfig) error {
			return config.Save(c, configPath)
		},
		Logger: log,
	})

	// NOTE: /api/ → /api/v1/ rewriting is handled by APIAliasMiddleware.
	// No catch-all route needed here.

	// WebSocket upgrade handler — register on the router before static file mounting.
	// TS uses noServer mode which upgrades on any path; frontend connects to ws://host/ (root).
	webServer.SetWebSocketHandler(wsHub.HandleUpgrade)

	// Wire WebSocket auth check for external connections
	wsHub.AuthCheck = func(r *http.Request) bool {
		if !web.IsAuthRequired(cfg.Network.NetworkAccess, cfg.Network.PasswordHash) {
			return true
		}
		cookie, err := r.Cookie("moombox_session")
		if err != nil {
			return false
		}
		return authSvc.ValidateSession(cookie.Value)
	}

	// Wire initial state provider for WebSocket connections
	wsHub.InitialState = func() map[string]any {
		jobs, _ := db.GetAllJobs(false)
		jobs = filterJobsByAge(jobs, cfg)
		return map[string]any{
			"jobs":             jobs,
			"nextFeedCheck":    feedMon.GetNextCheckAt(),
			"nextDecapiCheck":  decapiMon.GetNextCheckAt(),
			"nextTwitchCheck":  twitchMon.GetNextCheckAt(),
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
		if !cfg.Cookies.AutoEnabled {
			log.Debug("Auth lost but auto-cookies disabled, skipping recovery", "platform", platform)
			return
		}
		log.Warn("Auth lost, attempting auto-cookie recovery", "platform", platform)
		go func() {
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
			outputDir = cfg.Paths.OutputDirectory
		}

		thumbnailURL := fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", videoID)

		now := time.Now().UTC().Format(time.RFC3339)
		job := &database.Job{
			ID:               videoID,
			VideoID:          videoID,
			URL:              videoURL,
			Title:            title,
			ChannelName:      ch.Name,
			Platform:         "youtube",
			Status:           database.StatusUpcoming,
			ThumbnailURL:     thumbnailURL,
			OutputDirectory:  outputDir,
			AllowNonStream:   includeNonLive,
			CreatedAt:        now,
			UpdatedAt:        now,
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
		wsHub.BroadcastJobsUpdate(getAllJobsSafe(db))
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

	// Monitor -> Worker: create jobs for found videos
	feedMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		createYouTubeJob(videoID, title, url, ch, "feed")
	}

	decapiMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		createYouTubeJob(videoID, title, url, ch, "decapi")
	}

	twitchMon.OnStreamFound = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig) {
		jobID := twitch.BuildJobID(info.StreamID, false)
		log.Info("Stream found by Twitch monitor", slog.String("jobID", jobID), slog.String("title", info.Title))

		// Determine output directory
		outputDir := ch.OutputDirectory
		if outputDir == "" {
			outputDir = cfg.Paths.OutputDirectory
		}

		now := time.Now().UTC().Format(time.RFC3339)
		title := info.ChannelDisplayName + " — " + info.Title
		if info.Title == "" {
			title = info.ChannelDisplayName + " — " + time.Now().UTC().Format(time.RFC3339)
		}

		job := &database.Job{
			ID:               jobID,
			VideoID:          info.StreamID,
			URL:              "https://twitch.tv/" + info.ChannelLogin,
			Title:            title,
			ChannelName:      info.ChannelDisplayName,
			Platform:         "twitch",
			Status:           database.StatusLive, // Twitch: immediately Live (confirmed by GQL)
			ThumbnailURL:     info.ThumbnailURL,
			ChannelAvatarURL: info.ProfileImageURL,
			TwitchCategory:   info.GameCategory,
			TwitchQuality:    ch.QualityPreference,
			StreamStartTime:  info.StartedAt,
			OutputDirectory:  outputDir,
			CreatedAt:        now,
			UpdatedAt:        now,
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
		wsHub.BroadcastJobsUpdate(getAllJobsSafe(db))
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
	if existingJobs, err := db.GetAllJobs(true); err == nil {
		for _, j := range existingJobs {
			db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates
	db.OnJobUpdate(func(job *database.Job) {
		wsHub.BroadcastJobUpdate(job.ID, job)
	})
	db.OnJobsChange(func(jobs []*database.Job) {
		// Keep per-job log tracking in sync (matches TS knownJobIds update)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			db.TrackJobForLogs(j.ID)
		}
		db.PruneJobLogs(activeIDs)

		wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, cfg))
	})

	// Logger -> WebSocket: broadcast log lines + route to per-job buffers
	logSub := log.Subscribe()
	go func() {
		for line := range logSub {
			wsHub.BroadcastLog(line)
			db.RouteLogToJobs(line) // Route to per-job buffer (matches TS knownJobIds log routing)
		}
	}()

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
	go dlWorker.Start(ctx)

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
			if err := <-webErrCh; err != nil {
				log.Error("Web server error", slog.String("error", err.Error()))
			}
		}()
	}

	// Expose actual bound port for TUI and other components (matches TS: process.env.MOOMBOX_PORT)
	if actualPort := webServer.ActualPort; actualPort > 0 {
		cfg.Network.Port = actualPort
	}

	// Initial disk space check (populate immediately so status bar has data).
	routes.UpdateDiskStatus(cfg.Paths.OutputDirectory, cfg)

	// TUI disk status channel (created early so the goroutine can reference it;
	// only receives messages when TUI mode is active).
	tuiDiskStatusCh := make(chan tui.DiskStatusMsg, 5)

	// TUI update status channel
	tuiUpdateStatusCh := make(chan tui.UpdateStatusMsg, 2)

	// Auto-update check: initial check + daily ticker
	if upd != nil && cfg.Updates.AutoCheckUpdates {
		go func() {
			// Initial check (slight delay to avoid slowing startup)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			checkAndBroadcastUpdate(upd, wsHub, notifyMgr, tuiUpdateStatusCh, log)

			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					checkAndBroadcastUpdate(upd, wsHub, notifyMgr, tuiUpdateStatusCh, log)
				}
			}
		}()
	}

	// Periodic memory + disk usage logging (every 2 minutes) for diagnostics.
	// Matches TS: process.memoryUsage() logging with heap delta tracking.
	go func() {
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
				runtime.GC() // Force GC for accurate measurement (like TS --expose-gc)
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
					if ds := routes.UpdateDiskStatus(cfg.Paths.OutputDirectory, cfg); ds != nil {
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

		// Pass config reference and version for settings panel
		app.SetConfig(cfg)
		app.SetVersion(version)
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
		app.OnRetryJob = func(jobID string) {
			db.UpdateJobFields(jobID, map[string]any{
				"status": string(database.StatusUpcoming),
				"error":  "",
			})
			dlWorker.EnqueueJob(jobID)
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
				stagingBase := cfg.Paths.StagingDirectory
				if stagingBase == "" {
					stagingBase = "./staging"
				}
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
			_ = cmd.Start()
		}
		app.OnCreateTrim = func(jobID string, startSec, endSec float64) {
			job, err := db.GetJob(jobID)
			if err != nil || job == nil {
				log.Error("Failed to get job for trim", slog.String("jobID", jobID))
				return
			}
			if _, err := trimSvc.CreateTrim(context.Background(), job, startSec, endSec); err != nil {
				log.Error("Failed to create trim", slog.String("error", err.Error()))
			}
		}
		app.OnDeleteTrim = func(jobID, trimID string) {
			if err := trimSvc.DeleteTrim(jobID, trimID); err != nil {
				log.Error("Failed to delete trim", slog.String("error", err.Error()))
			}
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
			return worker.DeleteOrphanedFile(path, cfg)
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
		app.OnRestart = func() {
			log.Info("Restart requested from TUI settings")
			restartRequested.Store(true)
			cancel()
			app.QuitTUI()
		}
		if upd != nil {
			app.OnApplyUpdate = func(ver string) string {
				release := routes.SharedUpdateInfo.Load()
				if release == nil {
					return "no update info available"
				}
				log.Info("Update requested from TUI", slog.String("version", ver))
				if err := upd.ApplyUpdate(release); err != nil {
					log.Error("[Updater] Update failed", slog.String("error", err.Error()))
					return err.Error()
				}
				restartRequested.Store(true)
				updateRestart.Store(true)
				cancel()
				app.QuitTUI()
				return ""
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
				return config.Save(updatedCfg, configPath)
			},
			func(port int) {
				if err := routes.InstallYtdlpPlugin(port, cfg.Network.HTTPSEnabled); err != nil {
					log.Error("Failed to install yt-dlp plugin from setup", slog.String("error", err.Error()))
				} else {
					log.Info("yt-dlp plugin installed from setup wizard", slog.Int("port", port))
				}
			},
			func(platform string) {
				if autoCookieSvc != nil {
					if err := autoCookieSvc.StartSetup(platform); err != nil {
						log.Error("Failed to start auto-cookie setup", slog.String("platform", platform), slog.String("error", err.Error()))
					}
				}
			},
			func() (bool, bool) {
				if autoCookieSvc != nil {
					yt, tw, err := autoCookieSvc.FinishSetup(context.Background())
					if err != nil {
						log.Error("Failed to finish auto-cookie setup", slog.String("error", err.Error()))
					}
					return yt, tw
				}
				return false, false
			},
			func() {
				if autoCookieSvc != nil {
					autoCookieSvc.CancelSetup()
				}
			},
			func() {
				log.Info("Restart requested from setup wizard")
				restartRequested.Store(true)
				cancel()
				app.QuitTUI()
			},
		)

		// Wire FFmpeg check callbacks for TUI
		app.OnCheckFFmpeg = func(path string) (bool, string) {
			if path == "" {
				path = "ffmpeg"
			}
			cmd := exec.Command(path, "-version")
			out, err := cmd.Output()
			if err != nil {
				return false, ""
			}
			ver := string(out)
			if idx := strings.IndexByte(ver, '\n'); idx > 0 {
				ver = ver[:idx]
			}
			return true, strings.TrimSpace(ver)
		}
		app.OnInstallFFmpeg = routes.InstallFFmpeg
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
			checkCtx, checkCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := exec.CommandContext(checkCtx, ffmpegPath, "-version").Run(); err != nil {
				app.ShowFFmpegCheck()
			}
			checkCancel()
		}

		// Create async update channels for TUI
		jobUpdateCh := make(chan *database.Job, 100)
		jobsUpdateCh := make(chan []*database.Job, 10)
		logCh := make(chan string, 200)
		checkTimersCh := make(chan tui.CheckTimersMsg, 10)
		cookieStatusCh := make(chan tui.CookieStatusMsg, 5)

		app.SetUpdateChannels(jobUpdateCh, jobsUpdateCh, logCh, checkTimersCh, cookieStatusCh, tuiDiskStatusCh, tuiUpdateStatusCh)

		// Push initial disk status to TUI
		if ds := routes.SharedDiskStatus.Load(); ds != nil {
			select {
			case tuiDiskStatusCh <- tui.DiskStatusMsg{Free: ds.Free, UsedPct: ds.UsedPct, Warn: ds.WarnLevel}:
			default:
			}
		}

		// Forward DB events to TUI channels
		db.OnJobUpdate(func(job *database.Job) {
			select {
			case jobUpdateCh <- job:
			default: // Drop if channel is full
			}
		})
		db.OnJobsChange(func(jobs []*database.Job) {
			select {
			case jobsUpdateCh <- jobs:
			default:
			}
		})

		// Forward log lines to TUI
		tuiLogSub := log.Subscribe()
		go func() {
			for line := range tuiLogSub {
				select {
				case logCh <- line:
				default:
				}
			}
		}()

		// Forward monitor schedule events to TUI
		origFeedOnSchedule := feedMon.OnSchedule
		feedMon.OnSchedule = func(nextCheckAt int64) {
			if origFeedOnSchedule != nil {
				origFeedOnSchedule(nextCheckAt)
			}
			t := time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
			select {
			case checkTimersCh <- tui.CheckTimersMsg{NextFeedCheck: t}:
			default:
			}
		}
		origDecapiOnSchedule := decapiMon.OnSchedule
		decapiMon.OnSchedule = func(nextCheckAt int64) {
			if origDecapiOnSchedule != nil {
				origDecapiOnSchedule(nextCheckAt)
			}
			t := time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
			select {
			case checkTimersCh <- tui.CheckTimersMsg{NextDecapiCheck: t}:
			default:
			}
		}
		origTwitchOnSchedule := twitchMon.OnSchedule
		twitchMon.OnSchedule = func(nextCheckAt int64) {
			if origTwitchOnSchedule != nil {
				origTwitchOnSchedule(nextCheckAt)
			}
			t := time.Unix(nextCheckAt/1000, (nextCheckAt%1000)*int64(time.Millisecond))
			select {
			case checkTimersCh <- tui.CheckTimersMsg{NextTwitchCheck: t}:
			default:
			}
		}

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
			ytActive, twActive := config.GetActivePlatforms(cfg)
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
		if jobs, err := db.GetAllJobs(false); err == nil {
			select {
			case jobsUpdateCh <- jobs:
			default:
			}
		}

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
	}

	log.Info("Shutdown signal received, shutting down gracefully...")

	// 10-second force-exit timer
	forceExit := time.AfterFunc(10*time.Second, func() {
		log.Error("Graceful shutdown timed out, forcing exit")
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

	// 7. Unsubscribe log forwarder
	log.Unsubscribe(logSub)

	// 8. Flush database
	stopService("Database", func() { db.Close() })

	if restartRequested.Load() {
		log.Info("Shutdown complete, restarting...")
	} else {
		log.Info("Shutdown complete")
	}

	return restartRequested.Load()
}

// addVideo adds a video/stream to the queue from the command line.
// Mirrors TypeScript's addVideo() from index.ts, including notification dispatch.
func addVideo(input string) {
	target := utils.ExtractMediaID(input)
	if target == nil {
		fmt.Fprintf(os.Stderr, "Invalid video ID or URL: %s\n", input)
		os.Exit(1)
	}

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	db, err := database.Open(cfg.Paths.DatabasePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Init notification manager for "Video Added" dispatch (matches TS addVideo)
	notifyMgr := notifications.NewManager(cfg, &nopLogger{})

	now := time.Now().UTC().Format(time.RFC3339)

	if target.Platform == "twitch" {
		tw := target.Twitch
		if tw.Type == utils.TwitchClip {
			fmt.Fprintln(os.Stderr, "Twitch clips are not supported.")
			os.Exit(1)
		}

		var jobID, jobURL, channelName string
		if tw.Type == utils.TwitchVOD {
			jobID = "tw_v" + tw.Value
			jobURL = "https://www.twitch.tv/videos/" + tw.Value
			channelName = "Manual"
		} else {
			jobID = tw.Value // Will be resolved by the worker
			jobURL = "https://www.twitch.tv/" + tw.Value
			channelName = tw.Value
		}

		if db.JobExists(jobID) {
			fmt.Printf("Job already exists: %s\n", jobID)
			return
		}

		job := &database.Job{
			ID:            jobID,
			VideoID:       jobID,
			URL:           jobURL,
			Title:         "Manual Add",
			ChannelName:   channelName,
			Platform:      "twitch",
			Status:        database.StatusUpcoming,
			ManuallyAdded: true,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		added, err := db.AddJob(job)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to add job: %v\n", err)
			os.Exit(1)
		}
		if added {
			fmt.Printf("Added Twitch %s %s to queue.\n", tw.Type, jobID)
			notifyMgr.Send("Video Added",
				fmt.Sprintf("Manually added Twitch %s: %s", tw.Type, jobID),
				notifications.TypeInfo,
				[]notifications.Field{{Name: "ID", Value: jobID, Inline: true}},
				notifications.SendOptions{URL: jobURL, Event: "added"})
		} else {
			fmt.Printf("Failed to add %s (may already exist).\n", jobID)
		}
	} else {
		// YouTube
		videoID := target.VideoID
		if db.JobExists(videoID) {
			fmt.Printf("Job already exists for video: %s\n", videoID)
			return
		}

		videoURL := "https://www.youtube.com/watch?v=" + videoID
		job := &database.Job{
			ID:            videoID,
			VideoID:       videoID,
			URL:           videoURL,
			Title:         "Manual Add",
			ChannelName:   "Manual",
			Platform:      "youtube",
			Status:        database.StatusUpcoming,
			ManuallyAdded: true,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		added, err := db.AddJob(job)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to add job: %v\n", err)
			os.Exit(1)
		}
		if added {
			fmt.Printf("Added %s to queue.\n", videoID)
			notifyMgr.Send("Video Added",
				fmt.Sprintf("Manually added: %s", videoID),
				notifications.TypeInfo,
				[]notifications.Field{{Name: "Video ID", Value: videoID, Inline: true}},
				notifications.SendOptions{URL: videoURL, Event: "added"})
		} else {
			fmt.Printf("Failed to add %s (may already exist).\n", videoID)
		}
	}

	// Give async notifications a moment to dispatch (match TS await behavior)
	time.Sleep(500 * time.Millisecond)
}

// nopLogger is a no-op logger for CLI commands where full logging isn't needed.
type nopLogger struct{}

func (n *nopLogger) Debug(_ string, _ ...any) {}
func (n *nopLogger) Info(_ string, _ ...any)  {}
func (n *nopLogger) Warn(_ string, _ ...any)  {}
func (n *nopLogger) Error(_ string, _ ...any) {}

func getAllJobsSafe(db *database.Database) []*database.Job {
	jobs, _ := db.GetAllJobs(false)
	return jobs
}

// ytFormatAdapter adapts the YouTube service to the FormatRoutesDeps interface.
type ytFormatAdapter struct {
	svc *youtube.Service
	cfg *config.MoomboxConfig
}

func (a *ytFormatAdapter) GetFormats(ctx context.Context, videoID string) (map[string]any, error) {
	info, _, err := a.svc.GetFormats(ctx, videoID, a.cfg.Downloader.MaxVideoResolution, a.cfg.Downloader.Prefer60fps)
	if err != nil {
		return nil, err
	}

	// Separate video and audio formats, only those with a URL
	var videoFormats, audioFormats []youtube.Format
	for _, f := range info.Formats {
		if f.URL == "" {
			continue
		}
		if strings.Contains(f.MimeType, "video") {
			videoFormats = append(videoFormats, f)
		} else if strings.Contains(f.MimeType, "audio") {
			audioFormats = append(audioFormats, f)
		}
	}

	// Sort video: resolution desc → fps desc → bitrate asc (match TypeScript)
	sort.SliceStable(videoFormats, func(i, j int) bool {
		a, b := videoFormats[i], videoFormats[j]
		aRes := maxDim(a.Width, a.Height)
		bRes := maxDim(b.Width, b.Height)
		if aRes != bRes {
			return aRes > bRes
		}
		aFps := derefInt(a.Fps)
		bFps := derefInt(b.Fps)
		if aFps != bFps {
			return aFps > bFps
		}
		return a.Bitrate < b.Bitrate
	})

	// Sort audio: bitrate desc (match TypeScript)
	sort.SliceStable(audioFormats, func(i, j int) bool {
		return audioFormats[i].Bitrate > audioFormats[j].Bitrate
	})

	// Build bestItags matching TypeScript format: bestWebmVideo, bestMp4Video, bestOpusAudio, bestAacAudio
	bestItags := map[string]any{
		"bestWebmVideo": nil,
		"bestMp4Video":  nil,
		"bestOpusAudio": nil,
		"bestAacAudio":  nil,
	}
	for _, f := range videoFormats {
		if bestItags["bestWebmVideo"] == nil && strings.Contains(f.MimeType, "webm") {
			bestItags["bestWebmVideo"] = f.Itag
		}
		if bestItags["bestMp4Video"] == nil && strings.Contains(f.MimeType, "mp4") {
			bestItags["bestMp4Video"] = f.Itag
		}
	}
	for _, f := range audioFormats {
		if bestItags["bestOpusAudio"] == nil && strings.Contains(f.MimeType, "opus") {
			bestItags["bestOpusAudio"] = f.Itag
		}
		if bestItags["bestAacAudio"] == nil && strings.Contains(f.MimeType, "mp4a") {
			bestItags["bestAacAudio"] = f.Itag
		}
	}

	return map[string]any{
		"videoId":       videoID,
		"title":         info.Title,
		"channelName":   info.ChannelName,
		"lengthSeconds": info.LengthSeconds,
		"streamStatus":  info.StreamStatus,
		"videoFormats":  videoFormats,
		"audioFormats":  audioFormats,
		"bestItags":     bestItags,
	}, nil
}

func maxDim(w, h *int) int {
	wv, hv := derefInt(w), derefInt(h)
	if wv > hv {
		return wv
	}
	return hv
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// twitchMetadataAdapter adapts the Twitch service to the TwitchMetadataFetcher interface.
type twitchMetadataAdapter struct {
	svc *twitch.Service
}

func (a *twitchMetadataAdapter) FetchStreamMetadata(ctx context.Context, login string) (*routes.TwitchJobMetadata, error) {
	info, err := a.svc.GetStreamInfo(ctx, login)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &routes.TwitchJobMetadata{
		StreamID:     info.StreamID,
		Title:        info.Title,
		ChannelName:  info.ChannelDisplayName,
		ThumbnailURL: info.ThumbnailURL,
		AvatarURL:    info.ProfileImageURL,
		StartedAt:    info.StartedAt,
		GameCategory: info.GameCategory,
		IsLive:       info.IsLive,
	}, nil
}

func (a *twitchMetadataAdapter) FetchVodMetadata(ctx context.Context, vodID string) (*routes.TwitchJobMetadata, error) {
	info, err := a.svc.GetVodInfo(ctx, vodID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &routes.TwitchJobMetadata{
		Title:        info.Title,
		ChannelName:  info.ChannelDisplayName,
		ThumbnailURL: info.ThumbnailURL,
		StartedAt:    info.CreatedAt,
		GameCategory: info.GameCategory,
	}, nil
}

// youtubeMetadataAdapter implements routes.YouTubeMetadataFetcher.
type youtubeMetadataAdapter struct {
	svc *youtube.Service
}

func (a *youtubeMetadataAdapter) FetchMetadata(ctx context.Context, videoID string) (*routes.YouTubeJobMetadata, error) {
	if a.svc == nil {
		return nil, nil
	}
	info, err := a.svc.ProbeVideoStatus(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &routes.YouTubeJobMetadata{
		Title:        info.Title,
		ChannelName:  info.ChannelName,
		ChannelID:    info.ChannelID,
		ThumbnailURL: info.ThumbnailURL,
	}, nil
}

// filterJobsByAge excludes finished jobs older than hide_finished_age_days (match TS filterJobsByAge).
// checkAndBroadcastUpdate checks for a new release and broadcasts the result.
func checkAndBroadcastUpdate(
	upd *updater.Updater,
	wsHub *web.WebSocketHub,
	notifyMgr *notifications.Manager,
	tuiCh chan<- tui.UpdateStatusMsg,
	log *logger.Logger,
) {
	release, err := upd.CheckForUpdate()
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

// execNewBinary launches the updated binary in a new console window and exits.
// This is called after an update has been applied and the graceful shutdown
// has completed (port is released, database is closed, etc).
// Uses syscall.CreateProcess directly (instead of exec.Command) because Go's
// exec package always sets STARTF_USESTDHANDLES which overrides the new
// console's handles with DevNull — leaving the new window empty. By calling
// CreateProcess without that flag, the child gets proper stdio from its new
// console.
func execNewBinary() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to determine executable path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Launching updated binary in new window: %s\n", exe)

	// Build quoted command line: "exe" "arg1" "arg2" ...
	cmdLine := syscall.EscapeArg(exe)
	for _, arg := range os.Args[1:] {
		cmdLine += " " + syscall.EscapeArg(arg)
	}
	cmdLineUTF16, err := syscall.UTF16PtrFromString(cmdLine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to encode command line: %v\n", err)
		os.Exit(1)
	}

	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	// Don't set Flags to STARTF_USESTDHANDLES — let the new console
	// provide its own stdin/stdout/stderr.

	var pi syscall.ProcessInformation
	err = syscall.CreateProcess(
		nil,          // lpApplicationName (parsed from command line)
		cmdLineUTF16, // lpCommandLine
		nil,          // lpProcessAttributes
		nil,          // lpThreadAttributes
		false,        // bInheritHandles
		0x00000010,   // dwCreationFlags: CREATE_NEW_CONSOLE
		nil,          // lpEnvironment (inherit)
		nil,          // lpCurrentDirectory (inherit)
		&si,
		&pi,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start updated binary: %v\n", err)
		os.Exit(1)
	}

	syscall.CloseHandle(pi.Thread)
	syscall.CloseHandle(pi.Process)
	os.Exit(0)
}

func filterJobsByAge(jobs []*database.Job, cfg *config.MoomboxConfig) []*database.Job {
	ageDays := int(cfg.Monitors.HideFinishedAgeDays.Value)
	cutoff := time.Now().AddDate(0, 0, -ageDays)
	var filtered []*database.Job
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

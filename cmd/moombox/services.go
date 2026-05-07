package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// initServices runs the 16 numbered construction sections from the original
// run() — config load, logger, updater, database, connectivity, cookies,
// platform services, worker, trim, monitors, cookie-refresh / auto-cookie,
// web server + rate limiters + auth + core shared closures (triggerRestart,
// kickMonitors, getActivePlatforms). On return, all runState fields for
// those phases are populated and the close-once helpers (closeLog, closeDB,
// closeLimiters) are ready for run() to defer.
//
// Returns a wrapped error from the three fatal-exit points (config.Load,
// logger.New, database.Open). The caller prints the error and calls
// os.Exit(1) to preserve the pre-refactor behaviour where deferred cleanup
// is skipped on a fatal startup failure.
func (s *runState) initServices(logLevelOverride string) error {
	if !s.useTUI {
		fmt.Println("Loading configuration...")
	}

	// =========================================================================
	// 1. Load config
	// =========================================================================
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	s.cfg = cfg
	// configStore owns the synchronising mutex (no external cfgMu — every
	// caller now goes through Store APIs per DECISIONS #8 wave 4-7).
	s.configStore = config.NewStore(cfg, s.configPath)

	if logLevelOverride != "" {
		cfg.Logs.LogLevel = logLevelOverride
	}

	// =========================================================================
	// 2. Initialize logger
	// =========================================================================
	log, err := logger.New(cfg.Paths.LogFilePath, cfg.Logs.LogLevel, cfg.Logs.LogMaxFileSize, cfg.Logs.LogMaxFiles)
	if err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	s.log = log
	// Close the logger at most once — the deferred close at the bottom of
	// run() AND the force-exit timer both need to flush buffered lines on
	// exit, and logger.Close is not guaranteed idempotent at this layer.
	var logCloseOnce sync.Once
	s.closeLog = func() { logCloseOnce.Do(func() { log.Close() }) }

	log.Info("Starting Moombox", slog.String("version", version), slog.String("commit", commit))

	// Apply Go runtime soft memory limit. SetMemoryLimit is a SOFT cap:
	// Go's GC runs more aggressively as the heap approaches the limit, but
	// allocations that genuinely need more memory still succeed (vs OOM).
	// Zero or negative disables (Go uses its default unbounded behaviour).
	if mb := cfg.Memory.GoSoftLimitMB; mb > 0 {
		debug.SetMemoryLimit(int64(mb) << 20)
		log.Info("Go soft memory limit applied", slog.Int("mb", mb))
	}

	// Updater: create instance and clean up .old binary from previous update
	upd, updErr := updater.New(version, log)
	if updErr != nil {
		log.Warn("Updater unavailable", slog.String("error", updErr.Error()))
	} else {
		upd.CleanupOldBinary()
	}
	s.upd = upd

	// Auto-convert plaintext password to scrypt hash (matches TS ConfigManager.load)
	if cfg.Network.PasswordHash != "" && !web.IsScryptHash(cfg.Network.PasswordHash) {
		log.Info("[Config] Plaintext password detected, converting to secure hash")
		tempAuth := web.NewAuthService()
		hash, err := tempAuth.HashPassword(cfg.Network.PasswordHash)
		if err == nil {
			if saveErr := s.configStore.Update(func(c *config.MoomboxConfig) {
				c.Network.PasswordHash = hash
			}); saveErr != nil {
				log.Warn("Failed to save auto-hashed password", slog.String("error", saveErr.Error()))
			}
		} else {
			log.Error("Failed to hash plaintext password", slog.String("error", err.Error()))
		}
	}

	// =========================================================================
	// 3. Open database
	// =========================================================================
	if !s.useTUI {
		fmt.Println("Initializing database...")
	}
	db, err := database.Open(cfg.Paths.DatabasePath, log)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	s.db = db
	// Database Close is wrapped in sync.Once so both the orderly shutdown
	// sequence (stopService "Database") and the run()-side defer are safe.
	var dbCloseOnce sync.Once
	s.closeDB = func() { dbCloseOnce.Do(func() { db.Close() }) }

	// =========================================================================
	// 3b. Connectivity monitor
	// =========================================================================
	s.connMon = connectivity.NewMonitor(log)
	s.connMon.Start(s.ctx)
	utils.SetConnectivityReporter(s.connMon)
	engine.SetConnectivityReporter(s.connMon)
	monitor.SetConnectivityReporter(s.connMon)

	// =========================================================================
	// 4. Load cookies
	// =========================================================================
	if !s.useTUI {
		fmt.Println("Starting services...")
	}
	jar := cookies.NewCookieJar()
	jar.SetLogger(log)
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
					saveErr := s.configStore.Update(func(c *config.MoomboxConfig) {
						c.Cookies.Platforms = detected
					})
					if saveErr != nil {
						log.Warn("Failed to persist detected cookie platforms", slog.String("error", saveErr.Error()))
					} else {
						log.Info("Detected cookie platforms from cookie file", slog.Any("platforms", detected))
					}
				}
			}
		}
	}
	s.jar = jar

	// =========================================================================
	// 5. YouTube service
	// =========================================================================
	ytService := youtube.NewService(jar, log)
	ytService.Init(s.ctx) // Fetch homepage for visitor data and API key
	s.ytService = ytService

	// =========================================================================
	// 6. Twitch service
	// =========================================================================
	twService := twitch.NewService(jar, log)
	if twService.Auth.HasAuthToken() {
		log.Info("[Twitch] Authenticated (auth-token found in cookies)")
	} else {
		log.Debug("[Twitch] No auth-token found, using anonymous access")
	}
	s.twService = twService

	// =========================================================================
	// 7. PO Token provider
	// =========================================================================
	// The interpreter-hash disk cache that previously lived in BgConfig
	// was removed (test.41) -- it sent cached hashes back to YouTube but
	// didn't actually cache the interpreter SCRIPT, so YouTube responded
	// with BAD_CONFIG. Phase 1 of the sidecar arc deleted the dead code
	// outright; the goja-fallback path now always fetches a fresh
	// interpreter, and the sidecar path runs entirely inside Node.
	potProvider := bgutils.NewPotProvider(&bgutils.BgConfig{}, log)
	s.potProvider = potProvider

	// =========================================================================
	// 7b. BotGuard sidecar (Node + JSDOM + bgutils-js subprocess)
	// =========================================================================
	// The sidecar produces real PO tokens that pass BotGuard's timing
	// fingerprint (which the goja-only path can't, see
	// docs/investigations/botguard-option-2-results.md). On any failure
	// we log a warning and PotProvider falls through to its goja path
	// which still produces websafe-fallback tokens -- downloads keep
	// working, but PO-token-gated formats may be unavailable.
	//
	// First launch extracts ~36 MB of embedded blobs to
	// %LOCALAPPDATA%/Moombox/sidecar (one-time, ~3-5s); subsequent
	// launches reuse the cached extraction (sub-second).
	//
	// Set [bgutils] use_sidecar = false in config.toml to disable.
	if cfg.Bgutils.UseSidecar {
		bgSidecar := sidecar.New(sidecar.Config{
			Logger:        log,
			V8HardLimitMB: cfg.Memory.SidecarHardLimitMB,
			// ExposeGC is required for the periodic TriggerGC call that
			// enforces the soft sidecar limit. Always on when the sidecar
			// is enabled — the cost is a function global no caller invokes
			// unless we ask for it.
			ExposeGC: true,
		})
		sCtx, sCancel := context.WithTimeout(s.ctx, 60*time.Second)
		startErr := bgSidecar.Start(sCtx)
		sCancel()
		if startErr != nil {
			log.Warn("BotGuard sidecar failed to start; falling back to goja", slog.String("error", startErr.Error()))
		} else {
			s.bgSidecar = bgSidecar
			potProvider.SetSidecar(bgSidecar)
			log.Info("BotGuard sidecar ready", slog.String("cacheDir", bgSidecar.CacheDir()))
		}
	} else {
		log.Info("BotGuard sidecar disabled in config; using goja fallback only")
	}

	// =========================================================================
	// 8. Cipher solver
	// =========================================================================
	// gojaSolver provides in-process n decryption (and historically sig,
	// though sig is broken on current YouTube players — see
	// docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md).
	// sidecarSolver routes sig + n to the BotGuard sidecar via ejs; it's
	// only constructed when the sidecar is healthy. compositeSolver
	// applies the routing policy (sig: sidecar-only; n: sidecar primary,
	// goja fallback). PlayerAPI continues to take *GojaResolver directly
	// so its existing GetSolvers/GetSts call sites work unchanged; the
	// composite is constructed for downstream callers that take the
	// cipher.Solver interface (currently a placeholder until those
	// call sites migrate).
	//
	// Failure to init the goja resolver is fatal: NewGojaResolver only errors
	// on os.MkdirAll of %TEMP%/yt-cipher, and without cipher solving
	// most YouTube format URLs (sig + n-param ciphered) cannot be
	// decrypted.
	cacheDir := filepath.Join(os.TempDir(), "yt-cipher")
	gojaSolver, err := cipher.NewGojaResolver(cacheDir, log)
	if err != nil {
		return fmt.Errorf("init goja cipher solver (cacheDir=%q): %w", cacheDir, err)
	}

	var sidecarCipher cipher.Solver
	if s.bgSidecar != nil {
		sidecarCipher = cipher.NewSidecarSolver(s.bgSidecar, gojaSolver)
	}
	cipherSolver := cipher.NewCompositeSolver(sidecarCipher, gojaSolver)
	s.cipherSolver = gojaSolver
	s.routedCipher = cipherSolver

	// Wire goja resolver for GetSts (signature timestamp lookup, not part of
	// the cipher.Solver interface) and the composite Solver for sig/n decryption.
	// Sig flows through the sidecar's V8 ejs; n falls back to goja if the sidecar
	// is unavailable.
	ytService.PlayerAPI.SetCipherSolver(gojaSolver)
	ytService.PlayerAPI.SetCipher(cipherSolver)

	// Wire PO token provider into Innertube player requests (audit youtube.md C1).
	ytService.PlayerAPI.SetPotProvider(potProvider)

	// =========================================================================
	// 9. Notification manager
	// =========================================================================
	notifyMgr := notifications.NewManager(cfg, log)
	s.notifyMgr = notifyMgr

	// =========================================================================
	// 10. Download worker
	// =========================================================================
	dlWorker := worker.NewDownloadWorker(db, ytService, cfg, log, &worker.DownloadWorkerDeps{
		CipherSolver:       gojaSolver,
		RoutedCipherSolver: cipherSolver,
		PotProvider:        potProvider,
		TwitchService:      twService,
		Notifier:           notifyMgr,
		Conn:               s.connMon,
	})
	s.dlWorker = dlWorker

	// =========================================================================
	// 11. Trim service
	// =========================================================================
	trimSvc := worker.NewTrimService(db, cfg.Paths.FfmpegPath, log)
	trimSvc.SetNotifier(notifyMgr)
	s.trimSvc = trimSvc

	// Sweep orphaned trim/two-pass tempdirs from %TEMP% on startup.
	// `defer os.RemoveAll(tempDir)` inside the trim path covers the
	// happy case; a hard process abort (panic in a sibling goroutine,
	// OS kill, power loss) bypasses the defer and leaks the dir. 24h
	// age threshold keeps concurrent trims' in-flight tempdirs safe.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("trim-tempdir cleanup panic", slog.Any("panic", r))
			}
		}()
		if removed, err := engine.CleanupOldTrimTempDirs(); err != nil {
			log.Debug("trim-tempdir cleanup", slog.String("error", err.Error()))
		} else if removed > 0 {
			log.Info("trim-tempdir cleanup", slog.Int("removed", removed))
		}
	}()

	// =========================================================================
	// 12. Feed monitor (YouTube RSS)
	// =========================================================================
	// Shared probe cooldown: feed and DECAPI both probe YouTube videos via
	// ytService.ProbeVideoStatus. Sharing one cache means a probe by either
	// monitor blocks the other from re-probing the same video for the
	// cooldown window. Caps per-video probe rate at ~2/hour.
	probeCooldown := monitor.NewProbeCooldown(monitor.DefaultProbeCooldown)

	feedMon := monitor.NewFeedMonitor(s.configStore, db, log)
	feedMon.ProbeCooldown = probeCooldown
	s.feedMon = feedMon

	// =========================================================================
	// 13. DECAPI monitor
	// =========================================================================
	decapiMon := monitor.NewDecapiMonitor(s.configStore, db, log)
	decapiMon.ProbeCooldown = probeCooldown
	s.decapiMon = decapiMon

	// =========================================================================
	// 14. Twitch monitor
	// =========================================================================
	twitchMon := monitor.NewTwitchMonitor(s.configStore, db, twService, log)
	s.twitchMon = twitchMon

	// Wire connectivity to monitors so they skip polls when offline
	feedMon.IsOnline = s.connMon.IsOnline
	decapiMon.IsOnline = s.connMon.IsOnline
	twitchMon.IsOnline = s.connMon.IsOnline

	// =========================================================================
	// 15. Cookie refresh service
	// =========================================================================
	cookieRefresh := cookies.NewRefreshService(jar, 0, log)
	s.cookieRefresh = cookieRefresh

	// =========================================================================
	// 15b. Auto-cookie service
	// =========================================================================
	browserProfileDir := cfg.Cookies.BrowserProfileDir
	if browserProfileDir == "" {
		browserProfileDir = "./browser-profile"
	}
	s.browserProfileDir = browserProfileDir
	autoCookieSvc := cookies.NewAutoCookieService(
		browserProfileDir,
		cfg.Cookies.CookieFile,
		jar,
		log,
	)
	// Wire auth verification callbacks so AutoCookieService can verify via real API
	autoCookieSvc.VerifyYouTubeAuth = cookieRefresh.CheckYouTubeAuth
	autoCookieSvc.VerifyTwitchAuth = cookieRefresh.CheckTwitchAuth
	// Wire configured-browser-override callback so GetStatus surfaces the
	// user's chosen browser_path/browser_type (if any) for the dropdown UI.
	autoCookieSvc.ConfiguredBrowserOverride = func() (string, string) {
		var path, btype string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			path = c.Cookies.BrowserPath
			btype = c.Cookies.BrowserType
		})
		return path, btype
	}
	s.autoCookieSvc = autoCookieSvc

	// OnAuthChange is fired from the cookie-refresh goroutine; set its plain
	// func field exactly once (before cookieRefresh.Start()) to a dispatcher
	// that atomically loads the TUI-side slot. The TUI branch stores a
	// callback later — Store is race-free against the refresh goroutine's
	// Load, unlike field reassignment.
	cookieRefresh.OnAuthChange = func(auth cookies.AuthStatus) {
		if fn := s.authChangeTUI.Load(); fn != nil {
			(*fn)(auth)
		}
	}

	// Wire persistPlatforms callback: saves verified platforms to config
	// so we can detect auth loss after restart (matches TS persistPlatforms).
	dlWorker.SetConfigStore(s.configStore)
	autoCookieSvc.PersistPlatforms = func(youtubeVerified, twitchVerified bool) {
		// During first-run setup, the config file doesn't exist yet. Don't
		// create it prematurely — the setup wizard's POST /api/setup/complete
		// will save everything (including platforms) when the user finishes.
		var platforms []string
		err := s.configStore.Update(func(c *config.MoomboxConfig) {
			if !c.ConfigLoaded {
				return
			}
			existing := make(map[string]bool)
			for _, p := range c.Cookies.Platforms {
				existing[p] = true
			}
			if youtubeVerified {
				existing["youtube"] = true
			}
			if twitchVerified {
				existing["twitch"] = true
			}
			platforms = make([]string, 0, len(existing))
			for p := range existing {
				platforms = append(platforms, p)
			}
			slices.Sort(platforms)
			c.Cookies.Platforms = platforms
		})
		if platforms == nil {
			return // first-run, nothing persisted
		}
		if err != nil {
			log.Warn("Failed to persist auto-cookie platforms", slog.String("error", err.Error()))
		} else {
			log.Debug("Persisted auto-cookie platforms", slog.Any("platforms", platforms))
		}
	}

	// Wire active-job lookup so the periodic auto-cookie refresh can skip
	// the headless-Chrome launch when nothing is actively pulling content
	// (audit reports/cookies.md #23). Uses the cached GetJobStats query
	// (jobStatsCacheTTL ≈ 5s) so the per-tick check stays cheap. A nil-stats
	// fallback returns true (refresh-on-error) so a transient DB hiccup
	// doesn't silently drop refreshes.
	autoCookieSvc.HasActiveJobs = func() bool {
		stats, err := s.db.GetJobStats()
		if err != nil || stats == nil {
			return true
		}
		return stats.ActiveCount > 0
	}

	// Mirror the cookies.dpapi_fallback config flag onto the service.
	// Read once at startup — toggling at runtime would require a
	// restart, which is consistent with how other AutoCookieService
	// fields work (set at construction, never re-read). DECISIONS #6.
	s.configStore.Read(func(c *config.MoomboxConfig) {
		autoCookieSvc.DpapiFallback = c.Cookies.DpapiFallback
	})

	// Wire auto-cookie refresh into download worker (attempts refresh on auth failure)
	dlWorker.OnCookieRefreshNeeded = func() bool {
		var autoEnabled bool
		s.configStore.Read(func(c *config.MoomboxConfig) {
			autoEnabled = c.Cookies.AutoEnabled
		})
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
	s.startTime = time.Now()

	// Configure cookie Secure-flag policy based on
	// trust_forwarded_proto. Default false: directly-exposed Moombox
	// MUST NOT trust the X-Forwarded-Proto header. Reverse-proxy
	// deployments that strip client-supplied X-Forwarded-Proto opt in
	// via the config flag.
	web.SetTrustForwardedProto(cfg.Network.TrustForwardedProto)

	webServer := web.NewServer(s.configStore, log)
	webServer.SetCommit(commit)
	s.webServer = webServer
	s.wsHub = webServer.WebSocket()
	s.r = webServer.Router()

	// Rate limiters and auth service. Each Close / Stop is non-idempotent
	// (close(done) panics on second call), so wrap them all in a single
	// sync.Once-guarded closeLimiters helper that both the normal deferred
	// shutdown AND the force-exit timer below can safely invoke.
	apiRL := web.NewRateLimiter(rateLimitAPIPerMinute, time.Minute)
	potRL := web.NewRateLimiter(rateLimitPOTPerMinute, time.Minute)
	authSvc := web.NewAuthService()
	authSvc.SetLogger(log)
	authSvc.Start()
	loginRL := web.NewRateLimiter(rateLimitLoginPerMinute, time.Minute)
	passwordRL := web.NewRateLimiter(rateLimitPasswordPerMinute, time.Minute)
	s.apiRL = apiRL
	s.potRL = potRL
	s.authSvc = authSvc
	s.loginRL = loginRL
	s.passwordRL = passwordRL

	var limitersCloseOnce sync.Once
	s.closeLimiters = func() {
		limitersCloseOnce.Do(func() {
			apiRL.Close()
			potRL.Close()
			loginRL.Close()
			passwordRL.Close()
			authSvc.Stop()
		})
	}

	// Wire auth middleware for external connections
	webServer.SetAuth(authSvc)
	s.r.Use(webServer.AuthMiddleware)

	// Shared closure: determines which platforms are active for cookie status display.
	s.getActivePlatforms = func() map[string]bool {
		var yt, tw bool
		s.configStore.Read(func(c *config.MoomboxConfig) {
			yt, tw = config.GetActivePlatforms(c)
		})
		return map[string]bool{"youtube": yt, "twitch": tw}
	}

	// Shared callback to kick all monitors when channels change.
	// Wakes monitors that went idle when they had no channels of their type.
	s.kickMonitors = func() {
		feedMon.CheckNow()
		decapiMon.CheckNow()
		twitchMon.CheckNow()
	}

	// Restart dispatcher — used by setup/update/API/TUI to unwind the run()
	// loop via cancel + quitTUI and return true from run().
	//
	// StartDrain flips the web server into a 503-for-new-requests mode
	// before the context is cancelled, then a 5-second grace timer lets
	// in-flight setup-wizard / save-config requests complete before the
	// hard shutdown. Audit reports/cmd-moombox.md C-main:165-166.
	s.triggerRestart = func(source string) {
		log.Info("Restart requested", slog.String("source", source))
		s.restartRequested.Store(true)
		if s.webServer != nil {
			s.webServer.StartDrain()
		}
		time.AfterFunc(5*time.Second, func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("restart drain timer panic", slog.Any("panic", r))
				}
			}()
			s.cancel()
			if s.quitTUI != nil {
				s.quitTUI()
			}
		})
	}

	// TUI-only channels created here so routes_wiring can reference
	// tuiUpdateStatusCh in the UpdateRoutes OnFound closure before
	// tui_wiring assigns the consumer side.
	s.tuiUpdateStatusCh = make(chan tui.UpdateStatusMsg, 2)
	s.tuiDiskStatusCh = make(chan tui.DiskStatusMsg, 5)

	return nil
}

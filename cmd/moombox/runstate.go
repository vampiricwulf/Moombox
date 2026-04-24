package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// runState holds all shared state that used to live as locals in run(). It
// exists so the per-phase helpers extracted into services.go,
// routes_wiring.go, ws_wiring.go, monitor_callbacks.go, tui_wiring.go and
// shutdown.go can read and write without a 40-parameter signature. All
// fields are owned by run() for its lifetime; concurrent reads/writes on
// top of cfg still go through cfgMu (matching the prior contract).
type runState struct {
	// --- Immutable context / parameters ---
	ctx        context.Context
	cancel     context.CancelFunc
	configPath string
	useTUI     bool

	// --- Config + synchronisation ---
	cfg   *config.MoomboxConfig
	cfgMu sync.RWMutex

	// --- Infrastructure ---
	log *logger.Logger
	upd *updater.Updater
	db  *database.Database

	// --- Connectivity ---
	connMon *connectivity.Monitor

	// --- Platform services ---
	jar          *cookies.CookieJar
	ytService    *youtube.Service
	twService    *twitch.Service
	potProvider  *bgutils.PotProvider
	cipherSolver *cipher.Solver

	// --- Worker + notifications ---
	notifyMgr *notifications.Manager
	dlWorker  *worker.DownloadWorker
	trimSvc   *worker.TrimService

	// --- Monitors ---
	feedMon   *monitor.FeedMonitor
	decapiMon *monitor.DecapiMonitor
	twitchMon *monitor.TwitchMonitor

	// --- Cookie refresh ---
	cookieRefresh     *cookies.RefreshService
	autoCookieSvc     *cookies.AutoCookieService
	browserProfileDir string

	// --- Web server core (section 16) ---
	webServer  *web.Server
	wsHub      *web.WebSocketHub
	r          chi.Router
	apiRL      *web.RateLimiter
	potRL      *web.RateLimiter
	loginRL    *web.RateLimiter
	passwordRL *web.RateLimiter
	authSvc    *web.AuthService
	startTime  time.Time

	// --- Lifecycle control ---
	restartRequested atomic.Bool
	quitTUI          func() // set by TUI section; called by triggerRestart

	// --- Shared closures (populated by initServices) ---
	triggerRestart     func(source string)
	kickMonitors       func()
	getActivePlatforms func() map[string]bool

	// --- Atomic callback slots ---
	// Set before cookieRefresh.Start(); TUI section Store()s a concrete func
	// into authChangeTUI. cookieRefresh.OnAuthChange dispatches through
	// atomic.Pointer.Load so the refresh goroutine never races on field
	// reassignment.
	authChangeTUI atomic.Pointer[func(cookies.AuthStatus)]

	// --- Close-once wrappers ---
	// sync.Once-guarded so both the orderly deferred shutdown and the
	// 10-second force-exit timer can invoke them safely.
	closeLog      func()
	closeDB       func()
	closeLimiters func()

	// --- TUI-only channels (created in initServices so routes_wiring can
	// reference them before tui_wiring fires) ---
	tuiUpdateStatusCh chan tui.UpdateStatusMsg
	tuiDiskStatusCh   chan tui.DiskStatusMsg

	// --- Subscription handles (assigned by monitor_callbacks wiring; needed
	// by shutdown to unsubscribe cleanly before the database closes) ---
	logSub            chan string
	unsubWSJobUpdate  func()
	unsubWSJobsChange func()
}

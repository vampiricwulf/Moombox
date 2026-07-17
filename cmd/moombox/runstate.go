package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
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
// top of cfg go through configStore (DECISIONS #8).
type runState struct {
	// --- Immutable context / parameters ---
	ctx        context.Context
	cancel     context.CancelFunc
	configPath string
	useTUI     bool

	// --- Config + synchronisation ---
	cfg *config.MoomboxConfig
	// configStore owns the cfg synchronisation. Read/Update give a locked
	// snapshot or lock-mutate-validate-save pass; legacy direct mutators
	// (settings model big-block, services.go init writes) take the lock
	// via configStore.RWMutex(). Populated in initServices once cfg +
	// configPath are known.
	configStore *config.Store

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
	bgSidecar    *sidecar.Sidecar
	cipherSolver *cipher.GojaResolver
	routedCipher cipher.Solver

	// --- Worker + notifications ---
	notifyMgr *notifications.Manager
	dlWorker  *worker.DownloadWorker
	trimSvc   *worker.TrimService

	// --- Monitors ---
	feedMon   *monitor.FeedMonitor
	decapiMon *monitor.DecapiMonitor
	twitchMon *monitor.TwitchMonitor
	// backfillWorker is the §11 feed-history backfill: the serial scan
	// queue + in-flight set driven by feedMon.BackfillSweep. Started in
	// run() just before the monitors; winds down with the run context (no
	// explicit Stop — cancellation is observed within one page).
	backfillWorker *monitor.BackfillWorker

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
	// quitTUI is written by runTUI on the main goroutine and read by
	// triggerRestart's grace-timer goroutine — atomic (like the callback
	// slots below) so a restart-triggering request that lands before
	// runTUI starts neither races nor is silently dropped.
	quitTUI atomic.Pointer[func()]

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

	// Same pattern for monitor OnSchedule: each monitor's goroutine loads
	// the atomic pointer on every schedule event; the TUI wiring later
	// Store()s a concrete wrapper that pushes into the CheckTimersCh channel.
	feedTUISchedule   atomic.Pointer[func(int64)]
	decapiTUISchedule atomic.Pointer[func(int64)]
	twitchTUISchedule atomic.Pointer[func(int64)]

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
	logSub              chan string
	unsubWSJobUpdate    func()
	unsubWSJobAdded     func()
	unsubWSJobDeleted   func()
	unsubWSTrimsChanged func()
	unsubWSJobsChange   func()
}

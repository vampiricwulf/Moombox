package cookies

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// cookiesHTTPClient performs YouTube auth-check + Twitch OAuth refresh
// against the shared httpx transport. Keep-alive across the auth-check
// + refresh round trip amortises the TLS handshake.
var cookiesHTTPClient = httpx.Client(30 * time.Second)

// Package vars, not consts, solely so tests can point them at an httptest
// server — these functions have no other seam (see refresh.go's note that
// the pure predicates were extracted for exactly this reason).
var (
	youtubeGuideURL        = "https://www.youtube.com/youtubei/v1/guide"
	youtubeGuideRefreshURL = "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false"
	twitchValidateURL      = "https://id.twitch.tv/oauth2/validate"
)

const (
	defaultRefreshInterval = 30 * time.Minute
	authCheckTimeout       = 15 * time.Second

	// livenessRefireWindow bounds how often ONE platform's logged-out
	// liveness verdict may clear the dedupe and reach OnRecoveryNeeded.
	//
	// The membership probe runs once per configured channel per feed cycle,
	// with a 500ms stagger between channels, so a dead session produces N
	// verdicts inside a couple of seconds.
	//
	// What N un-deduped verdicts cost is NOT N headless browsers.
	// AutoCookieService.RefreshCookiesDetailed single-flights on its
	// refreshCmd sentinel, so the first call claims the slot and every call
	// that arrives while it runs returns refreshDeclined() immediately. The
	// damage is that a decline is RefreshResult{}, whose zero-value verdict is
	// RefreshUnknown, which lands in runCookieRecovery's default branch and
	// sends "Cookie Auto-Refresh Ineffective" — a warning about a condition
	// Moombox created by racing itself. That notification stamps
	// lastAuthFailNotify, so when the one real attempt finishes ~2 minutes
	// later and genuinely fails, its accurate and actionable "Cookie
	// Auto-Refresh Failed" is inside the 30-minute cooldown and never sent.
	// The operator is left with the vague message instead of the useful one.
	//
	// So this window protects the QUALITY of what the operator is told, not
	// the machine's workload. That is the more valuable thing of the two.
	//
	// Its own constant on purpose. It is NOT the notification cooldown in
	// cmd/moombox's wireMonitorCallbacks, and it is NOT defaultRefreshInterval
	// above, however the three numbers happen to line up today. It is set to
	// match that cooldown so the two coalescing windows do not drift apart —
	// not because either implies the other.
	livenessRefireWindow = 30 * time.Minute

	// livenessFreshWindow bounds how old the last conclusive liveness
	// observation may be before a periodic refresh pays for the
	// FallbackLiveness probe. That probe is a full first-party page fetch;
	// an install whose membership probe is already reporting gets the same
	// answer for free every feed cycle and must not buy a second one.
	//
	// The upper bound is a real invariant. It must be strictly SHORTER than
	// defaultRefreshInterval, because the fallback records its own answer
	// through the same method. At one full cadence the fallback's own stamp
	// would still read as fresh on the next tick and the probe would quietly
	// suppress itself on alternate cycles — halving a coverage nobody decided
	// to halve. TestFallbackObservationAgesOutWithinOneCadence pins it.
	//
	// The lower bound is an ASSUMPTION about configuration, not an invariant,
	// and it is worth being exact about because nothing enforces it. The skip
	// only works while membership observations arrive more often than this
	// window expires. monitors.feed_check_interval defaults to 10 minutes but
	// validates to 1..1440, so any install that sets it above ~25 minutes lets
	// the observation age out between refreshes and pays for the fallback on
	// roughly every other cycle — the very cost the skip exists to remove.
	// TestFallbackSkipCoversTheDefaultFeedCadence pins the default case.
	//
	// That degradation is bounded and one-directional: an extra page fetch per
	// cycle on a slow-polling install. It is not a correctness problem, which
	// is why it is a documented assumption rather than a constraint plumbed
	// through from config — internal/cookies cannot see monitors config, and
	// deriving this from it would couple the cookie subsystem to the monitor's
	// schedule for a cost difference measured in one HTTP request per hour.
	livenessFreshWindow = 25 * time.Minute

	// youtubeClientVersion is the WEB client version sent in Innertube API requests.
	// Update this when YouTube bumps the client version — it's used in auth check
	// and session refresh requests. Format: "2.YYYYMMDD.00.00".
	youtubeClientVersion = "2.20260708.00.00"

	// authBodyFallbackLimit caps how much of the response body we promote to a
	// Go string for the JSON-parse-failed fallback path. The real
	// `"logged_in":"1"` / `"loggedIn":true` markers live in the first hundreds
	// of bytes of the responseContext block; scanning past 16KB only inflates
	// memory and increases the surface for accidentally logging cookies or
	// session tokens that may appear deeper in the payload (audit
	// reports/cookies.md #24).
	authBodyFallbackLimit = 16 << 10
)

// cookieUpdate holds a parsed Set-Cookie value, expiry, and authoritative
// domain. Domain is captured from the Set-Cookie Domain= attribute so new
// rows can be written under the correct host instead of guessing from the
// cookie name.
type cookieUpdate struct {
	Value  string
	Expiry int64
	Domain string // e.g. ".youtube.com" — from Set-Cookie Domain= when present
}

// AuthStatus tracks the authentication state for each platform.
type AuthStatus struct {
	YouTubeAuthenticated bool   `json:"youtubeAuthenticated"`
	TwitchAuthenticated  bool   `json:"twitchAuthenticated"`
	HasYouTubeCookies    bool   `json:"hasYouTubeCookies"`
	LastCheck            string `json:"lastCheck,omitempty"`
	YouTubeError         string `json:"youtubeError,omitempty"`
	TwitchError          string `json:"twitchError,omitempty"`
}

// RefreshService periodically reloads and validates cookies.
type RefreshService struct {
	mu              sync.RWMutex
	jar             *CookieJar
	cancel          context.CancelFunc
	status          AuthStatus
	refreshInterval time.Duration

	// Track previous auth state to detect auth → no-auth transitions.
	prevYouTubeAuth bool
	prevTwitchAuth  bool
	hasCheckedOnce  bool

	// ytEverConcluded / twEverConcluded track, per platform, whether THAT
	// platform has ever completed a conclusive (checkErr == nil) check.
	// This is deliberately NOT the same thing as hasCheckedOnce, which is
	// service-wide: Cookies.Platforms is a monotonic per-platform union
	// that only grows on successful verification, so SetExpectedPlatforms
	// can seed hasCheckedOnce=true from YouTube's presence alone while
	// Twitch cookies exist on disk but were never verified. Using the
	// shared hasCheckedOnce for the "first conclusive check" decision in
	// shouldFireRecovery would then treat Twitch's actual first check as a
	// "subsequent" one (prevTwitchAuth is the false zero value, so the
	// witnessed-transition condition never fires either) — silently
	// swallowing recovery for any platform absent from the persisted list
	// while a sibling platform is present. See shouldFireRecovery.
	ytEverConcluded bool
	twEverConcluded bool

	// prevYouTubeIdentity is the jar's YouTubeIdentity() as of the last
	// conclusive AND authenticated check — the baseline shouldObserveCredentials
	// compares against. See advanceIdentityBaseline for why an unauthenticated
	// check must not move it.
	//
	// Process-local, and that is not where correctness lives: the durable
	// record is each parked job's own park_identity, so this baseline only
	// decides WHEN to look, never WHAT moves. Its zero value therefore fires
	// once per process rather than staying silent — which is how an offline
	// cookie swap (stop, replace, start) gets noticed at all.
	//
	// YouTube only. Twitch's auth-token rotates on Twitch's schedule (see
	// twitch.ErrTwitchAuthExpired), so it is not a stable account
	// discriminator — and no Twitch failure produces a membership park, so
	// there is nothing on that side for the signal to unlock.
	prevYouTubeIdentity string

	// lastLivenessObserved records the last CONCLUSIVE external liveness
	// observation per platform, in BOTH directions. Consulted by exactly one
	// thing — the FallbackLiveness skip — which asks "has anything told us
	// about this session recently?", a question a healthy answer settles just
	// as well as a dead one.
	//
	// Deliberately a different map from lastRecoveryDecided below. One map
	// serving both questions cannot answer either: a healthy observation has
	// to make the fallback stand down, and it must not be able to swallow a
	// dead verdict that lands a moment later.
	lastLivenessObserved map[string]time.Time

	// lastRecoveryDecided records when a platform's recovery last cleared the
	// dedupe — from a logged-out liveness verdict in ObserveLiveness, or from
	// the tier-1 auth check in doRefresh, which stamps it so a dead session
	// cannot fire recovery twice in one pass. See livenessRefireWindow for
	// what a redundant fire actually costs: not a second browser, but a
	// spurious "Ineffective" notification that suppresses the real one.
	//
	// "Decided", not "fired": while livenessRecoveryArmed is false a cleared
	// liveness verdict is logged rather than acted on, so the stamp records
	// the decision this map exists to de-duplicate and not, in that case, a
	// call that happened. A LoggedIn observation must never write here.
	lastRecoveryDecided map[string]time.Time

	// lastLivenessVerdict is the previous verdict per platform, kept solely to
	// decide the LOG LEVEL of the next one (see ObserveLiveness). It steers no
	// behaviour: absence means "never observed in this process", which reads
	// as notable, so the worst a missing entry can do is emit one extra line.
	lastLivenessVerdict map[string]bool

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// OnAuthChange is called when auth status changes.
	OnAuthChange func(status AuthStatus)

	// OnRecoveryNeeded is called when a platform transitions from
	// authenticated to not-authenticated due to genuine auth loss (not
	// a network error and not a never-authenticated state). The platform
	// parameter is "youtube" or "twitch".
	OnRecoveryNeeded func(platform string)

	// OnAuthRecovered is called when a platform transitions from
	// not-authenticated to authenticated (the inverse of OnRecoveryNeeded).
	// Useful for waking jobs that were parked in the COOKIES? status.
	OnAuthRecovered func(platform string)

	// OnCredentialsChanged reports the platform's current working account
	// identity when it may have changed, so parked jobs can be re-evaluated
	// against it. The identity is an opaque equality token (see
	// CookieJar.YouTubeIdentity) — never log or display it.
	//
	// This is not a weaker OnAuthRecovered; it catches a case that one
	// structurally cannot. A job parked because the signed-in account lacks a
	// channel membership parked while auth was HEALTHY, so swapping to the
	// account that holds the membership produces no
	// not-authenticated → authenticated transition to ride. Without this
	// signal such a job has no automatic resume trigger at all.
	//
	// Fires for "youtube" only — see prevYouTubeIdentity for why Twitch has
	// no usable identity signal and nothing that would need one.
	OnCredentialsChanged func(platform, identity string)

	// FallbackLiveness is a channel-independent YouTube liveness probe,
	// injected by cmd/moombox rather than called directly, because this
	// package cannot import internal/youtube — internal/youtube/auth.go
	// imports this one, so the direct call would be an import cycle. The
	// injection matches what VerifyYouTubeAuth, HasActiveJobs and
	// ConfiguredBrowserOverride already do for the same reason.
	//
	// Called at the tail of a PERIODIC refresh, and only when no liveness
	// observation has arrived within livenessFreshWindow: the membership
	// probe already answers this for a normally-configured install, for free,
	// every feed cycle. The CheckNow path never calls it — that path runs
	// synchronously on an HTTP handler.
	//
	// conclusive == false means the probe learned nothing (a consent wall, a
	// rate limit, a transport failure) and MUST NOT move any state. Only a
	// conclusive answer is passed on to ObserveLiveness.
	FallbackLiveness func(ctx context.Context) (loggedIn, conclusive bool)
}

// livenessRecoveryArmed gates whether an external liveness verdict may
// actually invoke OnRecoveryNeeded.
//
// It is false, and that is the entire point of this landing. What arming would
// actually do, on BOTH install shapes — cmd/moombox's handleRecoveryNeeded
// splits on cookies.auto_enabled and neither arm is silent about a session it
// cannot restore:
//
//   - auto_enabled = true: a goroutine runs RefreshCookiesDetailed under a
//     2-minute timeout, which drives a headless browser. Only a successful
//     refresh is quiet; the other two verdicts notify — "Cookie Auto-Refresh
//     Failed" (TypeError) or "Cookie Auto-Refresh Ineffective" (TypeWarning) —
//     and a spurious verdict is by definition one no refresh can fix.
//   - auto_enabled = false: no browser, and no quiet case at all. A SYNCHRONOUS
//     "Cookie Re-Authentication Required" (TypeError) naming the cookie file,
//     every time. This arm used to Debug-log and send nothing; Task 7 replaced
//     that silence, so arming now alarms the population this arc elsewhere
//     identifies as LEAST able to reach the remedy it names — containers,
//     remote dashboards, a loopback-gated setup wizard.
//
// A per-platform 30-minute cooldown in wireMonitorCallbacks bounds how often
// that repeats; it does not withhold the first one.
//
// So the risk of arming is NOT scoped to auto_enabled installs, and the reason
// to stage it is not the browser — the disabled shape is if anything the worse
// of the two, because the operator it pages has no automated attempt that might
// have quietly fixed things first. It is that a false LoggedOut sends an
// operator to re-export credentials that were never wrong, on every install
// shape, and these verdicts have never been in the health path before. They
// therefore run log-only first: the observation, the dedupe and the freshness
// accounting all happen and are logged, and only the last step is withheld.
// Flipping this to true is a deliberate, separate change — not a side effect of
// wiring something else.
const livenessRecoveryArmed = false

// NewRefreshService creates a new cookie refresh service.
// If refreshInterval is zero, the default of 30 minutes is used.
func NewRefreshService(jar *CookieJar, refreshInterval time.Duration, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *RefreshService {
	interval := refreshInterval
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	return &RefreshService{
		jar:                  jar,
		refreshInterval:      interval,
		logger:               logger,
		lastLivenessObserved: make(map[string]time.Time),
		lastRecoveryDecided:  make(map[string]time.Time),
		lastLivenessVerdict:  make(map[string]bool),
	}
}

// SetExpectedPlatforms seeds the previous auth state from persisted platforms
// so that auth loss can be detected even if the app restarts after cookies expire.
// Call this before Start().
func (rs *RefreshService) SetExpectedPlatforms(platforms []string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	for _, p := range platforms {
		switch p {
		case "youtube":
			rs.prevYouTubeAuth = true
			rs.ytEverConcluded = true
		case "twitch":
			rs.prevTwitchAuth = true
			rs.twEverConcluded = true
		}
	}
	// If we have persisted platforms, consider the first check as a "subsequent"
	// check so that auth loss transitions can fire immediately.
	if len(platforms) > 0 {
		rs.hasCheckedOnce = true
	}
}

// Start begins the cookie refresh loop.
func (rs *RefreshService) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	rs.mu.Lock()
	rs.cancel = cancel
	rs.mu.Unlock()

	// Initial check. allowFallback is false, for exactly the reason CheckNow's
	// is: this call runs SYNCHRONOUSLY on the caller's goroutine, and
	// cmd/moombox's run() blocks on it before the web server binds. At startup
	// nothing has observed liveness yet, so the freshness skip cannot help —
	// every install with a YouTube auth cookie would pay a full page fetch (up
	// to livenessFetchTimeout, 20s in internal/youtube) ahead of the dashboard
	// coming up, on every start. Config changes restart the process, so that
	// would be one delayed startup per settings tweak.
	//
	// The cost of skipping it is that tier-2 coverage begins one cadence in
	// rather than immediately, which is the cheaper of the two.
	rs.refresh(ctx, false)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				rs.logger.Error("cookie refresh goroutine panic", "panic", r)
			}
		}()

		ticker := time.NewTicker(rs.refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rs.doRefresh(ctx)
			}
		}
	}()

	rs.logger.Info("cookie refresh service started",
		"interval", rs.refreshInterval.String())
}

// Stop stops the cookie refresh service.
func (rs *RefreshService) Stop() {
	rs.mu.Lock()
	cancel := rs.cancel
	rs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rs.logger.Info("cookie refresh service stopped")
}

// GetStatus returns the current auth status.
func (rs *RefreshService) GetStatus() AuthStatus {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.status
}

// CheckNow triggers an immediate cookie refresh and auth check.
//
// allowFallback is false: POST /api/cookies/recheck runs this synchronously on
// the HTTP handler goroutine, and the fallback probe is a full page fetch on
// top of the auth check that is already there. The periodic path owns that
// probe; a button press does not need to buy one.
func (rs *RefreshService) CheckNow(ctx context.Context) {
	rs.refresh(ctx, false)
}

// ObserveLiveness records an external verdict about whether `platform`'s
// stored session is still signed in, and — once livenessRecoveryArmed is true
// — fires OnRecoveryNeeded for a signed-out one.
//
// Callers must filter their own inconclusive results out: reaching this method
// means "YouTube told us", not "we asked". A consent wall, a rate limit, an
// off-host redirect and a never-configured jar are all silence, and passing
// any of them in as loggedIn=false would report working credentials as dead.
//
// Two producers exist today, both YouTube: the per-channel membership probe
// (which runs once per configured channel per feed cycle) and the
// channel-independent FallbackLiveness probe. The first is why the dedupe is
// not optional — one dead session must raise one alarm, not one per channel.
func (rs *RefreshService) ObserveLiveness(platform string, loggedIn bool) {
	due, notable := rs.recordLiveness(platform, loggedIn, time.Now())

	// While the pilot is disarmed this line is the ONLY evidence of what the
	// new signal would have done, so the level is chosen to keep every line
	// that evidence needs at Info while a healthy install stays quiet.
	//
	// Notable (Info) is every signed-out verdict, every change of verdict, and
	// the first observation of the process. Everything else is a repeat of an
	// answer already on the record, and repeats are the volume problem: this
	// method is called once per configured channel per feed cycle, which at
	// the default 10-minute cadence is 144*N lines a day — every one of them
	// also fanned out over the WebSocket log stream to the Web UI and TUI. A
	// healthy install now emits roughly one line per process instead.
	//
	// A signed-out verdict is never demoted, even when the dedupe already
	// refused it. Losing evidence of a dead session is the one direction this
	// must not fail in, and wouldFireRecovery on the line says which of the
	// burst cleared the dedupe.
	//
	// The line carries the verdict and the two decisions — never anything read
	// off the page the verdict came from.
	logAt := rs.logger.Debug
	if notable {
		logAt = rs.logger.Info
	}
	logAt("liveness observation",
		"platform", platform,
		"loggedIn", loggedIn,
		"wouldFireRecovery", due,
		"armed", livenessRecoveryArmed)

	if !livenessRecoveryArmed || !due {
		return
	}
	if fn := rs.OnRecoveryNeeded; fn != nil {
		// States what this method was told, and stops there. ObserveLiveness
		// has two producers — the per-channel membership probe and the
		// channel-independent fallback — and cannot tell which sent this
		// verdict. This is the line that will page an operator the day the
		// gate flips, so it must not name a mechanism it cannot know.
		rs.logger.Warn("a liveness observation reports this platform is signed out, triggering recovery", "platform", platform)
		fn(platform)
	}
}

// recordLiveness folds one conclusive observation into the liveness maps and
// reports two independent things about it:
//
//   - recoveryDue: it is signed out and cleared the dedupe, so it warrants
//     firing OnRecoveryNeeded.
//   - notable: it is worth an operator-visible log line — signed out, or a
//     change from this platform's previous verdict, or the first observation
//     of the process. See ObserveLiveness for why the distinction exists.
//
// Split out of ObserveLiveness so both decisions are testable on their own,
// upstream of the pilot gate that currently suppresses the call.
//
// `now` is a parameter so a test can drive the windows without sleeping
// through them.
//
// The lock is released before ObserveLiveness invokes the callback, following
// doRefresh's convention: OnRecoveryNeeded reaches out into cmd/moombox and
// must not run under this service's mutex.
func (rs *RefreshService) recordLiveness(platform string, loggedIn bool, now time.Time) (recoveryDue, notable bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.lastLivenessObserved == nil {
		rs.lastLivenessObserved = make(map[string]time.Time)
	}
	if rs.lastLivenessVerdict == nil {
		rs.lastLivenessVerdict = make(map[string]bool)
	}

	// Read the previous verdict BEFORE this one overwrites it. No entry means
	// this platform has never been observed in this process, which is itself
	// worth a line: it is the record that the signal started producing.
	prev, seen := rs.lastLivenessVerdict[platform]
	notable = !loggedIn || !seen || prev != loggedIn

	// Both directions. This map answers "did anything tell us recently", and
	// a healthy answer settles that question exactly as well as a dead one.
	rs.lastLivenessObserved[platform] = now
	rs.lastLivenessVerdict[platform] = loggedIn

	if loggedIn {
		// Positive evidence is silent, and must not touch lastRecoveryDecided:
		// stamping it here would let a healthy verdict swallow a dead one
		// arriving a moment later from another channel in the same cycle.
		return false, notable
	}
	if last, ok := rs.lastRecoveryDecided[platform]; ok && now.Sub(last) < livenessRefireWindow {
		return false, notable
	}
	if rs.lastRecoveryDecided == nil {
		rs.lastRecoveryDecided = make(map[string]time.Time)
	}
	rs.lastRecoveryDecided[platform] = now
	return true, notable
}

// noteRecoveryDecided stamps the dedupe map for a recovery that the tier-1
// auth check is about to fire.
//
// One-directional on purpose: the refresh stamps the map so a liveness verdict
// arriving in the same window cannot fire recovery for a problem the tier-1
// check is already working on, but it does not CONSULT the map. Suppressing
// the tier-1 fire would change behaviour that predates this signal entirely,
// and that check is the one with the longest field record.
//
// The second fire would not launch a second browser — RefreshCookiesDetailed
// single-flights — it would be DECLINED, and a decline is what produces the
// spurious "Ineffective" notification that then suppresses the real verdict.
// livenessRefireWindow carries the full chain.
func (rs *RefreshService) noteRecoveryDecided(platform string, now time.Time) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.lastRecoveryDecided == nil {
		rs.lastRecoveryDecided = make(map[string]time.Time)
	}
	rs.lastRecoveryDecided[platform] = now
}

// livenessObservedRecently reports whether any conclusive liveness observation
// for `platform` landed within livenessFreshWindow — the sole gate on paying
// for the FallbackLiveness probe.
func (rs *RefreshService) livenessObservedRecently(platform string, now time.Time) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	last, ok := rs.lastLivenessObserved[platform]
	return ok && now.Sub(last) < livenessFreshWindow
}

// CheckYouTubeAuth checks whether the current YouTube cookies are authenticated
// by making a guide API request. This is the public entry point for external
// callers (e.g. AutoCookieService verification).
func (rs *RefreshService) CheckYouTubeAuth(ctx context.Context) (bool, error) {
	return rs.checkYouTubeAuth(ctx)
}

// CheckTwitchAuth checks whether the current Twitch auth token is valid
// by calling the Twitch validation endpoint.
func (rs *RefreshService) CheckTwitchAuth(ctx context.Context) (bool, error) {
	return rs.checkTwitchAuth(ctx)
}

// doRefresh is the TICKER refresh, and the only path allowed to pay for the
// FallbackLiveness probe. Both of the other entry points run synchronously on
// a goroutine somebody is waiting on — CheckNow on an HTTP handler, Start's
// initial check ahead of the web server binding — and both pass
// allowFallback=false for that reason.
func (rs *RefreshService) doRefresh(ctx context.Context) {
	rs.refresh(ctx, true)
}

// refresh is the shared body of both entry points; allowFallback is the only
// thing that separates them. Commentary elsewhere in this file that names
// doRefresh is describing this body — the split is newer than the comments.
func (rs *RefreshService) refresh(ctx context.Context, allowFallback bool) {
	rs.logger.Debug("refreshing cookies")

	// Reload cookies from file
	if err := rs.jar.Reload(); err != nil {
		rs.logger.Warn("cookie reload failed", "err", err)
	}

	// Check YouTube auth and refresh session cookies in a single request
	// Returns: (authenticated bool, err error)
	//   err != nil       => network/request error (not auth loss)
	//   false, nil       => genuine auth failure or no cookies
	ytAuth, ytErr := rs.checkAndRefreshYouTube(ctx)
	ytErrStr := ""
	if ytErr != nil {
		ytErrStr = ytErr.Error()
		rs.logger.Debug("youtube auth check failed", "err", ytErr)
	}

	// Check Twitch auth
	twAuth, twErr := rs.checkTwitchAuth(ctx)
	twErrStr := ""
	if twErr != nil {
		twErrStr = twErr.Error()
		rs.logger.Debug("twitch auth check failed", "err", twErr)
	}

	rs.mu.Lock()
	prevStatus := rs.status
	prevYT := rs.prevYouTubeAuth
	prevTW := rs.prevTwitchAuth
	hasChecked := rs.hasCheckedOnce
	ytConcluded := rs.ytEverConcluded
	twConcluded := rs.twEverConcluded

	// Captured once here (not re-read at the shouldFireRecovery call sites
	// below) so the "cookies present" snapshot lines up with the rest of
	// this check's other snapshots, all taken under the same lock.
	//
	// "Was this platform ever configured", NOT "is the set complete right
	// now". shouldFireRecovery's first-check branch returns this value, and
	// the complete-set predicates cannot tell a never-configured platform
	// from one whose LOGIN_INFO YouTube has cleared, or from a Twitch session
	// whose HttpOnly auth-token an exporter dropped — the exact states that
	// must be reported, and that were silent forever.
	hasYTCookies := rs.jar.HasAnyYouTubeAuthCookie()
	hasTWCookies := rs.jar.HasAnyTwitchAuthCookie()

	// Sampled under the same lock as the rest of this check's snapshots, and
	// AFTER the jar.Reload() at the top of doRefresh, so it reflects whatever
	// account is on disk right now.
	ytIdentity := rs.jar.YouTubeIdentity()
	prevYTIdentity := rs.prevYouTubeIdentity

	rs.status = AuthStatus{
		YouTubeAuthenticated: ytAuth,
		TwitchAuthenticated:  twAuth,
		// Now "YouTube auth is configured" rather than "the cookie set is
		// complete", which is what the label this drives has always claimed.
		// A half-cleared jar consequently renders as configured-but-unverified
		// instead of as no-cookies-at-all — see AuthStatus.HasYouTubeCookies.
		HasYouTubeCookies: hasYTCookies,
		LastCheck:         time.Now().UTC().Format(time.RFC3339),
		YouTubeError:      ytErrStr,
		TwitchError:       twErrStr,
	}

	// Update previous auth state tracking.
	// Only update previous state when the check was conclusive (no network error).
	// A network-error check deliberately does NOT mark the platform
	// "concluded" — the next conclusive check still counts as that
	// platform's first, so shouldFireRecovery's startup-dead-auth case
	// still applies to it.
	if ytErr == nil {
		rs.prevYouTubeAuth = ytAuth
		rs.ytEverConcluded = true
	}
	// Deliberately outside the ytErr == nil block above: the baseline advances
	// only on a check that also AUTHENTICATED, so a stale intermediate export
	// cannot consume the edge. See advanceIdentityBaseline.
	rs.prevYouTubeIdentity = advanceIdentityBaseline(rs.prevYouTubeIdentity, ytIdentity, ytAuth, ytErr)
	if twErr == nil {
		rs.prevTwitchAuth = twAuth
		rs.twEverConcluded = true
	}
	rs.hasCheckedOnce = true

	changed := rs.status.YouTubeAuthenticated != prevStatus.YouTubeAuthenticated ||
		rs.status.TwitchAuthenticated != prevStatus.TwitchAuthenticated
	// Snapshot under the lock: a concurrent doRefresh (ticker vs CheckNow)
	// writes rs.status under rs.mu, so reading it after Unlock is a race —
	// and the callback could observe a status newer than the transition
	// that triggered it.
	statusCopy := rs.status
	rs.mu.Unlock()

	if changed && rs.OnAuthChange != nil {
		rs.OnAuthChange(statusCopy)
	}

	// Detect auth loss transitions: previously authenticated -> not authenticated,
	// and the failure is genuine auth loss (err == nil), not a network error.
	//
	// Startup case: auth already dead when the process began never produces
	// a witnessed transition, so it previously stayed silent forever
	// (field case 2026-08-20: youtube=false on every check, all day, no
	// recovery, no notification). The first CONCLUSIVE check that finds a
	// platform unauthenticated fires the same recovery path once;
	// subsequent checks return to transition-only. shouldFireRecovery
	// encodes both cases so the decision can be table-tested without a
	// network seam.
	//
	// Note this uses the PER-PLATFORM ytConcluded/twConcluded snapshots,
	// not the service-wide hasChecked: SetExpectedPlatforms seeds
	// hasCheckedOnce=true as soon as ANY platform is in the persisted
	// list, so using the shared flag here would treat a sibling platform's
	// presence as proof THIS platform was already checked, masking the
	// same silent-forever bug for whichever platform is absent from the
	// list (e.g. Platforms=["youtube"] with unverified Twitch cookies on
	// disk).
	//
	// Each fire stamps the shared dedupe map (noteRecoveryDecided) so a
	// liveness verdict landing in the same window — including the one the
	// fallback probe at the tail of this very pass may produce — does not
	// fire recovery again for a problem this one is already working on. A
	// redundant fire is declined by the auto-cookie single-flight and reports
	// back as "Ineffective", which then suppresses the real verdict's
	// notification; see livenessRefireWindow.
	if rs.OnRecoveryNeeded != nil {
		if shouldFireRecovery(ytConcluded, prevYT, ytAuth, ytErr, hasYTCookies) {
			rs.noteRecoveryDecided("youtube", time.Now())
			rs.logger.Warn("youtube auth lost, triggering recovery")
			rs.OnRecoveryNeeded("youtube")
		}
		if shouldFireRecovery(twConcluded, prevTW, twAuth, twErr, hasTWCookies) {
			rs.noteRecoveryDecided("twitch", time.Now())
			rs.logger.Warn("twitch auth lost, triggering recovery")
			rs.OnRecoveryNeeded("twitch")
		}
	}

	// Detect recovery transitions: previously not authenticated -> now authenticated.
	// Fired so callers can wake jobs parked in COOKIES? state.
	if hasChecked && rs.OnAuthRecovered != nil {
		if !prevYT && ytAuth && ytErr == nil {
			rs.logger.Info("youtube auth recovered")
			rs.OnAuthRecovered("youtube")
		}
		if !prevTW && twAuth && twErr == nil {
			rs.logger.Info("twitch auth recovered")
			rs.OnAuthRecovered("twitch")
		}
	}

	// Hand the current account identity to the sweep whenever it may have
	// changed. Deliberately independent of the auth-recovered transition above
	// — the case this exists for (a job blocked because the signed-in account
	// lacks a channel membership) parks while auth is healthy, so the
	// operator's fix produces no auth transition at all. Both can fire on the
	// same check when dead cookies are replaced by a different account's; the
	// sweeps they drive are idempotent, so the second finds nothing left.
	if rs.OnCredentialsChanged != nil && shouldObserveCredentials(prevYTIdentity, ytIdentity, ytAuth, ytErr) {
		rs.logger.Info("youtube account identity observed — re-evaluating parked jobs")
		rs.OnCredentialsChanged("youtube", ytIdentity)
	}

	// Tier 2: the channel-independent liveness probe, for the installs the
	// per-channel one cannot reach — no YouTube channels configured, or
	// membership discovery off everywhere. Skipped whenever something already
	// reported inside livenessFreshWindow, which is the normal case and is
	// what keeps a configured install from paying for a second full page
	// fetch every cycle.
	//
	// Runs inline on the ticker goroutine, which carries Start's inline
	// recover. Nothing is spawned, so there is no new recover obligation and
	// no overlap to guard against: the ticker coalesces missed ticks, and
	// neither synchronous entry point (CheckNow, Start's initial check)
	// reaches this branch.
	if allowFallback && rs.FallbackLiveness != nil && !rs.livenessObservedRecently("youtube", time.Now()) {
		// Only a conclusive answer moves anything. `false, false` is a consent
		// wall or a rate limit, not a dead session.
		if loggedIn, conclusive := rs.FallbackLiveness(ctx); conclusive {
			rs.ObserveLiveness("youtube", loggedIn)
		}
	}

	rs.logger.Debug("cookie refresh done",
		"youtube", ytAuth,
		"twitch", twAuth)
}

// shouldFireRecovery reports whether OnRecoveryNeeded should fire for a
// single platform's just-completed check. Pulled out of doRefresh as a pure
// function so the decision can be table-tested without a network seam
// (checkAndRefreshYouTube/checkTwitchAuth make real HTTP calls and have no
// stub hook).
//
// everConcluded and prevAuth are THIS PLATFORM's pre-check snapshot values
// (read under rs.mu before rs.ytEverConcluded/rs.twEverConcluded and
// rs.prev*Auth were updated for this check) — everConcluded must be
// per-platform, not the service-wide hasCheckedOnce, or one platform's
// presence in the persisted list masks a sibling platform that was never
// actually checked (see the ytEverConcluded/twEverConcluded field comment
// on RefreshService). nowAuth/checkErr are this check's result.
// cookiesPresent is whether THIS PLATFORM was ever configured — any auth
// cookie in the jar at all (jar.HasAnyYouTubeAuthCookie /
// jar.HasAnyTwitchAuthCookie), NOT whether the set is currently complete.
// The complete-set predicates read a half-cleared session as never
// configured, which is precisely how a dead platform stayed silent.
// Two cases fire:
//
//   - Witnessed transition: everConcluded is true and prevAuth was true —
//     the platform was authenticated on its previous conclusive check and
//     isn't now. Fires regardless of cookiesPresent — a REAL transition
//     from authenticated to not (cookies expired, wiped, or removed
//     entirely) is exactly what this case exists to catch.
//   - Startup dead-auth: everConcluded is false, meaning this is the first
//     conclusive check this platform has ever completed. Auth that was
//     already dead when the process started never produces a witnessed
//     transition (there's no "prev" state to fall from), so without this
//     case recovery silently never fires — field case 2026-08-20:
//     youtube=false on every half-hourly check all day, zero recovery
//     attempts, zero notifications. Gated on cookiesPresent (I6 fix): a
//     platform the user never configured has nowAuth=false and checkErr=nil
//     for the trivial reason that checkAndRefreshYouTube/checkTwitchAuth
//     return early on an empty jar — that is NOT dead auth, and firing
//     startup recovery for it launches a spurious headless-browser
//     credential-recovery attempt (and possibly a user-facing warning) for
//     a platform nobody set up. Dead-but-PRESENT cookies still fire —
//     that's the whole point of this case; only the never-configured
//     (absent) case is excluded. "Present" is deliberately the loose
//     any-auth-cookie test: a half-cleared session is a configured platform
//     with broken credentials, and reporting it is the point.
//
// In both cases checkErr must be nil (a network error is not auth loss) and
// nowAuth must be false (the platform must actually be unauthenticated).
func shouldFireRecovery(everConcluded, prevAuth, nowAuth bool, checkErr error, cookiesPresent bool) bool {
	if checkErr != nil || nowAuth {
		return false
	}
	if everConcluded {
		return prevAuth // witnessed transition
	}
	return cookiesPresent // first conclusive check — only for a configured platform
}

// shouldObserveCredentials reports whether OnCredentialsChanged should fire
// for a just-completed YouTube check. Pulled out of doRefresh as a pure
// function for the same reason shouldFireRecovery was: the network calls above
// it have no stub seam, so this is the only way to table-test the decision.
//
// This is a WAKE-UP, not the resume decision. What actually moves a job is the
// comparison between the identity it recorded when it parked and the current
// one (see cmd/moombox's sweepShouldResume), which is durable and level-based.
// That is what lets this predicate stay a cheap edge filter: a missed edge
// costs a delay until the next account change or restart, never a permanent
// strand.
//
// baseline is the fingerprint from the last conclusive AND authenticated
// check (see RefreshService.prevYouTubeIdentity); nowIdentity, nowAuth and
// checkErr are this check's results. Three things must hold:
//
//   - checkErr == nil. A network error means we learned nothing; the identity
//     may be fine and the auth answer is meaningless.
//   - nowAuth && nowIdentity != "". Credentials that don't authenticate are
//     not a fix, and an empty fingerprint compares unequal to every real one,
//     so firing on it would wake the sweep on every cookie-less cycle. This is
//     NOT deferring to OnAuthRecovered: that sweep skips membership parks by
//     design, so nothing else would pick them up. The reason it is safe is
//     that advanceIdentityBaseline holds the baseline here, leaving the edge
//     intact for the moment those same credentials start working.
//   - baseline == "" (the first authenticated observation of this process —
//     see below) or the two differ.
//
// The baseline == "" case fires ON PURPOSE. An operator who stops Moombox,
// replaces the cookies and starts it again produces no in-process transition
// at all, so a start-up that stayed silent could never see an offline swap.
// Firing here is safe precisely because the per-job comparison decides what
// moves: on an unchanged cookie file every parked job matches the current
// identity and nothing happens.
func shouldObserveCredentials(baseline, nowIdentity string, nowAuth bool, checkErr error) bool {
	if checkErr != nil || !nowAuth || nowIdentity == "" {
		return false
	}
	return baseline == "" || baseline != nowIdentity
}

// advanceIdentityBaseline returns the baseline to carry into the next check.
//
// It advances ONLY on a check that was both conclusive and authenticated —
// never on one that merely concluded. Advancing on a conclusive-but-dead check
// consumes the edge and strands exactly the job class this mechanism serves:
//
//  1. a membership park sits under account A, auth healthy;
//  2. the operator drops in account B's export, which is already stale
//     (routine — Moombox's own advice warns that browsing on in the source
//     profile invalidates an earlier export). Conclusive check, not
//     authenticated;
//  3. the operator re-exports B properly and it works.
//
// Had step 2 moved the baseline to B, step 3 would compare B against B and
// never fire — and OnAuthRecovered deliberately skips membership parks, so
// nothing else would pick the job up. Holding the baseline at A makes step 3
// the account change it actually is.
//
// This also cannot loop: firing requires nowAuth, so an account that stays
// broken never fires however many times it is observed, and one that starts
// working fires exactly once before becoming the new baseline.
func advanceIdentityBaseline(baseline, nowIdentity string, nowAuth bool, checkErr error) string {
	if checkErr != nil || !nowAuth {
		return baseline
	}
	return nowIdentity
}

// setYouTubeHeaders applies the standard YouTube API headers for cookie-authenticated requests.
func setYouTubeHeaders(req *http.Request, cookieHeader, origin, authHeader string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Origin", origin)
}

// youtubeGuideRequestBody returns the standard Innertube WEB request body
// for /youtubei/v1/guide. Centralised here so a clientVersion bump only
// touches one site (audit reports/cookies.md #35).
func youtubeGuideRequestBody() string {
	return `{"context":{"client":{"clientName":"WEB","clientVersion":"` + youtubeClientVersion + `","hl":"en"}}}`
}

// authResponseIsOurs returns nil only when resp can be read as an answer about
// THIS install's credential, and otherwise an error saying which way it failed
// to qualify. `sent` is the request we dispatched; credentialHeader is the
// header carrying the credential ("Cookie" for YouTube, "Authorization" for
// Twitch).
//
// This is internal/youtube's livenessResponseIsOurs rule, ported rather than
// shared: that function is unexported, and this package must not import
// internal/youtube — internal/youtube/auth.go already imports this one, so the
// dependency only runs the other way. Any change to either should be made to
// both.
//
// Why the tier-1 checks need it at all. cookiesHTTPClient installs no
// CheckRedirect, so Go follows redirects; on the first hop to a different
// HOSTNAME it drops the manually-set credential header, and the decision is
// STICKY (client.go:620 declares stripSensitiveHeaders once before the redirect
// loop and only ever sets it inside at :688; nothing clears it on a later hop).
// So origin → wall → origin lands back on the host we asked for and delivers a
// body fetched with no credentials. Neither guide check nor the Twitch validate
// check looked at where the answer came from: any 200 whose body lacked both
// `"logged_in":"1"` and `"loggedIn":true` fell through to a CONCLUSIVE "not
// authenticated", and any 401 was Twitch's documented dead-token verdict. Both
// are what shouldFireRecovery acts on, and after Task 7 a fire notifies the
// operator on BOTH install shapes. Task 1 closed the non-200 half of this; a
// followed redirect never presents as non-200, so it could not catch this one.
//
// The trigger is an intercepting intermediary — captive portal, transparent or
// corporate proxy (http.ProxyFromEnvironment is on the shared transport at
// internal/httpx/client.go:40, and this package consults no connectivity gate).
//
// A provenance failure is INCONCLUSIVE — an error, matching what Task 1 and
// Task 1b established for a non-200 — never a verdict. It deliberately does NOT
// wrap ErrAuthCheckNotAttempted: a request did leave the process, so this is
// the "the site could not answer for us" unknown, not the "we could not form
// the question" one, and checkPlatformAuth tells those apart.
//
// The check is non-vacuous only because both callers refuse the empty-credential
// case BEFORE fetching (checkYouTubeAuth/checkAndRefreshYouTube on an empty
// cookie header, checkTwitchAuth on an empty token). Past that point the header
// was definitely set, so finding none on the answering request can only mean it
// was taken away.
//
// COUPLING, and it fails silently if broken: the header rule only means
// anything because cookiesHTTPClient has no http.CookieJar. With one installed
// the stdlib would re-add a Cookie header on the final hop from the jar's own
// scope rules, the check would pass on a request that never carried OUR
// session, and nothing here would fail. httpx.Client documents that property;
// TestCookiesHTTPClientCarriesNoCookieJar pins it for THIS client, as
// internal/utils' TestUtilsHTTPClientCarriesNoCookieJar does for the one the
// tier-2 probes use.
//
// Positive confirmation throughout, and deliberately STRICTER than the stdlib
// rule it defends against, so every disagreement resolves toward inconclusive:
//
//   - Host is compared as host:port, raw. Go compares URL.Hostname() —
//     port-stripped and permitting subdomains (isDomainOrSubdomain,
//     client.go:1028) — so a port change or a subdomain hop fails here while Go
//     would still forward the credential.
//   - Scheme is compared at all. Go's strip decision looks only at Host, so an
//     https→http downgrade on the same host keeps the credential; we refuse it
//     rather than read a verdict off an exchange made in clear.
//
// Errors name a host and a header NAME — never a header value, never response
// bytes. They reach AutoCookieService.setError and are rendered in the Web UI
// and TUI.
func authResponseIsOurs(resp *http.Response, sent *http.Request, credentialHeader string) error {
	if sent == nil || sent.URL == nil {
		return fmt.Errorf("could not determine what was asked")
	}
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return fmt.Errorf("could not determine what answered %s", sent.URL.Host)
	}
	final := resp.Request.URL
	if !strings.EqualFold(final.Scheme, sent.URL.Scheme) || !strings.EqualFold(final.Host, sent.URL.Host) {
		return fmt.Errorf("%s was answered by %s://%s; not an observation of this session",
			sent.URL.Host, final.Scheme, final.Host)
	}
	if resp.Request.Header.Get(credentialHeader) == "" {
		return fmt.Errorf("%s was answered by a request that no longer carried the %s header; not an observation of this session",
			sent.URL.Host, credentialHeader)
	}
	return nil
}

// checkYouTubeAuth asks YouTube whether the jar's session is still signed in.
//
// Its three entry gates appear identically in checkAndRefreshYouTube and
// encode one rule — the rule this subsystem kept getting wrong. Only the
// FIRST of them may answer (false, nil).
//
//   - Nothing configured at all — no session to have an opinion about, so a
//     silent "not authenticated" is the truth and shouldFireRecovery's
//     cookiesPresent gate (fed by the same predicate) keeps it silent.
//   - Configured but no request could be built — a check that did NOT happen.
//     (false, nil) would report it as dead credentials, so it errors instead.
//
// Everything in between now reaches the network. In particular a jar with
// SAPISID and a cleared LOGIN_INFO — YouTube's own rotation-invalidation
// state — is CONFIGURED with BROKEN credentials, and its verdict has to come
// from YouTube rather than from a missing name in a map.
func (rs *RefreshService) checkYouTubeAuth(ctx context.Context) (bool, error) {
	if !rs.jar.HasAnyYouTubeAuthCookie() {
		return false, nil // Nothing configured at all.
	}

	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, fmt.Errorf("youtube auth check: no cookie header could be built: %w", ErrAuthCheckNotAttempted)
	}

	origin := "https://www.youtube.com"
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, fmt.Errorf("youtube auth check: no SAPISIDHASH could be generated: %w", ErrAuthCheckNotAttempted)
	}

	// POST to YouTube guide endpoint to check auth
	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	body := youtubeGuideRequestBody()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideURL+"?prettyPrint=false", strings.NewReader(body))
	if err != nil {
		return false, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("youtube auth check: %w", err)
	}
	defer resp.Body.Close()

	// Before the status, for the same reason internal/youtube checks it first:
	// a redirected answer is not this session's answer whatever status it
	// carries, and naming the route is more accurate than naming the code.
	if err := authResponseIsOurs(resp, req, "Cookie"); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, fmt.Errorf("youtube auth check: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		// NOT (false, nil). That means "conclusively not authenticated" to
		// shouldFireRecovery, so a 429/503/edge block would be reported as
		// dead credentials. We learned nothing about the session here.
		return false, fmt.Errorf("youtube auth check: unexpected status %d", resp.StatusCode)
	}

	// YouTube always returns 200 even with invalid cookies — parse body
	// and check for authentication indicators in the structured response.
	var data struct {
		ResponseContext struct {
			ServiceTrackingParams []struct {
				Params []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"params"`
			} `json:"serviceTrackingParams"`
			MainAppWebResponseContext struct {
				LoggedIn bool `json:"loggedIn"`
			} `json:"mainAppWebResponseContext"`
		} `json:"responseContext"`
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return false, fmt.Errorf("read YouTube auth response: %w", err)
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		// Fallback to string matching if JSON parse fails. Cap the slice so
		// the resulting Go string never carries the full multi-MB payload —
		// the auth marker lives in the first few hundred bytes (#24).
		respStr := string(respBody[:min(len(respBody), authBodyFallbackLimit)])
		return strings.Contains(respStr, `"logged_in":"1"`) ||
			strings.Contains(respStr, `"loggedIn":true`), nil
	}

	// Primary: serviceTrackingParams contains logged_in across all Innertube responses
	for _, service := range data.ResponseContext.ServiceTrackingParams {
		for _, param := range service.Params {
			if param.Key == "logged_in" && param.Value == "1" {
				return true, nil
			}
		}
	}

	// Fallback: mainAppWebResponseContext.loggedIn
	if data.ResponseContext.MainAppWebResponseContext.LoggedIn {
		return true, nil
	}

	return false, nil
}

// checkAndRefreshYouTube makes a single guide API request to both check
// YouTube auth status and refresh session cookies from Set-Cookie headers.
// This avoids the redundancy of separate check + refresh requests.
func (rs *RefreshService) checkAndRefreshYouTube(ctx context.Context) (bool, error) {
	// See the gate commentary above checkYouTubeAuth — same three gates, same
	// rule about which one may answer (false, nil).
	if !rs.jar.HasAnyYouTubeAuthCookie() {
		return false, nil // Nothing configured at all.
	}

	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, fmt.Errorf("youtube auth check: no cookie header could be built: %w", ErrAuthCheckNotAttempted)
	}

	origin := "https://www.youtube.com"
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, fmt.Errorf("youtube auth check: no SAPISIDHASH could be generated: %w", ErrAuthCheckNotAttempted)
	}

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	body := youtubeGuideRequestBody()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideRefreshURL, strings.NewReader(body))
	if err != nil {
		return false, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("youtube auth check: %w", err)
	}
	defer resp.Body.Close()

	// Before the status AND before the body, which matters more here than in
	// checkYouTubeAuth: this function also merges Set-Cookie headers back into
	// the jar on the authenticated path, and a redirected exchange must not be
	// allowed to write to it at all.
	if err := authResponseIsOurs(resp, req, "Cookie"); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, fmt.Errorf("youtube auth check: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		// NOT (false, nil). That means "conclusively not authenticated" to
		// shouldFireRecovery, so a 429/503/edge block would be reported as
		// dead credentials. We learned nothing about the session here.
		return false, fmt.Errorf("youtube auth check: unexpected status %d", resp.StatusCode)
	}

	// Read body for auth check
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return false, fmt.Errorf("read YouTube auth response: %w", err)
	}

	// Check auth status from response
	authenticated := false
	var data struct {
		ResponseContext struct {
			ServiceTrackingParams []struct {
				Params []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"params"`
			} `json:"serviceTrackingParams"`
			MainAppWebResponseContext struct {
				LoggedIn bool `json:"loggedIn"`
			} `json:"mainAppWebResponseContext"`
		} `json:"responseContext"`
	}

	if err := json.Unmarshal(respBody, &data); err != nil {
		// Same cap as the checkYouTubeAuth fallback (#24).
		respStr := string(respBody[:min(len(respBody), authBodyFallbackLimit)])
		authenticated = strings.Contains(respStr, `"logged_in":"1"`) ||
			strings.Contains(respStr, `"loggedIn":true`)
	} else {
		for _, service := range data.ResponseContext.ServiceTrackingParams {
			for _, param := range service.Params {
				if param.Key == "logged_in" && param.Value == "1" {
					authenticated = true
				}
			}
		}
		if !authenticated {
			authenticated = data.ResponseContext.MainAppWebResponseContext.LoggedIn
		}
	}

	// If authenticated, process Set-Cookie headers to refresh session cookies
	if authenticated {
		rs.processYouTubeSetCookies(resp)
	}

	return authenticated, nil
}

// processYouTubeSetCookies parses Set-Cookie headers from a YouTube API response
// and merges updated cookies into the cookie file.
func (rs *RefreshService) processYouTubeSetCookies(resp *http.Response) {
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		rs.logger.Debug("youtube session refresh: no Set-Cookie headers")
		return
	}

	updates := make(map[string]cookieUpdate)
	for _, sc := range setCookies {
		// Cheap early filter before tokenizing — only cookies that mention
		// youtube.com or google.com anywhere in the Set-Cookie string are
		// candidates. The authoritative check against the parsed Domain=
		// attribute happens below via domainMatches so hosts like
		// "fakegoogle.com" that merely embed the substring are rejected.
		scLower := strings.ToLower(sc)
		if !strings.Contains(scLower, "youtube.com") && !strings.Contains(scLower, "google.com") {
			continue
		}

		parts := strings.Split(sc, ";")
		if len(parts) == 0 {
			continue
		}
		nameValue := strings.TrimSpace(parts[0])
		name, value, ok := strings.Cut(nameValue, "=")
		if !ok || name == "" {
			continue
		}

		now := time.Now().Unix()
		expiry := now + 365*24*60*60
		skipCookie := false
		domainAttr := ""
		for _, part := range parts[1:] {
			trimmed := strings.TrimSpace(strings.ToLower(part))
			if strings.HasPrefix(trimmed, "expires=") {
				_, dateStr, _ := strings.Cut(part, "=")
				dateStr = strings.TrimSpace(dateStr)
				if t, err := time.Parse(time.RFC1123, dateStr); err == nil {
					expiry = t.Unix()
				} else if t, err := time.Parse("Mon, 02-Jan-2006 15:04:05 MST", dateStr); err == nil {
					expiry = t.Unix()
				} else if t, err := time.Parse(time.RFC1123Z, dateStr); err == nil {
					expiry = t.Unix()
				}
				// If all date formats fail, keep the default expiry
			} else if strings.HasPrefix(trimmed, "max-age=") {
				if maxAge, err := strconv.ParseInt(strings.TrimSpace(trimmed[8:]), 10, 64); err == nil {
					// Negative or zero max-age means the cookie should be deleted — skip it
					if maxAge <= 0 {
						skipCookie = true
						break
					}
					expiry = now + maxAge
				}
			} else if strings.HasPrefix(trimmed, "domain=") {
				_, dom, _ := strings.Cut(part, "=")
				domainAttr = strings.TrimSpace(dom)
			}
		}
		if skipCookie {
			continue
		}

		// Normalize domain so the Netscape row uses a leading-dot form when
		// the Set-Cookie explicitly said Domain= (which implies subdomain
		// scope per RFC 6265).
		if domainAttr != "" && !strings.HasPrefix(domainAttr, ".") {
			domainAttr = "." + domainAttr
		}

		// When the server supplied Domain=, reject anything that is not an
		// actual YouTube or Google host — blocks the corner case where the
		// early substring pre-filter let through a Set-Cookie carrying a
		// Domain= for e.g. accounts.google.com.evil.tld.
		if domainAttr != "" && !isYouTubeDomain(domainAttr) && !isGoogleDomain(domainAttr) {
			continue
		}

		updates[name] = cookieUpdate{Value: value, Expiry: expiry, Domain: domainAttr}
	}

	if len(updates) == 0 {
		rs.logger.Debug("youtube session refresh: no relevant cookies to update")
		return
	}

	rs.logger.Debug("youtube session refresh: updating cookies", "count", len(updates))

	// A failure here is not cosmetic: the rotated values are discarded and
	// the on-disk session ages out, so downloads eventually start failing
	// for a reason that has nothing to do with the download. Name the
	// deployment mistake that actually causes it — updateCookieFile ends in
	// writeFileAtomic (temp file + rename), and a rename cannot replace a
	// single-file bind mount.
	if err := rs.updateCookieFile(updates); err != nil {
		rs.logger.Warn("youtube session refresh: failed to update cookie file — rotated session cookies were discarded and the file will go stale",
			"err", err,
			"hint", "if this is Docker, do not bind-mount cookies.txt as an individual file; put it inside the mounted /data directory so the atomic rename can replace it")
		return
	}

	if err := rs.jar.Reload(); err != nil {
		rs.logger.Warn("youtube session refresh: failed to reload jar", "err", err)
	}
}

// updateCookieFile re-reads the cookie file, updates matching cookies with new
// values and expiry, and adds new cookies not already in the file.
//
// Behavior notes relative to the original implementation:
//   - Every row matching an updated cookie name is refreshed (per finding #4),
//     not just the first one. Leaving stale duplicates on .google.com while a
//     fresh value lands on .youtube.com caused silent cookie drift on legacy
//     files that contained multiple domain variants of the same name.
//   - The Netscape "include subdomains" flag is derived from whether the
//     domain begins with "." (finding #5) instead of being hardcoded TRUE.
//   - Domain for newly-inserted rows is taken from the Set-Cookie Domain=
//     attribute when the server provided one (finding #40); falling back to
//     the legacy .youtube.com / .google.com heuristic only as a last resort.
func (rs *RefreshService) updateCookieFile(updates map[string]cookieUpdate) error {
	filePath := rs.jar.GetFilePath()
	if filePath == "" {
		return fmt.Errorf("no cookie file path configured")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read cookie file: %w", err)
	}

	var result strings.Builder
	updated := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Netscape cookie files occasionally contain values that push a single
	// line past bufio.Scanner's default 64KiB buffer; bump the ceiling to
	// 1MiB so an oversized line surfaces as an error below instead of a
	// silently truncated row.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Check if this is a cookie line that we need to update.
		// Every matching row is rewritten (not just the first) so multi-domain
		// duplicates do not drift out of sync with the refreshed values.
		if trimmed != "" && (!strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "#HttpOnly_")) {
			parts := strings.Split(trimmed, "\t")
			if len(parts) >= 7 {
				cookieName := strings.TrimSpace(parts[5])
				if cu, ok := updates[cookieName]; ok {
					// Update value (field 6) and expiry (field 4)
					parts[4] = strconv.FormatInt(cu.Expiry, 10)
					parts[6] = cu.Value
					result.WriteString(strings.Join(parts, "\t"))
					result.WriteString("\n")
					updated[cookieName] = true
					continue
				}
			}
		}

		result.WriteString(line)
		result.WriteString("\n")
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan cookie file: %w", err)
	}

	// Add new cookies that weren't found in the existing file
	for name, cu := range updates {
		if updated[name] {
			continue
		}
		domain := cu.Domain
		if domain == "" {
			// Fallback when the Set-Cookie lacked Domain=. Prefer YouTube;
			// Google-only auth cookies are only emitted by google.com paths.
			domain = ".youtube.com"
			if isGoogleOnlyAuthName(name) {
				domain = ".google.com"
			}
		}
		// Subdomain flag follows RFC 6265: leading-dot domain = include
		// subdomains. The legacy code hardcoded TRUE even for no-dot domains.
		subdomains := "FALSE"
		if strings.HasPrefix(domain, ".") {
			subdomains = "TRUE"
		}
		secure := "FALSE"
		if strings.HasPrefix(name, "__Secure-") {
			secure = "TRUE"
		}
		// Netscape format: domain, include_subdomains, path, secure, expiry, name, value
		if _, werr := fmt.Fprintf(&result, "%s\t%s\t/\t%s\t%d\t%s\t%s\n",
			domain, subdomains, secure, cu.Expiry, name, cu.Value); werr != nil {
			return fmt.Errorf("write new cookie row: %w", werr)
		}
		rs.logger.Debug("added new cookie to file", "name", name, "domain", domain)
		updated[name] = true
	}

	// Atomic write via the shared same-package helper — it uses a unique
	// temp name (the AutoCookieService writes the same cookies.txt, so a
	// fixed ".tmp" would let the two writers interleave) and applies the
	// memoized parent-dir DACL tightening.
	if err := writeFileAtomic(filePath, []byte(result.String()), 0o600); err != nil {
		return err
	}

	if len(updated) > 0 {
		rs.logger.Debug("updated cookies in file", "updated", len(updated))
	}

	return nil
}

// isGoogleOnlyAuthName returns true for cookie names that live on the
// google.com domain (not youtube.com) in a typical Google session. The
// legacy code used strings.Contains(name, "GOOGLE") which matched nothing
// real — most google.com auth cookies are named SID, HSID, SSID, APISID,
// SAPISID, or the __Secure- variants.
func isGoogleOnlyAuthName(name string) bool {
	switch name {
	case "SID", "HSID", "SSID", "APISID", "SAPISID":
		return true
	}
	return strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P")
}

func (rs *RefreshService) checkTwitchAuth(ctx context.Context) (bool, error) {
	// Read the token once. It is both the gate and the credential, so asking
	// HasTwitchAuthCookies first and re-reading the value after would leave a
	// window in which a concurrent jar.Reload swaps the map between the two.
	token := rs.jar.GetTwitchAuthToken()
	if token == "" {
		// Deliberately NOT broadened the way the YouTube gate above was, and
		// the reason has nothing to do with what Twitch would answer.
		//
		// Twitch auth is a single bearer token. With no auth-token there is
		// no credential to validate, so a request could not learn anything
		// about THIS install's session whatever came back — which makes "not
		// authenticated" true here rather than inferred. That is the whole
		// difference from a cleared LOGIN_INFO, which says nothing at all
		// about whether the Google session still works.
		//
		// Sending an empty OAuth header just to reach the network therefore
		// buys nothing, and would force the 200/401-only rule below to read
		// the reply as if it were a verdict on a token this install does not
		// have.
		//
		// The "was Twitch ever configured" question, which is what decides
		// whether an alarm fires, is answered by jar.HasAnyTwitchAuthCookie
		// at the doRefresh gate instead: a jar holding twilight-user and no
		// auth-token is a session that plainly was configured and now has no
		// credential, and it reports as configured-and-broken rather than as
		// never-configured. See twitchAuthCookieNames for how that state
		// arises.
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, twitchValidateURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "OAuth "+token)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("twitch auth check: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// The 401 rule below is only a statement about OUR token if our token is
	// what reached the endpoint. Go strips Authorization on a cross-hostname
	// redirect exactly as it strips Cookie, and the strip is sticky, so an
	// intermediary that bounces this call and answers 401 would otherwise
	// produce a conclusive dead-token verdict about a token it never saw.
	if err := authResponseIsOurs(resp, req, "Authorization"); err != nil {
		return false, fmt.Errorf("twitch auth check: %w", err)
	}

	// Twitch documents exactly two answers for oauth2/validate: 200 for a
	// valid token, 401 for an invalid one. So 401 stays CONCLUSIVE — it is
	// the one status that genuinely means "sign in again", and folding it
	// into the error branch below would suppress recovery and the re-login
	// prompt for every expired token. Everything else is infrastructure (a
	// rate limiter, an outage, an edge block) and says nothing about the
	// token, so it must not be reported as dead credentials — the same
	// mistake the YouTube guide check made, reachable here through the same
	// checkPlatformAuth mapping.
	//
	// The error names the status and nothing else. It reaches
	// AutoCookieService.setError and is rendered in the Web UI and TUI, so a
	// response body echoed back by an intermediary must never be
	// interpolated into it.
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized:
		return false, nil
	default:
		return false, fmt.Errorf("twitch auth check: unexpected status %d", resp.StatusCode)
	}
}

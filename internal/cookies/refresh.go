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
}

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
		jar:             jar,
		refreshInterval: interval,
		logger:          logger,
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

	// Initial check
	rs.doRefresh(ctx)

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
func (rs *RefreshService) CheckNow(ctx context.Context) {
	rs.doRefresh(ctx)
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

func (rs *RefreshService) doRefresh(ctx context.Context) {
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
	if rs.OnRecoveryNeeded != nil {
		if shouldFireRecovery(ytConcluded, prevYT, ytAuth, ytErr, hasYTCookies) {
			rs.logger.Warn("youtube auth lost, triggering recovery")
			rs.OnRecoveryNeeded("youtube")
		}
		if shouldFireRecovery(twConcluded, prevTW, twAuth, twErr, hasTWCookies) {
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

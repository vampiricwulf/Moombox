package cookies

import (
	"context"
	"sync"
	"time"

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
	// One endpoint, and there was only ever one. This used to be two vars —
	// youtubeGuideURL and youtubeGuideRefreshURL — described as different
	// endpoints kept apart on purpose. They were not: checkYouTubeAuth sent
	// youtubeGuideURL+"?prettyPrint=false", which is youtubeGuideRefreshURL
	// character for character. Folding the two guide functions into one
	// exchange made the duplicate visible, since both call sites then read the
	// same expression from two different names.
	youtubeGuideURL   = "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false"
	twitchValidateURL = "https://id.twitch.tv/oauth2/validate"
)

const (
	defaultRefreshInterval = 30 * time.Minute
	authCheckTimeout       = 15 * time.Second

	// livenessRefireWindow bounds how often ONE platform's logged-out
	// liveness verdict may clear the dedupe and reach OnRecoveryNeeded — true
	// of the FIRST re-alarm only; see the back-off below for every one after
	// it.
	//
	// The membership probe runs once per configured channel per feed cycle,
	// with a 500ms stagger between channels, so a dead session produces N
	// verdicts inside a couple of seconds.
	//
	// What N un-deduped verdicts cost within ONE cycle is not N headless
	// browsers. AutoCookieService.RefreshCookiesDetailed single-flights on its
	// refreshCmd sentinel, so the first call claims the slot and every call
	// that arrives while it runs returns refreshDeclined() immediately.
	//
	// This comment used to say the damage was that each of those declines sent
	// "Cookie Auto-Refresh Ineffective" and stamped the notification cooldown,
	// suppressing the real verdict two minutes later. That was true when it was
	// written and is no longer: runCookieRecovery's Unknown branch now splits on
	// RefreshResult.Ran and a declined pass reports nothing at all. The hazard
	// was fixed where it lived rather than being held off by this window.
	//
	// What is left is workload, and it is per CYCLE rather than per verdict.
	// Without this window a dead session fires again on every feed cycle — 10
	// minutes by default — so the one call that does claim the slot drives a
	// real headless-browser refresh, under a 2-minute timeout, three times as
	// often as this window allows, with the rest of each burst spending a
	// goroutine apiece to be told no.
	//
	// Its own constant on purpose. It is NOT the notification cooldown in
	// cmd/moombox's wireMonitorCallbacks, and it is NOT defaultRefreshInterval
	// above, however the three numbers happen to line up today. It is set to
	// match that cooldown so the two coalescing windows do not drift apart —
	// not because either implies the other.
	livenessRefireWindow = 30 * time.Minute

	// livenessRefireFactor and livenessRefireCap turn livenessRefireWindow
	// above into a per-platform back-off, decided by the owner 2026-08-29:
	// alarm, re-alarm 30 minutes later, then double — 1 h, 2 h, 4 h — capped
	// near a day, and start over when auth returns.
	//
	// WHY A SCHEDULE. Tier 1 notifies once per process for a session already
	// dead at startup: shouldFireRecovery's witnessed-transition arm needs
	// prevAuth to have been true, and the first conclusive negative clears it.
	// An armed tier 2 has the opposite problem — it re-fires for as long as
	// the session stays dead, so a flat window is 48 notifications a day for
	// one loss. The back-off keeps the first hour responsive and then gets out
	// of the way.
	//
	// LIVE SINCE ARMING (2026-09-03): recordLiveness computes the schedule,
	// ObserveLiveness logs the answer, and livenessRecoveryArmed is true, so
	// that same answer is what decides whether OnRecoveryNeeded is called. The
	// state moves on every observation whatever the verdict, which is what
	// keeps the wouldFireRecovery field on that line honest — it is the bool
	// the fire reads. The schedule landed and was mutation-tested ahead of the
	// flip, which is what kept arming a one-constant change.
	livenessRefireFactor = 2
	livenessRefireCap    = 24 * time.Hour

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
	// NewRefreshService enforces that upper bound against the interval it is
	// actually handed, not just against the default constant: an interval at
	// or below this window is refused and replaced with the default, with a
	// Warn naming both numbers. Nothing in production reaches that clamp today
	// — no config knob feeds the constructor's refreshInterval parameter — so
	// it exists for the test constructor and for the day a knob appears.
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

	// authBodyFallbackLimit caps how much of the response body we promote to a
	// Go string for the JSON-parse-failed fallback path. The real
	// `"logged_in":"1"` / `"loggedIn":true` markers live in the first hundreds
	// of bytes of the responseContext block; scanning past 16KB only inflates
	// memory and increases the surface for accidentally logging cookies or
	// session tokens that may appear deeper in the payload (audit
	// reports/cookies.md #24).
	authBodyFallbackLimit = 16 << 10
)

// RefreshService periodically reloads and validates cookies.
type RefreshService struct {
	mu              sync.RWMutex
	jar             *CookieJar
	cancel          context.CancelFunc
	status          AuthStatus
	refreshInterval time.Duration

	// refreshInFlight is true for the duration of one refresh pass. It is the
	// single-flight for all three entry points — Start's initial check,
	// CheckNow and the ticker — and it deliberately reuses mu rather than
	// adding a second lock, because the thing it protects is service state
	// that mu already owns.
	//
	// A second caller is a NO-OP, not a queued pass. See refresh for what two
	// overlapping passes actually do to the cookie file, and for the arming
	// note about a manual recheck that appears to do nothing.
	refreshInFlight bool

	// refreshPassHook, when non-nil, is called once per pass that gets PAST
	// the in-flight guard, before any work begins.
	//
	// TEST SEAM. Nothing in cmd/ or internal/web ever sets it; it is
	// unexported and has no setter, so only this package can. It exists
	// because refresh's two lifecycle properties have no other seam: a test
	// cannot make a pass panic (every failure mode inside is caught and
	// downgraded to an inconclusive verdict, by design), and it cannot hold a
	// pass open long enough for a second caller to collide with it. Counting
	// calls here also counts PASSES rather than log lines, which is what the
	// concurrency test has to assert on — a guard that is deleted shows up as
	// a second call here and nowhere else.
	//
	// Read under mu with the guard, called after mu is released, so a hook
	// that blocks holds the single-flight without holding the lock.
	refreshPassHook func()

	// refreshLockedHook is the same seam INSIDE refresh's status-update
	// critical section, called with rs.mu held for writing.
	//
	// TEST SEAM, and a second one rather than a second call to the first,
	// because the two windows need opposite things. refreshPassHook must be
	// callable outside the lock so a test can BLOCK a pass there without
	// wedging every other rs.mu reader; this one must be inside it, because
	// the property it exists to test is what a panic does while the write lock
	// is held — the case that used to deadlock the guard release, and the only
	// case where "the release defer exists" is not the same claim as "the guard
	// is actually released".
	//
	// A hook installed here MUST NOT call back into RefreshService: rs.mu is a
	// plain non-reentrant RWMutex.
	refreshLockedHook func()

	// Track previous auth state to detect auth → no-auth transitions.
	prevYouTubeAuth bool
	prevTwitchAuth  bool
	hasCheckedOnce  bool

	// ytEverConcluded / twEverConcluded track, per platform, whether THAT
	// platform has ever completed a conclusive (checkErr == nil) check.
	// This is deliberately NOT the same thing as hasCheckedOnce, which is
	// service-wide: nothing AUTOMATIC ever prunes Cookies.Platforms — both
	// automatic writers only add, and the sole removal path is an operator
	// replacing the list wholesale through PUT /api/config — so
	// SetExpectedPlatforms can seed hasCheckedOnce=true from YouTube's
	// presence alone while Twitch cookies exist on disk but were never
	// verified. Using the shared hasCheckedOnce for the "first conclusive
	// check" decision in shouldFireRecovery would then treat Twitch's actual
	// first check as a "subsequent" one (prevTwitchAuth is the false zero
	// value, so the witnessed-transition condition never fires either) —
	// silently swallowing recovery for any platform absent from the persisted
	// list while a sibling platform is present. See shouldFireRecovery.
	ytEverConcluded bool
	twEverConcluded bool

	// ytUnrecovered / twUnrecovered carry an auth failure a PREVIOUS process
	// announced and never closed (SetUnrecoveredPlatforms). The first
	// conclusive authenticated check of that platform fires OnAuthRecovered
	// whatever this process's own baseline says, and clears the flag.
	ytUnrecovered bool
	twUnrecovered bool

	// twitchMark holds a Twitch credential failure that oauth2/validate cannot
	// see, and it is why rs.status has TWO writers rather than one. Written
	// under mu by NoteTwitchAuthLoss; consulted and cleared under mu by
	// refresh's status block. See twitchAuthMark.
	twitchMark twitchAuthMark

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

	// prevTwitchIdentity is the jar's TwitchIdentity() as of the last
	// conclusive AND authenticated Twitch check — the baseline
	// shouldObserveCredentials compares against, exactly as
	// prevYouTubeIdentity is for YouTube. See advanceIdentityBaseline for why
	// an unauthenticated check must not move it.
	//
	// The reason the YouTube field's comment above gave for NOT having this —
	// "Twitch's auth-token rotates on Twitch's schedule, so it is not a stable
	// account discriminator, and no Twitch failure produces a membership park"
	// — was correct about ACCOUNTS and is not what this answers. Arc 10 asks
	// "is this the same credential PAIR the chat downgrade was observed
	// under", and a rotation that changes the token is a genuine YES to that:
	// the new pair has not been proven broken, so clearing the mark and
	// reconnecting chat once is the right outcome, not a false positive.
	prevTwitchIdentity string

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
	// what a redundant fire actually costs: not a second browser — the
	// auto-cookie service single-flights — but a goroutine and its two-minute
	// timeout spent being told no.
	//
	// "Decided", not "fired", and the distinction outlived arming. The stamp is
	// written by recordLiveness, UPSTREAM of ObserveLiveness's nil check on
	// OnRecoveryNeeded, so a service with no callback wired still consumes the
	// window. What the map records is the DECISION it exists to de-duplicate;
	// whether a call followed is a separate question, which is why nothing
	// reads this map to find out. A LoggedIn observation must never write here.
	lastRecoveryDecided map[string]time.Time

	// lastLivenessKnown is the last thing this process learned about a
	// platform's session, kept solely to decide the LOG LEVEL of the next
	// line (see ObserveLiveness). It steers no behaviour: the zero value is
	// livenessNever — "nothing learned in this process" — which differs from
	// every real state and therefore reads as notable, so the worst a missing
	// entry can do is emit one extra line.
	//
	// THREE states, not two, because the fallback probe has a third outcome.
	// An inconclusive probe is not a verdict and must move no other state, but
	// it is the outcome the liveness pilot most needs to be able to see: with
	// only conclusive outcomes recorded, a signal that has gone permanently
	// dead behind a redirecting intermediary is indistinguishable from a
	// healthy install with nothing to report. It shares this map rather than
	// getting its own so there is ONE answer to "has this changed since last
	// time", whatever the change is between.
	lastLivenessKnown map[string]livenessRecord

	// livenessRefireBackoff is the interval that must pass before this
	// platform's next signed-out verdict may clear the dedupe again — the
	// schedule livenessRefireFactor / livenessRefireCap describe.
	//
	// A FOURTH map beside the other three rather than a field on
	// livenessRecord (a uint8 enum with no room in it) and rather than a
	// scalar, which would let a YouTube session dead all day delay Twitch's
	// FIRST alarm by 24 hours
	// (TestRecordLivenessRefireBackoffIsPerPlatform). A missing entry reads as
	// the base window, so the zero value is the right answer.
	//
	// Written by escalateLivenessRefire and resetLivenessRefire, read by
	// livenessRefireWindowFor; all three take rs.mu from their caller.
	livenessRefireBackoff map[string]time.Duration

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
	// Fires for BOTH platforms, and the two mean different things to their
	// subscribers. A YouTube fire is "the signed-in ACCOUNT may have changed",
	// which is what unsticks a membership park. A Twitch fire is "the
	// credential PAIR changed", which clears the Twitch auth mark and is the
	// only signal a live IRC chat session has that repaired cookies are on
	// disk — see CookieJar.TwitchIdentity and NoteTwitchAuthLoss.
	//
	// Both are governed by the same two pure functions,
	// shouldObserveCredentials and advanceIdentityBaseline, against
	// per-platform baselines.
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

	// TwitchFallbackLiveness is the Twitch twin of FallbackLiveness, injected
	// for the same structural reason and one more: internal/twitch imports
	// THIS package (service.go, auth.go), so the direct call is an import cycle
	// in the other direction. cmd/moombox builds the closure.
	//
	// It exists because checkTwitchAuth cannot answer the question it looks
	// like it answers: oauth2/validate returns 200 for a token that is valid
	// but no longer entitled to authenticated playback, so an install with no
	// capture running reads a dead session as healthy until a stream goes live.
	// The playback access token DOES say which session it was minted for — see
	// internal/twitch.Service.ProbeSessionLiveness and PlaybackTokenSession.
	//
	// Called at the tail of a PERIODIC refresh under the same three conditions
	// the YouTube twin uses, plus one: the jar must hold a Twitch auth-token
	// RIGHT NOW (HasTwitchAuthCookies, not the broad HasAnyTwitchAuthCookie).
	// Without the bearer token the request gets an anonymous playback token by
	// design, so the probe would decline anyway.
	//
	// conclusive == false means the probe learned nothing — no configured
	// channel, a rate limit, a transport failure, a 401/403 that may be an edge
	// block — and MUST NOT move any state.
	TwitchFallbackLiveness func(ctx context.Context) (loggedIn, conclusive bool)
}

// livenessRecord is what the liveness signal last told this process about one
// platform. The zero value is livenessNever so an unwritten map entry means
// "nothing yet" without a second lookup, and so the first thing learned about
// a platform always compares as a change.
//
// livenessInconclusive is not a verdict and never reaches ObserveLiveness — it
// is recorded only so a repeated "the probe learned nothing" stops being
// notable after the first one. See recordInconclusiveLiveness.
type livenessRecord uint8

const (
	livenessNever livenessRecord = iota
	livenessSignedIn
	livenessSignedOut
	livenessInconclusive
)

// NewRefreshService creates a new cookie refresh service.
// If refreshInterval is zero, the default of 30 minutes is used.
//
// An interval at or below livenessFreshWindow is also refused, and for a
// reason that has nothing to do with how often the service runs: that window
// bounds how old a liveness observation may be and still suppress the fallback
// probe, and the fallback records its own answer through the same method. Let
// the two meet and the probe's own stamp is still fresh on the next tick, so it
// suppresses itself on alternate cycles — coverage silently halved, with no
// symptom anywhere. The invariant is documented at livenessFreshWindow; this is
// where it is enforced. Substituting the default is deliberately louder than
// clamping to "window + 1": a caller who asked for 10 minutes and silently got
// 25 would be no better informed than one who got 30, and the default is the
// only value this file has ever reasoned about.
func NewRefreshService(jar *CookieJar, refreshInterval time.Duration, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *RefreshService {
	interval := refreshInterval
	switch {
	case interval <= 0:
		interval = defaultRefreshInterval
	case interval <= livenessFreshWindow:
		logger.Warn("cookie refresh interval is too short for the liveness freshness window, using the default",
			"requested", interval.String(),
			"livenessFreshWindow", livenessFreshWindow.String(),
			"using", defaultRefreshInterval.String())
		interval = defaultRefreshInterval
	}
	return &RefreshService{
		jar:                   jar,
		refreshInterval:       interval,
		logger:                logger,
		lastLivenessObserved:  make(map[string]time.Time),
		lastRecoveryDecided:   make(map[string]time.Time),
		lastLivenessKnown:     make(map[string]livenessRecord),
		livenessRefireBackoff: make(map[string]time.Duration),
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

// SetUnrecoveredPlatforms names the platforms whose auth failure a previous
// process announced and never closed. The first check of each that finds it
// authenticated, conclusively, fires OnAuthRecovered — the close of that
// announcement — even though this process never saw the platform fail: its
// baseline is the persisted platform list (SetExpectedPlatforms) or nothing,
// and on its first pass hasCheckedOnce is false, so the transition the close
// rides is otherwise invisible after a restart. The recovery-needed side is
// untouched. Call this before Start().
func (rs *RefreshService) SetUnrecoveredPlatforms(platforms []string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, p := range platforms {
		switch p {
		case "youtube":
			rs.ytUnrecovered = true
		case "twitch":
			rs.twUnrecovered = true
		}
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
	//
	// Wrapped in the same recover the ticker goroutine below carries, and for
	// a sharper reason. This call runs on the CALLER's goroutine — cmd/moombox's
	// run(), before the web server binds — so an unrecovered panic in the first
	// pass does not lose a refresh, it takes the whole process down at boot,
	// with no dashboard, no TUI and no log surface up to say why. The ticker's
	// identical panic one cadence later is survivable purely because something
	// recovers it.
	//
	// The wrap goes around the CALL, not around a new goroutine: CLAUDE.md's
	// rule is that every goroutine carries an inline recover, and making this
	// one asynchronous to satisfy that rule would break what the comment above
	// documents — run() blocks on this pass so a dead-credential install is
	// told within seconds of launch.
	func() {
		defer func() {
			if r := recover(); r != nil {
				rs.logger.Error("startup cookie refresh panic", "panic", r)
			}
		}()
		rs.refresh(ctx, false)
	}()

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
				// Its own recover: the goroutine's sits outside this loop and
				// would END it, so one panic — in the pass or in any callback
				// it fans out to (OnAuthChange, OnRecoveryNeeded,
				// OnAuthRecovered, OnCredentialsChanged) — would stop the
				// session refresh and all auth-loss detection for the life of
				// the process. This way it costs one tick.
				func() {
					defer func() {
						if r := recover(); r != nil {
							rs.logger.Error("cookie refresh tick panic", "panic", r)
						}
					}()
					rs.doRefresh(ctx)
				}()
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

// CheckNow triggers an immediate cookie refresh and auth check, and reports
// whether a pass actually RAN.
//
// allowFallback is false: POST /api/cookies/recheck runs this synchronously on
// the HTTP handler goroutine, and the fallback probe is a full page fetch on
// top of the auth check that is already there. The periodic path owns that
// probe; a button press does not need to buy one.
//
// The bool is false when another pass — the 30-minute ticker, or a second
// manual gesture — was already in flight, in which case this call did nothing
// at all. It is NOT a failure and it is NOT a decline in the sense
// RefreshDeclinedCauses means: that vocabulary belongs to AutoCookieService's
// browser refresh and is pinned exhaustively across three consumers, while this
// is the in-process check's own single-flight. Do not report it through those
// causes.
//
// What a caller should do with false depends on what it wanted the pass FOR.
// A caller that only wants the UI to catch up can ignore it: the pass already
// running publishes its own result through OnAuthChange and GetStatus, one
// pass later at worst. A caller that has just REWRITTEN cookies.txt and wants
// this specific file re-verified cannot be given that guarantee here — the
// in-flight pass may have read the old file — and should say so in its log.
// Nothing today blocks or queues on this; a skipped pass is skipped.
func (rs *RefreshService) CheckNow(ctx context.Context) bool {
	return rs.refresh(ctx, false)
}

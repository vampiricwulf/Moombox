package cookies

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// cookieUpdateKey identifies one pending Set-Cookie change. Name alone is not
// enough: a real cookie file carries the same name on both .youtube.com and
// .google.com, and a name-keyed map both loses one of two same-name headers in
// a single response and lets a deletion scoped to one domain destroy the other
// domain's row.
//
// Domain is the normalized Set-Cookie Domain= attribute (leading dot, e.g.
// ".youtube.com"), or "" when the server sent no Domain= at all.
type cookieUpdateKey struct {
	Name   string
	Domain string
}

// cookieUpdate holds a parsed Set-Cookie value, expiry and flags. The domain
// lives in the map key (cookieUpdateKey) so there is exactly one copy of it;
// the write path reads it from there rather than re-deriving it from the
// cookie name.
type cookieUpdate struct {
	Value    string
	Expiry   int64
	HTTPOnly bool // Set-Cookie carried the HttpOnly attribute -> "#HttpOnly_" row prefix
	// Delete marks a deletion request: Max-Age<=0, or an Expires at or before
	// now. The row is removed, not rewritten empty — see processYouTubeSetCookies.
	Delete bool
}

// cookieOrigin names the SITE whose response produced a batch of cookie
// updates, stated as that site's registrable domain.
//
// It exists because a Set-Cookie with no Domain= attribute is host-scoped to
// the response that carried it, and by the time the updates reach
// updateCookieFile that response is gone: the map holds a nil Domain and
// nothing else. Two of the matching rules need to know where it came from
// anyway — resolveRowUpdate's rule 2 and sameCookiePlatform's Domain-less
// default — and both used to assume youtube.com, which was a true statement
// about the ONE call site rather than about those functions. Declaring it makes
// the assumption an argument.
//
// The three constants are the complete set. The zero value names no site and is
// deliberately inert: covers reports false for every row and platform reports
// nothing, so a caller that forgets to declare an origin gets the NARROWEST
// behaviour (only updates whose own Domain= scope-matches a row apply) rather
// than inheriting YouTube's.
type cookieOrigin string

const (
	originYouTube cookieOrigin = "youtube.com"
	originGoogle  cookieOrigin = "google.com"
	originTwitch  cookieOrigin = "twitch.tv"
)

// covers reports whether a cookie file row's domain lies inside this origin's
// site, which is the scope a Domain-less Set-Cookie from it may reach.
func (o cookieOrigin) covers(rowDomain string) bool {
	return o != "" && domainMatches(rowDomain, string(o))
}

// platform reports the credential platform this origin belongs to, in the same
// vocabulary cookiePlatformOf uses for domains — so originYouTube and
// originGoogle are one platform, exactly as .youtube.com and .google.com rows
// are.
func (o cookieOrigin) platform() string { return cookiePlatformOf(string(o)) }

// cookiePlatformOf maps a domain to its credential platform. YouTube and Google
// are ONE platform: a Google session covers both, which is why a value refresh
// is allowed to fan out across them. Twitch is another. A domain on neither has
// no platform and therefore matches nothing — see sameCookiePlatform.
func cookiePlatformOf(domain string) string {
	switch {
	case isTwitchDomain(domain):
		return "twitch"
	case isYouTubeDomain(domain) || isGoogleDomain(domain):
		return "google"
	}
	return ""
}

// AuthStatus tracks the authentication state for each platform.
//
// The two *Authenticated booleans and the two *Verification verdicts answer
// DIFFERENT questions, and the whole point of carrying both is that they
// disagree on the state that used to be invisible:
//
//   - *Authenticated — "can we do authenticated work for this platform right
//     now?" FALSE on an inconclusive check, because a check that learned
//     nothing is not a licence to assume a working session. Its meaning has
//     never changed and must not: it is the field every pre-existing consumer
//     reads, and it is what the wire's `authenticated` key carries.
//   - *Verification — what the check CONCLUDED: RefreshOK, RefreshFailed, or
//     RefreshUnknown for "we could not find out". A transient DNS failure, a
//     non-200, a redirect and an unreadable body all land on RefreshUnknown.
//
// Before these fields existed, `false, RefreshUnknown` and `false,
// RefreshFailed` were the same value on every surface, so a network blip
// rendered as a red "not authenticated" badge and the reason was parked in
// YouTubeError — a field with no reader anywhere. Arc 1 had already stopped
// an inconclusive check from MOVING internal state; this is what makes the
// distinction VISIBLE.
//
// The verdicts are ADDITIVE, in the sense the wire projections below rely on:
// nothing that read this struct before is asked to read them, and the handlers
// that render it branch POSITIVELY on RefreshUnknown, so a consumer that has
// not been taught the third state degrades to the behaviour it had.
//
// The json tags are vestigial — this struct is never marshalled; every
// consumer hand-projects (see CookieStatusPayload / TwitchAuthStatusPayload in
// internal/web/routes/cookies.go, which is where the two wire shapes now live
// exactly once each). RefreshVerdict is an int, so it is deliberately NOT
// given a tag: it must reach the wire through String(), never as an ordinal.
type AuthStatus struct {
	YouTubeAuthenticated bool `json:"youtubeAuthenticated"`
	TwitchAuthenticated  bool `json:"twitchAuthenticated"`

	// HasYouTubeCookies / HasTwitchCookies are the LOOSE predicates — "was
	// this install ever configured for the platform", not "is the cookie set
	// complete right now". See doRefresh for why the complete-set predicates
	// cannot answer the question the badges ask.
	HasYouTubeCookies bool `json:"hasYouTubeCookies"`
	HasTwitchCookies  bool `json:"hasTwitchCookies"`

	YouTubeVerification RefreshVerdict `json:"-"`
	TwitchVerification  RefreshVerdict `json:"-"`

	LastCheck    string `json:"lastCheck,omitempty"`
	YouTubeError string `json:"youtubeError,omitempty"`
	TwitchError  string `json:"twitchError,omitempty"`
}

// verdictFromCheck projects one platform's (authenticated, err) pair onto the
// shared three-way enum.
//
// err is ALMOST ALWAYS the inconclusive signal, not a failure signal: a
// non-200, a redirected answer and a 200 with no recognisable login marker all
// arrive here as a non-nil error, and none of them is evidence against the
// credentials.
//
// ONE SENTINEL IS THE EXCEPTION, and it is a finding rather than an absence of
// one. ErrAuthCheckNotAttempted is raised only AFTER HasAnyYouTubeAuthCookie
// has said the platform is configured, when the jar still cannot produce a
// cookie header or a SAPISIDHASH — the realistic shape being LOGIN_INFO
// surviving while the whole SAPISID family is gone. No request will ever be
// signable out of that jar, and no amount of waiting changes it; the operator
// has to re-export. "Authenticated requests will not work" is precisely what
// RefreshFailed is documented to mean, and it covers "there are no usable
// credentials" as squarely as it covers "the site rejected them".
//
// Folding it into RefreshUnknown reported a permanent, actionable failure as
// uncertainty: hedged copy on both UIs, and on the TUI bar an indicator that
// then drops out at tierEssential — where before the tri-state landed it was
// an always-visible red alarm. That is this arc's own defect class running
// backwards, and it is the only residual that could leave a user UNWARNED
// rather than merely under-informed.
//
// PRESENTATION ONLY, which is what makes the split safe to make here. This
// function's result reaches nothing but AuthStatus's two verdict fields.
// Everything that DECIDES anything reads the error itself and never a verdict:
// shouldFireRecovery keys on checkErr, prev-state advancement gates on
// `ytErr == nil`, and advanceIdentityBaseline takes the error directly. The
// sentinel therefore stays inconclusive everywhere it drives behaviour — it
// must, or a structural failure would be read as a verdict on the credentials
// and fire recovery for a jar no browser refresh can repair — and becomes a
// conclusion only where a human reads it.
//
// The tier ladder is NOT the lever for this and was rejected as one:
// promoting the whole Unknown class closes no hole, because the actionable
// failures already reach the narrowest bar by two tier-surviving routes
// (RefreshFailed → CookieStatusCookiesOnly, and a parked COOKIES? job →
// cookiesRejected, independent of the check entirely). The defect was that
// THIS error was classified as Unknown, not that Unknown is too quiet.
func verdictFromCheck(authenticated bool, err error) RefreshVerdict {
	switch {
	case errors.Is(err, ErrAuthCheckNotAttempted):
		return RefreshFailed
	case err != nil:
		return RefreshUnknown
	case authenticated:
		return RefreshOK
	default:
		return RefreshFailed
	}
}

// authStatusChanged reports whether anything a SURFACE renders differs between
// two consecutive checks. It is the OnAuthChange gate.
//
// Compared: the two auth booleans, the two cookies-present flags and the two
// verdicts — i.e. every input to the TUI badge in cmd/moombox/tui_wiring.go and
// to the Web indicators. Deliberately NOT compared:
//
//   - LastCheck, which moves on every single tick and would make the callback
//     fire unconditionally, defeating the whole point of the gate.
//   - YouTubeError / TwitchError, whose text can vary between two occurrences
//     of the same outcome (a DNS message carries the resolver's wording). The
//     verdict already carries the part a surface renders, and nothing renders
//     the strings — see the note above errGuideLoginMarkerUnreadable.
//
// The verdicts and the cookies-present flags have to be in here, not just the
// booleans. A platform going from conclusively-rejected to could-not-check
// leaves both booleans false, and a Twitch session going from never-configured
// to configured-but-rejected leaves TwitchAuthenticated false — both are badge
// transitions the operator must see, and on the boolean-only gate both were
// silent until some unrelated flip happened to fire the callback.
func authStatusChanged(prev, next AuthStatus) bool {
	return next.YouTubeAuthenticated != prev.YouTubeAuthenticated ||
		next.TwitchAuthenticated != prev.TwitchAuthenticated ||
		next.HasYouTubeCookies != prev.HasYouTubeCookies ||
		next.HasTwitchCookies != prev.HasTwitchCookies ||
		next.YouTubeVerification != prev.YouTubeVerification ||
		next.TwitchVerification != prev.TwitchVerification
}

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
	// replacing the list wholesale through PATCH /api/config — so
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
	// what a redundant fire actually costs: not a second browser — the
	// auto-cookie service single-flights — but a goroutine and its two-minute
	// timeout spent being told no.
	//
	// "Decided", not "fired": while livenessRecoveryArmed is false a cleared
	// liveness verdict is logged rather than acted on, so the stamp records
	// the decision this map exists to de-duplicate and not, in that case, a
	// call that happened. A LoggedIn observation must never write here.
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
	// it is the outcome the log-only pilot most needs to be able to see: with
	// only conclusive outcomes recorded, a signal that has gone permanently
	// dead behind a redirecting intermediary is indistinguishable from a
	// healthy install with nothing to report. It shares this map rather than
	// getting its own so there is ONE answer to "has this changed since last
	// time", whatever the change is between.
	lastLivenessKnown map[string]livenessRecord

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

// livenessRecordOf maps a conclusive verdict onto its record.
func livenessRecordOf(loggedIn bool) livenessRecord {
	if loggedIn {
		return livenessSignedIn
	}
	return livenessSignedOut
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
//     2-minute timeout, which drives a headless browser. TWO outcomes are
//     quiet — a successful refresh, and a pass that DECLINED to run at all
//     (the Ran == false half of runCookieRecovery's Unknown branch, which
//     logs and returns). The rest notify: "Cookie Auto-Refresh Failed"
//     (TypeError) for a transport error or a conclusive failure, "Cookie
//     Auto-Refresh Ineffective" (TypeWarning) for a pass that ran without
//     reaching an answer. A spurious verdict is by definition one no refresh
//     can fix, so it lands in a notifying outcome unless it is declined.
//
//     Do not read the decline as a safety net. It is a RACE: the pass is
//     declined only while another one holds the auto-cookie single-flight,
//     which is likely for a verdict produced in the same pass as a tier-1
//     fire (that is what noteRecoveryDecided is for) and not otherwise. A
//     spurious LoggedOut arriving with the slot free runs the browser and
//     notifies exactly as before.
//
//   - auto_enabled = false: no browser, and no quiet case at all — the decline
//     above cannot help here, because handleRecoveryNeeded returns on this arm
//     without calling the refresher, so there is no single-flight to lose. A
//     SYNCHRONOUS "Cookie Re-Authentication Required" (TypeError) naming the
//     cookie file, every time. This arm used to Debug-log and send nothing;
//     Task 7 replaced that silence, so arming now alarms the population this
//     arc elsewhere identifies as LEAST able to reach the remedy it names —
//     containers, remote dashboards, a loopback-gated setup wizard.
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
		jar:                  jar,
		refreshInterval:      interval,
		logger:               logger,
		lastLivenessObserved: make(map[string]time.Time),
		lastRecoveryDecided:  make(map[string]time.Time),
		lastLivenessKnown:    make(map[string]livenessRecord),
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
	if rs.lastLivenessKnown == nil {
		rs.lastLivenessKnown = make(map[string]livenessRecord)
	}

	// Read what was last known BEFORE this observation overwrites it. The
	// missing entry reads as livenessNever, which differs from both verdicts,
	// so a platform's first observation is notable on its own: it is the
	// record that the signal started producing.
	record := livenessRecordOf(loggedIn)
	notable = !loggedIn || rs.lastLivenessKnown[platform] != record

	// Both directions. This map answers "did anything tell us recently", and
	// a healthy answer settles that question exactly as well as a dead one.
	rs.lastLivenessObserved[platform] = now
	rs.lastLivenessKnown[platform] = record

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

// recordInconclusiveLiveness folds a fallback probe that learned NOTHING into
// the same per-platform record a real verdict goes into, and reports whether
// that is worth an operator-visible line.
//
// It exists because the log-only pilot is being read as evidence, and silence
// was ambiguous: an install whose probe is permanently refused — a redirecting
// captive portal, a proxy answering on another host, a rate limit that never
// clears — produced exactly the same log as a perfectly healthy install with
// nothing new to say. That is the one distinction the pilot has to be able to
// make about its own signal.
//
// Deliberately touches NEITHER of the other two maps:
//
//   - not lastLivenessObserved, because recording an observation would make
//     the next cycle's freshness check skip the probe — silencing the signal
//     for as long as it keeps failing, which is backwards.
//   - not lastRecoveryDecided, because that window belongs to real signed-out
//     verdicts and consuming it here would swallow the next one.
//
// TestFallbackInconclusiveMovesNothing pins both.
//
// `notable` follows the same rule ObserveLiveness uses: notable on a change of
// what is known, or on the first thing known about the platform in this
// process; a repeat is Debug. An install stuck behind an intermediary
// therefore says so once and then goes quiet, rather than every cycle forever.
func (rs *RefreshService) recordInconclusiveLiveness(platform string) (notable bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.lastLivenessKnown == nil {
		rs.lastLivenessKnown = make(map[string]livenessRecord)
	}
	notable = rs.lastLivenessKnown[platform] != livenessInconclusive
	rs.lastLivenessKnown[platform] = livenessInconclusive
	return notable
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
// single-flights — it would be DECLINED, and since runCookieRecovery's Unknown
// branch started splitting on RefreshResult.Ran a decline reports nothing. So
// what this stamp saves is the redundant goroutine and its 2-minute timeout,
// not an operator-visible mistake; livenessRefireWindow has the accounting.
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
//
// Subject to the same single-flight as the other two, and in this direction it
// is a tick that gets dropped: a tick arriving while a manual recheck is in
// flight does nothing rather than doubling up, and the next one is a full
// interval away. That is the right trade — the manual pass currently running
// answers the same question — but it is why the guard is not merely a
// protection for the manual button. The return is deliberately discarded: a
// dropped tick needs no caller-side handling, only the Debug line.
func (rs *RefreshService) doRefresh(ctx context.Context) {
	rs.refresh(ctx, true)
}

// refresh is the shared body of all three entry points; allowFallback is the
// only thing that separates them. Commentary elsewhere in this file that names
// doRefresh is describing this body — the split is newer than the comments.
//
// Returns whether a pass RAN. False means another one was already in flight and
// this call did nothing whatever — see the guard below.
func (rs *RefreshService) refresh(ctx context.Context, allowFallback bool) bool {
	// THE SINGLE-FLIGHT. Three entry points reach this body — Start's initial
	// check, CheckNow (POST /api/cookies/recheck and the TUI's R C), and the
	// 30-minute ticker — and until this guard existed none of them looked to
	// see whether another was already running. A manual recheck landing during
	// a ticker pass ran a second full pass alongside the first: two guide
	// fetches, two Set-Cookie merges, and two updateCookieFile rewrites of the
	// SAME file interleaved, which is the part that is not merely wasteful.
	//
	// A second caller does nothing and returns. It does not queue and it does
	// not wait: waiting would put an HTTP handler behind a pass that may spend
	// two auth-check timeouts, to deliver an answer the first pass is about to
	// publish through OnAuthChange anyway.
	//
	// ARMING NOTE, and this is the line to read when a recheck "did nothing":
	// the ticker and a manual gesture collide on a real install, so an operator
	// following the A4/A5 acceptance methodology can press recheck, see no new
	// pass in the log, and conclude the button is broken. It is not — the
	// answer they get back is the in-flight pass's, one status snapshot behind
	// at worst. Any methodology that counts passes has to allow for that, and
	// the Debug line below is the only evidence that it happened.
	var hook, lockedHook func()
	claimed := func() bool {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		if rs.refreshInFlight {
			return false
		}
		rs.refreshInFlight = true
		hook, lockedHook = rs.refreshPassHook, rs.refreshLockedHook
		return true
	}()
	if !claimed {
		// Debug, not Info. A ticker pass overlapping a manual one is routine on
		// any install where somebody presses the button, and this line would
		// otherwise be fanned out over the WebSocket log stream to both UIs for
		// an event that changes nothing.
		//
		// Logged AFTER the section above returns, never inside it: nothing that
		// can panic may run while rs.mu is held. See the release defer's rule.
		rs.logger.Debug("cookie refresh skipped, another pass is already in flight")
		return false
	}
	// THE RELEASE, and the rule that makes it safe.
	//
	// This defer takes rs.mu, and rs.mu is a plain non-reentrant RWMutex. So it
	// releases the guard on a panic ONLY IF the unwinding stack is not already
	// holding that lock. It is not, and that is not an accident: every rs.mu
	// critical section in this function — the claim above and the status update
	// below — releases through `defer`, so a panic anywhere in this body reaches
	// here with rs.mu free.
	//
	// STANDING RULE for anyone editing refresh: a bare `rs.mu.Lock()` whose
	// `rs.mu.Unlock()` is a plain statement rather than a defer re-arms a trap
	// that is invisible in review and catastrophic in the field. A panic inside
	// such a window would unwind with the write lock held, this defer would
	// block on Lock() forever, and the goroutine would park holding rs.mu — so
	// the panic never leaves refresh, Start's recover never runs, and every
	// later GetStatus() (RLock) blocks behind it. At boot that turns a loud
	// crash with a stack trace into a silent hang with no dashboard, no TUI and
	// no log line. Lock and defer-unlock together, or scope the section into a
	// func literal that does.
	defer func() {
		rs.mu.Lock()
		rs.refreshInFlight = false
		rs.mu.Unlock()
	}()
	if hook != nil {
		hook()
	}

	rs.logger.Debug("refreshing cookies")

	// Reload cookies from file
	if err := rs.jar.Reload(); err != nil {
		rs.logger.Warn("cookie reload failed", "err", err)
	}

	// Check YouTube auth and refresh session cookies in a single request
	// Returns: (authenticated bool, err error)
	//   err != nil       => INCONCLUSIVE. Not auth loss, and not necessarily a
	//                       network fault either: a non-200, a redirected
	//                       answer, or a 200 whose body carries no login
	//                       marker we recognise all land here. All that is
	//                       claimed is that this check learned nothing.
	//   false, nil       => conclusive. An explicit negative marker, or no
	//                       cookies configured at all.
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

	// THE STATUS UPDATE, scoped into a func literal so its unlock is DEFERRED.
	//
	// This section holds the write lock across ~80 lines that read the jar, build
	// an AuthStatus and advance five pieces of baseline state. It used to end in
	// a plain rs.mu.Unlock(), which meant a panic anywhere inside it unwound with
	// rs.mu held — and the guard release above, which needs that same lock, would
	// then block forever. See that defer for what the resulting hang costs at
	// boot. Every value the rest of the pass needs is declared outside and
	// assigned inside; prevStatus is not, because nothing below uses it.
	var (
		prevYT, prevTW             bool
		ytConcluded, twConcluded   bool
		hasChecked                 bool
		hasYTCookies, hasTWCookies bool
		ytIdentity, prevYTIdentity string
		changed                    bool
		statusCopy                 AuthStatus
	)
	func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()

		// TEST SEAM, inside the lock on purpose — this is the window whose panic
		// behaviour the deadlock rule above is about, and a seam that fired
		// outside it would prove only that the release defer exists. See
		// refreshLockedHook.
		if lockedHook != nil {
			lockedHook()
		}

		prevStatus := rs.status
		prevYT = rs.prevYouTubeAuth
		prevTW = rs.prevTwitchAuth
		hasChecked = rs.hasCheckedOnce
		ytConcluded = rs.ytEverConcluded
		twConcluded = rs.twEverConcluded

		// Captured once here (not re-read at the shouldFireRecovery call sites
		// below) so the "cookies present" snapshot lines up with the rest of
		// this check's other snapshots, all taken under the same lock.
		//
		// "Was this platform ever configured", NOT "is the set complete right
		// now". shouldFireRecovery's first-check branch returns this value, and
		// the complete-set predicates cannot tell a never-configured platform
		// from one whose LOGIN_INFO YouTube has cleared, or from a Twitch session
		// whose auth-token was pruned out on expiry while twilight-user survived
		// (the jar ignores expiry, mergeCookieFiles prunes on it — see
		// twitchAuthCookieNames) — the exact states that must be reported, and
		// that were silent forever.
		hasYTCookies = rs.jar.HasAnyYouTubeAuthCookie()
		hasTWCookies = rs.jar.HasAnyTwitchAuthCookie()

		// Sampled under the same lock as the rest of this check's snapshots, and
		// AFTER the jar.Reload() at the top of doRefresh, so it reflects whatever
		// account is on disk right now.
		ytIdentity = rs.jar.YouTubeIdentity()
		prevYTIdentity = rs.prevYouTubeIdentity

		rs.status = AuthStatus{
			YouTubeAuthenticated: ytAuth,
			TwitchAuthenticated:  twAuth,
			// Now "YouTube auth is configured" rather than "the cookie set is
			// complete", which is what the label this drives has always claimed.
			// A half-cleared jar consequently renders as configured-but-unverified
			// instead of as no-cookies-at-all — see AuthStatus.HasYouTubeCookies.
			HasYouTubeCookies: hasYTCookies,
			// The Twitch counterpart was computed here and thrown away for as long
			// as hasTWCookies has existed — which is why the TUI could only ever
			// assign CookieStatusOK for Twitch, leaving its CookiesOnly arm dead:
			// a Twitch session whose auth-token was pruned on expiry was reported
			// exactly like one that was never configured.
			HasTwitchCookies: hasTWCookies,
			// The reason the two booleans above cannot carry on their own. See
			// verdictFromCheck: err means "this check learned nothing", never
			// "the credentials are dead".
			YouTubeVerification: verdictFromCheck(ytAuth, ytErr),
			TwitchVerification:  verdictFromCheck(twAuth, twErr),
			LastCheck:           time.Now().UTC().Format(time.RFC3339),
			YouTubeError:        ytErrStr,
			TwitchError:         twErrStr,
		}

		// Update previous auth state tracking.
		// Only update previous state when the check was CONCLUSIVE. That is not
		// the same as "no network error": a non-200, an answer from the wrong
		// host, and a 200 whose body carries no marker we recognise are all
		// inconclusive too, and none of them may move this baseline.
		// An inconclusive check deliberately does NOT mark the platform
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

		changed = authStatusChanged(prevStatus, rs.status)
		// Snapshot under the lock: a concurrent doRefresh (ticker vs CheckNow)
		// writes rs.status under rs.mu, so reading it after Unlock is a race —
		// and the callback could observe a status newer than the transition
		// that triggered it.
		statusCopy = rs.status
	}()

	if changed && rs.OnAuthChange != nil {
		rs.OnAuthChange(statusCopy)
	}

	// Detect auth loss transitions: previously authenticated -> not authenticated,
	// and the failure is genuine auth loss (err == nil) rather than any of the
	// inconclusive outcomes — a network fault, a non-200, a redirected answer,
	// or a 200 we could not read a marker out of.
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
	// redundant fire is declined by the auto-cookie single-flight and, since
	// that branch started splitting on RefreshResult.Ran, reports nothing at
	// all; what it still costs is the goroutine and its timeout. See
	// livenessRefireWindow.
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
		} else if hasYTCookies {
			// Not a verdict, but not nothing either. This branch used to be
			// absent entirely, which made a probe that has NEVER been able to
			// answer look identical in the log to a healthy install with
			// nothing to report — while the pilot's whole purpose is to be
			// read as evidence about the signal. Deduped through the same
			// record a verdict uses, so a permanently-refused probe says so
			// once per process instead of once per cycle.
			//
			// Gated on the platform being CONFIGURED, and the gate covers the
			// record as well as the line. An install with no YouTube auth
			// cookie at all makes ProbeAccountLiveness return (Unknown, nil)
			// from its own first gate — there is no session for it to report
			// on — so "the probe learned nothing about this session" would be
			// describing a session that does not exist, and the one
			// distinction this line exists to draw would be diluted by installs
			// that were never in scope. Recording without logging would be
			// worse than either: the entry would sit at livenessInconclusive,
			// and the FIRST genuine failure after cookies arrive would read as
			// a repeat and land at Debug.
			//
			// hasYTCookies is this pass's own snapshot, taken under the lock
			// with every other one, and it is the permissive
			// HasAnyYouTubeAuthCookie — so a half-cleared session, the state
			// the probe exists to detect, still reports.
			//
			// The reason is not here because the (loggedIn, conclusive) pair
			// cannot carry one; cmd/moombox's FallbackLiveness closure logs the
			// probe's own error at Debug, where it has it.
			logAt := rs.logger.Debug
			if rs.recordInconclusiveLiveness("youtube") {
				logAt = rs.logger.Info
			}
			logAt("liveness fallback probe learned nothing about this session", "platform", "youtube")
		}
	}

	rs.logger.Debug("cookie refresh done",
		"youtube", ytAuth,
		"twitch", twAuth)
	return true
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

// errGuideLoginMarkerUnreadable is what a 200 whose body carries no login
// marker we recognise resolves to. It is an INCONCLUSIVE outcome, in the same
// family as a non-200 and a failed provenance check, and every consumer already
// handles it: shouldFireRecovery returns false on any checkErr, and
// checkPlatformAuth maps it to verifyUnknown.
//
// It deliberately does NOT wrap ErrAuthCheckNotAttempted. That sentinel means
// the question could not be FORMED — no cookie header, no SAPISIDHASH, nothing
// left the process. Here a request went out and came back; we simply could not
// read the answer. autocookies_profile.go's `attempted` flag turns on exactly
// that distinction.
//
// WHERE THIS STRING ACTUALLY GOES, checked rather than assumed, because an
// earlier draft of this comment claimed the Web UI and TUI and that was FALSE:
// AuthStatus.YouTubeError, the field doRefresh assigns it to, still has no
// reader anywhere in the tree, and that is now a DECISION rather than an
// oversight. Every consumer projects from the booleans and the verdicts
// instead — internal/web/routes/cookies.go's CookieStatusPayload /
// TwitchAuthStatusPayload (the one copy of each wire shape, shared with
// cmd/moombox/routes_wiring.go's status route) and tui_wiring.go
// (authStatusToTUI's badge, OnRecheckCookies' two verdicts). The fact the
// operator needs — "this check could not conclude" — is carried by
// RefreshUnknown, which is a bounded value; this string is server-authored
// prose whose lifecycle nothing establishes, so rendering it in an always-on
// panel would put unattributable text on screen indefinitely. The second
// possible route is closed too: checkPlatformAuth consumes the error for an
// errors.Is test and discards the value, so the rollback messaging composes
// from the verification STATE and never interpolates this text.
//
// The one sink is rs.logger.Debug in doRefresh, and what that means depends on
// the operator's log level — which makes the no-body-bytes rule below MORE
// load-bearing than "it goes to a log", not less:
//
//   - At the default INFO (config.go's LogLevel) the line has NO sink at all.
//     Logger.log returns at its slog.Enabled gate before formatting, the ring
//     buffer, or any subscriber.
//   - At DEBUG the same line fans out to FIVE places: the rotating log file;
//     the in-memory ring buffer, which GET /api/logs serves to any authenticated
//     client; the Web UI's live log stream (cmd/moombox's log-forwarder
//     subscriber → wsHub.BroadcastLog → the frontend's "log" case); the per-job
//     log buffers that same forwarder writes to the DATABASE; and the TUI log
//     panel via its own subscriber.
//
// So the surface is conditional, persistent (file + DB), and remotely readable
// — and DEBUG is exactly the level an operator raises to when their cookies
// look broken, i.e. precisely when this error fires. TestUnreadableGuideError-
// CarriesNoBody earns its place on that basis: this error names no host, no
// header and no body bytes. The unreadable body is the subject of the report
// and must never become its content.
//
// The wording says "learned nothing", not "failed", for when that changes.
// Follow-up 1 of the remediation plan is to surface the inconclusive state in
// both UIs; until it lands, an install behind an intercepting intermediary
// still RENDERS as "cookies found, not authenticated" (doRefresh sets
// YouTubeAuthenticated: ytAuth, which is false on an inconclusive check). What
// this change stops is the false recovery FIRE and the notification — not yet
// the false badge.
var errGuideLoginMarkerUnreadable = errors.New(
	"the guide reply carried no login marker we recognise, so this check learned nothing about the session")

// The guide reply's login marker, in the two shapes it has been observed in.
//
// Measured anonymously against the live endpoint on 2026-08-27 (an anonymous
// POST needs no credentials, so this is cheap to re-verify):
//
//	"serviceTrackingParams":[...{"key":"logged_in","value":"0"}...]
//	"mainAppWebResponseContext":{"loggedOut":true,...}
//
// Two things in that are worth writing down. The negative marker really is
// `logged_in` = `"0"` — a STRING "0", not a JSON false — which is why the
// sibling reader in internal/youtube deliberately refuses to map 1/0 and this
// one must: the two layers read different serialisations of the same flag.
// And an anonymous reply carries NO `loggedIn` key at all; it carries
// `loggedOut:true` instead. So a bare `bool` field could not tell "the flag
// said false" from "the flag was absent", which is precisely the distinction
// this fix turns on — hence the pointers on the struct below.
const (
	guideLoginParamKey = "logged_in"
	guideLoginParamIn  = "1"
	guideLoginParamOut = "0"
)

// guideLoginMarkersIn / guideLoginMarkersOut are the literal needles for the
// string fallback, which runs ONLY when the body is not valid JSON at all.
//
// Positive needles only ever grew: the two that were here before are kept
// verbatim, and the third is the shape actually measured on the wire (the
// params are objects, so `"logged_in":"1"` never matched a real reply — a
// latent miss that was harmless while every real reply parsed as JSON, and is
// still harmless now because a miss is inconclusive). Never removing an
// accepted positive form is the rule that keeps this change from costing an
// authenticated session its verdict.
//
// Negative needles are new and are what make a conclusion possible here at
// all, so each is either measured on the wire or the unambiguous negation of a
// positive already accepted above.
//
// Deliberately NOT tolerant of whitespace or alternate quoting, unlike the
// sibling reader in internal/youtube: see youtubeGuideAuthVerdict.
var (
	guideLoginMarkersIn = []string{
		`"key":"logged_in","value":"1"`,
		`"logged_in":"1"`,
		`"loggedIn":true`,
	}
	guideLoginMarkersOut = []string{
		`"key":"logged_in","value":"0"`,
		`"logged_in":"0"`,
		`"loggedIn":false`,
		`"loggedOut":true`,
	}
)

// youtubeGuideAuthVerdict reads a 200 guide reply and says whether it is an
// observation of a signed-in session, a signed-out one, or neither.
//
// A conclusive "not authenticated" now requires an EXPLICIT negative marker.
// This used to be the fall-off-the-end answer: three separate exits — JSON
// parse failure with no positive needle, no `logged_in` = "1" among the
// tracking params, and `loggedIn` not true — all ended in `(false, nil)`,
// which is the one thing shouldFireRecovery acts on.
//
// The body that breaks that shape is a 200 carrying no marker at all. A
// transparent, NON-redirecting intermediary — captive portal, corporate proxy
// (http.ProxyFromEnvironment is on the shared transport) — answering our POST
// with HTML passes the provenance guard (same host, same scheme, credential
// header intact) and passes the status check, then produces a conclusive
// verdict of "your cookies are dead" about cookies that are perfectly fine.
// The same shape covers the fleet-wide case: one serialisation change upstream
// would tell every install at once, at the only tier that notifies today.
// A false failure is worse than a missed one, so anything unrecognisable is
// now errGuideLoginMarkerUnreadable.
//
// This is the rule livenessVerdict (internal/youtube/watch_page.go) already
// applies to the watch page: an explicit marker or nothing. The RULE is
// mirrored, not the code — internal/cookies must not import internal/youtube
// (internal/youtube/auth.go already imports this package, so the dependency
// only runs the other way), and the two read different serialisations anyway:
// booleans in a ytcfg blob there, "1"/"0" strings in a JSON object here.
//
// Whitespace and quoting tolerance, decided deliberately rather than copied:
//
//   - The JSON path already has FULL tolerance, and better tolerance than the
//     sibling's hand-rolled reader — encoding/json is a real parser, so
//     arbitrary whitespace, key order, unknown sibling fields and escaped
//     strings all cost nothing. Every real reply reaches this path.
//   - The string fallback stays literal-substring, with no whitespace or
//     quote-form tolerance. It runs only when the body is NOT valid JSON, i.e.
//     when the serialisation is already broken, and under the new rule every
//     needle it misses lands on inconclusive rather than on a verdict. Adding
//     a tolerant scanner would mean duplicating ~120 lines of the sibling's
//     generic marker reader to buy accuracy on a path whose failure mode is
//     already the safe one — and a redundant helper is its own defect.
//
// Positive wins over negative, and is checked first, so no reply that read as
// authenticated before reads as anything else now.
func youtubeGuideAuthVerdict(respBody []byte) (bool, error) {
	var data struct {
		ResponseContext struct {
			ServiceTrackingParams []struct {
				Params []struct {
					Key string `json:"key"`
					// RawMessage, not string, and the reason is a real failure
					// mode rather than tidiness. encoding/json fails the WHOLE
					// body if any single field mistypes, so one unrelated param
					// gaining a non-string value — `cver`, `e`, `visitor_data`,
					// keys this reader has no interest in — would collapse the
					// JSON path to the literal-needle fallback for every reply.
					// That fallback cannot see the measured wire shape's
					// positive as reliably as a parser can, so tier-1 would
					// degrade to permanently inconclusive: safe, but blind, and
					// triggered by a field we never asked about. Deferring the
					// decode confines that failure to the one param it actually
					// happened on.
					//
					// Key stays a plain string, and the residual is real: a
					// non-string `key` on ANY param collapses the whole body
					// exactly the same way, through the field this does not
					// confine. (An earlier version of this comment claimed such
					// a key would be unsurvivable in any typing. That is wrong —
					// a lazy Key would survive it fine, for the same reason a
					// lazy Value survives a mistyped Value.)
					//
					// It stays strict on COST, not on impossibility. Key is
					// compared on every param in the reply, so deferring it
					// means a decode per param on a ~15KB body on every refresh
					// cycle, where Value is decoded at most a handful of times —
					// only for params whose key already matched. Paying that to
					// guard a param NAME, the half of a key/value list least
					// likely to change type, is the wrong trade.
					//
					// If it ever happens the failure direction is the safe one:
					// the body falls to the literal-needle fallback and tier-1
					// degrades toward inconclusive, never toward a false alarm.
					Value json.RawMessage `json:"value"`
				} `json:"params"`
			} `json:"serviceTrackingParams"`
			MainAppWebResponseContext struct {
				// Pointers on purpose: absent and present-false are different
				// answers here, and only one of them is a verdict. See the
				// marker commentary above — a real anonymous reply omits
				// loggedIn entirely and sends loggedOut instead.
				LoggedIn  *bool `json:"loggedIn"`
				LoggedOut *bool `json:"loggedOut"`
			} `json:"mainAppWebResponseContext"`
		} `json:"responseContext"`
	}

	if err := json.Unmarshal(respBody, &data); err != nil {
		return youtubeGuideAuthVerdictFallback(respBody)
	}

	// An explicit negative anywhere is remembered but never returned early:
	// the scan keeps looking for a positive, because losing a signed-in read
	// is the one regression this must not introduce.
	negative := false

	// Primary: serviceTrackingParams carries logged_in across all Innertube
	// responses (several services each stamp their own copy).
	for _, service := range data.ResponseContext.ServiceTrackingParams {
		for _, param := range service.Params {
			if param.Key != guideLoginParamKey {
				continue
			}
			// Decoded here, for this param only — see the Value field comment.
			var value string
			if err := json.Unmarshal(param.Value, &value); err != nil {
				// The marker itself is no longer a string. A marker we found
				// and could not read is not a verdict; keep going.
				continue
			}
			switch value {
			case guideLoginParamIn:
				return true, nil
			case guideLoginParamOut:
				negative = true
			}
			// Any other value is a marker we found and could not read. Not a
			// verdict — fall through and let the rest of the reply speak.
		}
	}

	// Secondary: mainAppWebResponseContext's own flag, in whichever direction
	// it is emitted.
	if in := data.ResponseContext.MainAppWebResponseContext.LoggedIn; in != nil {
		if *in {
			return true, nil
		}
		negative = true
	}
	if out := data.ResponseContext.MainAppWebResponseContext.LoggedOut; out != nil && *out {
		negative = true
	}
	// `loggedOut:false` is deliberately not read as a positive. It has never
	// been observed, and inferring "signed in" from it would be a guess; the
	// cost of not guessing is an inconclusive result, which is safe.

	if negative {
		return false, nil
	}
	return false, errGuideLoginMarkerUnreadable
}

// youtubeGuideAuthVerdictFallback is youtubeGuideAuthVerdict for a body that
// is not valid JSON. Same rule, literal needles.
//
// The slice is capped so the resulting Go string never carries the full
// multi-MB payload — the marker lives in the first few hundred bytes of the
// responseContext block, and scanning past 16KB only inflates memory and
// widens the surface for session material to reach a log line (#24).
func youtubeGuideAuthVerdictFallback(respBody []byte) (bool, error) {
	respStr := string(respBody[:min(len(respBody), authBodyFallbackLimit)])
	for _, marker := range guideLoginMarkersIn {
		if strings.Contains(respStr, marker) {
			return true, nil
		}
	}
	for _, marker := range guideLoginMarkersOut {
		if strings.Contains(respStr, marker) {
			return false, nil
		}
	}
	return false, errGuideLoginMarkerUnreadable
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
// bytes. For where these strings actually go — and why that makes the rule more
// load-bearing rather than less — see errGuideLoginMarkerUnreadable's doc
// comment. They do NOT reach AutoCookieService.setError: checkPlatformAuth
// discards the value.
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

// youtubeGuideExchange makes the guide POST that both YouTube auth entry points
// are built on, and hands the verdict AND the response back to its caller. It
// never acts on the response itself, and that is the whole reason it has this
// shape.
//
// # Why it returns the response instead of using it
//
// checkYouTubeAuth is exported as CheckYouTubeAuth and wired into
// AutoCookieService.VerifyYouTubeAuth (cmd/moombox/services.go), where
// checkPlatformAuth runs it on the ROLLBACK path of a profile import: its answer
// is what decides whether the PREVIOUS cookies are restored. A shared exchange
// that merged Set-Cookie headers itself would therefore write the jar from the
// very response being used to judge the import — a bad import would rewrite the
// credentials it was about to be rolled back for, and the rollback would then
// restore them over a file that had already moved.
//
// So the write decision stays with the caller that owns it. This function reads
// the jar and never writes it; only checkAndRefreshYouTube calls
// processYouTubeSetCookies, and only on its own authenticated path. The
// invariant is structural rather than a matter of discipline: there is no jar
// write reachable from here to delete.
//
// The returned response has already had its body read to a verdict and CLOSED,
// so only its headers are still meaningful — which is all
// processYouTubeSetCookies reads. It is non-nil exactly when a request was made
// and a readable answer came back. Every error path returns nil, and so does the
// never-configured gate, which is what makes "a reply we could not read is not a
// reply anyone may write the jar from" a fact about the return values rather
// than a rule someone has to remember.
//
// # The gates
//
// The three entry gates encode one rule — the rule this subsystem kept getting
// wrong. Only the FIRST of them may answer (false, nil).
//
//   - Nothing configured at all — no session to have an opinion about, so a
//     silent "not authenticated" is the truth and shouldFireRecovery's
//     cookiesPresent gate (fed by the same predicate) keeps it silent.
//   - Configured but no request could be built — a check that did NOT happen.
//     (false, nil) would report it as dead credentials, so it errors instead.
//
// Everything in between reaches the network. In particular a jar with SAPISID
// and a cleared LOGIN_INFO — YouTube's own rotation-invalidation state — is
// CONFIGURED with BROKEN credentials, and its verdict has to come from YouTube
// rather than from a missing name in a map.
//
// The order after the request is load-bearing and is asserted in that order:
// provenance, then status, then body.
func (rs *RefreshService) youtubeGuideExchange(ctx context.Context, guideURL string) (bool, *http.Response, error) {
	if !rs.jar.HasAnyYouTubeAuthCookie() {
		return false, nil, nil // Nothing configured at all.
	}

	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, nil, fmt.Errorf("youtube auth check: no cookie header could be built: %w", ErrAuthCheckNotAttempted)
	}

	origin := "https://www.youtube.com"
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, nil, fmt.Errorf("youtube auth check: no SAPISIDHASH could be generated: %w", ErrAuthCheckNotAttempted)
	}

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	body := youtubeGuideRequestBody()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, guideURL, strings.NewReader(body))
	if err != nil {
		return false, nil, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, nil, fmt.Errorf("youtube auth check: %w", err)
	}
	// Closed here, not by the caller: by the time this returns the body has
	// been read to a verdict and nothing downstream needs it. The headers
	// survive the close.
	defer resp.Body.Close()

	// Before the status, for the same reason internal/youtube checks it first:
	// a redirected answer is not this session's answer whatever status it
	// carries, and naming the route is more accurate than naming the code.
	if err := authResponseIsOurs(resp, req, "Cookie"); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil, fmt.Errorf("youtube auth check: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		// NOT (false, nil). That means "conclusively not authenticated" to
		// shouldFireRecovery, so a 429/503/edge block would be reported as
		// dead credentials. We learned nothing about the session here.
		return false, nil, fmt.Errorf("youtube auth check: unexpected status %d", resp.StatusCode)
	}

	// YouTube always returns 200 even with invalid cookies, so the verdict has
	// to come out of the body. youtubeGuideAuthVerdict owns that rule for both
	// entry points; in particular a 200 carrying no marker it recognises is an
	// inconclusive error here, NOT (false, nil).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return false, nil, fmt.Errorf("read YouTube auth response: %w", err)
	}
	authenticated, err := youtubeGuideAuthVerdict(respBody)
	if err != nil {
		return false, nil, fmt.Errorf("youtube auth check: %w", err)
	}
	return authenticated, resp, nil
}

// checkYouTubeAuth asks YouTube whether the jar's session is still signed in,
// and does nothing else.
//
// This is the VERIFY path. It is exported as CheckYouTubeAuth and its answer
// decides whether an autocookies profile import is committed or ROLLED BACK, so
// it must not touch the jar — see youtubeGuideExchange's doc comment for why
// that invariant is expressed by discarding the response here rather than by a
// guard somewhere inside.
func (rs *RefreshService) checkYouTubeAuth(ctx context.Context) (bool, error) {
	authenticated, _, err := rs.youtubeGuideExchange(ctx, youtubeGuideURL+"?prettyPrint=false")
	return authenticated, err
}

// checkAndRefreshYouTube makes a single guide API request to both check
// YouTube auth status and refresh session cookies from Set-Cookie headers.
// This avoids the redundancy of separate check + refresh requests.
//
// It is the only caller in this file that writes the jar from a guide reply.
func (rs *RefreshService) checkAndRefreshYouTube(ctx context.Context) (bool, error) {
	authenticated, resp, err := rs.youtubeGuideExchange(ctx, youtubeGuideRefreshURL)
	if err != nil || !authenticated {
		// Anything short of an authenticated, readable reply stops here without
		// the jar being touched. Two of those cases are worth naming at the
		// write decision itself:
		//
		//   - Provenance. youtubeGuideExchange runs authResponseIsOurs before
		//     the status AND before the body, which matters more on THIS path
		//     than on the verify one: this is where Set-Cookie headers are
		//     merged back into the jar, and a redirected exchange must not be
		//     allowed to write to it at all.
		//   - An unreadable body. A reply we could not read is not a reply we
		//     may write the jar from, for the same reason a redirected one is
		//     not: we do not know whose session it describes. Both cases return
		//     a nil response, so there would be nothing to merge here even if
		//     this branch were deleted.
		return authenticated, err
	}
	rs.processYouTubeSetCookies(resp)
	return true, nil
}

// processYouTubeSetCookies parses Set-Cookie headers from a YouTube API response
// and merges updated cookies into the cookie file.
func (rs *RefreshService) processYouTubeSetCookies(resp *http.Response) {
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		rs.logger.Debug("youtube session refresh: no Set-Cookie headers")
		return
	}

	updates := make(map[cookieUpdateKey]cookieUpdate)
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

		var (
			expiresAt  int64
			hasExpires bool
			maxAge     int64
			hasMaxAge  bool
			httpOnly   bool
			domainAttr string
		)
		// Every attribute is read to the end of the header. The old loop broke
		// out early on Max-Age<=0, which threw away the Domain= that usually
		// follows it — and Domain= is what scopes the deletion below.
		for _, part := range parts[1:] {
			trimmed := strings.TrimSpace(strings.ToLower(part))
			switch {
			case strings.HasPrefix(trimmed, "expires="):
				_, dateStr, _ := strings.Cut(part, "=")
				dateStr = strings.TrimSpace(dateStr)
				if t, err := time.Parse(time.RFC1123, dateStr); err == nil {
					expiresAt, hasExpires = t.Unix(), true
				} else if t, err := time.Parse("Mon, 02-Jan-2006 15:04:05 MST", dateStr); err == nil {
					expiresAt, hasExpires = t.Unix(), true
				} else if t, err := time.Parse(time.RFC1123Z, dateStr); err == nil {
					expiresAt, hasExpires = t.Unix(), true
				}
				// If all date formats fail, hasExpires stays false and the
				// default one-year expiry below applies. An unreadable date
				// must not fall through as "expired" and delete the row.
			case strings.HasPrefix(trimmed, "max-age="):
				if v, err := strconv.ParseInt(strings.TrimSpace(trimmed[len("max-age="):]), 10, 64); err == nil {
					maxAge, hasMaxAge = v, true
				}
			case strings.HasPrefix(trimmed, "domain="):
				_, dom, _ := strings.Cut(part, "=")
				// Lowercased here, not just at comparison time. Domains are
				// case-insensitive and this string becomes a MAP KEY: without
				// this, "Domain=.YouTube.com" and "Domain=.youtube.com" are two
				// distinct keys that both scope-match the same row, and which
				// one wins is map-iteration order. CPython normalizes the same
				// way (_normalized_cookie_tuples: `if k == "domain": v = v.lower()`).
				domainAttr = strings.ToLower(strings.TrimSpace(dom))
			case trimmed == "httponly":
				httpOnly = true
			}
		}

		// RFC 6265 §4.1.2.2: Max-Age takes precedence over Expires.
		//
		// An expiry at or before now is a DELETION request and is treated
		// exactly as Max-Age<=0 is. This is the same rule yt-dlp gets from
		// Python's http.cookiejar: _cookie_from_cookie_tuple converts Max-Age
		// to an absolute expiry and then, for `expires <= self._now`, calls
		// self.clear(domain, path, name) and returns None — the cookie is
		// dropped from the jar entirely, keyed by domain+path+name. It is
		// never stored with an empty value, which is what this code used to
		// write: a row with value "" and expiry 0 that rowExpired will not
		// prune (it ignores exp == 0) and that CookieJar.Load cannot even see
		// (TrimSpace eats the trailing tab, leaving a 6-field "malformed" row).
		deleteCookie := false
		switch {
		case hasMaxAge:
			if maxAge <= 0 {
				deleteCookie = true
			} else {
				expiry = now + maxAge
			}
		case hasExpires:
			if expiresAt <= now {
				deleteCookie = true
			} else {
				expiry = expiresAt
			}
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

		updates[cookieUpdateKey{Name: name, Domain: domainAttr}] = cookieUpdate{
			Value:    value,
			Expiry:   expiry,
			HTTPOnly: httpOnly,
			Delete:   deleteCookie,
		}
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
	// originYouTube: this function is only ever fed a youtube.com guide
	// response. Stating that is what lets the write path scope a Domain-less
	// Set-Cookie without assuming where it came from — see updateCookieFile.
	if err := rs.updateCookieFile(updates, originYouTube); err != nil {
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
//   - Deletions remove the row. See resolveRowUpdate for why a value refresh
//     may cross domain variants while a deletion may not.
//
// origin is the SITE whose response produced these updates, and the caller has
// to state it because two of the matching rules need it and neither can recover
// it from the map: a Set-Cookie with no Domain= is host-scoped to the response
// that carried it, and that response is not in `updates`. resolveRowUpdate's
// rule 2 and sameCookiePlatform's Domain-less default both used to assume
// youtube.com, which was a true statement about the single call site rather than
// about those functions — correct today, and silently wrong in the
// DESTROY-SCOPE direction the day a second caller appears (the open one being a
// re-auth ingest response from accounts.google.com, whose unscoped deletions
// would have reached .youtube.com rows).
//
// This parameter is ENFORCEMENT of the existing rule, not a change to it: with
// originYouTube every case behaves exactly as before. The
// grow-broadly/destroy-narrowly asymmetry is likewise unchanged and deliberate —
// name-loose updates re-sync stale twins on purpose, domain-strict deletions
// keep .google.com auth out of reach of an unscoped YouTube deletion.
func (rs *RefreshService) updateCookieFile(updates map[cookieUpdateKey]cookieUpdate, origin cookieOrigin) error {
	filePath := rs.jar.GetFilePath()
	if filePath == "" {
		return fmt.Errorf("no cookie file path configured")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read cookie file: %w", err)
	}

	// Index by name once so each row costs a map lookup rather than a scan of
	// every pending update.
	byName := make(map[string][]cookieUpdateKey, len(updates))
	for k := range updates {
		byName[k.Name] = append(byName[k.Name], k)
	}

	var result strings.Builder
	handled := make(map[cookieUpdateKey]bool)
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
				rowDomain := strings.TrimPrefix(strings.TrimSpace(parts[0]), "#HttpOnly_")
				// essentialYouTubeCookies is a set of NAMES, and several of them
				// (PREF, CONSENT, YSC, LOGIN_INFO, the rotating SIDTS/SIDCC
				// pair) are not YouTube-exclusive strings — just names YouTube
				// happens to use. So a row only carries an essential YouTube
				// cookie when its DOMAIN says so too, the same guard Arc 5 put on
				// jar.Load and isEssentialCookie.
				//
				// Both readers below select log SEVERITY only and gate no
				// mutation, which is why the unguarded form was not wrong on the
				// wire. It is guarded because it was the last surviving copy of a
				// shape this plan has now removed three times, and the next
				// reader would reasonably lift it somewhere it does decide
				// something.
				rowHasEssential := essentialYouTubeCookies[cookieName] &&
					(isYouTubeDomain(rowDomain) || isGoogleDomain(rowDomain))
				if key, cu, ok := resolveRowUpdate(updates, byName[cookieName], rowDomain, origin); ok {
					handled[key] = true
					if cu.Delete {
						// Drop the row. Writing it back with an empty value
						// left a credential-shaped hole nothing could prune.
						if rowHasEssential {
							rs.logger.Info("youtube session refresh: the server deleted an essential cookie — the signed-in session may have ended",
								"name", cookieName, "domain", rowDomain)
						} else {
							rs.logger.Debug("youtube session refresh: server deleted a cookie", "name", cookieName, "domain", rowDomain)
						}
						continue
					}
					// An empty value with NO expiry attribute is refused, not
					// applied. Scoped to this path only — the global version of
					// this guard was rejected in review, and this function is
					// reachable from processYouTubeSetCookies alone.
					//
					// Two reasons it is a refusal rather than a third deletion form:
					//
					//  1. This package cannot represent an empty-valued row at
					//     all. CookieJar.Load TrimSpaces the line first, so the
					//     trailing tab disappears, the row reads as 6 fields and
					//     is skipped as malformed — the credential vanishes from
					//     the jar while the row sits in the file. Writing one is
					//     never the right answer.
					//  2. The server has two unambiguous ways to say "delete"
					//     (a past Expires, Max-Age<=0) and both are honoured
					//     above, and a real Google logout carries a past
					//     Expires — so it takes the deletion branch and never
					//     reaches here. A bare "NAME=" states no intent.
					//     Stronger still: this function only ever runs on a
					//     response YouTube just told us was AUTHENTICATED
					//     (refresh.go's `if authenticated` gate, further
					//     narrowed by authResponseIsOurs, the non-200 check
					//     and the unreadable-body check). A reply that asserts
					//     "you are signed in" while blanking the credential
					//     that proves it is self-contradictory; a value-
					//     stripping intermediary explains it, a logout does
					//     not. Keeping a stale value is recoverable — the
					//     auth check fails, park/sweep flags it, and the Warn
					//     below says so. Destroying a live one is not.
					//
					//     (Not "a truncated response": Set-Cookie is a header,
					//     and net/http parses the whole header block before Do
					//     returns, so a truncated body cannot blank one.)
					if cu.Value == "" {
						if rowHasEssential && strings.Join(parts[6:], "\t") != "" {
							rs.logger.Warn("youtube session refresh: refused to blank an essential cookie — the Set-Cookie carried an empty value but no expiry, so it is not a deletion and the existing value was kept",
								"name", cookieName, "domain", rowDomain)
						} else {
							rs.logger.Debug("youtube session refresh: ignoring empty-valued Set-Cookie with no expiry", "name", cookieName, "domain", rowDomain)
						}
						result.WriteString(line)
						result.WriteString("\n")
						continue
					}
					// Rebuild the row from EXACTLY the first seven fields of the
					// row being replaced, with the new expiry and value
					// substituted in. That is a claim about the row READ, and it
					// is the one that matters: CookieJar.Load reads fields 6.. as
					// one value that may itself contain tabs, so a live row can
					// arrive split into 8+ parts, and assigning parts[6] and
					// re-joining left the tail of the replaced value dangling on
					// the end of the new one.
					//
					// It is NOT a claim about the row WRITTEN. A tab inside
					// cu.Value emits 8+ fields again, and both readers in this
					// package handle that correctly — CookieJar.Load joins fields
					// 6.. back into one value, and mergeCookieFiles keys on
					// fields 0 and 5 and carries the whole line verbatim.
					//
					// parts[0] is emitted verbatim, which also settles the
					// "#HttpOnly_" question for this path: the prefix is
					// PRESERVED when the existing row carries it and never ADDED
					// when it does not. For a rewrite the file's own row is the
					// authority on HttpOnly-ness — it is what a browser export or
					// a previous insertion recorded — and this path is changing a
					// value and an expiry, not re-deciding the flag. A
					// Set-Cookie's HttpOnly attribute only matters on INSERTION,
					// where no existing row can be that authority and cu.HTTPOnly
					// is read instead (below). The consequence is that a server
					// which starts or stops sending HttpOnly for a cookie already
					// in the file does not flip the prefix; nothing in this
					// package treats the flag as a control (CookieJar.Load,
					// rowExpired and mergeCookieFiles all merely strip it, and
					// the jar does not retain it), so the cost is a stale
					// annotation rather than a downgraded cookie.
					result.WriteString(strings.Join([]string{
						parts[0], parts[1], parts[2], parts[3],
						strconv.FormatInt(cu.Expiry, 10),
						parts[5], cu.Value,
					}, "\t"))
					result.WriteString("\n")
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

	// Add new cookies that weren't found in the existing file. A deletion for
	// a row that is not there is simply done — it must never be inserted. Nor
	// may an empty value: same refusal as the rewrite path above, and an
	// inserted empty row would be one this package's own reader cannot read.
	for key, cu := range updates {
		if handled[key] || cu.Delete {
			continue
		}
		if cu.Value == "" {
			rs.logger.Debug("youtube session refresh: not inserting an empty-valued cookie", "name", key.Name, "domain", key.Domain)
			continue
		}
		name := key.Name
		domain := key.Domain
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
		// An HttpOnly cookie is written as a "#HttpOnly_"-prefixed row — the
		// Netscape convention every reader in this package already honours
		// (CookieJar.Load, rowExpired, mergeCookieFiles). Inserting it without
		// the prefix silently downgraded the flag.
		prefix := ""
		if cu.HTTPOnly {
			prefix = "#HttpOnly_"
		}
		// Netscape format: domain, include_subdomains, path, secure, expiry, name, value
		if _, werr := fmt.Fprintf(&result, "%s%s\t%s\t/\t%s\t%d\t%s\t%s\n",
			prefix, domain, subdomains, secure, cu.Expiry, name, cu.Value); werr != nil {
			return fmt.Errorf("write new cookie row: %w", werr)
		}
		rs.logger.Debug("added new cookie to file", "name", name, "domain", domain)
		handled[key] = true
	}

	// Atomic write via the shared same-package helper — it uses a unique
	// temp name (the AutoCookieService writes the same cookies.txt, so a
	// fixed ".tmp" would let the two writers interleave) and applies the
	// memoized parent-dir DACL tightening.
	if err := writeFileAtomic(filePath, []byte(result.String()), 0o600); err != nil {
		return err
	}

	if len(handled) > 0 {
		rs.logger.Debug("updated cookies in file", "updated", len(handled))
	}

	return nil
}

// resolveRowUpdate picks the pending update that applies to one file row, from
// the candidate keys that already share the row's cookie name.
//
// The rule is asymmetric on purpose: grow broadly, destroy narrowly.
//
//   - A value refresh may cross domain variants, but only within one platform.
//     The same session value is valid on .youtube.com and .google.com alike,
//     and leaving one variant stale while the other moves on is the drift that
//     finding #4 was about. Crossing to .twitch.tv is a different matter:
//     growing onto another platform's occupied slot IS destruction, so the
//     platforms are kept apart even though no name collides between them today.
//   - A deletion may not cross at all. It is unrecoverable, so it only ever
//     removes rows inside the scope the server actually named.
//
// The narrow half is deliberately under-applied rather than over-applied: a
// deletion scoped to ".youtube.com" does NOT remove a host-only
// "www.youtube.com" row, even though RFC 6265 domain-matching says it covers
// it. Browser extraction really does write host-only rows, so this is
// reachable — and the chosen failure is a stale row that keeps being sent
// (recoverable) over a deleted credential (not). Do not "fix" it into a
// suffix match without re-deciding that trade.
//
// At most one candidate can scope-match a given row: Domain= is normalized to
// one lowercased leading-dot form before it becomes a key, so two distinct keys
// for one name always name different hosts. Both halves of that normalization
// are load-bearing — without the lowercasing, ".YouTube.com" and ".youtube.com"
// are separate keys that both match, and which one wins is map-iteration order.
func resolveRowUpdate(updates map[cookieUpdateKey]cookieUpdate, candidates []cookieUpdateKey, rowDomain string, origin cookieOrigin) (cookieUpdateKey, cookieUpdate, bool) {
	if len(candidates) == 0 {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}
	// 1. A Set-Cookie scoped to this row's own host always wins.
	for _, k := range candidates {
		if k.Domain != "" && sameCookieScope(k.Domain, rowDomain) {
			return k, updates[k], true
		}
	}
	// 2. A Set-Cookie with no Domain= is host-scoped to the response that
	//    carried it, so it may only reach rows inside the site the CALLER
	//    declared that response came from. Confining it that way is what stops
	//    an unscoped deletion in a youtube.com reply reaching .google.com auth —
	//    and, the day a second caller exists, an unscoped deletion in a
	//    google.com reply reaching .youtube.com. An undeclared origin covers
	//    nothing, so the rule simply does not fire.
	if origin.covers(rowDomain) {
		for _, k := range candidates {
			if k.Domain == "" {
				return k, updates[k], true
			}
		}
	}
	// 3. Otherwise only a value refresh may cross domains — within one platform,
	//    and only when a single non-deleting update is in play so the choice is
	//    unambiguous.
	var only cookieUpdateKey
	refreshes := 0
	for _, k := range candidates {
		if !updates[k].Delete && sameCookiePlatform(k.Domain, rowDomain, origin) {
			only, refreshes = k, refreshes+1
		}
	}
	if refreshes == 1 {
		return only, updates[only], true
	}
	return cookieUpdateKey{}, cookieUpdate{}, false
}

// sameCookiePlatform reports whether an update's domain and a file row's domain
// belong to the same credential platform. YouTube and Google are one platform:
// a Google session covers both, which is exactly why a refresh is allowed to
// fan out across them. Twitch is another, and a row on neither matches nothing.
//
// An update with no Domain= counts as the platform of the site the CALLER
// declared the response came from. Like rule 2 in resolveRowUpdate, that is a
// property of the call site and not of this function, which is why it arrives as
// a parameter instead of the hardcoded "google" that used to stand here. An
// undeclared origin has no platform, so a Domain-less update matches nothing —
// the narrow direction, which is the safe one.
func sameCookiePlatform(updateDomain, rowDomain string, origin cookieOrigin) bool {
	up := origin.platform()
	if updateDomain != "" {
		up = cookiePlatformOf(updateDomain)
	}
	return up != "" && up == cookiePlatformOf(rowDomain)
}

// sameCookieScope reports whether two domain strings name the same host. The
// leading dot only encodes the Netscape include-subdomains flag, so
// ".youtube.com" and "youtube.com" are one scope written two ways.
func sameCookieScope(a, b string) bool {
	return strings.EqualFold(
		strings.TrimPrefix(strings.TrimSpace(a), "."),
		strings.TrimPrefix(strings.TrimSpace(b), "."),
	)
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
	// The error names the status and nothing else, so a response body echoed
	// back by an intermediary can never be interpolated into it. It does NOT
	// reach AutoCookieService.setError — services.go wires VerifyTwitchAuth to
	// CheckTwitchAuth through checkPlatformAuth, which discards the value. See
	// errGuideLoginMarkerUnreadable's doc comment for the real surface.
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized:
		return false, nil
	default:
		return false, fmt.Errorf("twitch auth check: unexpected status %d", resp.StatusCode)
	}
}

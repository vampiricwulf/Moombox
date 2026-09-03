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
// deliberately inert: covers reports false for every row, platform reports
// nothing, and updateCookieFile's insertion loop refuses every row whose
// platform does not match the declared one — which for the zero value is all of
// them. So a caller that forgets to declare an origin updates only rows whose
// own Domain= scope-matches, and writes nothing new at all.
//
// That last clause is load-bearing and was wrong when this type was introduced.
// "Matches nothing" and "does nothing" are different: an update no row accepts
// falls through to the insertion loop, which invents a domain from the cookie
// name. Without the platform check there, an inert origin was not narrow — it
// appended new rows under a domain nobody declared.
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
//
// EVERY FIELD HERE HAS A READER, and that is a property to keep rather than a
// coincidence. There used to be a LastCheck string, set by doRefresh on every
// pass and read by nothing — no projection carried it, and since the struct is
// never marshalled its `json:"lastCheck"` tag put it on no wire either. It was
// removed rather than wired: a field nobody reads on a status struct that two
// dashboards consume is a claim waiting to be misread — the obvious misreading
// being "the credentials were valid as of this time", which the timestamp of a
// pass that may have concluded NOTHING does not say. Anything re-added here
// needs a reader in the same change, and if it moves on every tick it also
// needs a line in authStatusChanged's exclusion list.
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

// The fixed vocabulary of NoteTwitchAuthLoss's reason.
//
// These mirror internal/twitch's AuthDowngrade* constants BY VALUE and cannot
// import them: internal/twitch imports THIS package (twitch/auth.go,
// twitch/service.go), so the dependency only runs one way. The pin against
// drift lives in internal/worker, which imports both
// (TestTwitchAuthLossVocabularyCoversEveryDowngradeReason). Every member added
// here needs its twitch-side twin added to that test's slice in the same
// change, or a value that drifts apart from its twin is caught by nothing.
//
// Opaque tokens, never sentences and never format strings: there is no verb
// here to interpolate a token, a login or a wire line into.
const (
	twitchLossLoginRefused        = "login-refused"
	twitchLossLoginUnacknowledged = "login-never-acknowledged"
	twitchLossNoLoginCookie       = "no-login-cookie"
	twitchLossUnusableLoginCookie = "unusable-login-cookie"
	// The one member that does NOT come from the chat handshake: the playback
	// access token Twitch minted for a capture was minted for nobody although
	// credentials were sent (Arc 10 R6). It is the only route a job with chat
	// capture switched off can produce.
	twitchLossPlaybackTokenAnonymous = "playback-token-anonymous"
)

// twitchAuthLossMessage renders the operator sentence for one downgrade route.
//
// THE SWITCH IS THE LEAK BARRIER, not a convenience. NoteTwitchAuthLoss's
// caller lives in internal/worker and hands over a token it received from
// internal/twitch; AuthStatus.TwitchError then reaches two per-request
// operator surfaces (routes.TwitchAuthStatusPayload's `twitchError` and the
// TUI's R C result line). Because every arm below returns a string LITERAL,
// the SET of strings that field can hold is fixed at compile time and no
// input — not a future upstream token, not a value read off the wire — can
// widen it. Returning the reason, or interpolating it, would move that
// guarantee from the type system to the caller's discipline.
//
// The default arm exists for a token added upstream without an arm here. It
// must still say a credential is broken: a status line that names no problem
// is worse than the log line it was meant to escape.
func twitchAuthLossMessage(reason string) string {
	switch reason {
	case twitchLossLoginRefused:
		return "Twitch refused the saved login."
	case twitchLossLoginUnacknowledged:
		return "Twitch never acknowledged the saved login."
	case twitchLossNoLoginCookie:
		return "The cookie file has a Twitch auth-token but no login cookie beside it."
	case twitchLossUnusableLoginCookie:
		return "The Twitch login cookie is not a name that can be sent to chat."
	case twitchLossPlaybackTokenAnonymous:
		return "Twitch issued an anonymous playback token although saved credentials were sent."
	default:
		return "The saved Twitch login could not be used."
	}
}

// twitchAuthMark is a Twitch credential failure observed somewhere OTHER than
// the periodic oauth2/validate check, held until the credential pair changes.
//
// It exists because validate CANNOT SEE two of the four ways a Twitch capture
// goes anonymous. An auth-token with no `login` beside it, and one with a
// `login` that cannot be sent as an IRC nickname, both leave the TOKEN valid —
// so validate answers 200, the platform reads green forever, and every
// subscriber-only message and badge is dropped for the whole job. A mark that
// validate could overwrite would therefore be no mark at all: it would be
// erased within one 30-minute tick with nothing fixed.
//
// The zero value is "no mark". `identity` is CookieJar.TwitchIdentity() as of
// the moment the mark was taken, and it is the ONLY thing that clears the mark
// — see refresh's status block. `reason` is a member of the vocabulary above
// and never anything read from the jar or the wire.
//
// A mark taken on a jar holding NO Twitch credentials writes a failed verdict
// and a reason for a platform nobody configured. That is inert rather than
// wrong, and deliberately left so: every surface takes its not-configured arm
// first (cookieBadgeFor returns CookieStatusNone on !hasCookies,
// cmd/moombox/tui_wiring.go; the web indicator branches on !found before it
// reads the verdict), so neither field is rendered. Suppressing the write
// would buy nothing and would add a second rule to a type whose whole value is
// having one.
type twitchAuthMark struct {
	set      bool
	reason   string
	identity string
}

// authStatusChanged reports whether anything a SURFACE renders differs between
// two consecutive checks. It is the OnAuthChange gate.
//
// Compared: the two auth booleans, the two cookies-present flags and the two
// verdicts — i.e. every input to the TUI badge in cmd/moombox/tui_wiring.go and
// to the Web indicators. Deliberately NOT compared:
//
//   - YouTubeError / TwitchError, whose text can vary between two occurrences
//     of the same outcome (a DNS message carries the resolver's wording), so
//     comparing them would fire the callback on churn no verdict transition
//     accompanies. The verdict carries the part a PUSH surface renders.
//
// That second exclusion is now a CONTRACT rather than an observation, and the
// distinction is the whole of it. This comment used to say "nothing renders the
// strings"; Arc 8 Task 12a made that false — they reach the REST cookie-status
// payload (`youtubeError` / `twitchError`) and the TUI's R C result line. Both
// of those are PER-REQUEST: each pulls a GetStatus() snapshot it asked for, so
// neither depends on this callback firing and neither can go stale on screen.
// The rule that keeps this gate correct is therefore stated forwards: NO
// OnAuthChange-driven surface may render the two strings; per-request surfaces
// may. Widening this gate to include them is the PRECONDITION for a push-driven
// surface that renders them — and it is a deliberate change with its own cost,
// not something to slip in beside one. See errGuideLoginMarkerUnreadable's doc
// comment for why per-request is the concession that let the fields be read at
// all.
//
// (There used to be a third exclusion, LastCheck, which moved on every tick and
// would have fired the callback unconditionally. The field is gone — see
// AuthStatus — so the exclusion is not needed.)
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
// means "the platform told us", not "we asked". A consent wall, a rate limit, an
// off-host redirect and a never-configured jar are all silence, and passing
// any of them in as loggedIn=false would report working credentials as dead.
//
// Three producers exist today: YouTube's per-channel membership probe (which
// runs once per configured channel per feed cycle), YouTube's
// channel-independent FallbackLiveness probe, and Twitch's
// TwitchFallbackLiveness probe. The first is why the dedupe is not optional —
// one dead session must raise one alarm, not one per channel.
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
		// has three producers — the per-channel membership probe and the two
		// channel-independent fallbacks — and cannot tell which sent this
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

// NoteTwitchAuthLoss records that Twitch credentials this install HOLDS were
// refused, or could not be used, by something other than the periodic
// oauth2/validate check — today the IRC chat handshake.
//
// THIS IS THE SECOND WRITER OF rs.status, and the only one that is not
// refresh's status block. Both write under rs.mu, and the rule between them is
// stated once, here, and enforced there: while a mark stands it WINS for
// TwitchAuthenticated, TwitchVerification and TwitchError, and only a changed
// credential fingerprint clears it. The sole-writer property that used to hold
// is gone on purpose; nothing else about the locking discipline changed.
//
// reason must be a member of the fixed vocabulary (twitchLossLoginRefused and
// friends). Nothing derived from a cookie value, a login, or a wire line may
// be passed here — and twitchAuthLossMessage refuses to render anything else
// anyway, which is what keeps AuthStatus.TwitchError's contents a compile-time
// set rather than a caller's promise.
//
// Recovery uses the SAME dedupe a validate-found loss gets. shouldFireRecovery
// is evaluated against this platform's own two pieces of baseline state and
// they are then advanced exactly as refresh advances them, so one loss raises
// one alarm however many times this is called for it, and a loss that follows
// a genuine repair raises a new one.
//
// nowAuth=false and checkErr=nil are not assumptions: a downgrade IS the
// conclusive negative. Something tried to use these credentials against Twitch
// and Twitch would not take them, which is a stronger statement than the
// endpoint check makes.
//
// Callers reach this from ChatDownloader's OnAuthDowngrade, which runs on the
// IRC session goroutine with the read loop parked behind it. This function
// makes no network call and holds no lock across a callback — but the
// callbacks it invokes may block (handleRecoveryNeeded's auto_enabled=false
// arm sends a webhook synchronously), so cmd/moombox's wiring must call it on
// its own goroutine. That wiring is twitchAuthLossHook in
// cmd/moombox/services.go, plugged into DownloadWorker.SetOnTwitchAuthLoss:
// it spawns the recover-guarded goroutine before calling here, so the
// obligation is met in the tree, not merely stated.
func (rs *RefreshService) NoteTwitchAuthLoss(reason string) {
	var (
		changed      bool
		fireRecovery bool
		statusCopy   AuthStatus
	)
	// Scoped into a func literal so the unlock is DEFERRED, for the reason
	// refresh's own status block documents: rs.mu is a plain non-reentrant
	// RWMutex, and a panic unwinding with the write lock held would park the
	// goroutine holding it and block every later GetStatus forever.
	func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()

		prev := rs.status
		rs.twitchMark = twitchAuthMark{
			set:    true,
			reason: reason,
			// Sampled under the same lock as the write, so the mark can never
			// be keyed to a pair that was already replaced by the time it
			// landed.
			identity: rs.jar.TwitchIdentity(),
		}
		rs.status.TwitchAuthenticated = false
		rs.status.TwitchVerification = RefreshFailed
		rs.status.TwitchError = twitchAuthLossMessage(reason)
		changed = authStatusChanged(prev, rs.status)
		statusCopy = rs.status

		// The dedupe, decided under the lock and advanced under it, so two
		// concurrent downgrades on one dead pair cannot both witness the
		// transition.
		fireRecovery = rs.OnRecoveryNeeded != nil &&
			shouldFireRecovery(rs.twEverConcluded, rs.prevTwitchAuth, false, nil, rs.jar.HasAnyTwitchAuthCookie())
		rs.prevTwitchAuth = false
		rs.twEverConcluded = true
		// hasCheckedOnce is deliberately NOT touched. It is service-wide and
		// means "a refresh pass has completed"; a chat downgrade is not one,
		// and setting it here would let a Twitch handshake decide whether
		// YouTube's first OnAuthRecovered transition is allowed to fire.
	}()

	// Both callbacks reach out into cmd/moombox and must not run under this
	// service's mutex, following refresh's convention exactly.
	if changed && rs.OnAuthChange != nil {
		rs.OnAuthChange(statusCopy)
	}
	if fireRecovery {
		// Stamp the shared dedupe map for the same reason the tier-1 fire does:
		// a liveness verdict landing in the same window must not fire recovery
		// for a problem this one is already working on.
		rs.noteRecoveryDecided("twitch", time.Now())
		// The SENTENCE, not the caller's `reason`. This function's own doc
		// says the switch is the leak barrier rather than the caller's
		// discipline, and logging the raw argument would quietly make that
		// false. TestTwitchAuthLossWarnCarriesTheMappedSentenceOnly watches
		// this exact line through a recording logger with a credential-shaped
		// reason; TestTwitchAuthLossReasonIsTheVocabularyOnly pins the switch
		// itself.
		rs.logger.Warn("twitch credentials were refused where they were used, triggering recovery",
			"reason", twitchAuthLossMessage(reason))
		rs.OnRecoveryNeeded("twitch")
	}
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
		hasTwitchToken             bool
		ytIdentity, prevYTIdentity string
		twIdentity, prevTWIdentity string
		twEffective                bool
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

		// The NARROW predicate, and deliberately not hasTWCookies. The tier-2
		// Twitch probe sends the bearer token as the credential; without it
		// Twitch mints an anonymous playback token by design and the probe
		// declines. Sampled here so the gate and every other snapshot this
		// pass reasons about describe the same reload.
		hasTwitchToken = rs.jar.HasTwitchAuthCookies()

		// Sampled under the same lock as the rest of this check's snapshots, and
		// AFTER the jar.Reload() at the top of doRefresh, so it reflects whatever
		// account is on disk right now.
		ytIdentity = rs.jar.YouTubeIdentity()
		prevYTIdentity = rs.prevYouTubeIdentity

		twIdentity = rs.jar.TwitchIdentity()
		prevTWIdentity = rs.prevTwitchIdentity

		// THE MARK, and the rule that makes rs.status's two writers coherent.
		//
		// A downgrade observed outside this check (NoteTwitchAuthLoss) stands
		// until the credential PAIR changes, and while it stands it wins over
		// validate for every Twitch conclusion drawn below. It has to: validate
		// answers 200 for a valid auth-token whether or not a usable `login`
		// sits beside it, so without this a no-login-cookie downgrade would be
		// erased on the next tick with nothing repaired.
		//
		// Clearing is keyed on the FINGERPRINT ALONE, with no authenticated
		// gate. Gating it on nowAuth would leave a stale mark in front of an
		// operator whose broken pair was replaced by a REVOKED one: they would
		// be told to add a login row while the real answer is a 401. Clearing
		// here and letting validate write the truth is both simpler and
		// honest — which is what "the mark clears and validate decides the
		// status again" says.
		twMarked := false
		if rs.twitchMark.set {
			if rs.twitchMark.identity != twIdentity {
				rs.twitchMark = twitchAuthMark{}
			} else {
				twMarked = true
			}
		}
		// twEffective is the Twitch auth answer everything below this line
		// uses — the status, the previous-auth baseline, the recovery gate, the
		// recovered transition and (Task 3) the identity baseline. ONE value
		// rather than a mark check at each site: five sites each deciding for
		// themselves is five chances for one to disagree, and the site that
		// would disagree first is OnAuthRecovered, which would announce a
		// recovery that never happened and resume every parked Twitch job into
		// the same failure.
		twEffective = twAuth && !twMarked
		twVerification := verdictFromCheck(twAuth, twErr)
		twStatusErr := twErrStr
		if twMarked {
			twVerification = RefreshFailed
			twStatusErr = twitchAuthLossMessage(rs.twitchMark.reason)
		}

		rs.status = AuthStatus{
			YouTubeAuthenticated: ytAuth,
			TwitchAuthenticated:  twEffective,
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
			TwitchVerification:  twVerification,
			YouTubeError:        ytErrStr,
			TwitchError:         twStatusErr,
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
			rs.prevTwitchAuth = twEffective
			rs.twEverConcluded = true
		}
		// Same rule as YouTube's, and deliberately outside the `twErr == nil`
		// block above for the same reason: the baseline advances only on a
		// check that also AUTHENTICATED, so a stale intermediate export cannot
		// consume the edge the properly re-exported one needs.
		//
		// twEffective, not twAuth — a marked platform has not authenticated,
		// whatever validate says, so a marked pass is not an observation of a
		// working pair and must not become the baseline one is compared
		// against. This is the fifth consumer of the single value the mark
		// block computes, and the one residual it accepts is narrow: holding
		// the baseline at the PRE-mark pair means a repair that reverts to
		// exactly that pair compares equal and fires nothing. Reaching it
		// requires the downgrade to land before any pass ever observed the
		// marked pair, which every credential write ending in a re-check
		// closes — Task 7a's job. If that task slips, the line to revisit is
		// this one, and the choice is encoded white-box in
		// TestAStandingTwitchMarkFiresNoCredentialChange's
		// `rs.prevTwitchIdentity != ""` assertion
		// (refresh_twitch_identity_test.go), which is what a revisit has to
		// move first.
		rs.prevTwitchIdentity = advanceIdentityBaseline(rs.prevTwitchIdentity, twIdentity, twEffective, twErr)
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
		if shouldFireRecovery(twConcluded, prevTW, twEffective, twErr, hasTWCookies) {
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
		if !prevTW && twEffective && twErr == nil {
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

	// The Twitch counterpart, and the one with a second subscriber: besides
	// the parked-job sweep, cmd/moombox broadcasts this to every live Twitch
	// chat downloader so a repaired cookie file reaches a capture that is
	// already running. See DownloadWorker.ReauthenticateTwitchChats.
	//
	// twEffective, not twAuth, for the reason the baseline advance above
	// gives: while a mark stands the pair has NOT been observed working, and
	// announcing that it has would reconnect every live IRC session straight
	// back into the downgrade that took the mark.
	//
	// The identity is an opaque equality token and is handed to the callback,
	// never to the log line.
	if rs.OnCredentialsChanged != nil && shouldObserveCredentials(prevTWIdentity, twIdentity, twEffective, twErr) {
		rs.logger.Info("twitch credential pair observed — re-evaluating parked jobs and live chat sessions")
		rs.OnCredentialsChanged("twitch", twIdentity)
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

	// Tier 2, Twitch. The same shape as the block above, and the same pilot
	// gate withholds the same last step. What differs is the jar condition:
	// this probe SENDS the auth-token, so an install without one is not
	// "unreported", it is unprobeable — and asking anyway would get an
	// anonymous playback token by design and read as a dead session.
	//
	// Runs inline on the ticker goroutine, which carries Start's inline
	// recover. Nothing is spawned.
	if allowFallback && rs.TwitchFallbackLiveness != nil && hasTwitchToken &&
		!rs.livenessObservedRecently("twitch", time.Now()) {
		// Only a conclusive answer moves anything. `false, false` is no
		// configured channel, a rate limit, a transport failure, or a 401/403
		// that may be an edge block — never a dead session.
		if loggedIn, conclusive := rs.TwitchFallbackLiveness(ctx); conclusive {
			rs.ObserveLiveness("twitch", loggedIn)
		} else {
			// Deduped through the same record a verdict uses, so an install
			// that can never answer — no Twitch channel configured, a
			// permanently refused request — says so once per process instead
			// of once per cycle. No second configured-platform gate is needed
			// the way the YouTube arm needs one: hasTwitchToken above already
			// established that there is a session to report on.
			//
			// The reason is not here because the (loggedIn, conclusive) pair
			// cannot carry one; cmd/moombox's closure logs the probe's own
			// error at Debug, where it has it.
			logAt := rs.logger.Debug
			if rs.recordInconclusiveLiveness("twitch") {
				logAt = rs.logger.Info
			}
			logAt("liveness fallback probe learned nothing about this session", "platform", "twitch")
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
// touches one site (audit reports/cookies.md #35) — that site is now
// constants.WebClient.ClientVersion, the single source of truth for every
// WEB-family client's version string.
func youtubeGuideRequestBody() string {
	return `{"context":{"client":{"clientName":"WEB","clientVersion":"` + constants.WebClient.ClientVersion + `","hl":"en"}}}`
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
// WHERE THIS STRING ACTUALLY GOES, checked rather than assumed, because this
// comment has been wrong in both directions: an early draft claimed the Web UI
// and TUI when nothing read the field at all, and its replacement claimed no
// reader anywhere when Arc 8 Task 12a had given it two.
// AuthStatus.YouTubeError, the field doRefresh assigns it to, has exactly two
// readers today and both are PER-REQUEST:
//
//   - internal/web/routes/cookies.go's CookieStatusPayload /
//     TwitchAuthStatusPayload — the one copy of each wire shape, shared with
//     cmd/moombox/routes_wiring.go's status route — which project it as
//     `youtubeError` / `twitchError`;
//   - cmd/moombox/tui_wiring.go's OnRecheckCookies, which passes it through as
//     the reason on the R C result line.
//
// Nothing else reads it. authStatusToTUI's badge and the Web indicators still
// project from the booleans and the verdicts alone, and the second possible
// route stays closed: checkPlatformAuth consumes the error for an errors.Is
// test and discards the value, so the rollback messaging composes from the
// verification STATE and never interpolates this text.
//
// PER-REQUEST IS THE WHOLE CONCESSION, and it is what answered the objection
// that kept this field unread for two arcs. The fact the operator needs —
// "this check could not conclude" — is carried by RefreshUnknown, which is a
// bounded value; this string is server-authored prose whose lifecycle nothing
// establishes, so rendering it in an ALWAYS-ON panel would put unattributable
// text on screen indefinitely. A surface the operator asked for a moment ago
// has a lifecycle by construction: it answers one question once and is replaced
// by the next answer. authStatusChanged is where that rule is enforced — it
// excludes these two fields from the OnAuthChange gate as a contract, so no
// push-driven surface can start rendering them without someone widening the
// gate on purpose.
//
// The remaining sink is rs.logger.Debug in doRefresh, and what that means
// depends on the operator's log level — which makes the no-body-bytes rule
// below MORE load-bearing than "it goes to a log", not less, and more
// load-bearing again now that the same string also reaches two screens:
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
// So that sink is conditional, persistent (file + DB), and remotely readable
// — and DEBUG is exactly the level an operator raises to when their cookies
// look broken, i.e. precisely when this error fires. TestUnreadableGuideError-
// CarriesNoBody earns its place on that basis: this error names no host, no
// header and no body bytes. The unreadable body is the subject of the report
// and must never become its content.
//
// The wording says "learned nothing", not "failed", and both UIs now agree with
// it. Follow-up 1 of the remediation plan — surface the inconclusive state in
// both UIs — landed as Arc 4+7's S12 (merge f2b4e30): verdictFromCheck maps this
// error to RefreshUnknown, AuthStatus carries that verdict beside each boolean,
// CookieStatusPayload projects it as `verification` for the Web indicators
// (cookieIndicatorState in web/public/modules/utils.js) and cookieBadgeFor reads
// it for the TUI status bar. So an install behind an intercepting intermediary
// renders as could-not-check, not as the "cookies found, not authenticated" this
// paragraph used to warn was still on screen.
//
// doRefresh does still set YouTubeAuthenticated: ytAuth, false on an
// inconclusive check, and that is deliberate rather than left over:
// `authenticated` keeps its "can we do authenticated work right now" meaning for
// every reader that predates the verdicts, and the verdict is what carries the
// distinction they cannot.
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
// bytes. Since Arc 8 Task 12a they reach two PER-REQUEST screens as
// AuthStatus.YouTubeError / TwitchError — the REST cookie-status payload and the
// TUI's R C result line — besides the Debug log; for the full accounting, and
// why that makes the rule more load-bearing rather than less, see
// errGuideLoginMarkerUnreadable's doc comment. They do NOT reach
// AutoCookieService.setError: checkPlatformAuth discards the value.
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
//
// It takes no URL: the two entry points were reading the same endpoint from two
// differently-named vars (see youtubeGuideURL), so the URL was never a thing
// they varied on.
func (rs *RefreshService) youtubeGuideExchange(ctx context.Context) (bool, *http.Response, error) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideURL, strings.NewReader(body))
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
	authenticated, _, err := rs.youtubeGuideExchange(ctx)
	return authenticated, err
}

// checkAndRefreshYouTube makes a single guide API request to both check
// YouTube auth status and refresh session cookies from Set-Cookie headers.
// This avoids the redundancy of separate check + refresh requests.
//
// It is the only caller in this file that writes the jar from a guide reply.
func (rs *RefreshService) checkAndRefreshYouTube(ctx context.Context) (bool, error) {
	authenticated, resp, err := rs.youtubeGuideExchange(ctx)
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

	// originYouTube: this function is only ever fed a youtube.com guide
	// response, and admitSetCookie needs to be told so — a Set-Cookie with no
	// Domain= is host-scoped to the response that carried it, and the header
	// alone does not say which response that was.
	updates := make(map[cookieUpdateKey]cookieUpdate)
	refused := 0
	for _, sc := range setCookies {
		key, cu, ok := admitSetCookie(sc, originYouTube)
		if !ok {
			refused++
			continue
		}
		updates[key] = cu
	}
	if refused > 0 {
		// A COUNT, never the header. A Set-Cookie is the credential itself, and
		// a refused one is by definition the shape we did not vouch for, so
		// neither its value nor its raw text may be written to a log.
		rs.logger.Debug("youtube session refresh: Set-Cookie headers not admitted", "count", refused)
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
	//
	// The same originYouTube is declared a second time here, to the write path,
	// which needs it for its own three decisions — see updateCookieFile.
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

// maxCookieLifetime caps how far into the future a Max-Age may push a row's
// expiry: 400 days, which is RFC 6265bis §5.5's limit and what Chrome, Edge and
// Safari already enforce. A browser-exported cookies.txt therefore never carries
// a longer one, so the clamp brings this writer into line with the rest of the
// file rather than shortening anything the file could otherwise hold.
//
// It is also what makes the arithmetic safe. `now + maxAge` on an int64 wraps:
// Max-Age=9223372036854775807 parses fine and produces a large NEGATIVE expiry,
// which lands in Netscape field 5 — and every expiry guard in this package is
// `exp > 0 && exp < now` (rowExpired, CookieJar.Load's capture,
// ExpiredAuthCookiesFor), so a negative value reads as "not expired" everywhere.
// The row would be unprunable AND invisible to the freshness accounting, and
// yt-dlp's loader would reject it outright on its `[0-9]+` expires match.
// Clamping the addend below the cap makes the overflow unreachable rather than
// detected.
//
// Deliberately applied to Max-Age ONLY. Expires stays exactly as it is: real
// Google auth cookies carry multi-year Expires values, and clamping those would
// shorten what ExpiredAuthCookiesFor and AuthCookieHorizonFor report about a
// session this code did not actually change.
const maxCookieLifetime = int64(400 * 24 * 60 * 60)

// rowBreakingChars are the bytes a Netscape cookie row cannot carry inside any
// of its fields: tab is the field separator, CR and LF end the row, and NUL is
// not representable in a line-oriented text file at all.
//
// Only the TAB is a live vector, and the distinction is worth stating so nobody
// later reads this as a defence against header injection. Go's HTTP client
// cannot hand this package a CR, LF or NUL inside a header value at all:
// net/textproto's validHeaderValueByte admits only VCHAR, SP, HTAB and %x80-FF,
// and readMIMEHeader fails the entire header block otherwise — so
// resp.Header.Values("Set-Cookie") can never carry one. They are checked anyway
// because this predicate guards a WRITE into a line-oriented file, and the cost
// of the belt is one ContainsAny over strings that are already in cache.
const rowBreakingChars = "\t\r\n\x00"

// hasRowBreakingChar reports whether s would corrupt the row it is written into.
func hasRowBreakingChar(s string) bool { return strings.ContainsAny(s, rowBreakingChars) }

// trackedCookieName reports whether name is one the cookie file tracks for the
// declared origin's credential platform. It is the admission set for an
// UNSCOPED Set-Cookie — one with no Domain= of its own to be judged on.
//
// The set is the union of the two name predicates the rest of this package
// already uses for the Google platform: essentialYouTubeCookies (the names
// CookieJar.Load keeps) and isGoogleOnlyAuthName. Neither contains the other —
// isGoogleOnlyAuthName matches the whole __Secure-1P/3P families,
// essentialYouTubeCookies names PREF, CONSENT, YSC and the rest — so the union
// is exactly "a name this package can store", and a random unscoped foo=bar is
// not one.
//
// isGoogleOnlyAuthName is used here as a NAME predicate and nothing more. It
// used to double as updateCookieFile's domain-inventor for a Domain-less update
// and no longer does; see the insertion loop for why that was wrong.
//
// Only the Google platform has a set here, and the reason is now simply that
// only a Google-platform caller exists. Until this fix round there was a
// structural reason as well — updateCookieFile invented a Domain-less update's
// domain from the cookie NAME and knew only youtube.com and google.com, so an
// admitted Twitch cookie would have been refused at the write — but that
// fallback now uses the declared origin's own site and would place a .twitch.tv
// row correctly. What remains is that a new admission surface is not something
// to open speculatively. Adding essentialTwitchCookies here is a one-line change
// the day a Twitch caller arrives, and it needs that caller's tests, not a
// guess.
func trackedCookieName(name string, origin cookieOrigin) bool {
	if origin.platform() != originYouTube.platform() {
		return false
	}
	return essentialYouTubeCookies[name] || isGoogleOnlyAuthName(name)
}

// admitSetCookie parses ONE raw Set-Cookie header from a response that came from
// origin and decides whether it may become a pending cookie-file update. It is
// the OUTER admission layer: a header it turns down never reaches the write path
// in any form.
//
// B2. This loop used to open with a substring pre-filter — the header had to
// mention "youtube.com" or "google.com" somewhere — and it ran BEFORE Domain=
// was parsed. It was wrong in both directions at once:
//
//   - It dropped every legitimate unscoped rotation. RFC 6265 §4.1.2.3: a
//     Set-Cookie with no Domain= is host-scoped to the responding host, which is
//     an ordinary way for youtube.com to rotate its own first-party cookies.
//     Such a header contains neither substring, so it never reached the parser —
//     while the rest of this package plainly expected it to. resolveRowUpdate's
//     rule 2 exists to confine a Domain-less deletion, and updateCookieFile's
//     insertion loop has a whole branch for a Domain-less update; both were
//     unreachable. That is the "cookies.txt untouched for a day" symptom: it
//     fails safe, and it still strands the session. (The insertion branch had
//     rotted while it was dead — it invented a domain from the cookie NAME —
//     and had to be corrected when this commit made it live.)
//   - It was never the guard it looked like. `x=youtube.com; Domain=evil.tld`
//     passes a substring test on its VALUE. The real guard has always been the
//     parsed-Domain= check below, which its own comment already claimed it was —
//     true only for headers that carried a Domain= at all.
//
// Admission is therefore by what the header SAYS, in three steps, after the
// whole attribute list has been read:
//
//  1. Row-breaking characters in the NAME, the VALUE or the DOMAIN are refused
//     outright, before either branch. A tab splits the Netscape row into the
//     wrong fields on the next Load; CR, LF and NUL are checked as belt (see
//     rowBreakingChars — Go's header parser cannot deliver them). The VALUE is
//     included because RFC 6265's cookie-octet excludes HTAB and every control
//     character, and browsers reject them, so no legitimate rotation carries one
//     — a tab in a value is a malformed header, not a shape worth preserving.
//     CookieJar.Load's tolerance of tab-carrying rows is a separate question and
//     is unchanged: it must keep reading whatever a browser export or a
//     third-party tool already wrote into the file. This rule governs only what
//     THIS writer may add.
//  2. SCOPED (Domain= present): admitted only when the domain lies on the
//     declared origin's credential platform. For originYouTube that is precisely
//     `isYouTubeDomain || isGoogleDomain` — the test this branch has always made,
//     since those two are one platform and nothing is both Google and Twitch — so
//     the only caller's behaviour is unchanged. accounts.google.com.evil.tld,
//     evil.tld, a bare "." and a value that merely contains "youtube.com" are all
//     refused here.
//  3. UNSCOPED (no Domain=): the key keeps Domain "" and stays host-scoped to the
//     declared origin, which resolveRowUpdate rule 2, sameCookiePlatform and the
//     insertion loop each enforce downstream. This is the newly-reachable surface,
//     so it is also the narrow one: admitted only under a name the jar actually
//     tracks (trackedCookieName), which keeps an unscoped foo=bar out of the file.
//
// Why none of this widens what a hostile header can reach:
//
//   - A scoped header can only land on the declared platform's domains (2).
//   - An unscoped header is admitted only under a tracked name (3), and what it
//     may then DO is scoped by verb — see WHAT AN ADMITTED HEADER MAY DO, BY
//     VERB below. In particular an unscoped DELETION still cannot reach
//     .google.com auth from a youtube.com reply: that was already true, and it
//     is now true and reachable rather than true and dead.
//   - Row-breaking characters cannot reach the file (1).
//   - Everything downstream is untouched: updateCookieFile's refusal to blank an
//     essential cookie, the seven-field rebuild, writeFileAtomic.
//
// WHAT AN ADMITTED HEADER MAY DO, BY VERB. The design first, because the rules
// below are only its enforcement and reading them in isolation gives the wrong
// shape: YOUTUBE COOKIES SHOULD ALLOW GOOGLE COOKIES AS WELL. YouTube and Google
// are ONE credential platform — .google.com and .youtube.com rows live in one
// jar, keyed by bare cookie name — so a youtube.com reply is ENTITLED to move
// Google rows, and does:
//
//   - a YouTube reply sending a cookie explicitly scoped `Domain=.google.com` is
//     admitted, and CREATES that Google row if the file holds none;
//   - an unscoped rotation from YouTube REFRESHES existing Google rows of the
//     same name, when it is the only candidate (rule 3's disambiguation, below).
//
// The one thing declined is MISATTRIBUTION. A host-only cookie from
// www.youtube.com carrying no Domain= is, per RFC 6265 §4.1.2.3, a youtube.com
// cookie — so a NEW row for it goes on .youtube.com and is not invented on
// .google.com. As observed, Google's own cookies carry an explicit Domain= (that
// is a fact about Google's servers, not one this code can enforce), so nothing
// real is caught by that: the retired branch that guessed .google.com from the
// cookie NAME was minting a DIFFERENT cookie under a real one's name.
//
// Three verbs, and their scopes are not one scope. Written out here because no
// single downstream rule states it — each verb is enforced somewhere else, so
// reading any one of them gives the wrong answer about the other two.
//
// REFRESH — an unscoped header may rewrite an existing same-name row anywhere
// inside the declared origin's PLATFORM. An unscoped `SID=fresh` from a
// youtube.com reply DOES rewrite an existing `.google.com SID` row. Two rules
// carry that between them: resolveRowUpdate's rule 2 (:2974) takes the rows the
// origin's own site covers, and rule 3 (:2981-2993) takes the rest of the
// platform through sameCookiePlatform (:3008-3014), whose Domain-less default is
// the DECLARED ORIGIN's platform. Rule 3 also DISAMBIGUATES rather than
// guessing: it fires only when exactly one non-deleting candidate qualifies
// (`refreshes == 1`, :2991), so two same-name updates inside the platform
// decline rather than let map order pick. The fan-out is deliberate (Arc 2 built
// it, Arc 8 preserved it): it is what stops one domain variant going stale while
// the other moves on, the drift finding #4 was about. Pinned by
// TestUnscopedRefreshCrossesDomainsOnlyInsideTheDeclaredPlatform.
//
// A SCOPED header refreshes across the platform on the same rule and is not
// narrowed here — rule 3 reads `sameCookiePlatform(k.Domain, …)`, so a
// `Domain=.google.com` rotation also repairs a stale `.youtube.com` twin when it
// is the only candidate. REFRESH is stated for the unscoped case because that is
// the case bullet 3 above admits and the one the deleted comment got wrong; the
// scoped/unscoped split appears under CREATE and, in narrower form, under DELETE
// (an unscoped deletion matches subdomain-wide through rule 2, a scoped one
// exact-host through rule 1).
//
// CREATE — the verb where scoped and unscoped part company, and that difference
// IS the misattribution rule:
//
//   - a SCOPED header creates on the domain it declared, whenever that domain is
//     inside the origin's platform. It reaches the insertion loop like any other
//     update that matched no row — that loop's own doc (:2743-2749) says
//     "everything the matching rules turned down arrives here" — keeps
//     `domain = key.Domain` (:2759) and passes the platform guard
//     (:2859-2864). So an admitted `SID=x; Domain=.google.com` from a
//     youtube.com reply DOES create a `.google.com` row against a file holding
//     none, and that is CORRECT: it is the rule above, not a leak past it.
//   - an UNSCOPED header creates only on the declared origin's own SITE. The
//     loop derives that row's domain from the origin and nothing else
//     (`domain = "." + string(origin)`, :2827). The branch that used to guess
//     it from the cookie NAME — writing `.google.com SID` out of an ordinary
//     youtube.com reply — is the one Arc 8 Task 2 removed. The real .google.com
//     SID is rotated by accounts.google.com with an explicit Domain=, which
//     takes the scoped path and never reaches that branch at all. Same name,
//     different cookie.
//   - narrower still, for one batch shape: an unscoped key is not inserted
//     beside a scoped NON-DELETING sibling of the same name (hasScopedSibling,
//     :2786), because the scoped header has already claimed a row and the
//     unscoped twin would override it by name in the jar. A scoped DELETION is
//     not such a sibling — delete-plus-insert is "replace" — see
//     hasScopedSibling's own doc.
//
// DELETE — an unscoped header may delete only inside the declared origin's own
// SITE, through rule 2 alone. Rule 1 needs a Domain=, rule 3 skips deletions
// (`!updates[k].Delete`, :2987) and the insertion loop skips them as well
// (:2751), so origin.covers is the only door a Domain-less deletion has. (A
// SCOPED deletion is rule 1's, and reaches only rows whose domain it exactly
// scope-matches — the scope the server actually named.)
//
// WHY UNSCOPED CREATE IS NARROWER THAN UNSCOPED REFRESH, given that both are
// "the same cookie": a rewrite repairs a row the FILE has already asserted
// belongs to this platform. That domain came from a browser export or an earlier
// scoped Set-Cookie, and the response is only supplying a fresher value for a
// scope something else established. A creation has no such prior assertion to
// lean on, so inventing `.google.com` out of an unscoped youtube.com header
// would be THIS WRITER asserting a scope nobody named — and a false one, per the
// misattribution rule above. A scoped header carries its own assertion, which is
// exactly why its CREATE is not narrowed. Deletion is narrow for the ordinary
// reason, that it is the unrecoverable verb.
//
// Two layers, and they are not redundant. THIS one decides what becomes an
// update at all, and it is the only code that ever sees the raw header — so
// row-breaking characters and the tracked-name rule belong here and nowhere else.
// updateCookieFile's insertion guard is the last check before a row is WRITTEN,
// and it covers the domain that loop DERIVES from the declared origin for an
// unscoped update — a domain that does not exist yet when this function returns,
// and which this function therefore cannot judge. Neither subsumes the other.
//
// This layer is also per-header and pure: it cannot see the other Set-Cookie
// headers in the same response. The one rule that needs that view — an unscoped
// key must not be INSERTED beside a scoped key of the same name — therefore
// lives in the insertion loop, which holds the whole batch.
//
// Scoped headers are admitted under ANY name; only unscoped ones are name-gated.
// That asymmetry is pre-existing and deliberately left alone here: narrowing it
// would mix a widening and a narrowing into one commit.
func admitSetCookie(sc string, origin cookieOrigin) (cookieUpdateKey, cookieUpdate, bool) {
	parts := strings.Split(sc, ";")
	if len(parts) == 0 {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}
	nameValue := strings.TrimSpace(parts[0])
	name, value, ok := strings.Cut(nameValue, "=")
	if !ok {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}
	// RFC 6265 §5.2 step 3: remove leading and trailing WS from the name-string
	// AND the value-string, separately. Trimming only the whole `name=value`
	// pair (the line above, kept because it also eats a stray line ending) left
	// `SAPISID = v` parsed as the name "SAPISID " and the value " v" — a name no
	// predicate in this package recognises and a value with a leading space.
	//
	// WS here is SP and HTAB exactly, per the grammar, which is why this is
	// strings.Trim and not strings.TrimSpace: TrimSpace would also eat a leading
	// CR or LF and quietly rescue a header that step 1 below should refuse.
	//
	// NOT de-quoted, and that is deliberate rather than unfinished. §5.2 takes
	// everything up to the first ";" as the name/value pair and never strips
	// DQUOTEs, so `Customer="WILE_E_COYOTE"` has the quotes as part of its value
	// and `chips="a;hoy"` really does truncate at the semicolon — every browser
	// behaves this way. CPython's SimpleCookie strips quotes because it
	// implements the older RFC 2109; matching it here would diverge from both
	// RFC 6265 and the browsers whose exports fill this file.
	name = strings.Trim(name, " \t")
	value = strings.Trim(value, " \t")
	if name == "" {
		return cookieUpdateKey{}, cookieUpdate{}, false
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
		switch {
		case maxAge <= 0:
			deleteCookie = true
		case maxAge > maxCookieLifetime:
			// Clamped, not refused. A too-long Max-Age is a statement about
			// lifetime, not a malformed header, and refusing it would throw away
			// a perfectly good rotated VALUE over an attribute every browser
			// silently caps anyway. See maxCookieLifetime for why the clamp also
			// closes the int64 overflow.
			expiry = now + maxCookieLifetime
		default:
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
	// scope per RFC 6265). A bare "Domain=" carries no value at all and
	// leaves domainAttr empty, which RFC 6265 §5.2.3 also says to treat as
	// host-only — so it takes the unscoped branch, not a "." one.
	if domainAttr != "" && !strings.HasPrefix(domainAttr, ".") {
		domainAttr = "." + domainAttr
	}

	// Step 1. Before either admission branch, and on all three fields that become
	// their own tab-separated column in the row. Normalization above can only
	// lowercase and prepend a dot, so checking after it sees the same characters
	// checking before it would; the WSP trim above has already removed the tabs
	// that RFC 6265 §5.2 says are not part of the name or the value at all, so
	// what reaches here is an INTERIOR one.
	if hasRowBreakingChar(name) || hasRowBreakingChar(value) || hasRowBreakingChar(domainAttr) {
		return cookieUpdateKey{}, cookieUpdate{}, false
	}

	if domainAttr != "" {
		// Step 2. cookiePlatformOf returns "" for a domain on no known platform
		// (evil.tld, accounts.google.com.evil.tld, a bare "."), and an undeclared
		// origin has no platform either — so the emptiness test is load-bearing:
		// without it, bare equality would read "" == "" as a match.
		p := cookiePlatformOf(domainAttr)
		if p == "" || p != origin.platform() {
			return cookieUpdateKey{}, cookieUpdate{}, false
		}
	} else if !trackedCookieName(name, origin) {
		// Step 3.
		return cookieUpdateKey{}, cookieUpdate{}, false
	}

	return cookieUpdateKey{Name: name, Domain: domainAttr}, cookieUpdate{
		Value:    value,
		Expiry:   expiry,
		HTTPOnly: httpOnly,
		Delete:   deleteCookie,
	}, true
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
//     attribute when the server provided one (finding #40); when it did not,
//     from the DECLARED ORIGIN's own site. It used to be guessed from the
//     cookie name — see the insertion loop for why that was wrong and why
//     nobody noticed for so long.
//   - Deletions remove the row. See resolveRowUpdate for why a value refresh
//     may cross domain variants while a deletion may not.
//
// origin is the SITE whose response produced these updates, and the caller has
// to state it because three decisions here need it and none can recover it from
// the map: a Set-Cookie with no Domain= is host-scoped to the response that
// carried it, and that response is not in `updates`. resolveRowUpdate's rule 2
// and sameCookiePlatform's Domain-less default both used to assume youtube.com,
// which was a true statement about the single call site rather than about those
// functions — correct today, and silently wrong in the DESTROY-SCOPE direction
// the day a second caller appears (the open one being a re-auth ingest response
// from accounts.google.com, whose unscoped deletions would have reached
// .youtube.com rows).
//
// The third is the INSERTION loop, and it is the one that is easy to miss:
// declining to match a row is not declining to write. Every update the two
// matching rules turn down lands in that loop, which derives a domain from the
// cookie name alone — so without an origin check there, a caller whose updates
// were refused everywhere still appended new rows, under a domain nobody
// declared. The loop now refuses any row outside the declared platform, and
// refuses everything when no origin was declared.
//
// This parameter is ENFORCEMENT of the existing rule, not a change to it: with
// originYouTube every case behaves exactly as before, insertion included (a
// youtube.com response's cookies land on youtube.com and google.com, which are
// one platform). The grow-broadly/destroy-narrowly asymmetry is likewise
// unchanged and deliberate — name-loose updates re-sync stale twins on purpose,
// domain-strict deletions keep .google.com auth out of reach of an unscoped
// YouTube deletion.
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
				if key, cu, ok := resolveRowUpdate(updates, byName[cookieName], rowDomain, origin); ok {
					handled[key] = true
					// essentialYouTubeCookies is a set of NAMES, and several of
					// them (PREF, CONSENT, YSC, LOGIN_INFO, the rotating
					// SIDTS/SIDCC pair) are not YouTube-exclusive strings — just
					// names YouTube happens to use. So a row only carries an
					// essential YouTube cookie when its DOMAIN says so too, the
					// same guard Arc 5 put on jar.Load and isEssentialCookie.
					//
					// Both readers below select log SEVERITY only and gate no
					// mutation, which is why the unguarded form was not wrong on
					// the wire. It is guarded because it was the last surviving
					// copy of a shape this plan has now removed three times, and
					// the next reader would reasonably lift it somewhere it does
					// decide something.
					//
					// Computed inside this branch: nearly every row in a real
					// cookies.txt matches no pending update, and neither reader
					// below is reachable for those.
					rowHasEssential := essentialYouTubeCookies[cookieName] &&
						(isYouTubeDomain(rowDomain) || isGoogleDomain(rowDomain))
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
	// Nor, per the origin check below, may a row outside the platform the
	// caller declared: everything the matching rules turned down arrives here,
	// so this loop is where "declined" has to become "not written".
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
		// An unscoped update may not be INSERTED when the same response also
		// carried a scoped one of the same name that it means to keep. The scoped
		// header has already claimed whichever row it matches; inserting the
		// unscoped twin appends a row that then OVERRIDES it in the jar.
		//
		// The override is by NAME, not by domain, and stating that correctly
		// matters because the two rows are usually on different domains. Since
		// fix round 1 the twin lands on the declared origin's own site, so an
		// originYouTube response carrying `SID=v1` and `SID=v2; Domain=.google.com`
		// against a file holding the .google.com row leaves `.google.com` and
		// `.youtube.com` SID rows side by side — not two rows on one domain, which
		// is only what the originGoogle case would produce. It is still a
		// downgrade: CookieJar.Load puts youtube.com and google.com rows in ONE
		// map keyed by bare cookie name, so the last row read wins and the
		// unscoped value silently defeats the value the server was more specific
		// about.
		//
		// Narrow on purpose, and all three halves of that matter. It fires only
		// for an unscoped key; only when a scoped SIBLING of the same name is in
		// this batch; and not when that sibling is a DELETION (see
		// hasScopedSibling — a delete plus an insert of one name is "replace",
		// and suppressing the insert loses the replacement). The obvious wider
		// rule — "once any key of this name matched a row, treat every key of
		// that name as handled" — destroys the legitimate case: a response
		// rotating SID on both .google.com and .youtube.com against a file
		// holding only the .google.com row must still insert the .youtube.com one.
		if domain == "" && hasScopedSibling(updates, byName[name]) {
			rs.logger.Debug("cookie update: not inserting an unscoped cookie beside a scoped one of the same name",
				"name", name, "origin", string(origin))
			continue
		}
		if domain == "" {
			// The Set-Cookie carried no Domain=, so RFC 6265 §4.1.2.3 host-scopes
			// it to the response that carried it — and the only thing here that
			// knows which response that was is the declared origin. So the domain
			// is the origin's own site, and nothing else contributes to it.
			//
			// It used to be guessed from the cookie NAME: .youtube.com, or
			// .google.com when isGoogleOnlyAuthName said so. That branch was DEAD
			// CODE for as long as it existed — processYouTubeSetCookies opened
			// with a substring pre-filter that dropped every Domain-less header
			// before it could become an unscoped key — and going live exposed it
			// as the exact inverse of resolveRowUpdate's rule 2. An unscoped SID
			// from an ordinary youtube.com reply was written as `.google.com SID`
			// and then sent to accounts.google.com on the next request. It is a
			// DIFFERENT COOKIE: the real .google.com SID is rotated by
			// accounts.google.com with an explicit Domain=, which takes the scoped
			// path and never reaches this branch at all. isGoogleOnlyAuthName is
			// retired as a domain-inventor and survives only as half of
			// trackedCookieName's admission set.
			//
			// The leading-dot registrable domain (".youtube.com") rather than the
			// host-only form the response literally scopes it to
			// ("www.youtube.com") because that is the shape the rest of the file
			// speaks: browser exports, mergeCookieFiles and CookieJar.Load all key
			// on the registrable domain with include-subdomains set, and
			// resolveRowUpdate's rule 2 matches through origin.covers(), which
			// accepts exactly this. A host-only row would be a shape no other
			// writer in this package produces, and the next refresh would not
			// match it.
			//
			// This makes the cross-platform hazard structural rather than caught:
			// an unscoped insertion now lands inside the declaring origin by
			// construction, so it CANNOT reach another platform's rows. The check
			// below still earns its place for the two cases construction does not
			// cover — an explicit cross-platform Domain=, and an undeclared origin
			// (which yields "." here, a domain on no platform at all).
			domain = "." + string(origin)
		}
		// An insertion may not leave the declared origin's platform, and an
		// undeclared origin may not insert at all.
		//
		// This is the half of the rule the matching rules cannot enforce, and
		// missing it inverted the whole point of declaring an origin. Declining
		// to MATCH is not declining to WRITE: when resolveRowUpdate turns down
		// every row, the update is not dropped, it arrives here. When this branch
		// guessed the domain from the cookie name, a Twitch (or undeclared)
		// caller's unscoped "SID" was refused by rules 2 and 3 and then appended a
		// brand-new .google.com SID row anyway, landing a foreign credential in
		// the Google jar — WIDER than the hardcoded behaviour the origin parameter
		// replaced, not narrower.
		//
		// The unscoped half of that is now impossible by construction (the domain
		// IS the origin's site), so what this check still decides is the explicit
		// -Domain= case and the undeclared origin. Kept whole rather than narrowed
		// to those two: it is one sentence about the row about to be written, and
		// splitting it would make the guarantee depend on which branch produced
		// the domain.
		//
		// Checked against the domain actually about to be written, not only the
		// fallback, so a key carrying an explicit cross-platform Domain= is
		// refused on the same rule. Nothing legitimate is lost: a Google session
		// covers youtube.com and google.com alike, so the two never fail this
		// against each other, and admitSetCookie already drops a Domain= that
		// is neither.
		//
		// A domain with no recognised platform is refused by the same test — an
		// undeclared origin and an unplaceable domain both yield "", and equality
		// alone would read that as a match.
		insertPlatform := cookiePlatformOf(domain)
		if insertPlatform == "" || insertPlatform != origin.platform() {
			rs.logger.Debug("cookie update: refusing to insert a row outside the declaring origin's platform",
				"name", name, "domain", domain, "origin", string(origin))
			continue
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

// hasScopedSibling reports whether any of these same-name update keys carries an
// explicit Domain= AND is a value it intends to keep. The caller has the keys for
// one name already grouped (the byName index), so this is a walk over one or two
// entries, not a scan.
//
// A scoped DELETION is not a sibling for this purpose, and that exclusion is the
// whole reason this takes the updates map rather than the keys alone. `SID=;
// Domain=.google.com; Max-Age=0` beside an unscoped `SID=fresh` is one response
// saying REPLACE — retire the cookie on google.com, set it host-scoped here.
// Counting the deletion as a claim on the name made the guard eat the
// replacement: the delete removed the .google.com row, the unscoped insert was
// then suppressed as "beside a scoped one", and the fresh value reached nothing.
// A deletion claims no row that an insertion could duplicate, because after it
// runs there is no row.
func hasScopedSibling(updates map[cookieUpdateKey]cookieUpdate, candidates []cookieUpdateKey) bool {
	for _, k := range candidates {
		if k.Domain != "" && !updates[k].Delete {
			return true
		}
	}
	return false
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
//
// It is a statement about NAMES, and it must not be used to decide a DOMAIN.
// updateCookieFile's insertion loop used to do exactly that for a Set-Cookie
// with no Domain=, which wrote a host-only youtube.com SID onto .google.com —
// a different cookie, sent to a different host. The sole caller now is
// trackedCookieName, which asks only whether the jar tracks the name.
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
	// CheckTwitchAuth through checkPlatformAuth, which discards the value. Since
	// Arc 8 Task 12a it DOES reach two per-request screens as
	// AuthStatus.TwitchError: the REST payload's `twitchError` and the TUI's R C
	// result line. See errGuideLoginMarkerUnreadable's doc comment for the full
	// accounting of where these strings go and why the rule above governs it.
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized:
		return false, nil
	default:
		return false, fmt.Errorf("twitch auth check: unexpected status %d", resp.StatusCode)
	}
}

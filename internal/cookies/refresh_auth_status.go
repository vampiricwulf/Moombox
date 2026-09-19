package cookies

// refresh_auth_status.go — the auth-status wire type, its verdict
// projection, and the Twitch out-of-band credential-loss vocabulary
// (twitchAuthLossMessage, twitchAuthMark) and change detection.

import "errors"

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

	// CookieFileError is the jar's last read failure, bounded and path-only
	// (CookieJar.LastLoadError). Empty when cookies.txt loaded, and empty when
	// there is no cookies.txt — an ABSENT file is never-configured, which both
	// dashboards already say correctly. This exists for the case they used to
	// get wrong: a file that is present on the volume and unreadable, which
	// rendered identically to "you have not set cookies up".
	//
	// PLATFORM-INDEPENDENT on purpose. One file holds both platforms' rows, so
	// both status payloads project it and either badge can name it.
	CookieFileError string `json:"cookieFileError,omitempty"`
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
//
// CookieFileError is compared for exactly that reason and is NOT an exception
// to the exclusion above. It is not a reason string that varies between two
// occurrences of one outcome: it is the badge state itself on both surfaces
// (tui.CookieStatusFileUnreadable, and the Web indicator's own arm), so a file
// becoming unreadable — or becoming readable again — is a transition that has
// to be pushed or the bar stays wrong until some unrelated flip fires.
func authStatusChanged(prev, next AuthStatus) bool {
	return next.YouTubeAuthenticated != prev.YouTubeAuthenticated ||
		next.TwitchAuthenticated != prev.TwitchAuthenticated ||
		next.HasYouTubeCookies != prev.HasYouTubeCookies ||
		next.HasTwitchCookies != prev.HasTwitchCookies ||
		next.YouTubeVerification != prev.YouTubeVerification ||
		next.TwitchVerification != prev.TwitchVerification ||
		next.CookieFileError != prev.CookieFileError
}

package cookies

// refresh_pass.go — the refresh pass: the public CheckYouTubeAuth /
// CheckTwitchAuth / NoteTwitchAuthLoss entry points, doRefresh/refresh,
// and the recovery/credential-change predicates the pass evaluates.

import (
	"context"
	"time"
)

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
		cookieFileErr              string
		ytIdentity, prevYTIdentity string
		twIdentity, prevTWIdentity string
		twEffective                bool
		reopenYT, reopenTW         bool
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

		// Sampled here with the rest of the snapshot, and AFTER doRefresh's
		// jar.Reload() at the top, so it describes the read that just happened.
		// It is the state the two predicates above CANNOT express: a cookies.txt
		// that is present and could not be read loads nothing, so both of them
		// answer false and every surface reported a mounted file as one that was
		// never set up.
		cookieFileErr = rs.jar.LastLoadError()

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
			// Platform-independent: one cookies.txt holds both platforms' rows.
			CookieFileError: cookieFileErr,
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

		// A failure a PREVIOUS process announced and never closed
		// (SetUnrecoveredPlatforms) is consumed by the first check that finds
		// the platform working, conclusively — the same test the recovered
		// transition below applies — and by nothing else: a dead first check
		// leaves it for the transition that follows the repair.
		if rs.ytUnrecovered && ytAuth && ytErr == nil {
			reopenYT, rs.ytUnrecovered = true, false
		}
		if rs.twUnrecovered && twEffective && twErr == nil {
			reopenTW, rs.twUnrecovered = true, false
		}

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
	//
	// reopenYT/reopenTW are the same transition witnessed across a restart: the
	// previous process saw the platform fail and said so, and this one cannot
	// see the fall itself — its baseline is whatever SetExpectedPlatforms
	// seeded, and hasChecked is false on a first pass — so without them the
	// close for that announcement never came.
	if rs.OnAuthRecovered != nil {
		if ((hasChecked && !prevYT) || reopenYT) && ytAuth && ytErr == nil {
			rs.logger.Info("youtube auth recovered")
			rs.OnAuthRecovered("youtube")
		}
		if ((hasChecked && !prevTW) || reopenTW) && twEffective && twErr == nil {
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
			// nothing to report — while this line is the only evidence anyone
			// has that the signal itself works. Deduped through the same
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
	// gate — armed since 2026-09-03 — carries the same last step. What differs
	// is the jar condition: this probe SENDS the auth-token, so an install
	// without one is not "unreported", it is unprobeable — and asking anyway
	// would get an anonymous playback token by design and read as a dead
	// session.
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

package cookies

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	loginURL         = "https://accounts.google.com/ServiceLogin?service=youtube"
	refreshURL       = "https://www.youtube.com"
	twitchLoginURL   = "https://www.twitch.tv/login"
	twitchRefreshURL = "https://www.twitch.tv"
)

// Cookie service timeout budget. Pulled out of inline literals so a tuning
// pass is one place rather than scattered across autocookies*.go (audit
// reports/cookies.md #38).
const (
	// processTimeout is the budget for ONE browser launch — and it is now
	// spent for real. runWithTimeout waits for the launched BROWSER to finish
	// rather than for the launcher stub that spawned it, so a Firefox refresh
	// that used to return in ~200ms legitimately takes seconds and may take
	// the whole 30s.
	//
	// It is COUPLED to refreshOverallBudget below, which caps the same work
	// end to end. Worst case for a two-platform Firefox refresh:
	//   2 × (processTimeout + postKillReapGrace)                       = 70s
	//   + firefoxLaunchSpacing                                          =  5s
	//   + the cookie-DB read retries (4 × cookieDBReadRetryBackoff)     ≈  2.0s
	//   + authVerifyTimeout — TWO checkPlatformAuth calls per pass, the
	//     pre-write snapshot and the post-write verify, each costing
	//     ONE window because the platforms verify concurrently  2 × 12s = 24s
	//                                                                   ≈ 101s
	// against a 120s cap. The rollback arm makes a THIRD call, which is the
	// real worst case at ≈ 113s — still under. Four is not reachable: the
	// snapshot is taken once per pass and the post-verify and the rollback
	// re-verify are the two arms of one decision.
	//
	// Four retries, not five: the loop runs cookieDBReadRetries attempts but
	// sleeps only BEFORE a re-try (autocookies_firefox.go), so it costs
	// (cookieDBReadRetries-1) × cookieDBReadRetryBackoff.
	//
	// Every term above is a named constant, and
	// TestAuthVerifyBudgetsStayUnderTheirCaps re-sums this table and the
	// setupAbandonGrace one below from those constants — so an edit to any of
	// them that breaks a cap fails a test instead of rotting a comment.
	//
	// RAISING processTimeout WITHOUT RAISING refreshOverallBudget makes the
	// outer ctx cancel the second platform's launch mid-flight instead of
	// granting it the budget it was just given. Making the two platform checks
	// SEQUENTIAL again would put the two-call pass at ≈ 125s and the
	// three-call pass at ≈ 149s, both over the cap — see checkPlatformAuth.
	processTimeout = 30 * time.Second
	// authVerifyTimeout bounds ONE VERIFIER — one window per platform, not one
	// window for the pair. checkPlatformAuth builds a context.WithTimeout
	// around each check(...) call, so VerifyYouTubeAuth and VerifyTwitchAuth
	// each get the full value and neither platform's verdict depends on how
	// slow the other one was.
	//
	// It used to be one shared deadline, and a YouTube check that spent it
	// left Twitch reporting verifyUnknown about a credential nobody had asked
	// about; see checkPlatformAuth for what that cost downstream.
	//
	// A whole checkPlatformAuth call is still worth ONE of these, not two: the
	// two platforms are verified concurrently, so this is both the per-verifier
	// bound and the per-call bound, and it stays that way if a third platform
	// is ever added. That is the number the refresh sum above (once per call,
	// two or three calls per pass), the setupAbandonGrace columns below and
	// data-and-storage.md's cross-writer window sentence all carry.
	//
	// 12 s, NOT the 15 s it was, and the 3 s came off here rather than out of
	// any outer budget (ruling J5a). The split alone would have put the binding
	// Chromium setup column at 60.3 s against a 60 s setupAbandonGrace and
	// against both clients' own 60 s FinishSetup caps — three constants the
	// cookie remediation set deliberately — and would have put a refresh pass
	// over refreshOverallBudget outright. Paying for it here, plus the
	// concurrency (J5b), keeps every one of them where it was.
	//
	// The value spent is authVerifyWindow, a var below, so a test can prove
	// the windows are per platform and simultaneous without waiting 12 s.
	//
	// It is the TIGHTEST of the three bounds a real verify sits under, which
	// is why the worst cases below are reachable and the margins worth keeping
	// positive: cookiesHTTPClient's own timeout is 30 s and refresh.go's
	// authCheckTimeout — which CheckYouTubeAuth and CheckTwitchAuth wrap
	// around themselves — is 15 s, so at 12 s this window is what actually
	// stops a hung verifier. Raise it past 15 s and authCheckTimeout starts
	// binding instead, and this constant quietly stops meaning anything.
	authVerifyTimeout    = 12 * time.Second
	refreshOverallBudget = 2 * time.Minute // periodic refresh: ctx cap end-to-end (see processTimeout)
	// taskkillDrainDelay is the post-taskkill pause that lets Windows release
	// the process handle before the next cleanup step inspects state. Replaces
	// a bare 300ms literal in killSetupProcess (audit reports/cookies.md #45).
	taskkillDrainDelay = 300 * time.Millisecond
	// killProcessTreePollDelay is the inner-loop pause inside the
	// kill-tree's "wait for sentinel to clear" loop. 50ms is fast enough
	// that a typical Firefox/Chromium teardown completes within 1-2
	// iterations while not pinning a CPU core. Audit reports/cookies.md #45.
	killProcessTreePollDelay = 50 * time.Millisecond
	// setupAbandonGrace is how long a setup whose browser has already exited
	// is still held before the next StartSetup / RefreshCookies / GetStatus
	// reaps it.
	//
	// It exists for exactly one caller: a FinishSetup that is already in
	// flight. A finish routinely runs with its spawned process already gone —
	// Chromium's closes the browser itself, and every Firefox-family launcher
	// exits ~170ms after start (see setupBrowserGone) — so "the process
	// exited" is a normal mid-finish state, not evidence of abandonment.
	// Reaping on that alone would pull the slot out from under the call that is
	// about to succeed.
	//
	// 60s is not a guess, and the headroom is much thinner than it looks. Both
	// clients cap one FinishSetup at 60 seconds — the Web dialog's
	// AbortController (settings.js / setup.js) and the TUI's finishCtx in
	// cmd/moombox/tui_wiring.go — and FinishSetup re-stamps setupRetainedSince
	// when it takes the slot, so a finish always gets a full window from the
	// moment it started rather than whatever was left of one.
	//
	// Server-side worst case inside that window, summed rather than sampled:
	//
	//   Firefox   taskkillDrainDelay            0.3s
	//             readFirefoxCookies retries   ~2.0s   (4 × cookieDBReadRetryBackoff)
	//             both authVerifyTimeouts      12.0s
	//                                        ≈ 14.3s
	//   Chromium  cdpExtractTimeout            30.0s
	//             taskkillDrainDelay            0.3s
	//             both authVerifyTimeouts      12.0s
	//                                        ≈ 42.3s
	//
	// BOTH platforms' windows are in those columns and TOGETHER they cost one,
	// not two: checkPlatformAuth verifies the two concurrently, so a call is
	// worth 12.0s however many platforms are configured. FinishSetup makes
	// exactly one such call, so unlike the refresh sum above this column
	// multiplies nothing. CHROMIUM IS THE BINDING COLUMN, so the real margin
	// is ~17.7s — not the ~45s a "one column plus the read retries" reading
	// suggests.
	//
	// That margin was paid for, not found: the per-platform split doubled this
	// line, running the checks concurrently halved it back, and
	// authVerifyTimeout was cut 15s → 12s alongside (rulings J5a, J5b). Take
	// the concurrency away and this column is 54.3s; take the cut away as well
	// and it is 60.3s — OVER both this window and the clients' own 60s cap.
	// Both of those are load-bearing, so neither is a free edit: serialising
	// checkPlatformAuth or raising authVerifyTimeout re-opens the overrun
	// silently, and either one has to move this constant and the two client
	// caps with it.
	//
	// The Firefox column deliberately does NOT price closeFirefoxGracefully's
	// 8.0s poll or the 0.5s cdpCloseFlushDelay behind it. That branch is
	// unreachable on the path it was written for: the launcher has already
	// exited by the time a finish runs, so the `exited` check at the top of
	// closeFirefoxGracefully short-circuits BEFORE the taskkill and the
	// function returns after one taskkillDrainDelay. Those 8.5s only come back
	// if a Firefox-family binary stops handing off, and even then the column
	// stays under Chromium's.
	//
	// DO NOT LOWER THIS TO 30s: a slow Chromium finish would be reaped
	// mid-flight, which is precisely what the re-stamp above exists to prevent.
	// Raising any of the constants above eats the margin directly.
	//
	// It is also the longest an ABANDONED setup can wedge acquisition, which is
	// the bug this whole change exists to fix, so it must not grow without a
	// reason on the finish side.
	setupAbandonGrace = 60 * time.Second
)

// launchWindowKillBudget caps how long a kill will wait for a launcher to
// publish the process it is about to start. Both slots have the same
// window — the claim is taken (setupClaimed / the refreshCmd sentinel)
// before the real process exists — so both killers poll for it, and both
// give up rather than let Stop() block on a launcher that errored before
// it ever assigned. See killSetupProcess and killRefreshProcess.
//
// A var rather than a const SOLELY so tests can shorten it (owner decision
// O-Q, 2026-09-17): seven of them wait this out in full, ~13.5 s of the ~41 s
// that makes this package the whole suite's wall-time floor. Nothing in
// production writes it, and the value is unchanged. Because it is package
// state that tests mutate, this package must stay free of t.Parallel — see
// withLaunchWindowKillBudget.
var launchWindowKillBudget = 2 * time.Second

// authVerifyWindow is the value checkPlatformAuth actually spends, and it is a
// var for exactly one reason: a test cannot wait out a 12 s window to prove the
// two platforms are budgeted apart. Production never assigns it — the constant
// above is the number, this is only the seam a test shortens. Same shape as
// applyUserOnlyDACL and loadCookieJar elsewhere in this package.
var authVerifyWindow = authVerifyTimeout

// platformRefreshURLs maps platform names to their refresh URLs.
var platformRefreshURLs = map[string]string{
	"youtube": refreshURL,
	"twitch":  twitchRefreshURL,
}

// The two values of cookies.acquisition, which decide how a REFRESH pass
// acquires credentials. Exported because cmd/moombox names them, and because a
// literal repeated across four packages is how an enum drifts.
//
// Two, by ruling (2026-09-02). The audit proposed a third, "browser", meaning
// "launch when a browser resolves" — which is what "auto" already means, so at
// every site the decision was profile-vs-rest and the two values could not be
// told apart. A value that behaves like another is a trap; it was dropped, and
// a later semantics can add it additively.
const (
	AcquisitionAuto    = "auto"
	AcquisitionProfile = "profile"
)

// AutoCookieReloginRequired tracks which platforms need manual re-login,
// keyed by lowercase platform name ("youtube", "twitch", and any future
// addition). The JSON wire shape stays compatible with the previous
// struct form because the consumer reads `obj["youtube"]` / `obj["twitch"]`
// — adding a third platform now needs zero schema edits. Audit
// reports/cookies.md #44.
//
// Always initialised with both supported platforms by NewAutoCookieService
// so the JSON output is never an empty `{}` for the existing consumers.
type AutoCookieReloginRequired map[string]bool

// AutoCookieService manages automatic browser-based cookie extraction.
type AutoCookieService struct {
	mu            sync.Mutex
	profileDir    string
	cookiePath    string
	jar           *CookieJar
	setupProcess  *os.Process
	setupClaimed  bool        // StartSetup slot claim — held from the gate check until the browser process is registered (or the attempt fails)
	setupJob      *processJob // Windows: a Job Object. Linux: the browser's process group. nil elsewhere
	refreshCmd    *exec.Cmd   // tracks in-flight headless refresh browser
	setupBrowser  *DetectedBrowser
	browserExited bool
	// setupRetainedSince is the last moment the setup in the slot was known to
	// be in use — the timestamp setupRetainedLocked measures setupAbandonGrace
	// from. THREE writers, all moving it FORWARD only:
	//
	//   - the wait goroutines, at the moment they observe the spawned process
	//     exit (guarded by `s.setupProcess == proc`, so a stale wait from an
	//     earlier attempt cannot stamp the current one);
	//   - FinishSetup, when it takes the slot, so a finish that starts near the
	//     end of a window is not reaped part-way through the read it is doing;
	//     and
	//   - reapAbandonedSetupLocked, every time it finds the setup still alive,
	//     because a browser that outlives its launcher would otherwise burn its
	//     whole window while running. See the re-arm there.
	//
	// Keep this list honest when a fourth appears. It said "two writers" for
	// one commit after the third landed, which is the same doc drift F-3 was
	// raised about.
	//
	// Meaningless unless browserExited is set. Cleared by cleanupLocked with
	// the rest of the per-attempt state, and again by StartSetup alongside its
	// browserExited reset. A stale zero value reads as "long expired", which is
	// the safe direction: it reaps rather than retains.
	setupRetainedSince time.Time
	cdpPort            int
	lastRefresh        *time.Time

	// lastError is the last thing a cookie pass concluded that the OPERATOR has
	// to act on. It is published as AutoCookieStatus.LastError and is meant to
	// sit beside the cookie status in both dashboards, so it is read as "your
	// recordings will fail" — which is what the write policy below exists to
	// keep true.
	//
	// THE POLICY, in one rule: a write is allowed only where THIS pass
	// established the thing it is asserting. Setting asserts a problem;
	// CLEARING asserts that whatever was recorded is not wrong any more, and
	// that is the half that keeps getting written by paths with no basis for it.
	//
	// The writers, audited (Arc 8 Task 12a). Nothing else may write it:
	//
	//   - setError — the single SET, and the only place a message enters this
	//     field. Callers: FinishSetup's empty-profile, read-failure, merge-abort,
	//     mkdir, write and jar-load exits; the refresh's import failure, merge
	//     abort, mkdir, write, jar-load, credential-loss and verification-failure
	//     exits; and both Chromium launchers' ErrProfileInUse refusals — the
	//     refresh's declined pass and the setup's refused start. Each of those is
	//     a conclusion the pass reached. (The refresh's mkdir, write and jar-load
	//     exits were the last three silent ones; they are the sweep's T1-8 and
	//     are pinned by autocookies_refresh_lasterror_test.go.)
	//
	//     THE LAST THREE OF FINISHSETUP'S WERE MISSING until Arc 8 Task 12a fix
	//     round 1, and the shape of the miss is worth keeping written down
	//     because it is what the rule below is for: the mkdir, writeFileAtomic
	//     and jar.Load exits called cleanup() and returned an error with no set
	//     at all, so a setup that died on a permission or mount problem put one
	//     sentence in a modal dialog and left this field — the thing an operator
	//     looks at AFTERWARDS, on both dashboards — blank. An audit that stops at
	//     "every failure funnels through setError" without walking the exits is
	//     how that survived; the first version of this comment said exactly that
	//     and was wrong.
	//
	//     So: EVERY exit that returns an error from a cookie pass sets. Adding an
	//     early return here without one puts the field back in that state, and
	//     nothing about the code will look wrong. The two exits that do NOT set
	//     are the guard clauses at the top of FinishSetupDetailed —
	//     ErrNoSetupInProgress and ErrSetupCancelled — and they are excluded on
	//     purpose, not overlooked: no pass has run when they fire, so there is
	//     no failure for the operator to see afterwards, and the caller gets the
	//     answer synchronously in the same dialog. "A pass" is the boundary; a
	//     guard that refuses to start one is not an exit from one.
	//   - the loss branch in RefreshCookiesDetailed's any-platform-verified arm
	//     — sets, via s.lastError directly, because a partial success still has
	//     to report the platform that was lost.
	//   - that same arm's `case renewed` — CLEARS, and only when the pass
	//     actually produced the credentials it verified. The `default` beside it
	//     deliberately does NOT clear: a pass whose browser did nothing has
	//     established that the credentials on disk work, not that the refresh
	//     mechanism does, and retracting an earlier "the browser profile
	//     contained no cookies" off it is how a twice-broken refresh presents a
	//     clean bill of health.
	//   - StartSetup, at the slot claim — CLEARS. Correct, and the one clear
	//     that is about intent rather than evidence: a new setup attempt is
	//     starting, the recorded message belongs to an attempt that is over, and
	//     leaving it would make the wizard open under a stale red line. The one
	//     message that is NOT over by then is a held profile, and the clear runs
	//     before the Chromium launcher has judged the lock — so that launcher's
	//     refusal sets the line again, or a refused sign-in would erase "in use
	//     by <host>" while every later refresh still skips for it.
	//   - the "nothing to verify" branch of the same switch as the loss branch —
	//     CLEARS. Reachability: no route to it has been found (it needs
	//     fetchedRows == 0 with neither platform having had a credential, and
	//     the only path that fetches nothing is the empty-browser-profile
	//     downgrade, which is gated on refreshPlatforms() having found one).
	//     Left in place with this note rather than deleted, because "I could not
	//     find a route" is not "there is none".
	//
	// cleanup() / cleanupLocked() MUST NOT clear it, and that is the rule this
	// audit exists for. cleanup runs on every setup exit path INCLUDING the
	// failed ones — FinishSetup calls setError and then cleanup on each of its
	// six failure exits — so clearing there would erase the message the failure
	// had just produced, microseconds after it was written, and the dialog would
	// report a failure the status page had no record of. See cleanupLocked: it
	// touches only state describing a browser that is gone.
	//
	// That also makes the set/cleanup ORDER a convention rather than a
	// requirement, which is worth knowing before rearranging one of those exits.
	//
	// Pinned by TestCleanupAfterAFailedSetupKeepsLastError and
	// TestFinishSetupRecordsTheFailureItReturns.
	lastError *string

	needsRelogin   AutoCookieReloginRequired
	targetPlatform string // "youtube" or "twitch"

	// suppliedThisRun is the set of platforms whose cookies the operator put
	// in the cookie file during this process: a browser login
	// FinishSetupDetailed ACCEPTED, or an import whose rows ImportCookies
	// installed. CarryCookieFileTo carries those platforms' rows and nothing
	// else — the file at the boot path can hold another install's session
	// (an earlier install, a second instance in the same folder), and a
	// first-run setup that ran no login must leave the cookie file it names
	// alone. Only ever grows; never cleared by cleanup().
	suppliedThisRun map[string]bool

	// The two lifecycle flags. Kept together and apart from the state above
	// because they are DECISIONS — an abort was asked for, the service was
	// shut down — rather than descriptions of a browser, and because cleanup()
	// clears neither of them.

	// cancelled is the per-setup abort flag. Raised by CancelSetup and by
	// Stop, read by StartSetup's mid-preparation check and by FinishSetup, and
	// cleared in exactly one place: StartSetup's slot claim. cleanup() does
	// not clear it — see cleanup for why doing so erased every complete cancel
	// microseconds after it was raised.
	cancelled bool

	// stopped latches the service's shutdown. Unlike cancelled it is scoped to
	// the SERVICE, not to one setup attempt: Stop means "this service is done"
	// for the remaining lifetime of the process, so no cleanup(), no claim and
	// no later StartSetup may lower it. StartSetup and RefreshCookiesDetailed
	// both refuse while it is set.
	stopped bool

	// Optional auth verification callbacks (set by caller for real API verification)
	VerifyYouTubeAuth func(ctx context.Context) (bool, error)
	VerifyTwitchAuth  func(ctx context.Context) (bool, error)

	// Optional callback to persist verified platforms to config (e.g. ["youtube", "twitch"]).
	// Called from FinishSetup after successful auth verification.
	PersistPlatforms func(youtubeVerified, twitchVerified bool)

	// HasActiveJobs reports whether the database has any Live or Downloading
	// jobs. Optional. Used to skip the periodic refresh's headless-Chrome
	// launch (1-5s, GPU init, memory) when nothing's actively pulling
	// authenticated content. nil leaves the legacy always-fire behaviour;
	// when set, periodic refresh skips ticks where the callback returns
	// false. Audit reports/cookies.md #23.
	HasActiveJobs func() bool

	// OnPassCompleted is called after an AUTOMATIC refresh pass that actually
	// ran, so whoever owns the in-process auth check can re-read the file this
	// pass may have rewritten.
	//
	// Injected rather than called directly for the reason FallbackLiveness and
	// HasActiveJobs are: this package must not reach into RefreshService's
	// lifecycle, and cmd/moombox holds both.
	//
	// It exists for EXACTLY TWO callers, the two credential writers with no
	// caller outside this package: StartPeriodicRefresh's tick and
	// StartProfileSeed's boot import. Every other credential-writing gesture —
	// the recovery, the worker's auth-failure refresh, R F, both setup wizards —
	// has a caller in cmd/moombox or internal/web/routes that runs the re-check
	// itself, so firing this from refreshCookiesDetailed instead would double
	// every one of them. The pair is pinned by
	// TestNotePassCompletedHasExactlyItsTwoWritingCallers.
	//
	// Called on whichever of those two goroutines fired it, with no lock held
	// and after that pass's own log lines, and it MAY run a full in-process
	// re-check (RefreshService.CheckNow — two validate round-trips, up to their
	// timeouts). That is deliberate rather than tolerated: the ticker coalesces
	// missed ticks and the seed runs once, so a slow hook costs cadence, never
	// correctness, and the alternative — spawning a goroutine here — would put
	// an unbounded number of re-checks behind a browser pass that is already
	// single-flighted. What the hook must NOT do is block forever.
	//
	// It must also not PANIC out: the periodic goroutine's recover sits outside
	// its for loop, so an escaping panic ends the 30-minute timer for the life
	// of the process rather than costing one tick. cmd/moombox wraps the body
	// it injects here in its own recover for exactly that reason.
	OnPassCompleted func()

	// DpapiFallback enables the Windows-only DPAPI cookie-extraction
	// path as a fallback when the CDP refresh launch fails. Off by
	// default — the fallback reads the user's REAL Chromium-family
	// browser profile, which is a privacy surface the user has to
	// opt into. When it answers true, RefreshCookies tries DPAPI as a
	// backstop once the primary CDP launch returns an error. DECISIONS #6.
	//
	// The injected form of cookies.dpapi_fallback, read LIVE like
	// AcquisitionMode and DpapiProfileDir: it was a bool mirrored once at
	// boot, which made the setting restart-required with nothing in either
	// UI saying so. nil reads as off (see dpapiFallbackOn).
	DpapiFallback func() bool

	// BrowserLaunchAllowed reports whether a REFRESH PASS may execute a
	// headless browser. It is the injected form of cookies.auto_enabled, which
	// this package deliberately cannot read: internal/cookies has no dependency
	// on internal/config and keeping it that way is why this is a predicate
	// rather than a bool copied in at construction — the flag is read live
	// everywhere else, so a snapshot would go stale the moment it is edited.
	//
	// Moombox keeps cookies alive with two independent mechanisms on two
	// independent timers: the in-process Go refresh (RefreshService, gated on
	// cookies.cookie_file alone) and, only when the operator turns this flag on,
	// a much slower headless-browser pass. The flag decides whether the second
	// mechanism runs on a timer at all — that decision lives in main.go and does
	// not come through here.
	//
	// What comes through here is the MANUAL trigger for that mechanism: the
	// TUI's R F chord and the Web dashboard's shift+click. Those are wired
	// unconditionally, because an operator who has hand-updated their browser
	// profile wants them to import from it immediately, and a disabled install
	// is exactly the install that does so. So a false answer does not refuse
	// the pass — it drops the browser, and every branch below takes the
	// browser-free import path that already exists for containers.
	//
	// Consulted only from browserLaunchBlocked, inside RefreshCookiesDetailed,
	// and deliberately NOT inside resolvedBrowser itself: StartSetup resolves a
	// browser too, and setup is acquisition — an explicit gesture in a visible
	// window, and the thing that turns this flag on. Gating it there would make
	// the setting unreachable on a fresh install, where it is false by
	// definition.
	//
	// The periodic goroutine is exempt (see browserGatePolicy): the flag IS
	// that timer, main.go answered it at boot, and re-asking it per tick would
	// let a runtime flip leave the timer running on a different mechanism.
	//
	// nil = allowed, so every existing caller and test keeps today's behaviour.
	BrowserLaunchAllowed func() bool

	// ConfiguredBrowserOverride, when set, returns the user's configured
	// browser_path and browser_type from the active config. Empty values
	// mean "no override; use auto-detect". Used by GetStatus to surface
	// the configured selection in the UI dropdown. nil leaves
	// ConfiguredBrowserPath/Type empty in the status response, which the
	// UI treats as "auto-detect selected".
	ConfiguredBrowserOverride func() (path, browserType string)

	// AcquisitionMode returns cookies.acquisition from the ACTIVE config. It is
	// the injected form of that setting for the same reason BrowserLaunchAllowed
	// is the injected form of cookies.auto_enabled: internal/cookies has no
	// dependency on internal/config, and keeping it that way is why this is a
	// predicate rather than a string copied in at construction — the operator
	// can change the mode while the process runs and the next R F has to see it.
	//
	// Consulted at refreshCookiesDetailed's launch-vs-import decision, and at
	// the three sites that used to INFER that decision from the host: the two
	// READ-ONLY profile sites (which stop consulting the launch guard when this
	// answers "profile" — audit G3), decideStartupSeed's browser short-circuit,
	// and the periodic tick's browser-free test. resolvedBrowser is untouched,
	// and so is StartSetup — the interactive login is acquisition, not a refresh.
	//
	// nil = "auto", so every existing caller and test keeps today's behaviour.
	AcquisitionMode func() string

	// DpapiProfileDir returns cookies.dpapi_profile_dir from the ACTIVE config,
	// or "" when the operator set none. Injected by cmd/moombox and read LIVE,
	// the same shape as AcquisitionMode above and for the same reason: this
	// package cannot import config, and a value snapshotted at construction
	// would make the setting restart-required with nothing in either UI saying
	// so. Nil is treated as "".
	//
	// Consulted twice, and both are one-shot reads rather than a cached copy:
	// LogDpapiProfileDirVerdict says at boot what the directory will do, and
	// refreshCookiesDetailed hands the value to dpapiExtractAsNetscape once per
	// pass, where it REPLACES the %LOCALAPPDATA% discovery walk.
	DpapiProfileDir func() string

	// profileDirErr captures any validation failure on the configured
	// profile directory (e.g. it points at a real browser's profile
	// tree). Computed once at construction so all subprocess-launching
	// entry points can fast-fail with the same message instead of each
	// re-running the check. Audit reports/cookies.md #26.
	//
	// The four subprocess sites read this field DIRECTLY, in every mode. The
	// two read-only sites go through readOnlyProfileDirErr instead, which
	// consults cookies.acquisition and rewords the refusal — a sentence about
	// refusing to launch is a claim a path that launches nothing cannot make.
	profileDirErr error

	// detectBrowser is the browser-detection seam used by resolvedBrowser.
	// Defaults to DetectBrowser; tests override it to exercise the
	// browserless (mounted-profile import) path on a host that does have a
	// browser installed. nil is treated as DetectBrowser so services built
	// via struct literal keep working.
	detectBrowser func() *DetectedBrowser

	// firefoxLaunchSpacing overrides the delay refreshFirefox waits between
	// consecutive Firefox launches — see the package-level firefoxLaunchSpacing
	// const (autocookies_firefox.go) for the production value and why it
	// exists. Defaults to that const in NewAutoCookieService; tests lower it
	// so a two-platform Firefox refresh doesn't burn 5 real seconds per run.
	// Zero/negative is treated as the const so services built via struct
	// literal keep launching at the production spacing. Same seam
	// convention as detectBrowser. Arc 8 7(d).
	firefoxLaunchSpacing time.Duration

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewAutoCookieService creates a new auto-cookie service.
func NewAutoCookieService(profileDir, cookiePath string, jar *CookieJar, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *AutoCookieService {
	// Resolve to absolute so browser subprocesses (Firefox -profile,
	// Chromium --user-data-dir) always find the profile regardless of CWD.
	if profileDir != "" {
		if abs, err := filepath.Abs(profileDir); err == nil {
			profileDir = abs
		}
	}
	// Validate ONCE at construction; subprocess-launching entry points
	// fast-fail with the cached error rather than each running the
	// scan independently. A malformed dir doesn't return an error from
	// the constructor — the empty case simply leaves every profile-
	// dependent entry point returning ErrProfileNotFound, and the
	// dangerous case hits the entry-point fast-fail — to preserve the
	// current "constructor never errors" contract.
	// Audit reports/cookies.md #26.
	//
	// The read-only sites reuse the same verdict through readOnlyProfileDirErr
	// rather than a second scan — one computed answer, two ways of consulting it.
	//
	// Computed here, SAID somewhere else. cmd/moombox builds the service and
	// only afterwards wires AcquisitionMode, so at this point the mode is not
	// knowable and any level chosen here is chosen blind — which is how an
	// install following the README's `cookies.acquisition = "profile"` recipe
	// logged a red "profile dir rejected ... refusing to launch" on every boot,
	// for the directory it is SUPPOSED to point at (Arc 12c arc-close F1). The
	// verdict and every reader of it are unchanged; only the sentence moved, to
	// LogProfileDirVerdict, which the wiring site calls once the mode is there.
	profileDirErr := validateBrowserProfileDirForLaunch(profileDir)
	s := &AutoCookieService{
		profileDir:           profileDir,
		cookiePath:           cookiePath,
		jar:                  jar,
		profileDirErr:        profileDirErr,
		detectBrowser:        DetectBrowser,
		firefoxLaunchSpacing: firefoxLaunchSpacing,
		// Always populated with both supported platforms so the JSON wire
		// shape stays {"youtube": false, "twitch": false} even for
		// fresh-install state. Audit reports/cookies.md #44.
		needsRelogin: AutoCookieReloginRequired{
			"youtube": false,
			"twitch":  false,
		},
		logger: logger,
	}
	// Restore LastRefresh from the on-disk sidecar so periodic refresh
	// doesn't fire immediately on every startup. Audit reports/
	// cookies.md #48. Missing sidecar (fresh install or pre-#48
	// version) is silent; load errors are logged but don't fail
	// construction.
	if meta, err := LoadMeta(cookiePath); err != nil {
		if logger != nil {
			logger.Warn("could not load cookies.meta.json", "err", err)
		}
	} else if meta != nil && !meta.LastRefresh.IsZero() {
		t := meta.LastRefresh
		s.lastRefresh = &t
	}
	// Reclaim writeFileAtomic temp files orphaned by a previous run that was
	// killed between os.CreateTemp and the rename — each one is a full copy
	// of cookies.txt. Service construction is the one place that always has
	// the real cookie path up front (see cookieTempFileSweepOnce's doc).
	// Empty cookiePath (no cookie file configured yet) has nothing to sweep.
	if cookiePath != "" {
		sweepCookieTempFilesOnce(&cookieTempFileSweepOnce, filepath.Dir(cookiePath), filepath.Base(cookiePath), cookieTempFileMaxAge)
	}
	return s
}

// refreshPlatforms returns the platforms that have cookies in the jar and need
// refreshing. Order is stable: YouTube first, then Twitch. It gates the whole
// browser refresh (RefreshCookiesDetailed), the Firefox launch loop and the
// Chromium navigation loop, so a platform missing from this list is never
// visited at all.
//
// The question is "does the jar already hold cookies worth re-fetching" — was
// this platform ever configured — not "is the credential set complete right
// now". Hence the permissive predicates.
//
// Read strictly, this list makes the remedy unreachable exactly when it is
// needed. A jar holding SAPISID with LOGIN_INFO cleared is what YouTube's
// rotation-invalidation leaves behind; doRefresh now fires OnRecoveryNeeded on
// it, recovery calls RefreshCookiesDetailed, the strict predicate returns an
// empty list, and the refresh declines — so the one platform the pass existed
// to fix gets no attempt at all. The operator is not even told: since
// runCookieRecovery's Unknown branch started splitting on RefreshResult.Ran, a
// declined pass is a log line and nothing more, which is right for a decline
// and useless here. Same for a Twitch session left holding only twilight-user.
//
// A platform with no auth cookie at all is still excluded, and that is not the
// same omission: there is no session to re-fetch, so a browser launched at it
// costs a process and can bring nothing back.
func (s *AutoCookieService) refreshPlatforms() []string {
	var platforms []string
	if s.jar.HasAnyYouTubeAuthCookie() {
		platforms = append(platforms, "youtube")
	}
	if s.jar.HasAnyTwitchAuthCookie() {
		platforms = append(platforms, "twitch")
	}
	return platforms
}

// browserGatePolicy says whether one refresh pass consults
// BrowserLaunchAllowed at all.
//
// It exists because the flag answers two different questions and only one of
// them is live. "May THIS gesture launch a browser?" is asked fresh, by the
// manual triggers, and must see the operator's current setting. "Does the
// periodic headless-browser timer exist?" was answered once, at boot, in
// main.go — cookies.auto_enabled is labelled restart-required in both UIs
// precisely because that is where it is read.
//
// The zero value is gateApplies, so anything that forgets to say gets the
// safe answer.
type browserGatePolicy int

const (
	// gateApplies is every caller acting on a live operator intention: the
	// TUI's R F, the dashboard's shift+click and its "refresh now", and the
	// automatic recovery attempt. Turning the flag off must reach them
	// immediately — that is what makes "switch it off, press R F, get a
	// browser-free import from the profile I just updated" work.
	gateApplies browserGatePolicy = iota

	// gateExempt is StartPeriodicRefresh's goroutine, and nothing else.
	//
	// The flag IS that timer. If a tick re-asked it, an operator who turned
	// the setting off without restarting would leave the timer running while
	// it silently switched to browser-free imports of the browser profile —
	// re-reading a profile nothing has changed, on a schedule, forever. That
	// is the behaviour the periodic loop is deliberately NOT given (see
	// StartPeriodicRefresh); arriving at it by accident is worse than
	// arriving at it on purpose. The timer keeps the mechanism it was started
	// with until the restart both settings pages already ask for.
	gateExempt
)

// refreshBrowser is resolvedBrowser as a REFRESH PASS sees it: the configured
// or detected browser, or nil when cookies.auto_enabled has switched headless
// browser runs off and this pass is one the flag speaks for.
//
// The wrapper exists so the gate applies to refreshes only. resolvedBrowser has
// two other callers — StartSetup, which is acquisition and must never be gated
// (see BrowserLaunchAllowed), and decideStartupSeed, which uses it to ask "is
// this a browserless host?", a question about the machine rather than about a
// setting — asked only when cookies.acquisition has not already answered it.
func (s *AutoCookieService) refreshBrowser(policy browserGatePolicy) *DetectedBrowser {
	if s.browserLaunchBlocked(policy) {
		return nil
	}
	return s.resolvedBrowser()
}

// FlagManualRelogin marks a platform as needing manual re-login.
//
// Exported because its callers are in cmd/moombox: handleRecoveryNeeded raises
// it at BOTH of its exits — the disabled branch, which is the container's
// documented configuration and never runs a pass at all, and the failed-recovery
// branch downstream of the enabled one. On either of those installs it is the
// only way the prompt is ever raised: RefreshCookiesDetailed's verify-failed
// arm, the other producer, needs a pass that got as far as checking, which
// wants either a browser or a mounted profile.
//
// It was deleted in Arc 8 Task 12a for having zero production callers, having
// been written for an ingest path that did not exist yet. It exists again
// because that path does (Arc 11), and it must not outlive that caller: an
// exported setter on a security-sensitive service with nothing calling it reads
// to the next reader as a wired feature.
//
// Process-local, like every other write to this map. Cleared per platform by
// FinishSetupDetailed, by RefreshCookiesDetailed's accepted arm and by
// ImportCookies — the gesture this flag is asking the operator to perform.
func (s *AutoCookieService) FlagManualRelogin(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A switch rather than a bare map write: the map's two keys are the wire
	// shape both UIs iterate, and an unrecognised platform must not widen it.
	switch platform {
	case "youtube":
		s.needsRelogin["youtube"] = true
	case "twitch":
		s.needsRelogin["twitch"] = true
	}
}

// ClearManualRelogin lowers a platform's re-login flag and reports whether it
// was raised. For the recovery the flag's own clearers (a browser refresh,
// setup, an import) never see: authentication coming back through the
// background re-check — a cookies.txt the operator replaced by hand, as the
// failure notification suggests, or a transient signed-out reading that
// healed. The flag stayed up, and both dashboards rank it above
// "Authenticated", so they showed "Re-login required" until a restart.
func (s *AutoCookieService) ClearManualRelogin(platform string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch platform {
	case "youtube", "twitch":
		was := s.needsRelogin[platform]
		s.needsRelogin[platform] = false
		return was
	}
	return false
}

// Stop stops the auto-cookie service, permanently for this service's
// lifetime. After it, StartSetup returns ErrServiceStopped and
// RefreshCookies/RefreshCookiesDetailed decline.
//
// The latch is a separate field from `cancelled` because the two answer
// different questions and have different lifetimes. `cancelled` aborts ONE
// setup attempt and is consumed by the next claim; `stopped` is a statement
// about the service, so nothing downstream may lower it — least of all the
// cleanup() this very function calls at the end.
func (s *AutoCookieService) Stop() {
	s.mu.Lock()
	s.stopped = true
	// Raise the per-setup flag too, for one narrow window: a FinishSetup that
	// reaches its gate after this write but before the cleanup() below nils
	// setupProcess is turned away as cancelled rather than allowed to drive a
	// browser that is being killed underneath it. Past that point the nil
	// setupProcess covers it on its own.
	s.cancelled = true
	s.mu.Unlock()
	s.killSetupProcess()
	s.killRefreshProcess()
	s.cleanup()
}

// dpapiFallbackOn reports cookies.dpapi_fallback through the DpapiFallback
// callback; an unwired service has the fallback off.
func (s *AutoCookieService) dpapiFallbackOn() bool {
	return s.DpapiFallback != nil && s.DpapiFallback()
}

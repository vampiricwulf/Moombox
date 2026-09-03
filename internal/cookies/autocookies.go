package cookies

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
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
	//   2 × (processTimeout + the 5s post-kill reap in runWithTimeout) = 70s
	//   + firefoxLaunchSpacing                                          =  5s
	//   + the cookie-DB read retries (5 × 500ms)                        ≈  2.5s
	//   + authVerifyTimeout — ONE window covering BOTH platforms, not
	//     one each; see checkPlatformAuth                               = 15s
	//                                                                   ≈ 92s
	// against a 120s cap. RAISING processTimeout WITHOUT RAISING
	// refreshOverallBudget makes the outer ctx cancel the second platform's
	// launch mid-flight instead of granting it the budget it was just given.
	processTimeout       = 30 * time.Second
	authVerifyTimeout    = 15 * time.Second // single VerifyYouTubeAuth / VerifyTwitchAuth call
	refreshOverallBudget = 2 * time.Minute  // periodic refresh: ctx cap end-to-end (see processTimeout)
	// taskkillDrainDelay is the post-taskkill pause that lets Windows release
	// the process handle before the next cleanup step inspects state. Replaces
	// a bare 300ms literal in killSetupProcess (audit reports/cookies.md #45).
	taskkillDrainDelay = 300 * time.Millisecond
	// killProcessTreePollDelay is the inner-loop pause inside the
	// kill-tree's "wait for sentinel to clear" loop. 50ms is fast enough
	// that a typical Firefox/Chromium teardown completes within 1-2
	// iterations while not pinning a CPU core. Audit reports/cookies.md #45.
	killProcessTreePollDelay = 50 * time.Millisecond
	// launchWindowKillBudget caps how long a kill will wait for a launcher to
	// publish the process it is about to start. Both slots have the same
	// window — the claim is taken (setupClaimed / the refreshCmd sentinel)
	// before the real process exists — so both killers poll for it, and both
	// give up rather than let Stop() block on a launcher that errored before
	// it ever assigned. See killSetupProcess and killRefreshProcess.
	launchWindowKillBudget = 2 * time.Second
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
	//             readFirefoxCookies retries   ~2.0s   (5 × 500ms)
	//             authVerifyTimeout            15.0s
	//                                        ≈ 17.3s
	//   Chromium  cdpExtractTimeout            30.0s
	//             taskkillDrainDelay            0.3s
	//             authVerifyTimeout            15.0s
	//                                        ≈ 45.3s
	//
	// plus merge / write / jar-load I/O in both columns. CHROMIUM IS THE
	// BINDING COLUMN, so the real margin is ~14.7s — not the ~42s an
	// "authVerifyTimeout plus the read retries" reading suggests.
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

// platformRefreshURLs maps platform names to their refresh URLs.
var platformRefreshURLs = map[string]string{
	"youtube": refreshURL,
	"twitch":  twitchRefreshURL,
}

// dangerousProfilePathSubstrings flags absolute profile directories that
// belong to a real installed browser. Allowing the auto-cookie service
// to launch headless against one of these would let a malicious config
// (or, in the future, a compromised /api/config write) launch Chrome
// against the user's actual logged-in profile and exfiltrate session
// cookies via the cookies.txt export. Patterns are matched
// case-insensitively against the path's lowercased absolute form;
// backslashes on Windows are preserved (filepath.Abs already
// canonicalises). Audit reports/cookies.md #26.
var dangerousProfilePathSubstrings = []string{
	`\google\chrome\user data`,
	`\google\chrome beta\user data`,
	`\google\chrome dev\user data`,
	`\google\chrome canary\user data`,
	`\microsoft\edge\user data`,
	`\microsoft\edge beta\user data`,
	`\microsoft\edge dev\user data`,
	`\microsoft\edge canary\user data`,
	`\bravesoftware\brave-browser\user data`,
	`\chromium\user data`,
	`\vivaldi\user data`,
	`\opera software\opera stable`,
	`\opera software\opera gx stable`,
	`\mozilla\firefox\profiles`,
	`\mozilla\firefox developer edition\profiles`,
	`\waterfox\profiles`,
	`\thunderbird\profiles`,
	`\librewolf\profiles`,
}

// validateBrowserProfileDir refuses configured profile directories that
// sit inside any user-installed browser's real profile tree. Empty
// input is allowed — it just leaves the service inert, since every entry
// point that needs a profile dir returns ErrProfileNotFound without one.
// Otherwise the path is resolved to absolute, lowercased, and checked
// against the dangerous-substring list above. Audit reports/cookies.md #26.
func validateBrowserProfileDir(profileDir string) error {
	if profileDir == "" {
		return nil
	}
	abs, err := filepath.Abs(profileDir)
	if err != nil {
		return fmt.Errorf("resolve profile dir: %w", err)
	}
	lower := strings.ToLower(abs)
	for _, pat := range dangerousProfilePathSubstrings {
		if strings.Contains(lower, pat) {
			return fmt.Errorf("profile dir %q points at a known browser profile path; refusing to launch a headless session against it (audit cookies.md #26)", abs)
		}
	}
	return nil
}

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

// AutoCookieStatus holds the current status of the auto-cookie service.
//
// There is deliberately no `configured` flag. One existed, computed as
// `profileDir != ""`, and it could not be false: cmd/moombox seeds the profile
// dir with a "./browser-profile" default before the service is constructed, so
// every install reported true from the first run. Nothing read it — not the
// dashboard, not the settings dialog, not the TUI — and a value that is always
// true and read by nobody is worse than an absent one, because the next reader
// believes it. Whether auto-cookies were ever set up is answered honestly by
// lastRefresh and needsManualRelogin, which describe what actually happened.
type AutoCookieStatus struct {
	SetupInProgress       bool                      `json:"setupInProgress"`
	Browser               *DetectedBrowser          `json:"browser"`
	AvailableBrowsers     []DetectedBrowser         `json:"availableBrowsers"`
	ConfiguredBrowserPath string                    `json:"configuredBrowserPath,omitempty"`
	ConfiguredBrowserType string                    `json:"configuredBrowserType,omitempty"`
	LastRefresh           *string                   `json:"lastRefresh"`
	LastError             *string                   `json:"lastError"`
	NeedsManualRelogin    AutoCookieReloginRequired `json:"needsManualRelogin"`
}

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
	//     abort, credential-loss and verification-failure exits. Each of those is
	//     a conclusion the pass reached.
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
	//     leaving it would make the wizard open under a stale red line.
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
	// opt into. When true, RefreshCookies tries DPAPI as a backstop
	// once the primary CDP launch returns an error. DECISIONS #6.
	DpapiFallback bool

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

	// profileDirErr captures any validation failure on the configured
	// profile directory (e.g. it points at a real browser's profile
	// tree). Computed once at construction so all subprocess-launching
	// entry points can fast-fail with the same message instead of each
	// re-running the check. Audit reports/cookies.md #26.
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
	profileDirErr := validateBrowserProfileDir(profileDir)
	if profileDirErr != nil && logger != nil {
		logger.Error("auto-cookie profile dir rejected at construction", "err", profileDirErr)
	}
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

// --- setup slot lifecycle ---
//
// One liveness probe, three predicates and one reaper — the predicates and the
// reaper all requiring s.mu — which together answer "is the setup slot still in
// use?" without the answer being "yes, forever".
//
// It used to be "forever". The wait goroutines set browserExited when the user
// closed the browser or walked away, and NOTHING ever cleared setupProcess, so
// SetupInProgress stayed true and StartSetup, RefreshCookies and GetStatus all
// kept refusing until the process restarted. One abandoned wizard wedged every
// form of cookie acquisition for the lifetime of the run.
//
// The correction that shapes all of it: browserExited is a statement about the
// PROCESS MOOMBOX SPAWNED, not about the browser. Where the two differ — every
// Firefox-family launch, and any Chromium behind a shim — only the Job Object
// knows, and where there is no Job Object nothing knows. See setupBrowserGone.

// setupBrowserGone reports whether every process the setup's Job Object was
// tracking is gone, and — separately — whether anything could actually say.
//
// PROCESS EXIT IS A HINT; AN EMPTY JOB IS THE FACT. This is Arc 0's finding,
// applied to the setup slot. `cmd.Wait()` returning tells us the process
// MOOMBOX SPAWNED exited, which for a launcher is not the browser: Firefox and
// its forks hand off and exit in ~170ms (measured — see drainJob's doc, where
// closing the job at that moment was found to kill the browser mid-load), and
// a Chromium binary behind a shim, a `.bat`, `msedge_proxy.exe`, a snap or any
// custom path accepted through Settings does the same. Believing the hint
// there would have the reap close a Job Object with the user's live login
// inside it 60 seconds after they started typing.
//
// `known` false means NO ANSWER, and the caller must read that as "still
// running" — never as "gone". drainJob draws the identical line on the same
// syscall: a zero from a platform that cannot count is "nothing was waited on",
// which is a different statement from "the browser finished".
//
// WHERE THAT LEAVES THE REAP, stated so nobody has to derive it:
//
//   - Windows + Chromium-family — answerable, and the reap works.
//   - Windows + Firefox-family — answerable too, since startFirefoxSetup
//     creates and stores a job of its own. That was the last Windows path where
//     the reap could not fire, and it was the common one: knownBrowsers lists
//     the Firefox family ahead of every Chromium entry.
//   - Linux and Docker — answerable since the process-group reap landed.
//     configureCmdSysProcAttr sets Setpgid, so every browser leads its own
//     group; queryable() is true once that group was adopted, and
//     activeProcesses counts its members from /proc. One case still answers
//     "no idea", honestly: a container whose /proc cannot be walked. One
//     answers WRONGLY: a browser that called setsid() and left the group
//     reads as gone, and the reap releases the slot with it still on screen
//     (no kill — the group it would signal is empty). Which packagings do
//     that is unmeasured. NOT FIELD-VERIFIED — built and unit-tested against
//     a fake process table, with a user's bug report as the gate.
//   - darwin and every other target — no primitive at all (job_other.go is
//     still a no-op stub), so nothing is answerable and the reap never fires.
//     The client-side cancel (the unload beacon, Skip, Escape, the TUI
//     countdown) is what clears an abandoned setup there.
//
// Three answerable targets and one that is not. Wherever it is not — and on
// Windows and Linux wherever newProcessJob or its assign failed — the
// client-side cancel (the unload beacon, Skip, Escape, and the TUI countdown)
// is what clears an abandoned setup; the gap is specifically "no client
// survived to say anything".
//
// A package variable so tests can supply the answer a real Job Object gives on
// a machine where no browser may be launched — the same seam convention as
// detectBrowser, killProcessTree and writeCookieFile. Nothing in production
// reassigns it.
var setupBrowserGone = realSetupBrowserGone

// realSetupBrowserGone is setupBrowserGone's implementation — see there for the
// contract and for why `known` is the half that matters. Split out and named
// only so a test that has stubbed the seam can put the genuine probe back for
// one case; nothing else should call it directly.
func realSetupBrowserGone(job *processJob) (gone, known bool) {
	return browserGoneFrom(job)
}

// browserGoneFrom is realSetupBrowserGone's body, written against the two
// methods it actually uses rather than against *processJob.
//
// The split buys exactly one thing, and it is the thing the owner's ruling
// asked for. The Linux processJob forwards both methods to a pgroupJob, so a
// test on Windows can hand THIS function a pgroupJob backed by a fake process
// table and execute the real pairing — including the branch that matters most,
// a table that cannot be read answering "cannot say" rather than "gone". No
// Linux box, no browser, and no second copy of the rule to drift.
//
// queryable, not `job != nil`. activeProcesses answers 0 for three different
// situations and only one of them is "the job is empty": a nil job (a launch
// where newProcessJob failed, or where the assign failed and the launcher
// dropped the untrackable job rather than let it lie), an already-closed handle
// or a forgotten group, and a platform whose processJob cannot count at all.
// The type knows which it is; this does not.
//
// Passing a nil *processJob through the interface parameter still answers
// (false, false): all three platform implementations nil-check their receiver,
// which is the same property that makes queryable() the right question.
func browserGoneFrom(job interface {
	queryable() bool
	activeProcesses() (int, error)
}) (gone, known bool) {
	if !job.queryable() {
		return false, false
	}
	active, err := job.activeProcesses()
	if err != nil {
		return false, false
	}
	return active == 0, true
}

// setupBrowserLiveLocked reports whether a setup browser is still running.
//
// Three cases, in the order they are cheap:
//
//   - no process registered            → not live;
//   - the spawned process has not exited → live, on the hint alone, which is
//     sufficient in that direction: a running launcher means a running setup;
//   - the spawned process HAS exited    → ask the Job Object, and treat "no
//     answer" as live. See setupBrowserGone for why the hint cannot be trusted
//     in this direction and what the consequence of trusting it would be.
//
// Caller must hold s.mu.
func (s *AutoCookieService) setupBrowserLiveLocked() bool {
	if s.setupProcess == nil {
		return false
	}
	if !s.browserExited {
		return true
	}
	gone, known := setupBrowserGone(s.setupJob)
	return !(known && gone)
}

// setupRetainedLocked reports whether a setup whose spawned process has ALREADY
// exited is still being held. It is the grace window, and its only purpose is
// to let a FinishSetup that is in flight finish: see setupAbandonGrace.
//
// The distinction from setupBrowserLiveLocked matters because the two decay
// differently — "live" ends when the browser is observed to be gone, "retained"
// ends on a clock — and because only the retained state is ever reaped. The two
// overlap while a launcher has exited but its browser has not; that is
// deliberate, since both disjuncts of setupInProgressLocked say "hands off" and
// the reap tests them in order.
//
// Caller must hold s.mu.
func (s *AutoCookieService) setupRetainedLocked() bool {
	return s.setupProcess != nil && s.browserExited &&
		time.Since(s.setupRetainedSince) < setupAbandonGrace
}

// setupInProgressLocked is the predicate the three ACQUISITION consumers share:
// GetStatus publishes it as SetupInProgress, StartSetup refuses on it and
// RefreshCookiesDetailed declines on it. Keeping it in one place is the point —
// all three used to spell `setupProcess != nil || setupClaimed` out
// individually, and a fix applied to two of them would have been silent.
//
// CancelSetup is the fourth reader of that old expression and is DELIBERATELY
// NOT MIGRATED; see its doc for why the two predicates are now different, and
// in which direction.
//
// The claim is in it for the reason CancelSetup's doc gives: between
// StartSetup's gate and the browser launch there is no process yet, but there
// IS a setup in flight.
//
// Caller must hold s.mu.
func (s *AutoCookieService) setupInProgressLocked() bool {
	return s.setupClaimed || s.setupBrowserLiveLocked() || s.setupRetainedLocked()
}

// reapAbandonedSetupLocked releases a setup whose browser is gone and whose
// grace window has run out. Called INLINE by the three consumers that are about
// to test setupInProgressLocked, each already holding s.mu.
//
// IT NEVER CLOSES A JOB OBJECT THAT STILL HAS PROCESSES IN IT. The liveness
// test is setupBrowserLiveLocked, which asks the job rather than trusting the
// spawned process's exit, and which answers "live" whenever nothing can say.
// Read its doc before changing the order of the tests below.
//
// THERE IS DELIBERATELY NO REAPER GOROUTINE, and one must never be added. A
// reaper that sleeps and then takes the lock decides what to reap at a moment
// it did not observe: by the time it wakes, the slot it sampled can hold a
// NEWER attempt, and cleanupLocked closes the setup Job Object —
// KILL_ON_JOB_CLOSE — which would terminate the browser window the user is
// signed into right now. Reaping inline, under the lock the caller already
// holds, means the predicate and the cleanup see the same instant and no such
// gap exists. (The cancel-on-timeout that DOES need a clock is a client
// concern; the TUI wizard's countdown at tui/setup_wizard.go is where it
// lives, and it POSTs a cancel rather than reaching into this state.)
//
// Caller must hold s.mu.
func (s *AutoCookieService) reapAbandonedSetupLocked() {
	if s.setupProcess == nil || s.setupClaimed {
		return // nothing registered, or a StartSetup owns the slot right now
	}
	if s.setupBrowserLiveLocked() {
		// Re-arm the grace from the last moment the setup was OBSERVED alive,
		// not from the moment its launcher exited. Without this a browser that
		// outlives its launcher — the whole reason the job is consulted above —
		// would burn its entire window while still running, and be reaped the
		// instant it closed with no grace left for the finish the user is about
		// to ask for. Best-effort, because it only advances when someone looks;
		// FinishSetup's own stamp is what guarantees a finish its full window.
		s.setupRetainedSince = time.Now()
		return
	}
	if s.setupRetainedLocked() {
		return
	}
	if s.logger != nil {
		s.logger.Info("releasing an abandoned cookie setup — its browser is gone and no finish followed",
			"last_seen_alive", time.Since(s.setupRetainedSince).Round(time.Second).String()+" ago",
			"platform", s.targetPlatform)
	}
	s.cleanupLocked()
}

// GetStatus returns the current auto-cookie status.
func (s *AutoCookieService) GetStatus() AutoCookieStatus {
	// DetectBrowser/DetectBrowsers do filesystem I/O and registry queries —
	// call outside the lock to avoid holding it while doing slow I/O.
	browser := DetectBrowser()
	available := DetectBrowsers()
	var cfgPath, cfgType string
	if s.ConfiguredBrowserOverride != nil {
		cfgPath, cfgType = s.ConfiguredBrowserOverride()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Status polling is the most frequent visitor to this lock, which makes it
	// the reap that actually fires in practice — both UIs poll it while their
	// cookie dialog is open, and the TUI polls it with no dialog at all.
	s.reapAbandonedSetupLocked()

	var lastRefreshStr *string
	if s.lastRefresh != nil {
		v := s.lastRefresh.UTC().Format(time.RFC3339)
		lastRefreshStr = &v
	}

	return AutoCookieStatus{
		SetupInProgress:       s.setupInProgressLocked(),
		Browser:               browser,
		AvailableBrowsers:     available,
		ConfiguredBrowserPath: cfgPath,
		ConfiguredBrowserType: cfgType,
		LastRefresh:           lastRefreshStr,
		LastError:             s.lastError,
		// Must be a COPY: the HTTP handlers JSON-marshal this after the lock
		// is released, while refresh/flag paths mutate the live map under
		// s.mu — a concurrent map read+write is a fatal runtime throw that
		// RecoveryMiddleware cannot catch.
		NeedsManualRelogin: maps.Clone(s.needsRelogin),
	}
}

// ReloginStatus returns which platforms need the user to sign in again,
// without GetStatus's browser/registry detection scan (~155ms measured
// 2026-08-25: DetectBrowser + DetectBrowsers, filesystem I/O and a reg.exe
// spawn on Windows). Four of GetStatus's five production callers read
// nothing else from the returned AutoCookieStatus, so they pay that cost on
// every poll for a field this method computes identically — same lock,
// same clone-under-lock copy of s.needsRelogin GetStatus returns.
//
// Status polling is the most frequent visitor to this lock, which makes it
// the reap that actually fires in practice — both UIs poll it while their
// cookie dialog is open, and the TUI polls it with no dialog at all (see
// GetStatus's doc comment, which this one must keep agreeing with). Moving
// GetStatus's four highest-frequency callers here means THIS method is now
// that most-frequent visitor, so it MUST call reapAbandonedSetupLocked
// exactly as GetStatus does — a future "simplification" that drops the call
// because ReloginStatus "only reads needsRelogin" would silently stop Arc
// 3's abandoned-setup reap from firing in production, with no existing test
// noticing because the reap still exists, it would just never run.
func (s *AutoCookieService) ReloginStatus() AutoCookieReloginRequired {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapAbandonedSetupLocked()
	return maps.Clone(s.needsRelogin)
}

// browserOverrideConfigured reports whether a (path, browserType) pair
// read from ConfiguredBrowserOverride is a REAL override, as opposed to a
// partially-filled setting. Both must be non-empty: the TUI's browser_type
// field is free text with no matching path field required
// (tui/settings.go's Save writes BrowserType independently of
// BrowserPath), so a type typed in without a path is reachable in
// practice, not just in theory. resolvedBrowser and
// dpapiExtractAsNetscape's caller (autocookies.go's DPAPI fallback branch)
// both gate on this SAME predicate — Arc 8 fix round 1, Finding 3: before
// this existed, the DPAPI call site keyed off browserType alone, so a
// type-only setting was "no override" everywhere else but a hard filter
// there.
func browserOverrideConfigured(path, browserType string) bool {
	return path != "" && browserType != ""
}

// resolvedBrowser returns the user's configured browser when set, else
// the auto-detected best match. Used by StartSetup and RefreshCookies
// so the UI's browser_path/browser_type setting actually drives
// extraction (not just cosmetic display in the dropdown).
func (s *AutoCookieService) resolvedBrowser() *DetectedBrowser {
	if s.ConfiguredBrowserOverride != nil {
		path, btype := s.ConfiguredBrowserOverride()
		if browserOverrideConfigured(path, btype) {
			// Try to find the matching DetectedBrowser entry from
			// DetectBrowsers so Name is human-readable; fall back to
			// path-as-name if the configured path isn't in the
			// detected set (legitimate case for a user-supplied
			// custom binary).
			for _, b := range DetectBrowsers() {
				if b.Path == path {
					return &b
				}
			}
			return &DetectedBrowser{Type: btype, Path: path, Name: path}
		}
	}
	if s.detectBrowser != nil {
		return s.detectBrowser()
	}
	return DetectBrowser()
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

// browserLaunchBlocked reports whether this pass is forbidden a headless
// browser. Both consultations of the gate go through here so they cannot drift
// apart: one decides whether a browser is used, the other decides which of two
// sentences explains its absence, and a reader given the wrong sentence is sent
// to install a browser they already have.
//
// nil predicate = allowed, so a service built without it behaves exactly as
// before.
func (s *AutoCookieService) browserLaunchBlocked(policy browserGatePolicy) bool {
	return policy == gateApplies && s.BrowserLaunchAllowed != nil && !s.BrowserLaunchAllowed()
}

// refreshBrowser is resolvedBrowser as a REFRESH PASS sees it: the configured
// or detected browser, or nil when cookies.auto_enabled has switched headless
// browser runs off and this pass is one the flag speaks for.
//
// The wrapper exists so the gate applies to refreshes only. resolvedBrowser has
// two other callers — StartSetup, which is acquisition and must never be gated
// (see BrowserLaunchAllowed), and decideStartupSeed, which uses it to ask "is
// this a browserless host?", a question about the machine rather than about a
// setting.
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

// StartSetup launches a browser for the user to log in.
func (s *AutoCookieService) StartSetup(platform string) error {
	s.mu.Lock()
	// Checked before the in-progress gate: a stopped service is not "busy",
	// and telling the caller to try again shortly would be wrong — Stop is
	// permanent for this service's lifetime.
	if s.stopped {
		s.mu.Unlock()
		return ErrServiceStopped
	}
	// Before the gate, not after: a setup the user walked away from must not be
	// the reason they cannot start a new one. This is the site the wedge was
	// reported at.
	s.reapAbandonedSetupLocked()
	if s.setupInProgressLocked() {
		s.mu.Unlock()
		return ErrSetupInProgress
	}
	if s.refreshCmd != nil {
		s.mu.Unlock()
		return fmt.Errorf("please try again shortly: %w", ErrRefreshInProgress)
	}
	// Claim the slot inside this critical section (mirrors RefreshCookies'
	// refreshCmd sentinel): browser detection, MkdirAll, and the icacls
	// shell-out below take tens of milliseconds, and a second StartSetup
	// passing the gate in that window would launch a second browser against
	// the same profile and leak the first Job Object. The claim drops when
	// this call returns — by then either setupProcess holds the real
	// process (success) or the attempt failed and the slot must free up.
	//
	// Claim time is the ONE place `cancelled` is cleared, and that is what
	// makes the check at the end of the preparation below mean anything. It
	// used to be cleared in cleanup() as well; cleanup() runs on every setup
	// exit path INCLUDING CancelSetup's own last act, so a complete cancel
	// erased its own flag microseconds after raising it and the check below
	// could only ever catch a cancel that landed in the sliver between
	// CancelSetup's flag write and its cleanup(). Clearing it here and nowhere
	// else means the flag survives until the setup it belongs to consumes it.
	s.setupClaimed = true
	s.cancelled = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.setupClaimed = false
		s.mu.Unlock()
	}()

	browser := s.resolvedBrowser()
	if browser == nil {
		return fmt.Errorf("supported browser (Firefox, Chrome, Brave, Edge, Opera, or Waterfox) required: %w", ErrNoBrowserFound)
	}

	if err := os.MkdirAll(s.profileDir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	// Tighten the dir's ACL so only the current user can read its
	// contents (cookies.sqlite, browsing history, login state). Non-
	// Windows hosts get a no-op; Windows shells out to icacls. A
	// failed tightening doesn't fail setup — log and continue.
	// Audit reports/cookies.md #25. Demoted to Debug (matches the config +
	// cookie dir sites): the common failure is ACCESS_DENIED on a dir
	// created under an elevated/admin context, benign on the single-user
	// host this targets — raise the log level to Debug to see the miss.
	if err := utils.ApplyUserOnlyDACL(s.profileDir); err != nil {
		s.logger.Debug("could not restrict profile dir to current user", "path", s.profileDir, "err", err)
	}

	s.mu.Lock()
	if s.cancelled {
		// CancelSetup landed while we were detecting browsers / preparing
		// the profile dir — the slot was already claimed, so the UI showed
		// setup-in-progress and offered Cancel. Honor it instead of
		// launching a browser the user just dismissed.
		s.mu.Unlock()
		return ErrSetupCancelled
	}
	s.setupBrowser = browser
	s.lastError = nil
	s.browserExited = false
	// Reset with browserExited, which it only has meaning alongside. The reap
	// or a cleanup has already zeroed it on every path that reaches here; the
	// pairing is so the two can never be read out of step.
	s.setupRetainedSince = time.Time{}
	if platform == "" {
		platform = "youtube"
	}
	s.targetPlatform = platform
	s.mu.Unlock()

	loginTarget := loginURL
	if platform == "twitch" {
		loginTarget = twitchLoginURL
	}

	if isFirefoxBased(browser.Type) {
		return s.startFirefoxSetup(browser, loginTarget)
	}
	return s.startChromiumSetup(browser, loginTarget)
}

// SetupResult reports what ONE interactive setup concluded, per platform.
//
// Two independent facts per platform, and the reason this type exists is that
// they can disagree:
//
//   - the verdict — what the auth check CONCLUDED, in the same three-way
//     vocabulary a refresh pass uses. RefreshUnknown is the zero value on
//     purpose, so any path that returns without checking cannot accidentally
//     assert health or failure.
//   - Accepted — what the CALLER is told, which is deliberately not the
//     verdict. A sign-in the user just completed is accepted when the site
//     could not answer; see the acceptance predicate in FinishSetupDetailed
//     for why, and why it is not extended to a check that never ran.
//
// Accepted with a verdict of RefreshUnknown is the state this type exists to
// carry: the cookies are saved and in use, and Moombox could not reach the
// site to confirm them. Collapsing that into either neighbour is what tells a
// user whose network blipped mid-check that their sign-in failed.
type SetupResult struct {
	YouTube RefreshVerdict
	Twitch  RefreshVerdict

	YouTubeAccepted bool
	TwitchAccepted  bool

	// Wrote reports that this finish REPLACED cookies.txt, and it is true on
	// one error path as well as on success.
	//
	// It is the setup path's counterpart to RefreshResult.Ran, and it exists
	// for the same caller: whoever has to decide whether the credential
	// fingerprint may have moved and an auth re-check is therefore owed
	// (Arc 10 R4/R5). Every other exit from FinishSetupDetailed leaves the file
	// exactly as it was — no setup in progress, a cancelled one, an unreadable
	// browser profile, the S9 abort that refuses to overwrite a cookies.txt it
	// could not read, a failed MkdirAll, a failed write — but the reload after
	// a SUCCESSFUL write can still fail, and that exit returns an error over a
	// file that has already been replaced. A caller that gates its re-check on
	// `err == nil` misses precisely that case, which is the one where the jar
	// in memory is stale and a re-check would repair it.
	//
	// Deliberately not on the wire: cookieSetupOutcome builds its payload key
	// by key, and this is an internal signal about the file, not a verdict
	// about a platform.
	Wrote bool
}

// FinishSetup extracts cookies from the running browser and saves them, and
// reports only whether each platform was ACCEPTED.
//
// A thin wrapper over FinishSetupDetailed, the same split RefreshCookies /
// RefreshCookiesDetailed already draws — with one honest difference: both of
// the callers that RENDER a finish (the HTTP route and the TUI wizard) had to
// move to the detailed form, so nothing but the tests calls this today. It is
// kept because the projection is the thing worth pinning: the acceptance
// answer must not drift as the verdicts gain consumers, and a caller whose
// question really is "did this platform end up usable" should not have to
// know about verdicts to ask it.
//
// A caller that renders the outcome must NOT use this: the bool pair cannot
// say "saved, but we could not check them", and a UI built on it has to guess.
func (s *AutoCookieService) FinishSetup(ctx context.Context) (ytAuth, twAuth bool, err error) {
	result, err := s.FinishSetupDetailed(ctx)
	return result.YouTubeAccepted, result.TwitchAccepted, err
}

// FinishSetupDetailed extracts cookies from the running browser, saves them,
// and reports both what it accepted and what it concluded.
func (s *AutoCookieService) FinishSetupDetailed(ctx context.Context) (SetupResult, error) {
	s.mu.Lock()
	if s.setupProcess == nil || s.setupBrowser == nil {
		s.mu.Unlock()
		return SetupResult{}, ErrNoSetupInProgress
	}
	if s.cancelled {
		s.mu.Unlock()
		return SetupResult{}, ErrSetupCancelled
	}
	// Restart the retention clock. The grace window exists FOR this call, and
	// measuring it from the browser's exit alone would let a finish that
	// started legitimately inside the window be reaped part-way through: the
	// Chromium path reads s.cdpPort, which cleanupLocked zeroes, so the reap
	// would turn a working extraction into "CDP port not available".
	//
	// Deliberately not a second flag. A "finishing" latch is one more piece of
	// lifecycle state that one path sets and another has to remember to clear —
	// this arc's recurring hazard — whereas moving a timestamp forward cannot
	// leave the slot stuck: the window still expires on its own, and every exit
	// path from here already calls cleanup().
	//
	// Note it stamps even when the browser is still running (browserExited
	// false), where it means nothing yet; the Firefox path is about to close
	// the browser itself, and the wait goroutine will re-stamp on the real exit
	// a moment later.
	s.setupRetainedSince = time.Now()
	browser := s.setupBrowser
	s.mu.Unlock()

	var netscapeCookies string
	var err error

	if isFirefoxBased(browser.Type) {
		s.closeFirefoxGracefully()
		var stats firefoxReadStats
		netscapeCookies, stats, err = readFirefoxCookies(s.profileDir)
		s.logFirefoxReadStats(stats)
	} else {
		netscapeCookies, err = s.extractChromiumCookies()
		s.killSetupProcess()
	}

	// Interactive setup has a legitimate empty state the refresh and
	// profile-import paths do not: the user opened the browser and closed it
	// without signing in. Both read paths report an empty profile as a hard
	// error (a silently empty jar is the bug those errors exist to catch), so
	// translate it back here — the setup dialog should say "no login detected",
	// not fail.
	//
	// OUTSIDE the if/else, not inside the Firefox arm where it used to live.
	// Chromium can produce the same sentinel now that cdpGetCookiesAsNetscape
	// distinguishes "the browser answered and holds nothing" from "the read
	// failed", and while it could not, a Chromium user who never signed in got
	// the route's default 500 "failed to finish setup" for a state that is not
	// a failure at all.
	if errors.Is(err, ErrNoCookiesInProfile) {
		// The error rides into the log line. On the Chromium path it can carry
		// a tier failure that was out-voted by another tier's empty answer, and
		// cdpGetCookiesAsNetscape has no logger of its own — so dropping it here
		// would leave the only evidence that this verdict might be wrong with
		// nowhere to go.
		s.logger.Info("cookie setup finished with an empty profile — no login detected", "detail", err)
		s.setError("no login detected — sign in before finishing setup")
		s.cleanup()
		// RefreshFailed, not RefreshUnknown, and the difference is what the
		// dialog says. This attempt produced no credential of any kind, which
		// is the same conclusion checkPlatformAuth reaches for a platform with
		// nothing on disk — "there is nothing to send, so no request can be
		// authenticated". Unknown would route the UI to its "we could not
		// check" copy, which is the one wrong thing to say about a browser
		// that plainly held no login.
		//
		// It is a statement about THIS setup, not about cookies.txt: this path
		// deliberately merges nothing and reloads nothing, so a working session
		// already on disk is untouched and unexamined. The UI branch it selects
		// says "no login detected" and makes no verification claim.
		return SetupResult{YouTube: RefreshFailed, Twitch: RefreshFailed}, nil
	}

	if err != nil {
		s.setError(err.Error())
		s.cleanup()
		return SetupResult{}, err
	}

	// Merge with existing cookies using temp file + rename for atomicity
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		// THE SET, which this exit and the two below it were missing. The
		// policy on the lastError field says every failure exit records what it
		// concluded, and these three returned an error to the caller while
		// leaving the field both dashboards render blank — so a setup that died
		// on a permission or mount problem showed one sentence in the dialog and
		// then, once the dialog closed, nothing anywhere. The dialog is modal and
		// transient; the status field is where an operator looks afterwards.
		//
		// Ordering is a convention rather than a requirement: cleanup() never
		// clears (see the field's policy and
		// TestCleanupAfterAFailedSetupKeepsLastError), so the set survives it
		// either way. Kept before cleanup to match every other exit here.
		s.setError("could not create the directory for cookies.txt: " + err.Error())
		s.cleanup()
		return SetupResult{}, err
	}

	existingData, readErr := readCookieFile(s.cookiePath)
	switch {
	case readErr == nil:
		if len(existingData) > 0 {
			netscapeCookies = mergeCookieFiles(string(existingData), netscapeCookies)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		// No cookies.txt yet — the normal first-run case. Nothing to
		// merge; proceed with just the freshly extracted cookies exactly
		// as before.
	default:
		// A transient read failure (permission blip, locked file, I/O
		// error) is NOT the same as "no existing file" and must not be
		// treated as nothing to merge — that used to fall straight
		// through to the write below with ONLY the newly extracted
		// cookies, silently replacing a cookies.txt that may hold
		// working credentials for the other platform. Abort instead:
		// don't merge, don't write.
		//
		// Wraps ErrCookieFileUnreadable so callers can tell this apart from
		// every other setup failure — see the sentinel's doc comment for
		// why that distinction has to survive to the operator: the file was
		// deliberately left untouched, and must not be the thing they are
		// told to replace.
		mergeErr := fmt.Errorf("%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			ErrCookieFileUnreadable, readErr)
		s.logger.Error("cookie setup: aborting rather than overwrite cookies.txt after a read failure",
			"path", s.cookiePath, "err", readErr)
		s.setError(mergeErr.Error())
		s.cleanup()
		return SetupResult{}, mergeErr
	}

	// Write merged cookies via temp file + rename to prevent corruption on partial failure
	if err := writeFileAtomic(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		// Sets, for the reason spelled out at the MkdirAll exit above. The hint
		// names the one deployment mistake that actually produces this — the
		// write ends in a rename, and a rename cannot replace a single-file bind
		// mount — and is kept SHORT, unlike the paragraph refresh.go attaches to
		// its own failed write: that one goes to a log, this one goes to a status
		// line both dashboards render.
		s.setError("could not write cookies.txt: " + err.Error() +
			" — if this is Docker, mount the data directory rather than cookies.txt itself")
		s.cleanup()
		return SetupResult{}, err
	}

	// Reload jar and verify.
	//
	// Wrote is set from here down: writeFileAtomic above has replaced
	// cookies.txt, so every exit past this point — this error one included —
	// leaves a file whose credential pair may differ from the one the running
	// process last compared. See SetupResult.Wrote.
	if err := s.jar.Load(s.cookiePath); err != nil {
		// Sets, for the reason spelled out at the MkdirAll exit above. This one
		// is the worst of the three to leave silent: the cookies were extracted
		// AND written, so the file on disk is fine and nothing about the state
		// looks wrong — the setup simply reports nothing and the user has no
		// idea whether to run it again.
		s.setError("cookies.txt was written but could not be loaded: " + err.Error())
		s.cleanup()
		return SetupResult{Wrote: true}, err
	}

	// Presence + real API verification, through the same pairing the refresh
	// path uses. This was the last inline copy of it; the nil-callback contract
	// (presence is then the only signal, reported as success with a warning so
	// callers cannot quietly succeed on cookies that are present-but-invalid —
	// audit reports/cookies.md #21) lives in checkPlatformAuth now.
	ytCheck, twCheck := s.checkPlatformAuth(ctx)

	ytAuth := credentialAccepted(ytCheck)
	twAuth := credentialAccepted(twCheck)

	// What gets WRITTEN DOWN, which is a different claim and must be the
	// stricter one. PersistPlatforms unions into cfg.Cookies.Platforms, a set
	// that only ever grows and is never retracted, so accepting a login on an
	// inconclusive check and then recording it as verified turns one rate limit
	// during setup into a durable, permanent assertion that YouTube was
	// verified. Accepting is right; recording the acceptance as a verification
	// is not.
	ytVerified := ytCheck.ok()
	twVerified := twCheck.ok()

	// Inconclusive has to read as inconclusive. The nil-callback branch inside
	// checkPlatformAuth says so; the errored branch said nothing at all, so a
	// 429 during setup was indistinguishable from a clean pass. No cause is
	// named and no error is recorded: the check did not complete, which is not
	// a finding about the credentials, and s.lastError renders in Settings as
	// "your recordings will fail". The two halves get different wording because
	// they carry different advice — one is "try again", the other is "this
	// login is not usable as it stands".
	warnInconclusive := func(platform string, p platformAuth) {
		if s.logger == nil || !p.hasCookies || p.state != verifyUnknown {
			return
		}
		if p.attempted {
			s.logger.Warn(platform + " auth check did not complete during setup — accepting the sign-in without verifying it")
			return
		}
		s.logger.Warn(platform + " auth check was never attempted during setup — the extracted cookies cannot form an authenticated request")
	}
	warnInconclusive("YouTube", ytCheck)
	warnInconclusive("Twitch", twCheck)

	if !ytAuth && !twAuth && s.logger != nil {
		// Not "verification failed": a platform with no auth cookie at all was
		// never verified, and in a single-platform setup one of the two never
		// is.
		s.logger.Warn("cookies extracted, but neither platform is authenticated")
	}

	// Clear the re-login flag for every platform this setup ACCEPTED, not just
	// the ones it verified. The flag means "go and sign in again", the user
	// just did exactly that, and leaving it raised because the confirming
	// request hit a rate limit would nag them about work they have already
	// done. Unlike the persisted set below, this is process-local and the next
	// conclusive check re-raises it.
	s.mu.Lock()
	if ytAuth {
		s.needsRelogin["youtube"] = false
	}
	if twAuth {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	// Persist verified platforms to config so we can detect auth loss after
	// restart (matches TS autoCookies.ts persistPlatforms). VERIFIED, not
	// accepted — see above. Withholding an unverified platform costs no
	// recovery: shouldFireRecovery's first-conclusive-check branch fires for a
	// platform absent from the persisted list, which is precisely the case
	// SetExpectedPlatforms's per-platform everConcluded flags exist to keep
	// working.
	if s.PersistPlatforms != nil {
		s.PersistPlatforms(ytVerified, twVerified)
	}

	now := time.Now()
	s.mu.Lock()
	s.lastRefresh = &now
	s.mu.Unlock()
	s.cleanup()

	// Persist LastRefresh to the sidecar so the next launch doesn't
	// re-run the refresh immediately. Audit reports/cookies.md #48.
	persistedPlatforms := []string{}
	if ytVerified {
		persistedPlatforms = append(persistedPlatforms, "youtube")
	}
	if twVerified {
		persistedPlatforms = append(persistedPlatforms, "twitch")
	}
	if metaErr := SaveMeta(s.cookiePath, CookieMeta{
		LastRefresh: now,
		Platforms:   persistedPlatforms,
	}); metaErr != nil && s.logger != nil {
		s.logger.Warn("could not persist cookies.meta.json", "err", metaErr)
	}

	// "verified" is the word this line uses, so only what verified goes in it.
	var verified []string
	if ytVerified {
		verified = append(verified, "YouTube")
	}
	if twVerified {
		verified = append(verified, "Twitch")
	}
	if len(verified) > 0 {
		s.logger.Info("[AutoCookies] Setup complete — verified: " + strings.Join(verified, " + "))
	}

	// The distinction the three log lines above draw used to end at the log.
	// verdictOf is the same projection the refresh path publishes, so the
	// dialog can render "saved, but we could not check them" in the wording
	// the manual-refresh surfaces already use.
	return SetupResult{
		YouTube:         verdictOf(ytCheck),
		Twitch:          verdictOf(twCheck),
		YouTubeAccepted: ytAuth,
		TwitchAccepted:  twAuth,
		Wrote:           true,
	}, nil
}

// CancelSetup aborts an in-flight setup: it raises the cancelled flag, kills
// the setup browser if one is running, and clears the per-setup state.
//
// "In flight" is `setupProcess != nil || setupClaimed`: there is something in
// the slot to tear down. The claim half is not a technicality — between
// StartSetup's gate and the browser launch there is no process yet, but there
// IS a setup to cancel, and StartSetup's mid-preparation check is what consumes
// the flag this call raises.
//
// THAT IS DELIBERATELY NOT setupInProgressLocked, AND IT IS DELIBERATELY WIDER.
// It used to be the identical expression, and this doc used to say so; the
// setup slot now expires, so the two have to diverge and the direction of the
// divergence is the whole point. Every disjunct of setupInProgressLocked
// implies `setupClaimed || setupProcess != nil`, so this gate is a strict
// SUPERSET: a cancel can never answer "nothing to cancel" while the UI is still
// showing the Cancel button that produced it. The converse — a slot that has
// expired but not yet been reaped reports SetupInProgress false while a cancel
// still succeeds — is the useful direction: that cancel is what tears the dead
// slot down, and answering 404 while leaving state behind would be worse.
//
// Narrowing this to setupInProgressLocked would therefore need a fourth reap
// site to stay coherent, and would trade a cancel that cleans up for one that
// declines. Don't.
//
// Returns ErrNoSetupInProgress when there was nothing to cancel — a second
// cancel, or a cancel with no setup ever started. This used to return nothing
// at all and the route answered `{"success": true}` unconditionally, so
// cancelling twice reported a cancel that never happened.
func (s *AutoCookieService) CancelSetup() error {
	s.mu.Lock()
	// Deliberately a superset of setupInProgressLocked — see above. Not a
	// missed migration.
	if s.setupProcess == nil && !s.setupClaimed {
		s.mu.Unlock()
		return ErrNoSetupInProgress
	}
	s.cancelled = true
	s.mu.Unlock()

	s.killSetupProcess()
	s.cleanup()
	s.logger.Info("auto-cookie setup cancelled")
	return nil
}

// AbandonSetup is what a CLIENT reports when the client itself went away — the
// dashboard tab unloaded. It is NOT CancelSetup, and the difference is the
// point: a deliberate click is consent to close the browser, a tab unload is
// not.
//
// The beacon behind this used to POST /cancel. That was harmless when it was
// written, because a Firefox setup had no Job Object and a cancel could not
// reach the browser. S5 gave it one, so every cancel now closes the window —
// and the setup flow's own instructions send the user AWAY from the dashboard
// tab to go and sign in. Closing the now-idle tab became a remote kill of the
// window they are typing their password into, on the default Windows path.
//
// So this releases the slot WITHOUT killing anything, and it releases only
// where releasing is not itself a kill. The split is not a new rule; it is
// setupBrowserGone's existing `known`, asked one more time:
//
//   - known — the Job Object can be interrogated, so the REAP owns this. It
//     will release the slot on its own correct predicate (the browser actually
//     being gone) and cannot fire while a login is in progress. Releasing here
//     would mean cleanupLocked, which closes that handle, which is
//     KILL_ON_JOB_CLOSE on a live browser. Do nothing.
//   - not known — no job (a failed assign on either platform), a Linux group
//     whose /proc cannot be read, or a platform with no primitive at all
//     (darwin and the fallback build). The reap can never fire there, so this
//     is the only thing that releases the slot; and with nothing able to reach
//     the browser, releasing kills nothing — on Linux the group kill inside
//     cleanupLocked refuses a group it cannot see. Release.
//
// Which is to say plainly: this call is redundant wherever a group or a Job
// Object was adopted — Windows, and Linux since the process-group reap — and
// load-bearing where nothing was: darwin, the fallback build, and any Linux
// launch whose group could not be adopted. The declining arm is not dead code
// on either platform — deleting the check would restore the kill on both.
//
// Two things it deliberately does NOT do. It does not raise `cancelled`,
// because it is not an abort: a StartSetup still preparing a launch is left to
// finish, and the slot it publishes is then governed by the normal rules. And
// it does not call killSetupProcess, which is the whole point.
//
// Returns ErrNoSetupInProgress when there was nothing to release, matching
// CancelSetup so the route can answer both the same way. `released` reports
// whether the slot was actually cleared; the beacon cannot read it, but the
// log line and the tests can.
func (s *AutoCookieService) AbandonSetup() (released bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.setupProcess == nil && !s.setupClaimed {
		return false, ErrNoSetupInProgress
	}
	if _, known := setupBrowserGone(s.setupJob); known {
		s.logger.Debug("client abandoned a cookie setup; leaving it to the reap",
			"platform", s.targetPlatform)
		return false, nil
	}
	if s.setupProcess == nil {
		// A claim in flight and no browser published yet. There is nothing to
		// release, and aborting the launch is not this call's business.
		s.logger.Debug("client abandoned a cookie setup mid-launch; nothing to release yet")
		return false, nil
	}
	s.logger.Info("client abandoned a cookie setup — releasing the slot, leaving the browser alone",
		"platform", s.targetPlatform)
	s.cleanupLocked()
	return true, nil
}

// RefreshVerdict is what a refresh pass concluded about ONE platform.
//
// The three states exist for the same reason verificationState's do: "we did
// not find out" is not "it is dead", and a notification built on the second
// when only the first is true tells an operator to re-export cookies that are
// perfectly fine.
type RefreshVerdict int

const (
	// RefreshUnknown means this pass learned nothing about the platform: the
	// refresh declined to run, aborted before it could verify, or the
	// verification itself could not reach the service. It is the ZERO VALUE
	// on purpose — a caller that forgets to populate a field must not
	// accidentally assert health or failure.
	RefreshUnknown RefreshVerdict = iota
	// RefreshFailed means the platform is conclusively not authenticated
	// after this pass. It covers "the credentials were rejected" and "there
	// are no credentials at all" alike: the question this answers is whether
	// authenticated requests will work, and in both cases they will not.
	RefreshFailed
	// RefreshOK means the platform is conclusively authenticated.
	RefreshOK
)

func (v RefreshVerdict) String() string {
	switch v {
	case RefreshFailed:
		return "failed"
	case RefreshOK:
		return "ok"
	default:
		return "unknown"
	}
}

// RecheckedPlatform pairs one platform's display label with what the manual
// recheck concluded about it.
type RecheckedPlatform struct {
	Label   string
	Verdict RefreshVerdict
}

// RecheckReport words the answer to a manual "recheck cookies".
//
// TWO SURFACES, ONE SENTENCE, exported for the same reason
// RefreshDeclinedCauses is: the TUI's R C chord and the Web dashboard's
// refresh button are THE SAME GESTURE, and they were answering it differently
// — the TUI with a three-way verdict, the Web with a two-arm
// "successful"/"completed" keyed on a success bool that is false for a check
// that never reached the site. The Web copy cannot import Go, so a test pins
// the rendered string against this function; sharing the sentence is what
// stops a fourth phrasing appearing the next time one side is edited.
//
// Only RefreshFailed says anything about the credentials. RefreshUnknown
// speaks about the CHECK — "could not establish", the arc's settled wording —
// because a check that could not reach the site has concluded nothing, and
// telling an operator their cookies failed is how they get sent off to
// re-export a session that is perfectly alive.
//
// Callers pass only the platforms they actually monitor; an empty list is a
// real state (nothing configured) and gets its own sentence rather than an
// empty one.
func RecheckReport(platforms ...RecheckedPlatform) string {
	if len(platforms) == 0 {
		return "Cookies: no platforms configured"
	}
	parts := make([]string, 0, len(platforms))
	for _, p := range platforms {
		switch p.Verdict {
		case RefreshOK:
			parts = append(parts, p.Label+" OK")
		case RefreshFailed:
			parts = append(parts, p.Label+" not authenticated")
		default:
			parts = append(parts, p.Label+" — could not establish")
		}
	}
	return "Cookies: " + strings.Join(parts, ", ")
}

// RefreshResult reports a refresh pass PER PLATFORM.
//
// The whole-service bool that RefreshCookies still returns cannot answer the
// question its callers actually ask. A recovery attempt is triggered FOR a
// platform, and a healthy sibling made a dead YouTube look recovered: the
// field log for 2026-08-20 03:40:01 reads "YouTube auth verification failed
// after refresh" and "auto-cookie recovery succeeded platform=youtube" three
// lines apart. Both platforms were already computed at that point; only the
// signature threw them away.
type RefreshResult struct {
	// Ran is false when the pass declined before doing any work at all —
	// setup in progress, a refresh already in flight, nothing to refresh.
	// Both verdicts are RefreshUnknown whenever this is false.
	Ran     bool
	YouTube RefreshVerdict
	Twitch  RefreshVerdict

	// Renewed reports whether THIS pass produced the credentials it verified,
	// as opposed to finding the previous ones still alive — the independent
	// 30-minute RefreshService keeps a working session alive whether or not
	// the browser refresh does anything at all.
	//
	// It is a fact about the MECHANISM, and the verdicts are facts about the
	// credentials; the two are independent and must stay that way. A pass can
	// verify both platforms while renewing nothing (a browser that never ran),
	// which is why RefreshCookies still returns true there: authenticated
	// requests will work. What must not happen is a UI reporting that as an
	// unqualified success — the operator pressed a button that runs the
	// browser refresh, and asking whether it worked is the whole reason they
	// pressed it.
	//
	// False on every declined and aborted pass, which renewed nothing by
	// definition. It is not a platform statement: the Firefox verdict is
	// browserLaunchActed's screenshot check everywhere, and the drain only adds
	// errBrowserDrainTimeout where a count exists (a Job Object on Windows, a
	// process group on Linux since the process-group arc; darwin and the
	// fallback build have none). It therefore means "not confirmed", never
	// "the browser failed" — see the wording note on browserLaunchActed.
	Renewed bool

	// YouTubeStored / TwitchStored report whether Moombox held ANY auth
	// cookies for that platform when the verdict was reached. This is
	// PRESENCE, not liveness, and it must never be used to decide whether
	// requests will work — that is what the verdict is for, and a stored
	// cookie that does not work is worth nothing.
	//
	// Its one legitimate use is choosing WORDING. RefreshFailed covers "the
	// credentials were rejected" and "there are no credentials at all"
	// alike, correctly, because neither will authenticate a request; but
	// telling an operator to replace dead cookies for a platform they never
	// configured names a cause that did not happen. The same AND-with-
	// presence guards three claims elsewhere in this file (needsRelogin, the
	// "manual re-login required" Warn, and the `failed` list, whose comment
	// reads "if a user only signed in to YouTube, they should not see
	// 'Twitch needs re-login'").
	//
	// False for both platforms on any pass that did not reach verification —
	// a declined or aborted pass never looked at the jar. That pairs with a
	// RefreshUnknown verdict, which asserts nothing either way.
	YouTubeStored bool
	TwitchStored  bool
}

// Verdict returns the verdict for a platform key ("youtube" / "twitch"),
// matched case-insensitively.
//
// An unrecognised key — including the empty string — returns RefreshUnknown,
// never RefreshFailed. Callers turn RefreshFailed into "your cookies are
// dead, recordings will fail"; firing that off a typo'd or absent platform
// string would be an unearned assertion sourced from a programming error.
func (r RefreshResult) Verdict(platform string) RefreshVerdict {
	switch strings.ToLower(platform) {
	case "youtube":
		return r.YouTube
	case "twitch":
		return r.Twitch
	default:
		return RefreshUnknown
	}
}

// HasCredentials reports whether Moombox held any auth cookies for the named
// platform when this pass reached its verdict. Unrecognised keys — including
// the empty string — report false.
//
// Presence, not liveness: see the field comment on RefreshResult. Use it only
// to choose how a Verdict is WORDED, never in place of one.
func (r RefreshResult) HasCredentials(platform string) bool {
	switch strings.ToLower(platform) {
	case "youtube":
		return r.YouTubeStored
	case "twitch":
		return r.TwitchStored
	default:
		return false
	}
}

// Overall folds the per-platform verdicts into the single verdict a
// whole-service caller can render. It is the one place that fold happens.
//
//   - RefreshOK: at least one platform is conclusively authenticated, so
//     authenticated work is possible.
//   - RefreshFailed: no platform verified AND at least one was conclusively
//     found unauthenticated. Conclusive, and the only branch entitled to
//     report a verification failure.
//   - RefreshUnknown: nothing conclusive either way. Says nothing about the
//     credentials — most of the ways to get here leave a perfectly healthy
//     session.
//
// Unknown covers two very different events and callers MUST NOT word them the
// same: a pass that declined before doing any work, and a pass that ran and
// could not find out. Ran draws exactly that line — see its field comment, and
// cookieRefreshReportFor in cmd/moombox/services.go for the vocabulary.
func (r RefreshResult) Overall() RefreshVerdict {
	switch {
	case r.YouTube == RefreshOK || r.Twitch == RefreshOK:
		return RefreshOK
	case r.YouTube == RefreshFailed || r.Twitch == RefreshFailed:
		return RefreshFailed
	default:
		return RefreshUnknown
	}
}

// AnyVerified is the whole-service bool RefreshCookies has always returned:
// at least one platform is conclusively authenticated.
//
// Exported because the Web and TUI wirings each hand-rolled this same OR over
// the two verdict fields, which is two copies of a rule that has to agree.
// Derived from Overall so there is a single fold rather than a second one that
// can drift from it.
func (r RefreshResult) AnyVerified() bool {
	return r.Overall() == RefreshOK
}

// RefreshDeclinedCauses names every way a refresh pass can decline with a NIL
// error IN FRONT OF A READER, for UI copy that has to explain a decline
// without naming a cause it cannot know.
//
// Three named, four in the code, and the gap is deliberate — read this before
// adding a fifth. The named three are the two slot conflicts at the top of
// RefreshCookiesDetailed (setup in progress, refresh already in flight) and the
// empty refreshPlatforms() gate. The unnamed fourth is the `stopped` latch,
// which sits above all of them and declines once Stop() has been called.
//
// The latch is left out because this constant is operator-facing copy, rendered
// by a worker log line, a TUI toast and a Web toast, and a decline caused by
// Stop() has no reader: the only way to reach it is a "Refresh now" that raced
// process shutdown, and by the time the toast would render the service it
// describes is gone. Naming it would put a cause nobody can act on in front of
// every operator who ever hits one of the three real ones.
//
// So the invariant is exhaustiveness over the declines a RUNNING service can
// produce, not over every refreshDeclined() return. A new decline reachable
// before Stop() must be added here; one reachable only after it must not.
//
// The other two refreshDeclined() returns — no browser and no profile, profile
// not found — both carry an error, so every caller has already branched away
// before it reaches this text.
//
// Exported because three surfaces render it (the worker's log via
// cookieRefreshReportFor, the TUI's R F feedback, and the Web toast) and they
// had already begun to drift — "cookies to refresh" against "cookies worth
// refreshing" — with nothing to catch it. The Go callers share this constant;
// the Web copy is pinned against it by a test, since app.js cannot import it.
const RefreshDeclinedCauses = "a setup or another refresh is already in flight, " +
	"or no platform has cookies worth refreshing"

// refreshDeclined is the result of a pass that did no work: it says nothing
// about either platform, because it never looked. See RefreshDeclinedCauses for
// the ways to get here with a nil error.
func refreshDeclined() RefreshResult { return RefreshResult{} }

// refreshAborted is the result of a pass that started work and stopped before
// verification. It ran, but it still learned nothing about either platform —
// an extraction or write failure says nothing about whether the credentials
// on disk are alive.
func refreshAborted() RefreshResult { return RefreshResult{Ran: true} }

// verdictOf projects one platform's internal verification state onto the
// exported enum. The mapping is one-to-one by construction; there is no state
// that "sort of" verified.
func verdictOf(p platformAuth) RefreshVerdict {
	switch p.state {
	case verifyOK:
		return RefreshOK
	case verifyFailed:
		return RefreshFailed
	default:
		return RefreshUnknown
	}
}

// RefreshCookies performs a headless browser visit to refresh cookies and
// reports whether ANY platform ended up authenticated.
//
// Kept as a thin wrapper over RefreshCookiesDetailed for callers whose question
// really is whole-service ("can we do authenticated work at all?"). It once had
// four: the startup seed, the periodic tick, the Settings "refresh now" button
// and the TUI's equivalent. All four have since moved to the detailed form, for
// two different reasons — the manual pair need to tell "this pass renewed the
// credentials" from "the old ones still work", and the two AUTOMATIC ones need
// Ran, which this projection discards, to decide whether anything was written
// and an auth re-check is owed (see OnPassCompleted). Nothing in production
// calls this today; the projection is kept because it is the honest answer to
// the whole-service question and the tests that ask it are the ones pinning
// that it does not drift.
//
// Callers acting ON BEHALF of one platform must use RefreshCookiesDetailed
// instead — see RefreshResult.
func (s *AutoCookieService) RefreshCookies(ctx context.Context) (bool, error) {
	return s.refreshCookies(ctx, gateApplies)
}

func (s *AutoCookieService) refreshCookies(ctx context.Context, policy browserGatePolicy) (bool, error) {
	result, err := s.refreshCookiesDetailed(ctx, policy)
	return result.AnyVerified(), err
}

// RefreshCookiesDetailed performs a headless browser visit to refresh cookies
// and reports the outcome per platform.
//
// The exported form always honours cookies.auto_enabled. The periodic timer is
// the one exception and calls refreshCookiesDetailed directly — see
// browserGatePolicy.
//
// FOUR callers outside this package, and the count matters because getting it
// wrong hid a real question once already (a review wrote "three" and the missing
// one was the only automatic caller):
//
//	internal/web/routes/cookies.go   the dashboard's shift+click, and the
//	                                 Settings page's profile-import button
//	cmd/moombox/tui_wiring.go        the TUI's R F chord
//	cmd/moombox/services.go          the download worker's auth-failure retry
//	cmd/moombox/monitor_callbacks.go the monitor's recovery attempt — PASSED AS
//	                                 A METHOD VALUE (s.autoCookieSvc.Refresh-
//	                                 CookiesDetailed, handed to
//	                                 handleRecoveryNeeded), which is why it is
//	                                 easy to miss: it is not a call expression,
//	                                 so a structural search for call sites walks
//	                                 straight past it. TestRefreshCookiesDetailed-
//	                                 CallersAreEnumerated matches references
//	                                 rather than calls for exactly that reason.
//
// The last two are AUTOMATIC and deliberately do NOT consult
// automaticImportGuard, so on a browserless host they import over an existing
// cookies.txt. That is correct and is not an oversight — see the guard's doc
// for why, and the comment at the monitor_callbacks.go call site.
func (s *AutoCookieService) RefreshCookiesDetailed(ctx context.Context) (RefreshResult, error) {
	return s.refreshCookiesDetailed(ctx, gateApplies)
}

func (s *AutoCookieService) refreshCookiesDetailed(ctx context.Context, policy browserGatePolicy) (RefreshResult, error) {
	s.mu.Lock()
	// A stopped service must not launch a browser. Declined rather than
	// errored, matching the two gates below it: nothing was examined, so the
	// pass has no verdict to report and no failure to blame on the
	// credentials. Stop() latches, so unlike those two this never clears.
	if s.stopped {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — service stopped")
		return refreshDeclined(), nil
	}
	s.reapAbandonedSetupLocked()
	// GRACE-GATED, NOT LIVE-GATED, and the difference is a data-loss bug.
	// setupInProgressLocked stays true for a setup whose browser has exited but
	// whose FinishSetup may still be running, so a headless refresh cannot
	// launch a second browser at the same profile directory while that finish
	// is reading it and merging into cookies.txt. Two writers into one cookie
	// store is the class of bug the previous arc was entirely about. Weakening
	// this to setupBrowserLiveLocked() would buy at most 60 seconds of
	// refresh availability and re-open it.
	if s.setupInProgressLocked() {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — setup in progress")
		return refreshDeclined(), nil
	}
	if s.refreshCmd != nil {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — already refreshing")
		return refreshDeclined(), nil
	}
	s.refreshCmd = &exec.Cmd{} // sentinel to claim slot
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.refreshCmd = nil
		s.mu.Unlock()
	}()

	// refreshBrowser, not resolvedBrowser: this is the one site where
	// cookies.auto_enabled reaches a refresh, and it reaches it by answering
	// nil rather than by refusing the pass. See BrowserLaunchAllowed.
	browser := s.refreshBrowser(policy)

	if _, err := statProfileDir(s.profileDir); os.IsNotExist(err) {
		// Neither a browser to drive nor a profile to import from: the
		// install genuinely has no cookie source, so keep the historical
		// answer and the "install a supported browser" UI copy that hangs
		// off it.
		if browser == nil {
			// Two ways to arrive with no browser, and they do not share a
			// remedy. The gate above can drop a browser that is installed and
			// working, and "no browser found" would send that operator to
			// install a second copy of one they already have — the unearned
			// cause this arc exists to stop. Same sentinel either way, because
			// every consumer's branch is genuinely the same (there is no
			// browser to use); different sentence, because the reader's next
			// action is not.
			if s.browserLaunchBlocked(policy) {
				disabled := fmt.Errorf(
					"cookies.auto_enabled is false so no headless browser was launched, and there is no "+
						"browser profile at %s to import from (%w for this pass)",
					s.profileDir, ErrNoBrowserFound)
				s.setError(disabled.Error())
				return refreshDeclined(), disabled
			}
			// Wrapped rather than bare, and symmetrically with the arm above,
			// because the Web route renders this message verbatim now — the
			// static sentence it used to substitute ("no supported browser
			// installed") is a claim only ONE of these two arms can support.
			// Both now say which of the two cookie sources was missing and why.
			missing := fmt.Errorf("%w, and there is no browser profile at %s to import from",
				ErrNoBrowserFound, s.profileDir)
			s.setError("no browser found for refresh, and no browser profile to import from")
			return refreshDeclined(), missing
		}
		s.setError("browser profile not found — run setup first")
		return refreshDeclined(), fmt.Errorf("run setup first: %w", ErrProfileNotFound)
	}

	// importedFromProfile selects the browser-free path: no browser is
	// installed (a container, or a headless host) or none may be launched
	// (cookies.auto_enabled is off), but the configured profile directory is
	// present and may hold a readable cookies.sqlite.
	// Before this branch existed, RefreshCookies bailed on `browser == nil`
	// BEFORE it ever looked at the profile — so a perfectly readable
	// mounted profile was refused on a technicality.
	//
	// The disabled case is the same shape and lands here for the same reason:
	// an operator who hand-updates their browser profile presses R F to have it
	// read, and launching nothing is precisely what they want.
	importedFromProfile := browser == nil

	var netscapeCookies string
	var err error
	// browserActed answers the question this function used to get wrong: did
	// the browser we launched actually DO anything?
	//
	// Success here is measured against cookies.txt, which the independent
	// 30-minute RefreshService keeps alive regardless — so a refresh whose
	// browser was killed mid-load, or never started, still verified and still
	// logged "cookie refresh succeeded".
	//
	// It starts TRUE and only the browser branch below can clear it. That is
	// the scoping, not an oversight: the import path and the empty-profile
	// fallback never launch a browser, so there is no browser whose inaction
	// could be detected, and gating them on this would make every
	// containerised profile import report a refresh that never renews —
	// permanently, on every restart.
	browserActed := true
	// emptyBrowserProfile records that a browser refresh read a profile with
	// no relevant cookies in it. Not fatal on its own (see below), but it is
	// the explanation the operator needs if auth then fails to verify.
	emptyBrowserProfile := false

	if importedFromProfile {
		// No refreshPlatforms() gate here. That gate asks "does the jar
		// already hold cookies worth re-fetching?", which is the right
		// question for a browser refresh and the wrong one for an import:
		// seeding a container that has no cookies.txt yet is the primary
		// use case.
		netscapeCookies, err = s.importProfileCookies()
		if err != nil {
			s.setError(err.Error())
			s.logger.Warn("browser-profile cookie import failed", "profile_dir", s.profileDir, "err", err)
			return refreshAborted(), err
		}
	} else {
		if len(s.refreshPlatforms()) == 0 {
			s.logger.Debug("skipping cookie refresh — no platforms have cookies")
			return refreshDeclined(), nil
		}

		s.logger.Info("refreshing cookies via " + browser.Type)

		if isFirefoxBased(browser.Type) {
			netscapeCookies, browserActed, err = s.refreshFirefox(ctx, browser)
		} else {
			// Chromium needs no screenshot: the navigations are driven over
			// CDP, so each one reports its own outcome and refreshChromium
			// ANDs them, exactly as refreshFirefox ANDs its per-launch
			// verdicts. It is a weaker signal than the Firefox screenshot —
			// "no navigation reported a transport failure", see
			// refreshChromium — but it is the one this path can produce.
			//
			// The READ error alone is not it, which is what this used to take
			// it for. That error comes from cdpGetCookiesAsNetscape, and the
			// read is satisfied by a profile the previous session already
			// populated — so every navigation could have failed and the pass
			// would still have claimed it renewed the credentials. Both halves
			// are required.
			var navigated bool
			netscapeCookies, navigated, err = refreshChromiumCookies(s, ctx, browser)
			browserActed = err == nil && navigated
		}
	}

	// DPAPI fallback: if the CDP path failed and the user has opted
	// in, try reading cookies directly from their real Chromium-family
	// profile via CryptUnprotectData. Skipped for Firefox-based
	// browsers — Firefox uses cookies.sqlite (no DPAPI involved) and
	// already has its own SQLite-direct path. DECISIONS #6.
	if err != nil && browser != nil && s.DpapiFallback && !isFirefoxBased(browser.Type) {
		s.logger.Warn("CDP refresh failed; attempting DPAPI fallback", "cdp_err", err)
		// H7: the configured browser OVERRIDE, not the resolved/auto-detected
		// `browser` above — an operator who explicitly named a browser in
		// settings gets DPAPI restricted to it; auto-detect leaves every
		// Chromium-family profile as a candidate for dpapiExtractAsNetscape's
		// own selection (it picks exactly one, it never merges). Gated by
		// browserOverrideConfigured — the SAME predicate resolvedBrowser
		// uses — so a browser_type set with no path (Finding 3, Arc 8 fix
		// round 1: reachable from the TUI's free-text field) is "no
		// override" here too, not a hard filter on a half-configured value.
		var cfgBrowserType string
		if s.ConfiguredBrowserOverride != nil {
			if path, btype := s.ConfiguredBrowserOverride(); browserOverrideConfigured(path, btype) {
				cfgBrowserType = btype
			}
		}
		fallbackCookies, fallbackErr := dpapiExtractAsNetscape(s.logger, cfgBrowserType)
		if fallbackErr != nil {
			s.logger.Warn("DPAPI fallback also failed; surfacing original CDP error",
				"dpapi_err", fallbackErr)
			// fall through with the original CDP err
		} else {
			s.logger.Info("DPAPI fallback succeeded; using user's signed-in browser cookies")
			netscapeCookies = fallbackCookies
			err = nil
			// The headless launch failed, but this pass did bring fresh
			// credential material in — read out of the user's real,
			// signed-in browser profile rather than recycled from
			// cookies.txt. That is what browserActed asks about, so the
			// failed launch does not veto it.
			browserActed = true
		}
	}

	// An empty profile is a hard error for the IMPORT path, where it means the
	// read is broken. That path has already returned above on any error, so the
	// !importedFromProfile guard is belt-and-braces — it also keeps browser
	// non-nil for the log line.
	//
	// On the BROWSER path it has a mundane explanation — a browser set to clear
	// cookies on close leaves the profile empty every time — and before this
	// package that produced a no-op merge and a refresh that succeeded off the
	// still-good cookies.txt. Failing here instead would fire "Cookie
	// Auto-Refresh Failed — recordings will fail" at a user whose recordings are
	// fine.
	//
	// So: contribute nothing, let verification below decide, and remember the
	// fact so it is named if the existing cookies turn out to be dead too. That
	// keeps the desktop behaviour while refusing to let a browser that silently
	// stopped saving cookies masquerade as an ordinary expiry.
	//
	// BOTH FAMILIES. This used to sit inside the Firefox arm of the if/else
	// above, so the identical Chromium state — cdpGetCookiesAsNetscape can now
	// tell an empty profile from a failed read and says ErrNoCookiesInProfile
	// for it — aborted the refresh instead.
	//
	// Placed AFTER the DPAPI fallback rather than immediately after the if/else,
	// and that ordering is load-bearing on Windows: an empty headless profile is
	// exactly when reading the user's real signed-in profile is worth trying, so
	// the downgrade must not consume the error before the fallback sees it.
	// Firefox is unaffected either way — the fallback skips that family.
	//
	// browserActed is deliberately left as the branches set it. This pass
	// contributed no credentials, so whatever verifies below was already on
	// disk, and Renewed must not claim otherwise.
	if !importedFromProfile && errors.Is(err, ErrNoCookiesInProfile) {
		s.logger.Warn("browser refresh produced no cookies — falling back to the existing cookies.txt",
			"browser", browser.Type, "profile_dir", s.profileDir, "err", err)
		emptyBrowserProfile = true
		netscapeCookies, err = "", nil
	}

	if err != nil {
		s.setError(err.Error())
		return refreshAborted(), err
	}

	// Merge with existing cookies using temp file + rename for atomicity.
	// previousCookies is kept verbatim so an import that turns out to have
	// damaged a platform can hand that platform's rows back untouched.
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		return refreshAborted(), err
	}
	var previousCookies string
	existingData, readErr := readCookieFile(s.cookiePath)
	switch {
	case readErr == nil:
		if len(existingData) > 0 {
			previousCookies = string(existingData)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		// No cookies.txt yet — nothing to merge or protect via rollback.
	default:
		// This has to abort BEFORE previousCookies is used for anything:
		// it gates both the merge below and, on the import path, whether
		// the pre-import verification that makes rollback possible even
		// runs at all (`importedFromProfile && previousCookies != ""`
		// further down). Silently treating a transient read failure as
		// "no existing file" would leave previousCookies empty, which
		// both disables that rollback AND lets the write below replace
		// cookies.txt with only the newly-fetched cookies — losing
		// whatever the other platform had. Abort instead: don't merge,
		// don't write, don't touch the rollback gate.
		//
		// Wraps ErrCookieFileUnreadable so callers can tell this apart from
		// every other refresh failure — see the sentinel's doc comment for
		// why that distinction has to survive to the operator: the file was
		// deliberately left untouched, and must not be the thing they are
		// told to replace.
		mergeErr := fmt.Errorf("%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			ErrCookieFileUnreadable, readErr)
		s.setError(mergeErr.Error())
		s.logger.Error("cookie refresh: aborting rather than overwrite cookies.txt after a read failure",
			"path", s.cookiePath, "err", readErr)
		return refreshAborted(), mergeErr
	}

	// Verify BEFORE overwriting, on the import path only. Rolling back a
	// regression is impossible without knowing what worked beforehand, and
	// "the file had cookies" is not the same as "those cookies worked".
	// Skipped when there is nothing to protect, so the common container case
	// (no cookies.txt yet) costs no extra round trips.
	pre := map[string]platformAuth{}
	if importedFromProfile && previousCookies != "" {
		if loadErr := s.jar.Load(s.cookiePath); loadErr != nil {
			s.logger.Warn("could not load existing cookies before import — rollback protection is off", "err", loadErr)
		}
		preYT, preTW := s.checkPlatformAuth(ctx)
		pre["youtube"], pre["twitch"] = preYT, preTW
	}

	// What we believed we held going in, and what this pass actually brought
	// back. Both are read BEFORE the write, because afterwards the only thing
	// left to look at is the merged result — and the whole point below is to
	// tell "we lost what we had" apart from "there was never anything here".
	//
	// The jar is the right source for the first pair: it is what
	// refreshPlatforms() gated on, and the disagreement between the jar
	// (which ignores expiry) and mergeCookieFiles (which prunes on it) is
	// precisely how a refresh can end up writing an empty file.
	//
	// Same predicate as refreshPlatforms() and checkPlatformAuth, so `lost`
	// below compares like with like. It also makes the sentence
	// cookiesLostMessage prints true: "nothing is left to authenticate with"
	// describes a platform that went from some credential to none, which is
	// what these now measure. Under the complete-set predicate a full set
	// degrading to a partial one was reported as a total loss (false), and a
	// partial set vanishing entirely was not reported at all (silent).
	hadYTAuth := s.jar.HasAnyYouTubeAuthCookie()
	hadTWAuth := s.jar.HasAnyTwitchAuthCookie()
	fetchedRows := countNetscapeCookieRows(netscapeCookies)
	// fetchedNoCredential is the state the outcome switch at the bottom of this
	// function could not name: rows came back, and NOT ONE of them is a session
	// credential. A browser profile that is signed out, or one set to clear
	// cookies on exit and re-seeded with YSC/VISITOR_INFO1_LIVE by the
	// navigation this pass just made, lands here every time.
	//
	// Read as either of the two nearest existing cases it was wrong: "the
	// browser profile contained no cookies" is FALSE (rows came back — that arm
	// is emptyBrowserProfile, which is only set when the read produced nothing
	// at all), and "auth verification failed — manual re-login required" is true
	// but says nothing an operator can act on, because the thing to fix is that
	// the browser is not signed in rather than that Moombox's check failed.
	//
	// A NEW FLAG, never a redefinition of fetchedRows. Overloading `fetchedRows
	// == 0` would put this state inside the counter, whose deliberate
	// over-counting is load-bearing for the import guard and is mutation-pinned;
	// and reusing emptyBrowserProfile would make "the profile was empty" mean
	// two different things one line apart.
	//
	// Measured on what THIS PASS FETCHED, before the merge below folds the
	// previous cookies.txt in. After the merge the answer would be about the
	// file rather than about the browser, and the file's credentials are exactly
	// the ones this state is not a statement about.
	fetchedNoCredential := fetchedRows > 0 && !netscapeCookiesHoldACredential(netscapeCookies)

	if previousCookies != "" {
		netscapeCookies = mergeCookieFiles(previousCookies, netscapeCookies)
	}
	if err := writeCookieFile(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		return refreshAborted(), err
	}

	// Reload jar
	if err := s.jar.Load(s.cookiePath); err != nil {
		return refreshAborted(), err
	}

	// Verify auth via API callbacks (matches TypeScript refreshCookies behavior)
	postYT, postTW := s.checkPlatformAuth(ctx)

	// Roll back, per platform, an import that made that platform worse.
	//
	// A mounted profile can be STALE, and mergeCookieFiles lets the imported
	// value win by name+domain+path — so a dead Twitch token in the profile
	// overwrites a working one on disk. Judging the import as a WHOLE hides
	// exactly that: a healthy YouTube result masks the Twitch loss, the
	// refresh reports success, and the working credential is gone. The
	// startup one-shot would then repeat it on every restart.
	//
	// Only the import path does this. The browser path writes cookies it just
	// re-fetched from the live site, which cannot be staler than what was on
	// disk.
	//
	// importCheck is kept under its own name because the rollback branch below
	// REPLACES postYT/postTW with a re-verification of what was restored. The
	// question "why did we reject the import" can only be answered by the check
	// that rejected it, and after the re-verify that check is no longer in
	// scope. See rollbackWasInconclusive.
	importCheck := map[string]platformAuth{"youtube": postYT, "twitch": postTW}
	var restoredPlatforms []string
	if restore := platformsToRestore(pre, importCheck); len(restore) > 0 {
		for _, platform := range []string{"youtube", "twitch"} {
			if restore[platform] {
				restoredPlatforms = append(restoredPlatforms, platform)
			}
		}
		s.logger.Warn("imported profile cookies did not hold up — restoring the previous credentials for those platforms",
			"platforms", strings.Join(restoredPlatforms, ","), "profile_dir", s.profileDir)

		restored := restorePlatformRows(netscapeCookies, previousCookies, restore)

		// A rollback that does not land must not be reported as one. Both
		// failures below leave the process describing a jar that is not what
		// is on disk, and the status built at the bottom of this function
		// would go on to say "kept the previous cookies for X" — while the
		// rejected import is the file the next download actually uses. Worse,
		// a sibling platform that verified would carry the whole call to
		// "refresh succeeded".
		//
		// So they end the refresh instead, with a message describing the
		// state that really exists. Failing the call matches how every other
		// write failure in this function is handled.
		if restoreErr := writeCookieFile(s.cookiePath, []byte(restored), 0o600); restoreErr != nil {
			errMsg := "the browser profile did not verify for " + strings.Join(restoredPlatforms, " + ") +
				", and Moombox could not restore the previous cookies (" + restoreErr.Error() +
				") — cookies.txt still holds the rejected imported credentials"
			s.setError(errMsg)
			s.logger.Error("could not restore pre-import cookies.txt",
				"err", restoreErr, "platforms", strings.Join(restoredPlatforms, ","))
			return refreshAborted(), fmt.Errorf("restore pre-import cookies: %w", restoreErr)
		}
		if loadErr := s.jar.Load(s.cookiePath); loadErr != nil {
			// The FILE is correct here; the running process is not. Saying
			// "kept the previous cookies" would be true of the disk and false
			// of everything using the jar until the next successful load.
			errMsg := "restored the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
				" after the browser profile did not verify, but reloading them failed (" + loadErr.Error() +
				") — this process is still using the rejected credentials until the next refresh"
			s.setError(errMsg)
			s.logger.Error("could not reload cookie jar after restoring pre-import cookies.txt",
				"err", loadErr, "platforms", strings.Join(restoredPlatforms, ","))
			return refreshAborted(), fmt.Errorf("reload cookie jar after restore: %w", loadErr)
		}

		// Re-verify the file we actually kept. Without this, the status
		// below would describe the DISCARDED merged jar and flag a
		// re-login for credentials that were restored and never
		// re-checked — an instruction a container operator cannot even
		// act on.
		postYT, postTW = s.checkPlatformAuth(ctx)
	}

	ytAuth := postYT.ok()
	twAuth := postTW.ok()

	// The per-platform answer, fixed here because postYT/postTW are final from
	// this point on (the rollback branch above is the last thing that can
	// re-verify). Every remaining exit reports THIS — the three of them differ
	// in what they log and record, not in what they concluded.
	//
	// hasCookies travels WITH the verdict rather than being re-read from the
	// jar, so the two can never describe different moments: a rollback
	// re-verifies, and a presence bit sampled before it would belong to the
	// discarded import. renewed rides along for the same reason — the gates
	// further down consume it, and a UI caller that has to re-derive it would
	// be re-deriving it from information it does not have.
	//
	// renewed says whether this pass actually produced the credentials it is
	// about to be judged on, as opposed to finding the previous ones still
	// alive. The import and empty-profile paths always did (they read a
	// profile); the browser path only did if the browser ran.
	//
	// Written as an explicit `importedFromProfile ||` rather than leaning on
	// browserActed's initialiser so the scoping is visible at the point of
	// use: an earlier draft of this change gated success on the profile
	// database's mtime and would have made every containerised import report
	// failure forever.
	renewed := importedFromProfile || browserActed
	result := RefreshResult{
		Ran:           true,
		Renewed:       renewed,
		YouTube:       verdictOf(postYT),
		YouTubeStored: postYT.hasCookies,
		Twitch:        verdictOf(postTW),
		TwitchStored:  postTW.hasCookies,
	}

	// Update re-login flags based on verification results. Only a CONCLUSIVE
	// failure flags a re-login: an unreachable network told us nothing about
	// the credentials, and sending the user to sign in again over a blip is
	// both wrong and, in a container, impossible to act on.
	//
	// Taken from the verdicts rather than re-read from the jar, as the comment
	// above result says: the presence bit has to describe the same moment and
	// the same question as the state it is paired with. Re-reading it strictly
	// here silently dropped the half-cleared platform out of both `failed` and
	// the re-login flag, so a session YouTube had conclusively rejected
	// produced no prompt and no targeted message.
	ytHasCookies := postYT.hasCookies
	twHasCookies := postTW.hasCookies

	// Platforms that HAD auth cookies going into this refresh and do not
	// have them coming out. This is per platform on purpose: the jar ignores
	// expiry while mergeCookieFiles prunes on it, so one platform's rows can
	// vanish while the other's survive — and a sibling that verifies would
	// otherwise carry the whole call to "refresh succeeded" over a
	// credential that just disappeared. A platform the import legitimately
	// REPLACED still has auth in the reloaded jar, and the rollback above
	// puts previous rows back before this point, so neither of those reads
	// as a loss.
	var lost []string
	if hadYTAuth && !ytHasCookies {
		lost = append(lost, "YouTube")
	}
	if hadTWAuth && !twHasCookies {
		lost = append(lost, "Twitch")
	}

	s.mu.Lock()
	if postYT.state == verifyFailed && ytHasCookies {
		s.needsRelogin["youtube"] = true
	}
	if postTW.state == verifyFailed && twHasCookies {
		s.needsRelogin["twitch"] = true
	}
	if ytAuth {
		s.needsRelogin["youtube"] = false
	}
	if twAuth {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	if postYT.state == verifyFailed && ytHasCookies {
		s.logger.Warn("YouTube auth verification failed after refresh — manual re-login required")
	}
	if postTW.state == verifyFailed && twHasCookies {
		s.logger.Warn("Twitch auth verification failed after refresh — manual re-login required")
	}

	// Consider refresh successful if any platform verified
	if ytAuth || twAuth {
		now := time.Now()
		// One platform verifying does not license clearing the status over
		// another platform's credentials disappearing. Success here is
		// partial, and the part that was lost is the part nobody would
		// otherwise find out about.
		lostMsg := ""
		if len(lost) > 0 {
			lostMsg = cookiesLostMessage(lost)
		}
		s.mu.Lock()
		if renewed {
			// Withheld when the browser did nothing. lastRefresh is what
			// shouldSkipPeriodicRefresh consults and what the settings page
			// prints as "Last refresh"; stamping it for a pass that renewed
			// nothing would both suppress the NEXT attempt (interval/2) and
			// tell the user their credentials are fresher than they are.
			s.lastRefresh = &now
		}
		switch {
		case lostMsg != "":
			// A loss is something THIS pass observed, so it is recorded
			// whether or not the pass renewed anything.
			s.lastError = &lostMsg
		case renewed:
			s.lastError = nil
		default:
			// Withheld for the same reason lastRefresh is. Clearing lastError
			// is an assertion — "whatever was wrong is not wrong any more" —
			// and this pass has no basis for it. What it established is that
			// the credentials ON DISK verify; what it could not establish is
			// that the refresh mechanism works, which is exactly what a
			// previously recorded error may have been about ("the browser
			// profile contained no cookies to refresh from — check whether the
			// browser is clearing cookies on exit"). Retracting that report
			// off a pass whose browser did nothing is how a twice-broken
			// refresh presents a clean bill of health.
			//
			// Nothing is set here either: the credentials verify, so the
			// Settings error field — which reads as "your recordings will
			// fail" — would be alarming a user whose recordings are fine.
			// The honest signals for this case are a lastRefresh that stays
			// stale and the Warn logged below.
		}
		s.mu.Unlock()

		// Persist LastRefresh to the sidecar (audit cookies.md #48).
		persistedPlatforms := []string{}
		if ytAuth {
			persistedPlatforms = append(persistedPlatforms, "youtube")
		}
		if twAuth {
			persistedPlatforms = append(persistedPlatforms, "twitch")
		}
		// Same reason as lastRefresh above: the sidecar is the copy that
		// survives a restart, so writing a timestamp for a refresh that never
		// ran makes the lie durable.
		if renewed {
			if metaErr := SaveMeta(s.cookiePath, CookieMeta{
				LastRefresh: now,
				Platforms:   persistedPlatforms,
			}); metaErr != nil && s.logger != nil {
				s.logger.Warn("could not persist cookies.meta.json", "err", metaErr)
			}
		}

		var verified []string
		if ytAuth {
			verified = append(verified, "YouTube")
		}
		if twAuth {
			verified = append(verified, "Twitch")
		}
		switch {
		case lostMsg != "":
			s.logger.Warn("cookie refresh verified one platform and lost another",
				"verified", strings.Join(verified, " + "), "lost", strings.Join(lost, ","), "detail", lostMsg)
		case !renewed:
			// The credentials on disk verify, but nothing here established that
			// THIS pass produced them: they may be the same ones that were
			// already there, kept alive by the independent 30-minute
			// RefreshService. Calling that "cookie refresh succeeded" is the
			// claim this branch exists to stop making — it is how a
			// Firefox-family refresh that did nothing at all reported success
			// on every run for the life of the feature.
			//
			// The wording stops at "could not confirm" on purpose. Naming a
			// mechanism ("the browser never completed a page load") would be
			// wrong in the partial case — with two platforms, one browser can
			// genuinely have rendered while the other did not, and the verdict
			// is an AND — and unprovable wherever there is no Job Object to
			// drain. Replacing one unearned claim with its mirror image is not
			// the fix.
			//
			// Still `return true`: the caller asked whether authenticated
			// requests will work, and they will. What changes is that nothing
			// here credits this pass for it.
			s.logger.Warn("cookies still verify, but this pass could not confirm the browser refreshed the profile",
				"verified", strings.Join(verified, " + "))
		default:
			s.logger.Info("cookie refresh succeeded", "verified", strings.Join(verified, " + "))
		}
		return result, nil
	}

	// Neither platform verified. Build a targeted message naming only the
	// platforms that actually had cookies worth verifying — if a user only
	// signed in to YouTube, they should not see "Twitch needs re-login".
	var failed []string
	if ytHasCookies {
		failed = append(failed, "YouTube")
	}
	if twHasCookies {
		failed = append(failed, "Twitch")
	}
	if len(failed) == 0 {
		// Execution is PAST writeFileAtomic, so whatever sits in cookies.txt
		// now is what this pass produced — and it authenticates neither
		// platform. That is three different situations wearing one face, and
		// clearing lastError for all of them (the old behaviour) is only
		// right for the third.
		switch {
		case len(lost) > 0:
			// We HAD credentials and now the file has none. The usual cause
			// is the disagreement noted above: the jar ignores expiry, the
			// merge prunes on it, so every stored row can be dropped by a
			// refresh that thought it had something to refresh. Whatever the
			// cause, an empty credential file must never be reported as a
			// clean no-op.
			errMsg := cookiesLostMessage(lost)
			s.setError(errMsg)
			s.logger.Warn("cookie refresh left no auth cookies on disk",
				"platforms", strings.Join(lost, ","), "detail", errMsg)
		case fetchedRows > 0:
			// Nothing was lost — there was nothing to lose — but this pass
			// did write cookies, and none of them authenticate anything. The
			// container case: a mounted profile that is not signed in, or
			// one whose saved logins have since lapsed. Saying nothing here
			// makes a useless mount look like a working one.
			errMsg := "cookies were read but none of them authenticate YouTube or Twitch — " +
				"the browser profile is not signed in to either platform, or its saved logins have expired"
			s.setError(errMsg)
			s.logger.Warn("cookie refresh produced no auth cookies", "rows", fetchedRows, "detail", errMsg)
		default:
			// Genuinely nothing to do and nothing lost (e.g. first run before
			// setup). The refresh completed cleanly; there was just nothing
			// to refresh yet.
			//
			// NO ROUTE TO HERE HAS BEEN FOUND, as of Arc 8 Task 12a. Reaching it
			// needs `failed` and `lost` both empty with fetchedRows == 0, i.e.
			// a pass that fetched nothing while neither platform had a
			// credential going in. The only path that fetches nothing is the
			// ErrNoCookiesInProfile downgrade above, which lives on the browser
			// branch — and that branch is gated on refreshPlatforms() being
			// non-empty, which is the same pair of loose predicates hadYTAuth /
			// hadTWAuth are read from. The import branch has no such gate but
			// cannot fetch nothing: importProfileCookies raises
			// ErrNoCookiesInProfile rather than returning an empty blob.
			//
			// Kept, with the derivation written down, rather than deleted:
			// "I could not find a route" is not "there is none", and the arm is
			// the right behaviour for the state it describes. If a future change
			// does open a route, note that this is a CLEAR — see the write
			// policy on the lastError field for why clears are the dangerous
			// half — and it may only stay correct while the state really is
			// "nothing happened and nothing was lost".
			s.logger.Debug("cookie refresh completed with no cookies to verify")
			s.mu.Lock()
			s.lastError = nil
			s.mu.Unlock()
		}
		return result, nil
	}
	// Say what actually happened. "Manual re-login required" is the right
	// advice only when the credentials were conclusively rejected; when the
	// checks could not reach the network, or when we kept the previous
	// credentials rather than the import, that message sends the operator
	// after the wrong problem.
	//
	// A rollback and an inconclusive check can be true at once — in fact the
	// pure-network case is exactly that, since a check that cannot complete
	// is itself a reason to keep the previous credentials. Blaming the
	// profile there would send a container operator off to re-export one
	// that is perfectly fine, which is the misattribution verifyUnknown
	// exists to prevent. So that combination gets its own message carrying
	// both facts: what we kept, and why.
	//
	// Two different questions, so two different sources. `inconclusive`
	// describes the credentials in force NOW, which after a rollback are the
	// restored ones — that is the right input for the no-rollback branches
	// below. rollbackWasInconclusive describes why the IMPORT was rejected, and
	// only the check that rejected it can answer that: the re-verification
	// overwrote postYT/postTW, so reading them would attribute the rollback to
	// a check performed afterwards on different cookies. When the restored
	// credentials then verify conclusively-false — the ordinary outcome once a
	// dead-but-configured platform can reach arm 2 at all — that misattribution
	// prints "the mounted browser profile did not verify" about a profile that
	// was never evaluated.
	inconclusive := postYT.state == verifyUnknown || postTW.state == verifyUnknown
	rollbackWasInconclusive := false
	for _, platform := range restoredPlatforms {
		if importCheck[platform].state == verifyUnknown {
			rollbackWasInconclusive = true
		}
	}
	// rollbackHedge is the (network?) hedge's replacement across whichever
	// restored platforms are inconclusive. combinedInconclusiveHedge folds
	// them to ONE hedge when they agree (the common case: at most one
	// platform is usually restored at all) and to a per-platform breakdown
	// when they do not — see its doc for why collapsing a disagreement to
	// one hedge would assert a cause about a platform the code knows is
	// false. Reviewer round 1 finding 1 caught the previous AND/OR
	// tie-break doing exactly that for this arm's twin below; this arm
	// shares the same fix rather than getting its own tie-break.
	rollbackHedge, _ := combinedInconclusiveHedge(restoredPlatforms, importCheck)
	// inconclusiveHedge/inconclusiveAgree is rollbackHedge's twin for the
	// no-rollback arm below, over postYT/postTW instead of importCheck.
	inconclusiveHedge, inconclusiveAgree := combinedInconclusiveHedge(
		[]string{"youtube", "twitch"},
		map[string]platformAuth{"youtube": postYT, "twitch": postTW})
	var errMsg string
	switch {
	case len(restoredPlatforms) > 0 && rollbackWasInconclusive:
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — " + rollbackHedge + ", so the imported profile was not accepted"
	case len(restoredPlatforms) > 0:
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — the mounted browser profile did not verify"
	case inconclusive && inconclusiveAgree:
		// Single hedge, one sentence — the shape every existing test and
		// every single-platform (the overwhelmingly common) inconclusive
		// check already expects.
		errMsg = strings.Join(failed, " + ") + " auth could not be verified — " + inconclusiveHedge
	case inconclusive:
		// The platforms disagree on why, so the "<platforms> auth could not
		// be verified —" lead-in is dropped rather than paired with a
		// per-platform breakdown that already names each platform: the
		// combined hedge IS the message.
		errMsg = inconclusiveHedge
	case emptyBrowserProfile:
		errMsg = strings.Join(failed, " + ") + " auth verification failed, and the browser profile contained " +
			"no cookies to refresh from — check whether the browser is clearing cookies on exit"
	case fetchedNoCredential:
		// PLACED HERE, immediately above default, and the position is the
		// constraint rather than an aesthetic choice: this case carves its state
		// out of `default` and out of nothing else.
		//
		// It cannot overlap emptyBrowserProfile above — that arm is only set on
		// a read that produced no text at all, so fetchedRows is 0 there and
		// this flag is false. It is kept BELOW the two inconclusive arms
		// deliberately: "the browser is signed out" is a strong claim about the
		// profile, and a check that could not reach the site has not earned it.
		// Moving it up would silently change what those arms cover, which is
		// exactly what this case was forbidden from doing.
		//
		// NAMES THE PLATFORMS, like every sibling arm. It did not, and was the
		// only arm in this switch that did not: with two platforms configured
		// and one of them failing, a message that opens on the browser leaves
		// the operator to guess which session the verdict is about — and this
		// arm is reachable in exactly that mixed state.
		errMsg = fmt.Sprintf("%s auth verification failed, and the browser profile returned %d "+
			"cookies but none of them is a session credential — the browser is signed out",
			strings.Join(failed, " + "), fetchedRows)
	default:
		errMsg = strings.Join(failed, " + ") + " auth verification failed — manual re-login required"
	}
	// A platform can be LOST while another is merely rejected, and the
	// rejection message would otherwise be the only thing said — naming the
	// surviving platform's problem while the other one's credentials
	// silently left the file.
	if len(lost) > 0 {
		errMsg = cookiesLostMessage(lost) + ". " + errMsg
	}
	s.setError(errMsg)
	s.logger.Warn("refresh completed but auth verification failed",
		"platforms", strings.Join(failed, ","), "lost", strings.Join(lost, ","), "detail", errMsg)
	return result, nil
}

// platformDisplayName maps the lowercase platform keys used internally in
// this file (restoredPlatforms, importCheck, the map literal
// combinedInconclusiveHedge is called with) to the capitalized names
// `failed`/`lost` already render to the operator.
func platformDisplayName(platform string) string {
	switch platform {
	case "youtube":
		return "YouTube"
	case "twitch":
		return "Twitch"
	default:
		return platform
	}
}

// inconclusiveHedge renders the (network?) hedge's replacement wording for
// ONE platform's inconclusive check, disambiguated by attempted — see
// platformAuth's doc for what the two halves of verifyUnknown mean.
func inconclusiveHedge(p platformAuth) string {
	if p.attempted {
		// A request went out and came back unusable — network, an
		// intermediary, a timeout. Say that; no question mark, because this
		// is no longer a guess.
		return "the auth check did not complete"
	}
	// No request was ever attempted — the extracted cookies could not form
	// one. Same wording the Warn at checkPlatformAuth's caller (FinishSetup)
	// already uses for the same fact — see attempted's doc on platformAuth —
	// minus the "during setup" qualifier, which does not apply to a refresh
	// pass.
	return "the auth check was never attempted — the extracted cookies cannot form an authenticated request"
}

// combinedInconclusiveHedge renders the (network?) hedge across every
// platform in order whose check in checks landed on verifyUnknown.
//
// When every one of them agrees on attempted — true for a single inconclusive
// platform, which is the overwhelmingly common shape — it returns ONE hedge
// and allAgree=true, the joined-sentence form callers had before this
// existed. When they disagree — one platform's check went out and came back
// unusable while the other's cookies could never form a request at all —
// collapsing them to a single hedge asserts a cause about a platform the
// code knows is false (reviewer round 1, finding 1, caught this for the
// AND/OR tie-break this function replaced). allAgree=false then, and hedge
// is a full "Platform: cause; Platform: cause" breakdown naming each one, not
// a fragment meant to be embedded after a shared lead-in — see the two call
// sites for how each folds allAgree=false into its own sentence.
//
// order fixes iteration to a stable sequence (callers pass
// []string{"youtube", "twitch"} or restoredPlatforms, which is already built
// in that order) so the rendered message is deterministic.
func combinedInconclusiveHedge(order []string, checks map[string]platformAuth) (hedge string, allAgree bool) {
	var names []string
	var pairs []platformAuth
	for _, platform := range order {
		p, ok := checks[platform]
		if !ok || p.state != verifyUnknown {
			continue
		}
		names = append(names, platform)
		pairs = append(pairs, p)
	}
	if len(pairs) == 0 {
		return "", true
	}
	agreedHedge := inconclusiveHedge(pairs[0])
	allAgree = true
	for _, p := range pairs[1:] {
		if inconclusiveHedge(p) != agreedHedge {
			allAgree = false
			break
		}
	}
	if allAgree {
		return agreedHedge, true
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = platformDisplayName(names[i]) + ": " + inconclusiveHedge(p)
	}
	return strings.Join(parts, "; "), false
}

// cookiesLostMessage names the platforms whose credentials were on disk
// before a refresh and are not on disk after it. One wording for every exit
// that can observe the loss, so the operator sees the same sentence whether
// the sibling platform verified, was rejected, or was never there.
func cookiesLostMessage(lost []string) string {
	return strings.Join(lost, " + ") + " cookies are gone from cookies.txt after this refresh — " +
		"every stored credential had expired or was dropped, so nothing is left to authenticate with; sign in again"
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

// shouldSkipPeriodicRefresh reports whether the next periodic-refresh tick
// should be skipped because no active jobs exist. When HasActiveJobs is nil
// the service refreshes on every tick (legacy behaviour). Audit
// reports/cookies.md #23.
// shouldSkipPeriodicRefresh decides whether the periodic ticker should fire.
// Two skip conditions:
//
//  1. No active jobs (existing) — when nothing is downloading or live, an
//     auth-token refresh isn't urgent. Headless Chrome launch is ~1-5s of
//     CPU + ~150MB RAM; not worth it for an idle session.
//  2. Recent successful refresh (audit reports/cookies.md #23) — if we
//     refreshed within `interval/2`, the next tick is too close to be
//     useful. Trips when the user just-now used "refresh now" or a job-
//     level recovery path triggered a refresh between ticks.
//
// Either condition skips the tick.
func (s *AutoCookieService) shouldSkipPeriodicRefresh(interval time.Duration) bool {
	if s.HasActiveJobs != nil && !s.HasActiveJobs() {
		return true
	}
	s.mu.Lock()
	last := s.lastRefresh
	s.mu.Unlock()
	if last != nil && time.Since(*last) < interval/2 {
		return true
	}
	return false
}

// periodicRefreshHasSource reports whether a periodic tick has anything to work
// with: the browser profile directory must exist. Both refresh mechanisms need
// it — the headless browser is launched against it, and the browser-free import
// reads out of it — so RefreshCookiesDetailed returns without doing anything
// useful when it is absent.
//
// This used to be answered ONCE, by an os.Stat in main.go, before the periodic
// goroutine was allowed to start at all. That silently punished the operator it
// was meant to serve: turn cookies.auto_enabled on, complete setup — which is
// what CREATES the directory — and the timer that the setting exists to start
// stayed unstarted until the next restart, with nothing saying so. No setting
// had changed, so even the restart-required labelling never fired.
//
// Asked per tick instead, so a setup completed at runtime is picked up by the
// next one. The other side of that trade is the reason this is a quiet skip
// rather than a pass that fails: on a flag-on install where setup has never
// been run, a real pass would call setError("browser profile not found — run
// setup first") and log a warning on every interval forever, putting a
// permanent error on the settings page for a state the operator has not been
// asked to fix yet.
func (s *AutoCookieService) periodicRefreshHasSource() bool {
	_, err := statProfileDir(s.profileDir)
	return err == nil
}

// notePassCompleted fires OnPassCompleted if one is wired.
//
// A named method rather than an inline nil check so the decision has a seam a
// test can drive: the tick that calls it needs a browser profile, a browser
// and a network, so the branch is otherwise unreachable offline.
func (s *AutoCookieService) notePassCompleted() {
	if s.OnPassCompleted != nil {
		s.OnPassCompleted()
	}
}

// profileImportStartupDelay is how long the browserless startup import waits
// before running. RefreshCookies verifies the imported cookies over the
// network, and firing that the instant the process comes up — before DNS,
// the network stack, or a VPN sidecar is ready — would report a false
// "auth verification failed" and flag a re-login the user does not need.
const profileImportStartupDelay = 15 * time.Second

// StartProfileSeed runs AT MOST ONE browser-free import out of the configured
// browser profile, shortly after start, on an install that has no cookies to
// lose. It returns immediately; the import happens on its own goroutine.
//
// NOT gated on cookies.auto_enabled, and that separation is the point. The flag
// owns the periodic timer — a repeating read of a profile nothing changes
// between ticks, which is why the operator triggers those reads with R F. This
// is not that. It is once per boot, and a boot is the moment a mounted profile
// most plausibly DID change: the container was down while somebody replaced it.
//
// The condition that keeps it safe is the cookie file, not the flag. A cold
// start with no usable cookies.txt has nothing to lose and everything to gain
// from reading the profile once; an install that already has cookies is never
// touched here, because whatever is on disk may be working credentials and R F
// is the way to replace those deliberately. decideStartupSeed owns that call.
func (s *AutoCookieService) StartProfileSeed(ctx context.Context) {
	switch d := s.decideStartupSeed(); d {
	case autoImportOK:
	case autoImportCookieFileUnreadable:
		// The one stand-down that is operator-actionable, and the one that
		// must never be silent — see ErrCookieFileUnreadable. An unreadable
		// file is not an absent one: it may hold working credentials for a
		// platform this process has not looked at, so it is left alone.
		s.logger.Warn("startup browser-profile cookie import stood down — the existing cookies.txt "+
			"could not be read, so it was left untouched rather than imported over. Fix the "+
			"permission or mount problem; nothing here needs replacing.",
			"path", s.cookiePath)
		return
	default:
		s.logger.Debug("startup browser-profile cookie import not applicable", "reason", d.String())
		return
	}

	s.logger.Info("no browser and no cookies to lose — seeding cookies from the configured browser profile",
		"profile_dir", s.profileDir, "cookie_file", s.cookiePath,
		"delay", profileImportStartupDelay.String())

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in startup cookie seed goroutine", "panic", fmt.Sprintf("%v", r))
			}
		}()
		if err := utils.Sleep(ctx, profileImportStartupDelay); err != nil {
			return
		}
		// Asked again, because the whole gate is "there is nothing here to
		// lose" and the wait is long enough for that to stop being true — an
		// interactive setup finishing, or an operator dropping a hand-exported
		// cookies.txt in, both write the file this decision was made about.
		if s.decideStartupSeed() != autoImportOK {
			s.logger.Debug("startup browser-profile cookie import stood down — cookies appeared while it waited")
			return
		}
		seedCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
		// RefreshCookies, i.e. gateApplies: this is not the timer, so it has no
		// claim on the exemption. The two policies are provably identical here
		// anyway — decideStartupSeed runs this only when resolvedBrowser() is
		// nil, and the gate's only power is to turn a non-nil browser into nil
		// — so gateExempt would buy nothing and would blur what it means.
		//
		// Detailed rather than the RefreshCookies wrapper, which returns
		// AnyVerified() and DISCARDS Ran. ok below is that same bool, so the
		// three log arms are unchanged; Ran is the extra fact, and it is the
		// one that decides whether anything was written.
		result, err := s.refreshCookiesDetailed(seedCtx, gateApplies)
		cancel()
		ok := result.AnyVerified()
		switch {
		case err != nil:
			s.logger.Warn("startup browser-profile cookie import failed", "err", err)
		case ok:
			s.logger.Info("startup browser-profile cookie import succeeded")
		default:
			s.logger.Warn("startup browser-profile cookie import did not authenticate any platform")
		}
		// The second site of the seam, and the one Arc 10 missed. This is the
		// container install's ONLY credential writer — nothing here will ever
		// run a browser — and at boot it lands 15 s after the refresh service
		// took its first status over an empty jar. Without this the seed repairs
		// the credentials, a Twitch job that already went anonymous has marked
		// the platform, and nothing compares the fingerprint, clears that mark
		// or reconnects the live chat session until the 30-minute ticker.
		//
		// Gated on Ran and not on ok, for the reason the tick is: a pass that
		// ran and produced a dead pair moved the fingerprint exactly as a
		// working one did. Below the log chain, also for the tick's reason —
		// the hook may run a full in-process re-check, and the operator should
		// not read the import's verdict half a minute after the re-check's
		// output.
		if result.Ran {
			s.notePassCompleted()
		}
	}()
}

// StartPeriodicRefresh starts a background goroutine that periodically
// refreshes cookies via headless browser visit. When HasActiveJobs is set,
// ticks where it returns false are skipped to avoid spawning a headless
// browser when nothing needs authenticated YouTube/Twitch access.
//
// The one-shot startup import used to live in here, which coupled it to
// cookies.auto_enabled for no reason it could justify. It is StartProfileSeed
// now, and main.go calls that unconditionally.
//
// EVERY pass this goroutine runs is gateExempt, and that is the whole reason
// the policy exists. main.go starts this loop only when cookies.auto_enabled
// was true at boot, so the flag has already been consulted; re-consulting it
// per pass would mean an operator who switched it off without restarting kept
// the timer AND had it quietly change mechanism, importing an unchanged browser
// profile on a schedule. Moombox's answer for a profile the operator updates by
// hand is the manual trigger — R F in the TUI, shift+click on the dashboard —
// because a refresh is only meaningful when something changed the profile, and
// the operator is the only thing that can.
func (s *AutoCookieService) StartPeriodicRefresh(ctx context.Context, interval time.Duration) {
	s.logger.Info("auto-cookie periodic refresh enabled", "interval", interval.String())
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in periodic cookie refresh goroutine", "panic", fmt.Sprintf("%v", r))
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.periodicRefreshHasSource() {
					s.logger.Debug("periodic auto-cookie refresh skipped — no browser profile directory yet",
						"profile_dir", s.profileDir)
					continue
				}
				// THE RULE, at its second automatic site: a browser-free import
				// runs only when there is no cookies.txt to lose.
				//
				// Scoped to a tick that would BE a browser-free import, and the
				// scope is load-bearing. Refreshing a LIVE cookies.txt through a
				// headless browser is what this timer is for, so a host with a
				// browser must keep doing exactly that. Only the browserless
				// pass is an import, and an import over an existing cookie file
				// is the thing the owner ruled out: nothing between two ticks
				// changes a mounted profile, so it re-reads identical bytes over
				// credentials that may be working.
				//
				// gateExempt to match the pass this tick would actually run —
				// asking with a different policy could answer "browser" here
				// and "no browser" three lines down.
				if s.refreshBrowser(gateExempt) == nil {
					if v := s.automaticImportGuard(); v != autoImportOK {
						s.logger.Debug("periodic auto-cookie refresh skipped — a browser-free import "+
							"may only run when there is nothing to lose", "reason", v.String())
						continue
					}
				}
				if s.shouldSkipPeriodicRefresh(interval) {
					s.logger.Debug("periodic auto-cookie refresh skipped — no active jobs or recent refresh")
					continue
				}
				s.logger.Debug("periodic auto-cookie refresh triggered")
				refreshCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
				// Detailed, not the bool wrapper: only the full result carries
				// Ran, and Ran is what decides whether anything was written.
				result, err := s.refreshCookiesDetailed(refreshCtx, gateExempt)
				cancel()
				ok := result.AnyVerified()
				if err != nil {
					s.logger.Warn("periodic auto-cookie refresh failed", "err", err)
				} else if ok {
					// Debug, and deliberately not "succeeded": RefreshCookies
					// has just logged the one line that knows whether this pass
					// RENEWED the credentials or merely found the previous ones
					// still alive — at Info when it did, at Warn when it did
					// not. Repeating "succeeded" here would contradict the
					// second case and put the false claim back a line later.
					s.logger.Debug("periodic auto-cookie refresh tick finished with authenticated cookies on disk")
				}
				// AFTER the verdict above, not before it. The hook may run a
				// full in-process re-check — two validate round-trips, ~30 s at
				// the client timeout — and everything RefreshService.refresh
				// logs on the way lands in between. Firing it first buried this
				// tick's own "failed"/"finished" line half a minute down the log,
				// underneath output about a different pass.
				//
				// Gated on Ran, NOT on success. A pass that ran and failed still
				// rewrote cookies.txt — a browser refresh that produced a
				// new-but-dead pair moves the credential fingerprint exactly as a
				// working one does — so firing on success only would leave the
				// Twitch auth mark keyed to a pair that is no longer on disk. A
				// DECLINED pass (seven refreshDeclined() exits) wrote nothing, so
				// there is nothing to re-read.
				if result.Ran {
					s.notePassCompleted()
				}
			}
		}
	}()
}

// --- helpers ---

// refreshChromiumCookies is the Chromium browser-refresh step behind a package
// variable, so a test can exercise what RefreshCookiesDetailed DOES WITH the
// step's result without launching a browser — notably the ErrNoCookiesInProfile
// downgrade above, which is unreachable otherwise: the real function has to
// start a headless Chromium and speak CDP to it before it can report an empty
// profile, and no test in this package may launch a browser.
//
// Same seam convention as detectBrowser, setupBrowserGone, killProcessTree and
// writeCookieFile. Nothing in production reassigns it.
var refreshChromiumCookies = (*AutoCookieService).refreshChromium

// killProcessTree kills a process and all its children on Windows (taskkill /T /F),
// or just the process itself on other platforms.
//
// A package variable purely so tests can exercise the kill DECISION without a
// real process — same reason writeCookieFile below is one. Nothing in
// production reassigns it, and it is always addressed by PID: never by image
// name, which on a developer's machine would take out their own browser.
var killProcessTree = func(proc *os.Process) {
	if proc == nil {
		return
	}
	if isWindows() {
		exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		killProcessTreeUnix(proc)
	}
}

// killProcessTreeUnix is the non-Windows arm, split out of the closure above so
// a test on ANY platform can execute it: isWindows() reads runtime.GOOS through
// a plain function rather than a seam, so on the Windows machine this project
// is developed on the else branch is otherwise unreachable. The one-line wiring
// above is reviewed by eye — the same coverage posture startChromiumSetup
// states in prose for its own trackedSetupJob call.
//
// ON LINUX THE TREE IS THE GROUP. configureCmdSysProcAttr sets Setpgid on every
// browser this package launches, so the child leads a group whose id is its own
// pid, and one kill(-pgid) reaches the browser the launcher handed off to.
// proc.Kill() alone never did: it kills the launcher, which on the Firefox
// family exited ~170 ms after start.
//
// IT GOES THROUGH pgroupJob, NOT STRAIGHT TO THE HOOK, so it inherits adopt's
// and killGroup's refusals: the pid must be in the table right now and lead its
// own group, and the group must still have members. killSetupProcess reaches
// here with a REAPED pid whenever no job could vouch for the browser, and
// proc.Kill() was safe there — Go refuses it on a process that has already
// been waited on (os.ErrProcessDone). A bare kill(-pid) has no such memory; it
// fires at whatever group the kernel has since given that number to.
//
// Everywhere the refusals apply — darwin and the fallback build, where the
// table hook is unbound and answers errNoProcessTable; a Linux /proc that
// cannot be read; a pid that is gone or sits in someone else's group — the
// direct kill below is exactly today's behaviour. The pid <= 0 guard is
// adopt's; see killProcessGroup's doc for the three checks on kill(-0, …).
func killProcessTreeUnix(proc *os.Process) {
	if proc == nil {
		return
	}
	group := &pgroupJob{}
	if err := group.adopt(proc.Pid); err == nil {
		if err := group.killGroup(); err == nil {
			return
		}
	}
	killOneProcess(proc)
}

// killOneProcess is (*os.Process).Kill behind a package variable, for the same
// reason killProcessTree itself is one: the fallback above has to be
// exercisable without a real process, and a fabricated PID must never reach a
// real signal on the machine running the tests. Nothing in production
// reassigns it.
var killOneProcess = func(proc *os.Process) error { return proc.Kill() }

// killSetupProcess terminates the setup browser, waiting briefly for one that
// has been decided on but not yet launched.
//
// The poll is the same fix killRefreshProcess carries for the refresh slot, for
// the identical race in the setup slot. StartSetup claims with `setupClaimed`
// and only assigns setupProcess once the launcher has started the browser, so a
// CancelSetup or Stop landing between the mid-preparation cancel check and that
// assignment used to find nil, kill nothing, and return — and the launcher then
// registered a browser into a slot nobody was watching. The user got a stuck
// wizard and an open browser window.
//
// Deliberately NOT a new flag saying "a launch is in flight": setupClaimed
// already says exactly that, and a second field describing the same window is
// one more thing to get wrong. Capped like the refresh side so a launcher that
// errors before assigning cannot make Stop() hang; past the cap the
// mid-preparation cancel check is what stops the launch, and what is left is a
// sub-second window between that check and cmd.Start() returning.
//
// IT WILL NOT SHELL A TASKKILL AT A PID THAT HAS ALREADY BEEN REAPED. Once the
// spawned process has exited AND a live Job Object is holding whatever it
// handed off to, this PID identifies nothing: Windows recycles PIDs, so the
// kill can only ever land on an unrelated process, and the thing it was meant
// to reach is killed by cleanupLocked closing the job a moment later. That
// last clause is the guard's premise, so it is checked rather than assumed:
// ALL FOUR callers — CancelSetup, Stop, FinishSetup's Chromium branch, and
// startChromiumSetup's CDP-timeout bail — call cleanup() immediately after
// this, so the job always gets its turn. A fifth caller that does not must not
// rely on this.
//
// That state is the NORMAL one on the Firefox family, where the launcher hands
// off and exits in ~170ms, and it is why this guard arrived with the setup Job
// Object rather than before it: until the job existed, killing the stale PID
// was the only thing that even pretended to help. It is the same rule
// runWithTimeout applies with onLauncherReaped, which stops advertising a
// reaped PID for exactly this reason.
//
// It does NOT make killing-by-PID safe in general, and does not try to. Where
// no job can vouch for the browser — a failed assign on either platform, and
// darwin and the fallback build, where queryable() is always false — the kill
// still runs, because there it is the only thing that can work. On Linux that
// kill goes through killProcessTreeUnix, which signals a group only when the
// pid still leads one; a reaped pid falls back to proc.Kill, which Go refuses
// on a process it has already waited on.
func (s *AutoCookieService) killSetupProcess() {
	deadline := time.Now().Add(launchWindowKillBudget)
	for {
		s.mu.Lock()
		proc := s.setupProcess
		claimed := s.setupClaimed
		// Read under the same lock as proc: the question is whether THIS
		// process is still a thing worth killing, and both halves of the answer
		// have to describe the same instant.
		reapedPID := s.browserExited && s.setupJob.queryable()
		s.mu.Unlock()

		if proc != nil {
			if reapedPID {
				return // the job will do it; this PID belongs to whoever Windows gave it to next
			}
			killProcessTree(proc)
			time.Sleep(taskkillDrainDelay)
			return
		}
		if !claimed {
			return // no process, and no launch on the way — nothing to kill
		}
		if !time.Now().Before(deadline) {
			return // still unpublished — give up rather than block the caller
		}
		time.Sleep(killProcessTreePollDelay)
	}
}

func (s *AutoCookieService) killRefreshProcess() {
	// RefreshCookies claims the slot with `s.refreshCmd = &exec.Cmd{}`
	// (a sentinel with nil Process) before refreshFirefox/refreshChromium
	// assigns the real cmd. A naive `Process == nil → bail` lets a Stop()
	// during that window leak the real browser when it lands a moment later
	// (audit reports/cookies.md #22). Poll briefly so the kill catches the
	// real process once the launcher publishes it, but cap the wait so Stop()
	// doesn't block indefinitely if the launcher errors before assignment.
	deadline := time.Now().Add(launchWindowKillBudget)
	for {
		s.mu.Lock()
		cmd := s.refreshCmd
		s.mu.Unlock()

		if cmd == nil {
			return // launcher cleared the slot — nothing to kill
		}
		if cmd.Process != nil {
			killProcessTree(cmd.Process)
			return
		}
		if !time.Now().Before(deadline) {
			return // sentinel still in place — give up rather than block Stop()
		}
		time.Sleep(killProcessTreePollDelay)
	}
}

// cleanup releases the per-attempt setup state so the next StartSetup begins
// from a clean slate. It runs on EVERY setup exit path — success, extraction
// failure, the empty-profile "no login detected" case, each of the mkdir /
// write / jar-load failures, the S9 read-abort that refuses to overwrite an
// unreadable cookies.txt, the Chromium CDP-timeout bail, CancelSetup and Stop
// — and that breadth is precisely why the two decision flags below are NOT in
// the list.
//
// `cancelled` is not reset here. It used to be, and because CancelSetup's own
// last act is to call cleanup(), a complete cancel erased its own flag
// microseconds after raising it: StartSetup's mid-preparation check could
// never observe one. It is cleared at claim time in StartSetup instead, which
// is where a cancel is actually consumed. Do not "fix" a lingering flag by
// putting the reset back here under some condition — that is the same defect
// with extra steps.
//
// `stopped` is not reset here for a stronger reason: Stop() calls cleanup()
// itself, so clearing it would un-stop the service inside the very call that
// stopped it. Nothing may lower that latch.
//
// What does get cleared is only ever state describing a browser that is gone:
// the Job Object handle, the process, the browser record, the exit flag, the
// retention timestamp, the CDP port and the target platform.
func (s *AutoCookieService) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
}

// cleanupLocked is cleanup's body for callers that already hold s.mu — the
// inline reap in reapAbandonedSetupLocked, which must decide and clean up
// inside ONE critical section. See cleanup for what is cleared and, more
// importantly, what is not.
//
// ON WINDOWS, CLOSING THE JOB OBJECT KILLS BROWSERS. That is the point of it —
// KILL_ON_JOB_CLOSE finishes off a setup browser killSetupProcess missed, on
// BOTH families now that startFirefoxSetup creates a job too — but it is also
// why every caller must have established, under the lock it still holds, that
// the slot it is clearing is the slot it looked at. A caller that sampled the
// state, released the lock, and came back later can be looking at a different
// attempt's job, and closing that one kills a browser the user is actively
// using.
//
// IT KILLS ON LINUX TOO NOW, but by a different route: the close only forgets a
// process-group id there, so the killTrackedProcesses call in the body below is
// what reaches the browser. Same consequence, same requirement on callers. On darwin and the
// fallback build job_other.go is still a no-op stub, nothing is tracked and
// nothing is killed; a browser left behind there keeps running (pdeathsig ties
// it to Moombox's death, not to this call).
//
// Caller must hold s.mu.
func (s *AutoCookieService) cleanupLocked() {
	if s.setupJob != nil {
		// BEFORE the close, and before the field is nilled. On Windows the
		// close is itself the kill and this is a no-op; on Linux the close
		// forgets a number, so this is the only thing that reaches the browser.
		// See killTrackedProcesses.
		if err := killTrackedProcesses(s.setupJob); err != nil && s.logger != nil {
			s.logger.Warn("could not kill the setup browser's process group; it may still be running",
				"err", err)
		}
		s.setupJob.close()
		s.setupJob = nil
	}
	s.setupProcess = nil
	s.setupBrowser = nil
	s.browserExited = false
	s.setupRetainedSince = time.Time{}
	s.cdpPort = 0
	s.targetPlatform = ""
}

// assignProcessToJob is processJob.assign behind a package variable, so a test
// can induce the failure trackedSetupJob exists to handle. That failure needs a
// hostile process state — a handle that cannot be opened for
// PROCESS_SET_QUOTA|PROCESS_TERMINATE, or an AssignProcessToJobObject refusal —
// which no test in this package can arrange without launching something. Same
// seam convention as setupBrowserGone, killProcessTree and writeCookieFile;
// nothing in production reassigns it.
var assignProcessToJob = func(job *processJob, p *os.Process) error { return job.assign(p) }

// trackedSetupJob puts a launched setup browser under its Job Object and
// returns the job the setup slot should own — nil when nothing is actually
// being tracked. Both launchers call it, so the decision below is made once.
//
// A JOB THAT TRACKS NOTHING IS WORSE THAN NO JOB. Its handle is live, so
// queryable() is true and activeProcesses() answers 0 — and setupBrowserGone
// reads that pair as a POSITIVE "the browser is gone". The reap would then
// release a setup whose browser is still on screen. It cannot kill that
// browser (nothing is in the job for KILL_ON_JOB_CLOSE to take), so the
// consequence is a premature release: the user's next "I'm logged in" answers
// ErrNoSetupInProgress. Dropping the job instead makes the probe say "no idea",
// which is the honest reading of a failed assign, and leaves the slot alone.
//
// Dropping loses nothing. A job with a failed assign provides neither the
// crash-time cleanup nor the count it was created for, because the launcher's
// children join the launcher's OWN job, not this one.
//
// NOTE THE ASYMMETRY WITH runWithTimeout, which keeps a failed-assign job on
// purpose. Correct there, and for the same underlying rule: drainJob reads its
// zero as "nothing was waited on", never as "the browser finished". The two
// paths differ because their readers differ.
func (s *AutoCookieService) trackedSetupJob(job *processJob, proc *os.Process, family string) *processJob {
	if job == nil {
		return nil
	}
	if err := assignProcessToJob(job, proc); err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to assign setup process to job object — dropping the job "+
				"rather than let an empty one look like a closed browser",
				"family", family, "pid", proc.Pid, "err", err)
		}
		job.close()
		return nil
	}
	return job
}

// adoptSetupJobLocked hands ownership of a freshly created Job Object to the
// setup slot, closing whatever handle an earlier attempt left there. Both
// launchers go through it — startChromiumSetup and startFirefoxSetup — so the
// guard below exists once instead of in two copies free to drift.
//
// NEVER OVERWRITE A LIVE JOB OBJECT HANDLE. Dropping one leaks the handle AND
// the browser it holds: nothing else has a reference, so KILL_ON_JOB_CLOSE
// never fires and the orphan runs until Moombox exits. StartSetup's reap should
// have cleared this already — if it did not, the invariant broke somewhere and
// closing is still the right answer, because the only process such a job can
// hold is a setup browser from an attempt the gate has already declared over.
//
// Closing it cannot touch the browser the caller just launched: that browser
// was assigned to `job`, and a process is only ever in the job it was assigned
// to. Callers must therefore assign BEFORE calling, which both do.
//
// Caller must hold s.mu.
func (s *AutoCookieService) adoptSetupJobLocked(job *processJob) {
	if s.setupJob != nil {
		if s.logger != nil {
			s.logger.Warn("closing a setup Job Object left behind by an earlier attempt")
		}
		if err := killTrackedProcesses(s.setupJob); err != nil && s.logger != nil {
			s.logger.Warn("could not kill the stale setup browser's process group", "err", err)
		}
		s.setupJob.close()
	}
	s.setupJob = job
}

func (s *AutoCookieService) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = &msg
}

// isWindows returns true when running on Windows.
func isWindows() bool {
	return runtimeGOOS() == "windows"
}

// writeCookieFile is the cookie-file write RefreshCookies goes through, as a
// package variable so tests can exercise the branches that only exist for a
// FAILED write — notably a rollback that cannot put the previous credentials
// back, which decides what the operator is told is on disk.
var writeCookieFile = writeFileAtomic

// readCookieFile is the read FinishSetup and RefreshCookiesDetailed go
// through before merging freshly-extracted cookies into an existing
// cookies.txt, as a package variable so tests can exercise a read that fails
// for a reason OTHER than "file does not exist" (a permission blip, a locked
// file, an I/O error) without needing to make a real fixture file
// unreadable — mirrors writeCookieFile above. See the callers for why that
// distinction matters: os.IsNotExist is the normal first-run case, and every
// other error must abort rather than silently proceed as if there were
// nothing to merge.
var readCookieFile = os.ReadFile

// statProfileDir is os.Stat, behind a seam, for the three places that ask
// whether the configured browser profile directory can be looked at: the
// missing-profile gate in RefreshCookiesDetailed, the import in
// importProfileCookies, and the periodic loop's per-tick precondition in
// periodicRefreshHasSource. The first two are the reason it is a seam at all:
// a test needs to drive the real sequence — the gate sees a non-ENOENT error
// and proceeds, and the import classifies it. The third goes through it for
// consistency, so all three answer the same way when a test does substitute;
// TestPeriodicLoopPicksUpAProfileThatAppearsAtRuntime does not substitute, and
// creates a real directory mid-test instead.
//
// A seam rather than a fixture because the states that matter are not portably
// constructible: EACCES needs a chmod that means nothing on Windows, and the
// ENOTDIR shape (a file in the middle of the path) surfaces as ERROR_PATH_NOT_
// FOUND there, which os.IsNotExist reports as true. Building the failure by
// hand would pin the case on one platform and skip it on the other, and the one
// it would skip is the one this seam exists to test.
var statProfileDir = os.Stat

// writeFileAtomic writes data to a temp file then renames it to the target path,
// preventing corruption on partial failure. Applies
// utils.ApplyUserOnlyDACL to the parent directory (successful attempts are
// memoised per directory; see tightenCookieDirOnce) so the highest-value
// secret in the app (auth-token + SAPISID for the user's session) doesn't
// sit on disk with a parent-inherited world-readable ACL when the cookie
// file lives outside the config dir (e.g. default `./cookies.txt` in the
// project root). The DACL is applied to the parent dir rather than the file
// because (a) icacls /inheritance:r on individual files has corner cases
// where the new ACL ends up over-restrictive, and (b) propagating from the
// dir covers any future writes (rotated cookies, side-files) without
// per-write icacls latency. Real hardening on non-Windows too — see
// utils.ApplyUserOnlyDACL's non-Windows implementation, which chmods to
// 0700/0600 rather than no-op'ing — not just Windows; idempotent once
// applied.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	// Unique temp name (os.CreateTemp): the RefreshService rewrites the same
	// cookies.txt through its own temp file, and a shared fixed ".tmp" name
	// would let two concurrent writers interleave into a corrupt file.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cookie file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp cookie file: %w", err)
	}
	// fsync before rename (matches ResumeStore.Save / WriteChatFileAtomic /
	// config.Save): without it a crash can journal the rename while the data
	// pages never hit disk, leaving a zero-length/torn cookies.txt that Load
	// silently trusts — dropping auth cookies until a full re-login.
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync temp cookie file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp cookie file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp cookie file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp cookie file: %w", err)
	}
	tightenCookieDirOnce(filepath.Dir(path))
	return nil
}

// cookieTempFileMaxAge bounds how long an orphaned writeFileAtomic temp file
// may survive before the sweep below reclaims it. A write completes in
// milliseconds; this is three orders of magnitude of margin, so nothing but
// a genuinely abandoned temp file — left behind by a crash, a kill, or a
// panic between os.CreateTemp and the rename — is ever old enough to match.
// Do not lower this "to be thorough": age is the only guard against
// sweeping a write in progress.
const cookieTempFileMaxAge = time.Hour

// cookieTempFileSweepOnce fires sweepStaleCookieTempFiles exactly once per
// process. Package-level state, same shape as snapshotSweepOnce in
// autocookies_profile.go — different root (the cookie file's own directory,
// not os.TempDir()) and a different secret (the whole cookie file, not a
// browser DB snapshot), so it stays a sibling rather than merging with it.
//
// Wired at service construction (NewAutoCookieService), which is the one
// place in the package that always holds the REAL cookie file path up
// front. writeFileAtomic itself is a generic temp-then-rename helper shared
// with meta.go's cookies.meta.json sidecar writes and refresh.go's own
// cookies.txt rewrite, so keying the "once" off whichever path happens to
// call writeFileAtomic first would risk sweeping with the wrong base name
// if call order ever changed.
var cookieTempFileSweepOnce sync.Once

// sweepCookieTempFilesOnce runs sweepStaleCookieTempFiles through the given
// *sync.Once. Production code always passes &cookieTempFileSweepOnce; tests
// pass a local one so the process-lifetime package var doesn't make every
// test but the first one in the binary a no-op.
func sweepCookieTempFilesOnce(once *sync.Once, dir, base string, maxAge time.Duration) {
	once.Do(func() { sweepStaleCookieTempFiles(dir, base, maxAge) })
}

// sweepStaleCookieTempFiles removes orphaned writeFileAtomic temp files left
// beside the cookie file: a crash, a kill, or a panic between os.CreateTemp
// and the rename leaves `<base>.<random>.tmp` on disk forever, and each one
// is a full copy of cookies.txt — the highest-value secret in the app —
// under a name nothing reads and nothing else removes.
//
// Matches ONLY <base>.<anything>.tmp in dir — the exact shape
// os.CreateTemp(dir, base+".*.tmp") produces in writeFileAtomic. The prefix
// check is anchored on base+"." (not on the directory as a whole), so this
// can never match base itself (no .tmp suffix), never matches an operator
// file like cookies.txt.bak, and never matches an unrelated file's temp
// (other.txt.NNN.tmp).
//
// Age (see cookieTempFileMaxAge) is the guard against sweeping a live
// write. Best-effort — every failure is logged at Debug, with the path only
// and never content or size, and left for the next process start to retry;
// this is housekeeping, not correctness, and must never fail a caller over
// a stale file it couldn't remove.
func sweepStaleCookieTempFiles(dir, base string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Debug("cookie temp file sweep: could not read directory", "dir", dir, "err", err)
		return
	}
	prefix := base + "."
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			slog.Debug("cookie temp file sweep: could not remove orphaned temp file", "path", path, "err", err)
			continue
		}
		slog.Debug("cookie temp file sweep: removed orphaned temp file", "path", path)
	}
}

// applyUserOnlyDACL is utils.ApplyUserOnlyDACL behind a seam, so
// tightenCookieDirOnce's retry-on-failure memoisation (below) can be tested
// with a fake that fails once then succeeds, instead of shelling out to a
// real icacls (Windows) or depending on real chmod failure modes (Linux) in
// CI. Mirrors writeCookieFile/readCookieFile/statProfileDir above.
var applyUserOnlyDACL = utils.ApplyUserOnlyDACL

// dirTightenState is tightenCookieDirOnce's per-directory memo. Two states,
// not one boolean, because "an apply is running right now" and "an apply
// already succeeded" must be told apart: the first still says "don't spawn
// another", the second says "there is nothing left to do, ever". A dir
// absent from the map is the implicit third state, not started.
type dirTightenState int

const (
	dirTighteningInFlight dirTightenState = iota
	dirTighteningDone
)

// tightenCookieDirOnce applies utils.ApplyUserOnlyDACL to the given parent
// dir. Memoised on SUCCESS, not on attempt: icacls (Windows) / chmod
// (Linux — see utils.ApplyUserOnlyDACL's non-Windows implementation, which
// really chmods to 0700/0600 there, not a no-op) is a ~30-80ms shell-out/
// syscall that would otherwise fire on every cookie write, but a transient
// failure (an AV scanner holding the dir, a first-write race with the dir's
// own creation) must not disable hardening for the rest of the process just
// because the failure is demoted to a Debug log line.
//
// Three states per dir (dirTightenState above, plus "absent = not
// started"):
//   - not started: this call marks the dir in flight, synchronously, before
//     spawning the goroutine that runs the apply — so a second write
//     landing during the 30-80ms shell-out sees "in flight" and returns
//     without spawning a second one.
//   - in flight: return; the goroutine already running will resolve it.
//   - done: return; already applied, nothing left to do.
//
// A failure — or a panic mid-apply — deletes the entry, putting the dir
// back to "not started" so the NEXT cookie write retries rather than
// leaving it stuck in flight or falsely memoised as done.
//
// Cost if icacls/chmod fails permanently on a given host: one extra
// ~30-80ms shell-out per cookie write instead of one per process. Writes
// happen on the refresh cadence (30 min) and on imports, so the bound is a
// handful of shell-outs an hour. No failure cap, no backoff — either would
// be a mechanism to contain a mechanism, and nothing has profiled the plain
// retry as costing anything.
var (
	tightenedCookieDirsMu sync.Mutex
	tightenedCookieDirs   = make(map[string]dirTightenState)
)

func tightenCookieDirOnce(dir string) {
	tightenedCookieDirsMu.Lock()
	if _, ok := tightenedCookieDirs[dir]; ok {
		tightenedCookieDirsMu.Unlock()
		return // in flight or already done
	}
	tightenedCookieDirs[dir] = dirTighteningInFlight
	tightenedCookieDirsMu.Unlock()

	go func() {
		succeeded := false
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("cookie dir DACL tightening panicked", "dir", dir, "panic", fmt.Sprint(r))
			}
			if !succeeded {
				// Covers both a returned error and a recovered panic: either
				// way the apply did not finish successfully, so the memo
				// goes back to "not started" for the next write to retry.
				tightenedCookieDirsMu.Lock()
				delete(tightenedCookieDirs, dir)
				tightenedCookieDirsMu.Unlock()
			}
		}()
		// This is the hardening for the auth-cookie file (highest-value
		// secret in the app). Demoted to Debug (matches the config +
		// sidecar + profile dir sites): the common failure is ACCESS_DENIED
		// on a dir created under an elevated/admin context, and on the
		// single-user host this app targets nobody else can read it anyway.
		// Raise the log level to Debug to surface the miss. A failure here
		// is retried on the NEXT cookie write, not memoised — see the
		// doc comment above.
		if err := applyUserOnlyDACL(dir); err != nil {
			slog.Debug("could not restrict cookie dir to current user", "dir", dir, "err", err)
			return
		}
		tightenedCookieDirsMu.Lock()
		tightenedCookieDirs[dir] = dirTighteningDone
		tightenedCookieDirsMu.Unlock()
		succeeded = true
	}()
}

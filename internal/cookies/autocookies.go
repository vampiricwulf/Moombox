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
// input is allowed — that just signals "auto-cookies not configured"
// and the service's Configured status reports false. Otherwise the
// path is resolved to absolute, lowercased, and checked against the
// dangerous-substring list above. Audit reports/cookies.md #26.
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
type AutoCookieStatus struct {
	Configured            bool                      `json:"configured"`
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
	setupJob      *processJob // Windows Job Object for setup browser; nil on non-Windows
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
	lastError          *string
	needsRelogin       AutoCookieReloginRequired
	targetPlatform     string // "youtube" or "twitch"

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

	// DpapiFallback enables the Windows-only DPAPI cookie-extraction
	// path as a fallback when the CDP refresh launch fails. Off by
	// default — the fallback reads the user's REAL Chromium-family
	// browser profile, which is a privacy surface the user has to
	// opt into. When true, RefreshCookies tries DPAPI as a backstop
	// once the primary CDP launch returns an error. DECISIONS #6.
	DpapiFallback bool

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
	// the constructor — Configured will simply report false (empty
	// case) or the entry-point fast-fail kicks in (dangerous case)
	// — to preserve the current "constructor never errors" contract.
	// Audit reports/cookies.md #26.
	profileDirErr := validateBrowserProfileDir(profileDir)
	if profileDirErr != nil && logger != nil {
		logger.Error("auto-cookie profile dir rejected at construction", "err", profileDirErr)
	}
	s := &AutoCookieService{
		profileDir:    profileDir,
		cookiePath:    cookiePath,
		jar:           jar,
		profileDirErr: profileDirErr,
		detectBrowser: DetectBrowser,
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
//   - Linux, and every non-Windows target — there is no Job Object PRIMITIVE
//     at all (job_linux.go / job_other.go are no-op stubs), so nothing is
//     answerable on ANY path, Chromium included, and the reap never fires.
//     GIVING FIREFOX A JOB DID NOT CHANGE THAT. Docker and native Linux keep
//     the abandoned-setup wedge until something other than a Job Object owns
//     the liveness question there. Recorded as a known residual, not an
//     oversight.
//
// Two answerable cases and one that is not. Wherever it is not — and on Windows
// wherever newProcessJob or its assign failed — the client-side cancel (the
// unload beacon, Skip, Escape, and the TUI countdown) is what clears an
// abandoned setup; the gap is specifically "no client survived to say
// anything".
//
// A package variable so tests can supply the answer a real Job Object gives on
// a machine where no browser may be launched — the same seam convention as
// detectBrowser, killProcessTree and writeCookieFile. Nothing in production
// reassigns it.
var setupBrowserGone = func(job *processJob) (gone, known bool) {
	// queryable, not `job != nil`. activeProcesses answers 0 for three
	// different situations and only one of them is "the job is empty": a nil
	// job (a launch where newProcessJob failed, or where the assign failed and
	// the launcher dropped the untrackable job rather than let it lie), an
	// already-closed handle, and a platform whose processJob cannot count at
	// all. The type knows which it is; this does not.
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
		Configured:            s.profileDir != "",
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

// resolvedBrowser returns the user's configured browser when set, else
// the auto-detected best match. Used by StartSetup and RefreshCookies
// so the UI's browser_path/browser_type setting actually drives
// extraction (not just cosmetic display in the dropdown).
func (s *AutoCookieService) resolvedBrowser() *DetectedBrowser {
	if s.ConfiguredBrowserOverride != nil {
		path, btype := s.ConfiguredBrowserOverride()
		if path != "" && btype != "" {
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

// FlagManualRelogin marks a platform as needing manual re-login.
func (s *AutoCookieService) FlagManualRelogin(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// FinishSetup extracts cookies from the running browser and saves them.
func (s *AutoCookieService) FinishSetup(ctx context.Context) (ytAuth, twAuth bool, err error) {
	s.mu.Lock()
	if s.setupProcess == nil || s.setupBrowser == nil {
		s.mu.Unlock()
		return false, false, ErrNoSetupInProgress
	}
	if s.cancelled {
		s.mu.Unlock()
		return false, false, ErrSetupCancelled
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

	if isFirefoxBased(browser.Type) {
		s.closeFirefoxGracefully()
		var stats firefoxReadStats
		netscapeCookies, stats, err = readFirefoxCookies(s.profileDir)
		s.logFirefoxReadStats(stats)
		// Interactive setup has a legitimate empty state the refresh and
		// profile-import paths do not: the user opened the browser and
		// closed it without signing in. readFirefoxCookies now reports an
		// empty profile as a hard error (a silently empty jar is the bug
		// that error exists to catch), so translate it back here — the setup
		// dialog should say "no login detected", not fail.
		if errors.Is(err, ErrNoCookiesInProfile) {
			s.logger.Info("cookie setup finished with an empty profile — no login detected")
			s.setError("no login detected — sign in before finishing setup")
			s.cleanup()
			return false, false, nil
		}
	} else {
		netscapeCookies, err = s.extractChromiumCookies()
		s.killSetupProcess()
	}

	if err != nil {
		s.setError(err.Error())
		s.cleanup()
		return false, false, err
	}

	// Merge with existing cookies using temp file + rename for atomicity
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		s.cleanup()
		return false, false, err
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
		return false, false, mergeErr
	}

	// Write merged cookies via temp file + rename to prevent corruption on partial failure
	if err := writeFileAtomic(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		s.cleanup()
		return false, false, err
	}

	// Reload jar and verify
	if err := s.jar.Load(s.cookiePath); err != nil {
		s.cleanup()
		return false, false, err
	}

	// Presence + real API verification, through the same pairing the refresh
	// path uses. This was the last inline copy of it; the nil-callback contract
	// (presence is then the only signal, reported as success with a warning so
	// callers cannot quietly succeed on cookies that are present-but-invalid —
	// audit reports/cookies.md #21) lives in checkPlatformAuth now.
	ytCheck, twCheck := s.checkPlatformAuth(ctx)

	// What the CALLER is told, and it is deliberately not the verification
	// result. A sign-in the user just completed is accepted when the site could
	// not answer: a 429, a captive portal or a DNS blip is not evidence against
	// a login that happened thirty seconds ago, and refusing it would send the
	// user back through a wizard that was working. False failure is the worse
	// direction there.
	//
	// It is NOT accepted when nothing was ever asked. A jar that cannot produce
	// a cookie header or a SAPISIDHASH made no request, so there is no answer
	// to extend the benefit of the doubt to — and the caller-facing value is
	// what setup.js turns into a green "YouTube cookies configured" badge and
	// an entry in active_platforms. Because FinishSetup merges the pre-existing
	// cookies.txt before checking, a leftover Google remnant with no SAPISID
	// would otherwise light that badge up for a user who only signed in to
	// Twitch. attempted is what separates the two; see platformAuth.
	accepted := func(p platformAuth) bool {
		return p.hasCookies && (p.state == verifyOK || (p.state == verifyUnknown && p.attempted))
	}
	ytAuth = accepted(ytCheck)
	twAuth = accepted(twCheck)

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

	return ytAuth, twAuth, nil
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
	// definition. On Linux it is false nearly always: there is no Job Object
	// to drain, so a launch cannot be confirmed to have acted. It therefore
	// means "not confirmed", never "the browser failed" — see the wording
	// note on browserLaunchActed.
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
// Kept as a thin wrapper over RefreshCookiesDetailed for the callers whose
// question really is whole-service ("can we do authenticated work at all?"):
// the startup seed, the periodic tick, the Settings "refresh now" button and
// the TUI's equivalent. Callers acting ON BEHALF of one platform must use
// RefreshCookiesDetailed instead — see RefreshResult.
func (s *AutoCookieService) RefreshCookies(ctx context.Context) (bool, error) {
	result, err := s.RefreshCookiesDetailed(ctx)
	return result.AnyVerified(), err
}

// RefreshCookiesDetailed performs a headless browser visit to refresh cookies
// and reports the outcome per platform.
func (s *AutoCookieService) RefreshCookiesDetailed(ctx context.Context) (RefreshResult, error) {
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

	browser := s.resolvedBrowser()

	if _, err := os.Stat(s.profileDir); os.IsNotExist(err) {
		// Neither a browser to drive nor a profile to import from: the
		// install genuinely has no cookie source, so keep the historical
		// answer and the "install a supported browser" UI copy that hangs
		// off it.
		if browser == nil {
			s.setError("no browser found for refresh, and no browser profile to import from")
			return refreshDeclined(), ErrNoBrowserFound
		}
		s.setError("browser profile not found — run setup first")
		return refreshDeclined(), fmt.Errorf("run setup first: %w", ErrProfileNotFound)
	}

	// importedFromProfile selects the browser-free path: no browser is
	// installed (a container, or a headless host), but the configured
	// profile directory is present and may hold a readable cookies.sqlite.
	// Before this branch existed, RefreshCookies bailed on `browser == nil`
	// BEFORE it ever looked at the profile — so a perfectly readable
	// mounted profile was refused on a technicality.
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
			// An empty profile is a hard error for the IMPORT path, where it
			// means the read is broken. On the browser path it has a mundane
			// explanation — Firefox set to clear cookies on close leaves the
			// profile empty every time — and before this package that
			// produced a no-op merge and a refresh that succeeded off the
			// still-good cookies.txt. Failing here instead would fire
			// "Cookie Auto-Refresh Failed — recordings will fail" at a user
			// whose recordings are fine.
			//
			// So: contribute nothing, let verification below decide, and
			// remember the fact so it is named if the existing cookies turn
			// out to be dead too. That keeps the desktop behaviour while
			// refusing to let a browser that silently stopped saving cookies
			// masquerade as an ordinary expiry.
			if errors.Is(err, ErrNoCookiesInProfile) {
				s.logger.Warn("browser refresh produced no cookies — falling back to the existing cookies.txt",
					"profile_dir", s.profileDir, "err", err)
				emptyBrowserProfile = true
				netscapeCookies, err = "", nil
			}
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
			netscapeCookies, navigated, err = s.refreshChromium(ctx, browser)
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
		fallbackCookies, fallbackErr := dpapiExtractAsNetscape(s.logger)
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
	// value win by name+domain — so a dead Twitch token in the profile
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
	var errMsg string
	switch {
	case len(restoredPlatforms) > 0 && rollbackWasInconclusive:
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — the auth check did not complete (network?), so the imported profile was not accepted"
	case len(restoredPlatforms) > 0:
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — the mounted browser profile did not verify"
	case inconclusive:
		errMsg = strings.Join(failed, " + ") + " auth could not be verified — the check did not complete (network?)"
	case emptyBrowserProfile:
		errMsg = strings.Join(failed, " + ") + " auth verification failed, and the browser profile contained " +
			"no cookies to refresh from — check whether the browser is clearing cookies on exit"
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

// profileImportStartupDelay is how long the browserless startup import waits
// before running. RefreshCookies verifies the imported cookies over the
// network, and firing that the instant the process comes up — before DNS,
// the network stack, or a VPN sidecar is ready — would report a false
// "auth verification failed" and flag a re-login the user does not need.
const profileImportStartupDelay = 15 * time.Second

// StartPeriodicRefresh starts a background goroutine that periodically
// refreshes cookies via headless browser visit. When HasActiveJobs is set,
// ticks where it returns false are skipped to avoid spawning a headless
// browser when nothing needs authenticated YouTube/Twitch access.
//
// On a browserless host with an importable profile (the Docker case) the
// goroutine also runs ONE import shortly after start, instead of leaving a
// freshly-mounted profile unread until the first tick — which with the
// default six-hour interval would be most of a day.
func (s *AutoCookieService) StartPeriodicRefresh(ctx context.Context, interval time.Duration) {
	s.logger.Info("auto-cookie periodic refresh enabled", "interval", interval.String())
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in periodic cookie refresh goroutine", "panic", fmt.Sprintf("%v", r))
			}
		}()

		if s.shouldSeedFromProfileAtStartup() {
			s.logger.Info("no browser detected — seeding cookies from the configured browser profile",
				"profile_dir", s.profileDir, "delay", profileImportStartupDelay.String())
			if err := utils.Sleep(ctx, profileImportStartupDelay); err == nil {
				// Deliberately bypasses shouldSkipPeriodicRefresh: the
				// profile may have been swapped while we were down, so
				// "nothing is downloading" and "we refreshed recently" are
				// both the wrong reasons to skip the first read of it.
				seedCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
				ok, err := s.RefreshCookies(seedCtx)
				cancel()
				switch {
				case err != nil:
					s.logger.Warn("startup browser-profile cookie import failed", "err", err)
				case ok:
					s.logger.Info("startup browser-profile cookie import succeeded")
				default:
					s.logger.Warn("startup browser-profile cookie import did not authenticate any platform")
				}
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.shouldSkipPeriodicRefresh(interval) {
					s.logger.Debug("periodic auto-cookie refresh skipped — no active jobs or recent refresh")
					continue
				}
				s.logger.Debug("periodic auto-cookie refresh triggered")
				refreshCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
				ok, err := s.RefreshCookies(refreshCtx)
				cancel()
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
			}
		}
	}()
}

// --- helpers ---

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
		proc.Kill()
	}
}

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
func (s *AutoCookieService) killSetupProcess() {
	deadline := time.Now().Add(launchWindowKillBudget)
	for {
		s.mu.Lock()
		proc := s.setupProcess
		claimed := s.setupClaimed
		s.mu.Unlock()

		if proc != nil {
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
// It kills NOTHING anywhere else: job_linux.go and job_other.go are no-op
// stubs, so on Linux and in Docker this only drops per-attempt state and a
// browser left behind keeps running (pdeathsig ties it to Moombox's death, not
// to this call). Do not read the sentence above as cross-platform.
//
// Caller must hold s.mu.
func (s *AutoCookieService) cleanupLocked() {
	if s.setupJob != nil {
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

// writeFileAtomic writes data to a temp file then renames it to the target path,
// preventing corruption on partial failure. Applies
// utils.ApplyUserOnlyDACL ONCE per parent directory across the process
// lifetime so the highest-value secret in the app (auth-token + SAPISID
// for the user's session) doesn't sit on disk with a parent-inherited
// world-readable ACL when the cookie file lives outside the config dir
// (e.g. default `./cookies.txt` in the project root). The DACL is
// applied to the parent dir rather than the file because (a) icacls
// /inheritance:r on individual files has corner cases where the new
// ACL ends up over-restrictive, and (b) propagating from the dir
// covers any future writes (rotated cookies, side-files) without
// per-write icacls latency. No-op on non-Windows; idempotent.
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

// tightenCookieDirOnce applies utils.ApplyUserOnlyDACL to the given
// parent dir at most once per process. Memoised because icacls is a
// ~30-80ms shell-out that would otherwise fire on every cookie write.
var (
	tightenedCookieDirsMu sync.Mutex
	tightenedCookieDirs   = make(map[string]struct{})
)

func tightenCookieDirOnce(dir string) {
	tightenedCookieDirsMu.Lock()
	defer tightenedCookieDirsMu.Unlock()
	if _, ok := tightenedCookieDirs[dir]; ok {
		return
	}
	tightenedCookieDirs[dir] = struct{}{}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("cookie dir DACL tightening panicked", "dir", dir, "panic", fmt.Sprint(r))
			}
		}()
		// This is the hardening for the auth-cookie file (highest-value
		// secret in the app). Demoted to Debug (matches the config +
		// sidecar + profile dir sites): the common failure is ACCESS_DENIED
		// on a dir created under an elevated/admin context, and on the
		// single-user host this app targets nobody else can read it anyway.
		// Raise the log level to Debug to surface the miss.
		if err := utils.ApplyUserOnlyDACL(dir); err != nil {
			slog.Debug("could not restrict cookie dir to current user", "dir", dir, "err", err)
		}
	}()
}

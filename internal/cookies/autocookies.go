package cookies

import (
	"context"
	"errors"
	"fmt"
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
	mu             sync.Mutex
	profileDir     string
	cookiePath     string
	jar            *CookieJar
	setupProcess   *os.Process
	setupClaimed   bool        // StartSetup slot claim — held from the gate check until the browser process is registered (or the attempt fails)
	setupJob       *processJob // Windows Job Object for setup browser; nil on non-Windows
	refreshCmd     *exec.Cmd   // tracks in-flight headless refresh browser
	setupBrowser   *DetectedBrowser
	browserExited  bool
	cdpPort        int
	cancelled      bool
	lastRefresh    *time.Time
	lastError      *string
	needsRelogin   AutoCookieReloginRequired
	targetPlatform string // "youtube" or "twitch"

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
// empty list, the refresh declines — and the operator reads "Cookie
// Auto-Refresh Ineffective … it either declined to run or found nothing
// usable" about the one platform it was called to fix. Same for a Twitch
// session left holding only twilight-user.
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

	var lastRefreshStr *string
	if s.lastRefresh != nil {
		v := s.lastRefresh.UTC().Format(time.RFC3339)
		lastRefreshStr = &v
	}

	return AutoCookieStatus{
		Configured:            s.profileDir != "",
		SetupInProgress:       s.setupProcess != nil || s.setupClaimed,
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
	if s.setupProcess != nil || s.setupClaimed {
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
	// cancelled is reset here, at claim time, so a CancelSetup arriving
	// DURING the preparation below is observed rather than erased.
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

	if existingData, readErr := os.ReadFile(s.cookiePath); readErr == nil && len(existingData) > 0 {
		netscapeCookies = mergeCookieFiles(string(existingData), netscapeCookies)
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

	// Validate: first check cookie presence, then verify via API if callbacks available
	ytAuth = s.jar.HasYouTubeAuthCookies()
	twAuth = s.jar.HasTwitchAuthCookies()

	// Real API verification (more reliable than just checking cookie presence).
	// When the corresponding Verify callback is nil (test wiring or alternate
	// constructors), the presence check above is the only signal we have —
	// surface that explicitly so callers can't quietly succeed on cookies that
	// are present-but-invalid (audit reports/cookies.md #21).
	ctx, cancel := context.WithTimeout(ctx, authVerifyTimeout)
	defer cancel()
	if ytAuth {
		if s.VerifyYouTubeAuth != nil {
			if verified, err := s.VerifyYouTubeAuth(ctx); err == nil {
				ytAuth = verified
			}
		} else {
			s.logger.Warn("YouTube auth verification callback not wired — reporting based on cookie presence alone")
		}
	}
	if twAuth {
		if s.VerifyTwitchAuth != nil {
			if verified, err := s.VerifyTwitchAuth(ctx); err == nil {
				twAuth = verified
			}
		} else {
			s.logger.Warn("Twitch auth verification callback not wired — reporting based on cookie presence alone")
		}
	}

	if !ytAuth && !twAuth {
		s.logger.Warn("cookies extracted but authentication verification failed")
	}

	// Clear re-login flags for verified platforms
	s.mu.Lock()
	if ytAuth {
		s.needsRelogin["youtube"] = false
	}
	if twAuth {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	// Persist verified platforms to config so we can detect auth loss after restart
	// (matches TS autoCookies.ts persistPlatforms)
	if s.PersistPlatforms != nil {
		s.PersistPlatforms(ytAuth, twAuth)
	}

	now := time.Now()
	s.mu.Lock()
	s.lastRefresh = &now
	s.mu.Unlock()
	s.cleanup()

	// Persist LastRefresh to the sidecar so the next launch doesn't
	// re-run the refresh immediately. Audit reports/cookies.md #48.
	persistedPlatforms := []string{}
	if ytAuth {
		persistedPlatforms = append(persistedPlatforms, "youtube")
	}
	if twAuth {
		persistedPlatforms = append(persistedPlatforms, "twitch")
	}
	if metaErr := SaveMeta(s.cookiePath, CookieMeta{
		LastRefresh: now,
		Platforms:   persistedPlatforms,
	}); metaErr != nil && s.logger != nil {
		s.logger.Warn("could not persist cookies.meta.json", "err", metaErr)
	}

	var verified []string
	if ytAuth {
		verified = append(verified, "YouTube")
	}
	if twAuth {
		verified = append(verified, "Twitch")
	}
	if len(verified) > 0 {
		s.logger.Info("[AutoCookies] Setup complete — verified: " + strings.Join(verified, " + "))
	}

	return ytAuth, twAuth, nil
}

// CancelSetup kills the setup browser.
func (s *AutoCookieService) CancelSetup() {
	s.mu.Lock()
	s.cancelled = true
	s.mu.Unlock()

	s.killSetupProcess()
	s.cleanup()
	s.logger.Info("auto-cookie setup cancelled")
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

// anyVerified is the whole-service bool RefreshCookies has always returned:
// at least one platform is conclusively authenticated.
func (r RefreshResult) anyVerified() bool {
	return r.YouTube == RefreshOK || r.Twitch == RefreshOK
}

// refreshDeclined is the result of a pass that did no work: it says nothing
// about either platform, because it never looked.
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
	return result.anyVerified(), err
}

// RefreshCookiesDetailed performs a headless browser visit to refresh cookies
// and reports the outcome per platform.
func (s *AutoCookieService) RefreshCookiesDetailed(ctx context.Context) (RefreshResult, error) {
	s.mu.Lock()
	if s.setupProcess != nil || s.setupClaimed {
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
			netscapeCookies, err = s.refreshChromium(ctx, browser)
			// Chromium needs no screenshot to prove it ran. refreshChromium
			// waits for the CDP endpoint, navigates, and RETURNS what it read
			// back over that connection — a nil error means the browser was
			// alive and answering, which is proof of the same order as the
			// Firefox screenshot and already in hand.
			browserActed = err == nil
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
	if existingData, readErr := os.ReadFile(s.cookiePath); readErr == nil && len(existingData) > 0 {
		previousCookies = string(existingData)
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
	var restoredPlatforms []string
	if restore := platformsToRestore(pre, map[string]platformAuth{"youtube": postYT, "twitch": postTW}); len(restore) > 0 {
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
	inconclusive := postYT.state == verifyUnknown || postTW.state == verifyUnknown
	var errMsg string
	switch {
	case len(restoredPlatforms) > 0 && inconclusive:
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

// Stop stops the auto-cookie service.
func (s *AutoCookieService) Stop() {
	s.mu.Lock()
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
func killProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	if isWindows() {
		exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Kill()
	}
}

func (s *AutoCookieService) killSetupProcess() {
	s.mu.Lock()
	proc := s.setupProcess
	s.mu.Unlock()

	if proc == nil {
		return
	}

	killProcessTree(proc)
	time.Sleep(taskkillDrainDelay)
}

func (s *AutoCookieService) killRefreshProcess() {
	// RefreshCookies claims the slot with `s.refreshCmd = &exec.Cmd{}`
	// (a sentinel with nil Process) before refreshFirefox/refreshChromium
	// assigns the real cmd. A naive `Process == nil → bail` lets a Stop()
	// during that window leak the real browser when it lands a moment later
	// (audit reports/cookies.md #22). Poll briefly so the kill catches the
	// real process once the launcher publishes it, but cap the wait so Stop()
	// doesn't block indefinitely if the launcher errors before assignment.
	deadline := time.Now().Add(2 * time.Second)
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

func (s *AutoCookieService) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Close the setup Job Object — KILL_ON_JOB_CLOSE terminates any
	// browser process the user left behind even if killSetupProcess didn't.
	if s.setupJob != nil {
		s.setupJob.close()
		s.setupJob = nil
	}
	s.setupProcess = nil
	s.setupBrowser = nil
	s.browserExited = false
	s.cdpPort = 0
	s.cancelled = false
	s.targetPlatform = ""
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

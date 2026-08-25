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
	processTimeout       = 30 * time.Second // overall browser-launch budget for refresh
	authVerifyTimeout    = 15 * time.Second // single VerifyYouTubeAuth / VerifyTwitchAuth call
	refreshOverallBudget = 2 * time.Minute  // periodic refresh: ctx cap end-to-end
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

// refreshPlatforms returns the platforms that have cookies in the jar and need refreshing.
// Order is stable: YouTube first, then Twitch.
func (s *AutoCookieService) refreshPlatforms() []string {
	var platforms []string
	if s.jar.HasYouTubeAuthCookies() {
		platforms = append(platforms, "youtube")
	}
	if s.jar.HasTwitchAuthCookies() {
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

// RefreshCookies performs a headless browser visit to refresh cookies.
func (s *AutoCookieService) RefreshCookies(ctx context.Context) (bool, error) {
	s.mu.Lock()
	if s.setupProcess != nil || s.setupClaimed {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — setup in progress")
		return false, nil
	}
	if s.refreshCmd != nil {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — already refreshing")
		return false, nil
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
			return false, ErrNoBrowserFound
		}
		s.setError("browser profile not found — run setup first")
		return false, fmt.Errorf("run setup first: %w", ErrProfileNotFound)
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
			return false, err
		}
	} else {
		if len(s.refreshPlatforms()) == 0 {
			s.logger.Debug("skipping cookie refresh — no platforms have cookies")
			return false, nil
		}

		s.logger.Info("refreshing cookies via " + browser.Type)

		if isFirefoxBased(browser.Type) {
			netscapeCookies, err = s.refreshFirefox(ctx, browser)
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
		}
	}

	// DPAPI fallback: if the CDP path failed and the user has opted
	// in, try reading cookies directly from their real Chromium-family
	// profile via CryptUnprotectData. Skipped for Firefox-based
	// browsers — Firefox uses cookies.sqlite (no DPAPI involved) and
	// already has its own SQLite-direct path. DECISIONS #6.
	if err != nil && browser != nil && s.DpapiFallback && !isFirefoxBased(browser.Type) {
		s.logger.Warn("CDP refresh failed; attempting DPAPI fallback", "cdp_err", err)
		fallbackCookies, fallbackErr := dpapiExtractAsNetscape()
		if fallbackErr != nil {
			s.logger.Warn("DPAPI fallback also failed; surfacing original CDP error",
				"dpapi_err", fallbackErr)
			// fall through with the original CDP err
		} else {
			s.logger.Info("DPAPI fallback succeeded; using user's signed-in browser cookies")
			netscapeCookies = fallbackCookies
			err = nil
		}
	}

	if err != nil {
		s.setError(err.Error())
		return false, err
	}

	// Merge with existing cookies using temp file + rename for atomicity.
	// previousCookies is kept verbatim so an import that turns out to have
	// damaged a platform can hand that platform's rows back untouched.
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		return false, err
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

	if previousCookies != "" {
		netscapeCookies = mergeCookieFiles(previousCookies, netscapeCookies)
	}
	if err := writeFileAtomic(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		return false, err
	}

	// Reload jar
	if err := s.jar.Load(s.cookiePath); err != nil {
		return false, err
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
		if restoreErr := writeFileAtomic(s.cookiePath, []byte(restored), 0o600); restoreErr != nil {
			s.logger.Error("could not restore pre-import cookies.txt", "err", restoreErr)
		} else if loadErr := s.jar.Load(s.cookiePath); loadErr != nil {
			s.logger.Error("could not reload cookie jar after restoring pre-import cookies.txt", "err", loadErr)
		} else {
			// Re-verify the file we actually kept. Without this, the status
			// below would describe the DISCARDED merged jar and flag a
			// re-login for credentials that were restored and never
			// re-checked — an instruction a container operator cannot even
			// act on.
			postYT, postTW = s.checkPlatformAuth(ctx)
		}
	}

	ytAuth := postYT.ok()
	twAuth := postTW.ok()

	// Update re-login flags based on verification results. Only a CONCLUSIVE
	// failure flags a re-login: an unreachable network told us nothing about
	// the credentials, and sending the user to sign in again over a blip is
	// both wrong and, in a container, impossible to act on.
	ytHasCookies := s.jar.HasYouTubeAuthCookies()
	twHasCookies := s.jar.HasTwitchAuthCookies()

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
		s.mu.Lock()
		s.lastRefresh = &now
		s.lastError = nil
		s.mu.Unlock()

		// Persist LastRefresh to the sidecar (audit cookies.md #48).
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
		s.logger.Info("cookie refresh succeeded", "verified", strings.Join(verified, " + "))
		return true, nil
	}

	// Neither platform verified. Build a targeted message naming only the
	// platforms that actually had cookies worth verifying — if a user only
	// signed in to YouTube, they should not see "Twitch needs re-login".
	// If neither platform even has cookies (e.g. first run before setup),
	// emit no error at all: the refresh completed cleanly, there was just
	// nothing to refresh yet.
	var failed []string
	if ytHasCookies {
		failed = append(failed, "YouTube")
	}
	if twHasCookies {
		failed = append(failed, "Twitch")
	}
	if len(failed) == 0 {
		s.logger.Debug("cookie refresh completed with no cookies to verify")
		s.mu.Lock()
		s.lastError = nil
		s.mu.Unlock()
		return false, nil
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
	s.setError(errMsg)
	s.logger.Warn("refresh completed but auth verification failed", "platforms", strings.Join(failed, ","), "detail", errMsg)
	return false, nil
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
					s.logger.Info("periodic auto-cookie refresh succeeded")
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

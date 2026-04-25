package cookies

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	processTimeout       = 30 * time.Second // overall browser-launch budget for refresh
	authVerifyTimeout    = 15 * time.Second // single VerifyYouTubeAuth / VerifyTwitchAuth call
	refreshOverallBudget = 2 * time.Minute  // periodic refresh: ctx cap end-to-end
	// taskkillDrainDelay is the post-taskkill pause that lets Windows release
	// the process handle before the next cleanup step inspects state. Replaces
	// a bare 300ms literal in killSetupProcess (audit reports/cookies.md #45).
	taskkillDrainDelay = 300 * time.Millisecond
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

// AutoCookieReloginRequired tracks which platforms need manual re-login.
type AutoCookieReloginRequired struct {
	YouTube bool `json:"youtube"`
	Twitch  bool `json:"twitch"`
}

// AutoCookieStatus holds the current status of the auto-cookie service.
type AutoCookieStatus struct {
	Configured         bool                      `json:"configured"`
	SetupInProgress    bool                      `json:"setupInProgress"`
	Browser            *DetectedBrowser          `json:"browser"`
	LastRefresh        *string                   `json:"lastRefresh"`
	LastError          *string                   `json:"lastError"`
	NeedsManualRelogin AutoCookieReloginRequired `json:"needsManualRelogin"`
}

// AutoCookieService manages automatic browser-based cookie extraction.
type AutoCookieService struct {
	mu             sync.Mutex
	profileDir     string
	cookiePath     string
	jar            *CookieJar
	setupProcess   *os.Process
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

	// profileDirErr captures any validation failure on the configured
	// profile directory (e.g. it points at a real browser's profile
	// tree). Computed once at construction so all subprocess-launching
	// entry points can fast-fail with the same message instead of each
	// re-running the check. Audit reports/cookies.md #26.
	profileDirErr error

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
	return &AutoCookieService{
		profileDir:    profileDir,
		cookiePath:    cookiePath,
		jar:           jar,
		profileDirErr: profileDirErr,
		logger:        logger,
	}
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
	// DetectBrowser does filesystem I/O and registry queries — call outside the lock.
	browser := DetectBrowser()

	s.mu.Lock()
	defer s.mu.Unlock()

	var lastRefreshStr *string
	if s.lastRefresh != nil {
		v := s.lastRefresh.UTC().Format(time.RFC3339)
		lastRefreshStr = &v
	}

	return AutoCookieStatus{
		Configured:         s.profileDir != "",
		SetupInProgress:    s.setupProcess != nil,
		Browser:            browser,
		LastRefresh:        lastRefreshStr,
		LastError:          s.lastError,
		NeedsManualRelogin: s.needsRelogin,
	}
}

// FlagManualRelogin marks a platform as needing manual re-login.
func (s *AutoCookieService) FlagManualRelogin(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch platform {
	case "youtube":
		s.needsRelogin.YouTube = true
	case "twitch":
		s.needsRelogin.Twitch = true
	}
}

// StartSetup launches a browser for the user to log in.
func (s *AutoCookieService) StartSetup(platform string) error {
	s.mu.Lock()
	if s.setupProcess != nil {
		s.mu.Unlock()
		return fmt.Errorf("setup already in progress")
	}
	if s.refreshCmd != nil {
		s.mu.Unlock()
		return fmt.Errorf("cookie refresh in progress, please try again shortly")
	}
	s.mu.Unlock()

	browser := DetectBrowser()
	if browser == nil {
		return fmt.Errorf("no supported browser found (Firefox, Chrome, Brave, Edge, Opera, or Waterfox required)")
	}

	if err := os.MkdirAll(s.profileDir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}

	s.mu.Lock()
	s.setupBrowser = browser
	s.lastError = nil
	s.cancelled = false
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
		return false, false, fmt.Errorf("no setup in progress")
	}
	if s.cancelled {
		s.mu.Unlock()
		return false, false, fmt.Errorf("setup was cancelled")
	}
	browser := s.setupBrowser
	s.mu.Unlock()

	var netscapeCookies string

	if isFirefoxBased(browser.Type) {
		s.closeFirefoxGracefully()
		netscapeCookies, err = readFirefoxCookies(s.profileDir)
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
		s.needsRelogin.YouTube = false
	}
	if twAuth {
		s.needsRelogin.Twitch = false
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
	if s.setupProcess != nil {
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

	browser := DetectBrowser()
	if browser == nil {
		s.setError("no browser found for refresh")
		return false, fmt.Errorf("no browser found")
	}

	if _, err := os.Stat(s.profileDir); os.IsNotExist(err) {
		s.setError("browser profile not found — run setup first")
		return false, fmt.Errorf("profile not found")
	}

	if len(s.refreshPlatforms()) == 0 {
		s.logger.Debug("skipping cookie refresh — no platforms have cookies")
		return false, nil
	}

	s.logger.Info("refreshing cookies via " + browser.Type)

	var netscapeCookies string
	var err error

	if isFirefoxBased(browser.Type) {
		netscapeCookies, err = s.refreshFirefox(ctx, browser)
	} else {
		netscapeCookies, err = s.refreshChromium(ctx, browser)
	}

	if err != nil {
		s.setError(err.Error())
		return false, err
	}

	// Merge with existing cookies using temp file + rename for atomicity
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		return false, err
	}
	if existingData, readErr := os.ReadFile(s.cookiePath); readErr == nil && len(existingData) > 0 {
		netscapeCookies = mergeCookieFiles(string(existingData), netscapeCookies)
	}
	if err := writeFileAtomic(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		return false, err
	}

	// Reload jar
	if err := s.jar.Load(s.cookiePath); err != nil {
		return false, err
	}

	// Verify auth via API callbacks (matches TypeScript refreshCookies behavior)
	verifyCtx, verifyCancel := context.WithTimeout(ctx, authVerifyTimeout)
	defer verifyCancel()

	ytAuth := false
	twAuth := false

	if s.jar.HasYouTubeAuthCookies() && s.VerifyYouTubeAuth != nil {
		if verified, err := s.VerifyYouTubeAuth(verifyCtx); err == nil {
			ytAuth = verified
		}
	}
	if s.jar.HasTwitchAuthCookies() && s.VerifyTwitchAuth != nil {
		if verified, err := s.VerifyTwitchAuth(verifyCtx); err == nil {
			twAuth = verified
		}
	}

	// Update re-login flags based on verification results
	ytHasCookies := s.jar.HasYouTubeAuthCookies()
	twHasCookies := s.jar.HasTwitchAuthCookies()

	s.mu.Lock()
	if !ytAuth && ytHasCookies {
		s.needsRelogin.YouTube = true
	}
	if !twAuth && twHasCookies {
		s.needsRelogin.Twitch = true
	}
	if ytAuth {
		s.needsRelogin.YouTube = false
	}
	if twAuth {
		s.needsRelogin.Twitch = false
	}
	s.mu.Unlock()

	if !ytAuth && ytHasCookies {
		s.logger.Warn("YouTube auth verification failed after refresh — manual re-login required")
	}
	if !twAuth && twHasCookies {
		s.logger.Warn("Twitch auth verification failed after refresh — manual re-login required")
	}

	// Consider refresh successful if any platform verified
	if ytAuth || twAuth {
		now := time.Now()
		s.mu.Lock()
		s.lastRefresh = &now
		s.lastError = nil
		s.mu.Unlock()

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
	errMsg := strings.Join(failed, " + ") + " auth verification failed — manual re-login required"
	s.setError(errMsg)
	s.logger.Warn("refresh completed but auth verification failed", "platforms", strings.Join(failed, ","))
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
func (s *AutoCookieService) shouldSkipPeriodicRefresh() bool {
	if s.HasActiveJobs == nil {
		return false
	}
	return !s.HasActiveJobs()
}

// StartPeriodicRefresh starts a background goroutine that periodically
// refreshes cookies via headless browser visit. When HasActiveJobs is set,
// ticks where it returns false are skipped to avoid spawning a headless
// browser when nothing needs authenticated YouTube/Twitch access.
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
				if s.shouldSkipPeriodicRefresh() {
					s.logger.Debug("periodic auto-cookie refresh skipped — no active jobs")
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
		time.Sleep(50 * time.Millisecond)
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
// preventing corruption on partial failure.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return fmt.Errorf("write temp cookie file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp cookie file: %w", err)
	}
	return nil
}

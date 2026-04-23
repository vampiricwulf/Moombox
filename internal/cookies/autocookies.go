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
	processTimeout   = 30 * time.Second
)

// platformRefreshURLs maps platform names to their refresh URLs.
var platformRefreshURLs = map[string]string{
	"youtube": refreshURL,
	"twitch":  twitchRefreshURL,
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
	return &AutoCookieService{
		profileDir: profileDir,
		cookiePath: cookiePath,
		jar:        jar,
		logger:     logger,
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

	// Real API verification (more reliable than just checking cookie presence)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if ytAuth && s.VerifyYouTubeAuth != nil {
		if verified, err := s.VerifyYouTubeAuth(ctx); err == nil {
			ytAuth = verified
		}
	}
	if twAuth && s.VerifyTwitchAuth != nil {
		if verified, err := s.VerifyTwitchAuth(ctx); err == nil {
			twAuth = verified
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
	verifyCtx, verifyCancel := context.WithTimeout(ctx, 15*time.Second)
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

// StartPeriodicRefresh starts a background goroutine that periodically
// refreshes cookies via headless browser visit.
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
				s.logger.Debug("periodic auto-cookie refresh triggered")
				refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
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
	time.Sleep(300 * time.Millisecond)
}

func (s *AutoCookieService) killRefreshProcess() {
	s.mu.Lock()
	cmd := s.refreshCmd
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	killProcessTree(cmd.Process)
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

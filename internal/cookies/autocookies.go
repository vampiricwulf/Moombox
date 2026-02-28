package cookies

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"nhooyr.io/websocket"
)

// Firefox user.js preferences to suppress first-run dialogs.
const firefoxUserJS = `user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("datareporting.policy.dataSubmissionPolicyBypassNotification", true);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("browser.rights.3.shown", true);
`

const (
	loginURL          = "https://accounts.google.com/ServiceLogin?service=youtube"
	refreshURL        = "https://www.youtube.com"
	twitchLoginURL    = "https://www.twitch.tv/login"
	twitchRefreshURL  = "https://www.twitch.tv"
	processTimeoutMs  = 30_000
	cdpPollIntervalMs = 500
	cdpPollTimeoutMs  = 15_000
)

// Chromium lock files that prevent headless launch when a headed session was killed.
var chromiumLockFiles = []string{"lockfile", "SingletonLock", "SingletonSocket", "SingletonCookie"}

// DetectedBrowser holds info about a detected browser.
type DetectedBrowser struct {
	Type string // "firefox", "edge", "chrome"
	Path string
	Name string
}

// AutoCookieReloginRequired tracks which platforms need manual re-login.
type AutoCookieReloginRequired struct {
	YouTube bool `json:"youtube"`
	Twitch  bool `json:"twitch"`
}

// AutoCookieStatus holds the current status of the auto-cookie service.
type AutoCookieStatus struct {
	Configured         bool                       `json:"configured"`
	SetupInProgress    bool                       `json:"setupInProgress"`
	Browser            *DetectedBrowser           `json:"browser"`
	LastRefresh        *string                    `json:"lastRefresh"`
	LastError          *string                    `json:"lastError"`
	NeedsManualRelogin AutoCookieReloginRequired  `json:"needsManualRelogin"`
}

// AutoCookieService manages automatic browser-based cookie extraction.
type AutoCookieService struct {
	mu             sync.Mutex
	profileDir     string
	cookiePath     string
	jar            *CookieJar
	setupProcess   *os.Process
	setupCmd       *exec.Cmd
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
	return &AutoCookieService{
		profileDir: profileDir,
		cookiePath: cookiePath,
		jar:        jar,
		logger:     logger,
	}
}

// GetStatus returns the current auto-cookie status.
func (s *AutoCookieService) GetStatus() AutoCookieStatus {
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
		Browser:            DetectBrowser(),
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
	s.mu.Unlock()

	browser := DetectBrowser()
	if browser == nil {
		return fmt.Errorf("no supported browser found (Firefox, Edge, or Chrome required)")
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

	if browser.Type == "firefox" {
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

	if browser.Type == "firefox" {
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

	// Merge with existing cookies (new cookies take priority)
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		s.cleanup()
		return false, false, err
	}

	if existingData, readErr := os.ReadFile(s.cookiePath); readErr == nil && len(existingData) > 0 {
		netscapeCookies = mergeCookieFiles(string(existingData), netscapeCookies)
	}

	// Write merged cookies file
	if err := os.WriteFile(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		s.cleanup()
		return false, false, err
	}

	// Reload jar and verify
	if err := s.jar.Load(s.cookiePath); err != nil {
		s.cleanup()
		return false, false, err
	}

	// Validate: first check cookie presence, then verify via API if callbacks available
	ytAuth = s.jar.HasAuthCookies()
	twAuth = s.jar.GetCookie("auth-token") != ""

	// Real API verification (more reliable than just checking cookie presence)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

// NavigateToTwitch navigates the Chromium setup browser to Twitch login.
func (s *AutoCookieService) NavigateToTwitch() error {
	s.mu.Lock()
	if s.setupProcess == nil || s.setupBrowser == nil {
		s.mu.Unlock()
		return fmt.Errorf("no setup in progress")
	}
	browser := s.setupBrowser
	port := s.cdpPort
	s.mu.Unlock()

	if browser.Type == "firefox" {
		return nil // Firefox: user navigates manually
	}

	if port == 0 {
		return fmt.Errorf("CDP port not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return cdpNavigate(ctx, port, twitchLoginURL)
}

// RefreshCookies performs a headless browser visit to refresh cookies.
func (s *AutoCookieService) RefreshCookies(ctx context.Context) (bool, error) {
	browser := DetectBrowser()
	if browser == nil {
		s.setError("no browser found for refresh")
		return false, fmt.Errorf("no browser found")
	}

	if _, err := os.Stat(s.profileDir); os.IsNotExist(err) {
		s.setError("browser profile not found — run setup first")
		return false, fmt.Errorf("profile not found")
	}

	s.logger.Info("refreshing cookies via " + browser.Type)

	var netscapeCookies string
	var err error

	if browser.Type == "firefox" {
		netscapeCookies, err = s.refreshFirefox(browser)
	} else {
		netscapeCookies, err = s.refreshChromium(browser)
	}

	if err != nil {
		s.setError(err.Error())
		return false, err
	}

	// Merge with existing cookies (new cookies take priority)
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		return false, err
	}
	if existingData, readErr := os.ReadFile(s.cookiePath); readErr == nil && len(existingData) > 0 {
		netscapeCookies = mergeCookieFiles(string(existingData), netscapeCookies)
	}
	if err := os.WriteFile(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
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

	if s.jar.HasAuthCookies() && s.VerifyYouTubeAuth != nil {
		if verified, err := s.VerifyYouTubeAuth(verifyCtx); err == nil {
			ytAuth = verified
		}
	}
	if s.jar.GetCookie("auth-token") != "" && s.VerifyTwitchAuth != nil {
		if verified, err := s.VerifyTwitchAuth(verifyCtx); err == nil {
			twAuth = verified
		}
	}

	// Update re-login flags based on verification results
	ytHasCookies := s.jar.HasAuthCookies()
	twHasCookies := s.jar.GetCookie("auth-token") != ""

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

	errMsg := "refresh completed but auth verification failed — manual re-login required"
	s.setError(errMsg)
	s.logger.Warn("refresh completed but auth verification failed")
	return false, nil
}

// Stop stops the auto-cookie service.
func (s *AutoCookieService) Stop() {
	s.mu.Lock()
	s.cancelled = true
	s.mu.Unlock()
	s.killSetupProcess()
	s.cleanup()
}

// StartPeriodicRefresh starts a background goroutine that periodically
// refreshes cookies via headless browser visit.
func (s *AutoCookieService) StartPeriodicRefresh(ctx context.Context, interval time.Duration) {
	s.logger.Info("auto-cookie periodic refresh enabled", "interval", interval.String())
	go func() {
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

// --- Firefox flows ---

func (s *AutoCookieService) startFirefoxSetup(browser *DetectedBrowser, url string) error {
	// Write user.js to suppress first-run dialogs
	if err := os.WriteFile(filepath.Join(s.profileDir, "user.js"), []byte(firefoxUserJS), 0o644); err != nil {
		return fmt.Errorf("write user.js: %w", err)
	}

	cmd := exec.Command(browser.Path, "-profile", s.profileDir, "-no-remote", url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firefox: %w", err)
	}

	s.mu.Lock()
	s.setupCmd = cmd
	s.setupProcess = cmd.Process
	s.mu.Unlock()

	// Monitor for exit
	go func() {
		cmd.Wait()
		s.mu.Lock()
		s.browserExited = true
		s.mu.Unlock()
	}()

	return nil
}

func (s *AutoCookieService) closeFirefoxGracefully() {
	s.mu.Lock()
	proc := s.setupProcess
	exited := s.browserExited
	s.mu.Unlock()

	if proc == nil || exited {
		time.Sleep(300 * time.Millisecond)
		return
	}

	s.logger.Debug("sending graceful close to Firefox")

	// Graceful close
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Signal(os.Interrupt)
	}

	// Wait up to 8 seconds for clean exit
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		exited = s.browserExited
		s.mu.Unlock()
		if exited {
			s.logger.Debug("Firefox exited cleanly")
			time.Sleep(300 * time.Millisecond)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Force kill
	s.logger.Warn("Firefox did not exit gracefully, force killing")
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Kill()
	}
	time.Sleep(500 * time.Millisecond)
}

func (s *AutoCookieService) refreshFirefox(browser *DetectedBrowser) (string, error) {
	tempScreenshot := filepath.Join(s.profileDir, "refresh-screenshot.png")
	defer os.Remove(tempScreenshot)

	// Visit YouTube
	cmd1 := exec.Command(browser.Path, "--screenshot", tempScreenshot, "-no-remote", "-profile", s.profileDir, refreshURL)
	if err := runWithTimeout(cmd1, processTimeoutMs); err != nil {
		s.logger.Warn("firefox youtube refresh failed", "err", err)
	}

	// Visit Twitch
	cmd2 := exec.Command(browser.Path, "--screenshot", tempScreenshot, "-no-remote", "-profile", s.profileDir, twitchRefreshURL)
	if err := runWithTimeout(cmd2, processTimeoutMs); err != nil {
		s.logger.Warn("firefox twitch refresh failed", "err", err)
	}

	return readFirefoxCookies(s.profileDir)
}

func readFirefoxCookies(profileDir string) (string, error) {
	dbPath := filepath.Join(profileDir, "cookies.sqlite")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", fmt.Errorf("Firefox cookies.sqlite not found")
	}

	// Retry loop for SQLite WAL lock contention (Firefox may not have fully released the lock)
	const maxRetries = 5
	const retryBackoff = 500 * time.Millisecond

	var lines []string
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryBackoff)
		}

		lines, lastErr = queryFirefoxCookieDB(dbPath)
		if lastErr == nil {
			break
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("query cookies after %d attempts: %w", maxRetries, lastErr)
	}

	result := []string{
		"# Netscape HTTP Cookie File",
		"# Extracted by Moombox auto-cookie service",
		"",
	}
	result = append(result, lines...)

	return strings.Join(result, "\n") + "\n", nil
}

// extractedCookie holds a cookie from browser extraction before Netscape formatting.
type extractedCookie struct {
	domain   string
	httpOnly bool
	path     string
	secure   bool
	expiry   int64
	name     string
	value    string
}

// isRelevantDomain returns true for YouTube/Google/Twitch domains (matching TS).
func isRelevantDomain(domain string) bool {
	return strings.Contains(domain, "youtube.com") ||
		strings.Contains(domain, "google.com") ||
		strings.Contains(domain, "twitch.tv")
}

// isEssentialCookie checks if a cookie should be included in extraction (matching TS).
func isEssentialCookie(name, domain string) bool {
	// YouTube essential cookies
	if essentialYouTubeCookies[name] {
		return true
	}
	// Google domain auth cookies
	if strings.Contains(domain, "google.com") {
		if name == "SID" || name == "HSID" || name == "SSID" || name == "APISID" || name == "SAPISID" ||
			strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P") {
			return true
		}
	}
	// Twitch essential cookies
	if strings.Contains(domain, "twitch.tv") && essentialTwitchCookies[name] {
		return true
	}
	return false
}

// deduplicateAndFormat filters, deduplicates (preferring youtube.com over google.com),
// and formats cookies to Netscape format. Matches TS deduplicateCookies() behavior.
func deduplicateAndFormat(cookies []extractedCookie) []string {
	// Deduplicate by name, preferring youtube.com over google.com
	type entry struct {
		cookie extractedCookie
		domain string
	}
	byName := make(map[string]entry)
	// Preserve order
	var order []string

	for _, c := range cookies {
		if !isRelevantDomain(c.domain) {
			continue
		}
		if !isEssentialCookie(c.name, c.domain) {
			continue
		}

		existing, exists := byName[c.name]
		if exists && strings.Contains(existing.domain, "youtube.com") && !strings.Contains(c.domain, "youtube.com") {
			continue // Already have youtube.com version, skip google.com
		}
		if !exists {
			order = append(order, c.name)
		}
		byName[c.name] = entry{cookie: c, domain: c.domain}
	}

	var lines []string
	for _, name := range order {
		e := byName[name]
		c := e.cookie

		subdomain := "FALSE"
		if strings.HasPrefix(c.domain, ".") {
			subdomain = "TRUE"
		}
		secureStr := "FALSE"
		if c.secure {
			secureStr = "TRUE"
		}
		prefix := ""
		if c.httpOnly {
			prefix = "#HttpOnly_"
		}

		lines = append(lines, fmt.Sprintf("%s%s\t%s\t%s\t%s\t%d\t%s\t%s",
			prefix, c.domain, subdomain, c.path, secureStr, c.expiry, c.name, c.value))
	}
	return lines
}

// queryFirefoxCookieDB opens the Firefox cookie database and reads all cookies.
func queryFirefoxCookieDB(dbPath string) ([]string, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open cookies.sqlite: %w", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT name, value, host, path, expiry, isHttpOnly, isSecure FROM moz_cookies")
	if err != nil {
		return nil, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()

	var collected []extractedCookie
	for rows.Next() {
		var name, value, host, cookiePath string
		var expiry, isHttpOnly, isSecure int64
		if err := rows.Scan(&name, &value, &host, &cookiePath, &expiry, &isHttpOnly, &isSecure); err != nil {
			continue
		}

		collected = append(collected, extractedCookie{
			domain:   host,
			httpOnly: isHttpOnly != 0,
			path:     cookiePath,
			secure:   isSecure != 0,
			expiry:   expiry,
			name:     name,
			value:    value,
		})
	}

	return deduplicateAndFormat(collected), nil
}

// --- Chromium flows ---

func (s *AutoCookieService) startChromiumSetup(browser *DetectedBrowser, url string) error {
	port, err := getFreePort()
	if err != nil {
		return fmt.Errorf("get free port: %w", err)
	}

	cleanChromiumLockFiles(s.profileDir)

	cmd := exec.Command(browser.Path,
		fmt.Sprintf("--user-data-dir=%s", s.profileDir),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		url,
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}

	s.mu.Lock()
	s.setupCmd = cmd
	s.setupProcess = cmd.Process
	s.cdpPort = port
	s.mu.Unlock()

	go func() {
		cmd.Wait()
		s.mu.Lock()
		s.browserExited = true
		s.mu.Unlock()
	}()

	// Wait for CDP to be available
	if err := waitForCDP(context.Background(), port, cdpPollTimeoutMs); err != nil {
		s.killSetupProcess()
		return err
	}

	return nil
}

func (s *AutoCookieService) extractChromiumCookies() (string, error) {
	s.mu.Lock()
	port := s.cdpPort
	s.mu.Unlock()

	if port == 0 {
		return "", fmt.Errorf("CDP port not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return cdpGetCookiesAsNetscape(ctx, port)
}

func (s *AutoCookieService) refreshChromium(browser *DetectedBrowser) (string, error) {
	cleanChromiumLockFiles(s.profileDir)

	port, err := getFreePort()
	if err != nil {
		return "", fmt.Errorf("get free port: %w", err)
	}

	cmd := exec.Command(browser.Path,
		"--headless=new",
		fmt.Sprintf("--user-data-dir=%s", s.profileDir),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-session-crashed-bubble",
		"--disable-features=InfiniteSessionRestore",
		fmt.Sprintf("--remote-debugging-port=%d", port),
	)

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start headless browser: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := waitForCDP(ctx, port, cdpPollTimeoutMs); err != nil {
		return "", err
	}

	// Navigate to YouTube and Twitch (waits for page load, matching TS)
	cdpNavigate(ctx, port, refreshURL)
	cdpNavigate(ctx, port, twitchRefreshURL)

	// Extract cookies
	netscapeCookies, err := cdpGetCookiesAsNetscape(ctx, port)
	if err != nil {
		return "", err
	}

	// Close browser
	cdpCloseBrowser(ctx, port)
	time.Sleep(500 * time.Millisecond)

	return netscapeCookies, nil
}

// --- helpers ---

func (s *AutoCookieService) killSetupProcess() {
	s.mu.Lock()
	proc := s.setupProcess
	s.mu.Unlock()

	if proc == nil {
		return
	}

	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Kill()
	}
	time.Sleep(300 * time.Millisecond)
}

func (s *AutoCookieService) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setupProcess = nil
	s.setupCmd = nil
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

// --- Browser detection ---

// DetectBrowser finds the first available browser: Firefox > Edge > Chrome.
func DetectBrowser() *DetectedBrowser {
	// Firefox
	for _, name := range firefoxPaths() {
		if path, err := exec.LookPath(name); err == nil {
			return &DetectedBrowser{Type: "firefox", Path: path, Name: "Firefox"}
		}
	}
	// Edge
	for _, name := range edgePaths() {
		if path, err := exec.LookPath(name); err == nil {
			return &DetectedBrowser{Type: "edge", Path: path, Name: "Microsoft Edge"}
		}
	}
	// Chrome
	for _, name := range chromePaths() {
		if path, err := exec.LookPath(name); err == nil {
			return &DetectedBrowser{Type: "chrome", Path: path, Name: "Google Chrome"}
		}
	}

	// Check common install locations on Windows using env-based search roots
	// (matches TS browserDetect.ts getSearchRoots + CANDIDATES)
	if runtime.GOOS == "windows" {
		var roots []string
		if pf := os.Getenv("PROGRAMFILES"); pf != "" {
			roots = append(roots, pf)
		}
		if pf86 := os.Getenv("PROGRAMFILES(X86)"); pf86 != "" {
			roots = append(roots, pf86)
		}
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			roots = append(roots, localApp)
		}

		candidates := []struct {
			typ          string
			name         string
			relativePath string
		}{
			{"firefox", "Firefox", `Mozilla Firefox\firefox.exe`},
			{"edge", "Microsoft Edge", `Microsoft\Edge\Application\msedge.exe`},
			{"chrome", "Google Chrome", `Google\Chrome\Application\chrome.exe`},
		}
		seenTypes := make(map[string]bool)
		for _, c := range candidates {
			if seenTypes[c.typ] {
				continue
			}
			for _, root := range roots {
				fullPath := filepath.Join(root, c.relativePath)
				if _, err := os.Stat(fullPath); err == nil {
					return &DetectedBrowser{Type: c.typ, Path: fullPath, Name: c.name}
				}
			}
		}
	}

	return nil
}

func firefoxPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"firefox", "/Applications/Firefox.app/Contents/MacOS/firefox"}
	}
	return []string{"firefox"}
}

func edgePaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"microsoft-edge", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}
	}
	return []string{"msedge", "microsoft-edge"}
}

func chromePaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"google-chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	}
	return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
}

// --- CDP helpers (minimal implementation) ---

func waitForCDP(ctx context.Context, port int, timeoutMs int) error {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(time.Duration(cdpPollIntervalMs) * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for CDP endpoint on port %d", port)
}

func cdpNavigate(ctx context.Context, port int, url string) error {
	// Get available pages
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	var targets []struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		Type                 string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return err
	}

	// Find a page target
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return cdpNavigateAndWait(ctx, t.WebSocketDebuggerURL, url)
		}
	}
	return fmt.Errorf("no page target found")
}

// cdpNavigateAndWait navigates to a URL via CDP and waits for Page.loadEventFired.
// Matches TS CdpClient.navigate(): Page.enable → Page.navigate → wait for load.
func cdpNavigateAndWait(ctx context.Context, wsURL string, targetURL string) error {
	navCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(navCtx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("CDP connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Enable Page events
	enableMsg, _ := json.Marshal(map[string]any{"id": 1, "method": "Page.enable"})
	if err := conn.Write(navCtx, websocket.MessageText, enableMsg); err != nil {
		return fmt.Errorf("CDP Page.enable: %w", err)
	}
	// Read Page.enable response
	conn.Read(navCtx)

	// Send Page.navigate
	navMsg, _ := json.Marshal(map[string]any{
		"id":     2,
		"method": "Page.navigate",
		"params": map[string]any{"url": targetURL},
	})
	if err := conn.Write(navCtx, websocket.MessageText, navMsg); err != nil {
		return fmt.Errorf("CDP Page.navigate: %w", err)
	}

	// Wait for Page.loadEventFired (read messages until we see it or timeout)
	for {
		_, data, err := conn.Read(navCtx)
		if err != nil {
			// Context cancelled or timeout — still acceptable, page likely loaded
			return nil
		}
		var msg struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Method == "Page.loadEventFired" {
			return nil
		}
	}
}

type cdpCookieResult struct {
	Cookies []struct {
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Domain   string  `json:"domain"`
		Path     string  `json:"path"`
		Expires  float64 `json:"expires"`
		HTTPOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
	} `json:"cookies"`
}

func cdpGetCookiesAsNetscape(ctx context.Context, port int) (string, error) {
	// Get version info for the browser websocket URL
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return "", err
	}

	if version.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("no websocket URL in CDP version response")
	}

	// Use Storage.getCookies via the browser-level connection
	result, err := cdpSendCommandWithResult(version.WebSocketDebuggerURL, "Storage.getCookies", nil)
	if err != nil {
		// Fall back to page-level Network.getAllCookies
		fallbackReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", port), nil)
		pagesResp, err2 := http.DefaultClient.Do(fallbackReq)
		if err2 != nil {
			return "", fmt.Errorf("CDP Storage.getCookies failed: %v, fallback failed: %v", err, err2)
		}
		defer func() {
			io.Copy(io.Discard, pagesResp.Body)
			pagesResp.Body.Close()
		}()

		var targets []struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			Type                 string `json:"type"`
		}
		json.NewDecoder(pagesResp.Body).Decode(&targets)

		for _, t := range targets {
			if t.Type == "page" && t.WebSocketDebuggerURL != "" {
				result, err = cdpSendCommandWithResult(t.WebSocketDebuggerURL, "Network.getAllCookies", nil)
				if err == nil {
					break
				}
			}
		}
		// Third fallback: Network.getCookies with explicit URLs (matching TS CdpClient.getAllCookies)
		if err != nil {
			for _, t := range targets {
				if t.Type == "page" && t.WebSocketDebuggerURL != "" {
					params := map[string]interface{}{
						"urls": []string{
							"https://www.youtube.com",
							"https://youtube.com",
							"https://accounts.google.com",
							"https://www.google.com",
							"https://google.com",
							"https://www.twitch.tv",
							"https://twitch.tv",
						},
					}
					result, err = cdpSendCommandWithResult(t.WebSocketDebuggerURL, "Network.getCookies", params)
					if err == nil {
						break
					}
				}
			}
		}
		if err != nil {
			return "", fmt.Errorf("failed to extract cookies via CDP")
		}
	}

	// Parse cookie result
	var cookieResult cdpCookieResult
	if err := json.Unmarshal(result, &cookieResult); err != nil {
		return "", fmt.Errorf("parse CDP cookies: %w", err)
	}

	// Convert to extractedCookie for filtering/deduplication (matching TS cdpCookiesToNetscape)
	var collected []extractedCookie
	for _, c := range cookieResult.Cookies {
		expiry := int64(c.Expires)
		if expiry < 0 {
			expiry = 0
		}
		collected = append(collected, extractedCookie{
			domain:   c.Domain,
			httpOnly: c.HTTPOnly,
			path:     c.Path,
			secure:   c.Secure,
			expiry:   expiry,
			name:     c.Name,
			value:    c.Value,
		})
	}

	filtered := deduplicateAndFormat(collected)

	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Extracted by Moombox auto-cookie service (CDP)")
	lines = append(lines, "")
	lines = append(lines, filtered...)

	return strings.Join(lines, "\n") + "\n", nil
}

func cdpCloseBrowser(ctx context.Context, port int) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	json.NewDecoder(resp.Body).Decode(&version)

	if version.WebSocketDebuggerURL != "" {
		cdpSendCommand(version.WebSocketDebuggerURL, "Browser.close", nil)
	}
}

// cdpSendCommand sends a CDP command via WebSocket (fire-and-forget).
func cdpSendCommand(wsURL string, method string, params map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("CDP connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	msg := map[string]any{"id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)

	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("CDP write: %w", err)
	}

	// Read response (wait up to 5 seconds)
	_, _, _ = conn.Read(ctx)
	return nil
}

// cdpSendCommandWithResult sends a CDP command and returns the result.
func cdpSendCommandWithResult(wsURL string, method string, params map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("CDP connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	msg := map[string]any{"id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)

	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, fmt.Errorf("CDP write: %w", err)
	}

	// Read response
	_, respData, err := conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("CDP read: %w", err)
	}

	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, fmt.Errorf("CDP parse: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("CDP error: %s", result.Error.Message)
	}

	return result.Result, nil
}

// mergeCookieFiles merges existing and new Netscape cookie strings.
// New cookies take priority over existing ones with the same name+domain.
func mergeCookieFiles(existing, newCookies string) string {
	type cookieKey struct {
		name   string
		domain string
	}

	// Parse a Netscape cookie file into ordered keys and a map
	parseCookies := func(content string) ([]cookieKey, map[cookieKey]string) {
		keys := make([]cookieKey, 0)
		m := make(map[cookieKey]string)
		for _, line := range strings.Split(content, "\n") {
			trimmed := line
			// Handle #HttpOnly_ prefix
			if strings.HasPrefix(trimmed, "#HttpOnly_") {
				trimmed = strings.TrimPrefix(trimmed, "#HttpOnly_")
			} else if strings.HasPrefix(trimmed, "#") || strings.TrimSpace(trimmed) == "" {
				continue
			}
			fields := strings.Split(trimmed, "\t")
			if len(fields) < 7 {
				continue
			}
			domain := fields[0]
			name := fields[6]
			k := cookieKey{name: name, domain: domain}
			if _, exists := m[k]; !exists {
				keys = append(keys, k)
			}
			m[k] = line
		}
		return keys, m
	}

	existingKeys, existingMap := parseCookies(existing)
	_, newMap := parseCookies(newCookies)

	// Start with existing cookies, overwrite with new ones
	merged := make(map[cookieKey]string)
	for k, v := range existingMap {
		merged[k] = v
	}
	allKeys := make([]cookieKey, 0, len(existingKeys))
	allKeys = append(allKeys, existingKeys...)

	for k, v := range newMap {
		if _, exists := merged[k]; !exists {
			allKeys = append(allKeys, k)
		}
		merged[k] = v
	}

	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Extracted by Moombox auto-cookie service")
	lines = append(lines, "")
	for _, k := range allKeys {
		if line, ok := merged[k]; ok {
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

func getFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func cleanChromiumLockFiles(profileDir string) {
	for _, name := range chromiumLockFiles {
		os.Remove(filepath.Join(profileDir, name))
	}
}

func runWithTimeout(cmd *exec.Cmd, timeoutMs int) error {
	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		cmd.Process.Kill()
		return fmt.Errorf("process timed out after %dms", timeoutMs)
	}
}

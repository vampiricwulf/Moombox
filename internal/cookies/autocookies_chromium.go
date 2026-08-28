package cookies

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// CDP timing constants. Pulled out of inline literals so a tuning pass is one
// place rather than scattered across every CDP helper (audit
// reports/cookies.md #38).
const (
	cdpPollTimeout    = 15 * time.Second // wait for /json/version readiness
	cdpExtractTimeout = 30 * time.Second // total budget for extractChromiumCookies
	// cdpRefreshTimeout is the total budget for refreshChromium's CDP work:
	// waitForCDP cold start (up to 15s on a busy machine) + SEQUENTIAL
	// per-platform navigations + cookie extraction all share it. 60s gives the
	// two-platform case real headroom (the Firefox path gets processTimeout
	// PER platform launch) while staying well inside refreshOverallBudget.
	cdpRefreshTimeout  = 60 * time.Second
	cdpNavigateTimeout = 30 * time.Second // single Page.navigate + loadEventFired wait
	// cdpCloseFlushDelay is the post-Browser.close pause that lets Chromium
	// flush cookies to disk before our defer kills the process tree. Replaces
	// a bare 500ms literal (audit reports/cookies.md #45).
	cdpCloseFlushDelay = 500 * time.Millisecond
	// cdpEnsureTargetSettleDelay is the post-Target.createTarget pause that
	// lets the new tab register as a CDP page target before the cookie
	// extraction iterates targets again. Audit reports/cookies.md #3.
	cdpEnsureTargetSettleDelay = 500 * time.Millisecond
)

// Chromium lock files that prevent headless launch when a headed session was killed.
var chromiumLockFiles = []string{"lockfile", "SingletonLock", "SingletonSocket", "SingletonCookie"}

func (s *AutoCookieService) startChromiumSetup(browser *DetectedBrowser, url string) error {
	if s.profileDirErr != nil {
		return s.profileDirErr
	}
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
	configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG; Windows: no-op (Job Object below)

	// Create a Job Object before launching so the browser dies if Moombox
	// crashes before cleanup runs. Closed by cleanup() during
	// FinishSetup/CancelSetup. Mirrors the Firefox refresh path.
	job, jobErr := newProcessJob()
	if jobErr != nil {
		s.logger.Warn("failed to create job object for chromium setup", "err", jobErr)
	}

	if err := cmd.Start(); err != nil {
		if job != nil {
			job.close()
		}
		return fmt.Errorf("start browser: %w", err)
	}

	if job != nil {
		if assignErr := job.assign(cmd.Process); assignErr != nil {
			s.logger.Warn("failed to assign chromium setup process to job object",
				"pid", cmd.Process.Pid, "err", assignErr)
		}
	}

	s.mu.Lock()
	// Never overwrite a live Job Object handle. Dropping one leaks the handle
	// AND the browser it holds: nothing else has a reference, so
	// KILL_ON_JOB_CLOSE never fires and the orphan runs until Moombox exits.
	// StartSetup's reap should have cleared this already — if it did not, the
	// invariant broke somewhere and closing is still the right answer, because
	// the only process this job can hold is a setup browser from an attempt the
	// gate has already declared over. It happens BEFORE cmd.Process is assigned
	// to the new job, so it cannot touch the browser we just launched.
	if s.setupJob != nil {
		s.logger.Warn("closing a setup Job Object left behind by an earlier attempt")
		s.setupJob.close()
	}
	s.setupProcess = cmd.Process
	s.setupJob = job
	s.cdpPort = port
	s.mu.Unlock()

	// Capture the process pointer for the wait goroutine. If cleanup() runs
	// before this goroutine wakes (e.g. a CDP failure → cleanup → new
	// StartSetup races with the old wait), we must not flag the *new*
	// setupProcess as exited just because the *old* cmd.Wait() returned.
	// Comparing against s.setupProcess scopes the write to this setup attempt
	// without needing a per-setup struct (audit reports/cookies.md #14).
	proc := cmd.Process
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in browser wait goroutine", "panic", r)
			}
		}()
		cmd.Wait()
		s.mu.Lock()
		if s.setupProcess == proc {
			s.browserExited = true
			// Starts the retention grace — see the Firefox wait goroutine and
			// setupAbandonGrace. Without it this exit was recorded and then
			// ignored, and the slot never freed.
			s.setupRetainedSince = time.Now()
		}
		s.mu.Unlock()
	}()

	// Wait for CDP to be available (use a timeout context so this doesn't block forever)
	cdpCtx, cdpCancel := context.WithTimeout(context.Background(), cdpPollTimeout)
	defer cdpCancel()
	if err := waitForCDP(cdpCtx, port, cdpPollTimeout); err != nil {
		// killSetupProcess only signals the OS process; setupProcess,
		// cdpPort, and setupBrowser remain populated on the struct, so a
		// follow-up StartSetup would see "setup already in progress" and
		// refuse to retry. Clearing via cleanup() lets the user try again
		// immediately after a CDP start-up failure.
		s.killSetupProcess()
		s.cleanup()
		return err
	}

	return nil
}

func (s *AutoCookieService) extractChromiumCookies() (string, error) {
	s.mu.Lock()
	port := s.cdpPort
	platform := s.targetPlatform
	s.mu.Unlock()

	if port == 0 {
		return "", fmt.Errorf("CDP port not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cdpExtractTimeout)
	defer cancel()

	// Ensure a page target on the platform host exists before extraction.
	// Without this, the per-page fallbacks in cdpGetCookiesAsNetscape
	// (Network.getAllCookies / Network.getCookies, both per-page) silently
	// return empty if the user closed all tabs after logging in. Soft-fail:
	// if the ensure step errors, still try extraction — Storage.getCookies
	// is browser-level and may succeed regardless. Audit reports/cookies.md #3.
	if targetURL, ok := platformRefreshURLs[platform]; ok {
		if err := cdpEnsurePageTarget(ctx, port, targetURL); err != nil {
			s.logger.Debug("ensure CDP page target before extraction failed",
				"err", err, "url", targetURL)
		}
	}

	return cdpGetCookiesAsNetscape(ctx, port)
}

// refreshChromium drives one headless Chromium over CDP: navigate to each
// active platform, then read the cookies back out.
//
// The middle return is the ACTED verdict, and it is an AND across every
// navigation this pass makes — the same shape as refreshFirefox's, for the same
// reason.
//
// Read it precisely: TRUE means no navigation reported a transport failure. It
// is NOT the positive artifact the Firefox path demands. cdpNavigateAndWait
// returns nil when its own 30 s budget expires without a Page.loadEventFired
// ("navigation likely complete, just no event"), so a page that connected and
// never finished loading folds in as a success. FALSE, in the other direction,
// says only that the pass has no proof — never that the browser did nothing.
//
// That asymmetry with Firefox — a positive artifact there, the absence of an
// error from a function that swallows its own timeout here — is pre-existing
// and deliberately left alone. It errs toward a MISSED alarm, which is also
// what keeps slow pages from producing spurious Renewed=false. Tightening it
// (having the budget-exhausted branch return an error, so a page that never
// fired its load event stops counting) is a behaviour change with its own
// false-alarm risk and wants its own decision, not a side effect of this one.
//
// No minPlausibleBrowserRefresh-style floor here, deliberately. That
// threshold was measured against the Firefox launcher-handoff path — a
// stamped startTime, a drain wait, a screenshot artifact — none of which
// exist here: this function drives the browser directly over CDP with no
// intermediate launcher process. A floor calibrated on a different
// mechanism, with no timing data of its own, would be a new heuristic
// guessing at a failure mode this path hasn't been shown to have.
func (s *AutoCookieService) refreshChromium(ctx context.Context, browser *DetectedBrowser) (string, bool, error) {
	if s.profileDirErr != nil {
		return "", false, s.profileDirErr
	}
	cleanChromiumLockFiles(s.profileDir)

	port, err := getFreePort()
	if err != nil {
		return "", false, fmt.Errorf("get free port: %w", err)
	}

	// Mirror the anti-automation flags used in the visible setup launch —
	// YouTube raises fraud scores and may rate-limit sessions when a browser
	// advertises itself as automated, which would invalidate the very cookies
	// we are trying to refresh. --window-size + --disable-blink-features
	// together make the headless session look closer to a normal window.
	cmd := exec.Command(browser.Path,
		"--headless=new",
		fmt.Sprintf("--user-data-dir=%s", s.profileDir),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		"--disable-gpu",
		"--disable-session-crashed-bubble",
		"--disable-features=InfiniteSessionRestore",
		"--window-size=1280,720",
		fmt.Sprintf("--remote-debugging-port=%d", port),
	)
	configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG; Windows: no-op (Job Object below)

	// Job Object so an orphaned headless Chromium dies with Moombox if we
	// crash before the defer below runs. Headless leaks are invisible —
	// without this, a crashed refresh leaves a zombie chrome.exe holding
	// the profile lock, breaking the next refresh attempt silently.
	job, jobErr := newProcessJob()
	if jobErr != nil {
		s.logger.Warn("failed to create job object for chromium refresh", "err", jobErr)
	}
	defer func() {
		if job != nil {
			job.close()
		}
	}()

	if err := cmd.Start(); err != nil {
		return "", false, fmt.Errorf("start headless browser: %w", err)
	}

	if job != nil {
		if assignErr := job.assign(cmd.Process); assignErr != nil {
			s.logger.Warn("failed to assign chromium refresh process to job object",
				"pid", cmd.Process.Pid, "err", assignErr)
		}
	}

	s.mu.Lock()
	s.refreshCmd = cmd
	s.mu.Unlock()
	defer func() {
		if cmd.Process != nil {
			killProcessTree(cmd.Process)
			cmd.Wait()
		}
		s.mu.Lock()
		// Restore the claim SENTINEL, not nil: RefreshCookies still has its
		// critical tail to run after this returns (merge → atomic write → jar
		// reload → auth verify → meta save, up to ~15s). Clearing the slot
		// here opened that window to a concurrent RefreshCookies or a
		// StartSetup — a second browser launched against the same profile
		// mid-write. The outer defer in RefreshCookies releases the slot when
		// the whole refresh is done; killRefreshProcess tolerates the
		// nil-Process sentinel (its poll loop caps at 2s). The Firefox path
		// already holds the slot this way.
		s.refreshCmd = &exec.Cmd{}
		s.mu.Unlock()
	}()

	cdpCtx, cancel := context.WithTimeout(ctx, cdpRefreshTimeout)
	defer cancel()

	if err := waitForCDP(cdpCtx, port, cdpPollTimeout); err != nil {
		return "", false, err
	}

	// Navigate to each active platform.
	allNavigated, navFailures := navigateAllPlatforms(cdpCtx, port, s.refreshPlatforms(), cdpNavigate)
	for _, f := range navFailures {
		// Not fatal: another platform may still have navigated, and the
		// profile read below can still return a usable set. Say what happened
		// and keep going — the same degrade-don't-abort shape the Firefox path
		// uses for a drain timeout.
		s.logger.Warn("chromium "+f.platform+" navigation failed; this pass cannot claim it refreshed that platform",
			"err", f.err)
	}

	// Extract cookies
	netscapeCookies, err := cdpGetCookiesAsNetscape(cdpCtx, port)
	if err != nil {
		return "", false, err
	}

	// Close browser, then pause briefly so Chromium flushes any final cookie
	// writes before the defer above tears down the process tree.
	cdpCloseBrowser(cdpCtx, port)
	time.Sleep(cdpCloseFlushDelay)

	return netscapeCookies, allNavigated, nil
}

// navFailure names one platform whose refresh navigation did not succeed.
type navFailure struct {
	platform string
	err      error
}

// navigateAllPlatforms visits each platform's refresh URL and reports whether
// EVERY visit came back without an error, plus the ones that did not so the
// caller can name them. See refreshChromium for how much that does and does not
// establish — navigate swallows its own load-event timeout.
//
// The bool is an AND, and it exists because the per-navigation error used to be
// discarded — the Chromium half of the silent-success hole that the screenshot
// check closed on the Firefox side. Every navigation could fail and the pass
// still reported that it had renewed the credentials, because the only thing
// consulted afterwards was whether the cookie READ succeeded, and that read is
// satisfied by a profile the previous session already populated.
//
// Vacuous truth is the wrong default here for the same reason as on the Firefox
// path: no platforms means nothing was navigated, not "every navigation
// succeeded". refreshChromium is only reached with at least one platform, so
// that guard covers a future caller that does not.
//
// navigate is a parameter so the fold can be tested without a browser; the one
// production caller passes cdpNavigate.
func navigateAllPlatforms(ctx context.Context, port int, platforms []string,
	navigate func(context.Context, int, string) error) (bool, []navFailure) {
	allOK := len(platforms) > 0
	var failures []navFailure
	for _, platform := range platforms {
		if err := navigate(ctx, port, platformRefreshURLs[platform]); err != nil {
			allOK = false
			failures = append(failures, navFailure{platform: platform, err: err})
		}
	}
	return allOK, failures
}

// --- CDP helpers (minimal implementation) ---

func waitForCDP(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 200 * time.Millisecond
	const maxDelay = 2 * time.Second
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
		resp, err := cookiesHTTPClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(delay)
		// Exponential backoff: 200ms -> 400ms -> 800ms -> 1600ms -> 2000ms (capped)
		delay = min(delay*2, maxDelay)
	}
	return fmt.Errorf("timeout waiting for CDP endpoint on port %d", port)
}

func cdpNavigate(ctx context.Context, port int, url string) error {
	// Get available pages
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", port), nil)
	resp, err := cookiesHTTPClient.Do(req)
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

// cdpEnsurePageTarget guarantees that the browser at port has at least one
// CDP `page` target that has been navigated to targetURL. The cookie
// extraction fallbacks in cdpGetCookiesAsNetscape iterate over
// `t.Type == "page"` targets; if the user closed all tabs after logging in,
// those fallbacks silently return empty.
//
// Behavior:
//   - Reuses the first existing page target if one exists, navigating it
//     to targetURL via cdpNavigateAndWait so the cookies for the platform
//     origin are guaranteed to have been touched.
//   - Otherwise spawns a new tab via Target.createTarget on the
//     browser-level WebSocket, then waits a short settle window so the
//     new tab registers as a page target before the caller iterates again.
//
// Audit reports/cookies.md #3.
func cdpEnsurePageTarget(ctx context.Context, port int, targetURL string) error {
	listReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", port), nil)
	listResp, err := cookiesHTTPClient.Do(listReq)
	if err != nil {
		return fmt.Errorf("CDP target list: %w", err)
	}
	var targets []struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		Type                 string `json:"type"`
	}
	decodeErr := json.NewDecoder(listResp.Body).Decode(&targets)
	io.Copy(io.Discard, listResp.Body)
	listResp.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("CDP target list decode: %w", decodeErr)
	}

	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return cdpNavigateAndWait(ctx, t.WebSocketDebuggerURL, targetURL)
		}
	}

	// No page target — fetch the browser-level WebSocket URL and spawn a
	// new tab. This is the user-closed-all-tabs branch the audit calls out.
	versionReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	versionResp, err := cookiesHTTPClient.Do(versionReq)
	if err != nil {
		return fmt.Errorf("CDP version fetch: %w", err)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	decodeErr = json.NewDecoder(versionResp.Body).Decode(&version)
	io.Copy(io.Discard, versionResp.Body)
	versionResp.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("CDP version decode: %w", decodeErr)
	}
	if version.WebSocketDebuggerURL == "" {
		return fmt.Errorf("CDP version response missing webSocketDebuggerUrl")
	}

	if _, err := cdpSendCommandWithResult(version.WebSocketDebuggerURL, "Target.createTarget", map[string]any{"url": targetURL}); err != nil {
		return fmt.Errorf("Target.createTarget: %w", err)
	}
	select {
	case <-time.After(cdpEnsureTargetSettleDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// cdpNavigateAndWait navigates to a URL via CDP and waits for Page.loadEventFired.
// Matches TS CdpClient.navigate(): Page.enable -> Page.navigate -> wait for load.
func cdpNavigateAndWait(ctx context.Context, wsURL string, targetURL string) error {
	navCtx, cancel := context.WithTimeout(ctx, cdpNavigateTimeout)
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
	// Read Page.enable response (response body ignored; an error here is
	// surfaced later when Page.navigate also fails — no logger plumbed
	// into this free function, see audit reports/cookies.md #42).
	_, _, _ = conn.Read(navCtx)

	// Send Page.navigate
	navMsg, _ := json.Marshal(map[string]any{
		"id":     2,
		"method": "Page.navigate",
		"params": map[string]any{"url": targetURL},
	})
	if err := conn.Write(navCtx, websocket.MessageText, navMsg); err != nil {
		return fmt.Errorf("CDP Page.navigate: %w", err)
	}

	// Wait for Page.loadEventFired (read messages until we see it or timeout).
	// Distinguish "context expired" (the read-loop budget ran out — page is
	// likely loaded, just didn't fire the event in time) from "read failed
	// before the budget ran out" (genuine transport failure: return the err
	// so the caller can decide).
	for {
		_, data, err := conn.Read(navCtx)
		if err != nil {
			if navCtx.Err() != nil {
				// Budget exhausted — navigation likely complete, just no event.
				return nil
			}
			return fmt.Errorf("CDP read during navigate: %w", err)
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
	resp, err := cookiesHTTPClient.Do(req)
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

	// Helper: parse a CDP result blob into cookies.
	parseResult := func(raw json.RawMessage) (cdpCookieResult, error) {
		var cookieResult cdpCookieResult
		if err := json.Unmarshal(raw, &cookieResult); err != nil {
			return cookieResult, fmt.Errorf("parse CDP cookies: %w", err)
		}
		return cookieResult, nil
	}

	// Primary: browser-level Storage.getCookies. Returns a result even if
	// empty when the page has no assigned cookies yet, so a zero-length
	// cookies slice is not definitive success — we still try the fallbacks
	// before giving up (finding #17).
	var cookieResult cdpCookieResult
	if result, err := cdpSendCommandWithResult(version.WebSocketDebuggerURL, "Storage.getCookies", nil); err == nil {
		cookieResult, _ = parseResult(result)
	}

	// If Storage.getCookies returned nothing, try the page-level fallbacks.
	if len(cookieResult.Cookies) == 0 {
		fallbackReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", port), nil)
		pagesResp, err2 := cookiesHTTPClient.Do(fallbackReq)
		if err2 != nil {
			return "", fmt.Errorf("CDP cookie fallback listing failed: %w", err2)
		}
		defer func() {
			io.Copy(io.Discard, pagesResp.Body)
			pagesResp.Body.Close()
		}()

		var targets []struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			Type                 string `json:"type"`
		}
		if err := json.NewDecoder(pagesResp.Body).Decode(&targets); err != nil {
			return "", fmt.Errorf("CDP fallback decode: %w", err)
		}

		// Fallback 1: page-level Network.getAllCookies.
		for _, t := range targets {
			if t.Type == "page" && t.WebSocketDebuggerURL != "" {
				if raw, err := cdpSendCommandWithResult(t.WebSocketDebuggerURL, "Network.getAllCookies", nil); err == nil {
					if parsed, perr := parseResult(raw); perr == nil && len(parsed.Cookies) > 0 {
						cookieResult = parsed
						break
					}
				}
			}
		}

		// Fallback 2: Network.getCookies with explicit URL list (matches TS
		// CdpClient.getAllCookies behavior for browsers that no longer
		// expose Storage.getCookies or Network.getAllCookies).
		if len(cookieResult.Cookies) == 0 {
			for _, t := range targets {
				if t.Type == "page" && t.WebSocketDebuggerURL != "" {
					params := map[string]any{
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
					if raw, err := cdpSendCommandWithResult(t.WebSocketDebuggerURL, "Network.getCookies", params); err == nil {
						if parsed, perr := parseResult(raw); perr == nil && len(parsed.Cookies) > 0 {
							cookieResult = parsed
							break
						}
					}
				}
			}
		}

		// Still nothing — surface as an explicit error instead of silently
		// handing back a Netscape file with just headers.
		if len(cookieResult.Cookies) == 0 {
			return "", fmt.Errorf("CDP returned no cookies (Storage.getCookies, Network.getAllCookies, Network.getCookies all empty)")
		}
	}

	// Convert to extractedCookie for filtering/deduplication (matching TS cdpCookiesToNetscape)
	var collected []extractedCookie
	for _, c := range cookieResult.Cookies {
		expiry := max(int64(c.Expires), 0)
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
	resp, err := cookiesHTTPClient.Do(req)
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

	// Block briefly for an ack so the WebSocket flushes before defer Close
	// races the server. The result is intentionally discarded — the only
	// in-tree caller (cdpCloseBrowser → Browser.close) does not care about
	// the response payload, and a Read error here just means the browser
	// went away faster than we could read the ack, which is the desired
	// outcome of Browser.close anyway (audit reports/cookies.md #19).
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

	// Read responses until we find one with a matching command ID.
	// CDP can send interleaved events that don't have an "id" field.
	for {
		_, respData, err := conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("CDP read: %w", err)
		}

		var result struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(respData, &result); err != nil {
			return nil, fmt.Errorf("CDP parse: %w", err)
		}

		// Skip messages without an id (CDP events)
		if result.ID == nil || *result.ID != 1 {
			continue
		}

		if result.Error != nil {
			return nil, fmt.Errorf("CDP error: %s", result.Error.Message)
		}

		return result.Result, nil
	}
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

// lockFileFreshThreshold guards against deleting a lock file that another
// browser instance is actively using. A truly stale lock from a crashed run
// will be older than this; a lock held by a live browser will have been
// touched within seconds (audit reports/cookies.md #9).
const lockFileFreshThreshold = 5 * time.Second

// removeStaleLock unlinks path only if its mtime is older than
// lockFileFreshThreshold. Errors stat'ing the file are treated as "not
// present" — proceed with the unlink attempt, which is itself a no-op for
// missing files.
func removeStaleLock(path string) {
	if info, err := os.Stat(path); err == nil {
		if time.Since(info.ModTime()) < lockFileFreshThreshold {
			return // recently touched — likely held by a live browser
		}
	}
	os.Remove(path)
}

func cleanChromiumLockFiles(profileDir string) {
	// Remove the canonical set first; covers every known Chromium variant.
	for _, name := range chromiumLockFiles {
		removeStaleLock(filepath.Join(profileDir, name))
	}
	// Newer Chrome/Brave/Edge/Opera builds sometimes leave additional
	// Singleton* variants (e.g. SingletonLock.lock); glob them too so a
	// stray lock file from a recent version doesn't block headless launch.
	for _, pattern := range []string{"Singleton*", "*lockfile*"} {
		matches, err := filepath.Glob(filepath.Join(profileDir, pattern))
		if err != nil {
			continue
		}
		for _, m := range matches {
			removeStaleLock(m)
		}
	}
}

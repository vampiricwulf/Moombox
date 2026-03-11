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

	"nhooyr.io/websocket"
)

const (
	cdpPollInterval = 500 * time.Millisecond
	cdpPollTimeout  = 15 * time.Second
)

// Chromium lock files that prevent headless launch when a headed session was killed.
var chromiumLockFiles = []string{"lockfile", "SingletonLock", "SingletonSocket", "SingletonCookie"}

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
		defer func() { recover() }()
		cmd.Wait()
		s.mu.Lock()
		s.browserExited = true
		s.mu.Unlock()
	}()

	// Wait for CDP to be available (use a timeout context so this doesn't block forever)
	cdpCtx, cdpCancel := context.WithTimeout(context.Background(), cdpPollTimeout)
	defer cdpCancel()
	if err := waitForCDP(cdpCtx, port, cdpPollTimeout); err != nil {
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

func (s *AutoCookieService) refreshChromium(ctx context.Context, browser *DetectedBrowser) (string, error) {
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
	s.mu.Lock()
	s.refreshCmd = cmd
	s.mu.Unlock()
	defer func() {
		if cmd.Process != nil {
			killProcessTree(cmd.Process)
			cmd.Wait()
		}
		s.mu.Lock()
		s.refreshCmd = nil
		s.mu.Unlock()
	}()

	cdpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := waitForCDP(cdpCtx, port, cdpPollTimeout); err != nil {
		return "", err
	}

	// Navigate to each active platform
	for _, platform := range s.refreshPlatforms() {
		cdpNavigate(cdpCtx, port, platformRefreshURLs[platform])
	}

	// Extract cookies
	netscapeCookies, err := cdpGetCookiesAsNetscape(cdpCtx, port)
	if err != nil {
		return "", err
	}

	// Close browser
	cdpCloseBrowser(cdpCtx, port)
	time.Sleep(500 * time.Millisecond)

	return netscapeCookies, nil
}

// --- CDP helpers (minimal implementation) ---

func waitForCDP(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 200 * time.Millisecond // Start with shorter delay, backoff to cdpPollInterval
	const maxDelay = 2 * time.Second
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(delay)
		// Exponential backoff: 200ms -> 400ms -> 800ms -> 1600ms -> 2000ms (capped)
		delay = delay * 2
		if delay > maxDelay {
			delay = maxDelay
		}
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
// Matches TS CdpClient.navigate(): Page.enable -> Page.navigate -> wait for load.
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

func cleanChromiumLockFiles(profileDir string) {
	for _, name := range chromiumLockFiles {
		os.Remove(filepath.Join(profileDir, name))
	}
}

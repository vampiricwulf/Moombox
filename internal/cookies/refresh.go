package cookies

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultRefreshInterval = 30 * time.Minute
	authCheckTimeout       = 15 * time.Second
	youtubeGuideURL        = "https://www.youtube.com/youtubei/v1/guide"
	youtubeGuideRefreshURL = "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false"
	twitchValidateURL      = "https://id.twitch.tv/oauth2/validate"
)

// cookieUpdate holds a parsed Set-Cookie value and its expiry timestamp.
type cookieUpdate struct {
	Value  string
	Expiry int64
}

// AuthStatus tracks the authentication state for each platform.
type AuthStatus struct {
	YouTubeAuthenticated bool   `json:"youtubeAuthenticated"`
	TwitchAuthenticated  bool   `json:"twitchAuthenticated"`
	HasYouTubeCookies    bool   `json:"hasYouTubeCookies"`
	LastCheck            string `json:"lastCheck,omitempty"`
	YouTubeError         string `json:"youtubeError,omitempty"`
	TwitchError          string `json:"twitchError,omitempty"`
}

// RefreshService periodically reloads and validates cookies.
type RefreshService struct {
	mu              sync.Mutex
	jar             *CookieJar
	cancel          context.CancelFunc
	status          AuthStatus
	refreshInterval time.Duration

	// Track previous auth state to detect auth → no-auth transitions.
	prevYouTubeAuth bool
	prevTwitchAuth  bool
	hasCheckedOnce  bool

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// OnAuthChange is called when auth status changes.
	OnAuthChange func(status AuthStatus)

	// OnRecoveryNeeded is called when a platform transitions from
	// authenticated to not-authenticated due to genuine auth loss (not
	// a network error and not a never-authenticated state). The platform
	// parameter is "youtube" or "twitch".
	OnRecoveryNeeded func(platform string)
}

// NewRefreshService creates a new cookie refresh service.
// If refreshInterval is zero, the default of 30 minutes is used.
func NewRefreshService(jar *CookieJar, refreshInterval time.Duration, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *RefreshService {
	interval := refreshInterval
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	return &RefreshService{
		jar:             jar,
		refreshInterval: interval,
		logger:          logger,
	}
}

// SetExpectedPlatforms seeds the previous auth state from persisted platforms
// so that auth loss can be detected even if the app restarts after cookies expire.
// Call this before Start().
func (rs *RefreshService) SetExpectedPlatforms(platforms []string) {
	for _, p := range platforms {
		switch p {
		case "youtube":
			rs.prevYouTubeAuth = true
		case "twitch":
			rs.prevTwitchAuth = true
		}
	}
	// If we have persisted platforms, consider the first check as a "subsequent"
	// check so that auth loss transitions can fire immediately.
	if len(platforms) > 0 {
		rs.hasCheckedOnce = true
	}
}

// Start begins the cookie refresh loop.
func (rs *RefreshService) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	rs.cancel = cancel

	// Initial check
	rs.doRefresh(ctx)

	go func() {
		ticker := time.NewTicker(rs.refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rs.doRefresh(ctx)
			}
		}
	}()

	rs.logger.Info("cookie refresh service started",
		"interval", rs.refreshInterval.String())
}

// Stop stops the cookie refresh service.
func (rs *RefreshService) Stop() {
	if rs.cancel != nil {
		rs.cancel()
	}
	rs.logger.Info("cookie refresh service stopped")
}

// GetStatus returns the current auth status.
func (rs *RefreshService) GetStatus() AuthStatus {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.status
}

// CheckNow triggers an immediate cookie refresh and auth check.
func (rs *RefreshService) CheckNow(ctx context.Context) {
	rs.doRefresh(ctx)
}

// CheckYouTubeAuth checks whether the current YouTube cookies are authenticated
// by making a guide API request. This is the public entry point for external
// callers (e.g. AutoCookieService verification).
func (rs *RefreshService) CheckYouTubeAuth(ctx context.Context) (bool, error) {
	return rs.checkYouTubeAuth(ctx)
}

// CheckTwitchAuth checks whether the current Twitch auth token is valid
// by calling the Twitch validation endpoint.
func (rs *RefreshService) CheckTwitchAuth(ctx context.Context) (bool, error) {
	return rs.checkTwitchAuth(ctx)
}

func (rs *RefreshService) doRefresh(ctx context.Context) {
	rs.logger.Debug("refreshing cookies")

	// Reload cookies from file
	if err := rs.jar.Reload(); err != nil {
		rs.logger.Warn("cookie reload failed", "err", err)
	}

	// Check YouTube auth and refresh session cookies in a single request
	// Returns: (authenticated bool, err error)
	//   err != nil       => network/request error (not auth loss)
	//   false, nil       => genuine auth failure or no cookies
	ytAuth, ytErr := rs.checkAndRefreshYouTube(ctx)
	ytErrStr := ""
	if ytErr != nil {
		ytErrStr = ytErr.Error()
		rs.logger.Debug("youtube auth check failed", "err", ytErr)
	}

	// Check Twitch auth
	twAuth, twErr := rs.checkTwitchAuth(ctx)
	twErrStr := ""
	if twErr != nil {
		twErrStr = twErr.Error()
		rs.logger.Debug("twitch auth check failed", "err", twErr)
	}

	rs.mu.Lock()
	prevStatus := rs.status
	prevYT := rs.prevYouTubeAuth
	prevTW := rs.prevTwitchAuth
	hasChecked := rs.hasCheckedOnce

	rs.status = AuthStatus{
		YouTubeAuthenticated: ytAuth,
		TwitchAuthenticated:  twAuth,
		HasYouTubeCookies:    rs.jar.HasYouTubeAuthCookies(),
		LastCheck:            time.Now().UTC().Format(time.RFC3339),
		YouTubeError:         ytErrStr,
		TwitchError:          twErrStr,
	}

	// Update previous auth state tracking.
	// Only update previous state when the check was conclusive (no network error).
	if ytErr == nil {
		rs.prevYouTubeAuth = ytAuth
	}
	if twErr == nil {
		rs.prevTwitchAuth = twAuth
	}
	rs.hasCheckedOnce = true

	changed := rs.status.YouTubeAuthenticated != prevStatus.YouTubeAuthenticated ||
		rs.status.TwitchAuthenticated != prevStatus.TwitchAuthenticated
	rs.mu.Unlock()

	if changed && rs.OnAuthChange != nil {
		rs.OnAuthChange(rs.status)
	}

	// Detect auth loss transitions: previously authenticated -> not authenticated,
	// and the failure is genuine auth loss (err == nil), not a network error.
	// Only trigger after the first check so we can distinguish "was authed" from "never checked".
	if hasChecked && rs.OnRecoveryNeeded != nil {
		// YouTube: was authenticated, now not authenticated, and it's a genuine auth failure (no network error)
		if prevYT && !ytAuth && ytErr == nil {
			rs.logger.Warn("youtube auth lost, triggering recovery")
			rs.OnRecoveryNeeded("youtube")
		}
		// Twitch: was authenticated, now not authenticated, and it's a genuine auth failure (no network error)
		if prevTW && !twAuth && twErr == nil {
			rs.logger.Warn("twitch auth lost, triggering recovery")
			rs.OnRecoveryNeeded("twitch")
		}
	}

	rs.logger.Debug("cookie refresh done",
		"youtube", ytAuth,
		"twitch", twAuth)
}

// setYouTubeHeaders applies the standard YouTube API headers for cookie-authenticated requests.
func setYouTubeHeaders(req *http.Request, cookieHeader, origin, authHeader string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Origin", origin)
}

func (rs *RefreshService) checkYouTubeAuth(ctx context.Context) (bool, error) {
	if !rs.jar.HasYouTubeAuthCookies() {
		return false, nil // No auth cookies
	}

	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, nil
	}

	origin := "https://www.youtube.com"
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, nil
	}

	// POST to YouTube guide endpoint to check auth
	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	body := `{"context":{"client":{"clientName":"WEB","clientVersion":"2.20260101.00.00","hl":"en"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideURL+"?prettyPrint=false", strings.NewReader(body))
	if err != nil {
		return false, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("youtube auth check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	// YouTube always returns 200 even with invalid cookies — parse body
	// and check for authentication indicators in the structured response.
	var data struct {
		ResponseContext struct {
			ServiceTrackingParams []struct {
				Params []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"params"`
			} `json:"serviceTrackingParams"`
			MainAppWebResponseContext struct {
				LoggedIn bool `json:"loggedIn"`
			} `json:"mainAppWebResponseContext"`
		} `json:"responseContext"`
	}

	respBody, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(respBody, &data); err != nil {
		// Fallback to string matching if JSON parse fails
		respStr := string(respBody)
		return strings.Contains(respStr, `"logged_in":"1"`) ||
			strings.Contains(respStr, `"loggedIn":true`), nil
	}

	// Primary: serviceTrackingParams contains logged_in across all Innertube responses
	for _, service := range data.ResponseContext.ServiceTrackingParams {
		for _, param := range service.Params {
			if param.Key == "logged_in" && param.Value == "1" {
				return true, nil
			}
		}
	}

	// Fallback: mainAppWebResponseContext.loggedIn
	if data.ResponseContext.MainAppWebResponseContext.LoggedIn {
		return true, nil
	}

	return false, nil
}

// checkAndRefreshYouTube makes a single guide API request to both check
// YouTube auth status and refresh session cookies from Set-Cookie headers.
// This avoids the redundancy of separate check + refresh requests.
func (rs *RefreshService) checkAndRefreshYouTube(ctx context.Context) (bool, error) {
	if !rs.jar.HasYouTubeAuthCookies() {
		return false, nil
	}

	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, nil
	}

	origin := "https://www.youtube.com"
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	body := `{"context":{"client":{"clientName":"WEB","clientVersion":"2.20260101.00.00","hl":"en"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideRefreshURL, strings.NewReader(body))
	if err != nil {
		return false, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("youtube auth check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	// Read body for auth check
	respBody, _ := io.ReadAll(resp.Body)

	// Check auth status from response
	authenticated := false
	var data struct {
		ResponseContext struct {
			ServiceTrackingParams []struct {
				Params []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"params"`
			} `json:"serviceTrackingParams"`
			MainAppWebResponseContext struct {
				LoggedIn bool `json:"loggedIn"`
			} `json:"mainAppWebResponseContext"`
		} `json:"responseContext"`
	}

	if err := json.Unmarshal(respBody, &data); err != nil {
		respStr := string(respBody)
		authenticated = strings.Contains(respStr, `"logged_in":"1"`) ||
			strings.Contains(respStr, `"loggedIn":true`)
	} else {
		for _, service := range data.ResponseContext.ServiceTrackingParams {
			for _, param := range service.Params {
				if param.Key == "logged_in" && param.Value == "1" {
					authenticated = true
				}
			}
		}
		if !authenticated {
			authenticated = data.ResponseContext.MainAppWebResponseContext.LoggedIn
		}
	}

	// If authenticated, process Set-Cookie headers to refresh session cookies
	if authenticated {
		rs.processYouTubeSetCookies(resp)
	}

	return authenticated, nil
}

// processYouTubeSetCookies parses Set-Cookie headers from a YouTube API response
// and merges updated cookies into the cookie file.
func (rs *RefreshService) processYouTubeSetCookies(resp *http.Response) {
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		rs.logger.Debug("youtube session refresh: no Set-Cookie headers")
		return
	}

	updates := make(map[string]cookieUpdate)
	for _, sc := range setCookies {
		scLower := strings.ToLower(sc)
		if !strings.Contains(scLower, "youtube.com") && !strings.Contains(scLower, "google.com") {
			continue
		}

		parts := strings.Split(sc, ";")
		if len(parts) == 0 {
			continue
		}
		nameValue := strings.TrimSpace(parts[0])
		eqIdx := strings.IndexByte(nameValue, '=')
		if eqIdx <= 0 {
			continue
		}
		name := nameValue[:eqIdx]
		value := nameValue[eqIdx+1:]

		expiry := time.Now().Unix() + 365*24*60*60
		for _, part := range parts[1:] {
			trimmed := strings.TrimSpace(strings.ToLower(part))
			if strings.HasPrefix(trimmed, "expires=") {
				dateStr := strings.TrimSpace(part[strings.Index(part, "=")+1:])
				if t, err := time.Parse(time.RFC1123, dateStr); err == nil {
					expiry = t.Unix()
				} else if t, err := time.Parse("Mon, 02-Jan-2006 15:04:05 MST", dateStr); err == nil {
					expiry = t.Unix()
				}
			} else if strings.HasPrefix(trimmed, "max-age=") {
				if maxAge, err := strconv.ParseInt(strings.TrimSpace(trimmed[8:]), 10, 64); err == nil {
					expiry = time.Now().Unix() + maxAge
				}
			}
		}

		updates[name] = cookieUpdate{Value: value, Expiry: expiry}
	}

	if len(updates) == 0 {
		rs.logger.Debug("youtube session refresh: no relevant cookies to update")
		return
	}

	rs.logger.Debug("youtube session refresh: updating cookies", "count", len(updates))

	if err := rs.updateCookieFile(updates); err != nil {
		rs.logger.Warn("youtube session refresh: failed to update cookie file", "err", err)
		return
	}

	if err := rs.jar.Reload(); err != nil {
		rs.logger.Warn("youtube session refresh: failed to reload jar", "err", err)
	}
}

// updateCookieFile re-reads the cookie file, updates matching cookies with new
// values and expiry, and adds new cookies not already in the file.
func (rs *RefreshService) updateCookieFile(updates map[string]cookieUpdate) error {
	filePath := rs.jar.GetFilePath()
	if filePath == "" {
		return fmt.Errorf("no cookie file path configured")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read cookie file: %w", err)
	}

	var result strings.Builder
	updated := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Check if this is a cookie line that we need to update
		if trimmed != "" && (!strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "#HttpOnly_")) {
			parts := strings.Split(trimmed, "\t")
			if len(parts) >= 7 {
				cookieName := strings.TrimSpace(parts[5])
				if cu, ok := updates[cookieName]; ok {
					// Update value (field 6) and expiry (field 4)
					parts[4] = strconv.FormatInt(cu.Expiry, 10)
					parts[6] = cu.Value
					result.WriteString(strings.Join(parts, "\t"))
					result.WriteString("\n")
					updated[cookieName] = true
					continue
				}
			}
		}

		result.WriteString(line)
		result.WriteString("\n")
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan cookie file: %w", err)
	}

	// Add new cookies that weren't found in the existing file
	for name, cu := range updates {
		if updated[name] {
			continue
		}
		// Determine domain and secure flag
		domain := ".youtube.com"
		if strings.Contains(strings.ToUpper(name), "GOOGLE") {
			domain = ".google.com"
		}
		secure := "FALSE"
		if strings.HasPrefix(name, "__Secure-") {
			secure = "TRUE"
		}
		// Netscape format: domain, include_subdomains, path, secure, expiry, name, value
		result.WriteString(fmt.Sprintf("%s\tTRUE\t/\t%s\t%d\t%s\t%s\n",
			domain, secure, cu.Expiry, name, cu.Value))
		rs.logger.Debug("added new cookie to file", "name", name)
		updated[name] = true
	}

	if err := os.WriteFile(filePath, []byte(result.String()), 0600); err != nil {
		return fmt.Errorf("write cookie file: %w", err)
	}

	if len(updated) > 0 {
		rs.logger.Debug("updated cookies in file", "updated", len(updated))
	}

	return nil
}

func (rs *RefreshService) checkTwitchAuth(ctx context.Context) (bool, error) {
	if !rs.jar.HasTwitchAuthCookies() {
		return false, nil // No auth token
	}
	token := rs.jar.GetCookie("auth-token")

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, twitchValidateURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "OAuth "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("twitch auth check: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	return resp.StatusCode == http.StatusOK, nil
}

package cookies

import (
	"bufio"
	"bytes"
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

// cookiesHTTPClient is a shared HTTP client with a timeout for cookie refresh requests.
var cookiesHTTPClient = &http.Client{Timeout: 30 * time.Second}

const (
	defaultRefreshInterval = 30 * time.Minute
	authCheckTimeout       = 15 * time.Second
	youtubeGuideURL        = "https://www.youtube.com/youtubei/v1/guide"
	youtubeGuideRefreshURL = "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false"
	twitchValidateURL      = "https://id.twitch.tv/oauth2/validate"

	// youtubeClientVersion is the WEB client version sent in Innertube API requests.
	// Update this when YouTube bumps the client version — it's used in auth check
	// and session refresh requests. Format: "2.YYYYMMDD.00.00".
	youtubeClientVersion = "2.20260301.00.00"

	// authBodyFallbackLimit caps how much of the response body we promote to a
	// Go string for the JSON-parse-failed fallback path. The real
	// `"logged_in":"1"` / `"loggedIn":true` markers live in the first hundreds
	// of bytes of the responseContext block; scanning past 16KB only inflates
	// memory and increases the surface for accidentally logging cookies or
	// session tokens that may appear deeper in the payload (audit
	// reports/cookies.md #24).
	authBodyFallbackLimit = 16 << 10
)

// cookieUpdate holds a parsed Set-Cookie value, expiry, and authoritative
// domain. Domain is captured from the Set-Cookie Domain= attribute so new
// rows can be written under the correct host instead of guessing from the
// cookie name.
type cookieUpdate struct {
	Value  string
	Expiry int64
	Domain string // e.g. ".youtube.com" — from Set-Cookie Domain= when present
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
	mu              sync.RWMutex
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

	// OnAuthRecovered is called when a platform transitions from
	// not-authenticated to authenticated (the inverse of OnRecoveryNeeded).
	// Useful for waking jobs that were parked in the COOKIES? status.
	OnAuthRecovered func(platform string)
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
	rs.mu.Lock()
	defer rs.mu.Unlock()

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
	rs.mu.Lock()
	rs.cancel = cancel
	rs.mu.Unlock()

	// Initial check
	rs.doRefresh(ctx)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				rs.logger.Error("cookie refresh goroutine panic", "panic", r)
			}
		}()

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
	rs.mu.Lock()
	cancel := rs.cancel
	rs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rs.logger.Info("cookie refresh service stopped")
}

// GetStatus returns the current auth status.
func (rs *RefreshService) GetStatus() AuthStatus {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
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

	// Detect recovery transitions: previously not authenticated -> now authenticated.
	// Fired so callers can wake jobs parked in COOKIES? state.
	if hasChecked && rs.OnAuthRecovered != nil {
		if !prevYT && ytAuth && ytErr == nil {
			rs.logger.Info("youtube auth recovered")
			rs.OnAuthRecovered("youtube")
		}
		if !prevTW && twAuth && twErr == nil {
			rs.logger.Info("twitch auth recovered")
			rs.OnAuthRecovered("twitch")
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

// youtubeGuideRequestBody returns the standard Innertube WEB request body
// for /youtubei/v1/guide. Centralised here so a clientVersion bump only
// touches one site (audit reports/cookies.md #35).
func youtubeGuideRequestBody() string {
	return `{"context":{"client":{"clientName":"WEB","clientVersion":"` + youtubeClientVersion + `","hl":"en"}}}`
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

	body := youtubeGuideRequestBody()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideURL+"?prettyPrint=false", strings.NewReader(body))
	if err != nil {
		return false, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := cookiesHTTPClient.Do(req)
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return false, fmt.Errorf("read YouTube auth response: %w", err)
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		// Fallback to string matching if JSON parse fails. Cap the slice so
		// the resulting Go string never carries the full multi-MB payload —
		// the auth marker lives in the first few hundred bytes (#24).
		respStr := string(respBody[:min(len(respBody), authBodyFallbackLimit)])
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

	body := youtubeGuideRequestBody()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideRefreshURL, strings.NewReader(body))
	if err != nil {
		return false, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("youtube auth check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	// Read body for auth check
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return false, fmt.Errorf("read YouTube auth response: %w", err)
	}

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
		// Same cap as the checkYouTubeAuth fallback (#24).
		respStr := string(respBody[:min(len(respBody), authBodyFallbackLimit)])
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
		// Cheap early filter before tokenizing — only cookies that mention
		// youtube.com or google.com anywhere in the Set-Cookie string are
		// candidates. The authoritative check against the parsed Domain=
		// attribute happens below via domainMatches so hosts like
		// "fakegoogle.com" that merely embed the substring are rejected.
		scLower := strings.ToLower(sc)
		if !strings.Contains(scLower, "youtube.com") && !strings.Contains(scLower, "google.com") {
			continue
		}

		parts := strings.Split(sc, ";")
		if len(parts) == 0 {
			continue
		}
		nameValue := strings.TrimSpace(parts[0])
		name, value, ok := strings.Cut(nameValue, "=")
		if !ok || name == "" {
			continue
		}

		now := time.Now().Unix()
		expiry := now + 365*24*60*60
		skipCookie := false
		domainAttr := ""
		for _, part := range parts[1:] {
			trimmed := strings.TrimSpace(strings.ToLower(part))
			if strings.HasPrefix(trimmed, "expires=") {
				_, dateStr, _ := strings.Cut(part, "=")
				dateStr = strings.TrimSpace(dateStr)
				if t, err := time.Parse(time.RFC1123, dateStr); err == nil {
					expiry = t.Unix()
				} else if t, err := time.Parse("Mon, 02-Jan-2006 15:04:05 MST", dateStr); err == nil {
					expiry = t.Unix()
				} else if t, err := time.Parse(time.RFC1123Z, dateStr); err == nil {
					expiry = t.Unix()
				}
				// If all date formats fail, keep the default expiry
			} else if strings.HasPrefix(trimmed, "max-age=") {
				if maxAge, err := strconv.ParseInt(strings.TrimSpace(trimmed[8:]), 10, 64); err == nil {
					// Negative or zero max-age means the cookie should be deleted — skip it
					if maxAge <= 0 {
						skipCookie = true
						break
					}
					expiry = now + maxAge
				}
			} else if strings.HasPrefix(trimmed, "domain=") {
				_, dom, _ := strings.Cut(part, "=")
				domainAttr = strings.TrimSpace(dom)
			}
		}
		if skipCookie {
			continue
		}

		// Normalize domain so the Netscape row uses a leading-dot form when
		// the Set-Cookie explicitly said Domain= (which implies subdomain
		// scope per RFC 6265).
		if domainAttr != "" && !strings.HasPrefix(domainAttr, ".") {
			domainAttr = "." + domainAttr
		}

		// When the server supplied Domain=, reject anything that is not an
		// actual YouTube or Google host — blocks the corner case where the
		// early substring pre-filter let through a Set-Cookie carrying a
		// Domain= for e.g. accounts.google.com.evil.tld.
		if domainAttr != "" && !isYouTubeDomain(domainAttr) && !isGoogleDomain(domainAttr) {
			continue
		}

		updates[name] = cookieUpdate{Value: value, Expiry: expiry, Domain: domainAttr}
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
//
// Behavior notes relative to the original implementation:
//   - Every row matching an updated cookie name is refreshed (per finding #4),
//     not just the first one. Leaving stale duplicates on .google.com while a
//     fresh value lands on .youtube.com caused silent cookie drift on legacy
//     files that contained multiple domain variants of the same name.
//   - The Netscape "include subdomains" flag is derived from whether the
//     domain begins with "." (finding #5) instead of being hardcoded TRUE.
//   - Domain for newly-inserted rows is taken from the Set-Cookie Domain=
//     attribute when the server provided one (finding #40); falling back to
//     the legacy .youtube.com / .google.com heuristic only as a last resort.
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
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Netscape cookie files occasionally contain values that push a single
	// line past bufio.Scanner's default 64KiB buffer; bump the ceiling to
	// 1MiB so an oversized line surfaces as an error below instead of a
	// silently truncated row.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Check if this is a cookie line that we need to update.
		// Every matching row is rewritten (not just the first) so multi-domain
		// duplicates do not drift out of sync with the refreshed values.
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
		domain := cu.Domain
		if domain == "" {
			// Fallback when the Set-Cookie lacked Domain=. Prefer YouTube;
			// Google-only auth cookies are only emitted by google.com paths.
			domain = ".youtube.com"
			if isGoogleOnlyAuthName(name) {
				domain = ".google.com"
			}
		}
		// Subdomain flag follows RFC 6265: leading-dot domain = include
		// subdomains. The legacy code hardcoded TRUE even for no-dot domains.
		subdomains := "FALSE"
		if strings.HasPrefix(domain, ".") {
			subdomains = "TRUE"
		}
		secure := "FALSE"
		if strings.HasPrefix(name, "__Secure-") {
			secure = "TRUE"
		}
		// Netscape format: domain, include_subdomains, path, secure, expiry, name, value
		if _, werr := fmt.Fprintf(&result, "%s\t%s\t/\t%s\t%d\t%s\t%s\n",
			domain, subdomains, secure, cu.Expiry, name, cu.Value); werr != nil {
			return fmt.Errorf("write new cookie row: %w", werr)
		}
		rs.logger.Debug("added new cookie to file", "name", name, "domain", domain)
		updated[name] = true
	}

	// Write via temp file + rename to prevent corruption on partial failure
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(result.String()), 0o600); err != nil {
		return fmt.Errorf("write temp cookie file: %w", err)
	}
	if err := os.Rename(tmpPath, filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename cookie file: %w", err)
	}

	if len(updated) > 0 {
		rs.logger.Debug("updated cookies in file", "updated", len(updated))
	}

	return nil
}

// isGoogleOnlyAuthName returns true for cookie names that live on the
// google.com domain (not youtube.com) in a typical Google session. The
// legacy code used strings.Contains(name, "GOOGLE") which matched nothing
// real — most google.com auth cookies are named SID, HSID, SSID, APISID,
// SAPISID, or the __Secure- variants.
func isGoogleOnlyAuthName(name string) bool {
	switch name {
	case "SID", "HSID", "SSID", "APISID", "SAPISID":
		return true
	}
	return strings.HasPrefix(name, "__Secure-1P") || strings.HasPrefix(name, "__Secure-3P")
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

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("twitch auth check: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	return resp.StatusCode == http.StatusOK, nil
}

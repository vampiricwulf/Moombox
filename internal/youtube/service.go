package youtube

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

var (
	// homepageApiKeyRe extracts the Innertube API key from the YouTube homepage.
	// Visitor-data extraction reuses the shared visitorDataRegex from
	// watch_page.go so a rename only has to be edited in one place.
	homepageApiKeyRe = regexp.MustCompile(`"INNERTUBE_API_KEY":"([^"]+)"`)
)

// Service is the YouTube service facade wrapping player API, auth, and format selection.
type Service struct {
	Auth        *Auth
	PlayerAPI   *PlayerAPI
	Cookies     *cookies.CookieJar
	visitorData string // Cached visitor data from watch page
	vdMu        sync.RWMutex
	initOnce    sync.Once
	logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewService creates a new YouTube service.
func NewService(jar *cookies.CookieJar, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Service {
	auth := NewAuth(jar, logger)
	playerAPI := NewPlayerAPI(auth, logger)

	svc := &Service{
		Auth:      auth,
		PlayerAPI: playerAPI,
		Cookies:   jar,
		logger:    logger,
	}

	// Wire visitor data capture from watch page fetches
	playerAPI.OnVisitorData = func(vd string) {
		svc.SetVisitorData(vd)
	}

	return svc
}

// Init performs one-time initialization: fetches YouTube homepage to extract
// visitor data and API key. Safe to call multiple times (idempotent).
func (s *Service) Init(ctx context.Context) {
	s.initOnce.Do(func() {
		// Load/sync cookies
		if err := s.Auth.SyncCookies(); err != nil {
			s.logger.Warn("[YouTube] SyncCookies failed during init", "error", err)
		}

		// Fetch YouTube homepage for visitor data and API key
		headers := map[string]string{
			"User-Agent": constants.UserAgents.Web,
		}
		if ch := s.Auth.GetCookieHeader(); ch != "" {
			headers["Cookie"] = ch
		}
		body, err := utils.FetchBody(ctx, constants.YouTubeURLs.Base, 15*time.Second, headers)
		if err != nil {
			s.logger.Warn("[YouTube] Failed to fetch homepage", "error", err)
			return
		}
		html := string(body)

		// Extract visitor data
		if m := visitorDataRegex.FindStringSubmatch(html); m != nil {
			s.vdMu.Lock()
			s.visitorData = m[1]
			s.vdMu.Unlock()
			s.logger.Debug("[YouTube] Visitor data extracted", "prefix", m[1][:min(30, len(m[1]))])
		}

		// Extract API key
		if m := homepageApiKeyRe.FindStringSubmatch(html); m != nil {
			s.PlayerAPI.SetAPIKey(m[1])
			s.logger.Debug("[YouTube] API key extracted", "key", m[1])
		}
	})
}

// FetchWatchPageHtml fetches the raw HTML of a video's watch page.
// Used for chat continuation token extraction.
func (s *Service) FetchWatchPageHtml(ctx context.Context, videoID string) (string, error) {
	result, err := FetchWatchPage(ctx, videoID, s.Auth.GetCookieHeader())
	if err != nil {
		return "", err
	}
	return result.HTML, nil
}

// GetApiKey returns the current Innertube API key.
func (s *Service) GetApiKey() string {
	return s.PlayerAPI.apiKey
}

// GetVideoInfo fetches video info, using authentication if cookies are available.
func (s *Service) GetVideoInfo(ctx context.Context, videoID string) (*VideoInfo, error) {
	// Sync cookies (may have been refreshed since last call)
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed", "error", err)
	}

	if s.Auth.HasAuthCookies() {
		return s.PlayerAPI.GetVideoInfoAuthenticated(ctx, videoID)
	}
	return s.PlayerAPI.GetVideoInfoPublic(ctx, videoID)
}

// ProbeVideoStatus performs a lightweight probe (ANDROID_VR, no auth).
// Uses cached visitor data if available.
func (s *Service) ProbeVideoStatus(ctx context.Context, videoID string) (*VideoInfo, error) {
	s.vdMu.RLock()
	vd := s.visitorData
	s.vdMu.RUnlock()
	return s.PlayerAPI.ProbeVideoStatus(ctx, videoID, vd)
}

// SetVisitorData stores visitor data extracted from a watch page for use in probes.
func (s *Service) SetVisitorData(vd string) {
	if vd != "" {
		s.vdMu.Lock()
		s.visitorData = vd
		s.vdMu.Unlock()
	}
}

// ProbeVideoStatusAuthenticated performs an authenticated probe using TV_DOWNGRADED.
// Used for polling members-only upcoming streams.
func (s *Service) ProbeVideoStatusAuthenticated(ctx context.Context, videoID string) (*VideoInfo, error) {
	s.vdMu.RLock()
	vd := s.visitorData
	s.vdMu.RUnlock()
	return s.PlayerAPI.ProbeVideoStatusAuthenticated(ctx, videoID, vd)
}

// DecryptDashManifestUrl decrypts the n-parameter in a DASH manifest URL.
func (s *Service) DecryptDashManifestUrl(ctx context.Context, dashURL, playerURL string) string {
	return s.PlayerAPI.DecryptDashManifestUrl(ctx, dashURL, playerURL)
}

// DecryptNParamInUrl decrypts the n-parameter in any URL.
// Handles both query string (?n=...) and path-based (/n/{value}/) formats.
func (s *Service) DecryptNParamInUrl(ctx context.Context, rawURL, playerURL string) string {
	return s.PlayerAPI.DecryptNParamInUrl(ctx, rawURL, playerURL)
}

// ReloadCookies forces a reload of cookies from the cookie file.
func (s *Service) ReloadCookies() error {
	return s.Auth.SyncCookies()
}

// HasAuthCookies returns true if valid authentication cookies are present.
func (s *Service) HasAuthCookies() bool {
	return s.Auth.HasAuthCookies()
}

// GetVisitorData returns the cached visitor data extracted from a watch page.
func (s *Service) GetVisitorData() string {
	s.vdMu.RLock()
	vd := s.visitorData
	s.vdMu.RUnlock()
	return vd
}

// GetCookieHeader returns the Cookie header string for authenticated requests.
func (s *Service) GetCookieHeader() string {
	return s.Auth.GetCookieHeader()
}

// GetAuthState returns the current authentication state for status display.
func (s *Service) GetAuthState() map[string]bool {
	return map[string]bool{
		"isLoggedIn": s.Auth.HasAuthCookies(),
		"hasCookies": s.Auth.GetCookieHeader() != "",
	}
}

// GetFormats fetches available formats and returns them with best-selection info.
func (s *Service) GetFormats(ctx context.Context, videoID string, maxRes int, prefer60fps bool) (*VideoInfo, *SelectedFormats, error) {
	info, err := s.GetVideoInfo(ctx, videoID)
	if err != nil {
		return nil, nil, err
	}

	selected := SelectBestFormatsWithLogger(info.Formats, maxRes, prefer60fps, s.logger)
	return info, &selected, nil
}

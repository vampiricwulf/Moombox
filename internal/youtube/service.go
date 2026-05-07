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

// initRetryInterval bounds how often Init re-fetches the homepage AFTER
// a successful fetch. 1h matches typical visitor-data rotation cadence
// on YouTube and avoids hammering the homepage on every job. Audit
// reports/youtube.md I4.
const initRetryInterval = 1 * time.Hour

// initFastRetryInterval is the shorter floor used after a FAILED fetch.
// Without a separate failure interval, a startup network blip would set
// lastInitAt eagerly and lock the process out for the full 1h with
// empty VisitorData. 60s lets the next pulled job recover quickly while
// still rate-limiting a sustained outage.
const initFastRetryInterval = 60 * time.Second

// visitorDataTTL caps how long a single cached visitor data is reused. The
// sticky-write semantics in SetVisitorData prevent per-call rotation from
// thrashing the POT cache, but a 24/7 archiver session can outlive whatever
// session lifetime YouTube assumed when issuing the value. Six hours matches
// the integrity-token TTL in PotProvider — when the next minter rotation
// happens, fresh visitor data lines up with it.
const visitorDataTTL = 6 * time.Hour

// Service is the YouTube service facade wrapping player API, auth, and format selection.
type Service struct {
	Auth             *Auth
	PlayerAPI        *PlayerAPI
	Cookies          *cookies.CookieJar
	visitorData      string    // Cached visitor data from watch page
	visitorDataSetAt time.Time // When visitorData was last (re)populated; gates TTL-based refresh
	vdMu             sync.RWMutex
	// initMu serialises Init runs; lastInitAt is read+written under this lock.
	// The previous initOnce one-shot meant a transient startup network failure
	// would leave the process with empty VisitorData forever — bad for a 24/7
	// archiver. Audit reports/youtube.md I4.
	initMu        sync.Mutex
	lastInitAt    time.Time
	initSucceeded bool // true after the most recent Init fetched the homepage cleanly
	logger        interface {
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

// Init fetches the YouTube homepage to extract visitor data + API key. Safe
// to call repeatedly: a debounce window (initRetryInterval) suppresses
// rapid re-runs, and a successful fetch backfills only the fields that are
// still empty. Callers can call this defensively from any code path that
// needs fresh VisitorData; the watch-page OnVisitorData callback continues
// to backfill from normal traffic regardless.
//
// Audit reports/youtube.md I4 — was sync.Once which meant any startup-blip
// failure left VisitorData empty for the lifetime of a 24/7 process.
func (s *Service) Init(ctx context.Context) {
	s.initMu.Lock()
	if !s.lastInitAt.IsZero() {
		// initSucceeded gates which interval applies: a successful
		// fetch debounces for 1h, a failed fetch debounces for 60s.
		interval := initRetryInterval
		if !s.initSucceeded {
			interval = initFastRetryInterval
		}
		if time.Since(s.lastInitAt) < interval {
			s.initMu.Unlock()
			return
		}
	}
	// Skip work entirely if both fields are already populated. The lock
	// covers the read-then-set, which is enough — concurrent successful
	// OnVisitorData callbacks could land while we're fetching, in which
	// case we'd waste one homepage fetch but stay correct.
	s.vdMu.RLock()
	haveVD := s.visitorData != ""
	s.vdMu.RUnlock()
	haveKey := s.PlayerAPI.apiKey != "" && s.PlayerAPI.apiKey != constants.DefaultAPIKey
	if haveVD && haveKey {
		s.lastInitAt = time.Now()
		s.initSucceeded = true
		s.initMu.Unlock()
		return
	}
	s.initMu.Unlock()

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
		s.initMu.Lock()
		s.lastInitAt = time.Now()
		s.initSucceeded = false
		s.initMu.Unlock()
		return
	}
	html := string(body)

	// Extract visitor data (only set if currently empty so a fresher value
	// from OnVisitorData isn't clobbered by an older homepage fetch).
	if m := visitorDataRegex.FindStringSubmatch(html); m != nil {
		s.vdMu.Lock()
		if s.visitorData == "" {
			s.visitorData = m[1]
			s.visitorDataSetAt = time.Now()
			s.logger.Debug("[YouTube] Visitor data extracted", "prefix", m[1][:min(30, len(m[1]))])
		}
		s.vdMu.Unlock()
	}

	// Extract API key
	if m := homepageApiKeyRe.FindStringSubmatch(html); m != nil {
		s.PlayerAPI.SetAPIKey(m[1])
		s.logger.Debug("[YouTube] API key extracted", "key", m[1])
	}

	// Mark this Init successful. Set lastInitAt here (not at function
	// entry) so a failed fetch is debounced by the shorter
	// initFastRetryInterval rather than the 1h success interval.
	s.initMu.Lock()
	s.lastInitAt = time.Now()
	s.initSucceeded = true
	s.initMu.Unlock()
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

// SetVisitorData stores visitor data extracted from a watch page. Sticky
// with a TTL: writes when no value is cached OR when the cached value is
// older than visitorDataTTL. Callers that re-fetch the watch page on the hot
// path (quality probes, full fetches) don't trigger a fresh visitor data on
// every call, but a long-running session still rotates eventually so a
// stale value can't pin the POT cache to a dead binding indefinitely.
//
// YouTube rotates VisitorData on each watch-page response, but the
// previously-issued value remains valid for some session window. Pinning
// one VD per session lets the POT session cache hit and avoids a sidecar
// mint per probe; the TTL acts as a safety net against pathological
// stickiness. To force a refresh after a downstream failure (403 burst
// suggesting POT expiry), call InvalidateVisitorData.
func (s *Service) SetVisitorData(vd string) {
	if vd == "" {
		return
	}
	s.vdMu.Lock()
	defer s.vdMu.Unlock()
	if s.visitorData == "" || time.Since(s.visitorDataSetAt) > visitorDataTTL {
		s.visitorData = vd
		s.visitorDataSetAt = time.Now()
	}
}

// InvalidateVisitorData clears the cached visitor data so the next watch-page
// fetch will install a fresh value. Call this when a downstream HTTP error
// (403 burst, POT-rejected response) suggests the bound POT is stale.
func (s *Service) InvalidateVisitorData() {
	s.vdMu.Lock()
	s.visitorData = ""
	s.visitorDataSetAt = time.Time{}
	s.vdMu.Unlock()
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

package youtube

import (
	"log/slog"
	"strconv"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// Auth handles YouTube authentication using cookies.
type Auth struct {
	jar    *cookies.CookieJar
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewAuth creates a new YouTube Auth handler.
func NewAuth(jar *cookies.CookieJar, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Auth {
	return &Auth{jar: jar, logger: logger}
}

// GenerateAuthorizationHeader generates the SAPISIDHASH Authorization header for YouTube API requests.
func (a *Auth) GenerateAuthorizationHeader() string {
	return a.jar.GenerateAuthorizationHeader("https://www.youtube.com")
}

// HasAuthCookies returns true if valid authentication cookies are present.
func (a *Auth) HasAuthCookies() bool {
	return a.jar.HasYouTubeAuthCookies()
}

// HasAnyAuthCookie reports whether YouTube auth was ever configured, as
// opposed to whether a complete working set is present right now. The two
// diverge on a jar holding SAPISID with LOGIN_INFO gone: HasAuthCookies wants
// both and so reads that as never-configured, when it is a configured session
// with broken credentials. See cookies.CookieJar.HasAnyYouTubeAuthCookie.
//
// The "YouTube" qualifier is dropped here on purpose — Auth IS the YouTube
// auth type, matching the existing HasAuthCookies above.
func (a *Auth) HasAnyAuthCookie() bool {
	return a.jar.HasAnyYouTubeAuthCookie()
}

// GetCookieHeader returns the Cookie header string.
func (a *Auth) GetCookieHeader() string {
	return a.jar.GetCookieHeader()
}

// SyncCookies reloads cookies from the file.
func (a *Auth) SyncCookies() error {
	return a.jar.Reload()
}

// GenerateAPIHeaders generates headers for YouTube API requests.
func (a *Auth) GenerateAPIHeaders(client constants.YouTubeClientConfig, ytcfg *YtcfgData) map[string]string {
	origin := "https://www.youtube.com"

	headers := map[string]string{
		"Content-Type":             "application/json",
		"User-Agent":               client.UserAgent,
		"X-YouTube-Client-Name":    client.ClientID,
		"X-YouTube-Client-Version": client.ClientVersion,
		"Origin":                   origin,
	}

	cookieHeader := a.jar.GetCookieHeader()
	if cookieHeader != "" {
		headers["Cookie"] = cookieHeader
	}

	if ytcfg != nil {
		if ytcfg.VisitorData != "" {
			headers["X-Goog-Visitor-Id"] = ytcfg.VisitorData
		}

		if ytcfg.DelegatedSessionID != "" {
			headers["X-Goog-PageId"] = ytcfg.DelegatedSessionID
		}

		if ytcfg.DelegatedSessionID != "" || ytcfg.SessionIndex != nil {
			idx := 0
			if ytcfg.SessionIndex != nil {
				idx = *ytcfg.SessionIndex
			}
			headers["X-Goog-AuthUser"] = strconv.Itoa(idx)
		}
	}

	// Add authorization header
	authHeader := a.jar.GenerateAuthorizationHeader(origin)
	if authHeader != "" {
		headers["Authorization"] = authHeader
		headers["X-Origin"] = origin
	}

	if a.jar.HasYouTubeAuthCookies() {
		headers["X-Youtube-Bootstrap-Logged-In"] = "true"
	}

	a.logger.Debug("[YouTubeAuth] API headers generated",
		slog.Bool("auth", authHeader != ""),
		slog.Bool("cookies", cookieHeader != ""),
	)

	return headers
}

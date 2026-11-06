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

// generateAuthorizationHeaderForUser is the Authorization header for
// www.youtube.com bound to userSessionID ("" for the unbound form); see
// cookies.CookieJar.GenerateAuthorizationHeaderForUser.
func (a *Auth) generateAuthorizationHeaderForUser(userSessionID string) string {
	return a.jar.GenerateAuthorizationHeaderForUser("https://www.youtube.com", userSessionID)
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

// Identity is the account the jar's cookies sign in as — an opaque equality
// token, "" exactly when HasAuthCookies is false. See
// cookies.CookieJar.YouTubeIdentity.
func (a *Auth) Identity() string {
	return a.jar.YouTubeIdentity()
}

// GetCookieHeader returns the Cookie header string.
func (a *Auth) GetCookieHeader() string {
	return a.jar.GetCookieHeader()
}

// PageCookieHeader is the Cookie header for a YouTube HTML page fetched with
// the jar's session: the authenticated watch page, the homepage, the liveness
// and membership pages, chat's continuation page. It is GetCookieHeader with
// yt-dlp's consent and language applied — SOCS=CAI when the jar is not signed
// in, PREF with hl=en and tz=UTC — in the header only (see
// cookies.CookieJar.YouTubePageCookieHeader). The Innertube calls keep
// GetCookieHeader (GenerateAPIHeaders): their contexts state the language and
// time zone themselves.
func (a *Auth) PageCookieHeader() string {
	return a.jar.YouTubePageCookieHeader()
}

// ConsentCookieHeader is the consent and language cookies alone, for a
// YouTube read made without the jar's session: the public cascade's watch
// page, the embed page while the jar is not signed in
// (PlayerAPI.embedPageCookieHeader) and the iframe API script. See
// cookies.CookieJar.YouTubeConsentCookieHeader.
func (a *Auth) ConsentCookieHeader() string {
	return a.jar.YouTubeConsentCookieHeader()
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

		for k, v := range accountSessionHeaders(ytcfg) {
			headers[k] = v
		}
	}

	// The Authorization header, bound to the ytcfg's user session id when it
	// names one, as _generate_cookie_auth_headers binds it
	// (_extract_user_session_id, _base.py:970; YtcfgData.UserSessionID).
	userSessionID := ""
	if ytcfg != nil {
		userSessionID = ytcfg.UserSessionID
	}
	authHeader := a.jar.GenerateAuthorizationHeaderForUser(origin, userSessionID)
	if authHeader != "" {
		headers["Authorization"] = authHeader
		headers["X-Origin"] = origin
	}

	// Only when the ytcfg says LOGGED_IN, upstream's condition (_base.py:
	// 975-976; YtcfgData.LoginState). It used to follow the jar's cookies
	// alone, so a session YouTube's own page had just answered logged out
	// still claimed a logged-in bootstrap on every call made under it.
	//
	// And only from a client that carries a page ytcfg of its own
	// (clientCarriesPageYtcfg), as yt-dlp builds the header per client from
	// that client's player ytcfg (_video.py:3064-3066, generate_api_headers'
	// ytcfg): _download_ytcfg leaves it empty for tv_samsung, android_vr and
	// visionos (_base.py:1013-1024), so upstream's calls by those clients
	// never carry the header. Moombox hands every client the master watch
	// page's ytcfg, or the kept session's (probeYtcfg), for its session
	// identity and Authorization binding (W15, W21) — those stay on every
	// client — but the logged-in bootstrap claim went out on the tv_samsung
	// cascade call and status probe, android_vr and visionos too, a header
	// those clients' own apps never send.
	if ytcfg != nil && ytcfg.LoginState == SessionAuthLoggedIn && clientCarriesPageYtcfg(client) {
		headers["X-Youtube-Bootstrap-Logged-In"] = "true"
	}

	a.logger.Debug("[YouTubeAuth] API headers generated",
		slog.Bool("auth", authHeader != ""),
		slog.Bool("cookies", cookieHeader != ""),
	)

	return headers
}

// clientCarriesPageYtcfg reports whether yt-dlp would build this client's
// Innertube headers from a page ytcfg: web (the watch page's own,
// webpage_ytcfg), web_safari (www.youtube.com read under its UA), web_creator
// (studio.youtube.com) and web_embedded (the embed page) are in
// _download_ytcfg's page map (_base.py:1013-1024); tv_samsung, tv_downgraded,
// android_vr and visionos are not and carry {}. /browse goes out as web, and
// chat builds its own headers (Service.ChatSessionHeaders) off the watch
// page's ytcfg, as yt-dlp's live chat downloader does. Keyed on the
// Innertube client name, which the four page-carrying clients and the
// page-less ones never share.
func clientCarriesPageYtcfg(client constants.YouTubeClientConfig) bool {
	switch client.ClientName {
	case "WEB", "WEB_CREATOR", "WEB_EMBEDDED_PLAYER":
		return true
	}
	return false
}

// accountSessionHeaders is the signed-in account's identity a ytcfg names, as
// yt-dlp's _generate_cookie_auth_headers states it (_base.py:960-978):
// X-Goog-PageId for a delegated (brand) session, and X-Goog-AuthUser — the
// session index, 0 when only a delegated session is named — whenever either
// is. Empty for a ytcfg that names no session. GenerateAPIHeaders' and
// Service.ChatSessionHeaders' one source, so chat cannot drift from the
// player calls.
func accountSessionHeaders(ytcfg *YtcfgData) map[string]string {
	headers := map[string]string{}
	if ytcfg == nil {
		return headers
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
	return headers
}

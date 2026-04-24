package main

import (
	"io/fs"
	"net/http"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
	webpublic "github.com/vampiricwulf/Moombox/web"
)

// wireWebSocket registers the WebSocket handler, persistent-client-token
// AuthMiddleware fallback, upgrade-time auth check, and InitialState
// provider on the web server. Also sets OpenBrowser=true and mounts the
// embedded static files (SPA fallback). Called once after route registration.
func (s *runState) wireWebSocket() {
	// WebSocket upgrade handler — register on the router before static file
	// mounting. TS uses noServer mode which upgrades on any path; frontend
	// connects to ws://host/ (root).
	s.webServer.SetWebSocketHandler(s.wsHub.HandleUpgrade)

	// Wire persistent client token check for AuthMiddleware fallback
	s.webServer.ClientTokenCheck = func(rawToken, ip string) (bool, string) {
		prefix := web.TokenPrefix(rawToken)
		ct, err := s.db.GetClientTokenByPrefix(prefix)
		if err != nil || ct == nil {
			return false, ""
		}
		if !web.VerifyToken(rawToken, ct.TokenHash) {
			return false, ""
		}
		sessionToken, err := s.authSvc.CreateSession()
		if err != nil {
			return false, ""
		}
		// Fire-and-forget usage update
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("client token usage update panic", "panic", r)
				}
			}()
			s.db.UpdateClientTokenUsage(ct.ID, ip)
		}()
		return true, sessionToken
	}

	// Wire WebSocket auth check for external connections
	s.wsHub.AuthCheck = func(r *http.Request) bool {
		s.cfgMu.RLock()
		networkAccess := s.cfg.Network.NetworkAccess
		passwordHash := s.cfg.Network.PasswordHash
		s.cfgMu.RUnlock()
		if !web.IsAuthRequired(networkAccess, passwordHash) {
			return true
		}
		// Check session cookie
		if cookie, err := r.Cookie("moombox_session"); err == nil {
			if s.authSvc.ValidateSession(cookie.Value) {
				return true
			}
		}
		// Fallback: check persistent client token (can't set cookies on WS
		// upgrade, just allow the connection).
		if cookie, err := r.Cookie("moombox_client"); err == nil && cookie.Value != "" {
			prefix := web.TokenPrefix(cookie.Value)
			if ct, err := s.db.GetClientTokenByPrefix(prefix); err == nil && ct != nil {
				if web.VerifyToken(cookie.Value, ct.TokenHash) {
					go func() {
						defer func() {
							if r := recover(); r != nil {
								s.log.Error("client token usage update panic", "panic", r)
							}
						}()
						s.db.UpdateClientTokenUsage(ct.ID, extractWSIP(r))
					}()
					return true
				}
			}
		}
		return false
	}

	// Wire initial state provider for WebSocket connections
	s.wsHub.InitialState = func() map[string]any {
		jobs, err := s.db.GetAllJobs()
		if err != nil {
			jobs = []*database.Job{} // Send empty array, not null
		}
		jobs = filterJobsByAge(jobs, s.configStore)
		return map[string]any{
			"jobs":            jobs,
			"logs":            s.log.GetRecentLines(),
			"nextFeedCheck":   s.feedMon.GetNextCheckAt(),
			"nextDecapiCheck": s.decapiMon.GetNextCheckAt(),
			"nextTwitchCheck": s.twitchMon.GetNextCheckAt(),
			"connectivity":    s.connMon.IsOnline(),
		}
	}

	// Open browser to dashboard URL on start (matches TS openBrowser=true default)
	s.webServer.OpenBrowser = true

	// Serve embedded static files (web dashboard) with SPA fallback
	staticFS, _ := fs.Sub(webpublic.PublicFS, "public")
	s.webServer.MountStaticFiles(staticFS)
}

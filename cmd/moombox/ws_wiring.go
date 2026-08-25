package main

import (
	"io/fs"
	"net/http"

	"github.com/vampiricwulf/Moombox/internal/config"
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
		var networkAccess, passwordHash string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			networkAccess = c.Network.NetworkAccess
			passwordHash = c.Network.PasswordHash
		})
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
						s.db.UpdateClientTokenUsage(ct.ID, web.EffectiveClientIP(s.configStore, r))
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
		// Capture the threshold ONCE so the filtered job list and the
		// hideFinishedAgeDays we return to the client are guaranteed to agree
		// (a concurrent config change between two separate store reads could
		// otherwise hand the client a list filtered by a different threshold
		// than the one its _evaluateArchiveBoundary is told to use).
		var hideAge float64
		s.configStore.Read(func(c *config.MoomboxConfig) {
			hideAge = c.Monitors.HideFinishedAgeDays.Value
		})
		jobs = filterJobsByAgeThreshold(jobs, hideAge)
		// Backfill progress snapshot (spec §11): a scan pages for minutes at
		// 1 page/sec, so connecting MID-FLIGHT is the common case — without
		// this seed a client would see nothing until the next page event.
		// Same per-channel objects the backfill_status broadcasts carry.
		s.backfillMu.Lock()
		backfill := make([]map[string]any, 0, len(s.backfillProgress))
		for chID, p := range s.backfillProgress {
			backfill = append(backfill, map[string]any{
				"channel": chID,
				"tab":     p.Tab,
				"pages":   p.Pages,
				"state":   p.State,
			})
		}
		s.backfillMu.Unlock()
		return map[string]any{
			"jobs":                jobs,
			"logs":                s.log.GetRecentLines(),
			"nextFeedCheck":       s.feedMon.GetNextCheckAt(),
			"nextDecapiCheck":     s.decapiMon.GetNextCheckAt(),
			"nextTwitchCheck":     s.twitchMon.GetNextCheckAt(),
			"connectivity":        s.connMon.IsOnline(),
			"hideFinishedAgeDays": hideAge,
			"backfill":            backfill,
		}
	}

	// Open browser to dashboard URL on start (matches TS openBrowser=true default)
	s.webServer.OpenBrowser = true

	// Serve embedded static files (web dashboard) with SPA fallback
	staticFS, _ := fs.Sub(webpublic.PublicFS, "public")
	s.webServer.MountStaticFiles(staticFS)
}

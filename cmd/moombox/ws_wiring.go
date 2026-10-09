package main

import (
	"io/fs"
	"net/http"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
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
		ct := s.clientTokenFor(rawToken)
		if ct == nil {
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
			if err := s.db.UpdateClientTokenUsage(ct.ID, ip); err != nil {
				s.log.Debug("client token last-used update failed", "error", err)
			}
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
			if ct := s.clientTokenFor(cookie.Value); ct != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							s.log.Error("client token usage update panic", "panic", r)
						}
					}()
					if err := s.db.UpdateClientTokenUsage(ct.ID, web.EffectiveClientIP(s.configStore, r)); err != nil {
						s.log.Debug("client token last-used update failed", "error", err)
					}
				}()
				return true
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
		// Trims the trim service is running, with their latest progress: a
		// dashboard trim runs detached from the page that started it, so a
		// page reloaded mid-trim learns of it here and draws its bar from the
		// next trim_status frame on.
		runningTrims := []worker.TrimTask{}
		if s.trimSvc != nil {
			runningTrims = s.trimSvc.RunningTrims()
		}
		// logSeq numbers the snapshot's newest line. This client joined the
		// hub before this read, so a line logged since is here AND on its
		// way as a log frame; the dashboard skips frames at or below logSeq
		// (W24-14).
		logs, logSeq := s.log.RecentLines()
		return map[string]any{
			"jobs":                jobs,
			"logs":                logs,
			"logSeq":              logSeq,
			"nextFeedCheck":       s.feedMon.GetNextCheckAt(),
			"nextDecapiCheck":     s.decapiMon.GetNextCheckAt(),
			"nextTwitchCheck":     s.twitchMon.GetNextCheckAt(),
			"connectivity":        s.connMon.IsOnline(),
			"hideFinishedAgeDays": hideAge,
			"backfill":            backfill,
			"runningTrims":        runningTrims,
		}
	}

	// Open browser to dashboard URL on start (matches TS openBrowser=true default)
	s.webServer.OpenBrowser = true

	// Serve embedded static files (web dashboard) with SPA fallback
	staticFS, _ := fs.Sub(webpublic.PublicFS, "public")
	s.webServer.MountStaticFiles(staticFS)
}

// clientTokenFor returns the stored client token a raw moombox_client value
// proves, or nil. The one check both auth paths (the HTTP fallback and the
// WebSocket upgrade) share.
//
// network.client_token_ttl_days is enforced HERE, against the row's
// created_at. It used to set only the cookie's Max-Age, which is the client's
// to ignore: a value captured once (a plain-HTTP external install, a proxy
// log, a copied browser profile) authenticated forever with `curl -b`,
// minting a fresh session per request. An expired row is deleted, so the
// client-token list stops showing it too.
func (s *runState) clientTokenFor(raw string) *database.ClientToken {
	ct, err := s.db.GetClientTokenByPrefix(web.TokenPrefix(raw))
	if err != nil || ct == nil || !web.VerifyToken(raw, ct.TokenHash) {
		return nil
	}
	var ttlDays int
	s.configStore.Read(func(c *config.MoomboxConfig) { ttlDays = c.Network.ClientTokenTTLDays })
	if clientTokenExpired(ct.CreatedAt, ttlDays, time.Now()) {
		if err := s.db.DeleteClientToken(ct.ID); err != nil {
			s.log.Debug("expired client token delete failed", "error", err)
		}
		s.log.Info("expired client token refused and removed", "label", ct.Label, "createdAt", ct.CreatedAt)
		return nil
	}
	return ct
}

// clientTokenExpired reports whether a token created at createdAt (RFC 3339,
// as AddClientToken's caller writes it) has outlived ttlDays. A ttl of 0
// means the 365-day default, as setClientCookie does. An unreadable
// timestamp counts as expired: the row cannot prove its age, and the cost is
// one fresh login.
func clientTokenExpired(createdAt string, ttlDays int, now time.Time) bool {
	if ttlDays <= 0 {
		ttlDays = 365
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return true
	}
	return now.Sub(t) > time.Duration(ttlDays)*24*time.Hour
}

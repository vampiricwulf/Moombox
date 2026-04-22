package routes

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// AuthRoutesDeps holds dependencies for auth routes.
type AuthRoutesDeps struct {
	Cfg        *config.MoomboxConfig
	Auth       *web.AuthService
	DB         *database.Database
	LoginRL    *web.RateLimiter // 5 attempts/60s
	PasswordRL *web.RateLimiter // 3 attempts/60s
	SaveConfig func(*config.MoomboxConfig) error
	Logger     interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// AuthRoutes registers authentication endpoints.
// cfgMu protects concurrent reads/writes to the shared cfg struct.
func AuthRoutes(r chi.Router, deps *AuthRoutesDeps, cfgMu *sync.RWMutex) {
	// GET /api/auth/status - public, returns auth state
	r.Get("/api/auth/status", func(rw http.ResponseWriter, req *http.Request) {
		sessionToken := getSessionToken(req)
		authenticated := deps.Auth.ValidateSession(sessionToken)

		cfgMu.RLock()
		networkAccess := deps.Cfg.Network.NetworkAccess
		passwordHash := deps.Cfg.Network.PasswordHash
		cfgMu.RUnlock()

		jsonResponse(rw, map[string]any{
			"authRequired":  web.IsAuthRequired(networkAccess, passwordHash),
			"authenticated": authenticated,
			"hasPassword":   passwordHash != "",
		})
	})

	// POST /api/auth/login - rate limited
	r.With(deps.LoginRL.Middleware).Post("/api/auth/login", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		if len(body.Password) == 0 {
			jsonError(rw, "Password is required", http.StatusBadRequest)
			return
		}
		if len(body.Password) > 128 {
			jsonError(rw, "Password too long (max 128 characters)", http.StatusBadRequest)
			return
		}

		cfgMu.RLock()
		passwordHash := deps.Cfg.Network.PasswordHash
		cfgMu.RUnlock()

		if passwordHash == "" {
			jsonError(rw, "No password is set", http.StatusBadRequest)
			return
		}

		if !deps.Auth.VerifyPassword(body.Password, passwordHash) {
			if deps.Logger != nil {
				deps.Logger.Warn("[Auth] Failed login attempt from " + web.ExtractIP(req))
			}
			jsonError(rw, "Invalid password", http.StatusUnauthorized)
			return
		}

		// Invalidate any existing (stale) session before creating a new one
		if oldToken := getSessionToken(req); oldToken != "" {
			deps.Auth.InvalidateSession(oldToken)
		}

		token, err := deps.Auth.CreateSession()
		if err != nil {
			jsonError(rw, "Failed to create session", http.StatusInternalServerError)
			return
		}

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Successful login from " + web.ExtractIP(req))
		}

		web.SetSessionCookie(rw, req, token)

		// Issue persistent client token for remote clients.
		// Revoke any existing client token first (re-login from same browser).
		if deps.DB != nil {
			if oldCookie, err := req.Cookie("moombox_client"); err == nil && oldCookie.Value != "" {
				prefix := web.TokenPrefix(oldCookie.Value)
				if oldCT, err := deps.DB.GetClientTokenByPrefix(prefix); err == nil && oldCT != nil {
					deps.DB.DeleteClientToken(oldCT.ID)
				}
			}
			if rawToken, err := web.GenerateToken(); err == nil {
				if tokenHash, err := web.HashToken(rawToken); err == nil {
					now := time.Now().UTC().Format(time.RFC3339)
					ct := &database.ClientToken{
						ID:          generateShortID(),
						TokenPrefix: web.TokenPrefix(rawToken),
						TokenHash:   tokenHash,
						Label:       buildTokenLabel(req),
						CreatedAt:   now,
						LastUsedAt:  now,
						LastIP:      web.ExtractIP(req),
					}
					if err := deps.DB.AddClientToken(ct); err == nil {
						cfgMu.RLock()
						ttlDays := deps.Cfg.Network.ClientTokenTTLDays
						cfgMu.RUnlock()
						setClientCookie(rw, req, rawToken, ttlDays)
					}
				}
			}
		}

		jsonResponse(rw, map[string]any{
			"success": true,
		})
	})

	// POST /api/auth/logout - requires valid session
	r.Post("/api/auth/logout", func(rw http.ResponseWriter, req *http.Request) {
		token := getSessionToken(req)
		if token != "" {
			deps.Auth.InvalidateSession(token)
		}

		// Revoke persistent client token
		if deps.DB != nil {
			if cookie, err := req.Cookie("moombox_client"); err == nil && cookie.Value != "" {
				prefix := web.TokenPrefix(cookie.Value)
				if ct, err := deps.DB.GetClientTokenByPrefix(prefix); err == nil && ct != nil {
					if web.VerifyToken(cookie.Value, ct.TokenHash) {
						deps.DB.DeleteClientToken(ct.ID)
					}
				}
			}
		}

		clearSessionCookie(rw, req)
		clearClientCookie(rw, req)
		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/auth/set-password - rate limited, requires session OR local/LAN
	r.With(deps.PasswordRL.Middleware).Post("/api/auth/set-password", func(rw http.ResponseWriter, req *http.Request) {
		// Must be authenticated via session OR from loopback/private IP
		token := getSessionToken(req)
		isAuthenticated := deps.Auth.ValidateSession(token)
		isLocal := web.IsLocalOrPrivateRequest(req)

		if !isAuthenticated && !isLocal {
			jsonError(rw, "authentication required", http.StatusUnauthorized)
			return
		}

		var body struct {
			CurrentPassword string `json:"currentPassword"`
			NewPassword     string `json:"newPassword"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		if len(body.NewPassword) < 8 {
			jsonError(rw, "Password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		if len(body.NewPassword) > 128 {
			jsonError(rw, "Password too long (max 128 characters)", http.StatusBadRequest)
			return
		}

		// Hash new password (CPU-intensive, done outside lock)
		hash, err := deps.Auth.HashPassword(body.NewPassword)
		if err != nil {
			jsonError(rw, "failed to hash password", http.StatusInternalServerError)
			return
		}

		// Verify current password and write new hash under the same lock
		// to prevent TOCTOU (hash could change between read and write)
		cfgMu.Lock()
		oldHash := deps.Cfg.Network.PasswordHash
		if oldHash != "" {
			if !deps.Auth.VerifyPassword(body.CurrentPassword, oldHash) {
				cfgMu.Unlock()
				jsonError(rw, "Current password is incorrect", http.StatusUnauthorized)
				return
			}
		}
		deps.Cfg.Network.PasswordHash = hash
		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(deps.Cfg); err != nil {
				deps.Cfg.Network.PasswordHash = oldHash
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}
		cfgMu.Unlock()

		// Invalidate other sessions (keep current if authenticated)
		if isAuthenticated && token != "" {
			deps.Auth.InvalidateOtherSessions(token)
		} else {
			deps.Auth.InvalidateAllSessions()
		}

		// Clear all client tokens — password changed, all remotes must re-auth
		if deps.DB != nil {
			deps.DB.DeleteAllClientTokens()
		}
		clearClientCookie(rw, req)

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Password set/changed from " + web.ExtractIP(req))
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/auth/remove-password - rate limited
	r.With(deps.PasswordRL.Middleware).Post("/api/auth/remove-password", func(rw http.ResponseWriter, req *http.Request) {
		token := getSessionToken(req)
		isAuthenticated := deps.Auth.ValidateSession(token)
		isLocal := web.IsLocalOrPrivateRequest(req)

		if !isAuthenticated && !isLocal {
			jsonError(rw, "authentication required", http.StatusUnauthorized)
			return
		}

		var body struct {
			CurrentPassword string `json:"currentPassword"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		if len(body.CurrentPassword) == 0 {
			jsonError(rw, "Current password is required", http.StatusBadRequest)
			return
		}
		if len(body.CurrentPassword) > 128 {
			jsonError(rw, "Password too long (max 128 characters)", http.StatusBadRequest)
			return
		}

		// Verify and remove under the same lock to prevent TOCTOU
		cfgMu.Lock()
		oldHash := deps.Cfg.Network.PasswordHash
		oldAccess := deps.Cfg.Network.NetworkAccess

		if oldHash == "" {
			cfgMu.Unlock()
			jsonError(rw, "No password is set", http.StatusBadRequest)
			return
		}

		if !deps.Auth.VerifyPassword(body.CurrentPassword, oldHash) {
			cfgMu.Unlock()
			jsonError(rw, "Current password is incorrect", http.StatusUnauthorized)
			return
		}
		deps.Cfg.Network.PasswordHash = ""
		deps.Cfg.Network.NetworkAccess = "localhost" // Reset to safe default
		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(deps.Cfg); err != nil {
				deps.Cfg.Network.PasswordHash = oldHash
				deps.Cfg.Network.NetworkAccess = oldAccess
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}
		cfgMu.Unlock()

		deps.Auth.InvalidateAllSessions()
		if deps.DB != nil {
			deps.DB.DeleteAllClientTokens()
		}
		clearSessionCookie(rw, req)
		clearClientCookie(rw, req)

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Password removed, network_access reset to localhost from " + web.ExtractIP(req))
		}

		jsonResponse(rw, map[string]any{
			"success":              true,
			"networkAccessChanged": true,
		})
	})
}

// ClientTokenRoutes registers client token management endpoints.
func ClientTokenRoutes(r chi.Router, deps *AuthRoutesDeps) {
	// GET /api/client-tokens — list all tokens
	r.Get("/api/client-tokens", func(rw http.ResponseWriter, req *http.Request) {
		if deps.DB == nil {
			jsonResponse(rw, []any{})
			return
		}
		tokens, err := deps.DB.ListClientTokens()
		if err != nil {
			jsonError(rw, "failed to list tokens", http.StatusInternalServerError)
			return
		}
		if tokens == nil {
			tokens = []*database.ClientToken{}
		}
		jsonResponse(rw, tokens)
	})

	// DELETE /api/client-tokens/{id} — revoke one token
	r.Delete("/api/client-tokens/{id}", func(rw http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "id")
		if id == "" {
			jsonError(rw, "id required", http.StatusBadRequest)
			return
		}
		if deps.DB == nil {
			jsonError(rw, "database unavailable", http.StatusInternalServerError)
			return
		}
		if err := deps.DB.DeleteClientToken(id); err != nil {
			jsonError(rw, "failed to delete token", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, map[string]any{"success": true})
	})
}


func getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("moombox_session")
	if err != nil {
		return ""
	}
	return cookie.Value
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func setClientCookie(w http.ResponseWriter, r *http.Request, rawToken string, ttlDays int) {
	if ttlDays <= 0 {
		ttlDays = 365 // mirror config default: 1 year
	}
	maxAge := ttlDays * 86400
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_client",
		Value:    rawToken,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearClientCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_client",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// buildTokenLabel creates a human-readable label from the User-Agent and IP.
func buildTokenLabel(r *http.Request) string {
	ua := r.UserAgent()
	ip := web.ExtractIP(r)

	// Truncate UA to something useful
	label := ua
	if len(label) > 60 {
		label = label[:60]
	}
	// Extract just the browser/device portion if possible
	if before, _, ok := strings.Cut(label, "("); ok {
		label = strings.TrimSpace(before)
	}
	if label == "" {
		label = "Unknown"
	}

	return fmt.Sprintf("%s — %s", label, ip)
}

// generateShortID creates a short random hex ID for client tokens.
func generateShortID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

package routes

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
func AuthRoutes(r chi.Router, deps *AuthRoutesDeps) {
	// GET /api/auth/status - public, returns auth state
	r.Get("/api/auth/status", func(rw http.ResponseWriter, req *http.Request) {
		sessionToken := getSessionToken(req)
		authenticated := deps.Auth.ValidateSession(sessionToken)

		jsonResponse(rw, map[string]any{
			"authRequired":  web.IsAuthRequired(deps.Cfg.Network.NetworkAccess, deps.Cfg.Network.PasswordHash),
			"authenticated": authenticated,
			"hasPassword":   deps.Cfg.Network.PasswordHash != "",
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

		if deps.Cfg.Network.PasswordHash == "" {
			jsonError(rw, "No password is set", http.StatusBadRequest)
			return
		}

		if !deps.Auth.VerifyPassword(body.Password, deps.Cfg.Network.PasswordHash) {
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
						setClientCookie(rw, req, rawToken)
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

		clearSessionCookie(rw)
		clearClientCookie(rw)
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

		// If password exists, verify current password
		if deps.Cfg.Network.PasswordHash != "" {
			if !deps.Auth.VerifyPassword(body.CurrentPassword, deps.Cfg.Network.PasswordHash) {
				jsonError(rw, "Current password is incorrect", http.StatusUnauthorized)
				return
			}
		}

		// Hash new password
		hash, err := deps.Auth.HashPassword(body.NewPassword)
		if err != nil {
			jsonError(rw, "failed to hash password", http.StatusInternalServerError)
			return
		}

		deps.Cfg.Network.PasswordHash = hash

		// Save config
		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(deps.Cfg); err != nil {
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}

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
		clearClientCookie(rw)

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

		if deps.Cfg.Network.PasswordHash == "" {
			jsonError(rw, "No password is set", http.StatusBadRequest)
			return
		}

		if !deps.Auth.VerifyPassword(body.CurrentPassword, deps.Cfg.Network.PasswordHash) {
			jsonError(rw, "Current password is incorrect", http.StatusUnauthorized)
			return
		}

		deps.Cfg.Network.PasswordHash = ""
		deps.Cfg.Network.NetworkAccess = "local" // Reset to safe default

		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(deps.Cfg); err != nil {
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}

		deps.Auth.InvalidateAllSessions()
		if deps.DB != nil {
			deps.DB.DeleteAllClientTokens()
		}
		clearSessionCookie(rw)
		clearClientCookie(rw)

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Password removed, network_access reset to local from " + web.ExtractIP(req))
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

// AuthMiddleware returns middleware that requires authentication when enabled.
func AuthMiddleware(cfg *config.MoomboxConfig, auth *web.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			// Skip auth check if not required
			if !web.IsAuthRequired(cfg.Network.NetworkAccess, cfg.Network.PasswordHash) {
				next.ServeHTTP(rw, req)
				return
			}

			// Allow loopback always
			if web.IsLoopbackRequest(req) {
				next.ServeHTTP(rw, req)
				return
			}

			// Check session
			token := getSessionToken(req)
			if auth.ValidateSession(token) {
				next.ServeHTTP(rw, req)
				return
			}

			jsonError(rw, "authentication required", http.StatusUnauthorized)
		})
	}
}


func getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("moombox_session")
	if err != nil {
		return ""
	}
	return cookie.Value
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func setClientCookie(w http.ResponseWriter, r *http.Request, rawToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_client",
		Value:    rawToken,
		Path:     "/",
		MaxAge:   315360000, // ~10 years
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearClientCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_client",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
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
	if idx := strings.Index(label, "("); idx > 0 {
		label = strings.TrimSpace(label[:idx])
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

package routes

import (
	"encoding/json"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// AuthRoutesDeps holds dependencies for auth routes.
type AuthRoutesDeps struct {
	Cfg        *config.MoomboxConfig
	Auth       *web.AuthService
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
	// GET /api/v1/auth/status - public, returns auth state
	r.Get("/api/v1/auth/status", func(rw http.ResponseWriter, req *http.Request) {
		sessionToken := getSessionToken(req)
		authenticated := deps.Auth.ValidateSession(sessionToken)

		jsonResponse(rw, map[string]any{
			"authRequired":  web.IsAuthRequired(deps.Cfg.Network.NetworkAccess, deps.Cfg.Network.PasswordHash),
			"authenticated": authenticated,
			"hasPassword":   deps.Cfg.Network.PasswordHash != "",
		})
	})

	// POST /api/v1/auth/login - rate limited
	r.With(deps.LoginRL.Middleware).Post("/api/v1/auth/login", func(rw http.ResponseWriter, req *http.Request) {
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
				deps.Logger.Warn("[Auth] Failed login attempt from " + extractClientIP(req))
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
			deps.Logger.Info("[Auth] Successful login from " + extractClientIP(req))
		}

		setSessionCookie(rw, req, token)
		jsonResponse(rw, map[string]any{
			"success": true,
		})
	})

	// POST /api/v1/auth/logout - requires valid session
	r.Post("/api/v1/auth/logout", func(rw http.ResponseWriter, req *http.Request) {
		token := getSessionToken(req)
		if token != "" {
			deps.Auth.InvalidateSession(token)
		}
		clearSessionCookie(rw)
		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/v1/auth/set-password - rate limited, requires session OR local/LAN
	r.With(deps.PasswordRL.Middleware).Post("/api/v1/auth/set-password", func(rw http.ResponseWriter, req *http.Request) {
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

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Password set/changed from " + extractClientIP(req))
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/v1/auth/remove-password - rate limited
	r.With(deps.PasswordRL.Middleware).Post("/api/v1/auth/remove-password", func(rw http.ResponseWriter, req *http.Request) {
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
		deps.Cfg.Network.NetworkAccess = "localhost" // Reset to safe default

		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(deps.Cfg); err != nil {
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}

		deps.Auth.InvalidateAllSessions()
		clearSessionCookie(rw)

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Password removed, network_access reset to localhost from " + extractClientIP(req))
		}

		jsonResponse(rw, map[string]any{
			"success":              true,
			"networkAccessChanged": true,
		})
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

func extractClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("moombox_session")
	if err != nil {
		return ""
	}
	return cookie.Value
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "moombox_session",
		Value:    token,
		Path:     "/",
		MaxAge:   86400, // 24 hours
		HttpOnly: true,
		Secure:   r.TLS != nil, // Dynamic: match TS req.secure behavior
		SameSite: http.SameSiteLaxMode,
	})
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

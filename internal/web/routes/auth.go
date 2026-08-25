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
	Auth       *web.AuthService
	DB         *database.Database
	LoginRL    *web.RateLimiter // 5 attempts/60s
	PasswordRL *web.RateLimiter // 3 attempts/60s
	Logger     interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// AuthRoutes registers authentication endpoints. The Store carries both
// the read-mostly *config.MoomboxConfig used for password-hash / network
// access checks and the lock that the set-password / remove-password
// handlers grab to atomically verify-and-replace the hash.
func AuthRoutes(r chi.Router, deps *AuthRoutesDeps, store *config.Store) {
	// Capture once: pointer stays stable for the lifetime of the Store.
	// store.RWMutex() / store.Config() are the deprecated migration
	// accessors — they go away once the write sites move to a future
	// transactional Update API (DECISIONS #8 follow-up).
	mu := store.RWMutex()
	cfg := store.Config()

	// GET /api/auth/status - public, returns auth state
	r.Get("/api/auth/status", func(rw http.ResponseWriter, req *http.Request) {
		sessionToken := getSessionToken(req)
		authenticated := deps.Auth.ValidateSession(sessionToken)

		var networkAccess, passwordHash string
		store.Read(func(c *config.MoomboxConfig) {
			networkAccess = c.Network.NetworkAccess
			passwordHash = c.Network.PasswordHash
		})

		jsonResponse(rw, map[string]any{
			"authRequired":  web.IsAuthRequired(networkAccess, passwordHash),
			"authenticated": authenticated,
			"hasPassword":   passwordHash != "",
			// External/public access with no password: open to any IP that
			// can reach the port. Drives the web UI's persistent security
			// banner (block-set/warn-boot policy — the state can only come
			// from a hand-edited config file).
			"passwordlessExternal": (networkAccess == "external" || networkAccess == "public") && passwordHash == "",
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

		var passwordHash string
		store.Read(func(c *config.MoomboxConfig) {
			passwordHash = c.Network.PasswordHash
		})

		if passwordHash == "" {
			jsonError(rw, "No password is set", http.StatusBadRequest)
			return
		}

		if !deps.Auth.VerifyPassword(body.Password, passwordHash) {
			if deps.Logger != nil {
				deps.Logger.Warn("[Auth] Failed login attempt from " + web.EffectiveClientIP(store, req))
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
			deps.Logger.Info("[Auth] Successful login from " + web.EffectiveClientIP(store, req))
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
						Label:       buildTokenLabel(store, req),
						CreatedAt:   now,
						LastUsedAt:  now,
						LastIP:      web.EffectiveClientIP(store, req),
					}
					if err := deps.DB.AddClientToken(ct); err == nil {
						var ttlDays int
						store.Read(func(c *config.MoomboxConfig) {
							ttlDays = c.Network.ClientTokenTTLDays
						})
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
		isLocal := web.IsLocalOrPrivateRequest(store, req)

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
		// to prevent TOCTOU (hash could change between read and write).
		// Rollback on save failure keeps in-memory cfg in sync with disk.
		mu.Lock()
		oldHash := cfg.Network.PasswordHash
		if oldHash != "" {
			if !deps.Auth.VerifyPassword(body.CurrentPassword, oldHash) {
				mu.Unlock()
				jsonError(rw, "Current password is incorrect", http.StatusUnauthorized)
				return
			}
		} else if !isAuthenticated && !web.IsLoopbackRequest(req) {
			// First-run set-password (no existing hash) is only allowed
			// from loopback when no session exists. Without this gate, a
			// LAN peer reaching a network_access=lan deployment could
			// claim the admin password before the user does. The earlier
			// IsLocalOrPrivateRequest gate covered authenticated change-
			// password from LAN; this stricter check applies only to the
			// "no password set yet" branch where there's no
			// proof-of-physical-access signal.
			mu.Unlock()
			jsonError(rw, "first-time password setup must be from localhost", http.StatusUnauthorized)
			return
		}
		cfg.Network.PasswordHash = hash
		if err := store.SaveLocked(); err != nil {
			cfg.Network.PasswordHash = oldHash
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}
		mu.Unlock()

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
			deps.Logger.Info("[Auth] Password set/changed from " + web.EffectiveClientIP(store, req))
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/auth/remove-password - rate limited
	r.With(deps.PasswordRL.Middleware).Post("/api/auth/remove-password", func(rw http.ResponseWriter, req *http.Request) {
		token := getSessionToken(req)
		isAuthenticated := deps.Auth.ValidateSession(token)
		isLocal := web.IsLocalOrPrivateRequest(store, req)

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

		// Verify and remove under the same lock to prevent TOCTOU.
		// Rollback on save failure keeps in-memory cfg in sync with disk.
		mu.Lock()
		oldHash := cfg.Network.PasswordHash
		oldAccess := cfg.Network.NetworkAccess

		if oldHash == "" {
			mu.Unlock()
			jsonError(rw, "No password is set", http.StatusBadRequest)
			return
		}

		if !deps.Auth.VerifyPassword(body.CurrentPassword, oldHash) {
			mu.Unlock()
			jsonError(rw, "Current password is incorrect", http.StatusUnauthorized)
			return
		}
		cfg.Network.PasswordHash = ""
		cfg.Network.NetworkAccess = "localhost" // Reset to safe default
		if err := store.SaveLocked(); err != nil {
			cfg.Network.PasswordHash = oldHash
			cfg.Network.NetworkAccess = oldAccess
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}
		mu.Unlock()

		deps.Auth.InvalidateAllSessions()
		if deps.DB != nil {
			deps.DB.DeleteAllClientTokens()
		}
		clearSessionCookie(rw, req)
		clearClientCookie(rw, req)

		if deps.Logger != nil {
			deps.Logger.Info("[Auth] Password removed, network_access reset to localhost from " + web.EffectiveClientIP(store, req))
		}

		jsonResponse(rw, map[string]any{
			"success":              true,
			"networkAccessChanged": true,
		})
	})
}

// ClientTokenRoutes registers client token management endpoints.
func ClientTokenRoutes(r chi.Router, deps *AuthRoutesDeps) {
	// GET /api/client-tokens — list all tokens.
	// LastIP is intentionally redacted to "/24" granularity in the response
	// payload (per audit reports/web.md S-13). The full IP is still
	// persisted in the DB for the operator's local diagnostics; it just
	// doesn't ride out over the network where it would land in browser
	// devtools, screenshots, etc.
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
		out := make([]map[string]any, 0, len(tokens))
		for _, t := range tokens {
			out = append(out, map[string]any{
				"id":          t.ID,
				"tokenPrefix": t.TokenPrefix,
				"label":       t.Label,
				"createdAt":   t.CreatedAt,
				"lastUsedAt":  t.LastUsedAt,
				"lastIp":      redactIP(t.LastIP),
			})
		}
		jsonResponse(rw, out)
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

// redactIP truncates an IPv4 to /24 ("a.b.c.0") and IPv6 to /64.
// Empty input returns empty string. Malformed input returns "" rather
// than leaking the unparseable original.
func redactIP(ip string) string {
	if ip == "" {
		return ""
	}
	if i := strings.LastIndex(ip, "."); i > 0 && strings.Count(ip, ".") == 3 {
		// IPv4: keep a.b.c, replace last octet with 0.
		return ip[:i] + ".0"
	}
	if strings.Contains(ip, ":") {
		// IPv6: keep first 4 hextets (covers /64).
		segs := strings.Split(ip, ":")
		if len(segs) < 4 {
			return ""
		}
		return strings.Join(segs[:4], ":") + "::"
	}
	return ""
}

func getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("moombox_session")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// setAuthCookie writes a Moombox auth-related cookie (session or client) with
// the standard flag set. Audit D-2: the previous setSessionCookie /
// clearSessionCookie / setClientCookie / clearClientCookie helpers were
// near-identical except for name, value, and MaxAge — a single helper keeps
// the flags (HttpOnly, Secure, SameSite=Lax, Path=/) in lockstep so a future
// change to e.g. SameSite=Strict (audit S-5) only needs editing one place.
// MaxAge < 0 deletes the cookie; MaxAge == 0 is browser-session lifetime.
func setAuthCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   web.IsRequestSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	setAuthCookie(w, r, "moombox_session", "", -1)
}

func setClientCookie(w http.ResponseWriter, r *http.Request, rawToken string, ttlDays int) {
	if ttlDays <= 0 {
		ttlDays = 365 // mirror config default: 1 year
	}
	setAuthCookie(w, r, "moombox_client", rawToken, ttlDays*86400)
}

func clearClientCookie(w http.ResponseWriter, r *http.Request) {
	setAuthCookie(w, r, "moombox_client", "", -1)
}

// buildTokenLabel creates a human-readable label from the User-Agent and IP.
func buildTokenLabel(store *config.Store, r *http.Request) string {
	ua := r.UserAgent()
	ip := web.EffectiveClientIP(store, r)

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

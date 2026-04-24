package routes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/web"
)

// PotSessionData represents a PO token response.
type PotSessionData struct {
	PoToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
}

// PotProviderInterface defines the PO token provider methods needed by routes.
type PotProviderInterface interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
	GeneratePoTokenSession(ctx context.Context, contentBinding string, bypassCache bool) (poToken string, actualBinding string, err error)
	InvalidateCaches()
	InvalidateIntegrityTokens()
	GetMinterCacheKeys() []string
}

// PotRoutesDeps holds dependencies for POT routes.
type PotRoutesDeps struct {
	PotProvider PotProviderInterface
	StartTime   time.Time
	RateLimit   *web.RateLimiter
	Logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// PotRoutes registers POT provider endpoints (root-mounted).
func PotRoutes(r chi.Router, deps *PotRoutesDeps) {
	potRL := deps.RateLimit

	r.With(web.LoopbackOnly, potRL.Middleware).Post("/get_pot", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			ContentBinding string `json:"content_binding"`
			BypassCache    bool   `json:"bypass_cache"`
			DataSyncID     string `json:"data_sync_id"`
			VisitorData    string `json:"visitor_data"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Reject deprecated fields (match TS: separate checks with warn logs)
		if body.DataSyncID != "" {
			if deps.Logger != nil {
				deps.Logger.Warn("[PotProvider] data_sync_id is deprecated, use content_binding instead")
			}
			jsonError(rw, "data_sync_id is deprecated; use content_binding", http.StatusBadRequest)
			return
		}
		if body.VisitorData != "" {
			if deps.Logger != nil {
				deps.Logger.Warn("[PotProvider] visitor_data is deprecated, use content_binding instead")
			}
			jsonError(rw, "visitor_data is deprecated; use content_binding", http.StatusBadRequest)
			return
		}

		if deps.PotProvider == nil {
			jsonError(rw, "PO token provider not available", http.StatusServiceUnavailable)
			return
		}

		cbLabel := ""
		if body.ContentBinding != "" {
			cbLabel = " (binding=" + body.ContentBinding + ")"
		}
		if deps.Logger != nil {
			deps.Logger.Info("[PotProvider] POT requested" + cbLabel)
		}

		poToken, actualBinding, err := deps.PotProvider.GeneratePoTokenSession(req.Context(), body.ContentBinding, body.BypassCache)
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("failed to generate PO token", "error", err)
			}
			jsonError(rw, "failed to generate PO token", http.StatusInternalServerError)
			return
		}

		if deps.Logger != nil {
			hash := sha256.Sum256([]byte(poToken))
			deps.Logger.Info("[PotProvider] POT generated", "hash", fmt.Sprintf("%x", hash[:4]), "len", len(poToken))
		}

		jsonResponse(rw, &PotSessionData{
			PoToken:        poToken,
			ContentBinding: actualBinding,
		})
	})

	r.With(web.LoopbackOnly).Post("/invalidate_caches", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Info("[PotProvider] Cache invalidation requested")
		}
		if deps.PotProvider != nil {
			deps.PotProvider.InvalidateCaches()
		}
		rw.WriteHeader(http.StatusNoContent)
	})

	r.With(web.LoopbackOnly).Post("/invalidate_it", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Info("[PotProvider] Integrity token invalidation requested")
		}
		if deps.PotProvider != nil {
			deps.PotProvider.InvalidateIntegrityTokens()
		}
		rw.WriteHeader(http.StatusNoContent)
	})

	r.Get("/ping", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Debug("[PotProvider] Ping received")
		}
		jsonResponse(rw, map[string]any{
			"server_uptime": time.Since(deps.StartTime).Seconds(),
			"version":       "1.0.0",
		})
	})

	// Audit Q-23: GET /minter_cache reveals internal POT cache keys.
	// Wrap in LoopbackOnly for parity with the other POT endpoints — this
	// is consumed by the local yt-dlp plugin only.
	r.With(web.LoopbackOnly).Get("/minter_cache", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Debug("[PotProvider] Minter cache requested")
		}
		if deps.PotProvider == nil {
			jsonResponse(rw, []string{})
			return
		}
		keys := deps.PotProvider.GetMinterCacheKeys()
		if keys == nil {
			keys = []string{}
		}
		jsonResponse(rw, keys)
	})
}

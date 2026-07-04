package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// NotificationRouteDeps carries the dependencies for the notification
// utility routes.
type NotificationRouteDeps struct {
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NotificationRoutes registers notification utility endpoints.
//
// POST /api/notifications/test {url} — synchronously delivers a single test
// embed to the given webhook URL (which may be unsaved: both the web add
// dialog and the TUI editor test the staged value before committing it to
// config). Single attempt, no retries — the caller wants the immediate
// outcome. Validation errors return 400; delivery failures 502.
func NotificationRoutes(r chi.Router, deps *NotificationRouteDeps) {
	r.Post("/api/notifications/test", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.URL == "" {
			jsonError(rw, "missing url", http.StatusBadRequest)
			return
		}

		if err := notifications.ValidateURL(body.URL); err != nil {
			jsonError(rw, err.Error(), http.StatusBadRequest)
			return
		}

		if err := notifications.SendTest(body.URL); err != nil {
			// The error already avoids echoing the URL (secrets discipline
			// in the notifications package).
			deps.Logger.Warn("test notification failed", "err", err)
			jsonError(rw, err.Error(), http.StatusBadGateway)
			return
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}

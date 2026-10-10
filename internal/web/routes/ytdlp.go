package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// YtdlpPluginInfo is what GET /api/ytdlp-plugin/status reports; the type and
// its computation live in internal/ytdlpplugin so the TUI can read them
// without importing the HTTP layer — and, transitively, the BotGuard embed
// blobs a fresh checkout does not have. The alias keeps this package's
// spec-named symbol true for cmd/moombox and the docs.
type YtdlpPluginInfo = ytdlpplugin.Info

// YtdlpPluginStatus reads the installed plugin file, if any, and reports it
// against the port and scheme this process is actually serving on — the body
// of the GET route below and of the TUI's E Y overlay. See ytdlpplugin.Status.
func YtdlpPluginStatus(port int, httpsEnabled bool) (YtdlpPluginInfo, error) {
	return ytdlpplugin.Status(port, httpsEnabled)
}

// InstallYtdlpPlugin writes the yt-dlp PO token provider plugin to the
// standard yt-dlp plugin directory. Called from the TUI's setup wizard and
// E Y overlay and from the web setup route. See ytdlpplugin.Install.
func InstallYtdlpPlugin(port int, httpsEnabled bool) error {
	return ytdlpplugin.Install(port, httpsEnabled)
}

// YtdlpRoutes registers yt-dlp plugin routes. currentPort is a GETTER
// evaluated per request: route wiring runs before the listener binds, so a
// port captured by value would be the configured port — with auto-pick
// (port 0) the plugin file would be generated pointing at ":0" forever.
func YtdlpRoutes(r chi.Router, currentPort func() int, httpsEnabled bool) {
	// GET /api/ytdlp-plugin/status
	r.Get("/api/ytdlp-plugin/status", func(rw http.ResponseWriter, req *http.Request) {
		info, err := YtdlpPluginStatus(currentPort(), httpsEnabled)
		if err != nil {
			jsonError(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, info)
	})

	// POST /api/ytdlp-plugin/install
	r.Post("/api/ytdlp-plugin/install", func(rw http.ResponseWriter, req *http.Request) {
		port := currentPort()
		var body struct {
			Force bool `json:"force"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// The same Status and Install the TUI uses. This route used to
		// write the file itself, with its own copy of the already-installed
		// check, and answered a failure with a bare "failed to write
		// plugin" — dropping the cause (permission denied, a read-only
		// file system) that the TUI's I key shows.
		if !body.Force {
			if info, err := ytdlpplugin.Status(port, httpsEnabled); err == nil &&
				info.Installed && !info.Unparseable && !info.PortMismatch {
				jsonResponse(rw, map[string]any{
					"success":          true,
					"alreadyInstalled": true,
					"path":             ytdlpplugin.File(),
				})
				return
			}
		}
		if err := ytdlpplugin.Install(port, httpsEnabled); err != nil {
			jsonError(rw, "Failed to install plugin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		pluginPath := ytdlpplugin.File()

		jsonResponse(rw, map[string]any{
			"success":          true,
			"alreadyInstalled": false,
			"path":             pluginPath,
		})
	})
}

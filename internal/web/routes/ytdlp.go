package routes

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

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
// standard yt-dlp plugin directory. Called from the TUI setup wizard and the
// web install route. See ytdlpplugin.Install.
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

		pluginDir := ytdlpplugin.Dir()
		if pluginDir == "" {
			jsonError(rw, "cannot determine yt-dlp plugin directory", http.StatusInternalServerError)
			return
		}

		// Create plugin directory structure (matches TypeScript layout)
		targetDir := filepath.Join(pluginDir, "moombox", "yt_dlp_plugins", "extractor")
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			jsonError(rw, "failed to create plugin directory", http.StatusInternalServerError)
			return
		}

		pluginPath := filepath.Join(targetDir, "getpot_moombox.py")

		// Check if already installed with correct port and scheme
		expectedScheme := "http"
		if httpsEnabled {
			expectedScheme = "https"
		}
		if !body.Force {
			if data, err := os.ReadFile(pluginPath); err == nil {
				existingScheme, existingPort := ytdlpplugin.ParseInstalled(string(data))
				if existingPort == port && existingScheme == expectedScheme {
					jsonResponse(rw, map[string]any{
						"success":          true,
						"alreadyInstalled": true,
						"path":             pluginPath,
					})
					return
				}
			}
		}

		// Write the plugin file with the current port
		pluginContent := ytdlpplugin.Generate(port, httpsEnabled)
		if err := os.WriteFile(pluginPath, []byte(pluginContent), 0o644); err != nil {
			jsonError(rw, "failed to write plugin", http.StatusInternalServerError)
			return
		}

		jsonResponse(rw, map[string]any{
			"success":          true,
			"alreadyInstalled": false,
			"path":             pluginPath,
		})
	})
}

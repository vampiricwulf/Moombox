package routes

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// SetupDeps holds dependencies for setup wizard routes.
type SetupDeps struct {
	Auth           *web.AuthService
	OnInstallYtdlp func(port int, httpsEnabled bool)
	OnRestart      func()
}

// SetupRoutes registers setup wizard endpoints. The Store carries cfg
// and the lock; setup/complete keeps copy-on-write so a save failure
// never leaks partial mutations into the live config.
func SetupRoutes(r chi.Router, deps *SetupDeps, store *config.Store) {
	mu := store.RWMutex()
	cfg := store.Config()

	r.Get("/api/setup/status", func(rw http.ResponseWriter, req *http.Request) {
		// isFirstRun matches TypeScript: !configManager.hasConfig()
		var configLoaded bool
		var ffmpegPath string
		store.Read(func(c *config.MoomboxConfig) {
			configLoaded = c.ConfigLoaded
			ffmpegPath = c.Paths.FfmpegPath
		})

		resp := map[string]any{
			"isFirstRun": !configLoaded,
		}
		// Always include FFmpeg status (helps user during setup and post-setup)
		path := ffmpegPath
		if path == "" {
			path = "ffmpeg"
		}
		valid, version, _ := CheckFFmpegCached(path)
		resp["ffmpegValid"] = valid
		if valid {
			resp["ffmpegVersion"] = version
		}
		jsonResponse(rw, resp)
	})

	// POST /api/setup/complete — uses same updateConfigSchema as PUT /config (snake_case, nested)
	// plus an additional "password" field for first-run password setup.
	r.Post("/api/setup/complete", func(rw http.ResponseWriter, req *http.Request) {
		// Guard: only allow setup on first run (before config is loaded/saved)
		var alreadyConfigured bool
		store.Read(func(c *config.MoomboxConfig) {
			alreadyConfigured = c.ConfigLoaded
		})
		if alreadyConfigured {
			jsonError(rw, "setup already completed", http.StatusBadRequest)
			return
		}

		var updates map[string]any
		if err := json.NewDecoder(req.Body).Decode(&updates); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Extract special fields before validation (not part of updateConfigSchema)
		password, _ := updates["password"].(string)
		delete(updates, "password")
		installYtdlp, _ := updates["install_ytdlp_plugin"].(bool)
		delete(updates, "install_ytdlp_plugin")

		// Validate with Zod-equivalent schema constraints (match TS updateConfigSchema)
		if validationErrs := validateConfigUpdates(updates); len(validationErrs) > 0 {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(rw).Encode(map[string]any{
				"error":   "Validation failed",
				"details": validationErrs,
			})
			return
		}

		// Hash password if provided (needed before external access check)
		var passwordHash string
		if password != "" {
			if len(password) < 8 {
				jsonError(rw, "password must be at least 8 characters", http.StatusBadRequest)
				return
			}
			if deps.Auth != nil {
				hash, err := deps.Auth.HashPassword(password)
				if err != nil {
					jsonError(rw, "failed to hash password", http.StatusInternalServerError)
					return
				}
				passwordHash = hash
			}
		}

		mu.Lock()

		// Work on a copy so the live config isn't modified if save fails
		cfgCopy := *cfg

		if passwordHash != "" {
			cfgCopy.Network.PasswordHash = passwordHash
		}

		// Validate external access requires password (match TS)
		if net, ok := updates["network"].(map[string]any); ok {
			if v, ok := net["network_access"].(string); ok && v == "external" {
				if cfgCopy.Network.PasswordHash == "" {
					mu.Unlock()
					jsonError(rw, "A password (min 8 characters) is required for external access.", http.StatusBadRequest)
					return
				}
			}
		}

		// Apply config updates to copy (same schema as PUT /config)
		applyConfigUpdates(&cfgCopy, updates)

		// Save the copy directly via config.Save so a save failure leaves
		// the live cfg untouched. Only assign back on success.
		if err := config.Save(&cfgCopy, store.SavePath()); err != nil {
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}

		// Save succeeded — apply to live config
		*cfg = cfgCopy

		// Snapshot directories for mkdir after unlock
		outputDir := cfg.Paths.OutputDirectory
		stagingDir := cfg.Paths.StagingDirectory

		// Snapshot values needed after unlock
		port := cfg.Network.Port
		httpsEnabled := cfg.Network.HTTPSEnabled

		mu.Unlock()

		// Create directories if specified (outside lock — I/O)
		if outputDir != "" {
			os.MkdirAll(outputDir, 0o755)
		}
		if stagingDir != "" {
			os.MkdirAll(stagingDir, 0o755)
		}

		// Send response before yt-dlp install / restart to avoid client timeout
		jsonResponse(rw, map[string]any{"success": true})

		// Trigger yt-dlp install (if requested) and restart in background.
		// OnChannelChange is intentionally NOT called here — the restart
		// re-initializes all services with the new config, including monitors.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reportPanic("setup post-save handler", r)
				}
			}()
			if installYtdlp && deps.OnInstallYtdlp != nil {
				deps.OnInstallYtdlp(port, httpsEnabled)
			}
			time.Sleep(500 * time.Millisecond)
			if deps.OnRestart != nil {
				deps.OnRestart()
			}
		}()
	})
}

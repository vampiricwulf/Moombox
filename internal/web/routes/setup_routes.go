package routes

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// SetupDeps holds dependencies for setup wizard routes.
type SetupDeps struct {
	Cfg             *config.MoomboxConfig
	Auth            *web.AuthService
	SaveConfig      func(*config.MoomboxConfig) error
	OnInstallYtdlp  func(port int, httpsEnabled bool)
	OnChannelChange func()
	OnRestart       func()
}

// SetupRoutes registers setup wizard endpoints.
// cfgMu protects concurrent reads/writes to the shared cfg struct.
func SetupRoutes(r chi.Router, deps *SetupDeps, cfgMu *sync.RWMutex) {
	r.Get("/api/setup/status", func(rw http.ResponseWriter, req *http.Request) {
		// isFirstRun matches TypeScript: !configManager.hasConfig()
		cfgMu.RLock()
		configLoaded := deps.Cfg.ConfigLoaded
		ffmpegPath := deps.Cfg.Paths.FfmpegPath
		cfgMu.RUnlock()

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
		var updates map[string]any
		if err := json.NewDecoder(req.Body).Decode(&updates); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		cfg := deps.Cfg

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

		cfgMu.Lock()

		// Work on a copy so the live config isn't modified if save fails
		cfgCopy := *cfg

		if passwordHash != "" {
			cfgCopy.Network.PasswordHash = passwordHash
		}

		// Validate external access requires password (match TS)
		if net, ok := updates["network"].(map[string]any); ok {
			if v, ok := net["network_access"].(string); ok && v == "external" {
				if cfgCopy.Network.PasswordHash == "" {
					cfgMu.Unlock()
					jsonError(rw, "A password (min 8 characters) is required for external access.", http.StatusBadRequest)
					return
				}
			}
		}

		// Apply config updates to copy (same schema as PUT /config)
		applyConfigUpdates(&cfgCopy, updates)

		// Save the copy
		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(&cfgCopy); err != nil {
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}

		// Save succeeded — apply to live config
		*cfg = cfgCopy

		// Snapshot directories for mkdir after unlock
		outputDir := cfg.Paths.OutputDirectory
		stagingDir := cfg.Paths.StagingDirectory

		// Snapshot values needed after unlock
		port := cfg.Network.Port
		httpsEnabled := cfg.Network.HTTPSEnabled

		cfgMu.Unlock()

		// Create directories if specified (outside lock — I/O)
		if outputDir != "" {
			os.MkdirAll(outputDir, 0o755)
		}
		if stagingDir != "" {
			os.MkdirAll(stagingDir, 0o755)
		}

		// Kick monitors to pick up any channels added during setup
		if deps.OnChannelChange != nil {
			deps.OnChannelChange()
		}

		// Install yt-dlp plugin if requested (before restart so it uses the new config values)
		if installYtdlp && deps.OnInstallYtdlp != nil {
			deps.OnInstallYtdlp(port, httpsEnabled)
		}

		jsonResponse(rw, map[string]any{"success": true})

		// Trigger a restart so all services re-initialize with new config
		if deps.OnRestart != nil {
			go func() {
				defer func() {
					recover() // restart panics are non-recoverable; prevent process crash
				}()
				time.Sleep(500 * time.Millisecond)
				deps.OnRestart()
			}()
		}
	})
}

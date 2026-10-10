package routes

import (
	"encoding/json"
	"maps"
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

// setupCompleteBeforeLock runs as /api/setup/complete reaches the store
// lock, after its first-run check and the work before the save. A no-op;
// tests hold two completes here to overlap them.
var setupCompleteBeforeLock = func() {}

// SetupRoutes registers setup wizard endpoints. The Store carries cfg
// and the lock; setup/complete keeps copy-on-write so a save failure
// never leaks partial mutations into the live config.
func SetupRoutes(r chi.Router, deps *SetupDeps, store *config.Store) {
	mu := store.RWMutex()
	cfg := store.Config()

	// Refused cross-site like GET /api/ffmpeg/check: it runs the same
	// CheckFFmpegCached on the configured path, which spawns that binary
	// whenever the cache is cold. The wizard and app.js fetch it same-origin.
	r.With(web.RefuseCrossSite).Get("/api/setup/status", func(rw http.ResponseWriter, req *http.Request) {
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

		// First-run setup must come from loopback. Without this gate,
		// a LAN peer reaching a `network_access=lan`-deployed-but-
		// unconfigured Moombox could complete the setup wizard
		// (including claiming the admin password) before the legitimate
		// user does — bypassing the F25 gate on /api/auth/set-password.
		// Default network_access=localhost makes this redundant out of
		// the box; the gate matters when the operator pre-flips to lan
		// before completing setup.
		if !web.IsLoopbackRequest(req) {
			jsonError(rw, "first-time setup must be from localhost", http.StatusUnauthorized)
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

		// Channel IDs through the shared normaliser first, as PUT /api/config
		// does: the web wizard resolves URLs itself, but whatever reaches
		// here is stored. No rate limit — this route is loopback-only and
		// runs once.
		channelErrs := normalizeChannelUpdates(req.Context(), updates)

		// Validate the field constraints before anything is applied.
		validationErrs := validateConfigUpdates(updates)
		maps.Copy(validationErrs, channelErrs)
		var storedFFmpeg string
		store.Read(func(c *config.MoomboxConfig) { storedFFmpeg = c.Paths.FfmpegPath })
		if msg := newFFmpegPathError(updates, storedFFmpeg); msg != "" {
			validationErrs["paths.ffmpeg_path"] = msg
		}
		if len(validationErrs) > 0 {
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

		setupCompleteBeforeLock()
		mu.Lock()

		// The first-run guard again, now under the lock. The read at the
		// top runs before the body is validated and the password hashed,
		// and two completes that overlapped there — the tab Moombox opens
		// on boot and a second one — both passed it: each applied and saved
		// its settings over the other's, and each scheduled a restart. Save
		// sets ConfigLoaded, so whichever takes the lock second finds setup
		// done.
		if cfg.ConfigLoaded {
			mu.Unlock()
			jsonError(rw, "setup already completed", http.StatusBadRequest)
			return
		}

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

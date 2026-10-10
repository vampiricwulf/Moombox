package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
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
	// CarryCookies carries the cookies the running cookie service wrote —
	// a browser login the wizard just ran — into the cookie file the setup
	// saves, before it is saved (cookies.AutoCookieService.CarryCookieFileTo,
	// the rule the TUI wizard's save command follows too). Nil carries
	// nothing.
	CarryCookies func(cookieFile string) error
}

// setupFieldError answers a setup refused over one field the way a
// validation failure is answered, so the wizard names the field.
func setupFieldError(rw http.ResponseWriter, field, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(rw).Encode(map[string]any{
		"error":   "Validation failed",
		"details": map[string]string{field: msg},
	})
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
			// The bounds every password surface shares: a password the
			// login would refuse as too long must not be set here.
			if msg := config.PasswordLengthError(password); msg != "" {
				jsonError(rw, msg, http.StatusBadRequest)
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

		// The output and staging directories the setup names — the body's,
		// or the config's for one it leaves out — are created before
		// anything is saved, outside the lock (I/O). One that cannot be
		// created refuses the setup under its field, as a validation error
		// is (config.MakeSetupDirs).
		var outputDir, stagingDir, cookieFile string
		store.Read(func(c *config.MoomboxConfig) {
			outputDir, stagingDir = c.Paths.OutputDirectory, c.Paths.StagingDirectory
			cookieFile = c.Cookies.CookieFile
		})
		if paths, ok := updates["paths"].(map[string]any); ok {
			if v, ok := paths["output_directory"].(string); ok {
				outputDir = v
			}
			if v, ok := paths["staging_directory"].(string); ok {
				stagingDir = v
			}
		}
		if ck, ok := updates["cookies"].(map[string]any); ok {
			if v, ok := ck["cookie_file"].(string); ok {
				cookieFile = v
			}
		}
		if err := config.MakeSetupDirs(outputDir, stagingDir); err != nil {
			var dirErr *config.SetupDirError
			if !errors.As(err, &dirErr) {
				jsonError(rw, "failed to create directories", http.StatusInternalServerError)
				return
			}
			setupFieldError(rw, dirErr.Key, fmt.Sprintf("could not create %q: %v", dirErr.Path, dirErr.Err))
			return
		}

		// The cookies a browser login in this wizard wrote went to the
		// cookie file the running service was built with; carry them into
		// the one this setup saves, so the restart loads them. The
		// Advanced step offers another cookie file beside the login, and
		// the restart used to load an empty jar from it although the
		// wizard had reported the login Done.
		if deps.CarryCookies != nil {
			if err := deps.CarryCookies(cookieFile); err != nil {
				setupFieldError(rw, "cookies.cookie_file", "could not carry the signed-in cookies here: "+err.Error())
				return
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

		// Snapshot values needed after unlock
		port := cfg.Network.Port
		httpsEnabled := cfg.Network.HTTPSEnabled

		mu.Unlock()

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

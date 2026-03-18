package routes

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// ConfigRoutesCallbacks contains optional callbacks invoked when config changes require hot-reload.
type ConfigRoutesCallbacks struct {
	// OnLogLevelChange is called when the log_level config field changes.
	OnLogLevelChange func(level string)
	// OnMaxParallelChange is called when num_parallel_downloads changes.
	OnMaxParallelChange func(n int)
	// OnHideFinishedAgeChanged is called when hide_finished_age_days changes,
	// so callers can re-broadcast the job list with updated archive thresholds.
	OnHideFinishedAgeChanged func()
	// OnChannelChange is called when channels are added, updated, or removed,
	// so monitors can re-evaluate their channel lists immediately.
	OnChannelChange func()
}

// isSafePath validates that a path doesn't contain traversal or absolute paths.
// Matches TypeScript safePathSchema.
func isSafePath(p string) bool {
	if p == "" {
		return true
	}
	if strings.Contains(p, "..") {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	// Check Windows drive letter (e.g., C:)
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		return false
	}
	return true
}

// validateConfigUpdates validates the config update map against TypeScript Zod
// schema constraints. Returns a map of field->error messages (empty if valid).
// Matches TypeScript updateConfigSchema constraints exactly.
func validateConfigUpdates(updates map[string]any) map[string]string {
	errs := make(map[string]string)

	// Network sub-fields
	if net, ok := updates["network"].(map[string]any); ok {
		if v, ok := net["port"].(float64); ok {
			if v < 1 || v > 65535 {
				errs["network.port"] = "port must be between 1 and 65535"
			}
		}
		if v, ok := net["network_access"].(string); ok {
			switch v {
			case "localhost", "lan", "external":
			default:
				errs["network.network_access"] = "network_access must be localhost, lan, or external"
			}
		}
		if v, ok := net["tls_cert_path"].(string); ok {
			if v != "" && !isSafePath(v) {
				errs["network.tls_cert_path"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := net["tls_key_path"].(string); ok {
			if v != "" && !isSafePath(v) {
				errs["network.tls_key_path"] = "Path cannot contain .. or be absolute"
			}
		}
	}

	// Paths sub-fields
	if paths, ok := updates["paths"].(map[string]any); ok {
		if v, ok := paths["log_file_path"].(string); ok {
			if !isSafePath(v) {
				errs["paths.log_file_path"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["database_path"].(string); ok {
			if !isSafePath(v) {
				errs["paths.database_path"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["output_directory"].(string); ok {
			if !isSafePath(v) {
				errs["paths.output_directory"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["staging_directory"].(string); ok {
			if !isSafePath(v) {
				errs["paths.staging_directory"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["ffmpeg_path"].(string); ok {
			if !isSafePath(v) {
				errs["paths.ffmpeg_path"] = "Path cannot contain .. or be absolute"
			}
		}
	}

	// Logs sub-fields
	if logs, ok := updates["logs"].(map[string]any); ok {
		if v, ok := logs["log_level"].(string); ok {
			switch strings.ToUpper(v) {
			case "DEBUG", "INFO", "WARN", "ERROR":
			default:
				errs["logs.log_level"] = "log_level must be DEBUG, INFO, WARN, or ERROR"
			}
		}
		if v, ok := logs["log_max_file_size"].(float64); ok {
			if v < 1024 || v > 1073741824 {
				errs["logs.log_max_file_size"] = "log_max_file_size must be between 1024 and 1073741824"
			}
		}
		if v, ok := logs["log_max_files"].(float64); ok {
			if v < 1 || v > 100 {
				errs["logs.log_max_files"] = "log_max_files must be between 1 and 100"
			}
		}
	}

	// Monitors sub-fields
	if mon, ok := updates["monitors"].(map[string]any); ok {
		if v, ok := mon["max_feed_items"].(float64); ok {
			if v < 1 || v > 100 {
				errs["monitors.max_feed_items"] = "max_feed_items must be between 1 and 100"
			}
		}
		if v, ok := mon["decapi_check_interval"].(float64); ok {
			if v < 15 || v > 3600 {
				errs["monitors.decapi_check_interval"] = "decapi_check_interval must be between 15 and 3600"
			}
		}
		if v, ok := mon["twitch_check_interval"].(float64); ok {
			if v < 5 || v > 3600 {
				errs["monitors.twitch_check_interval"] = "twitch_check_interval must be between 5 and 3600"
			}
		}
		if v, ok := mon["feed_check_interval"].(float64); ok {
			if v < 1 || v > 1440 {
				errs["monitors.feed_check_interval"] = "feed_check_interval must be between 1 and 1440"
			}
		}
		if v, ok := mon["hide_finished_age_days"].(float64); ok {
			if v < 0 {
				errs["monitors.hide_finished_age_days"] = "hide_finished_age_days must be at least 0"
			}
		}
	}

	// Downloader sub-fields
	if dl, ok := updates["downloader"].(map[string]any); ok {
		if v, ok := dl["output_template"].(string); ok {
			if len(v) > 500 {
				errs["downloader.output_template"] = "output_template must be at most 500 characters"
			}
		}
		if v, ok := dl["num_parallel_downloads"].(float64); ok {
			if v < 1 {
				errs["downloader.num_parallel_downloads"] = "num_parallel_downloads must be at least 1"
			}
		}
		if v, ok := dl["max_video_resolution"].(float64); ok {
			if v < 1 {
				errs["downloader.max_video_resolution"] = "max_video_resolution must be at least 1"
			}
		}
		if v, ok := dl["segment_retry_delay_cap"].(float64); ok {
			if v < 1 || v > 300 {
				errs["downloader.segment_retry_delay_cap"] = "segment_retry_delay_cap must be between 1 and 300"
			}
		}
		if v, ok := dl["segment_live_check_retries"].(float64); ok {
			if v < 1 || v > 100 {
				errs["downloader.segment_live_check_retries"] = "segment_live_check_retries must be between 1 and 100"
			}
		}
	}

	// Disk sub-fields
	if dk, ok := updates["disk"].(map[string]any); ok {
		warnPct, warnOK := dk["disk_warn_percent"].(float64)
		critPct, critOK := dk["disk_critical_percent"].(float64)
		if warnOK {
			if warnPct < 1 || warnPct > 100 {
				errs["disk.disk_warn_percent"] = "disk_warn_percent must be between 1 and 100"
			}
		}
		if critOK {
			if critPct < 1 || critPct > 100 {
				errs["disk.disk_critical_percent"] = "disk_critical_percent must be between 1 and 100"
			}
		}
		if warnOK && critOK && critPct <= warnPct {
			errs["disk.disk_critical_percent"] = "critical threshold must be higher than warning threshold"
		}
	}

	// Cookies sub-fields
	if ck, ok := updates["cookies"].(map[string]any); ok {
		if v, ok := ck["cookie_file"].(string); ok {
			if !isSafePath(v) {
				errs["cookies.cookie_file"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := ck["browser_profile_dir"].(string); ok {
			if !isSafePath(v) {
				errs["cookies.browser_profile_dir"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := ck["refresh_interval"].(float64); ok {
			if v < 10 {
				errs["cookies.refresh_interval"] = "refresh_interval must be at least 10"
			}
		}
	}

	return errs
}

// applyConfigUpdates applies allowlisted config fields from a snake_case map
// to the config struct. Used by both PUT /config and POST /setup/complete.
// Matches TypeScript updateConfigSchema field names exactly.
func applyConfigUpdates(cfg *config.MoomboxConfig, updates map[string]any) {
	// Network sub-fields
	if net, ok := updates["network"].(map[string]any); ok {
		if v, ok := net["port"].(float64); ok {
			cfg.Network.Port = int(v)
		}
		if v, ok := net["network_access"].(string); ok {
			cfg.Network.NetworkAccess = v
		}
		if v, ok := net["https_enabled"].(bool); ok {
			cfg.Network.HTTPSEnabled = v
		}
		if v, ok := net["tls_cert_path"].(string); ok {
			cfg.Network.TLSCertPath = v
		}
		if v, ok := net["tls_key_path"].(string); ok {
			cfg.Network.TLSKeyPath = v
		}
	}

	// Paths sub-fields
	if paths, ok := updates["paths"].(map[string]any); ok {
		if v, ok := paths["log_file_path"].(string); ok {
			cfg.Paths.LogFilePath = v
		}
		if v, ok := paths["database_path"].(string); ok {
			cfg.Paths.DatabasePath = v
		}
		if v, ok := paths["output_directory"].(string); ok {
			cfg.Paths.OutputDirectory = v
		}
		if v, ok := paths["staging_directory"].(string); ok {
			cfg.Paths.StagingDirectory = v
		}
		if v, ok := paths["ffmpeg_path"].(string); ok {
			cfg.Paths.FfmpegPath = v
		}
	}

	// Logs sub-fields
	if logs, ok := updates["logs"].(map[string]any); ok {
		if v, ok := logs["log_level"].(string); ok {
			cfg.Logs.LogLevel = v
		}
		if v, ok := logs["log_max_file_size"].(float64); ok {
			cfg.Logs.LogMaxFileSize = int(v)
		}
		if v, ok := logs["log_max_files"].(float64); ok {
			cfg.Logs.LogMaxFiles = int(v)
		}
	}

	// Monitors sub-fields
	if mon, ok := updates["monitors"].(map[string]any); ok {
		if v, ok := mon["max_feed_items"].(float64); ok {
			cfg.Monitors.MaxFeedItems = int(v)
		}
		if v, ok := mon["feed_check_interval"].(float64); ok {
			cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: v}
		} else if vs, ok := mon["feed_check_interval"].(string); ok {
			cfg.Monitors.FeedCheckInterval = config.ParseFlexDuration(vs, "minutes", cfg.Monitors.FeedCheckInterval.Value)
		}
		if val, exists := mon["decapi_check_interval"]; exists {
			if v, ok := val.(float64); ok {
				n := int(v)
				cfg.Monitors.DecapiCheckInterval = &n
			} else {
				cfg.Monitors.DecapiCheckInterval = nil
			}
		}
		if val, exists := mon["twitch_check_interval"]; exists {
			if v, ok := val.(float64); ok {
				n := int(v)
				cfg.Monitors.TwitchCheckInterval = &n
			} else {
				cfg.Monitors.TwitchCheckInterval = nil
			}
		}
		if v, ok := mon["hide_finished_age_days"].(float64); ok {
			cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: v}
		} else if vs, ok := mon["hide_finished_age_days"].(string); ok {
			cfg.Monitors.HideFinishedAgeDays = config.ParseFlexDuration(vs, "days", cfg.Monitors.HideFinishedAgeDays.Value)
		}
	}

	// Downloader sub-fields
	if dl, ok := updates["downloader"].(map[string]any); ok {
		if v, ok := dl["max_video_resolution"].(float64); ok {
			cfg.Downloader.MaxVideoResolution = int(v)
		}
		if v, ok := dl["output_template"].(string); ok {
			cfg.Downloader.OutputTemplate = v
		}
		if v, ok := dl["num_parallel_downloads"].(float64); ok {
			cfg.Downloader.NumParallelDownloads = int(v)
		}
		if v, ok := dl["download_chat"].(bool); ok {
			cfg.Downloader.DownloadChat = v
		}
		if v, ok := dl["prefer_60fps"].(bool); ok {
			cfg.Downloader.Prefer60fps = v
		}
		if v, ok := dl["segment_retry_delay_cap"].(float64); ok {
			cfg.Downloader.SegmentRetryDelayCap = int(v)
		}
		if v, ok := dl["segment_live_check_retries"].(float64); ok {
			cfg.Downloader.SegmentLiveCheckRetries = int(v)
		}
		if v, ok := dl["po_token"].(string); ok {
			cfg.Downloader.PoToken = v
		}
		if v, ok := dl["visitor_data"].(string); ok {
			cfg.Downloader.VisitorData = v
		}
		if v, ok := dl["pot_provider_url"].(string); ok {
			cfg.Downloader.PotProviderURL = v
		}
	}

	// Cookies
	if ck, ok := updates["cookies"].(map[string]any); ok {
		if v, ok := ck["cookie_file"].(string); ok {
			cfg.Cookies.CookieFile = v
		}
		if v, ok := ck["auto_enabled"].(bool); ok {
			cfg.Cookies.AutoEnabled = v
		}
		if v, ok := ck["browser_profile_dir"].(string); ok {
			cfg.Cookies.BrowserProfileDir = v
		}
		if v, ok := ck["platforms"].([]any); ok {
			var platforms []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					platforms = append(platforms, s)
				}
			}
			cfg.Cookies.Platforms = platforms
		}
		if v, ok := ck["active_platforms"].([]any); ok {
			var activePlatforms []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					activePlatforms = append(activePlatforms, s)
				}
			}
			cfg.Cookies.ActivePlatforms = activePlatforms
		}
		if val, exists := ck["refresh_interval"]; exists {
			if v, ok := val.(float64); ok {
				cfg.Cookies.RefreshInterval = config.FlexDuration{Value: v}
			} else if vs, ok := val.(string); ok {
				cfg.Cookies.RefreshInterval = config.ParseFlexDuration(vs, "minutes", cfg.Cookies.RefreshInterval.Value)
			} else {
				// null — reset to zero; RefreshService defaults to 30min at runtime
				cfg.Cookies.RefreshInterval = config.FlexDuration{}
			}
		}
	}

	// Disk
	if dk, ok := updates["disk"].(map[string]any); ok {
		if v, ok := dk["disk_warn_percent"].(float64); ok {
			cfg.Disk.WarnPercent = int(v)
		}
		if v, ok := dk["disk_critical_percent"].(float64); ok {
			cfg.Disk.CriticalPercent = int(v)
		}
	}

	// Updates
	if upd, ok := updates["updates"].(map[string]any); ok {
		if v, ok := upd["auto_check_updates"].(bool); ok {
			cfg.Updates.AutoCheckUpdates = v
		}
	}

	// Notifications
	if notifs, ok := updates["notifications"].([]any); ok {
		var configs []config.NotificationConfig
		for _, n := range notifs {
			if nm, ok := n.(map[string]any); ok {
				nc := config.NotificationConfig{}
				if v, ok := nm["url"].(string); ok {
					nc.URL = v
				}
				if v, ok := nm["tags"].([]any); ok {
					for _, t := range v {
						if s, ok := t.(string); ok {
							nc.Tags = append(nc.Tags, s)
						}
					}
				}
				if v, ok := nm["events"].([]any); ok {
					for _, e := range v {
						if s, ok := e.(string); ok {
							nc.Events = append(nc.Events, s)
						}
					}
				}
				configs = append(configs, nc)
			}
		}
		cfg.Notifications = configs
	}

	// Channels
	if chs, ok := updates["channels"].([]any); ok {
		data, _ := json.Marshal(chs)
		var channels []config.ChannelConfig
		if json.Unmarshal(data, &channels) == nil {
			cfg.Channels = channels
		}
	}
}

// ConfigRoutes registers config-related API routes.
// cfgMu protects concurrent reads/writes to the shared cfg struct.
func ConfigRoutes(r chi.Router, cfg *config.MoomboxConfig, cfgMu *sync.RWMutex, saveConfig func(*config.MoomboxConfig) error, callbacks *ConfigRoutesCallbacks) {
	// GET /api/config
	r.Get("/api/config", func(rw http.ResponseWriter, req *http.Request) {
		// Clone config and return with hasPassword injected.
		// PasswordHash has json:"-" tag so it's already excluded from marshaling.
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		resp := struct {
			*config.MoomboxConfig
			HasPassword bool `json:"hasPassword"`
		}{
			MoomboxConfig: cfg,
			HasPassword:   cfg.Network.PasswordHash != "",
		}
		jsonResponse(rw, resp)
	})

	// PUT /api/config
	r.Put("/api/config", func(rw http.ResponseWriter, req *http.Request) {
		var updates map[string]any
		if err := json.NewDecoder(req.Body).Decode(&updates); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

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

		cfgMu.Lock()

		// Prevent enabling external access without a password
		if net, ok := updates["network"].(map[string]any); ok {
			if v, ok := net["network_access"].(string); ok && v == "external" {
				if cfg.Network.PasswordHash == "" {
					cfgMu.Unlock()
					jsonError(rw, "A password must be set before enabling external access. Go to Settings \u2192 Security.", http.StatusBadRequest)
					return
				}
			}
		}

		// Snapshot values that need hot-reload comparison after save.
		oldLogLevel := cfg.Logs.LogLevel
		oldNumParallel := cfg.Downloader.NumParallelDownloads
		oldHideAge := cfg.Monitors.HideFinishedAgeDays.Value

		// Work on a copy so the live config isn't modified if save fails
		cfgCopy := *cfg
		applyConfigUpdates(&cfgCopy, updates)

		// Persist to disk
		if saveConfig != nil {
			if err := saveConfig(&cfgCopy); err != nil {
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}

		// Save succeeded — apply to live config
		*cfg = cfgCopy

		// Read new values while still holding the lock
		newLogLevel := cfg.Logs.LogLevel
		newNumParallel := cfg.Downloader.NumParallelDownloads
		newHideAge := cfg.Monitors.HideFinishedAgeDays.Value

		cfgMu.Unlock()

		// Hot-reload runtime-reloadable settings (outside the lock to avoid deadlocks in callbacks)
		if callbacks != nil {
			if newLogLevel != oldLogLevel && callbacks.OnLogLevelChange != nil {
				callbacks.OnLogLevelChange(newLogLevel)
			}
			if newNumParallel != oldNumParallel && callbacks.OnMaxParallelChange != nil {
				callbacks.OnMaxParallelChange(newNumParallel)
			}
			if newHideAge != oldHideAge && callbacks.OnHideFinishedAgeChanged != nil {
				callbacks.OnHideFinishedAgeChanged()
			}
			if _, hasChannels := updates["channels"]; hasChannels && callbacks.OnChannelChange != nil {
				callbacks.OnChannelChange()
			}
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}

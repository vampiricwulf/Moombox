package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults returns a new MoomboxConfig with all default values applied.
func Defaults() *MoomboxConfig {
	return &MoomboxConfig{
		Network: NetworkConfig{
			Port:          774,
			NetworkAccess: "localhost",
		},
		Paths: PathsConfig{
			DatabasePath:     "./moombox.db",
			LogFilePath:      "./moombox.log",
			OutputDirectory:  "./output",
			StagingDirectory: "./staging",
		},
		Logs: LogsConfig{
			LogLevel:       "INFO",
			LogMaxFileSize: 10485760, // 10MB
			LogMaxFiles:    5,
		},
		Monitors: MonitorsConfig{
			MaxFeedItems:        15,
			FeedCheckInterval:   FlexDuration{Value: 10}, // minutes
			HideFinishedAgeDays: FlexDuration{Value: 30},
		},
		Downloader: DownloaderConfig{
			OutputTemplate:          "${channel}/${start_date} ${title} [${id}]",
			MaxVideoResolution:      2160,
			NumParallelDownloads:    2,
			DownloadChat:            true,
			Prefer60fps:             true,
			SegmentRetryDelayCap:    60,
			SegmentLiveCheckRetries: 16,
		},
		Cookies: CookiesConfig{
			CookieFile:        "./cookies.txt",
			BrowserProfileDir: "./browser-profile",
			Platforms:         []string{},
			RefreshInterval:   FlexDuration{Value: 360}, // 6 hours in minutes
		},
		Disk: DiskConfig{
			WarnPercent:     90,
			CriticalPercent: 95,
		},
		Updates: UpdatesConfig{
			AutoCheckUpdates: true,
		},
	}
}

// Load reads configuration from a TOML file, searching multiple locations.
// If customPath is empty, it searches: cwd -> ./config/ -> ~/.config/moombox/
func Load(customPath string) (*MoomboxConfig, error) {
	paths := []string{}
	if customPath != "" {
		paths = append(paths, customPath)
	}

	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	paths = append(paths,
		filepath.Join(cwd, "config.toml"),
		filepath.Join(cwd, "config", "config.toml"),
	)
	if home != "" {
		paths = append(paths, filepath.Join(home, ".config", "moombox", "config.toml"))
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return loadFromFile(p)
		}
	}

	// No config file found — use defaults
	cfg := Defaults()
	return cfg, nil
}

func loadFromFile(path string) (*MoomboxConfig, error) {
	cfg := Defaults()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	// Migrate legacy/old-format fields by decoding to a raw map.
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err == nil {
		migrateOldFormat(cfg, raw)
	}

	cfg.ConfigLoaded = true
	validate(cfg)
	return cfg, nil
}

// migrateOldFormat handles migration from the old flat config format to the
// new structured format with TOML tables.
func migrateOldFormat(cfg *MoomboxConfig, raw map[string]any) {
	// Legacy allow_lan/allow_external → network_access (matches TS config.ts)
	if cfg.Network.NetworkAccess == "" || cfg.Network.NetworkAccess == "localhost" {
		allowLan, hasLan := raw["allow_lan"].(bool)
		allowExt, hasExt := raw["allow_external"].(bool)
		if hasLan || hasExt {
			if allowLan && allowExt {
				cfg.Network.NetworkAccess = "external"
			} else if allowLan {
				cfg.Network.NetworkAccess = "lan"
			} else {
				cfg.Network.NetworkAccess = "localhost"
			}
		}
	}

	// Legacy [tasklist] section → monitors.hide_finished_age_days
	if tl, ok := raw["tasklist"].(map[string]any); ok {
		if _, hasMonitors := raw["monitors"]; !hasMonitors {
			if v, ok := tl["hide_finished_age_days"]; ok {
				cfg.Monitors.HideFinishedAgeDays = ParseFlexDuration(v, "days", cfg.Monitors.HideFinishedAgeDays.Value)
			}
		}
	}

	// Migrate old flat top-level keys → new sub-structs
	// Only migrate if the key exists at the top level (not inside a table)
	if v, ok := raw["port"]; ok {
		if n, ok := toFloat64(v); ok {
			cfg.Network.Port = int(n)
		}
	}
	if v, ok := raw["network_access"].(string); ok {
		cfg.Network.NetworkAccess = v
	}
	if v, ok := raw["https_enabled"].(bool); ok {
		cfg.Network.HTTPSEnabled = v
	}
	if v, ok := raw["tls_cert_path"].(string); ok {
		cfg.Network.TLSCertPath = v
	}
	if v, ok := raw["tls_key_path"].(string); ok {
		cfg.Network.TLSKeyPath = v
	}
	if v, ok := raw["password_hash"].(string); ok {
		cfg.Network.PasswordHash = v
	}
	if v, ok := raw["log_level"].(string); ok {
		cfg.Logs.LogLevel = v
	}
	if v, ok := raw["log_file_path"].(string); ok {
		cfg.Paths.LogFilePath = v
	}
	if v, ok := raw["log_max_file_size"]; ok {
		if n, ok := toFloat64(v); ok {
			cfg.Logs.LogMaxFileSize = int(n)
		}
	}
	if v, ok := raw["log_max_files"]; ok {
		if n, ok := toFloat64(v); ok {
			cfg.Logs.LogMaxFiles = int(n)
		}
	}
	if v, ok := raw["database_path"].(string); ok {
		cfg.Paths.DatabasePath = v
	}
	if v, ok := raw["max_feed_items"]; ok {
		if n, ok := toFloat64(v); ok {
			cfg.Monitors.MaxFeedItems = int(n)
		}
	}
	if v, ok := raw["feed_check_interval"]; ok {
		cfg.Monitors.FeedCheckInterval = ParseFlexDuration(v, "minutes", cfg.Monitors.FeedCheckInterval.Value)
	}
	if v, ok := raw["decapi_check_interval"]; ok {
		if n, ok := toFloat64(v); ok {
			val := int(n)
			cfg.Monitors.DecapiCheckInterval = &val
		}
	}
	if v, ok := raw["twitch_check_interval"]; ok {
		if n, ok := toFloat64(v); ok {
			val := int(n)
			cfg.Monitors.TwitchCheckInterval = &val
		}
	}
	if v, ok := raw["hide_finished_age_days"]; ok {
		cfg.Monitors.HideFinishedAgeDays = ParseFlexDuration(v, "days", cfg.Monitors.HideFinishedAgeDays.Value)
	}

	// Migrate old [downloader] keys that moved to [paths] or [cookies]
	if dl, ok := raw["downloader"].(map[string]any); ok {
		if v, ok := dl["output_directory"].(string); ok {
			// Only migrate if [paths] section doesn't exist
			if _, hasPaths := raw["paths"]; !hasPaths {
				cfg.Paths.OutputDirectory = v
			}
		}
		if v, ok := dl["staging_directory"].(string); ok {
			if _, hasPaths := raw["paths"]; !hasPaths {
				cfg.Paths.StagingDirectory = v
			}
		}
		if v, ok := dl["ffmpeg_path"].(string); ok {
			if _, hasPaths := raw["paths"]; !hasPaths {
				cfg.Paths.FfmpegPath = v
			}
		}
		if v, ok := dl["cookie_file"].(string); ok {
			if _, hasCookies := raw["cookies"]; !hasCookies {
				cfg.Cookies.CookieFile = v
			}
		}
	}

	// Migrate old [auto_cookies] → [cookies]
	if ac, ok := raw["auto_cookies"].(map[string]any); ok {
		if _, hasCookies := raw["cookies"]; !hasCookies {
			if v, ok := ac["enabled"].(bool); ok {
				cfg.Cookies.AutoEnabled = v
			}
			if v, ok := ac["browser_profile_dir"].(string); ok {
				cfg.Cookies.BrowserProfileDir = v
			}
			if v, ok := ac["refresh_interval"]; ok {
				cfg.Cookies.RefreshInterval = ParseFlexDuration(v, "minutes", cfg.Cookies.RefreshInterval.Value)
			}
			if arr, ok := ac["platforms"].([]any); ok {
				platforms := make([]string, 0, len(arr))
				for _, p := range arr {
					if s, ok := p.(string); ok {
						platforms = append(platforms, s)
					}
				}
				cfg.Cookies.Platforms = platforms
			}
		}
	}
}

// toFloat64 converts TOML numeric types (int64, float64) to float64.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// validate checks config values and replaces invalid ones with defaults.
func validate(cfg *MoomboxConfig) {
	defaults := Defaults()

	if cfg.Network.Port < 1 || cfg.Network.Port > 65535 {
		cfg.Network.Port = defaults.Network.Port
	}
	if cfg.Network.NetworkAccess != "localhost" && cfg.Network.NetworkAccess != "lan" && cfg.Network.NetworkAccess != "external" {
		cfg.Network.NetworkAccess = defaults.Network.NetworkAccess
	}
	switch strings.ToUpper(cfg.Logs.LogLevel) {
	case "DEBUG", "INFO", "WARN", "ERROR":
		cfg.Logs.LogLevel = strings.ToUpper(cfg.Logs.LogLevel)
	default:
		cfg.Logs.LogLevel = defaults.Logs.LogLevel
	}
	if cfg.Monitors.MaxFeedItems < 1 {
		cfg.Monitors.MaxFeedItems = defaults.Monitors.MaxFeedItems
	}
	if cfg.Monitors.FeedCheckInterval.Value < 1 {
		cfg.Monitors.FeedCheckInterval = defaults.Monitors.FeedCheckInterval
	}
	if cfg.Monitors.HideFinishedAgeDays.Value < 0 {
		cfg.Monitors.HideFinishedAgeDays = defaults.Monitors.HideFinishedAgeDays
	}
	if cfg.Monitors.DecapiCheckInterval != nil && (*cfg.Monitors.DecapiCheckInterval < 15 || *cfg.Monitors.DecapiCheckInterval > 3600) {
		cfg.Monitors.DecapiCheckInterval = nil
	}
	if cfg.Monitors.TwitchCheckInterval != nil && (*cfg.Monitors.TwitchCheckInterval < 1 || *cfg.Monitors.TwitchCheckInterval > 3600) {
		cfg.Monitors.TwitchCheckInterval = nil
	}
	if cfg.Logs.LogMaxFileSize < 1 {
		cfg.Logs.LogMaxFileSize = defaults.Logs.LogMaxFileSize
	}
	if cfg.Logs.LogMaxFiles < 1 {
		cfg.Logs.LogMaxFiles = defaults.Logs.LogMaxFiles
	}

	d := &cfg.Downloader
	if d.NumParallelDownloads < 1 {
		d.NumParallelDownloads = defaults.Downloader.NumParallelDownloads
	}
	if d.MaxVideoResolution < 1 {
		d.MaxVideoResolution = defaults.Downloader.MaxVideoResolution
	}
	if d.OutputTemplate == "" {
		d.OutputTemplate = defaults.Downloader.OutputTemplate
	}
	if d.SegmentRetryDelayCap < 1 {
		d.SegmentRetryDelayCap = defaults.Downloader.SegmentRetryDelayCap
	}
	if d.SegmentLiveCheckRetries < 1 {
		d.SegmentLiveCheckRetries = defaults.Downloader.SegmentLiveCheckRetries
	}

	if cfg.Paths.DatabasePath == "" {
		cfg.Paths.DatabasePath = defaults.Paths.DatabasePath
	}
	if cfg.Paths.LogFilePath == "" {
		cfg.Paths.LogFilePath = defaults.Paths.LogFilePath
	}
	if cfg.Paths.OutputDirectory == "" {
		cfg.Paths.OutputDirectory = defaults.Paths.OutputDirectory
	}
	if cfg.Paths.StagingDirectory == "" {
		cfg.Paths.StagingDirectory = defaults.Paths.StagingDirectory
	}
	if cfg.Cookies.CookieFile == "" {
		cfg.Cookies.CookieFile = defaults.Cookies.CookieFile
	}
	if cfg.Cookies.RefreshInterval.Value < 10 {
		cfg.Cookies.RefreshInterval = defaults.Cookies.RefreshInterval
	}

	// Disk thresholds
	if cfg.Disk.WarnPercent < 1 || cfg.Disk.WarnPercent > 99 {
		cfg.Disk.WarnPercent = defaults.Disk.WarnPercent
	}
	if cfg.Disk.CriticalPercent < 1 || cfg.Disk.CriticalPercent > 99 {
		cfg.Disk.CriticalPercent = defaults.Disk.CriticalPercent
	}
	if cfg.Disk.CriticalPercent <= cfg.Disk.WarnPercent {
		cfg.Disk.CriticalPercent = cfg.Disk.WarnPercent + 5
		if cfg.Disk.CriticalPercent > 99 {
			cfg.Disk.CriticalPercent = 99
		}
	}

	// Validate channel-level quality_preference (applies to both YouTube and Twitch)
	validQualities := map[string]bool{
		"best": true, "2160p60": true, "2160p": true, "1440p60": true, "1440p": true,
		"1080p60": true, "1080p": true, "900p60": true, "900p": true,
		"720p60": true, "720p": true, "480p": true, "360p": true, "160p": true,
		"audio_only": true,
	}
	for i := range cfg.Channels {
		if cfg.Channels[i].QualityPreference != "" && !validQualities[cfg.Channels[i].QualityPreference] {
			cfg.Channels[i].QualityPreference = ""
		}
	}
}

// Save writes the configuration to a TOML file at the given path.
func Save(cfg *MoomboxConfig, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	validate(cfg)

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		os.Remove(path + ".tmp")
		return fmt.Errorf("failed to encode config: %w", err)
	}
	f.Close()

	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	cfg.ConfigLoaded = true
	return nil
}

// GetActivePlatforms determines which platforms are active for cookie status
// display. If ActivePlatforms is explicitly set, it is used as-is. Otherwise,
// the function infers active platforms from the enabled channels list.
func GetActivePlatforms(cfg *MoomboxConfig) (youtube, twitch bool) {
	// 1. Explicit override (user set via settings)
	if len(cfg.Cookies.ActivePlatforms) > 0 {
		for _, p := range cfg.Cookies.ActivePlatforms {
			switch strings.ToLower(p) {
			case "youtube":
				youtube = true
			case "twitch":
				twitch = true
			}
		}
		return
	}
	// 2. Platforms with verified cookies (set by auto-cookie or cookie file validation)
	if len(cfg.Cookies.Platforms) > 0 {
		for _, p := range cfg.Cookies.Platforms {
			switch strings.ToLower(p) {
			case "youtube":
				youtube = true
			case "twitch":
				twitch = true
			}
		}
		return
	}
	// 3. Infer from enabled channels
	for i := range cfg.Channels {
		if !cfg.Channels[i].IsEnabled() {
			continue
		}
		switch cfg.Channels[i].GetPlatform() {
		case "youtube":
			youtube = true
		case "twitch":
			twitch = true
		}
		if youtube && twitch {
			return
		}
	}
	return
}

// sanitizeTemplateStr removes invalid filesystem characters from a string,
// preserving Unicode characters (CJK, Japanese, etc).
var invalidFSChars = regexp.MustCompile(`[^\w\s\-\x{3000}-\x{303F}\x{3040}-\x{309F}\x{30A0}-\x{30FF}\x{FF00}-\x{FFEF}\x{4E00}-\x{9FAF}]`)

func sanitizeTemplateStr(s string) string {
	return strings.TrimSpace(invalidFSChars.ReplaceAllString(s, ""))
}

// ResolveTemplate resolves an output template with the given variables.
func ResolveTemplate(template string, vars TemplateVariables) string {
	result := template

	safeTitle := sanitizeTemplateStr(vars.Title)
	safeChannel := sanitizeTemplateStr(vars.Channel)

	result = strings.ReplaceAll(result, "${title}", safeTitle)
	result = strings.ReplaceAll(result, "${id}", vars.ID)
	result = strings.ReplaceAll(result, "${channel}", safeChannel)

	now := time.Now()
	if vars.Date != nil {
		if t, err := time.Parse(time.RFC3339, *vars.Date); err == nil {
			now = t
		}
	}

	result = strings.ReplaceAll(result, "${start_date}", now.Format("20060102"))
	result = strings.ReplaceAll(result, "${start_time}", now.Format("1504"))

	return result
}

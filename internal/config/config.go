package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// dacledDirs memoises the set of dirs that have had their DACL tightened
// this process lifetime. icacls shells out (~30-80ms on Windows); without
// memoisation every config Save under Store.Update's write lock pays that
// latency on a no-op. The set lives for the process — restart re-applies.
var (
	dacledDirsMu sync.Mutex
	dacledDirs   = make(map[string]struct{})
)

// Defaults returns a new MoomboxConfig with all default values applied.
func Defaults() *MoomboxConfig {
	return &MoomboxConfig{
		Network: NetworkConfig{
			Port:               774,
			NetworkAccess:      "localhost",
			ClientTokenTTLDays: 365,
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
		Bgutils: BgutilsConfig{
			UseSidecar: true,
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

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to get working directory: %v\n", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to get home directory: %v\n", err)
	}

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
	// Read once and decode twice (struct + raw map) from the same bytes
	// so we don't pay disk I/O twice or risk the file changing between
	// reads. Second decode error is now surfaced to the caller; the
	// pre-fix code silently swallowed it which masked migrations that
	// failed because the TOML structure couldn't be expressed as a
	// generic map. Audit reports/config.md Finding 28.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	cfg := Defaults()
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	// Migrate legacy/old-format fields by decoding the SAME bytes to a
	// raw map. Errors propagate — migration is part of correctness, not
	// a best-effort step.
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config %s for migration: %w", path, err)
	}
	migrateOldFormat(cfg, raw)

	cfg.ConfigLoaded = true
	Normalize(cfg)
	return cfg, nil
}

// migrateOldFormat handles migration from the old flat config format to the
// new structured format with TOML tables.
func migrateOldFormat(cfg *MoomboxConfig, raw map[string]any) {
	// Legacy allow_lan/allow_external → network_access (matches TS config.ts).
	//
	// Skip the migration when the user has EXPLICITLY set network_access
	// anywhere (in [network], at the top level, or via the new key) —
	// otherwise the migration silently overrides the explicit choice
	// when the user kept their old `allow_lan = true` line for backward
	// compat AND added the new [network] section. Audit reports/config.md
	// Finding 8.
	networkAccessExplicit := false
	if _, ok := raw["network_access"]; ok {
		networkAccessExplicit = true
	}
	if networkSec, ok := raw["network"].(map[string]any); ok {
		if _, ok := networkSec["network_access"]; ok {
			networkAccessExplicit = true
		}
	}
	if !networkAccessExplicit && (cfg.Network.NetworkAccess == "" || cfg.Network.NetworkAccess == "localhost") {
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

	// Migrate old flat top-level keys → new sub-structs.
	// Only migrate if the key exists at the top level (not inside a table)
	// AND the corresponding structured section doesn't already define that field.
	networkRaw, _ := raw["network"].(map[string]any)
	logsRaw, _ := raw["logs"].(map[string]any)
	pathsRawFlat, _ := raw["paths"].(map[string]any)
	monitorsRaw, _ := raw["monitors"].(map[string]any)

	if v, ok := raw["port"]; ok {
		if networkRaw == nil || networkRaw["port"] == nil {
			if n, ok := toFloat64(v); ok {
				cfg.Network.Port = int(n)
			}
		}
	}
	if v, ok := raw["network_access"].(string); ok {
		if networkRaw == nil || networkRaw["network_access"] == nil {
			cfg.Network.NetworkAccess = v
		}
	}
	if v, ok := raw["https_enabled"].(bool); ok {
		if networkRaw == nil || networkRaw["https_enabled"] == nil {
			cfg.Network.HTTPSEnabled = v
		}
	}
	if v, ok := raw["tls_cert_path"].(string); ok {
		if networkRaw == nil || networkRaw["tls_cert_path"] == nil {
			cfg.Network.TLSCertPath = v
		}
	}
	if v, ok := raw["tls_key_path"].(string); ok {
		if networkRaw == nil || networkRaw["tls_key_path"] == nil {
			cfg.Network.TLSKeyPath = v
		}
	}
	if v, ok := raw["password_hash"].(string); ok {
		if networkRaw == nil || networkRaw["password_hash"] == nil {
			cfg.Network.PasswordHash = v
		}
	}
	if v, ok := raw["log_level"].(string); ok {
		if logsRaw == nil || logsRaw["log_level"] == nil {
			cfg.Logs.LogLevel = v
		}
	}
	if v, ok := raw["log_file_path"].(string); ok {
		if pathsRawFlat == nil || pathsRawFlat["log_file_path"] == nil {
			cfg.Paths.LogFilePath = v
		}
	}
	if v, ok := raw["log_max_file_size"]; ok {
		if logsRaw == nil || logsRaw["log_max_file_size"] == nil {
			if n, ok := toFloat64(v); ok {
				cfg.Logs.LogMaxFileSize = int(n)
			}
		}
	}
	if v, ok := raw["log_max_files"]; ok {
		if logsRaw == nil || logsRaw["log_max_files"] == nil {
			if n, ok := toFloat64(v); ok {
				cfg.Logs.LogMaxFiles = int(n)
			}
		}
	}
	if v, ok := raw["database_path"].(string); ok {
		if pathsRawFlat == nil || pathsRawFlat["database_path"] == nil {
			cfg.Paths.DatabasePath = v
		}
	}
	if v, ok := raw["max_feed_items"]; ok {
		if monitorsRaw == nil || monitorsRaw["max_feed_items"] == nil {
			if n, ok := toFloat64(v); ok {
				cfg.Monitors.MaxFeedItems = int(n)
			}
		}
	}
	if v, ok := raw["feed_check_interval"]; ok {
		if monitorsRaw == nil || monitorsRaw["feed_check_interval"] == nil {
			cfg.Monitors.FeedCheckInterval = ParseFlexDuration(v, "minutes", cfg.Monitors.FeedCheckInterval.Value)
		}
	}
	if v, ok := raw["decapi_check_interval"]; ok {
		if monitorsRaw == nil || monitorsRaw["decapi_check_interval"] == nil {
			if n, ok := toFloat64(v); ok {
				val := int(n)
				cfg.Monitors.DecapiCheckInterval = &val
			}
		}
	}
	if v, ok := raw["twitch_check_interval"]; ok {
		if monitorsRaw == nil || monitorsRaw["twitch_check_interval"] == nil {
			if n, ok := toFloat64(v); ok {
				val := int(n)
				cfg.Monitors.TwitchCheckInterval = &val
			}
		}
	}
	if v, ok := raw["hide_finished_age_days"]; ok {
		if monitorsRaw == nil || monitorsRaw["hide_finished_age_days"] == nil {
			cfg.Monitors.HideFinishedAgeDays = ParseFlexDuration(v, "days", cfg.Monitors.HideFinishedAgeDays.Value)
		}
	}

	// Migrate old [downloader] keys that moved to [paths] or [cookies].
	// Per-field checks: only migrate if the target section doesn't already define that field.
	if dl, ok := raw["downloader"].(map[string]any); ok {
		pathsRaw, _ := raw["paths"].(map[string]any)
		cookiesRaw, _ := raw["cookies"].(map[string]any)
		if v, ok := dl["output_directory"].(string); ok {
			if pathsRaw == nil || pathsRaw["output_directory"] == nil {
				cfg.Paths.OutputDirectory = v
			}
		}
		if v, ok := dl["staging_directory"].(string); ok {
			if pathsRaw == nil || pathsRaw["staging_directory"] == nil {
				cfg.Paths.StagingDirectory = v
			}
		}
		if v, ok := dl["ffmpeg_path"].(string); ok {
			if pathsRaw == nil || pathsRaw["ffmpeg_path"] == nil {
				cfg.Paths.FfmpegPath = v
			}
		}
		if v, ok := dl["cookie_file"].(string); ok {
			if cookiesRaw == nil || cookiesRaw["cookie_file"] == nil {
				cfg.Cookies.CookieFile = v
			}
		}
	}

	// Migrate old [auto_cookies] → [cookies].
	// Per-field checks: only migrate fields that the target [cookies] section doesn't define.
	if ac, ok := raw["auto_cookies"].(map[string]any); ok {
		cookiesRaw, _ := raw["cookies"].(map[string]any)
		if v, ok := ac["enabled"].(bool); ok {
			if cookiesRaw == nil || cookiesRaw["auto_enabled"] == nil {
				cfg.Cookies.AutoEnabled = v
			}
		}
		if v, ok := ac["browser_profile_dir"].(string); ok {
			if cookiesRaw == nil || cookiesRaw["browser_profile_dir"] == nil {
				cfg.Cookies.BrowserProfileDir = v
			}
		}
		if v, ok := ac["refresh_interval"]; ok {
			if cookiesRaw == nil || cookiesRaw["refresh_interval"] == nil {
				cfg.Cookies.RefreshInterval = ParseFlexDuration(v, "minutes", cfg.Cookies.RefreshInterval.Value)
			}
		}
		if arr, ok := ac["platforms"].([]any); ok {
			if cookiesRaw == nil || cookiesRaw["platforms"] == nil {
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

// Validate reports configuration issues without mutating cfg. Returns a
// slice of errors — empty when the config is fully valid. Paired with
// Normalize, which applies defaults/clamps (DECISIONS #9). Load auto-
// normalises to avoid data loss on legacy configs; Save validates first
// and refuses to write a failing config so the user sees the problem.
func Validate(cfg *MoomboxConfig) []error {
	return validateOrNormalize(cfg, true)
}

// Normalize replaces out-of-range or empty fields with their defaults.
// Idempotent — safe to call multiple times. Use Validate to surface issues
// before Normalize hides them (DECISIONS #9).
func Normalize(cfg *MoomboxConfig) {
	_ = validateOrNormalize(cfg, false)
}

// validateOrNormalize is the shared implementation. When reportOnly=true it
// records each issue as an error and leaves cfg untouched; when false it
// overwrites the offending field with the default and returns nil.
func validateOrNormalize(cfg *MoomboxConfig, reportOnly bool) []error {
	defaults := Defaults()
	var errs []error
	fail := func(format string, args ...any) {
		if reportOnly {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	if cfg.Network.Port < 1 || cfg.Network.Port > 65535 {
		fail("network.port %d out of range 1..65535", cfg.Network.Port)
		if !reportOnly {
			cfg.Network.Port = defaults.Network.Port
		}
	}
	if cfg.Network.NetworkAccess != "localhost" && cfg.Network.NetworkAccess != "lan" && cfg.Network.NetworkAccess != "external" {
		fail("network.network_access %q must be one of localhost|lan|external", cfg.Network.NetworkAccess)
		if !reportOnly {
			cfg.Network.NetworkAccess = defaults.Network.NetworkAccess
		}
	}
	// ClientTokenTTLDays: 1 day to 10 years. Default to 365d (1y) if unset or out of range.
	if cfg.Network.ClientTokenTTLDays <= 0 || cfg.Network.ClientTokenTTLDays > 3650 {
		fail("network.client_token_ttl_days %d out of range 1..3650", cfg.Network.ClientTokenTTLDays)
		if !reportOnly {
			cfg.Network.ClientTokenTTLDays = defaults.Network.ClientTokenTTLDays
		}
	}
	switch strings.ToUpper(cfg.Logs.LogLevel) {
	case "DEBUG", "INFO", "WARN", "ERROR":
		if !reportOnly {
			cfg.Logs.LogLevel = strings.ToUpper(cfg.Logs.LogLevel)
		}
	default:
		fail("logs.log_level %q must be one of DEBUG|INFO|WARN|ERROR", cfg.Logs.LogLevel)
		if !reportOnly {
			cfg.Logs.LogLevel = defaults.Logs.LogLevel
		}
	}
	// Upper bounds derived from plausible operating envelopes — over-large
	// values turn into per-tick scan costs (MaxFeedItems = O(N) per channel
	// per cycle), wall-clock latency (FeedCheckInterval > 24h means jobs
	// would never get found before they finish), and storage retention
	// shifts (HideFinishedAgeDays > 1y is a UI bug, not a feature). Audit
	// reports/config.md Finding 13.
	if cfg.Monitors.MaxFeedItems < 1 || cfg.Monitors.MaxFeedItems > 1000 {
		fail("monitors.max_feed_items %d out of range 1..1000", cfg.Monitors.MaxFeedItems)
		if !reportOnly {
			cfg.Monitors.MaxFeedItems = defaults.Monitors.MaxFeedItems
		}
	}
	if cfg.Monitors.FeedCheckInterval.Value < 1 || cfg.Monitors.FeedCheckInterval.Value > 1440 {
		fail("monitors.feed_check_interval %v out of range 1..1440 minutes", cfg.Monitors.FeedCheckInterval.Value)
		if !reportOnly {
			cfg.Monitors.FeedCheckInterval = defaults.Monitors.FeedCheckInterval
		}
	}
	if cfg.Monitors.HideFinishedAgeDays.Value < 0 || cfg.Monitors.HideFinishedAgeDays.Value > 365 {
		fail("monitors.hide_finished_age_days %v out of range 0..365 days", cfg.Monitors.HideFinishedAgeDays.Value)
		if !reportOnly {
			cfg.Monitors.HideFinishedAgeDays = defaults.Monitors.HideFinishedAgeDays
		}
	}
	if cfg.Monitors.DecapiCheckInterval != nil && (*cfg.Monitors.DecapiCheckInterval < 15 || *cfg.Monitors.DecapiCheckInterval > 3600) {
		fail("monitors.decapi_check_interval %d out of range 15..3600", *cfg.Monitors.DecapiCheckInterval)
		if !reportOnly {
			cfg.Monitors.DecapiCheckInterval = nil
		}
	}
	// Aligned with the PUT /api/config Zod schema (config_routes.go:142)
	// so a hand-edited TOML can't sneak in a value the web UI rejects.
	// 1-second polling against Twitch's GQL endpoint floods their rate
	// limiter; 5 seconds is the established minimum that matches the
	// WebSocket-style heartbeat cadence chats use. Audit reports/config.md
	// Finding 6.
	if cfg.Monitors.TwitchCheckInterval != nil && (*cfg.Monitors.TwitchCheckInterval < 5 || *cfg.Monitors.TwitchCheckInterval > 3600) {
		fail("monitors.twitch_check_interval %d out of range 5..3600", *cfg.Monitors.TwitchCheckInterval)
		if !reportOnly {
			cfg.Monitors.TwitchCheckInterval = nil
		}
	}
	// Bounds match the PUT /api/config Zod schema (config_routes.go) so a
	// hand-edited TOML can't get past Validate with a value the web UI
	// would have rejected. 1KB lower bound prevents pathological log
	// rotation; 1GB upper bound prevents accidental disk exhaustion.
	if cfg.Logs.LogMaxFileSize < 1024 || cfg.Logs.LogMaxFileSize > 1073741824 {
		fail("logs.log_max_file_size %d out of range 1024..1073741824", cfg.Logs.LogMaxFileSize)
		if !reportOnly {
			cfg.Logs.LogMaxFileSize = defaults.Logs.LogMaxFileSize
		}
	}
	if cfg.Logs.LogMaxFiles < 1 || cfg.Logs.LogMaxFiles > 100 {
		fail("logs.log_max_files %d out of range 1..100", cfg.Logs.LogMaxFiles)
		if !reportOnly {
			cfg.Logs.LogMaxFiles = defaults.Logs.LogMaxFiles
		}
	}

	d := &cfg.Downloader
	if d.NumParallelDownloads < 1 {
		fail("downloader.num_parallel_downloads %d must be >= 1", d.NumParallelDownloads)
		if !reportOnly {
			d.NumParallelDownloads = defaults.Downloader.NumParallelDownloads
		}
	}
	if d.MaxVideoResolution < 1 {
		fail("downloader.max_video_resolution %d must be >= 1", d.MaxVideoResolution)
		if !reportOnly {
			d.MaxVideoResolution = defaults.Downloader.MaxVideoResolution
		}
	}
	if d.OutputTemplate == "" {
		fail("downloader.output_template must not be empty")
		if !reportOnly {
			d.OutputTemplate = defaults.Downloader.OutputTemplate
		}
	}
	if d.SegmentRetryDelayCap < 1 {
		fail("downloader.segment_retry_delay_cap %d must be >= 1", d.SegmentRetryDelayCap)
		if !reportOnly {
			d.SegmentRetryDelayCap = defaults.Downloader.SegmentRetryDelayCap
		}
	}
	if d.SegmentLiveCheckRetries < 1 {
		fail("downloader.segment_live_check_retries %d must be >= 1", d.SegmentLiveCheckRetries)
		if !reportOnly {
			d.SegmentLiveCheckRetries = defaults.Downloader.SegmentLiveCheckRetries
		}
	}

	if cfg.Paths.DatabasePath == "" {
		fail("paths.database_path must not be empty")
		if !reportOnly {
			cfg.Paths.DatabasePath = defaults.Paths.DatabasePath
		}
	}
	if cfg.Paths.LogFilePath == "" {
		fail("paths.log_file_path must not be empty")
		if !reportOnly {
			cfg.Paths.LogFilePath = defaults.Paths.LogFilePath
		}
	}
	if cfg.Paths.OutputDirectory == "" {
		fail("paths.output_directory must not be empty")
		if !reportOnly {
			cfg.Paths.OutputDirectory = defaults.Paths.OutputDirectory
		}
	}
	if cfg.Paths.StagingDirectory == "" {
		fail("paths.staging_directory must not be empty")
		if !reportOnly {
			cfg.Paths.StagingDirectory = defaults.Paths.StagingDirectory
		}
	}
	if cfg.Cookies.CookieFile == "" {
		fail("cookies.cookie_file must not be empty")
		if !reportOnly {
			cfg.Cookies.CookieFile = defaults.Cookies.CookieFile
		}
	}
	// 10080 minutes = 7 days. YouTube SAPISID-family cookies last on the
	// order of years, so a refresh cadence longer than a week stops being
	// "preventive" and starts to look like a typo. Catch values like
	// "1000000" (1.9 years) before they become a silent never-refresh.
	// Audit reports/config.md Finding 15.
	if cfg.Cookies.RefreshInterval.Value < 10 || cfg.Cookies.RefreshInterval.Value > 10080 {
		fail("cookies.refresh_interval %v out of range 10..10080 minutes", cfg.Cookies.RefreshInterval.Value)
		if !reportOnly {
			cfg.Cookies.RefreshInterval = defaults.Cookies.RefreshInterval
		}
	}

	// Disk thresholds
	if cfg.Disk.WarnPercent < 1 || cfg.Disk.WarnPercent > 99 {
		fail("disk.warn_percent %d out of range 1..99", cfg.Disk.WarnPercent)
		if !reportOnly {
			cfg.Disk.WarnPercent = defaults.Disk.WarnPercent
		}
	}
	if cfg.Disk.CriticalPercent < 1 || cfg.Disk.CriticalPercent > 99 {
		fail("disk.critical_percent %d out of range 1..99", cfg.Disk.CriticalPercent)
		if !reportOnly {
			cfg.Disk.CriticalPercent = defaults.Disk.CriticalPercent
		}
	}
	if cfg.Disk.CriticalPercent <= cfg.Disk.WarnPercent {
		fail("disk.critical_percent %d must be > disk.warn_percent %d", cfg.Disk.CriticalPercent, cfg.Disk.WarnPercent)
		if !reportOnly {
			// Clamp WarnPercent down by enough to fit a 5-point critical
			// buffer below 99. WarnPercent=99/CriticalPercent=99 used to
			// round-trip with persistent error because we'd pin
			// CriticalPercent to min(99+5, 99) = 99 — the same broken
			// pair. Reset to defaults if the cap doesn't fit.
			if cfg.Disk.WarnPercent >= 95 {
				cfg.Disk.WarnPercent = defaults.Disk.WarnPercent
				cfg.Disk.CriticalPercent = defaults.Disk.CriticalPercent
			} else {
				cfg.Disk.CriticalPercent = min(cfg.Disk.WarnPercent+5, 99)
			}
		}
	}

	// Channel-level quality_preference (applies to both YouTube and Twitch).
	for i := range cfg.Channels {
		if cfg.Channels[i].QualityPreference != "" && !validQualityPreferences[cfg.Channels[i].QualityPreference] {
			fail("channels[%d].quality_preference %q unknown", i, cfg.Channels[i].QualityPreference)
			if !reportOnly {
				cfg.Channels[i].QualityPreference = ""
			}
		}
	}

	return errs
}

// validQualityPreferences is the authoritative set of quality_preference
// strings accepted on a channel. Package-level so validate() doesn't
// reallocate it per call.
var validQualityPreferences = map[string]bool{
	"best":       true,
	"2160p60":    true,
	"2160p":      true,
	"1440p60":    true,
	"1440p":      true,
	"1080p60":    true,
	"1080p":      true,
	"900p60":     true,
	"900p":       true,
	"720p60":     true,
	"720p":       true,
	"480p":       true,
	"360p":       true,
	"160p":       true,
	"audio_only": true,
}

// Save writes the configuration to a TOML file at the given path.
func Save(cfg *MoomboxConfig, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	// Tighten the config dir's ACL to current-user-only on Windows.
	// Files written inside (moombox.toml, .tmp scratchpads) inherit
	// the restrictive ACL so the password hash + cookie paths +
	// auth tokens aren't readable by other non-admin users on a
	// shared host. No-op on non-Windows; idempotent if the dir
	// already has the right ACL. Memoised across Save calls because
	// icacls shell-out is ~30-80ms and otherwise blocks every config
	// save under Store.Update's write lock for no benefit. Failures
	// are logged-but-survived at the caller level — config save still
	// proceeds. Audit reports/config.md Finding 22.
	dacledDirsMu.Lock()
	if _, ok := dacledDirs[dir]; !ok {
		dacledDirsMu.Unlock()
		_ = utils.ApplyUserOnlyDACL(dir)
		dacledDirsMu.Lock()
		dacledDirs[dir] = struct{}{}
	}
	dacledDirsMu.Unlock()

	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	// DECISIONS #9: Save validates first and refuses bad input so the user
	// sees the problem. Load stays auto-normalising to avoid data loss on
	// legacy configs.
	if errs := Validate(cfg); len(errs) > 0 {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	Normalize(cfg)
	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to encode config: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync config file: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close config file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
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
//
// The negated character class allowlists:
//   - \w        word characters (ASCII letters, digits, underscore)
//   - \s        whitespace
//   - \-        literal hyphen
//   - U+3000..U+303F   CJK symbols and punctuation
//   - U+3040..U+309F   Hiragana
//   - U+30A0..U+30FF   Katakana
//   - U+FF00..U+FFEF   Halfwidth and Fullwidth Forms
//   - U+4E00..U+9FAF   CJK Unified Ideographs (BMP plane covering common
//                      Chinese, Japanese kanji, and Korean hanja)
//
// Anything outside these ranges (e.g. emoji, math symbols, Cyrillic,
// Greek, Hebrew, Arabic) gets stripped — the goal is "filename-safe
// across NTFS / ext4 / APFS while keeping East Asian content readable",
// not "preserve every script". Audit reports/config.md Finding 27.
var invalidFSChars = regexp.MustCompile(`[^\w\s\-\x{3000}-\x{303F}\x{3040}-\x{309F}\x{30A0}-\x{30FF}\x{FF00}-\x{FFEF}\x{4E00}-\x{9FAF}]`)

func sanitizeTemplateStr(s string) string {
	return strings.TrimSpace(invalidFSChars.ReplaceAllString(s, ""))
}

// ResolveTemplate resolves an output template with the given variables.
func ResolveTemplate(template string, vars TemplateVariables) string {
	safeTitle := sanitizeTemplateStr(vars.Title)
	safeChannel := sanitizeTemplateStr(vars.Channel)

	now := time.Now()
	if vars.Date != nil {
		if t, err := time.Parse(time.RFC3339, *vars.Date); err == nil {
			now = t
		}
	}

	return strings.NewReplacer(
		"${title}", safeTitle,
		"${id}", vars.ID,
		"${channel}", safeChannel,
		"${start_date}", now.Format("20060102"),
		"${start_time}", now.Format("1504"),
	).Replace(template)
}

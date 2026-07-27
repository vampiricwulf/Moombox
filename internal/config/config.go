package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// DefaultProbeTargets is the user-facing default for connectivity.probe_targets.
// Kept in sync with connectivity/probe.go's in-package fallback.
var DefaultProbeTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}

// dacledDirs memoises the set of dirs that have had their DACL tightened
// this process lifetime. icacls shells out (~30-80ms on Windows); without
// memoisation every config Save under Store.Update's write lock pays that
// latency on a no-op. The set lives for the process — restart re-applies.
var (
	dacledDirsMu sync.Mutex
	dacledDirs   = make(map[string]struct{})
)

// Defaults returns a new MoomboxConfig with all default values applied.
// boolPtr returns a pointer to b. Used for *bool config fields whose default is
// a concrete value (a feature that is on unless explicitly disabled), where a
// plain bool couldn't distinguish "absent" from "explicitly false".
func boolPtr(b bool) *bool { return &b }

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
			ArchiveWindowDays:   3,
			ArchiveSlots:        3,
			FeedCheckInterval:   FlexDuration{Value: 10}, // minutes
			HideFinishedAgeDays: FlexDuration{Value: 30},
			ProbeCooldown:       FlexDuration{Value: 0}, // seconds; 0 = disabled
			MembershipDiscovery: boolPtr(true),
		},
		Downloader: DownloaderConfig{
			OutputTemplate:       "${channel}/${start_date} ${title} [${id}]",
			MaxVideoResolution:   2160,
			NumParallelDownloads: 10,
			DownloadChat:         true,
			Prefer60fps:          true,
			MaximumTimeout:       600,
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
		Memory: MemoryConfig{
			// Defaults derived from observed steady-state usage in the
			// field (see docs/spec/operations.md "Memory limits"). Each
			// threshold sits above the relevant working set so GC only
			// fires on actual growth, not normal operation.
			GoSoftLimitMB:      256, // p99 active-download Sys ≈ 238 MB
			SidecarSoftLimitMB: 200, // post-mint plateau ≈ 150 MB
			SidecarHardLimitMB: 512, // BotGuard bursts ≈ 400-500 MB
		},
		Connectivity: ConnectivityConfig{
			ProbeTargets: DefaultProbeTargets,
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

	// Auto-populate sections introduced after this config file was
	// originally written. The user's config might be from an older
	// Moombox version with no [memory] block; without this, the fields
	// would be invisible in the TUI/Web UI's "current values" view until
	// the user manually saved (since the encoder writes the full struct,
	// any save would persist defaults). Mark the file as needing a
	// rewrite; the caller (services.go) issues the actual SaveLocked
	// after wiring up the configStore.
	if _, hasMemory := raw["memory"]; !hasMemory {
		cfg.NeedsAutoPersist = true
	}
	if _, hasBgutils := raw["bgutils"]; !hasBgutils {
		cfg.NeedsAutoPersist = true
	}
	if _, hasConnectivity := raw["connectivity"]; !hasConnectivity {
		cfg.NeedsAutoPersist = true
	}

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
	// connectivity.probe_targets: each entry must be host:port. Empty list or
	// any malformed entry falls back to the defaults (Normalize); Validate
	// reports each malformed entry.
	if len(cfg.Connectivity.ProbeTargets) == 0 {
		fail("connectivity.probe_targets is empty")
		if !reportOnly {
			cfg.Connectivity.ProbeTargets = defaults.Connectivity.ProbeTargets
		}
	} else {
		for _, t := range cfg.Connectivity.ProbeTargets {
			if _, _, err := net.SplitHostPort(t); err != nil {
				fail("connectivity.probe_targets entry %q must be host:port: %v", t, err)
				if !reportOnly {
					cfg.Connectivity.ProbeTargets = defaults.Connectivity.ProbeTargets
					break
				}
			}
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
	// values turn into per-cycle scan costs (ArchiveWindowDays sets the
	// FeedScope cutoff every WALK/ARCHIVE pass re-reads — O(rows in window)
	// per channel per cycle; ArchiveSlots only caps concurrent backlog
	// downloads), wall-clock latency (FeedCheckInterval > 24h means jobs
	// would never get found before they finish), and storage retention
	// shifts (HideFinishedAgeDays > 1y is a UI bug, not a feature). Audit
	// reports/config.md Finding 13.
	if cfg.Monitors.ArchiveWindowDays < 1 || cfg.Monitors.ArchiveWindowDays > 3650 {
		fail("monitors.archive_window_days %d out of range 1..3650", cfg.Monitors.ArchiveWindowDays)
		if !reportOnly {
			cfg.Monitors.ArchiveWindowDays = defaults.Monitors.ArchiveWindowDays
		}
	}
	if cfg.Monitors.ArchiveSlots < 1 || cfg.Monitors.ArchiveSlots > 100 {
		fail("monitors.archive_slots %d out of range 1..100", cfg.Monitors.ArchiveSlots)
		if !reportOnly {
			cfg.Monitors.ArchiveSlots = defaults.Monitors.ArchiveSlots
		}
	}
	// monitors.membership_discovery defaults to on: a nil pointer (the field is
	// absent from an existing config) normalizes to true so the feature is
	// enabled unless an operator explicitly sets false.
	if cfg.Monitors.MembershipDiscovery == nil && !reportOnly {
		cfg.Monitors.MembershipDiscovery = boolPtr(true)
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
	// probe_cooldown: 0 disables (probe every cycle); there is deliberately no
	// maximum — a large value simply re-probes rarely. Only a negative value is
	// invalid, and it means the same as 0, so normalize it back to the default.
	if cfg.Monitors.ProbeCooldown.Value < 0 {
		fail("monitors.probe_cooldown %v must be >= 0 seconds (0 disables)", cfg.Monitors.ProbeCooldown.Value)
		if !reportOnly {
			cfg.Monitors.ProbeCooldown = defaults.Monitors.ProbeCooldown
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
	if d.MaximumTimeout < 30 {
		fail("downloader.maximum_timeout %d must be at least 30 seconds", d.MaximumTimeout)
		if !reportOnly {
			d.MaximumTimeout = defaults.Downloader.MaximumTimeout
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

	// Memory limits: 0 disables each knob; negatives are meaningless and
	// would invert consumer comparisons (a negative sidecar soft limit makes
	// the RSS-exceeded check true on every tick, firing TriggerGC every
	// 2 minutes forever; a negative --max-old-space-size breaks the sidecar
	// launch). Clamp to 0 = disabled.
	if cfg.Memory.GoSoftLimitMB < 0 {
		fail("memory.go_soft_limit_mb %d must be >= 0", cfg.Memory.GoSoftLimitMB)
		if !reportOnly {
			cfg.Memory.GoSoftLimitMB = 0
		}
	}
	if cfg.Memory.SidecarSoftLimitMB < 0 {
		fail("memory.sidecar_soft_limit_mb %d must be >= 0", cfg.Memory.SidecarSoftLimitMB)
		if !reportOnly {
			cfg.Memory.SidecarSoftLimitMB = 0
		}
	}
	if cfg.Memory.SidecarHardLimitMB < 0 {
		fail("memory.sidecar_hard_limit_mb %d must be >= 0", cfg.Memory.SidecarHardLimitMB)
		if !reportOnly {
			cfg.Memory.SidecarHardLimitMB = 0
		}
	}
	// A hard limit at or below the soft limit means V8 OOM-aborts the
	// sidecar before (or exactly when) the proactive-GC threshold would
	// help — the pair is unsatisfiable, so reset both to defaults
	// (mirrors the disk warn/critical pattern above).
	if cfg.Memory.SidecarSoftLimitMB > 0 && cfg.Memory.SidecarHardLimitMB > 0 &&
		cfg.Memory.SidecarHardLimitMB <= cfg.Memory.SidecarSoftLimitMB {
		fail("memory.sidecar_hard_limit_mb %d must be > sidecar_soft_limit_mb %d",
			cfg.Memory.SidecarHardLimitMB, cfg.Memory.SidecarSoftLimitMB)
		if !reportOnly {
			cfg.Memory.SidecarSoftLimitMB = defaults.Memory.SidecarSoftLimitMB
			cfg.Memory.SidecarHardLimitMB = defaults.Memory.SidecarHardLimitMB
		}
	}

	// Channel-level quality_preference (applies to both YouTube and Twitch)
	// and archive_window_days/archive_slots overrides.
	for i := range cfg.Channels {
		ch := &cfg.Channels[i]
		if ch.QualityPreference != "" && !validQualityPreferences[ch.QualityPreference] {
			fail("channels[%d].quality_preference %q unknown", i, ch.QualityPreference)
			if !reportOnly {
				ch.QualityPreference = ""
			}
		}
		if ch.ArchiveWindowDays != nil && (*ch.ArchiveWindowDays < 1 || *ch.ArchiveWindowDays > 3650) {
			fail("channel %q archive_window_days %d out of range 1..3650", ch.Name, *ch.ArchiveWindowDays)
			if !reportOnly {
				ch.ArchiveWindowDays = nil // clear override → falls back to global
			}
		}
		if ch.ArchiveSlots != nil && (*ch.ArchiveSlots < 1 || *ch.ArchiveSlots > 100) {
			fail("channel %q archive_slots %d out of range 1..100", ch.Name, *ch.ArchiveSlots)
			if !reportOnly {
				ch.ArchiveSlots = nil
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
	// Tighten the config dir's ACL to current-user-only. Files written
	// inside (moombox.toml, .tmp scratchpads) inherit the restrictive
	// ACL so the password hash + cookie paths + auth tokens aren't
	// readable by other non-admin users on a shared host. icacls on
	// Windows, chmod 0700 on POSIX; idempotent if the dir
	// already has the right ACL. Memoised across Save calls because
	// icacls shell-out is ~30-80ms and otherwise blocks every config
	// save under Store.Update's write lock for no benefit. Failures
	// are logged-but-survived at the caller level — config save still
	// proceeds. Audit reports/config.md Finding 22.
	dacledDirsMu.Lock()
	_, alreadyApplied := dacledDirs[dir]
	if !alreadyApplied {
		// Insert the marker BEFORE releasing the lock + invoking
		// icacls. This matches the F13 pattern in cookies/autocookies.go
		// and ensures concurrent Save callers for the same dir cannot
		// both miss the cache and both shell out. Today's only caller
		// path is Store.Update under a write lock so the race cannot
		// fire, but this future-proofs against any caller that bypasses
		// the Store.
		dacledDirs[dir] = struct{}{}
	}
	dacledDirsMu.Unlock()
	if !alreadyApplied {
		// Log-but-survive on failure (the comment above promises
		// "logged-but-survived"). Demoted to Debug rather than Warn: the
		// common failure is ACCESS_DENIED (icacls exit 5) on a config dir
		// created under an elevated/admin context — a UAC filtered token
		// then can't rewrite that dir's DACL. On the single-user, run-it-
		// 24/7 host this targets, the dir isn't reachable by anyone else
		// anyway, so a startup WARN there is pure noise. A multi-user
		// operator who genuinely cares about the tightening can raise the
		// log level to Debug to see the miss.
		if daclErr := utils.ApplyUserOnlyDACL(dir); daclErr != nil {
			slog.Debug("could not restrict config dir to current user", "dir", dir, "err", daclErr)
		}
	}

	// Unique temp name (not a fixed path+".tmp"): callers are expected to
	// serialize on the store lock, but a per-writer temp file is cheap
	// defense-in-depth against two saves that race the lock ever interleaving
	// into one temp file and renaming spliced bytes over config.toml.
	tmpF, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}
	tmpPath := tmpF.Name()
	f := tmpF
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("chmod config temp: %w", err)
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
//     Chinese, Japanese kanji, and Korean hanja)
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

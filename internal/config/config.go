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
		Port:              774,
		NetworkAccess:     "localhost",
		LogLevel:          "INFO",
		LogFilePath:       "./moombox.log",
		LogMaxFileSize:    10485760, // 10MB
		LogMaxFiles:       5,
		DatabasePath:      "./moombox.db",
		MaxFeedItems:      15,
		FeedCheckInterval: FlexDuration{Value: 10}, // minutes
		Downloader: DownloaderConfig{
			OutputDirectory:         "./output",
			OutputTemplate:          "${channel}/${start_date} ${title} [${id}]",
			StagingDirectory:        "./staging",
			NumParallelDownloads:    2,
			MaxVideoResolution:      1080,
			CookieFile:              "./cookies.txt",
			DownloadChat:            true,
			Prefer60fps:             true,
			SegmentRetryDelayCap:    60,
			SegmentLiveCheckRetries: 16,
		},
		HideFinishedAgeDays: FlexDuration{Value: 30},
		AutoCookies: &AutoCookiesConfig{
			Enabled:           false,
			BrowserProfileDir: "./browser-profile",
			Platforms:         []string{},
			RefreshInterval:   FlexDuration{Value: 360}, // 6 hours in minutes
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

	// Migrate legacy fields not in the Go struct by decoding to a raw map.
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err == nil {
		// Legacy allow_lan/allow_external → network_access (matches TS config.ts)
		if cfg.NetworkAccess == "" || cfg.NetworkAccess == "localhost" {
			allowLan, hasLan := raw["allow_lan"].(bool)
			allowExt, hasExt := raw["allow_external"].(bool)
			if hasLan || hasExt {
				if allowLan && allowExt {
					cfg.NetworkAccess = "external"
				} else if allowLan {
					cfg.NetworkAccess = "lan"
				} else {
					cfg.NetworkAccess = "localhost"
				}
			}
		}

		// Legacy [tasklist] section → top-level hide_finished_age_days
		if tl, ok := raw["tasklist"].(map[string]any); ok {
			if _, hasTopLevel := raw["hide_finished_age_days"]; !hasTopLevel {
				if v, ok := tl["hide_finished_age_days"]; ok {
					cfg.HideFinishedAgeDays = ParseFlexDuration(v, "days", cfg.HideFinishedAgeDays.Value)
				}
			}
		}
	}

	cfg.ConfigLoaded = true
	validate(cfg)
	return cfg, nil
}

// validate checks config values and replaces invalid ones with defaults.
func validate(cfg *MoomboxConfig) {
	defaults := Defaults()

	if cfg.Port < 1 || cfg.Port > 65535 {
		cfg.Port = defaults.Port
	}
	if cfg.NetworkAccess != "localhost" && cfg.NetworkAccess != "lan" && cfg.NetworkAccess != "external" {
		cfg.NetworkAccess = defaults.NetworkAccess
	}
	switch strings.ToUpper(cfg.LogLevel) {
	case "DEBUG", "INFO", "WARN", "ERROR":
		cfg.LogLevel = strings.ToUpper(cfg.LogLevel)
	default:
		cfg.LogLevel = defaults.LogLevel
	}
	if cfg.MaxFeedItems < 1 {
		cfg.MaxFeedItems = defaults.MaxFeedItems
	}
	if cfg.FeedCheckInterval.Value < 1 {
		cfg.FeedCheckInterval = defaults.FeedCheckInterval
	}
	if cfg.DecapiCheckInterval != nil && (*cfg.DecapiCheckInterval < 15 || *cfg.DecapiCheckInterval > 3600) {
		cfg.DecapiCheckInterval = nil
	}
	if cfg.TwitchCheckInterval != nil && (*cfg.TwitchCheckInterval < 5 || *cfg.TwitchCheckInterval > 3600) {
		cfg.TwitchCheckInterval = nil
	}
	if cfg.LogMaxFileSize < 1 {
		cfg.LogMaxFileSize = defaults.LogMaxFileSize
	}
	if cfg.LogMaxFiles < 1 {
		cfg.LogMaxFiles = defaults.LogMaxFiles
	}

	d := &cfg.Downloader
	if d.NumParallelDownloads < 1 {
		d.NumParallelDownloads = defaults.Downloader.NumParallelDownloads
	}
	if d.MaxVideoResolution < 1 {
		d.MaxVideoResolution = defaults.Downloader.MaxVideoResolution
	}
	if d.OutputDirectory == "" {
		d.OutputDirectory = defaults.Downloader.OutputDirectory
	}
	if d.StagingDirectory == "" {
		d.StagingDirectory = defaults.Downloader.StagingDirectory
	}
	if d.OutputTemplate == "" {
		d.OutputTemplate = defaults.Downloader.OutputTemplate
	}
	if d.CookieFile == "" {
		d.CookieFile = defaults.Downloader.CookieFile
	}
	if d.SegmentRetryDelayCap < 1 {
		d.SegmentRetryDelayCap = defaults.Downloader.SegmentRetryDelayCap
	}
	if d.SegmentLiveCheckRetries < 1 {
		d.SegmentLiveCheckRetries = defaults.Downloader.SegmentLiveCheckRetries
	}

	// Validate channel-level quality_preference
	for i := range cfg.Channels {
		if cfg.Channels[i].QualityPreference != "" {
			switch cfg.Channels[i].QualityPreference {
			case "best", "720p", "480p", "audio_only":
				// valid
			default:
				cfg.Channels[i].QualityPreference = ""
			}
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

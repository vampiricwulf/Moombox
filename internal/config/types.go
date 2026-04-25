// Package config provides TOML-based configuration management for Moombox.
package config

// MoomboxConfig is the complete application configuration.
type MoomboxConfig struct {
	Network       NetworkConfig        `toml:"network" json:"network"`
	Paths         PathsConfig          `toml:"paths" json:"paths"`
	Logs          LogsConfig           `toml:"logs" json:"logs"`
	Monitors      MonitorsConfig       `toml:"monitors" json:"monitors"`
	Downloader    DownloaderConfig     `toml:"downloader" json:"downloader"`
	Cookies       CookiesConfig        `toml:"cookies" json:"cookies"`
	Disk          DiskConfig           `toml:"disk" json:"disk"`
	Updates       UpdatesConfig        `toml:"updates" json:"updates"`
	Channels      []ChannelConfig      `toml:"channels,omitempty" json:"channels,omitempty"`
	Notifications []NotificationConfig `toml:"notifications,omitempty" json:"notifications,omitempty"`

	// ConfigLoaded is true when the config was read from an existing file on
	// disk (vs falling back to defaults). Not serialized.
	ConfigLoaded bool `toml:"-" json:"-"`
}

// NetworkConfig holds server and network access settings.
type NetworkConfig struct {
	Port               int    `toml:"port" json:"port"`
	NetworkAccess      string `toml:"network_access" json:"network_access"`
	HTTPSEnabled       bool   `toml:"https_enabled" json:"https_enabled"`
	TLSCertPath        string `toml:"tls_cert_path,omitempty" json:"tls_cert_path,omitempty"`
	TLSKeyPath         string `toml:"tls_key_path,omitempty" json:"tls_key_path,omitempty"`
	PasswordHash       string `toml:"password_hash,omitempty" json:"-"`
	ClientTokenTTLDays int    `toml:"client_token_ttl_days,omitempty" json:"client_token_ttl_days,omitempty"`
}

// PathsConfig holds file and directory path settings.
type PathsConfig struct {
	DatabasePath     string `toml:"database_path" json:"database_path"`
	LogFilePath      string `toml:"log_file_path" json:"log_file_path"`
	OutputDirectory  string `toml:"output_directory" json:"output_directory"`
	StagingDirectory string `toml:"staging_directory" json:"staging_directory"`
	FfmpegPath       string `toml:"ffmpeg_path,omitempty" json:"ffmpeg_path,omitempty"`
}

// EffectiveStagingDir returns the staging directory, falling back to "./staging".
func (p PathsConfig) EffectiveStagingDir() string {
	if p.StagingDirectory != "" {
		return p.StagingDirectory
	}
	return "./staging"
}

// LogsConfig holds logging settings.
type LogsConfig struct {
	LogLevel       string `toml:"log_level" json:"log_level"`
	LogMaxFileSize int    `toml:"log_max_file_size" json:"log_max_file_size"`
	LogMaxFiles    int    `toml:"log_max_files" json:"log_max_files"`
}

// MonitorsConfig holds feed and monitor check settings.
type MonitorsConfig struct {
	MaxFeedItems        int          `toml:"max_feed_items" json:"max_feed_items"`
	FeedCheckInterval   FlexDuration `toml:"feed_check_interval" json:"feed_check_interval"`
	DecapiCheckInterval *int         `toml:"decapi_check_interval,omitempty" json:"decapi_check_interval,omitempty"`
	TwitchCheckInterval *int         `toml:"twitch_check_interval,omitempty" json:"twitch_check_interval,omitempty"`
	HideFinishedAgeDays FlexDuration `toml:"hide_finished_age_days" json:"hide_finished_age_days"`
}

// DownloaderConfig holds download-related settings.
type DownloaderConfig struct {
	OutputTemplate          string `toml:"output_template" json:"output_template"`
	MaxVideoResolution      int    `toml:"max_video_resolution" json:"max_video_resolution"`
	NumParallelDownloads    int    `toml:"num_parallel_downloads" json:"num_parallel_downloads"`
	DownloadChat            bool   `toml:"download_chat" json:"download_chat"`
	Prefer60fps             bool   `toml:"prefer_60fps" json:"prefer_60fps"`
	SegmentRetryDelayCap    int    `toml:"segment_retry_delay_cap" json:"segment_retry_delay_cap"`
	SegmentLiveCheckRetries int    `toml:"segment_live_check_retries" json:"segment_live_check_retries"`
	// PoToken and VisitorData are session-scoped secrets. Like PasswordHash,
	// they must never be returned by GET /api/config — use json:"-" to hide
	// them from any encoder walking the Config struct. Operators who need to
	// inspect them can read config.toml directly.
	PoToken        string `toml:"po_token,omitempty" json:"-"`
	VisitorData    string `toml:"visitor_data,omitempty" json:"-"`
	PotProviderURL string `toml:"pot_provider_url,omitempty" json:"pot_provider_url,omitempty"`
}

// CookiesConfig holds cookie file and auto-cookie acquisition settings.
type CookiesConfig struct {
	CookieFile        string       `toml:"cookie_file" json:"cookie_file"`
	AutoEnabled       bool         `toml:"auto_enabled" json:"auto_enabled"`
	BrowserProfileDir string       `toml:"browser_profile_dir,omitempty" json:"browser_profile_dir,omitempty"`
	Platforms         []string     `toml:"platforms,omitempty" json:"platforms,omitempty"`
	ActivePlatforms   []string     `toml:"active_platforms,omitempty" json:"active_platforms,omitempty"`
	RefreshInterval   FlexDuration `toml:"refresh_interval" json:"refresh_interval"`

	// DpapiFallback enables a Windows-only fallback path: if the
	// CDP-based refresh launch fails (e.g. the user has Chrome open
	// and our managed-profile launch can't acquire whatever resource
	// it needs), Moombox tries to read cookies directly from the
	// user's REAL Chromium-family browser profile via the Windows
	// CryptUnprotectData API. Default off — the fallback reads the
	// user's actual signed-in cookies, so it's an opt-in privacy
	// surface that the user has to consciously enable. DECISIONS #6.
	DpapiFallback bool `toml:"dpapi_fallback,omitempty" json:"dpapi_fallback,omitempty"`
}

// DiskConfig holds disk space monitoring settings.
type DiskConfig struct {
	WarnPercent     int `toml:"disk_warn_percent" json:"disk_warn_percent"`
	CriticalPercent int `toml:"disk_critical_percent" json:"disk_critical_percent"`
}

// UpdatesConfig holds auto-update settings.
type UpdatesConfig struct {
	AutoCheckUpdates bool `toml:"auto_check_updates" json:"auto_check_updates"`
}

// ChannelConfig holds channel-specific monitoring settings.
type ChannelConfig struct {
	ID                    string       `toml:"id" json:"id"`
	Name                  string       `toml:"name,omitempty" json:"name,omitempty"`
	Platform              string       `toml:"platform,omitempty" json:"platform,omitempty"`
	Enabled               *bool        `toml:"enabled,omitempty" json:"enabled,omitempty"`
	Terms                 ChannelTerms `toml:"terms,omitempty" json:"terms"`
	NumDescLookbehind     *int         `toml:"num_desc_lookbehind,omitempty" json:"num_desc_lookbehind,omitempty"`
	OutputDirectory       string       `toml:"output_directory,omitempty" json:"output_directory,omitempty"`
	IncludeNonLiveContent bool         `toml:"include_non_live_content,omitempty" json:"include_non_live_content,omitempty"`
	MaxFeedItems          *int         `toml:"max_feed_items,omitempty" json:"max_feed_items,omitempty"`
	QualityPreference     string       `toml:"quality_preference,omitempty" json:"quality_preference,omitempty"`
}

// IsEnabled returns whether the channel is enabled (defaults to true).
func (c *ChannelConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// GetPlatform returns the channel platform (defaults to "youtube").
func (c *ChannelConfig) GetPlatform() string {
	if c.Platform == "" {
		return "youtube"
	}
	return c.Platform
}

// NotificationConfig holds notification endpoint configuration.
type NotificationConfig struct {
	URL    string   `toml:"url,omitempty" json:"url,omitempty"`
	Tags   []string `toml:"tags,omitempty" json:"tags,omitempty"`
	Events []string `toml:"events,omitempty" json:"events,omitempty"`
}

// TemplateVariables holds template variables for output filenames.
type TemplateVariables struct {
	Title   string
	ID      string
	Channel string
	Date    *string // ISO date, optional
}

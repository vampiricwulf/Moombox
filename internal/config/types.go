// Package config provides TOML-based configuration management for Moombox.
package config

// MoomboxConfig is the complete application configuration.
type MoomboxConfig struct {
	Port                int                  `toml:"port" json:"port"`
	NetworkAccess       string               `toml:"network_access" json:"network_access"`
	PasswordHash        string               `toml:"password_hash,omitempty" json:"-"`
	LogLevel            string               `toml:"log_level" json:"log_level"`
	LogFilePath         string               `toml:"log_file_path" json:"log_file_path"`
	LogMaxFileSize      int                  `toml:"log_max_file_size" json:"log_max_file_size"`
	LogMaxFiles         int                  `toml:"log_max_files" json:"log_max_files"`
	DatabasePath        string               `toml:"database_path" json:"database_path"`
	MaxFeedItems        int                  `toml:"max_feed_items" json:"max_feed_items"`
	FeedCheckInterval   FlexDuration         `toml:"feed_check_interval" json:"feed_check_interval"`
	DecapiCheckInterval *int                 `toml:"decapi_check_interval,omitempty" json:"decapi_check_interval,omitempty"`
	TwitchCheckInterval *int                 `toml:"twitch_check_interval,omitempty" json:"twitch_check_interval,omitempty"`
	Downloader          DownloaderConfig     `toml:"downloader" json:"downloader"`
	Tasklist            *TasklistConfig      `toml:"tasklist,omitempty" json:"tasklist,omitempty"`
	AutoCookies         *AutoCookiesConfig   `toml:"auto_cookies,omitempty" json:"auto_cookies,omitempty"`
	Notifications       []NotificationConfig `toml:"notifications,omitempty" json:"notifications,omitempty"`
	Channels            []ChannelConfig      `toml:"channels,omitempty" json:"channels,omitempty"`

	// ConfigLoaded is true when the config was read from an existing file on
	// disk (vs falling back to defaults). Not serialized.
	ConfigLoaded bool `toml:"-" json:"-"`
}

// DownloaderConfig holds download-related settings.
type DownloaderConfig struct {
	MaxVideoResolution      int    `toml:"max_video_resolution" json:"max_video_resolution"`
	FfmpegPath              string `toml:"ffmpeg_path,omitempty" json:"ffmpeg_path,omitempty"`
	StagingDirectory        string `toml:"staging_directory" json:"staging_directory"`
	OutputDirectory         string `toml:"output_directory" json:"output_directory"`
	OutputTemplate          string `toml:"output_template" json:"output_template"`
	NumParallelDownloads    int    `toml:"num_parallel_downloads" json:"num_parallel_downloads"`
	PoToken                 string `toml:"po_token,omitempty" json:"po_token,omitempty"`
	VisitorData             string `toml:"visitor_data,omitempty" json:"visitor_data,omitempty"`
	CookieFile              string `toml:"cookie_file" json:"cookie_file"`
	PotProviderURL          string `toml:"pot_provider_url,omitempty" json:"pot_provider_url,omitempty"`
	DownloadChat            bool   `toml:"download_chat" json:"download_chat"`
	Prefer60fps             bool   `toml:"prefer_60fps" json:"prefer_60fps"`
	SegmentRetryDelayCap    int    `toml:"segment_retry_delay_cap" json:"segment_retry_delay_cap"`
	SegmentLiveCheckRetries int    `toml:"segment_live_check_retries" json:"segment_live_check_retries"`
}

// ChannelConfig holds channel-specific monitoring settings.
type ChannelConfig struct {
	ID                    string         `toml:"id" json:"id"`
	Name                  string         `toml:"name,omitempty" json:"name,omitempty"`
	Platform              string         `toml:"platform,omitempty" json:"platform,omitempty"`
	Enabled               *bool          `toml:"enabled,omitempty" json:"enabled,omitempty"`
	Terms                 ChannelTerms   `toml:"terms,omitempty" json:"terms,omitempty"`
	NumDescLookbehind     *int           `toml:"num_desc_lookbehind,omitempty" json:"num_desc_lookbehind,omitempty"`
	OutputDirectory       string         `toml:"output_directory,omitempty" json:"output_directory,omitempty"`
	IncludeNonLiveContent bool           `toml:"include_non_live_content,omitempty" json:"include_non_live_content,omitempty"`
	MaxFeedItems          *int           `toml:"max_feed_items,omitempty" json:"max_feed_items,omitempty"`
	QualityPreference     string         `toml:"quality_preference,omitempty" json:"quality_preference,omitempty"`
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

// TasklistConfig holds task list display settings.
type TasklistConfig struct {
	HideFinishedAgeDays FlexDuration `toml:"hide_finished_age_days,omitempty" json:"hide_finished_age_days,omitempty"`
}

// NotificationConfig holds notification endpoint configuration.
type NotificationConfig struct {
	URL    string   `toml:"url,omitempty" json:"url,omitempty"`
	Tags   []string `toml:"tags,omitempty" json:"tags,omitempty"`
	Events []string `toml:"events,omitempty" json:"events,omitempty"`
}

// AutoCookiesConfig holds automatic cookie acquisition settings.
type AutoCookiesConfig struct {
	Enabled           bool         `toml:"enabled" json:"enabled"`
	BrowserProfileDir string       `toml:"browser_profile_dir,omitempty" json:"browser_profile_dir,omitempty"`
	Platforms         []string     `toml:"platforms,omitempty" json:"platforms,omitempty"`
	RefreshInterval   FlexDuration `toml:"refresh_interval" json:"refresh_interval"`
}

// TemplateVariables holds template variables for output filenames.
type TemplateVariables struct {
	Title   string
	ID      string
	Channel string
	Date    *string // ISO date, optional
}

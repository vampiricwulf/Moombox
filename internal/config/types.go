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
	Bgutils       BgutilsConfig        `toml:"bgutils" json:"bgutils"`
	Memory        MemoryConfig         `toml:"memory" json:"memory"`
	Connectivity  ConnectivityConfig   `toml:"connectivity" json:"connectivity"`
	Channels      []ChannelConfig      `toml:"channels,omitempty" json:"channels,omitempty"`
	Notifications []NotificationConfig `toml:"notifications,omitempty" json:"notifications,omitempty"`

	// ConfigLoaded is true when the config was read from an existing file on
	// disk (vs falling back to defaults). Not serialized.
	ConfigLoaded bool `toml:"-" json:"-"`
	// NeedsAutoPersist signals that loadFromFile detected one or more
	// new top-level sections missing from the user's TOML (e.g., [memory]
	// added in 2.6.21). The struct is already populated with defaults
	// for those sections; the caller can SaveLocked() after store wiring
	// to flush them to disk so they show up in the user's config.toml.
	// Excluded from TOML and JSON marshalling (in-memory signal only).
	NeedsAutoPersist bool `toml:"-" json:"-"`
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

	// TrustForwardedProto enables setting the Secure cookie flag based
	// on the X-Forwarded-Proto header instead of solely on r.TLS.
	// Default false: a directly-exposed Moombox MUST NOT trust this
	// header because clients can spoof it and then session cookies
	// would travel in plaintext over HTTP.
	//
	// Set to true ONLY when Moombox sits behind a reverse proxy that
	// terminates TLS (nginx, Caddy, Traefik) AND the proxy strips any
	// client-supplied X-Forwarded-Proto before adding its own. Without
	// this flag, reverse-proxy deployments end up with non-Secure
	// session cookies because Moombox sees plaintext HTTP from the
	// proxy.
	TrustForwardedProto bool `toml:"trust_forwarded_proto,omitempty" json:"trust_forwarded_proto,omitempty"`
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
	CookieFile        string `toml:"cookie_file" json:"cookie_file"`
	AutoEnabled       bool   `toml:"auto_enabled" json:"auto_enabled"`
	BrowserProfileDir string `toml:"browser_profile_dir,omitempty" json:"browser_profile_dir,omitempty"`

	// BrowserPath overrides the auto-detected browser. When empty
	// (default), the auto-cookies service uses DetectBrowser to pick
	// the best available browser. When set, the auto-cookies service
	// uses this exact executable path and BrowserType to drive
	// extraction. Setting BrowserPath without BrowserType is a config
	// error caught at validation.
	BrowserPath string `toml:"browser_path,omitempty" json:"browser_path,omitempty"`

	// BrowserType identifies which extraction path applies to BrowserPath
	// (firefox/chrome/brave/edge/etc.). Required when BrowserPath is set
	// because the path alone doesn't tell us which extraction backend
	// (Firefox cookies.sqlite vs Chromium CDP) to use. Validated against
	// the same identifier list used by DetectBrowser.
	BrowserType string `toml:"browser_type,omitempty" json:"browser_type,omitempty"`

	// Platforms is the auto-detected platform list — populated at
	// startup from cookie file inspection (HasYouTubeAuthCookies /
	// HasTwitchAuthCookies). Treat as read-only-from-config.
	Platforms []string `toml:"platforms,omitempty" json:"platforms,omitempty"`

	// ActivePlatforms is the user's explicit override. Takes precedence
	// over Platforms when set. Read via GetActivePlatforms() which
	// falls back to Platforms then to channel inference.
	ActivePlatforms []string `toml:"active_platforms,omitempty" json:"active_platforms,omitempty"`

	RefreshInterval FlexDuration `toml:"refresh_interval" json:"refresh_interval"`

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
	// LastRunVersion records the binary version of the previous run so
	// startup can detect a version change (self-update OR manual binary
	// swap) and emit the update_applied notification. Written by the
	// daemon on boot; not a user knob. Additive — absent in older
	// configs decodes to "" and just skips the first comparison.
	LastRunVersion string `toml:"last_run_version,omitempty" json:"last_run_version,omitempty"`
}

// MemoryConfig holds memory-management knobs. Defaults applied via
// config.Defaults().
//
// The Go runtime gets a soft memory limit (debug.SetMemoryLimit). Go GC
// runs more aggressively as the heap approaches GoSoftLimitMB; if a real
// allocation can't be served within the limit, Go allocates beyond it
// rather than OOM-aborting — this is "soft" by design.
//
// V8 has no soft-limit primitive, so the sidecar gets a hard ceiling
// (--max-old-space-size = SidecarHardLimitMB) plus manual GC triggers
// fired from Moombox when the sidecar's RSS exceeds SidecarSoftLimitMB.
// Set SidecarHardLimitMB high enough above SidecarSoftLimitMB that a
// transient allocation spike doesn't OOM-abort the sidecar.
type MemoryConfig struct {
	// GoSoftLimitMB is the soft memory limit for the Moombox process.
	// 0 disables (Go uses its default unbounded behaviour). Default 256.
	GoSoftLimitMB int `toml:"go_soft_limit_mb" json:"go_soft_limit_mb"`
	// SidecarSoftLimitMB is the RSS threshold at which Moombox tells the
	// sidecar to run global.gc(). 0 disables proactive triggering.
	// Default 200.
	SidecarSoftLimitMB int `toml:"sidecar_soft_limit_mb" json:"sidecar_soft_limit_mb"`
	// SidecarHardLimitMB is V8's --max-old-space-size for the sidecar.
	// Hitting this DOES OOM-abort the sidecar (V8 has no graceful soft
	// stop). Should be comfortably above SidecarSoftLimitMB.
	// 0 disables (V8 uses its default ~512-1500 MB depending on host).
	// Default 512.
	SidecarHardLimitMB int `toml:"sidecar_hard_limit_mb" json:"sidecar_hard_limit_mb"`
}

// ConnectivityConfig holds internet-reachability probe settings.
type ConnectivityConfig struct {
	// ProbeTargets are host:port endpoints the connectivity monitor TCP-dials
	// to verify real internet reachability (first success wins). Defaults to
	// public anycast resolvers on :443. Override if your network blocks
	// outbound connections to them.
	ProbeTargets []string `toml:"probe_targets" json:"probe_targets"`
}

// BgutilsConfig holds BotGuard sidecar settings. Defaults applied via
// config.Defaults() so a config file with no [bgutils] section gets a
// working sidecar enabled out of the box.
type BgutilsConfig struct {
	// UseSidecar enables the embedded Node + JSDOM + bgutils-js
	// subprocess that produces real PO tokens via BotGuard. Defaults to
	// true on Windows. When false (or when the sidecar fails to start),
	// PotProvider falls back to the goja-only path which produces only
	// websafe-fallback tokens.
	UseSidecar bool `toml:"use_sidecar" json:"use_sidecar"`
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
// (A legacy `tags` field was removed 2026-07: it was persisted and
// round-tripped but read by nothing — leftover keys in existing TOML files
// are ignored harmlessly by the decoder.)
type NotificationConfig struct {
	URL    string   `toml:"url,omitempty" json:"url,omitempty"`
	Events []string `toml:"events,omitempty" json:"events,omitempty"`
}

// TemplateVariables holds template variables for output filenames.
type TemplateVariables struct {
	Title   string
	ID      string
	Channel string
	Date    *string // ISO date, optional
}

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

	// TrustedProxies lists reverse-proxy source addresses (bare IPs or
	// CIDRs, e.g. "172.18.0.2" or "10.0.0.0/8") whose X-Forwarded-For
	// header is honored when resolving the client IP for trust decisions
	// (network_access gate, auth skip, rate limiting).
	//
	// Default empty: X-Forwarded-For is NEVER trusted, exactly as before
	// this setting existed. Without it, ANY reverse proxy in front of
	// Moombox makes all forwarded traffic — including internet traffic —
	// appear to come from the proxy's private address, which passes the
	// "lan" gate and skips authentication entirely.
	//
	// Loopback-gated endpoints (setup wizard, open-folder, POT provider,
	// first-time password setup) intentionally ignore this setting and
	// keep requiring a direct loopback connection.
	TrustedProxies []string `toml:"trusted_proxies,omitempty" json:"trusted_proxies,omitempty"`
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
	// ArchiveWindowDays is how many days back to archive: upcoming/live
	// content is always covered regardless of this window (see
	// ChannelConfig.ArchiveWindowDays for the per-channel override).
	ArchiveWindowDays int `toml:"archive_window_days" json:"archive_window_days"`
	// ArchiveSlots caps how many backlog (non-live) downloads a channel can
	// have running at once, so new live/upcoming content never waits behind
	// a backlog sweep (see ChannelConfig.ArchiveSlots for the per-channel
	// override).
	ArchiveSlots        int          `toml:"archive_slots" json:"archive_slots"`
	FeedCheckInterval   FlexDuration `toml:"feed_check_interval" json:"feed_check_interval"`
	DecapiCheckInterval *int         `toml:"decapi_check_interval,omitempty" json:"decapi_check_interval,omitempty"`
	TwitchCheckInterval *int         `toml:"twitch_check_interval,omitempty" json:"twitch_check_interval,omitempty"`
	HideFinishedAgeDays FlexDuration `toml:"hide_finished_age_days" json:"hide_finished_age_days"`
	// ProbeCooldown (seconds) is the minimum interval between re-probes of the
	// SAME video's metadata through the YouTube player API. Each feed/DECAPI
	// cycle re-lists the same upcoming/unfinished videos, so without a cooldown
	// every cycle re-probes every listed video. 0 disables the cooldown (every
	// cycle re-probes every listed video — the poll interval is then the only
	// throttle). No maximum. Hot-reloaded each monitor cycle.
	ProbeCooldown FlexDuration `toml:"probe_cooldown" json:"probe_cooldown"`
	// MembershipDiscovery enables per-cycle discovery of members-only videos via
	// each YouTube channel's authenticated /membership tab. RSS and DECAPI never
	// list members-only content, so this is the only source for members-only
	// live/upcoming streams (and, when include_non_live_content is set, their
	// VODs/premieres). Requires YouTube auth cookies to do anything. Defaults to
	// enabled: a nil pointer (absent from an existing config) is normalized to
	// true on load, so only an explicit false disables it.
	MembershipDiscovery *bool `toml:"membership_discovery,omitempty" json:"membership_discovery,omitempty"`
}

// MembershipDiscoveryEnabled reports whether members-only /membership discovery
// is on. The *bool defaults to ON, so a nil pointer (field absent from an
// existing config, i.e. before normalization) counts as enabled. Centralizes
// the "nil means on" semantic so callers never dereference the pointer unguarded.
func (m MonitorsConfig) MembershipDiscoveryEnabled() bool {
	return m.MembershipDiscovery == nil || *m.MembershipDiscovery
}

// DownloaderConfig holds download-related settings.
type DownloaderConfig struct {
	OutputTemplate       string `toml:"output_template" json:"output_template"`
	MaxVideoResolution   int    `toml:"max_video_resolution" json:"max_video_resolution"`
	NumParallelDownloads int    `toml:"num_parallel_downloads" json:"num_parallel_downloads"`
	// SegmentWorkers is how many segments are fetched CONCURRENTLY within a
	// single download. Distinct from NumParallelDownloads, which gates how
	// many VOD jobs download at once and which live broadcasts bypass
	// entirely — a distinction that cost real debugging time on 2026-08-15,
	// when a value of 1000 there had no effect on a live stream's catch-up.
	//
	// Higher values catch up faster on an in-progress stream (measured: six
	// connections sustained 11.3 MB/s where Moombox managed 5.96 MB/s), at
	// the cost of a wider fan-out to YouTube. There is deliberately no upper
	// limit; past SegmentWorkersWarnThreshold a warning is logged because a
	// large simultaneous fan-out is the kind of traffic shape that attracts
	// bot detection.
	SegmentWorkers int  `toml:"segment_workers" json:"segment_workers"`
	DownloadChat   bool `toml:"download_chat" json:"download_chat"`
	Prefer60fps    bool `toml:"prefer_60fps" json:"prefer_60fps"`
	// MaximumTimeout (seconds, YouTube livestreams only) is how long to keep
	// retrying — checking every 30s whether the stream has ended — before
	// force-finalizing the recording, even if YouTube still reports the stream
	// live. Resets whenever a segment arrives.
	MaximumTimeout int `toml:"maximum_timeout" json:"maximum_timeout"`
	// InterruptionTimeout (minutes) is how long finalize may stall waiting
	// for an interrupted broadcast to resume before giving up and finalizing
	// with what was captured so far. 0 disables the stall — finalize never
	// waits (Tier 2 preservation of the interrupted-but-not-yet-resumed
	// recording still applies regardless of this setting).
	InterruptionTimeout FlexDuration `toml:"interruption_timeout" json:"interruption_timeout"`
	// IncompleteStagingExpiryDays (days) is how long a Finished job flagged
	// incomplete_tail keeps its staging directory shielded from orphan
	// cleanup. The FLAG (the honest "may be missing its tail" badge) never
	// expires — only the disk-heavy staging preservation does: YouTube
	// cannot resume a broadcast days later, so week-old interruption staging
	// has zero resume value and would otherwise park gigabytes forever
	// (owner ruling 2026-08-21). After expiry the staging becomes an
	// ordinary orphan-scanner candidate; auto-resume's staging-existence
	// gate then falls back to the silent drop and manual Reinitialize
	// remains the recovery. 0 = never expire (preserve indefinitely).
	IncompleteStagingExpiryDays FlexDuration `toml:"incomplete_staging_expiry_days" json:"incomplete_staging_expiry_days"`
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

	// Platforms is the platform list SEEDED on first run by
	// detectCookiePlatforms (cmd/moombox/services.go), and only when BOTH
	// this list and ActivePlatforms are empty. The sidecar's recorded
	// meta.Platforms wins outright when it has one — a real verification
	// result, never unioned with a guess; only in its absence does the
	// seed fall back to the LOOSE HasAnyYouTubeAuthCookie /
	// HasAnyTwitchAuthCookie predicates, not the strict
	// HasYouTubeAuthCookies / HasTwitchAuthCookies pair, because a jar
	// holding SAPISID with LOGIN_INFO already cleared is a CONFIGURED
	// platform with broken credentials, not an unconfigured one. Nothing
	// automatic ever prunes it — the automatic writers only add — and the
	// sole removal path is an operator replacing the list through
	// PUT /api/config. Treat as read-only-from-config.
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

	// Acquisition selects HOW a cookie REFRESH acquires credentials.
	//
	//   "auto"    — the default, and exactly the behaviour that shipped before
	//               this field existed: a resolvable browser takes the headless
	//               launch path, a host with none imports the profile read-only.
	//   "profile" — never launch a browser for a REFRESH. Read
	//               browser_profile_dir read-only even on a desktop that has a
	//               browser installed. The only route to browserless import
	//               from a real signed-in profile on Windows, and the opt-in
	//               that lifts the launch-path profile-dir guard on the two
	//               read-only sites (audit G3).
	//
	// Two values by ruling (2026-09-02). The audit's "browser" was
	// observationally identical to "auto" at every site and was dropped: a
	// value that behaves like another is a trap. A later semantics can add it.
	//
	// Absent or empty means "auto", and there is NO migration: Load decodes the
	// file over Defaults(), so a config written before this key already carries
	// it. An unrecognised value is reported by Validate, replaced by Normalize.
	//
	// COMPOSES with AutoEnabled rather than replacing it — that flag still
	// decides whether a pass may launch at all, and whether the periodic timer
	// exists; under "profile" that timer and the automatic recovery attempt
	// import instead of launching, and the timer's import stays behind
	// automaticImportGuard. Read LIVE through AutoCookieService.AcquisitionMode,
	// so this is NOT restart-required.
	Acquisition string `toml:"acquisition" json:"acquisition"`
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
	// LastFailureMarkerSeen dedupes the boot-time failed-update-marker
	// notification (name|mtime of the last marker announced), so
	// config-change restarts don't re-ping about the same failure.
	// Written by the daemon; not a user knob. Additive.
	LastFailureMarkerSeen string `toml:"last_failure_marker_seen,omitempty" json:"last_failure_marker_seen,omitempty"`
	// SkippedVersion is the release tag the operator chose to skip via the
	// update dialog's "Skip this version" — that release stays out of
	// notifications/broadcasts; the next different tag notifies normally.
	// Written via the dismiss route; additive.
	SkippedVersion string `toml:"skipped_version,omitempty" json:"skipped_version,omitempty"`
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
	// ArchiveWindowDays/ArchiveSlots override the global monitors settings
	// for this channel. Nil or <= 0 falls back to the global value.
	ArchiveWindowDays *int   `toml:"archive_window_days,omitempty" json:"archive_window_days,omitempty"`
	ArchiveSlots      *int   `toml:"archive_slots,omitempty" json:"archive_slots,omitempty"`
	QualityPreference string `toml:"quality_preference,omitempty" json:"quality_preference,omitempty"`
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

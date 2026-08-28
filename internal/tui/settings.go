package tui

import (
	"fmt"
	"image/color"
	"maps"
	"math"
	"net"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// fieldType identifies how a settings field is edited.
type fieldType int

const (
	fieldText fieldType = iota
	fieldNumber
	fieldToggle
	fieldCycle
)

// fieldDef defines a single editable settings field.
type fieldDef struct {
	key       string
	label     string
	ftype     fieldType
	options   []string // for cycle fields
	help      string
	previewFn func(value string) string
}

// settingsSection groups fields under a heading.
type settingsSection struct {
	name   string
	fields []fieldDef
}

// Keys that require a restart when changed.
//
// The three cookie keys are not cosmetic entries. AutoCookieService is built
// once, at startup, from cookie_file and browser_profile_dir, and run() decides
// there and then whether the periodic refresh loop starts at all — it reads
// auto_enabled and os.Stat's the profile directory before any of this UI
// exists. So an operator acting on a "your cookies are dead" notification can
// turn the setting on, watch it save, and have nothing whatever happen until
// they restart. Keep them here until those three reads are genuinely live.
//
// Kept in step with RESTART_REQUIRED_FIELDS in web/public/modules/settings.js;
// the two lists are pinned against each other by TestRestartRequiredListsAgree.
var restartRequiredKeys = map[string]bool{
	"port":                true,
	"network_access":      true,
	"https_enabled":       true,
	"tls_cert_path":       true,
	"tls_key_path":        true,
	"database_path":       true,
	"log_file_path":       true,
	"log_max_file_size":   true,
	"log_max_files":       true,
	"cookie_file":         true,
	"auto_enabled":        true,
	"browser_profile_dir": true,
}

var sections = []settingsSection{
	{
		name: "Network",
		fields: []fieldDef{
			{"port", "Port", fieldNumber, nil, "web dashboard port, 1-65535 (requires restart)", nil},
			{"network_access", "Network access", fieldCycle, []string{"localhost", "lan", "external"}, "who can reach the dashboard (requires restart)", nil},
			{"https_enabled", "HTTPS enabled", fieldToggle, nil, "serve over HTTPS, needs TLS cert + key (requires restart)", nil},
			{"tls_cert_path", "TLS cert path", fieldText, nil, "PEM format certificate file (requires restart)", nil},
			{"tls_key_path", "TLS key path", fieldText, nil, "PEM format private key file (requires restart)", nil},
			{"trust_forwarded_proto", "Trust forwarded proto", fieldToggle, nil, "ONLY enable behind a TLS-terminating reverse proxy that strips client X-Forwarded-Proto", nil},
			{"trusted_proxies", "Trusted proxies", fieldText, nil, "comma-separated reverse-proxy IPs/CIDRs whose X-Forwarded-For is honored — leave empty unless behind a proxy you control", nil},
		},
	},
	{
		name: "Paths",
		fields: []fieldDef{
			{"database_path", "Database path", fieldText, nil, "SQLite database file (requires restart)", nil},
			{"log_file_path", "Log file path", fieldText, nil, "log output file (requires restart)", nil},
			{"output_directory", "Output directory", fieldText, nil, "where finished files go", nil},
			{"staging_directory", "Staging directory", fieldText, nil, "temp files during download", nil},
			{"ffmpeg_path", "FFmpeg path", fieldText, nil, "empty = system PATH", nil},
		},
	},
	{
		name: "Logs",
		fields: []fieldDef{
			{"log_level", "Log level", fieldCycle, []string{"DEBUG", "INFO", "WARN", "ERROR"}, "logging verbosity", nil},
			{"log_max_file_size", "Max log file size", fieldNumber, nil, "bytes, default: 10MB (requires restart)", nil},
			{"log_max_files", "Max log files", fieldNumber, nil, "rotated files to keep (requires restart)", nil},
		},
	},
	{
		name: "Monitors",
		fields: []fieldDef{
			{"archive_window_days", "Archive window (days)", fieldNumber, nil, "how many days back to archive; upcoming/live always covered (default: 3)", nil},
			{"archive_slots", "Archive slots", fieldNumber, nil, "backlog downloads per channel at once; new content never waits (default: 3)", nil},
			{"feed_check_interval", "Feed check interval", fieldNumber, nil, "minutes (default: 10)", nil},
			{"decapi_check_interval", "DECAPI check interval", fieldNumber, nil, "seconds, 15-3600 or empty for dynamic", nil},
			{"twitch_check_interval", "Twitch check interval", fieldNumber, nil, "seconds (default: 15, range: 1-3600)", nil},
			{"hide_finished_age_days", "Hide finished after", fieldNumber, nil, "days (default: 30)", nil},
			{"probe_cooldown", "Probe cooldown", fieldNumber, nil, "seconds between re-probing the same video's YouTube metadata; 0 = disabled/probe every cycle (default: 0, no max)", nil},
			{"membership_discovery", "Membership discovery", fieldToggle, nil, "scan each YouTube channel's members-only tab for members-only streams (+ their VODs for channels that archive uploads & premieres); needs YouTube cookies (default: on)", nil},
		},
	},
	{
		name: "Downloader",
		fields: []fieldDef{
			{"output_template", "Output template", fieldText, nil, "${title} ${id} ${channel} ${start_date} ${start_time}",
				func(value string) string {
					if value == "" {
						value = "${channel}/${start_date} ${title} [${id}]"
					}
					return templatePreview(value)
				},
			},
			{"max_video_resolution", "Max resolution", fieldNumber, nil, "pixels (e.g. 1080, 2160)", nil},
			{"num_parallel_downloads", "Parallel downloads", fieldNumber, nil, "2-4 recommended, higher uses more CPU/network", nil},
			{"segment_workers", "Segment workers", fieldNumber, nil, "segments fetched at once within one download, not the number of concurrent downloads (default: 12, min 1, no max; above 16 raises bot-detection risk)", nil},
			{"download_chat", "Download chat", fieldToggle, nil, "save live chat as JSON alongside video", nil},
			{"prefer_60fps", "Prefer 60fps", fieldToggle, nil, "prefer 60fps when same resolution available", nil},
			{"maximum_timeout", "YouTube max timeout", fieldNumber, nil, "seconds to keep retrying a stalled YouTube livestream (30s live-checks) before finalizing even if YouTube still reports it live (default: 600, min 30, no max; very large values risk account consequences)", nil},
			{"interruption_timeout", "Interruption resume timeout", fieldNumber, nil, "minutes finalize may stall waiting for an interrupted broadcast to resume; 0 = disabled, finalize never stalls (default: 120, no max)", nil},
			{"incomplete_staging_expiry_days", "Incomplete staging expiry", fieldNumber, nil, "days an incomplete-tail recording keeps staging preserved for Resume; badge never expires, 0 = preserve forever (default: 7, no max)", nil},
		},
	},
	{
		name: "Cookies",
		fields: []fieldDef{
			{"cookie_file", "Cookie file", fieldText, nil, "Netscape format cookies.txt", nil},
			{"active_youtube", "YouTube cookies", fieldToggle, nil, "YouTube cookie indicator in status bar", nil},
			{"active_twitch", "Twitch cookies", fieldToggle, nil, "Twitch cookie indicator in status bar", nil},
			{"auto_enabled", "Auto-cookie", fieldToggle, nil, "browser-based cookie acquisition", nil},
			{"browser_profile_dir", "Browser profile dir", fieldText, nil, "for auto-cookie browser data", nil},
			{"browser_path", "Browser path", fieldText, nil, "override (empty = auto-detect)", nil},
			{"browser_type", "Browser type", fieldText, nil, "firefox/chrome/brave/edge/etc. (required if path set)", nil},
			{"refresh_interval", "Refresh interval", fieldNumber, nil, "minutes (default: 360 = 6h)", nil},
			{"dpapi_fallback", "DPAPI fallback (Windows)", fieldToggle, nil, "fallback: read REAL browser cookies via DPAPI when CDP refresh fails (privacy: reads your signed-in session)", nil},
		},
	},
	{
		name: "Disk",
		fields: []fieldDef{
			{"disk_warn_percent", "Warning threshold", fieldNumber, nil, "% disk usage (default: 90)", nil},
			{"disk_critical_percent", "Critical threshold", fieldNumber, nil, "% disk usage, pauses downloads (default: 95)", nil},
		},
	},
	{
		name: "Updates",
		fields: []fieldDef{
			{"auto_check_updates", "Auto-check updates", fieldToggle, nil, "check GitHub on startup + daily", nil},
		},
	},
	{
		name: "BotGuard Sidecar",
		fields: []fieldDef{
			{"use_sidecar", "Enable sidecar", fieldToggle, nil, "Node + JSDOM + bgutils-js for real BotGuard PO tokens (default: on; falls back to goja-only when off)", nil},
		},
	},
	{
		name: "Memory",
		fields: []fieldDef{
			{"go_soft_limit_mb", "Go soft limit (MB)", fieldNumber, nil, "soft cap; GC ramps up but no OOM (default: 256, 0 disables)", nil},
			{"sidecar_soft_limit_mb", "Sidecar soft limit (MB)", fieldNumber, nil, "RSS threshold to trigger V8 GC (default: 200, 0 disables)", nil},
			{"sidecar_hard_limit_mb", "Sidecar hard limit (MB)", fieldNumber, nil, "V8 --max-old-space-size; OOMs on hit, must exceed soft (default: 512, 0 = V8 default)", nil},
		},
	},
	{
		name:   "Channels",
		fields: nil, // Sub-editor
	},
	{
		name:   "Integrations",
		fields: nil, // Notifications sub-editor
	},
}

// saveStatus tracks the save state.
type saveStatus int

const (
	saveIdle saveStatus = iota
	saveSaved
	saveError
	// saveNotice renders errorMsg as a neutral/positive notice (green) —
	// used for non-save outcomes like test-notification results, where
	// "Saved" would be misleading and red would read as failure.
	saveNotice
)

// securityMode tracks the security editor state.
type securityMode int

const (
	securityStatus securityMode = iota
	securitySet
	securityRemove
)

// notifEventGroup groups notification events under a heading.
type notifEventGroup struct {
	name   string
	events []string
}

// Notification event groups, derived from the canonical vocabulary in
// internal/notifications (notifications.EventGroups) so the TUI can never
// drift from the events the manager actually filters on. The web UI keeps
// a labeled mirror in web/public/modules/settings.js — update that copy
// (and docs/spec/operations.md) when the canonical registry changes.
var notifEventGroups = func() []notifEventGroup {
	out := make([]notifEventGroup, 0, len(notifications.EventGroups))
	for _, g := range notifications.EventGroups {
		out = append(out, notifEventGroup{name: g.Name, events: g.Events})
	}
	return out
}()

// allNotifEvents is a flat list derived from the groups (preserves order).
var allNotifEvents = func() []string {
	var out []string
	for _, g := range notifEventGroups {
		out = append(out, g.events...)
	}
	return out
}()

// channelFieldDef defines a channel editor field.
type channelFieldDef struct {
	key            string
	label          string
	ftype          fieldType
	options        []string
	help           string
	platformFilter string // "youtube", "twitch", or "" for all
}

var channelFields = []channelFieldDef{
	{"id", "Channel ID", fieldText, nil, "ID, @handle, or URL", ""},
	{"name", "Display name", fieldText, nil, "", ""},
	{"platform", "Platform", fieldCycle, []string{"youtube", "twitch"}, "", ""},
	{"enabled", "Enabled", fieldToggle, []string{"Yes", "No"}, "", ""},
	{"terms", "Filter regex", fieldText, nil, "e.g. (?i)karaoke", ""},
	{"include_non_live", "Archive uploads & premieres (YouTube only)", fieldToggle, []string{"No", "Yes"}, "also capture uploads and premieres, not just live streams", "youtube"},
	{"quality_preference", "Quality preference", fieldCycle, []string{"best", "2160p60", "2160p", "1440p60", "1440p", "1080p60", "1080p", "900p60", "900p", "720p60", "720p", "480p", "360p", "160p", "audio_only"}, "", ""},
}

// SettingsModel manages the settings overlay panel.
type SettingsModel struct {
	visible bool
	width   int
	height  int

	// Current section index
	sectionIndex int

	// Current field within section
	fieldIndex   int
	scrollOffset int

	// Values
	values         map[string]string
	originalValues map[string]string
	dirty          bool
	structDirty    bool // set by channel/notification/security edits (not clearable by recheckDirty)

	// Layout state (set during View, read by mouse handler)
	lastButtonContentY int // contentY where buttons were rendered (-1 = not rendered)

	// Save status
	status   saveStatus
	errorMsg string

	// Config reference. cfg is the direct *MoomboxConfig pointer used by
	// applyValues' big-block writes; configStore exposes the same struct
	// with synchronisation for snapshot reads. Both wired together via
	// App.SetConfigStore.
	cfg         *config.MoomboxConfig
	configStore *config.Store

	// Callbacks
	OnSave    func(cfg *config.MoomboxConfig)
	OnRestart func()
	// OnRestartRequired fires when a settings save commits a value
	// flagged in restartRequiredKeys, regardless of whether the user
	// then triggers OnRestart from the modal or dismisses it. The App
	// flips a persistent banner-visible flag so the dismissal case
	// doesn't leave a config/runtime mismatch with no visual reminder.
	// Audit reports/tui.md #26.
	OnRestartRequired func()
	// OnSecurityChanged fires after the Security sub-editor commits a change
	// to the dashboard password (set or remove). Both can flip the persistent
	// security banner — setting a password on a passwordless external config
	// clears it — and the banner occupies rows above the panels, so the App
	// re-derives panel heights + mouse regions the same way OnRestartRequired
	// does. The general settings save needs no equivalent: network_access is
	// a restartRequiredKey, so that path already recalcs via OnRestartRequired.
	OnSecurityChanged func()
	OnHashPassword    func(password string) string
	OnVerifyPassword  func(password, hash string) bool

	// Channel sub-editor state
	channelIndex      int
	channelMode       string // "list" or "edit"
	channelEditValues map[string]string
	channelEditField  int
	channelDeleteConf bool
	channelResolving  bool // true while async URL resolution is in progress
	channels          []config.ChannelConfig

	// Notification sub-editor state
	notifIndex      int
	notifMode       string // "list" or "edit"
	notifEditURL    string
	notifEditEvents map[string]bool
	notifEditFocus  int // 0=URL, 1+=event index
	// notifEditScrollStart is the body scroll offset renderNotifEdit applied on
	// the last render (0 = not scrolled). Mouse click mapping reads it to map an
	// on-screen row back to the original (unscrolled) line.
	notifEditScrollStart int
	notifDeleteConf      bool
	notifications        []config.NotificationConfig

	// Security sub-editor state
	secMode         securityMode
	secMessage      string
	secMessageColor color.Color
	secCurrentPw    string
	secNewPw        string
	secConfirmPw    string
	secRemovePw     string
	secFieldIndex   int

	// Restart overlay
	showRestartOverlay bool

	// Close confirmation
	closeConfirm bool

	// Action buttons (bottom of settings panel when dirty)
	// -1 = fields focused, 0 = Save button, 1 = Return button
	buttonFocus int

	// Shared text input component (holds the currently-active text field)
	textInput textinput.Model
}

// NewSettingsModel creates a new settings model.
func NewSettingsModel() *SettingsModel {
	return &SettingsModel{
		values:         make(map[string]string),
		originalValues: make(map[string]string),
		channelMode:    "list",
		notifMode:      "list",
		textInput:      newTextInput(),
	}
}

// Open shows the settings panel, loading current config values.
func (m *SettingsModel) Open(cfg *config.MoomboxConfig) {
	m.visible = true
	m.cfg = cfg
	m.sectionIndex = 0
	m.fieldIndex = 0
	m.scrollOffset = 0
	m.dirty = false
	m.structDirty = false
	m.status = saveIdle
	m.errorMsg = ""
	m.showRestartOverlay = false
	m.closeConfirm = false
	m.buttonFocus = -1

	// Snapshot config under read lock. Use the closure-scoped `c`
	// (the locked snapshot) consistently — `cfg` is the outer store
	// pointer and reading it outside the callback would not respect
	// Store.Read's RLock contract even though today they reference the
	// same memory.
	m.configStore.Read(func(c *config.MoomboxConfig) {
		// Channel editor
		m.channelIndex = 0
		m.channelMode = "list"
		m.channelDeleteConf = false
		m.channels = make([]config.ChannelConfig, len(c.Channels))
		copy(m.channels, c.Channels)

		// Notification editor
		m.notifIndex = 0
		m.notifMode = "list"
		m.notifDeleteConf = false
		m.notifications = make([]config.NotificationConfig, len(c.Notifications))
		copy(m.notifications, c.Notifications)

		// Load values
		m.loadValues(c)
	})

	// Security
	m.secMode = securityStatus
	m.secMessage = ""
	m.secCurrentPw = ""
	m.secNewPw = ""
	m.secConfirmPw = ""
	m.secRemovePw = ""
	m.secFieldIndex = 0

	m.originalValues = make(map[string]string, len(m.values))
	maps.Copy(m.originalValues, m.values)

	m.updateTextInputForField()
}

// Close hides the settings panel.
func (m *SettingsModel) Close() {
	m.visible = false
}

// IsVisible returns true if the settings panel is shown.
func (m *SettingsModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the panel dimensions.
func (m *SettingsModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.ensureFieldVisible()
}

func (m *SettingsModel) loadValues(cfg *config.MoomboxConfig) {
	// Network
	m.values["port"] = strconv.Itoa(cfg.Network.Port)
	m.values["network_access"] = cfg.Network.NetworkAccess
	m.values["https_enabled"] = boolToDisplay(cfg.Network.HTTPSEnabled)
	m.values["tls_cert_path"] = cfg.Network.TLSCertPath
	m.values["tls_key_path"] = cfg.Network.TLSKeyPath
	m.values["trust_forwarded_proto"] = boolToDisplay(cfg.Network.TrustForwardedProto)
	m.values["trusted_proxies"] = strings.Join(cfg.Network.TrustedProxies, ", ")

	// Paths
	m.values["database_path"] = cfg.Paths.DatabasePath
	m.values["log_file_path"] = cfg.Paths.LogFilePath
	m.values["output_directory"] = cfg.Paths.OutputDirectory
	m.values["staging_directory"] = cfg.Paths.StagingDirectory
	m.values["ffmpeg_path"] = cfg.Paths.FfmpegPath

	// Logs
	m.values["log_level"] = cfg.Logs.LogLevel
	m.values["log_max_file_size"] = strconv.Itoa(cfg.Logs.LogMaxFileSize)
	m.values["log_max_files"] = strconv.Itoa(cfg.Logs.LogMaxFiles)

	// Monitors
	m.values["archive_window_days"] = strconv.Itoa(cfg.Monitors.ArchiveWindowDays)
	m.values["archive_slots"] = strconv.Itoa(cfg.Monitors.ArchiveSlots)
	m.values["feed_check_interval"] = fmt.Sprintf("%.0f", cfg.Monitors.FeedCheckInterval.Minutes())
	if cfg.Monitors.DecapiCheckInterval != nil {
		m.values["decapi_check_interval"] = strconv.Itoa(*cfg.Monitors.DecapiCheckInterval)
	} else {
		m.values["decapi_check_interval"] = ""
	}
	if cfg.Monitors.TwitchCheckInterval != nil {
		m.values["twitch_check_interval"] = strconv.Itoa(*cfg.Monitors.TwitchCheckInterval)
	} else {
		m.values["twitch_check_interval"] = ""
	}
	// FormatFloat with -1 precision round-trips fractional values ("0.5"
	// stays "0.5", "30" stays "30") — %.0f silently rounded them away.
	m.values["hide_finished_age_days"] = strconv.FormatFloat(cfg.Monitors.HideFinishedAgeDays.Days(), 'f', -1, 64)
	m.values["probe_cooldown"] = strconv.Itoa(int(cfg.Monitors.ProbeCooldown.Value))
	// nil normalizes to true (default on) — matches config validation.
	m.values["membership_discovery"] = boolToDisplay(cfg.Monitors.MembershipDiscoveryEnabled())

	// Downloader
	m.values["output_template"] = cfg.Downloader.OutputTemplate
	m.values["max_video_resolution"] = strconv.Itoa(cfg.Downloader.MaxVideoResolution)
	m.values["num_parallel_downloads"] = strconv.Itoa(cfg.Downloader.NumParallelDownloads)
	m.values["segment_workers"] = strconv.Itoa(cfg.Downloader.SegmentWorkers)
	m.values["download_chat"] = boolToDisplay(cfg.Downloader.DownloadChat)
	m.values["prefer_60fps"] = boolToDisplay(cfg.Downloader.Prefer60fps)
	m.values["maximum_timeout"] = strconv.Itoa(cfg.Downloader.MaximumTimeout)
	m.values["interruption_timeout"] = fmt.Sprintf("%.0f", cfg.Downloader.InterruptionTimeout.Minutes())
	m.values["incomplete_staging_expiry_days"] = fmt.Sprintf("%.0f", cfg.Downloader.IncompleteStagingExpiryDays.Days())

	// Cookies
	m.values["cookie_file"] = cfg.Cookies.CookieFile
	ytActive, twActive := config.GetActivePlatforms(cfg)
	m.values["active_youtube"] = boolToDisplay(ytActive)
	m.values["active_twitch"] = boolToDisplay(twActive)
	m.values["auto_enabled"] = boolToDisplay(cfg.Cookies.AutoEnabled)
	m.values["browser_profile_dir"] = cfg.Cookies.BrowserProfileDir
	m.values["browser_path"] = cfg.Cookies.BrowserPath
	m.values["browser_type"] = cfg.Cookies.BrowserType
	m.values["refresh_interval"] = fmt.Sprintf("%.0f", cfg.Cookies.RefreshInterval.Minutes())
	m.values["dpapi_fallback"] = boolToDisplay(cfg.Cookies.DpapiFallback)

	// Disk
	m.values["disk_warn_percent"] = strconv.Itoa(cfg.Disk.WarnPercent)
	m.values["disk_critical_percent"] = strconv.Itoa(cfg.Disk.CriticalPercent)

	// Updates
	m.values["auto_check_updates"] = boolToDisplay(cfg.Updates.AutoCheckUpdates)

	// BotGuard sidecar
	m.values["use_sidecar"] = boolToDisplay(cfg.Bgutils.UseSidecar)

	// Memory
	m.values["go_soft_limit_mb"] = strconv.Itoa(cfg.Memory.GoSoftLimitMB)
	m.values["sidecar_soft_limit_mb"] = strconv.Itoa(cfg.Memory.SidecarSoftLimitMB)
	m.values["sidecar_hard_limit_mb"] = strconv.Itoa(cfg.Memory.SidecarHardLimitMB)
}

func (m *SettingsModel) applyValues() {
	if m.cfg == nil {
		return
	}

	// Validate port
	port, _ := strconv.Atoi(m.values["port"])
	if port < 1 || port > 65535 {
		m.errorMsg = "Port must be 1-65535"
		m.status = saveError
		return
	}

	// Validate external access requires password (snapshot under RLock)
	var passwordHash string
	m.configStore.Read(func(c *config.MoomboxConfig) {
		passwordHash = c.Network.PasswordHash
	})
	if isExternalAccess(m.values["network_access"]) && passwordHash == "" {
		m.errorMsg = "Password required for external access. Set password in Network section."
		m.status = saveError
		return
	}

	// Validate trusted_proxies entries. config.Validate — and therefore the
	// config.Save behind OnSave — REFUSES a config carrying an unparseable
	// entry, so without this gate one typo makes the whole save fail while
	// saveAndClose still reports "Saved" and every other change in that save
	// is lost. Mirrors validateConfigUpdates' web-side field error.
	for _, p := range strings.Split(m.values["trusted_proxies"], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ok := false
		if strings.Contains(p, "/") {
			_, _, err := net.ParseCIDR(p)
			ok = err == nil
		} else {
			ok = net.ParseIP(p) != nil
		}
		if !ok {
			m.errorMsg = fmt.Sprintf("Trusted proxies: %q is not a valid IP or CIDR", p)
			m.status = saveError
			return
		}
	}

	// Validate browser_path if set.
	// Static checks only — the full ValidateBrowserPath spawns a subprocess
	// and waits up to 10s for --version, which would freeze the BubbleTea
	// event loop. The web UI runs the full check via the async HTTP endpoint.
	browserPath := strings.TrimSpace(m.values["browser_path"])
	browserType := strings.TrimSpace(m.values["browser_type"])
	if browserPath != "" {
		if browserType == "" {
			m.errorMsg = "browser_type required when browser_path is set"
			m.status = saveError
			return
		}
		if err := cookies.ValidateBrowserPathQuick(browserPath, browserType); err != nil {
			m.errorMsg = "Invalid browser: " + err.Error()
			m.status = saveError
			return
		}
	}

	// Range-check the remaining numeric fields BEFORE writing — the ranges
	// mirror config.Validate (config.go validateOrNormalize), which Save
	// runs and REFUSES to persist on failure. Without this gate a bad value
	// (e.g. empty → Atoi 0) poisons the live config while the TUI reports
	// "Saved" and the change is silently lost on restart. Mirrors the setup
	// wizard's pre-build checks in finishAdvancedSetup.
	for _, c := range []struct {
		key, msg string
		min, max int
	}{
		{"log_max_file_size", "Max log file size must be 1024-1073741824 bytes", 1024, 1073741824},
		{"log_max_files", "Max log files must be 1-100", 1, 100},
		{"archive_window_days", "Archive window must be 1-3650 days", 1, 3650},
		{"archive_slots", "Archive slots must be 1-100", 1, 100},
		{"feed_check_interval", "Feed check interval must be 1-1440 minutes", 1, 1440},
		{"probe_cooldown", "Probe cooldown must be >= 0 seconds (0 disables)", 0, math.MaxInt},
		{"max_video_resolution", "Max resolution must be at least 1", 1, math.MaxInt},
		{"num_parallel_downloads", "Parallel downloads must be at least 1", 1, math.MaxInt},
		{"segment_workers", "Segment workers must be at least 1", 1, math.MaxInt},
		{"maximum_timeout", "YouTube max timeout must be at least 30 seconds", 30, math.MaxInt},
		{"interruption_timeout", "Interruption resume timeout must be >= 0 minutes (0 disables)", 0, math.MaxInt},
		{"incomplete_staging_expiry_days", "Incomplete staging expiry must be >= 0 days (0 preserves forever)", 0, math.MaxInt},
		{"refresh_interval", "Cookie refresh interval must be 10-10080 minutes", 10, 10080},
		{"disk_warn_percent", "Disk warning threshold must be 1-99", 1, 99},
		{"disk_critical_percent", "Disk critical threshold must be 1-99", 1, 99},
	} {
		n, err := strconv.Atoi(m.values[c.key])
		if err != nil || n < c.min || n > c.max {
			m.errorMsg = c.msg
			m.status = saveError
			return
		}
	}
	warnPct, _ := strconv.Atoi(m.values["disk_warn_percent"])
	critPct, _ := strconv.Atoi(m.values["disk_critical_percent"])
	if critPct <= warnPct {
		m.errorMsg = "Disk critical threshold must exceed warning threshold"
		m.status = saveError
		return
	}
	// hide_finished_age_days is a FLOAT — fractional days (0.5 = 12h) are
	// valid config that the Web UI and config file accept. Parsing it as an
	// int here would silently rewrite 0.5 → 0 on any unrelated TUI save.
	// The explicit NaN/Inf rejection matters: ParseFloat accepts "nan" (and
	// TOML 1.0 has nan/inf literals a hand-edited config could carry), and
	// NaN slips through a min/max range check because both comparisons are
	// false.
	hideAge, hideAgeErr := strconv.ParseFloat(m.values["hide_finished_age_days"], 64)
	if hideAgeErr != nil || math.IsNaN(hideAge) || math.IsInf(hideAge, 0) || hideAge < 0 || hideAge > 365 {
		m.errorMsg = "Hide finished after must be 0-365 days"
		m.status = saveError
		return
	}
	// Text fields config.Validate refuses to save empty.
	for _, c := range []struct{ key, msg string }{
		{"database_path", "Database path must not be empty"},
		{"log_file_path", "Log file path must not be empty"},
		{"output_directory", "Output directory must not be empty"},
		{"staging_directory", "Staging directory must not be empty"},
		{"cookie_file", "Cookie file must not be empty"},
		{"output_template", "Output template must not be empty"},
	} {
		if strings.TrimSpace(m.values[c.key]) == "" {
			m.errorMsg = c.msg
			m.status = saveError
			return
		}
	}
	// Path fields: reject ".." segments. Absolute paths are accepted here and
	// in the Web UI alike — config.PathHasTraversal is the single rule both
	// call, because a value one UI saves and the other refuses is exactly the
	// defect this replaced (see internal/config/pathcheck.go).
	for _, c := range []struct{ key, label string }{
		{"database_path", "Database path"},
		{"log_file_path", "Log file path"},
		{"output_directory", "Output directory"},
		{"staging_directory", "Staging directory"},
		{"ffmpeg_path", "FFmpeg path"},
		{"tls_cert_path", "TLS certificate path"},
		{"tls_key_path", "TLS key path"},
		{"cookie_file", "Cookie file"},
		{"browser_profile_dir", "Browser profile directory"},
	} {
		if config.PathHasTraversal(m.values[c.key]) {
			m.errorMsg = c.label + " cannot contain a .. segment"
			m.status = saveError
			return
		}
	}

	// Lock for all config writes
	mu := m.configStore.RWMutex()
	mu.Lock()

	// Network
	m.cfg.Network.Port = port
	m.cfg.Network.NetworkAccess = m.values["network_access"]
	m.cfg.Network.HTTPSEnabled = m.values["https_enabled"] == "Yes"
	m.cfg.Network.TLSCertPath = m.values["tls_cert_path"]
	m.cfg.Network.TLSKeyPath = m.values["tls_key_path"]
	m.cfg.Network.TrustForwardedProto = m.values["trust_forwarded_proto"] == "Yes"
	proxies := []string(nil)
	for _, p := range strings.Split(m.values["trusted_proxies"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			proxies = append(proxies, p)
		}
	}
	m.cfg.Network.TrustedProxies = proxies

	// Paths
	m.cfg.Paths.DatabasePath = m.values["database_path"]
	m.cfg.Paths.LogFilePath = m.values["log_file_path"]
	m.cfg.Paths.OutputDirectory = m.values["output_directory"]
	m.cfg.Paths.StagingDirectory = m.values["staging_directory"]
	m.cfg.Paths.FfmpegPath = m.values["ffmpeg_path"]

	// Logs
	m.cfg.Logs.LogLevel = m.values["log_level"]
	m.cfg.Logs.LogMaxFileSize, _ = strconv.Atoi(m.values["log_max_file_size"])
	m.cfg.Logs.LogMaxFiles, _ = strconv.Atoi(m.values["log_max_files"])

	// Monitors
	m.cfg.Monitors.ArchiveWindowDays, _ = strconv.Atoi(m.values["archive_window_days"])
	m.cfg.Monitors.ArchiveSlots, _ = strconv.Atoi(m.values["archive_slots"])
	feedMin, _ := strconv.Atoi(m.values["feed_check_interval"])
	m.cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: float64(feedMin)}
	if v := m.values["decapi_check_interval"]; v != "" {
		if d, err := strconv.Atoi(v); err == nil && d >= 15 && d <= 3600 {
			m.cfg.Monitors.DecapiCheckInterval = &d
		} else {
			m.cfg.Monitors.DecapiCheckInterval = nil
		}
	} else {
		m.cfg.Monitors.DecapiCheckInterval = nil
	}
	// Lower bound 5 matches config.Validate — a 1-4s value would make
	// Save refuse to persist the whole config.
	if v := m.values["twitch_check_interval"]; v != "" {
		if t, err := strconv.Atoi(v); err == nil && t >= 5 && t <= 3600 {
			m.cfg.Monitors.TwitchCheckInterval = &t
		} else {
			m.cfg.Monitors.TwitchCheckInterval = nil
		}
	} else {
		m.cfg.Monitors.TwitchCheckInterval = nil
	}
	m.cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: hideAge}
	probeCd, _ := strconv.Atoi(m.values["probe_cooldown"])
	m.cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: float64(probeCd)}
	membershipOn := m.values["membership_discovery"] == "Yes"
	m.cfg.Monitors.MembershipDiscovery = &membershipOn

	// Downloader
	m.cfg.Downloader.OutputTemplate = m.values["output_template"]
	m.cfg.Downloader.MaxVideoResolution, _ = strconv.Atoi(m.values["max_video_resolution"])
	m.cfg.Downloader.NumParallelDownloads, _ = strconv.Atoi(m.values["num_parallel_downloads"])
	m.cfg.Downloader.SegmentWorkers, _ = strconv.Atoi(m.values["segment_workers"])
	m.cfg.Downloader.DownloadChat = m.values["download_chat"] == "Yes"
	m.cfg.Downloader.Prefer60fps = m.values["prefer_60fps"] == "Yes"
	m.cfg.Downloader.MaximumTimeout, _ = strconv.Atoi(m.values["maximum_timeout"])
	interruptionMin, _ := strconv.Atoi(m.values["interruption_timeout"])
	m.cfg.Downloader.InterruptionTimeout = config.FlexDuration{Value: float64(interruptionMin)}
	expiryDays, _ := strconv.Atoi(m.values["incomplete_staging_expiry_days"])
	m.cfg.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: float64(expiryDays)}

	// Cookies
	m.cfg.Cookies.CookieFile = m.values["cookie_file"]
	var activePlats []string
	if m.values["active_youtube"] == "Yes" {
		activePlats = append(activePlats, "youtube")
	}
	if m.values["active_twitch"] == "Yes" {
		activePlats = append(activePlats, "twitch")
	}
	m.cfg.Cookies.ActivePlatforms = activePlats
	m.cfg.Cookies.AutoEnabled = m.values["auto_enabled"] == "Yes"
	m.cfg.Cookies.BrowserProfileDir = m.values["browser_profile_dir"]
	// TrimSpace matches what validateConfigUpdates does for the web path
	// (config_routes.go:424,427). Without trimming here, a user pasting
	// "  /usr/bin/firefox  " would pass the trimmed validation above but
	// persist whitespace into config — exec.Command would then fail with
	// "fork/exec  /usr/bin/firefox  : no such file or directory".
	m.cfg.Cookies.BrowserPath = strings.TrimSpace(m.values["browser_path"])
	m.cfg.Cookies.BrowserType = strings.TrimSpace(m.values["browser_type"])
	refreshMin, _ := strconv.Atoi(m.values["refresh_interval"])
	m.cfg.Cookies.RefreshInterval = config.FlexDuration{Value: float64(refreshMin)}
	m.cfg.Cookies.DpapiFallback = m.values["dpapi_fallback"] == "Yes"

	// Disk
	m.cfg.Disk.WarnPercent, _ = strconv.Atoi(m.values["disk_warn_percent"])
	m.cfg.Disk.CriticalPercent, _ = strconv.Atoi(m.values["disk_critical_percent"])

	// Updates
	m.cfg.Updates.AutoCheckUpdates = m.values["auto_check_updates"] == "Yes"

	// BotGuard sidecar
	m.cfg.Bgutils.UseSidecar = m.values["use_sidecar"] == "Yes"

	// Memory
	m.cfg.Memory.GoSoftLimitMB, _ = strconv.Atoi(m.values["go_soft_limit_mb"])
	m.cfg.Memory.SidecarSoftLimitMB, _ = strconv.Atoi(m.values["sidecar_soft_limit_mb"])
	m.cfg.Memory.SidecarHardLimitMB, _ = strconv.Atoi(m.values["sidecar_hard_limit_mb"])

	// Apply channels and notifications
	m.cfg.Channels = m.channels
	m.cfg.Notifications = m.notifications

	mu.Unlock()
}

// recheckDirty recalculates dirty state by comparing values against originalValues.
// Preserves dirty if structDirty is set (channel/notification/security changes).
func (m *SettingsModel) recheckDirty() {
	if m.structDirty {
		m.dirty = true
		return
	}
	for k, v := range m.values {
		if v != m.originalValues[k] {
			m.dirty = true
			return
		}
	}
	m.dirty = false
}

func (m *SettingsModel) hasRestartChanges() bool {
	for key := range restartRequiredKeys {
		if m.values[key] != m.originalValues[key] {
			return true
		}
	}
	return false
}

func (m *SettingsModel) isFieldSection() bool {
	return sections[m.sectionIndex].fields != nil
}

func boolToDisplay(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

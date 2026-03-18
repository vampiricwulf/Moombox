package tui

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// fieldType identifies how a settings field is edited.
type fieldType int

const (
	fieldText   fieldType = iota
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
var restartRequiredKeys = map[string]bool{
	"port":              true,
	"network_access":    true,
	"https_enabled":     true,
	"tls_cert_path":     true,
	"tls_key_path":      true,
	"database_path":     true,
	"log_file_path":     true,
	"log_max_file_size": true,
	"log_max_files":     true,
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
			{"max_feed_items", "Max feed items", fieldNumber, nil, "RSS items per feed (default: 15)", nil},
			{"feed_check_interval", "Feed check interval", fieldNumber, nil, "minutes (default: 10)", nil},
			{"decapi_check_interval", "DECAPI check interval", fieldNumber, nil, "seconds, 15-3600 or empty for dynamic", nil},
			{"twitch_check_interval", "Twitch check interval", fieldNumber, nil, "seconds (default: 15, range: 1-3600)", nil},
			{"hide_finished_age_days", "Hide finished after", fieldNumber, nil, "days (default: 30)", nil},
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
					now := time.Now().Format("2006-01-02")
					r := strings.NewReplacer(
						"${channel}", "Miko Ch",
						"${title}", "Singing Stream",
						"${id}", "dQw4w9WgXcQ",
						"${start_date}", now,
						"${start_time}", "20-00-00",
					)
					return "Example: " + r.Replace(value) + ".mkv"
				},
			},
			{"max_video_resolution", "Max resolution", fieldNumber, nil, "pixels (e.g. 1080, 2160)", nil},
			{"num_parallel_downloads", "Parallel downloads", fieldNumber, nil, "concurrent download jobs", nil},
			{"download_chat", "Download chat", fieldToggle, nil, "save live chat as JSON alongside video", nil},
			{"prefer_60fps", "Prefer 60fps", fieldToggle, nil, "prefer 60fps when same resolution available", nil},
			{"segment_retry_delay_cap", "Segment retry cap", fieldNumber, nil, "max retry backoff in seconds (default: 60)", nil},
			{"segment_live_check_retries", "Live check retries", fieldNumber, nil, "failures before API check (default: 16)", nil},
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
			{"refresh_interval", "Refresh interval", fieldNumber, nil, "minutes (default: 360 = 6h)", nil},
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

// Notification event groups.
var notifEventGroups = []notifEventGroup{
	{"Job Lifecycle", []string{"found", "added", "scheduled", "live", "downloading", "muxing", "finished", "error", "cancelled", "auth"}},
	{"Trim", []string{"trim_created", "trim_deleted", "trim_error"}},
	{"System", []string{"disk_warning", "update_available"}},
}

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

	// Save status
	status   saveStatus
	errorMsg string

	// Config reference
	cfg   *config.MoomboxConfig
	cfgMu *sync.RWMutex // shared config mutex

	// Callbacks
	OnSave           func(cfg *config.MoomboxConfig)
	OnRestart        func()
	OnHashPassword   func(password string) string
	OnVerifyPassword func(password, hash string) bool

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
	notifDeleteConf bool
	notifications   []config.NotificationConfig

	// Security sub-editor state
	secMode          securityMode
	secMessage       string
	secMessageColor  lipgloss.Color
	secCurrentPw     string
	secNewPw         string
	secConfirmPw     string
	secRemovePw      string
	secFieldIndex    int

	// Restart overlay
	showRestartOverlay bool

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
	m.status = saveIdle
	m.errorMsg = ""
	m.showRestartOverlay = false

	// Snapshot config under read lock
	if m.cfgMu != nil {
		m.cfgMu.RLock()
	}

	// Channel editor
	m.channelIndex = 0
	m.channelMode = "list"
	m.channelDeleteConf = false
	m.channels = make([]config.ChannelConfig, len(cfg.Channels))
	copy(m.channels, cfg.Channels)

	// Notification editor
	m.notifIndex = 0
	m.notifMode = "list"
	m.notifDeleteConf = false
	m.notifications = make([]config.NotificationConfig, len(cfg.Notifications))
	copy(m.notifications, cfg.Notifications)

	// Load values
	m.loadValues(cfg)

	if m.cfgMu != nil {
		m.cfgMu.RUnlock()
	}

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
	m.values["max_feed_items"] = strconv.Itoa(cfg.Monitors.MaxFeedItems)
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
	m.values["hide_finished_age_days"] = fmt.Sprintf("%.0f", cfg.Monitors.HideFinishedAgeDays.Days())

	// Downloader
	m.values["output_template"] = cfg.Downloader.OutputTemplate
	m.values["max_video_resolution"] = strconv.Itoa(cfg.Downloader.MaxVideoResolution)
	m.values["num_parallel_downloads"] = strconv.Itoa(cfg.Downloader.NumParallelDownloads)
	m.values["download_chat"] = boolToDisplay(cfg.Downloader.DownloadChat)
	m.values["prefer_60fps"] = boolToDisplay(cfg.Downloader.Prefer60fps)
	m.values["segment_retry_delay_cap"] = strconv.Itoa(cfg.Downloader.SegmentRetryDelayCap)
	m.values["segment_live_check_retries"] = strconv.Itoa(cfg.Downloader.SegmentLiveCheckRetries)

	// Cookies
	m.values["cookie_file"] = cfg.Cookies.CookieFile
	ytActive, twActive := config.GetActivePlatforms(cfg)
	m.values["active_youtube"] = boolToDisplay(ytActive)
	m.values["active_twitch"] = boolToDisplay(twActive)
	m.values["auto_enabled"] = boolToDisplay(cfg.Cookies.AutoEnabled)
	m.values["browser_profile_dir"] = cfg.Cookies.BrowserProfileDir
	m.values["refresh_interval"] = fmt.Sprintf("%.0f", cfg.Cookies.RefreshInterval.Minutes())

	// Disk
	m.values["disk_warn_percent"] = strconv.Itoa(cfg.Disk.WarnPercent)
	m.values["disk_critical_percent"] = strconv.Itoa(cfg.Disk.CriticalPercent)

	// Updates
	m.values["auto_check_updates"] = boolToDisplay(cfg.Updates.AutoCheckUpdates)
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
	if m.cfgMu != nil {
		m.cfgMu.RLock()
	}
	passwordHash := m.cfg.Network.PasswordHash
	if m.cfgMu != nil {
		m.cfgMu.RUnlock()
	}
	if m.values["network_access"] == "external" && passwordHash == "" {
		m.errorMsg = "Password required for external access. Set password in Network section."
		m.status = saveError
		return
	}

	// Lock for all config writes
	if m.cfgMu != nil {
		m.cfgMu.Lock()
	}

	// Network
	m.cfg.Network.Port = port
	m.cfg.Network.NetworkAccess = m.values["network_access"]
	m.cfg.Network.HTTPSEnabled = m.values["https_enabled"] == "Yes"
	m.cfg.Network.TLSCertPath = m.values["tls_cert_path"]
	m.cfg.Network.TLSKeyPath = m.values["tls_key_path"]

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
	m.cfg.Monitors.MaxFeedItems, _ = strconv.Atoi(m.values["max_feed_items"])
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
	if v := m.values["twitch_check_interval"]; v != "" {
		if t, err := strconv.Atoi(v); err == nil && t >= 1 && t <= 3600 {
			m.cfg.Monitors.TwitchCheckInterval = &t
		} else {
			m.cfg.Monitors.TwitchCheckInterval = nil
		}
	} else {
		m.cfg.Monitors.TwitchCheckInterval = nil
	}
	hideAge, _ := strconv.Atoi(m.values["hide_finished_age_days"])
	m.cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: float64(hideAge)}

	// Downloader
	m.cfg.Downloader.OutputTemplate = m.values["output_template"]
	m.cfg.Downloader.MaxVideoResolution, _ = strconv.Atoi(m.values["max_video_resolution"])
	m.cfg.Downloader.NumParallelDownloads, _ = strconv.Atoi(m.values["num_parallel_downloads"])
	m.cfg.Downloader.DownloadChat = m.values["download_chat"] == "Yes"
	m.cfg.Downloader.Prefer60fps = m.values["prefer_60fps"] == "Yes"
	m.cfg.Downloader.SegmentRetryDelayCap, _ = strconv.Atoi(m.values["segment_retry_delay_cap"])
	m.cfg.Downloader.SegmentLiveCheckRetries, _ = strconv.Atoi(m.values["segment_live_check_retries"])

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
	refreshMin, _ := strconv.Atoi(m.values["refresh_interval"])
	m.cfg.Cookies.RefreshInterval = config.FlexDuration{Value: float64(refreshMin)}

	// Disk
	m.cfg.Disk.WarnPercent, _ = strconv.Atoi(m.values["disk_warn_percent"])
	m.cfg.Disk.CriticalPercent, _ = strconv.Atoi(m.values["disk_critical_percent"])

	// Updates
	m.cfg.Updates.AutoCheckUpdates = m.values["auto_check_updates"] == "Yes"

	// Apply channels and notifications
	m.cfg.Channels = m.channels
	m.cfg.Notifications = m.notifications

	if m.cfgMu != nil {
		m.cfgMu.Unlock()
	}
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

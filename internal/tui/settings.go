package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

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
	key     string
	label   string
	ftype   fieldType
	options []string // for cycle fields
	help    string
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
			{"port", "Port", fieldNumber, nil, "1-65535"},
			{"network_access", "Network access", fieldCycle, []string{"localhost", "lan", "external"}, ""},
			{"https_enabled", "HTTPS enabled", fieldToggle, nil, ""},
		},
	},
	{
		name: "Paths",
		fields: []fieldDef{
			{"database_path", "Database path", fieldText, nil, ""},
			{"log_file_path", "Log file path", fieldText, nil, ""},
			{"output_directory", "Output directory", fieldText, nil, "where finished files go"},
			{"staging_directory", "Staging directory", fieldText, nil, "temp files during download"},
			{"ffmpeg_path", "FFmpeg path", fieldText, nil, "empty = system PATH"},
		},
	},
	{
		name: "Logs",
		fields: []fieldDef{
			{"log_level", "Log level", fieldCycle, []string{"DEBUG", "INFO", "WARN", "ERROR"}, ""},
			{"log_max_file_size", "Max log file size", fieldNumber, nil, "bytes"},
			{"log_max_files", "Max log files", fieldNumber, nil, ""},
		},
	},
	{
		name: "Monitors",
		fields: []fieldDef{
			{"max_feed_items", "Max feed items", fieldNumber, nil, "RSS items per feed"},
			{"feed_check_interval", "Feed check interval", fieldNumber, nil, "minutes"},
			{"decapi_check_interval", "DECAPI check interval", fieldNumber, nil, "seconds (0=dynamic)"},
			{"twitch_check_interval", "Twitch check interval", fieldNumber, nil, "seconds (default: 15)"},
			{"hide_finished_age_days", "Hide finished after", fieldNumber, nil, "days"},
		},
	},
	{
		name: "Downloader",
		fields: []fieldDef{
			{"output_template", "Output template", fieldText, nil, "${title} ${id} ${channel} ${start_date} ${start_time}"},
			{"max_video_resolution", "Max resolution", fieldNumber, nil, "pixels"},
			{"num_parallel_downloads", "Parallel downloads", fieldNumber, nil, "concurrent jobs"},
			{"download_chat", "Download chat", fieldToggle, nil, ""},
			{"prefer_60fps", "Prefer 60fps", fieldToggle, nil, ""},
			{"segment_retry_delay_cap", "Segment retry cap", fieldNumber, nil, "seconds"},
			{"segment_live_check_retries", "Live check retries", fieldNumber, nil, "before marking stream ended"},
		},
	},
	{
		name: "Cookies",
		fields: []fieldDef{
			{"cookie_file", "Cookie file", fieldText, nil, "Netscape format cookies.txt"},
			{"auto_enabled", "Auto-cookie", fieldToggle, nil, "browser-based cookie acquisition"},
			{"browser_profile_dir", "Browser profile dir", fieldText, nil, "for auto-cookie browser data"},
		},
	},
	{
		name: "Channels",
		fields: nil, // Sub-editor
	},
	{
		name: "Integrations",
		fields: nil, // Notifications sub-editor
	},
}

const settingsLabelWidth = 22

// saveStatus tracks the save state.
type saveStatus int

const (
	saveIdle saveStatus = iota
	saveSaving
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

// Notification events.
var allNotifEvents = []string{"found", "live", "downloading", "finished", "error", "cancelled", "auth", "added"}

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
	{"id", "Channel ID", fieldText, nil, "e.g. UC... or twitch login", ""},
	{"name", "Display name", fieldText, nil, "", ""},
	{"platform", "Platform", fieldCycle, []string{"youtube", "twitch"}, "", ""},
	{"enabled", "Enabled", fieldToggle, []string{"Yes", "No"}, "", ""},
	{"terms", "Filter regex", fieldText, nil, "e.g. (?i)karaoke", ""},
	{"include_non_live", "Include non-live", fieldToggle, []string{"No", "Yes"}, "", "youtube"},
	{"quality_preference", "Quality preference", fieldCycle, []string{"best", "720p", "480p", "audio_only"}, "", "twitch"},
}

// SettingsModel manages the settings overlay panel.
type SettingsModel struct {
	visible bool
	width   int
	height  int

	// Current section (0-7)
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
	cfg *config.MoomboxConfig

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
}

// NewSettingsModel creates a new settings model.
func NewSettingsModel() *SettingsModel {
	return &SettingsModel{
		values:         make(map[string]string),
		originalValues: make(map[string]string),
		channelMode:    "list",
		notifMode:      "list",
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

	// Security
	m.secMode = securityStatus
	m.secMessage = ""
	m.secCurrentPw = ""
	m.secNewPw = ""
	m.secConfirmPw = ""
	m.secRemovePw = ""
	m.secFieldIndex = 0

	// Load values
	m.loadValues(cfg)
	m.originalValues = make(map[string]string)
	for k, v := range m.values {
		m.originalValues[k] = v
	}
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
}

func (m *SettingsModel) loadValues(cfg *config.MoomboxConfig) {
	// Network
	m.values["port"] = strconv.Itoa(cfg.Network.Port)
	m.values["network_access"] = cfg.Network.NetworkAccess
	m.values["https_enabled"] = boolToDisplay(cfg.Network.HTTPSEnabled)

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
	m.values["feed_check_interval"] = strconv.Itoa(int(cfg.Monitors.FeedCheckInterval.Minutes()))
	decapi := 0
	if cfg.Monitors.DecapiCheckInterval != nil {
		decapi = *cfg.Monitors.DecapiCheckInterval
	}
	m.values["decapi_check_interval"] = strconv.Itoa(decapi)
	twitch := 15
	if cfg.Monitors.TwitchCheckInterval != nil {
		twitch = *cfg.Monitors.TwitchCheckInterval
	}
	m.values["twitch_check_interval"] = strconv.Itoa(twitch)
	m.values["hide_finished_age_days"] = strconv.Itoa(int(cfg.Monitors.HideFinishedAgeDays.Days()))

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
	m.values["auto_enabled"] = boolToDisplay(cfg.Cookies.AutoEnabled)
	m.values["browser_profile_dir"] = cfg.Cookies.BrowserProfileDir
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

	// Validate external access requires password
	if m.values["network_access"] == "external" && m.cfg.Network.PasswordHash == "" {
		m.errorMsg = "Password required for external access. Set password in Network section."
		m.status = saveError
		return
	}

	// Network
	m.cfg.Network.Port = port
	m.cfg.Network.NetworkAccess = m.values["network_access"]
	m.cfg.Network.HTTPSEnabled = m.values["https_enabled"] == "Yes"

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
	decapi, _ := strconv.Atoi(m.values["decapi_check_interval"])
	m.cfg.Monitors.DecapiCheckInterval = &decapi
	twitchInt, _ := strconv.Atoi(m.values["twitch_check_interval"])
	m.cfg.Monitors.TwitchCheckInterval = &twitchInt
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
	m.cfg.Cookies.AutoEnabled = m.values["auto_enabled"] == "Yes"
	m.cfg.Cookies.BrowserProfileDir = m.values["browser_profile_dir"]

	// Apply channels and notifications
	m.cfg.Channels = m.channels
	m.cfg.Notifications = m.notifications
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

// HandleKey processes key input in the settings panel.
func (m *SettingsModel) HandleKey(key string) (action string) {
	// Restart overlay
	if m.showRestartOverlay {
		switch key {
		case keyEnter:
			m.Close()
			return "restart"
		case keyEsc:
			m.showRestartOverlay = false
			m.Close()
			return "close"
		}
		return ""
	}

	// Clear error on input
	if m.status == saveError {
		m.status = saveIdle
		m.errorMsg = ""
	}

	sec := sections[m.sectionIndex]

	// Route to sub-editors
	switch sec.name {
	case "Channels":
		return m.handleChannelKey(key)
	case "Integrations":
		return m.handleNotifKey(key)
	case "Network":
		// Security sub-editor is embedded in Network section
		if m.secMode != securityStatus {
			return m.handleSecurityKey(key)
		}
		// Check for S/R to enter security mode (these won't conflict with
		// Network fields since port is number-only, the rest are cycle/toggle)
		switch key {
		case "s", "S":
			m.secMode = securitySet
			m.secCurrentPw = ""
			m.secNewPw = ""
			m.secConfirmPw = ""
			m.secFieldIndex = 0
			m.secMessage = ""
			return ""
		case "r", "R":
			if m.hasPassword() {
				m.secMode = securityRemove
				m.secRemovePw = ""
				m.secMessage = ""
				return ""
			}
		}
		return m.handleFieldKey(key)
	}

	// Field section handling (inline editing - no separate edit mode)
	return m.handleFieldKey(key)
}

func (m *SettingsModel) handleFieldKey(key string) string {
	sec := sections[m.sectionIndex]
	if sec.fields == nil {
		return ""
	}

	field := sec.fields[m.fieldIndex]
	isTextual := field.ftype == fieldText || field.ftype == fieldNumber

	switch key {
	case keyEsc:
		return m.handleClose()

	case keyUp:
		if m.fieldIndex > 0 {
			m.fieldIndex--
			m.ensureFieldVisible()
		}
		return ""

	case keyDown:
		if m.fieldIndex < len(sec.fields)-1 {
			m.fieldIndex++
			m.ensureFieldVisible()
		}
		return ""

	case "left":
		if field.ftype == fieldToggle {
			m.toggleField(field)
		} else if field.ftype == fieldCycle {
			m.cycleFieldReverse(field)
		} else if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
		}
		return ""

	case "right":
		if field.ftype == fieldToggle {
			m.toggleField(field)
		} else if field.ftype == fieldCycle {
			m.cycleFieldForward(field)
		} else if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
		}
		return ""

	case keyTab:
		m.switchSection((m.sectionIndex + 1) % len(sections))
		return ""

	case "backspace":
		if isTextual {
			cur := m.values[field.key]
			if len(cur) > 0 {
				runes := []rune(cur)
				m.values[field.key] = string(runes[:len(runes)-1])
				m.dirty = true
				m.status = saveIdle
			}
		}
		return ""

	case "ctrl+v":
		// Clipboard paste support (match TS: first line only, filter for number fields)
		if isTextual {
			if clip := readClipboard(); clip != "" {
				// Use first line only
				if idx := strings.IndexAny(clip, "\r\n"); idx >= 0 {
					clip = clip[:idx]
				}
				clip = strings.TrimSpace(clip)
				// Filter for number fields: strip non-digits (match TS)
				if field.ftype == fieldNumber {
					filtered := strings.Map(func(r rune) rune {
						if r >= '0' && r <= '9' {
							return r
						}
						return -1
					}, clip)
					clip = filtered
				}
				if clip != "" {
					m.values[field.key] += clip
					m.dirty = true
					m.status = saveIdle
				}
			}
		}
		return ""

	default:
		// Number keys 1-5: jump to section (only when NOT on text/number field)
		if !isTextual && len(key) == 1 && key[0] >= '1' && key[0] <= '8' {
			idx := int(key[0]-'0') - 1
			if idx >= 0 && idx < len(sections) {
				m.switchSection(idx)
			}
			return ""
		}

		// Text input for text/number fields
		if isTextual && len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			// Reject escape sequences
			if key[0] == 0x1b {
				return ""
			}
			// Number fields only accept digits
			if field.ftype == fieldNumber && (key[0] < '0' || key[0] > '9') {
				return ""
			}
			m.values[field.key] = m.values[field.key] + key
			m.dirty = true
			m.status = saveIdle
		}
		return ""
	}
}

func (m *SettingsModel) handleClose() string {
	if m.dirty && m.status != saveError {
		m.applyValues()
		if m.status == saveError {
			return "" // Validation failed, show error
		}
		if m.OnSave != nil {
			m.OnSave(m.cfg)
		}
		m.status = saveSaved
		m.dirty = false
		m.originalValues = make(map[string]string)
		for k, v := range m.values {
			m.originalValues[k] = v
		}
		if m.hasRestartChanges() {
			m.showRestartOverlay = true
			return ""
		}
	}
	m.Close()
	return "close"
}

func (m *SettingsModel) switchSection(idx int) {
	if idx >= 0 && idx < len(sections) {
		m.sectionIndex = idx
		m.fieldIndex = 0
		m.scrollOffset = 0
	}
}

func (m *SettingsModel) toggleField(fd fieldDef) {
	cur := m.values[fd.key]
	if cur == "Yes" {
		m.values[fd.key] = "No"
	} else {
		m.values[fd.key] = "Yes"
	}
	m.dirty = true
	m.status = saveIdle
}

func (m *SettingsModel) cycleFieldForward(fd fieldDef) {
	cur := m.values[fd.key]
	for i, opt := range fd.options {
		if strings.EqualFold(opt, cur) {
			m.values[fd.key] = fd.options[(i+1)%len(fd.options)]
			m.dirty = true
			m.status = saveIdle
			return
		}
	}
	if len(fd.options) > 0 {
		m.values[fd.key] = fd.options[0]
		m.dirty = true
		m.status = saveIdle
	}
}

func (m *SettingsModel) cycleFieldReverse(fd fieldDef) {
	cur := m.values[fd.key]
	for i, opt := range fd.options {
		if strings.EqualFold(opt, cur) {
			idx := (i - 1 + len(fd.options)) % len(fd.options)
			m.values[fd.key] = fd.options[idx]
			m.dirty = true
			m.status = saveIdle
			return
		}
	}
	if len(fd.options) > 0 {
		m.values[fd.key] = fd.options[len(fd.options)-1]
		m.dirty = true
		m.status = saveIdle
	}
}

func (m *SettingsModel) ensureFieldVisible() {
	contentH := m.settingsContentHeight()
	if m.fieldIndex < m.scrollOffset {
		m.scrollOffset = m.fieldIndex
	}
	if m.fieldIndex >= m.scrollOffset+contentH {
		m.scrollOffset = m.fieldIndex - contentH + 1
	}
}

func (m *SettingsModel) settingsContentHeight() int {
	h := m.height - 10 // borders, header, tabs, status, footer
	if h < 5 {
		h = 5
	}
	return h
}

// --- Channel sub-editor ---

func (m *SettingsModel) visibleChannelFields() []channelFieldDef {
	platform := "youtube"
	if m.channelEditValues != nil {
		if p, ok := m.channelEditValues["platform"]; ok {
			platform = p
		}
	}
	var fields []channelFieldDef
	for _, f := range channelFields {
		if f.platformFilter == "" || f.platformFilter == platform {
			fields = append(fields, f)
		}
	}
	return fields
}

func channelToValues(ch config.ChannelConfig) map[string]string {
	terms := ch.Terms.Simple
	enabled := "Yes"
	if ch.Enabled != nil && !*ch.Enabled {
		enabled = "No"
	}
	return map[string]string{
		"id":                ch.ID,
		"name":              ch.Name,
		"platform":          ch.GetPlatform(),
		"enabled":           enabled,
		"terms":             terms,
		"include_non_live":  boolToDisplay(ch.IncludeNonLiveContent),
		"quality_preference": ch.QualityPreference,
	}
}

func valuesToChannel(vals map[string]string) config.ChannelConfig {
	ch := config.ChannelConfig{
		ID:       vals["id"],
		Name:     vals["name"],
		Platform: vals["platform"],
	}
	if vals["enabled"] == "No" {
		f := false
		ch.Enabled = &f
	}
	if vals["terms"] != "" {
		ch.Terms = config.ChannelTerms{Simple: vals["terms"]}
	}
	if vals["platform"] == "youtube" && vals["include_non_live"] == "Yes" {
		ch.IncludeNonLiveContent = true
	}
	if vals["platform"] == "twitch" && vals["quality_preference"] != "" {
		ch.QualityPreference = vals["quality_preference"]
	}
	return ch
}

func (m *SettingsModel) handleChannelKey(key string) string {
	if m.channelMode == "edit" {
		return m.handleChannelEditKey(key)
	}

	// List mode
	if m.channelDeleteConf {
		if key == "d" || key == "D" {
			if m.channelIndex < len(m.channels) {
				m.channels = append(m.channels[:m.channelIndex], m.channels[m.channelIndex+1:]...)
				if m.channelIndex >= len(m.channels) && m.channelIndex > 0 {
					m.channelIndex--
				}
				m.dirty = true
			}
			m.channelDeleteConf = false
		} else {
			m.channelDeleteConf = false
		}
		return ""
	}

	switch key {
	case keyEsc:
		return m.handleClose()
	case keyUp:
		if m.channelIndex > 0 {
			m.channelIndex--
		}
	case keyDown:
		if m.channelIndex < len(m.channels)-1 {
			m.channelIndex++
		}
	case keyEnter:
		if len(m.channels) > 0 {
			m.channelMode = "edit"
			m.channelEditValues = channelToValues(m.channels[m.channelIndex])
			m.channelEditField = 0
		}
	case "a", "A":
		m.channelEditValues = map[string]string{
			"id": "", "name": "", "platform": "youtube",
			"enabled": "Yes", "terms": "",
			"include_non_live": "No", "quality_preference": "best",
		}
		m.channelEditField = 0
		m.channelIndex = len(m.channels) // Will be new index
		m.channelMode = "edit"
	case "d", "D":
		if len(m.channels) > 0 {
			m.channelDeleteConf = true
		}
	case "left":
		if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
		}
	case "right":
		if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
		}
	default:
		// Number keys for section jump
		if len(key) == 1 && key[0] >= '1' && key[0] <= '8' {
			idx := int(key[0]-'0') - 1
			if idx >= 0 && idx < len(sections) {
				m.switchSection(idx)
			}
		}
	}
	return ""
}

func (m *SettingsModel) handleChannelEditKey(key string) string {
	fields := m.visibleChannelFields()
	if m.channelEditField >= len(fields) {
		m.channelEditField = len(fields) - 1
	}
	field := fields[m.channelEditField]

	switch key {
	case keyEsc:
		m.channelMode = "list"
		return ""

	case keyEnter:
		// Save the channel
		if id := strings.TrimSpace(m.channelEditValues["id"]); id == "" {
			return "" // ID required
		}
		ch := valuesToChannel(m.channelEditValues)
		if m.channelIndex < len(m.channels) {
			m.channels[m.channelIndex] = ch
		} else {
			m.channels = append(m.channels, ch)
		}
		m.dirty = true
		m.status = saveIdle
		m.channelMode = "list"
		return ""

	case keyUp:
		if m.channelEditField > 0 {
			m.channelEditField--
		}
		return ""

	case keyDown:
		if m.channelEditField < len(fields)-1 {
			m.channelEditField++
		}
		return ""

	case "left":
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelOption(field, -1)
		}
		return ""

	case "right":
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelOption(field, 1)
		}
		return ""

	case "backspace":
		if field.ftype == fieldText {
			cur := m.channelEditValues[field.key]
			if len(cur) > 0 {
				runes := []rune(cur)
				m.channelEditValues[field.key] = string(runes[:len(runes)-1])
			}
		}
		return ""

	case "ctrl+v":
		if field.ftype == fieldText {
			if clip := readClipboard(); clip != "" {
				m.channelEditValues[field.key] += clip
			}
		}
		return ""

	default:
		if field.ftype == fieldText && len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			if key[0] == 0x1b {
				return ""
			}
			m.channelEditValues[field.key] = m.channelEditValues[field.key] + key
		}
		return ""
	}
}

func (m *SettingsModel) cycleChannelOption(field channelFieldDef, direction int) {
	if field.options == nil {
		return
	}
	cur := m.channelEditValues[field.key]
	if cur == "" && len(field.options) > 0 {
		cur = field.options[0]
	}
	idx := 0
	for i, opt := range field.options {
		if opt == cur {
			idx = i
			break
		}
	}
	next := (idx + direction + len(field.options)) % len(field.options)
	m.channelEditValues[field.key] = field.options[next]
}

// --- Notification sub-editor ---

func (m *SettingsModel) handleNotifKey(key string) string {
	if m.notifMode == "edit" {
		return m.handleNotifEditKey(key)
	}

	// List mode
	if m.notifDeleteConf {
		if key == "d" || key == "D" {
			if m.notifIndex < len(m.notifications) {
				m.notifications = append(m.notifications[:m.notifIndex], m.notifications[m.notifIndex+1:]...)
				if m.notifIndex >= len(m.notifications) && m.notifIndex > 0 {
					m.notifIndex--
				}
				m.dirty = true
			}
			m.notifDeleteConf = false
		} else {
			m.notifDeleteConf = false
		}
		return ""
	}

	switch key {
	case keyEsc:
		return m.handleClose()
	case keyUp:
		if m.notifIndex > 0 {
			m.notifIndex--
		}
	case keyDown:
		if m.notifIndex < len(m.notifications)-1 {
			m.notifIndex++
		}
	case keyEnter:
		if len(m.notifications) > 0 {
			n := m.notifications[m.notifIndex]
			m.notifEditURL = n.URL
			m.notifEditEvents = make(map[string]bool)
			if len(n.Events) == 0 {
				// All events
				for _, e := range allNotifEvents {
					m.notifEditEvents[e] = true
				}
			} else {
				for _, e := range n.Events {
					m.notifEditEvents[e] = true
				}
			}
			m.notifEditFocus = 0
			m.notifMode = "edit"
		}
	case "a", "A":
		m.notifEditURL = ""
		m.notifEditEvents = make(map[string]bool)
		for _, e := range allNotifEvents {
			m.notifEditEvents[e] = true
		}
		m.notifEditFocus = 0
		m.notifIndex = len(m.notifications)
		m.notifMode = "edit"
	case "d", "D":
		if len(m.notifications) > 0 {
			m.notifDeleteConf = true
		}
	case "left":
		if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
		}
	case "right":
		if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
		}
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '8' {
			idx := int(key[0]-'0') - 1
			if idx >= 0 && idx < len(sections) {
				m.switchSection(idx)
			}
		}
	}
	return ""
}

func (m *SettingsModel) handleNotifEditKey(key string) string {
	totalItems := 1 + len(allNotifEvents) // URL + events

	switch key {
	case keyEsc:
		m.notifMode = "list"
		return ""

	case keyEnter:
		// Save
		if strings.TrimSpace(m.notifEditURL) == "" {
			return ""
		}
		var events []string
		enabledCount := 0
		for _, e := range allNotifEvents {
			if m.notifEditEvents[e] {
				enabledCount++
				events = append(events, e)
			}
		}
		n := config.NotificationConfig{URL: m.notifEditURL}
		if enabledCount < len(allNotifEvents) {
			n.Events = events
		}
		if m.notifIndex < len(m.notifications) {
			m.notifications[m.notifIndex] = n
		} else {
			m.notifications = append(m.notifications, n)
		}
		m.dirty = true
		m.status = saveIdle
		m.notifMode = "list"
		return ""

	case keyUp:
		if m.notifEditFocus > 0 {
			m.notifEditFocus--
		}
		return ""

	case keyDown:
		if m.notifEditFocus < totalItems-1 {
			m.notifEditFocus++
		}
		return ""

	case " ":
		// Toggle event checkbox
		if m.notifEditFocus > 0 {
			eventIdx := m.notifEditFocus - 1
			if eventIdx < len(allNotifEvents) {
				event := allNotifEvents[eventIdx]
				m.notifEditEvents[event] = !m.notifEditEvents[event]
			}
		}
		return ""

	case "backspace":
		if m.notifEditFocus == 0 {
			if len(m.notifEditURL) > 0 {
				runes := []rune(m.notifEditURL)
				m.notifEditURL = string(runes[:len(runes)-1])
			}
		}
		return ""

	case "ctrl+v":
		if m.notifEditFocus == 0 {
			if clip := readClipboard(); clip != "" {
				m.notifEditURL += clip
			}
		}
		return ""

	default:
		if m.notifEditFocus == 0 && len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			if key[0] == 0x1b {
				return ""
			}
			m.notifEditURL += key
		}
		return ""
	}
}

// --- Security sub-editor ---

func (m *SettingsModel) handleSecurityKey(key string) string {
	switch m.secMode {
	case securityStatus:
		return m.handleSecurityStatusKey(key)
	case securitySet:
		return m.handleSecuritySetKey(key)
	case securityRemove:
		return m.handleSecurityRemoveKey(key)
	}
	return ""
}

func (m *SettingsModel) hasPassword() bool {
	return m.cfg != nil && m.cfg.Network.PasswordHash != ""
}

func (m *SettingsModel) handleSecurityStatusKey(key string) string {
	// Security status mode is no longer used as a standalone section.
	// S/R is handled directly in the Network section's HandleKey.
	// This function handles Esc from security sub-modes back to field mode.
	switch key {
	case keyEsc:
		m.secMode = securityStatus
		return ""
	}
	return ""
}

func (m *SettingsModel) handleSecuritySetKey(key string) string {
	// Determine visible fields
	fieldCount := 2 // new, confirm
	if m.hasPassword() {
		fieldCount = 3 // current, new, confirm
	}

	switch key {
	case keyEsc:
		m.secMode = securityStatus
		return ""

	case keyEnter:
		m.handleSetPassword()
		return ""

	case keyUp:
		if m.secFieldIndex > 0 {
			m.secFieldIndex--
		}
		return ""

	case keyDown, keyTab:
		if m.secFieldIndex < fieldCount-1 {
			m.secFieldIndex++
		}
		return ""

	case "backspace":
		target := m.secActiveField()
		if target != nil && len(*target) > 0 {
			runes := []rune(*target)
			*target = string(runes[:len(runes)-1])
		}
		return ""

	case "shift+tab":
		// Backward navigation (match TS Shift+Tab)
		if m.secFieldIndex > 0 {
			m.secFieldIndex--
		}
		return ""

	case "ctrl+v":
		target := m.secActiveField()
		if target != nil {
			if clip := readClipboard(); clip != "" {
				*target += clip
			}
		}
		return ""

	default:
		if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			if key[0] == 0x1b {
				return ""
			}
			target := m.secActiveField()
			if target != nil {
				*target += key
			}
		}
		return ""
	}
}

func (m *SettingsModel) secActiveField() *string {
	if m.hasPassword() {
		switch m.secFieldIndex {
		case 0:
			return &m.secCurrentPw
		case 1:
			return &m.secNewPw
		case 2:
			return &m.secConfirmPw
		}
	} else {
		switch m.secFieldIndex {
		case 0:
			return &m.secNewPw
		case 1:
			return &m.secConfirmPw
		}
	}
	return nil
}

func (m *SettingsModel) handleSetPassword() {
	// Verify current password if exists
	if m.hasPassword() {
		if m.secCurrentPw == "" {
			m.secMessage = "Current password required"
			m.secMessageColor = ColorRed
			return
		}
		if m.OnVerifyPassword != nil && !m.OnVerifyPassword(m.secCurrentPw, m.cfg.Network.PasswordHash) {
			m.secMessage = "Current password is incorrect"
			m.secMessageColor = ColorRed
			return
		}
	}

	if m.secNewPw == "" {
		m.secMessage = "New password required"
		m.secMessageColor = ColorRed
		return
	}
	if m.secNewPw != m.secConfirmPw {
		m.secMessage = "Passwords do not match"
		m.secMessageColor = ColorRed
		return
	}

	// Hash and save
	if m.OnHashPassword != nil {
		hash := m.OnHashPassword(m.secNewPw)
		if hash == "" {
			m.secMessage = "Failed to hash password"
			m.secMessageColor = ColorRed
			return
		}
		m.cfg.Network.PasswordHash = hash
		if m.OnSave != nil {
			m.OnSave(m.cfg)
		}
		m.secMessage = "Password set successfully"
		m.secMessageColor = ColorGreen
		m.secMode = securityStatus
	} else {
		m.secMessage = "Password hashing not available"
		m.secMessageColor = ColorRed
	}
}

func (m *SettingsModel) handleSecurityRemoveKey(key string) string {
	switch key {
	case keyEsc:
		m.secMode = securityStatus
		return ""

	case keyEnter:
		m.handleRemovePassword()
		return ""

	case "backspace":
		if len(m.secRemovePw) > 0 {
			runes := []rune(m.secRemovePw)
			m.secRemovePw = string(runes[:len(runes)-1])
		}
		return ""

	case "ctrl+v":
		if text := readClipboard(); text != "" {
			m.secRemovePw += text
		}
		return ""

	default:
		if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			if key[0] == 0x1b {
				return ""
			}
			m.secRemovePw += key
		}
		return ""
	}
}

func (m *SettingsModel) handleRemovePassword() {
	if !m.hasPassword() {
		m.secMessage = "No password is set"
		m.secMessageColor = ColorRed
		return
	}

	if m.OnVerifyPassword != nil && !m.OnVerifyPassword(m.secRemovePw, m.cfg.Network.PasswordHash) {
		m.secMessage = "Current password is incorrect"
		m.secMessageColor = ColorRed
		return
	}

	// Remove password
	networkReset := m.cfg.Network.NetworkAccess == "external"
	m.cfg.Network.PasswordHash = ""
	if networkReset {
		m.cfg.Network.NetworkAccess = "localhost"
		m.values["network_access"] = "localhost"
	}
	if m.OnSave != nil {
		m.OnSave(m.cfg)
	}
	if networkReset {
		m.secMessage = "Password removed, network access reset to localhost"
	} else {
		m.secMessage = "Password removed"
	}
	m.secMessageColor = ColorGreen
	m.secMode = securityStatus
}

// --- Rendering ---

// View renders the settings panel.
func (m *SettingsModel) View() string {
	if !m.visible {
		return ""
	}

	// Restart overlay
	if m.showRestartOverlay {
		return m.renderRestartOverlay()
	}

	boxW := min(78, m.width)
	if boxW < 40 {
		boxW = 40
	}
	innerW := boxW - 4
	h := m.height - 2
	if h < 10 {
		h = 10
	}

	var content strings.Builder

	// Header: "Settings — General | Downloader | ..."
	content.WriteString(m.renderHeader(innerW))
	content.WriteString("\n")

	// Divider
	content.WriteString(DimStyle.Render(strings.Repeat("\u2500", innerW)))
	content.WriteString("\n")

	sec := sections[m.sectionIndex]

	// Section content
	switch sec.name {
	case "Network":
		// Network fields + embedded security sub-editor
		if m.secMode != securityStatus {
			content.WriteString(m.renderSecurity(innerW))
		} else {
			content.WriteString(m.renderFields(sec, innerW, h-12))
			content.WriteString("\n")
			content.WriteString(m.renderSecurityCompact(innerW))
		}
	case "Channels":
		content.WriteString(m.renderChannels(innerW, h-8))
	case "Integrations":
		content.WriteString(m.renderNotifications(innerW, h-8))
	default:
		content.WriteString(m.renderFields(sec, innerW, h-8))
	}

	// Status line
	content.WriteString("\n")
	switch m.status {
	case saveSaving:
		content.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render("Saving..."))
	case saveSaved:
		content.WriteString(lipgloss.NewStyle().Foreground(ColorGreen).Render("Saved"))
	case saveError:
		content.WriteString(lipgloss.NewStyle().Foreground(ColorRed).Render(m.errorMsg))
	default:
		if m.dirty {
			content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Faint(true).Render("Unsaved changes"))
		}
	}

	// Hints
	content.WriteString("\n")
	hintLeft := DimStyle.Render("Esc: Close")
	hintRight := m.renderHintText()
	hintGap := innerW - runewidth.StringWidth("Esc: Close") - runewidth.StringWidth(hintRight)
	if hintGap < 1 {
		hintGap = 1
	}
	content.WriteString(hintLeft + strings.Repeat(" ", hintGap) + DimStyle.Render(hintRight))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(innerW).
		Height(h).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}

func (m *SettingsModel) renderHeader(w int) string {
	left := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Settings") +
		DimStyle.Render(" \u2500 ")

	var tabParts []string
	for i, sec := range sections {
		if i > 0 {
			tabParts = append(tabParts, DimStyle.Render(" \u2502 "))
		}
		if i == m.sectionIndex {
			tabParts = append(tabParts, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(sec.name))
		} else {
			tabParts = append(tabParts, DimStyle.Render(sec.name))
		}
	}
	tabs := strings.Join(tabParts, "")

	right := DimStyle.Render(fmt.Sprintf("%d/%d", m.sectionIndex+1, len(sections)))

	// Build full header
	return left + tabs + " " + right
}

func (m *SettingsModel) renderHintText() string {
	sec := sections[m.sectionIndex]
	if sec.name == "Network" && m.secMode != securityStatus {
		return "Esc: Back  \u2191/\u2193/Tab: Navigate  Enter: Save"
	}
	if m.isFieldSection() {
		field := sec.fields[m.fieldIndex]
		toggle := "Section"
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			toggle = "Toggle"
		}
		hint := fmt.Sprintf("\u2190/\u2192: %s  \u2191/\u2193: Navigate  1-8: Jump", toggle)
		if sec.name == "Network" {
			hint += "  S: Password"
		}
		return hint
	}
	return "\u2190/\u2192: Section  \u2191/\u2193: Navigate  A: Add  Enter: Edit  D: Delete  1-8: Jump"
}

func (m *SettingsModel) renderFields(sec settingsSection, w, maxH int) string {
	if sec.fields == nil {
		return DimStyle.Render("No settings in this section")
	}

	// Compute max label width for alignment
	maxLabel := 0
	for _, fd := range sec.fields {
		if len(fd.label) > maxLabel {
			maxLabel = len(fd.label)
		}
	}
	padWidth := maxLabel + 2

	var lines []string
	end := m.scrollOffset + maxH
	if end > len(sec.fields) {
		end = len(sec.fields)
	}

	for i := m.scrollOffset; i < end; i++ {
		fd := sec.fields[i]
		selected := i == m.fieldIndex
		isChanged := m.values[fd.key] != m.originalValues[fd.key]
		needsRestart := isChanged && restartRequiredKeys[fd.key]

		// Prefix
		prefix := "  "
		if selected {
			prefix = "> "
		}

		// Label
		labelStr := padRight(fd.label, padWidth)
		labelStyle := lipgloss.NewStyle().Foreground(ColorGray)
		if selected {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}

		// Value
		var value string
		switch fd.ftype {
		case fieldToggle:
			value = renderToggle(m.values[fd.key], selected)
		case fieldCycle:
			value = renderCycleOptions(fd.options, m.values[fd.key], selected)
		default:
			rawVal := m.values[fd.key]
			// For text/number fields: show tail of value when it overflows,
			// so the user always sees near the cursor.
			cursorStr := ""
			if selected {
				cursorStr = "_"
			}
			indicatorW := 0
			if isChanged || needsRestart {
				indicatorW = 2 // " *"
			}
			valueMaxW := w - len(prefix) - padWidth - indicatorW
			if valueMaxW < 5 {
				valueMaxW = 5
			}
			display := rawVal + cursorStr
			if runewidth.StringWidth(display) > valueMaxW {
				// Show tail: "…" + last (valueMaxW-1) chars
				displayRunes := []rune(display)
				tailW := valueMaxW - 1 // room for ellipsis
				start := len(displayRunes)
				w2 := 0
				for start > 0 {
					rw := runewidth.RuneWidth(displayRunes[start-1])
					if w2+rw > tailW {
						break
					}
					w2 += rw
					start--
				}
				display = "\u2026" + string(displayRunes[start:])
			}
			if selected {
				// Re-split to style the cursor
				if strings.HasSuffix(display, "_") {
					value = display[:len(display)-1] + lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
				} else {
					value = display
				}
			} else {
				value = display
			}
		}

		// Change indicator
		indicator := ""
		if needsRestart {
			indicator = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Faint(true).Render(" *")
		} else if isChanged {
			indicator = lipgloss.NewStyle().Foreground(ColorGreen).Faint(true).Render(" *")
		}

		prefixStyle := lipgloss.NewStyle()
		if selected {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		line := prefixStyle.Render(prefix) + labelStyle.Render(labelStr) + value + indicator
		lines = append(lines, line)
	}

	// Info area: divider + help text for focused field
	if m.fieldIndex < len(sec.fields) {
		fd := sec.fields[m.fieldIndex]
		isChanged := m.values[fd.key] != m.originalValues[fd.key]
		needsRestart := isChanged && restartRequiredKeys[fd.key]

		lines = append(lines, "")
		lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", w-4)))

		var infoParts []string
		if fd.help != "" {
			infoParts = append(infoParts, fd.help)
		}
		if needsRestart {
			infoParts = append(infoParts, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("[restart required]"))
		} else if isChanged {
			infoParts = append(infoParts, lipgloss.NewStyle().Foreground(ColorGreen).Render("[modified]"))
		}

		if len(infoParts) > 0 {
			lines = append(lines, DimStyle.Render(strings.Join(infoParts, "  ")))
		}
	}

	return strings.Join(lines, "\n")
}

func renderToggle(value string, focused bool) string {
	if value == "Yes" {
		return lipgloss.NewStyle().Foreground(ColorGreen).Render("Yes") + DimStyle.Render(" / No")
	}
	return DimStyle.Render("Yes / ") + lipgloss.NewStyle().Foreground(ColorRed).Render("No")
}

func renderCycleOptions(options []string, selected string, focused bool) string {
	var parts []string
	for _, opt := range options {
		if strings.EqualFold(opt, selected) {
			color := ColorWhite
			if focused {
				color = ColorCyan
			}
			parts = append(parts, lipgloss.NewStyle().Foreground(color).Bold(true).Render("["+opt+"]"))
		} else {
			parts = append(parts, DimStyle.Render(opt))
		}
	}
	return strings.Join(parts, DimStyle.Render(" / "))
}

func (m *SettingsModel) renderChannels(w, maxH int) string {
	if m.channelMode == "edit" {
		return m.renderChannelEdit(w)
	}

	var lines []string

	// Action bar
	actionBar := DimStyle.Render("A: Add  Enter: Edit  D: Delete  ")
	if m.channelDeleteConf {
		actionBar += lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("Press D again to confirm delete")
	}
	lines = append(lines, actionBar)

	if len(m.channels) == 0 {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  No channels configured. Press A to add one."))
		return strings.Join(lines, "\n")
	}

	for i, ch := range m.channels {
		selected := i == m.channelIndex

		prefix := "  "
		if selected {
			prefix = "> "
		}

		// Platform icon
		platformIcon := "YT"
		platformColor := ColorRed
		if ch.GetPlatform() == "twitch" {
			platformIcon = "TW"
			platformColor = ColorCookies
		}
		platStr := lipgloss.NewStyle().Foreground(platformColor).Render("[" + platformIcon + "]")

		// Name + ID
		name := ch.Name
		if name == "" {
			name = ch.ID
		}
		nameColor := ColorWhite
		if selected {
			nameColor = ColorCyan
		}
		enabled := ch.IsEnabled()
		nameStyle := lipgloss.NewStyle().Foreground(nameColor)
		if !enabled {
			nameStyle = nameStyle.Faint(true)
		}

		idStr := DimStyle.Render(truncateString(ch.ID, 24))

		line := prefix + platStr + " " + nameStyle.Render(truncateString(name, 20)) + " " + idStr
		if !enabled {
			line += lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Faint(true).Render(" (disabled)")
		}
		terms := ch.Terms.Simple
		if terms != "" {
			line += DimStyle.Render(" filter: "+truncateString(terms, 20))
		}

		if selected {
			line = lipgloss.NewStyle().Render(line)
		}

		lines = append(lines, line)
		if len(lines) >= maxH {
			break
		}
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderChannelEdit(w int) string {
	var lines []string

	title := "Edit Channel"
	if m.channelIndex >= len(m.channels) {
		title = "Add Channel"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))

	fields := m.visibleChannelFields()
	maxLabel := 0
	for _, f := range fields {
		if len(f.label) > maxLabel {
			maxLabel = len(f.label)
		}
	}
	padW := maxLabel + 2

	for idx, field := range fields {
		isFocused := idx == m.channelEditField
		val := m.channelEditValues[field.key]
		if val == "" && (field.ftype == fieldToggle || field.ftype == fieldCycle) && len(field.options) > 0 {
			val = field.options[0]
		}

		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelStyle := lipgloss.NewStyle().Foreground(ColorGray)
		if isFocused {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		prefixStyle := lipgloss.NewStyle()
		if isFocused {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		var value string
		switch field.ftype {
		case fieldToggle:
			value = renderToggle(val, isFocused)
		case fieldCycle:
			value = renderCycleOptions(field.options, val, isFocused)
		default:
			// Tail-visible for text fields in edit mode
			cursorStr := ""
			if isFocused {
				cursorStr = "_"
			}
			display := val + cursorStr
			valueMaxW := w - len(prefix) - padW - 2
			if valueMaxW < 5 {
				valueMaxW = 5
			}
			if runewidth.StringWidth(display) > valueMaxW {
				displayRunes := []rune(display)
				tailW := valueMaxW - 1
				start := len(displayRunes)
				w2 := 0
				for start > 0 {
					rw := runewidth.RuneWidth(displayRunes[start-1])
					if w2+rw > tailW {
						break
					}
					w2 += rw
					start--
				}
				display = "\u2026" + string(displayRunes[start:])
			}
			if isFocused && strings.HasSuffix(display, "_") {
				value = display[:len(display)-1] + lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
			} else {
				value = display
			}
		}

		line := prefixStyle.Render(prefix) + labelStyle.Render(padRight(field.label, padW)) + value
		if field.help != "" && isFocused {
			line += DimStyle.Render(" (" + field.help + ")")
		}
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderNotifications(w, maxH int) string {
	if m.notifMode == "edit" {
		return m.renderNotifEdit(w)
	}

	var lines []string

	actionBar := DimStyle.Render("A: Add  Enter: Edit  D: Delete  ")
	if m.notifDeleteConf {
		actionBar += lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("Press D again to confirm delete")
	}
	lines = append(lines, actionBar)

	if len(m.notifications) == 0 {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  No webhooks configured. Press A to add one."))
		return strings.Join(lines, "\n")
	}

	for i, n := range m.notifications {
		selected := i == m.notifIndex
		prefix := "  "
		if selected {
			prefix = "> "
		}

		urlDisplay := truncateString(n.URL, 50)
		eventCount := len(allNotifEvents)
		if len(n.Events) > 0 {
			eventCount = len(n.Events)
		}

		nameStyle := lipgloss.NewStyle()
		if selected {
			nameStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		line := prefix + nameStyle.Render(urlDisplay) +
			DimStyle.Render(fmt.Sprintf(" (%d/%d events)", eventCount, len(allNotifEvents)))

		lines = append(lines, line)
		if len(lines) >= maxH {
			break
		}
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderNotifEdit(w int) string {
	var lines []string

	title := "Edit Notification"
	if m.notifIndex >= len(m.notifications) {
		title = "Add Notification"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))

	// URL field
	urlFocused := m.notifEditFocus == 0
	urlPrefix := "  "
	if urlFocused {
		urlPrefix = "> "
	}
	urlLabel := "Webhook URL"
	urlLabelStyle := DimStyle
	if urlFocused {
		urlLabelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	}
	urlVal := m.notifEditURL
	// Tail-visible for URL field
	cursorStr := ""
	if urlFocused {
		cursorStr = "_"
	}
	urlDisplay := urlVal + cursorStr
	urlMaxW := w - len(urlPrefix) - 16 - 2
	if urlMaxW < 10 {
		urlMaxW = 10
	}
	if runewidth.StringWidth(urlDisplay) > urlMaxW {
		urlRunes := []rune(urlDisplay)
		tailW := urlMaxW - 1
		start := len(urlRunes)
		w2 := 0
		for start > 0 {
			rw := runewidth.RuneWidth(urlRunes[start-1])
			if w2+rw > tailW {
				break
			}
			w2 += rw
			start--
		}
		urlDisplay = "\u2026" + string(urlRunes[start:])
	}
	if urlFocused && strings.HasSuffix(urlDisplay, "_") {
		urlVal = urlDisplay[:len(urlDisplay)-1] + lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
	} else {
		urlVal = urlDisplay
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(func() lipgloss.Color {
		if urlFocused {
			return ColorCyan
		}
		return ColorWhite
	}()).Render(urlPrefix)+urlLabelStyle.Render(padRight(urlLabel, 16))+urlVal)

	// Events header
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("  Events (Space to toggle):"))

	// Event checkboxes
	for i, event := range allNotifEvents {
		isFocused := m.notifEditFocus == i+1
		isChecked := m.notifEditEvents[event]

		prefix := "  "
		if isFocused {
			prefix = "> "
		}

		checkStr := " "
		checkColor := ColorGray
		if isChecked {
			checkStr = "x"
			checkColor = ColorGreen
		}

		eventStyle := lipgloss.NewStyle().Foreground(ColorWhite)
		if isFocused {
			eventStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		lines = append(lines, lipgloss.NewStyle().Foreground(func() lipgloss.Color {
			if isFocused {
				return ColorCyan
			}
			return ColorWhite
		}()).Render(prefix)+
			lipgloss.NewStyle().Foreground(checkColor).Render("["+checkStr+"]")+
			eventStyle.Render(" "+event))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecurity(w int) string {
	switch m.secMode {
	case securitySet:
		return m.renderSecuritySet(w)
	case securityRemove:
		return m.renderSecurityRemove(w)
	default:
		return m.renderSecurityStatus(w)
	}
}

func (m *SettingsModel) renderSecurityStatus(w int) string {
	var lines []string

	// Password status
	lines = append(lines, "")
	if m.hasPassword() {
		lines = append(lines, "  Password: "+lipgloss.NewStyle().Foreground(ColorGreen).Bold(true).Render("Set"))
	} else {
		lines = append(lines, "  Password: "+DimStyle.Render("Not set"))
	}

	lines = append(lines, "")

	// Actions
	actionLabel := "Set"
	if m.hasPassword() {
		actionLabel = "Change"
	}
	actionLine := DimStyle.Render("  S: "+actionLabel+" password")
	if m.hasPassword() {
		actionLine += DimStyle.Render("  R: Remove password")
	}
	lines = append(lines, actionLine)

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

// renderSecurityCompact renders a compact password status below Network fields.
func (m *SettingsModel) renderSecurityCompact(w int) string {
	var lines []string
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", w-4)))

	status := DimStyle.Render("Not set")
	if m.hasPassword() {
		status = lipgloss.NewStyle().Foreground(ColorGreen).Render("Set")
	}
	lines = append(lines, "  Password: "+status)

	actionLabel := "Set"
	if m.hasPassword() {
		actionLabel = "Change"
	}
	actionLine := DimStyle.Render("  S: "+actionLabel+" password")
	if m.hasPassword() {
		actionLine += DimStyle.Render("  R: Remove")
	}
	lines = append(lines, actionLine)

	if m.secMessage != "" {
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecuritySet(w int) string {
	var lines []string

	title := "Set Password"
	if m.hasPassword() {
		title = "Change Password"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))
	lines = append(lines, "")

	type pwField struct {
		label string
		value string
	}
	var fields []pwField
	if m.hasPassword() {
		fields = append(fields, pwField{"Current password", m.secCurrentPw})
	}
	fields = append(fields, pwField{"New password", m.secNewPw})
	fields = append(fields, pwField{"Confirm password", m.secConfirmPw})

	maxLabel := 0
	for _, f := range fields {
		if len(f.label) > maxLabel {
			maxLabel = len(f.label)
		}
	}
	padW := maxLabel + 2

	for idx, f := range fields {
		isFocused := idx == m.secFieldIndex
		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelStyle := DimStyle
		if isFocused {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		prefixStyle := lipgloss.NewStyle()
		if isFocused {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		masked := strings.Repeat("*", len(f.value))
		cursor := ""
		if isFocused {
			cursor = lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
		}

		lines = append(lines, prefixStyle.Render(prefix)+labelStyle.Render(padRight(f.label, padW))+masked+cursor)
	}

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecurityRemove(w int) string {
	var lines []string

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorRed).Bold(true).Render("Remove Password")+
		DimStyle.Render(" (Enter: confirm, Esc: cancel)"))
	lines = append(lines, "")

	if m.cfg != nil && m.cfg.Network.NetworkAccess == "external" {
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
			"Warning: Network access will be reset to localhost"))
	}

	// Password field
	labelStyle := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	masked := strings.Repeat("*", len(m.secRemovePw))
	cursor := lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
	lines = append(lines, "> "+labelStyle.Render(padRight("Current password", 20))+masked+cursor)

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderRestartOverlay() string {
	w := min(50, m.width-4)
	h := 10

	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true).
		Render("Restart Required") + "\n\n")
	content.WriteString("Some settings require a restart to take effect:\n")
	content.WriteString(DimStyle.Render("port, network access, database path, log settings") + "\n\n")

	content.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render("Enter: Restart now"))
	content.WriteString("  ")
	content.WriteString(DimStyle.Render("Esc: Close without restart"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#f1c40f")).
		Width(w).
		Height(h).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}

func boolToDisplay(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

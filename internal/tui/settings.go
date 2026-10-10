package tui

import (
	"fmt"
	"image/color"
	"maps"
	"math"
	"net"
	"reflect"
	"slices"
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
// there and then whether the headless-browser refresh timer starts at all — it
// reads auto_enabled before any of this UI exists. So an operator acting on a
// "your cookies are dead" notification can turn the setting on, watch it save,
// and have no timer until they restart. Keep them here until those reads are
// genuinely live.
//
// What auto_enabled does NOT need a restart for is the manual triggers: R F and
// the dashboard's shift+click read it live and pick the browser or the
// browser-free profile import accordingly. And the profile DIRECTORY is no
// longer part of the start condition — run() used to os.Stat it at boot, which
// meant completing setup at runtime left the timer unstarted with nothing
// saying so; the loop asks per tick now (periodicRefreshHasSource).
//
// Kept in step with RESTART_REQUIRED_FIELDS in web/public/modules/settings.js;
// the two lists are pinned against each other by TestRestartRequiredListsAgree.
var restartRequiredKeys = map[string]bool{
	"port":                  true,
	"network_access":        true,
	"https_enabled":         true,
	"tls_cert_path":         true,
	"tls_key_path":          true,
	"database_path":         true,
	"log_file_path":         true,
	"log_max_file_size":     true,
	"log_max_files":         true,
	"cookie_file":           true,
	"refresh_interval":      true,
	"auto_enabled":          true,
	"browser_profile_dir":   true,
	"probe_targets":         true,
	"sidecar_hard_limit_mb": true,
	"use_sidecar":           true,
}

// resolutionPresets are the max_video_resolution values the arrow keys step
// through on the Downloader section's Max resolution row, in ascending numeric
// order with the unbounded sentinel first. The row is still a NUMBER row, so
// anything off this ladder can be typed and is kept — the presets are a
// shortcut, not a constraint. Kept in step with RESOLUTION_PRESETS in
// web/public/modules/settings.js.
var resolutionPresets = []string{"0", "480", "720", "1080", "1440", "2160", "4320"}

// resolutionPreview names the preset a stored cap corresponds to, on the dim
// line under the row. An empty or unparseable value previews nothing (the
// operator is mid-edit); a parseable value off the ladder previews as Custom,
// so an accidental 12800 is visible as one.
func resolutionPreview(value string) string {
	v := strings.TrimSpace(value)
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return ""
	}
	switch n {
	case 0:
		// Byte-identical to the Web option label in
		// web/public/modules/settings.js / index.html.
		return "Unbounded — always the largest"
	case 480, 720, 1080, 1440:
		return strconv.Itoa(n) + "p"
	case 2160:
		return "4K (2160)"
	case 4320:
		return "8K (4320)"
	default:
		return "Custom: " + strconv.Itoa(n)
	}
}

var sections = []settingsSection{
	{
		name: "Network",
		fields: []fieldDef{
			{"port", "Port", fieldNumber, nil, "web dashboard port, 1-65535 (requires restart)", nil},
			{"network_access", "Network access", fieldCycle, []string{"localhost", "lan", "external"}, "who can reach the dashboard; applies now, but leaving localhost needs a restart to listen on the network", nil},
			{"https_enabled", "HTTPS enabled", fieldToggle, nil, "serve over HTTPS, needs TLS cert + key (requires restart)", nil},
			{"tls_cert_path", "TLS cert path", fieldText, nil, "PEM format certificate file (requires restart)", nil},
			{"tls_key_path", "TLS key path", fieldText, nil, "PEM format private key file (requires restart)", nil},
			{"trust_forwarded_proto", "Trust forwarded proto", fieldToggle, nil, "ONLY enable behind a TLS-terminating reverse proxy that strips client X-Forwarded-Proto", nil},
			{"trusted_proxies", "Trusted proxies", fieldText, nil, "comma-separated reverse-proxy IPs/CIDRs whose X-Forwarded-For is honored — leave empty unless behind a proxy you control", nil},
			{"public_url", "Public dashboard URL", fieldText, nil, "address you type to reach this dashboard (e.g. https://moombox.example.com); webhook embeds link here, blank = YouTube/Twitch. Also trusted as the dashboard's own address: on localhost/lan a page on its port, from any local or private address, may drive the dashboard; on external, local browsers may open it by this host", nil},
			{"probe_targets", "Connectivity probe targets", fieldText, nil, "comma-separated host:port TCP targets raced to detect internet reachability; blank = keep the current targets (requires restart)", nil},
		},
	},
	{
		name: "Paths",
		fields: []fieldDef{
			{"database_path", "Database path", fieldText, nil, "SQLite database file (requires restart)", nil},
			{"log_file_path", "Log file path", fieldText, nil, "log output file (requires restart)", nil},
			{"output_directory", "Output directory", fieldText, nil, "where finished files go", nil},
			{"staging_directory", "Staging directory", fieldText, nil, "temp files during download; new jobs use a change at once, but staging under the old dir is no longer found — move it across", nil},
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
			{"feed_check_interval", "Feed check interval", fieldNumber, nil, "minutes, 1-1440; fractions allowed, e.g. 2.5 (default: 10)", nil},
			{"decapi_check_interval", "DECAPI check interval", fieldNumber, nil, "seconds, 15-3600 or empty for dynamic", nil},
			{"twitch_check_interval", "Twitch check interval", fieldNumber, nil, "seconds, 5-3600; empty = default 15 (±10% jitter always applied)", nil},
			{"hide_finished_age_days", "Hide finished after", fieldNumber, nil, "days, 0-365 (default: 30, 0 = archive immediately)", nil},
			{"probe_cooldown", "Probe cooldown", fieldNumber, nil, "seconds between re-probing the same video's YouTube metadata; 0 = disabled/probe every cycle; fractions allowed (default: 0, no max)", nil},
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
			{"max_video_resolution", "Max resolution", fieldNumber, resolutionPresets, "shorter edge in pixels; 0 = unbounded (e.g. 1080, 2160); ←/→ step the presets", resolutionPreview},
			{"num_parallel_downloads", "Parallel downloads", fieldNumber, nil, "VOD downloads at once across all channels; live streams never wait on this (default: 10)", nil},
			{"segment_workers", "Segment workers", fieldNumber, nil, "segments fetched at once within one download, not the number of concurrent downloads (default: 12, min 1, no max; above 16 raises bot-detection risk)", nil},
			{"reorder_buffer_mb", "Reorder buffer per job", fieldNumber, nil, "MB of out-of-order segments one download may hold in RAM; 0 = unbounded (default: 1024; 256 on arm64)", nil},
			{"reorder_budget_mb", "Reorder budget total", fieldNumber, nil, "MB every download's reorder buffer may hold between them; 0 = unbounded (default: 4096; 1024 on arm64)", nil},
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
			{"cookie_file", "Cookie file", fieldText, nil, "Netscape format cookies.txt (requires restart)", nil},
			{"active_youtube", "YouTube cookies", fieldToggle, nil, "YouTube cookie indicator in status bar", nil},
			{"active_twitch", "Twitch cookies", fieldToggle, nil, "Twitch cookie indicator in status bar", nil},
			{"auto_enabled", "Auto-cookie", fieldToggle, nil, "adds a slow headless-browser refresh timer (requires restart) + one browser retry on auth failure (applies now); R F imports either way", nil},
			{"acquisition", "Cookie source", fieldCycle, []string{"auto", "profile"}, "how a refresh gets cookies: auto = launch a browser when one is available, else read the profile; profile = never launch, read browser_profile_dir read-only (also allows a real browser's profile dir, which auto refuses). Takes effect immediately.", nil},
			{"browser_profile_dir", "Browser profile dir", fieldText, nil, "for auto-cookie browser data (requires restart)", nil},
			{"browser_path", "Browser path", fieldText, nil, "override (empty = auto-detect)", nil},
			{"browser_type", "Browser type", fieldText, nil, "firefox/chrome/brave/edge/etc. (required if path set)", nil},
			{"refresh_interval", "Refresh interval", fieldNumber, nil, "minutes (default: 360 = 6h) (requires restart)", nil},
			{"dpapi_fallback", "DPAPI fallback (Windows)", fieldToggle, nil, "fallback: read REAL browser cookies via DPAPI when CDP refresh fails (privacy: reads your signed-in session)", nil},
		},
	},
	{
		name: "Disk",
		fields: []fieldDef{
			{"disk_warn_percent", "Warning threshold", fieldNumber, nil, "% disk usage (default: 90)", nil},
			{"disk_critical_percent", "Critical threshold", fieldNumber, nil, "% disk usage for a critical alert; from it until 2 points below it backlog VODs wait in Queued, live and running downloads are not paused (default: 95)", nil},
		},
	},
	{
		name: "Updates",
		fields: []fieldDef{
			{"auto_check_updates", "Auto-check updates", fieldToggle, nil, "check GitHub on startup + daily; turning it on checks within a minute", nil},
		},
	},
	{
		name: "BotGuard Sidecar",
		fields: []fieldDef{
			{"use_sidecar", "Enable sidecar", fieldToggle, nil, "Node + JSDOM + bgutils-js for real BotGuard PO tokens and signature solving (default: on; when off, no PO tokens are minted and signature-ciphered formats are unavailable) (requires restart)", nil},
		},
	},
	{
		name: "Memory",
		fields: []fieldDef{
			{"go_soft_limit_mb", "Go soft limit (MB)", fieldNumber, nil, "soft cap; GC ramps up but no OOM (default: 256, 0 disables)", nil},
			{"sidecar_soft_limit_mb", "Sidecar soft limit (MB)", fieldNumber, nil, "RSS threshold to trigger V8 GC (default: 200, 0 disables)", nil},
			{"sidecar_hard_limit_mb", "Sidecar hard limit (MB)", fieldNumber, nil, "V8 --max-old-space-size; OOMs on hit, must exceed soft (default: 512, 0 = V8 default) (requires restart)", nil},
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

// The notification editor's focus map. Row 0 is the Webhook URL, and
// notifEditEventBase is the first index that names an event row. Every offset
// in the editor's renderer and mouse map goes through these, because three of
// them were hard-coded +1 before the Enabled and Mention rows existed.
const (
	notifEditURLRow      = 0
	notifEditEnabledRow  = 1
	notifEditMentionRow  = 2
	notifEditDeliveryRow = 3
	notifEditEventBase   = 4
)

// notifEventNameWidth is the column the per-event @ mention marker starts at,
// so the markers line up under each other across groups of differing name
// lengths. Derived rather than guessed — a longer event id widens the column
// instead of pushing one row's marker out of line.
var notifEventNameWidth = func() int {
	w := 0
	for _, e := range allNotifEvents {
		w = max(w, len(e)) // event ids are ASCII
	}
	return w
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
	{"output_directory", "Output directory", fieldText, nil, "per-channel override; blank = the global output directory", ""},
	{"archive_window_days", "Archive window (days)", fieldNumber, nil, "per-channel override, 1-3650; blank = global", ""},
	{"archive_slots", "Archive slots", fieldNumber, nil, "per-channel override, 1-100; blank = global", ""},
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
	headerTabStart     int // first section tab the header shows
	headerTabEnd       int // one past the last; 0 = header not rendered yet

	// Save status
	status   saveStatus
	errorMsg string

	// Config reference. cfg is the direct *MoomboxConfig pointer applyValues
	// writes its edited fields into; configStore exposes the same struct
	// with synchronisation for snapshot reads. Both wired together via
	// App.SetConfigStore.
	cfg         *config.MoomboxConfig
	configStore *config.Store

	// Callbacks
	//
	// OnSave persists cfg and returns the save error. The error is not
	// decoration: applyValues (and the two security commits) have already
	// written into the live *MoomboxConfig by the time it is called, so a
	// refused write is the moment the running process and config.toml
	// diverge — the caller reports it and rolls the live struct back
	// (CORE-4). A Settings save that changed nothing in the live config does
	// not call it.
	OnSave    func(cfg *config.MoomboxConfig) error
	OnRestart func()
	// OnRestartRequired fires when a settings save leaves a setting flagged
	// in restartRequiredKeys that the operator edited at a value other than
	// the one Open showed (settingsWrite.restart), regardless of whether the
	// user then triggers OnRestart from the modal or dismisses it. The App
	// flips a persistent banner-visible flag so the dismissal case doesn't
	// leave a config/runtime mismatch with no visual reminder. Audit
	// reports/tui.md #26.
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
	// channelsAtOpen is the channel list as Open copied it. A save writes
	// back only what the editor changed relative to it (mergeChannelEdits),
	// so a channel the dashboard added, disabled or removed while the
	// overlay was open is not reverted by a save of something else.
	channelsAtOpen []config.ChannelConfig

	// The channel-removal prompt and the choices it took
	// (settings_channel_removal.go).
	channelRemovalState

	// Notification sub-editor state
	notifIndex      int
	notifMode       string // "list" or "edit"
	notifEditURL    string
	notifEditEvents map[string]bool
	// notifEditDelivery is the per-TARGET delivery mode being edited,
	// "separate" or "edit". Deliberately not named notifEditMode: notifMode
	// above is the SUB-EDITOR's mode ("list" / "edit"), a different axis that
	// happens to share the word.
	notifEditDelivery string
	// notifEditFocus indexes the edit form's rows: 0 = Webhook URL,
	// 1 = Enabled, 2 = Mention, 3 = Delivery, notifEditEventBase+n = the nth
	// event row.
	notifEditFocus int
	// notifEditEnabled is the per-target mute. Absent in config means
	// delivering, so an existing target opens on IsEnabled().
	notifEditEnabled bool
	notifEditMention string
	// notifEditMentionEvents lights the per-event @ column. Seeded from the
	// target's RESOLVED mention filter, so an absent mention_events shows the
	// shipped defaults without ever having written them down.
	notifEditMentionEvents map[string]bool
	// notifEditMentionTouched records whether the operator changed any mention
	// toggle in this editing session. False keeps mention_events ABSENT on
	// save, so the target keeps following the shipped default list instead of
	// freezing today's defaults into their config file.
	notifEditMentionTouched bool
	// notifEditMentionExtras holds resolved mention ids this build's
	// EventGroups has no row for — a hand-written id, or a key a later arc
	// adds to the vocabulary before it adds the row. They cannot be unticked,
	// so they ride along on save instead of being dropped by a toggle on an
	// unrelated row — settings.js keeps them the same way, through its
	// resolved list.
	notifEditMentionExtras []string
	// notifEditScrollStart is the body scroll offset renderNotifEdit applied on
	// the last render (0 = not scrolled). Mouse click mapping reads it to map an
	// on-screen row back to the original (unscrolled) line.
	notifEditScrollStart int
	notifDeleteConf      bool
	notifications        []config.NotificationConfig
	// notificationsAtOpen is the target list as Open copied it, and
	// notifFrom names, for each entry of notifications, the index of the
	// notificationsAtOpen entry it was opened from (-1: the editor added it).
	// A save applies only what the editor changed relative to its own Open
	// copy, by that copy's identity (mergeNotificationEdits), so a target the
	// dashboard added, edited or removed while the overlay was open is not
	// reverted by a save of something else. Tracked by origin rather than by
	// URL because the URL is one of the fields the editor edits.
	notificationsAtOpen []config.NotificationConfig
	notifFrom           []int

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
	// afterClose is the action a prompted close hands back in place of
	// "close" once the prompt is answered with Save or Discard — today only
	// Ctrl+O's "open_ffmpeg", whose installer replaces the panel.
	afterClose string

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
	m.afterClose = ""
	m.buttonFocus = -1
	m.resetChannelRemoval()

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
		m.channelsAtOpen = slices.Clone(c.Channels)

		// Notification editor
		m.notifIndex = 0
		m.notifMode = "list"
		m.notifDeleteConf = false
		m.notifications = make([]config.NotificationConfig, len(c.Notifications))
		copy(m.notifications, c.Notifications)
		m.notificationsAtOpen = slices.Clone(c.Notifications)
		m.notifFrom = openOrigins(len(c.Notifications))

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

// openOrigins is notifFrom for a list Open has just copied: every entry is
// its own Open copy.
func openOrigins(n int) []int {
	from := make([]int, n)
	for i := range from {
		from[i] = i
	}
	return from
}

// resyncFromLive makes the config just saved — this overlay's changes merged
// into everyone else's — both what the overlay shows and the base the next
// save diffs against, as a fresh Open would: the field values, the channel
// list and the notification targets. Copies, under the store's read lock:
// the live slices are shared with Snapshot readers.
func (m *SettingsModel) resyncFromLive() {
	read := func(c *config.MoomboxConfig) {
		m.loadValues(c)
		m.channels = slices.Clone(c.Channels)
		m.channelsAtOpen = slices.Clone(c.Channels)
		m.notifications = slices.Clone(c.Notifications)
		m.notificationsAtOpen = slices.Clone(c.Notifications)
		m.notifFrom = openOrigins(len(c.Notifications))
	}
	switch {
	case m.configStore != nil:
		m.configStore.Read(read)
	case m.cfg != nil:
		read(m.cfg)
	default:
		return
	}
	m.originalValues = maps.Clone(m.values)
	if m.channelIndex >= len(m.channels) {
		m.channelIndex = max(0, len(m.channels)-1)
	}
	if m.notifIndex >= len(m.notifications) {
		m.notifIndex = max(0, len(m.notifications)-1)
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
	m.ensureFieldVisible()
}

func (m *SettingsModel) loadValues(cfg *config.MoomboxConfig) {
	loadSettingsValues(m.values, cfg)
}

// loadSettingsValues fills v with cfg's value for every field the overlay
// shows, each in the form the overlay shows and edits it. A save reads the
// live config through it too, so the values it merges and validates, and
// the restart check's before and after, are in that same form.
func loadSettingsValues(v map[string]string, cfg *config.MoomboxConfig) {
	// Network
	v["port"] = strconv.Itoa(cfg.Network.Port)
	v["network_access"] = cfg.Network.NetworkAccess
	v["https_enabled"] = boolToDisplay(cfg.Network.HTTPSEnabled)
	v["tls_cert_path"] = cfg.Network.TLSCertPath
	v["tls_key_path"] = cfg.Network.TLSKeyPath
	v["trust_forwarded_proto"] = boolToDisplay(cfg.Network.TrustForwardedProto)
	v["trusted_proxies"] = strings.Join(cfg.Network.TrustedProxies, ", ")
	v["public_url"] = cfg.Network.PublicURL
	v["probe_targets"] = strings.Join(cfg.Connectivity.ProbeTargets, ", ")

	// Paths
	v["database_path"] = cfg.Paths.DatabasePath
	v["log_file_path"] = cfg.Paths.LogFilePath
	v["output_directory"] = cfg.Paths.OutputDirectory
	v["staging_directory"] = cfg.Paths.StagingDirectory
	v["ffmpeg_path"] = cfg.Paths.FfmpegPath

	// Logs
	v["log_level"] = cfg.Logs.LogLevel
	v["log_max_file_size"] = strconv.Itoa(cfg.Logs.LogMaxFileSize)
	v["log_max_files"] = strconv.Itoa(cfg.Logs.LogMaxFiles)

	// Monitors
	v["archive_window_days"] = strconv.Itoa(cfg.Monitors.ArchiveWindowDays)
	v["archive_slots"] = strconv.Itoa(cfg.Monitors.ArchiveSlots)
	// FormatFloat with -1 precision round-trips fractional values ("0.5"
	// stays "0.5", "30" stays "30") — %.0f and int() silently rounded them
	// away on EVERY save, touched field or not (CORE-7). The form carries
	// the CANONICAL FLOAT in the field's documented unit, never the string
	// form a config.toml may spell it with: ParseFlexDuration has already
	// turned "90s" into 1.5 minutes before the overlay sees the struct, and
	// FlexDuration.MarshalTOML writes it back with this exact spelling.
	v["feed_check_interval"] = strconv.FormatFloat(cfg.Monitors.FeedCheckInterval.Minutes(), 'f', -1, 64)
	if cfg.Monitors.DecapiCheckInterval != nil {
		v["decapi_check_interval"] = strconv.Itoa(*cfg.Monitors.DecapiCheckInterval)
	} else {
		v["decapi_check_interval"] = ""
	}
	if cfg.Monitors.TwitchCheckInterval != nil {
		v["twitch_check_interval"] = strconv.Itoa(*cfg.Monitors.TwitchCheckInterval)
	} else {
		v["twitch_check_interval"] = ""
	}
	v["hide_finished_age_days"] = strconv.FormatFloat(cfg.Monitors.HideFinishedAgeDays.Days(), 'f', -1, 64)
	v["probe_cooldown"] = strconv.FormatFloat(cfg.Monitors.ProbeCooldown.Value, 'f', -1, 64)
	// nil normalizes to true (default on) — matches config validation.
	v["membership_discovery"] = boolToDisplay(cfg.Monitors.MembershipDiscoveryEnabled())

	// Downloader
	v["output_template"] = cfg.Downloader.OutputTemplate
	v["max_video_resolution"] = strconv.Itoa(cfg.Downloader.MaxVideoResolution)
	v["num_parallel_downloads"] = strconv.Itoa(cfg.Downloader.NumParallelDownloads)
	v["segment_workers"] = strconv.Itoa(cfg.Downloader.SegmentWorkers)
	v["reorder_buffer_mb"] = strconv.Itoa(cfg.Downloader.ReorderBufferMB)
	v["reorder_budget_mb"] = strconv.Itoa(cfg.Downloader.ReorderBudgetMB)
	v["download_chat"] = boolToDisplay(cfg.Downloader.DownloadChat)
	v["prefer_60fps"] = boolToDisplay(cfg.Downloader.Prefer60fps)
	v["maximum_timeout"] = strconv.Itoa(cfg.Downloader.MaximumTimeout)
	v["interruption_timeout"] = strconv.FormatFloat(cfg.Downloader.InterruptionTimeout.Minutes(), 'f', -1, 64)
	v["incomplete_staging_expiry_days"] = strconv.FormatFloat(cfg.Downloader.IncompleteStagingExpiryDays.Days(), 'f', -1, 64)

	// Cookies
	v["cookie_file"] = cfg.Cookies.CookieFile
	ytActive, twActive := config.GetActivePlatforms(cfg)
	v["active_youtube"] = boolToDisplay(ytActive)
	v["active_twitch"] = boolToDisplay(twActive)
	v["auto_enabled"] = boolToDisplay(cfg.Cookies.AutoEnabled)
	v["browser_profile_dir"] = cfg.Cookies.BrowserProfileDir
	v["browser_path"] = cfg.Cookies.BrowserPath
	v["browser_type"] = cfg.Cookies.BrowserType
	v["refresh_interval"] = strconv.FormatFloat(cfg.Cookies.RefreshInterval.Minutes(), 'f', -1, 64)
	v["dpapi_fallback"] = boolToDisplay(cfg.Cookies.DpapiFallback)
	v["acquisition"] = cfg.Cookies.Acquisition

	// Disk
	v["disk_warn_percent"] = strconv.Itoa(cfg.Disk.WarnPercent)
	v["disk_critical_percent"] = strconv.Itoa(cfg.Disk.CriticalPercent)

	// Updates
	v["auto_check_updates"] = boolToDisplay(cfg.Updates.AutoCheckUpdates)

	// BotGuard sidecar
	v["use_sidecar"] = boolToDisplay(cfg.Bgutils.UseSidecar)

	// Memory
	v["go_soft_limit_mb"] = strconv.Itoa(cfg.Memory.GoSoftLimitMB)
	v["sidecar_soft_limit_mb"] = strconv.Itoa(cfg.Memory.SidecarSoftLimitMB)
	v["sidecar_hard_limit_mb"] = strconv.Itoa(cfg.Memory.SidecarHardLimitMB)
}

// settingsWrite is what one applyValues did to the live config.
type settingsWrite struct {
	// before is the live config as the write found it, copied under the
	// lock the write held: what a refused config.Save rolls back to.
	before config.MoomboxConfig
	// changed reports that the write changed the live config at all. A save
	// that changed nothing has nothing to put on disk.
	changed bool
	// restart reports that a restartRequiredKeys field the operator edited
	// now holds, in the live config after the write, a value other than the
	// one Open showed. The running process cannot have restarted since Open
	// (a restart closes the overlay), so a value that differs from Open's is
	// one config.toml now carries and the process may not run on — even when
	// the dashboard had already saved that same value and the write itself
	// changed nothing. A blank probe_targets keeps the stored list, so it
	// counts only when that list is not the one Open showed. A field the
	// operator did not edit never counts: a restart value the dashboard saved
	// is the dashboard's to prompt for.
	restart bool
}

// settingsParsed carries what validateSettingsValues parsed, so the write
// uses the values the checks passed rather than parsing them a second way.
type settingsParsed struct {
	port      int
	publicURL string
	flex      map[string]float64
}

// applyValues writes the overlay's own edits into the live config. Only a
// field the operator edited — one whose value now differs from the value
// Open loaded — is written; every other field keeps its live value, whatever
// the dashboard set it to while the overlay was open, and so does every
// channel and notification target the overlay did not touch
// (mergeChannelEdits, mergeNotificationEdits). Open copied every setting and
// a save used to write them all back, so a save of the log level reverted
// a parallel-downloads count, a webhook target or anything else changed on
// the dashboard meanwhile.
//
// All of it runs under the store's write lock, the checks included: the
// merged result — the edited fields over the live config as it stands now —
// is what every check validates and what is written, so no write can land
// between the two. It reports false, with the overlay's error set and
// nothing written, when a check refuses.
func (m *SettingsModel) applyValues() (settingsWrite, bool) {
	if m.cfg == nil {
		return settingsWrite{}, false
	}
	mu := m.configStore.RWMutex()
	mu.Lock()
	defer mu.Unlock()

	live := make(map[string]string, len(m.values))
	loadSettingsValues(live, m.cfg)
	edited := m.editedKeys()
	merged := maps.Clone(live)
	for _, k := range edited {
		merged[k] = m.values[k]
	}
	p, msg := validateSettingsValues(merged, m.cfg.Network.PasswordHash)
	if msg != "" {
		m.errorMsg = msg
		m.status = saveError
		return settingsWrite{}, false
	}

	w := settingsWrite{before: *m.cfg}
	for _, k := range edited {
		writeSettingsField(m.cfg, k, merged, p)
	}

	// Channels: only this editor's own changes, merged by ID into the list
	// as it stands NOW, under the store lock. Writing m.channels back whole
	// reverted every channel change the dashboard made while the overlay was
	// open — a save of the log level dropped a channel added there, and the
	// next sweep then pruned that channel's jobs and feed history as
	// departed. Untouched, the live list is left exactly as it is.
	if chs, changed := mergeChannelEdits(m.channelsAtOpen, m.channels, m.cfg.Channels); changed {
		m.cfg.Channels = chs
	}
	// Notification targets: the same rule, by each target's identity.
	if ns, changed := mergeNotificationEdits(m.notificationsAtOpen, m.notifications, m.notifFrom, m.cfg.Notifications); changed {
		m.cfg.Notifications = ns
	}

	after := make(map[string]string, len(live))
	loadSettingsValues(after, m.cfg)
	for _, k := range edited {
		if restartRequiredKeys[k] && after[k] != m.originalValues[k] {
			w.restart = true
			break
		}
	}
	w.changed = !reflect.DeepEqual(w.before, *m.cfg)
	return w, true
}

// editedKeys returns, sorted, the fields whose value differs from the one
// Open loaded: the fields the operator edited.
func (m *SettingsModel) editedKeys() []string {
	var keys []string
	for k, v := range m.values {
		if v != m.originalValues[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// validateSettingsValues runs every check a save makes over v, the merged
// values, and returns the first refusal's message — "" when v passes —
// with what the checks parsed. passwordHash is the live dashboard password.
func validateSettingsValues(v map[string]string, passwordHash string) (settingsParsed, string) {
	var p settingsParsed

	// Validate port
	p.port, _ = strconv.Atoi(v["port"])
	if p.port < 1 || p.port > 65535 {
		return p, "Port must be 1-65535"
	}

	// Validate external access requires password
	if isExternalAccess(v["network_access"]) && passwordHash == "" {
		return p, "Password required for external access. Set password in Network section."
	}

	// Validate trusted_proxies entries. config.Validate — and therefore the
	// config.Save behind OnSave — REFUSES a config carrying an unparseable
	// entry, so without this gate one typo makes the whole save fail while
	// saveAndClose still reports "Saved" and every other change in that save
	// is lost. Mirrors validateConfigUpdates' web-side field error.
	for e := range strings.SplitSeq(v["trusted_proxies"], ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		ok := false
		if strings.Contains(e, "/") {
			_, _, err := net.ParseCIDR(e)
			ok = err == nil
		} else {
			ok = net.ParseIP(e) != nil
		}
		if !ok {
			return p, fmt.Sprintf("Trusted proxies: %q is not a valid IP or CIDR", e)
		}
	}

	// Validate probe_targets entries. Same rationale as trusted_proxies above:
	// config.Validate refuses an unparseable host:port, so gate it here too.
	for e := range strings.SplitSeq(v["probe_targets"], ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(e); err != nil {
			return p, fmt.Sprintf("Probe targets: %q is not a valid host:port", e)
		}
	}

	// Validate network.public_url. Same rationale as the two gates above:
	// config.Validate refuses a config carrying an unusable value, so without
	// this one typo makes the whole save fail while saveAndClose still reports
	// "Saved". The canonical form (lowercased scheme, trailing slash trimmed)
	// is what gets written, so the TUI stores exactly what the web path's
	// validateConfigUpdates would.
	publicURL, err := config.ValidatePublicURL(v["public_url"])
	if err != nil {
		return p, fmt.Sprintf("Public dashboard URL: %v", err)
	}
	p.publicURL = publicURL

	// Validate browser_path if set.
	// Static checks only — the full ValidateBrowserPath spawns a subprocess
	// and waits up to 10s for --version, which would freeze the BubbleTea
	// event loop. The web UI runs the full check via the async HTTP endpoint.
	browserPath := strings.TrimSpace(v["browser_path"])
	browserType := strings.TrimSpace(v["browser_type"])
	if browserPath != "" {
		if browserType == "" {
			return p, "browser_type required when browser_path is set"
		}
		if err := cookies.ValidateBrowserPathQuick(browserPath, browserType); err != nil {
			return p, "Invalid browser: " + err.Error()
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
		{"max_video_resolution", "Max resolution must be at least 0 (0 = unbounded)", 0, math.MaxInt},
		{"num_parallel_downloads", "Parallel downloads must be at least 1", 1, math.MaxInt},
		{"segment_workers", "Segment workers must be at least 1", 1, math.MaxInt},
		{"reorder_buffer_mb", "Reorder buffer must be >= 0 MB (0 = unbounded)", 0, math.MaxInt},
		{"reorder_budget_mb", "Reorder budget must be >= 0 MB (0 = unbounded)", 0, math.MaxInt},
		{"maximum_timeout", "YouTube max timeout must be at least 30 seconds", 30, math.MaxInt},
		{"disk_warn_percent", "Disk warning threshold must be 1-99", 1, 99},
		{"disk_critical_percent", "Disk critical threshold must be 1-99", 1, 99},
		// The memory limits: 0 means "no limit", so an emptied field (Atoi 0)
		// switched the limit off and applied that at once. 65536 is the web
		// route's cap.
		{"go_soft_limit_mb", "Go soft memory limit must be 0-65536 MB (0 = no limit)", 0, 65536},
		{"sidecar_soft_limit_mb", "Sidecar soft memory limit must be 0-65536 MB (0 = no limit)", 0, 65536},
		{"sidecar_hard_limit_mb", "Sidecar hard memory limit must be 0-65536 MB (0 = no limit)", 0, 65536},
	} {
		n, err := strconv.Atoi(v[c.key])
		if err != nil || n < c.min || n > c.max {
			return p, c.msg
		}
	}
	warnPct, _ := strconv.Atoi(v["disk_warn_percent"])
	critPct, _ := strconv.Atoi(v["disk_critical_percent"])
	if critPct <= warnPct {
		return p, "Disk critical threshold must exceed warning threshold"
	}
	// The same pair rule config.Validate applies (and Save refuses on), said
	// as a field message instead of a raw "Save failed: invalid config".
	sideSoft, _ := strconv.Atoi(v["sidecar_soft_limit_mb"])
	sideHard, _ := strconv.Atoi(v["sidecar_hard_limit_mb"])
	if sideSoft > 0 && sideHard > 0 && sideHard <= sideSoft {
		return p, "Sidecar hard memory limit must exceed the soft limit"
	}
	// The FlexDuration-backed fields are FLOATS. Fractional days/minutes are
	// valid config the Web UI and the config file both accept, and parsing
	// them as ints here silently rewrote them on any unrelated TUI save
	// (CORE-7). The explicit NaN/Inf rejection matters: ParseFloat accepts
	// "nan" (and TOML 1.0 has nan/inf literals a hand-edited config could
	// carry), and NaN slips through a min/max range check because both
	// comparisons are false.
	p.flex = make(map[string]float64, 6)
	for _, c := range []struct {
		key, msg string
		min, max float64
	}{
		{"feed_check_interval", "Feed check interval must be 1-1440 minutes", 1, 1440},
		{"probe_cooldown", "Probe cooldown must be >= 0 seconds (0 disables)", 0, math.MaxFloat64},
		{"interruption_timeout", "Interruption resume timeout must be >= 0 minutes (0 disables)", 0, math.MaxFloat64},
		{"incomplete_staging_expiry_days", "Incomplete staging expiry must be >= 0 days (0 preserves forever)", 0, math.MaxFloat64},
		{"refresh_interval", "Cookie refresh interval must be 10-10080 minutes", 10, 10080},
		{"hide_finished_age_days", "Hide finished after must be 0-365 days", 0, 365},
	} {
		f, err := strconv.ParseFloat(strings.TrimSpace(v[c.key]), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < c.min || f > c.max {
			return p, c.msg
		}
		p.flex[c.key] = f
	}

	// The two optional interval overrides: empty means "dynamic". A value
	// OUT of range used to be silently nil'ed here too, which read as the
	// operator having asked for dynamic — the Web path returns a field error
	// instead, and the TUI's help text advertised a floor of 1 that
	// config.Validate refuses (CORE-20).
	for _, c := range []struct {
		key, msg string
		min, max int
	}{
		{"decapi_check_interval", "DECAPI check interval must be 15-3600 seconds (or empty for dynamic)", 15, 3600},
		{"twitch_check_interval", "Twitch check interval must be 5-3600 seconds (or empty for the default 15)", 5, 3600},
	} {
		s := strings.TrimSpace(v[c.key])
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < c.min || n > c.max {
			return p, c.msg
		}
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
		if strings.TrimSpace(v[c.key]) == "" {
			return p, c.msg
		}
	}
	if len(v["output_template"]) > config.OutputTemplateMaxLen {
		return p, fmt.Sprintf("Output template must be at most %d characters", config.OutputTemplateMaxLen)
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
		if config.PathHasTraversal(v[c.key]) {
			return p, c.label + " cannot contain a .. segment"
		}
	}
	return p, ""
}

// writeSettingsField writes field key of v — passed and parsed by
// validateSettingsValues into p — into c, and reports false for a key it
// does not know. One case per field loadSettingsValues loads; the two are
// pinned against each other by TestEverySettingsFieldIsWritable.
func writeSettingsField(c *config.MoomboxConfig, key string, v map[string]string, p settingsParsed) bool {
	val := v[key]
	yes := val == "Yes"
	// Every int field is range-checked by validateSettingsValues, so this
	// parse cannot fail for one; it is unused for the rest.
	n, _ := strconv.Atoi(val)
	switch key {
	// Network
	case "port":
		c.Network.Port = p.port
	case "network_access":
		c.Network.NetworkAccess = val
	case "https_enabled":
		c.Network.HTTPSEnabled = yes
	case "tls_cert_path":
		c.Network.TLSCertPath = val
	case "tls_key_path":
		c.Network.TLSKeyPath = val
	case "trust_forwarded_proto":
		c.Network.TrustForwardedProto = yes
	case "trusted_proxies":
		c.Network.TrustedProxies = splitSettingsList(val)
	case "public_url":
		c.Network.PublicURL = p.publicURL
	case "probe_targets":
		// Blank keeps the stored targets, as the dashboard's field does (an
		// empty list is refused there). The TUI used to write the defaults
		// instead, so the same gesture gave two configs.
		if targets := splitSettingsList(val); len(targets) > 0 {
			c.Connectivity.ProbeTargets = targets
		}

	// Paths
	case "database_path":
		c.Paths.DatabasePath = val
	case "log_file_path":
		c.Paths.LogFilePath = val
	case "output_directory":
		c.Paths.OutputDirectory = val
	case "staging_directory":
		c.Paths.StagingDirectory = val
	case "ffmpeg_path":
		c.Paths.FfmpegPath = val

	// Logs
	case "log_level":
		c.Logs.LogLevel = val
	case "log_max_file_size":
		c.Logs.LogMaxFileSize = n
	case "log_max_files":
		c.Logs.LogMaxFiles = n

	// Monitors
	case "archive_window_days":
		c.Monitors.ArchiveWindowDays = n
	case "archive_slots":
		c.Monitors.ArchiveSlots = n
	case "feed_check_interval":
		c.Monitors.FeedCheckInterval = config.FlexDuration{Value: p.flex[key]}
	case "decapi_check_interval":
		c.Monitors.DecapiCheckInterval = optionalSettingsInt(val)
	case "twitch_check_interval":
		c.Monitors.TwitchCheckInterval = optionalSettingsInt(val)
	case "hide_finished_age_days":
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: p.flex[key]}
	case "probe_cooldown":
		c.Monitors.ProbeCooldown = config.FlexDuration{Value: p.flex[key]}
	case "membership_discovery":
		c.Monitors.MembershipDiscovery = &yes

	// Downloader
	case "output_template":
		c.Downloader.OutputTemplate = val
	case "max_video_resolution":
		c.Downloader.MaxVideoResolution = n
	case "num_parallel_downloads":
		c.Downloader.NumParallelDownloads = n
	case "segment_workers":
		c.Downloader.SegmentWorkers = n
	case "reorder_buffer_mb":
		c.Downloader.ReorderBufferMB = n
	case "reorder_budget_mb":
		c.Downloader.ReorderBudgetMB = n
	case "download_chat":
		c.Downloader.DownloadChat = yes
	case "prefer_60fps":
		c.Downloader.Prefer60fps = yes
	case "maximum_timeout":
		c.Downloader.MaximumTimeout = n
	case "interruption_timeout":
		c.Downloader.InterruptionTimeout = config.FlexDuration{Value: p.flex[key]}
	case "incomplete_staging_expiry_days":
		c.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: p.flex[key]}

	// Cookies
	case "cookie_file":
		c.Cookies.CookieFile = val
	case "active_youtube", "active_twitch":
		// The toggles display GetActivePlatforms' answer, which may be
		// inferred. Only an edited toggle records the explicit override, so
		// this runs only for one: writing an unedited inferred answer back
		// would freeze it, and a channel added later would never light its
		// platform. Non-nil even when empty — [] is the explicit "both off"
		// override. The other toggle is its merged value: its live answer
		// unless it was edited too.
		activePlats := []string{}
		if v["active_youtube"] == "Yes" {
			activePlats = append(activePlats, "youtube")
		}
		if v["active_twitch"] == "Yes" {
			activePlats = append(activePlats, "twitch")
		}
		c.Cookies.ActivePlatforms = activePlats
	case "auto_enabled":
		c.Cookies.AutoEnabled = yes
	case "browser_profile_dir":
		c.Cookies.BrowserProfileDir = val
	case "browser_path":
		// TrimSpace matches what validateConfigUpdates does for the web path
		// (and applyConfigUpdates, in config_routes.go). Without trimming
		// here, a user pasting "  /usr/bin/firefox  " would pass the trimmed
		// validation but persist whitespace into config — exec.Command would
		// then fail with "fork/exec  /usr/bin/firefox  : no such file or
		// directory".
		c.Cookies.BrowserPath = strings.TrimSpace(val)
	case "browser_type":
		c.Cookies.BrowserType = strings.TrimSpace(val)
	case "refresh_interval":
		c.Cookies.RefreshInterval = config.FlexDuration{Value: p.flex[key]}
	case "dpapi_fallback":
		c.Cookies.DpapiFallback = yes
	case "acquisition":
		c.Cookies.Acquisition = val

	// Disk
	case "disk_warn_percent":
		c.Disk.WarnPercent = n
	case "disk_critical_percent":
		c.Disk.CriticalPercent = n

	// Updates
	case "auto_check_updates":
		c.Updates.AutoCheckUpdates = yes

	// BotGuard sidecar
	case "use_sidecar":
		c.Bgutils.UseSidecar = yes

	// Memory
	case "go_soft_limit_mb":
		c.Memory.GoSoftLimitMB = n
	case "sidecar_soft_limit_mb":
		c.Memory.SidecarSoftLimitMB = n
	case "sidecar_hard_limit_mb":
		c.Memory.SidecarHardLimitMB = n
	default:
		return false
	}
	return true
}

// splitSettingsList splits a comma-separated field into its trimmed,
// non-empty entries; nil when there are none.
func splitSettingsList(val string) []string {
	var out []string
	for e := range strings.SplitSeq(val, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// optionalSettingsInt is an optional interval override: nil for an empty
// field, which means the dynamic default.
func optionalSettingsInt(val string) *int {
	val = strings.TrimSpace(val)
	if val == "" {
		return nil
	}
	n, _ := strconv.Atoi(val) // range-checked by validateSettingsValues
	return &n
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

func (m *SettingsModel) isFieldSection() bool {
	return sections[m.sectionIndex].fields != nil
}

func boolToDisplay(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

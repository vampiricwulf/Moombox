package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// setupFieldType identifies field editing behavior in the wizard.
type setupFieldType int

const (
	setupFieldText   setupFieldType = iota
	setupFieldNumber
	setupFieldToggle
	setupFieldCycle
)

// setupFieldDef defines a field in a setup wizard step.
type setupFieldDef struct {
	key            string
	label          string
	defaultDisplay string
	help           string
	ftype          setupFieldType
	options        []string // for toggle/cycle
}

// setupStepDef defines a setup wizard step.
type setupStepDef struct {
	title    string
	subtitle string
	fields   []setupFieldDef
	footer   string
}

var setupSteps = []setupStepDef{
	{
		title:    "Output & Paths",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"outputDir", "Output directory", "./output", "Where finished downloads are saved", setupFieldText, nil},
			{"outputTemplate", "Filename template", "${channel}/${start_date} ${title} [${id}]", "Variables: ${title} ${id} ${channel} ${start_date} ${start_time}", setupFieldText, nil},
			{"stagingDir", "Staging directory", "./staging", "Temporary directory for segments during download", setupFieldText, nil},
			{"databasePath", "Database path", "./moombox.json", "Job database file location", setupFieldText, nil},
			{"ffmpegPath", "FFmpeg path", "system PATH", "Leave empty to use system ffmpeg", setupFieldText, nil},
		},
	},
	{
		title:    "Download Preferences",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"maxRes", "Max resolution", "1080", "Maximum video dimension (width or height)", setupFieldNumber, nil},
			{"prefer60fps", "Prefer 60fps", "Yes", "When same resolution, prefer 60fps. Resolution always wins", setupFieldToggle, []string{"Yes", "No"}},
			{"numParallel", "Parallel downloads", "2", "How many streams to download simultaneously", setupFieldNumber, nil},
			{"downloadChat", "Download chat", "Yes", "Save live chat as JSON alongside video", setupFieldToggle, []string{"Yes", "No"}},
			{"autoCookies", "Auto cookie login", "No", "Opens browser for Google login, grabs cookies automatically", setupFieldToggle, []string{"No", "Yes"}},
			{"cookieFile", "Cookie file (manual)", "empty = none", "Netscape-format cookie file. Not needed if auto-login is enabled", setupFieldText, nil},
		},
	},
	{
		title:    "Logging",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"logLevel", "Log level", "INFO", "Verbosity of log output", setupFieldCycle, []string{"DEBUG", "INFO", "WARN", "ERROR"}},
			{"logFilePath", "Log file path", "./moombox.log", "Where log file is written", setupFieldText, nil},
			{"logMaxSize", "Max log file size", "10485760", "Maximum bytes before log rotation (10MB)", setupFieldNumber, nil},
			{"logMaxFiles", "Max log files", "5", "Number of rotated log files to keep", setupFieldNumber, nil},
		},
	},
	{
		title:    "Display & Monitoring",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"hideAge", "Hide finished after (days)", "30", "Finished jobs older than this move to Archived", setupFieldNumber, nil},
			{"maxFeedItems", "Max feed items", "15", "RSS feed items to check per channel", setupFieldNumber, nil},
			{"port", "Dashboard port", "774", "Web dashboard & API port (tries nearby if busy)", setupFieldNumber, nil},
			{"networkAccess", "Network access", "Localhost", "Who can access the dashboard", setupFieldCycle, []string{"Localhost", "LAN", "External"}},
			{"installYtdlpPlugin", "Install yt-dlp plugin", "No", "Install a yt-dlp plugin so it uses Moombox for PO tokens", setupFieldToggle, []string{"Yes", "No"}},
		},
	},
	{
		title:    "Monitor a Channel (Optional)",
		subtitle: "Leave empty to skip channel monitoring",
		fields: []setupFieldDef{
			{"channelId", "Channel ID", "empty = skip", "YouTube channel ID (starts with UC)", setupFieldText, nil},
			{"channelName", "Display name", "empty", "Friendly name shown in task list", setupFieldText, nil},
			{"channelTerms", "Filter regex", "empty = all streams", "Only archive streams matching this pattern", setupFieldText, nil},
			{"includeNonLive", "Include non-live", "No", "Also download regular uploaded videos", setupFieldToggle, []string{"No", "Yes"}},
		},
		footer: "Examples: (?i)karaoke  |  (?i)(singing|\u6b4c\u679a)  |  (?i)unarchived",
	},
}

// SetupWizardModel manages the first-run setup wizard.
type SetupWizardModel struct {
	visible bool
	width   int
	height  int

	step         int
	focusedField int
	values       map[string]string
	saving       bool
	errorMsg     string

	// Callbacks for finishing setup
	OnComplete        func(cfg *config.MoomboxConfig) error
	OnInstallYtdlp    func(port int)
	OnStartAutoCookie func()
}

// NewSetupWizardModel creates a new setup wizard model.
func NewSetupWizardModel() *SetupWizardModel {
	return &SetupWizardModel{
		values: make(map[string]string),
	}
}

// Open shows the setup wizard.
func (m *SetupWizardModel) Open() {
	m.visible = true
	m.step = 0
	m.focusedField = 0
	m.values = make(map[string]string)
	m.saving = false
	m.errorMsg = ""
}

// Close hides the wizard.
func (m *SetupWizardModel) Close() {
	m.visible = false
}

// IsVisible returns true if the wizard is shown.
func (m *SetupWizardModel) IsVisible() bool {
	return m.visible
}

// SetSize updates dimensions.
func (m *SetupWizardModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// getFieldValue returns the current value for a field.
// Toggles and cycles default to their defaultDisplay; text/number default to "".
func (m *SetupWizardModel) getFieldValue(field setupFieldDef) string {
	if val, ok := m.values[field.key]; ok {
		return val
	}
	if field.ftype == setupFieldToggle || field.ftype == setupFieldCycle {
		return field.defaultDisplay
	}
	return ""
}

// HandleKey processes key input. Returns "complete" when setup finishes.
func (m *SetupWizardModel) HandleKey(key string) string {
	if m.saving {
		return ""
	}

	// Clear error on any input
	if m.errorMsg != "" {
		m.errorMsg = ""
	}

	currentStep := setupSteps[m.step]
	fieldCount := len(currentStep.fields)

	// Determine focused field
	var field *setupFieldDef
	if m.focusedField >= 0 && m.focusedField < fieldCount {
		field = &currentStep.fields[m.focusedField]
	}

	switch key {
	case keyEsc:
		if m.step > 0 {
			m.step--
			m.focusedField = 0
		}
		return ""

	case "left":
		if field != nil && (field.ftype == setupFieldToggle || field.ftype == setupFieldCycle) {
			m.cycleOption(*field, -1)
		}
		return ""

	case "right":
		if field != nil && (field.ftype == setupFieldToggle || field.ftype == setupFieldCycle) {
			m.cycleOption(*field, 1)
		}
		return ""

	case keyEnter, keyTab:
		if m.step < len(setupSteps)-1 {
			m.step++
			m.focusedField = 0
		} else {
			return m.finishSetup()
		}
		return ""

	case keyUp:
		if m.focusedField > 0 {
			m.focusedField--
		}
		return ""

	case keyDown:
		if m.focusedField < fieldCount-1 {
			m.focusedField++
		}
		return ""

	case "backspace", "delete":
		// Match TS: key.backspace || key.delete
		if field == nil || field.ftype == setupFieldToggle || field.ftype == setupFieldCycle {
			return ""
		}
		cur := m.getFieldValue(*field)
		if len(cur) > 0 {
			runes := []rune(cur)
			m.values[field.key] = string(runes[:len(runes)-1])
		}
		return ""

	case "ctrl+v":
		// Clipboard paste support (match TS)
		if field == nil || field.ftype == setupFieldToggle || field.ftype == setupFieldCycle {
			return ""
		}
		if clip := readClipboard(); clip != "" {
			// Take first line only (match TS: text.split(/[\r\n]/)[0].trim())
			if idx := strings.IndexAny(clip, "\r\n"); idx >= 0 {
				clip = clip[:idx]
			}
			clip = strings.TrimSpace(clip)
			// Filter non-digits for number fields (match TS clipboard paste behavior)
			if field.ftype == setupFieldNumber {
				var filtered strings.Builder
				for _, ch := range clip {
					if ch >= '0' && ch <= '9' {
						filtered.WriteRune(ch)
					}
				}
				clip = filtered.String()
			}
			if clip != "" {
				cur := m.getFieldValue(*field)
				m.values[field.key] = cur + clip
			}
		}
		return ""

	default:
		if field == nil || field.ftype == setupFieldToggle || field.ftype == setupFieldCycle {
			return ""
		}
		if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			// Reject escape sequence fragments
			if key[0] == 0x1b {
				return ""
			}
			// Number fields only accept digits
			if field.ftype == setupFieldNumber && (key[0] < '0' || key[0] > '9') {
				return ""
			}
			cur := m.getFieldValue(*field)
			m.values[field.key] = cur + key
		}
		return ""
	}
}

func (m *SetupWizardModel) cycleOption(field setupFieldDef, direction int) {
	if field.options == nil {
		return
	}
	current := m.getFieldValue(field)
	idx := 0
	for i, opt := range field.options {
		if opt == current {
			idx = i
			break
		}
	}
	next := (idx + direction + len(field.options)) % len(field.options)
	m.values[field.key] = field.options[next]
}

func (m *SetupWizardModel) finishSetup() string {
	m.saving = true
	m.errorMsg = ""

	v := func(key string) string {
		return strings.TrimSpace(m.values[key])
	}
	vNum := func(key string) int {
		s := v(key)
		if s == "" {
			return 0
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	vBool := func(key string, defaultVal bool) bool {
		s, ok := m.values[key]
		if !ok {
			return defaultVal
		}
		return s == "Yes"
	}

	// Map display names to config values
	netAccessMap := map[string]string{
		"Localhost": "localhost",
		"LAN":      "lan",
		"External":  "external",
	}
	networkAccess := netAccessMap[m.values["networkAccess"]]
	if networkAccess == "" {
		networkAccess = "localhost"
	}

	cfg := &config.MoomboxConfig{
		NetworkAccess:  networkAccess,
		LogLevel:       m.values["logLevel"],
		LogFilePath:    v("logFilePath"),
		LogMaxFileSize: vNum("logMaxSize"),
		LogMaxFiles:    vNum("logMaxFiles"),
		DatabasePath:   v("databasePath"),
		MaxFeedItems:   vNum("maxFeedItems"),
		Downloader: config.DownloaderConfig{
			OutputDirectory:      v("outputDir"),
			OutputTemplate:       v("outputTemplate"),
			StagingDirectory:     v("stagingDir"),
			NumParallelDownloads: vNum("numParallel"),
			MaxVideoResolution:   vNum("maxRes"),
			Prefer60fps:          vBool("prefer60fps", true),
			DownloadChat:         vBool("downloadChat", true),
			FfmpegPath:           v("ffmpegPath"),
			CookieFile:           v("cookieFile"),
		},
	}

	port := vNum("port")
	if port > 0 {
		cfg.Port = port
	}

	hideAge := vNum("hideAge")
	if hideAge > 0 {
		cfg.Tasklist = &config.TasklistConfig{
			HideFinishedAgeDays: config.FlexDuration{Value: float64(hideAge)},
		}
	}

	if vBool("autoCookies", false) {
		cfg.AutoCookies = &config.AutoCookiesConfig{
			Enabled: true,
		}
	}

	channelID := v("channelId")
	if channelID != "" {
		ch := config.ChannelConfig{
			ID:   channelID,
			Name: v("channelName"),
		}
		terms := v("channelTerms")
		if terms != "" {
			ch.Terms = config.ChannelTerms{Simple: terms}
		}
		if vBool("includeNonLive", false) {
			ch.IncludeNonLiveContent = true
		}
		cfg.Channels = []config.ChannelConfig{ch}
	}

	// Save config (match TS try/catch around ConfigManager.save)
	if m.OnComplete != nil {
		if err := m.OnComplete(cfg); err != nil {
			m.errorMsg = fmt.Sprintf("Failed to save: %v", err)
			m.saving = false
			return ""
		}
	}

	// Post-save actions (non-fatal, match TS behavior)
	if vBool("installYtdlpPlugin", false) && m.OnInstallYtdlp != nil {
		p := port
		if p == 0 {
			p = 774
		}
		m.OnInstallYtdlp(p)
	}

	if vBool("autoCookies", false) && m.OnStartAutoCookie != nil {
		m.OnStartAutoCookie()
	}

	return "complete"
}

// Port returns the port value (for backward compatibility).
func (m *SetupWizardModel) Port() string { return m.values["port"] }

// Directory returns the output directory value.
func (m *SetupWizardModel) Directory() string { return m.values["outputDir"] }

// CookiePath returns the cookie file path value.
func (m *SetupWizardModel) CookiePath() string { return m.values["cookieFile"] }

// View renders the setup wizard.
func (m *SetupWizardModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := min(72, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4
	totalSteps := len(setupSteps)
	currentStep := setupSteps[m.step]
	fieldCount := len(currentStep.fields)

	var lines []string

	// Title bar: "Moombox Setup" left, "Step X/Y" right
	titleText := "Moombox Setup"
	stepText := fmt.Sprintf("Step %d/%d", m.step+1, totalSteps)
	titleRendered := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(titleText)
	stepRendered := DimStyle.Render(stepText)
	titlePad := contentW - runewidth.StringWidth(titleText) - runewidth.StringWidth(stepText)
	if titlePad < 1 {
		titlePad = 1
	}
	lines = append(lines, titleRendered+strings.Repeat(" ", titlePad)+stepRendered)

	// Step indicator: [+] 1 - [>] 2 - [ ] 3 - [ ] 4 - [ ] 5
	var stepParts []string
	for i := range setupSteps {
		marker := " "
		color := ColorGray
		if i < m.step {
			marker = "+"
			color = ColorGreen
		} else if i == m.step {
			marker = ">"
			color = ColorCyan
		}
		stepParts = append(stepParts, lipgloss.NewStyle().Foreground(color).Render(
			fmt.Sprintf("[%s] %d", marker, i+1),
		))
	}
	lines = append(lines, strings.Join(stepParts, DimStyle.Render(" - ")))

	// Step title + subtitle
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render(currentStep.title))
	if currentStep.subtitle != "" {
		lines = append(lines, DimStyle.Render(currentStep.subtitle))
	}

	// Divider
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

	// Fields
	for idx := range fieldCount {
		field := currentStep.fields[idx]
		isFocused := idx == m.focusedField
		val := m.getFieldValue(field)
		isToggleOrCycle := field.ftype == setupFieldToggle || field.ftype == setupFieldCycle

		// Label line: "> Label (default: value)" or "  Label (default: value)"
		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelColor := ColorWhite
		if isFocused {
			labelColor = ColorCyan
		}
		labelStyle := lipgloss.NewStyle().Foreground(labelColor)
		if isFocused {
			labelStyle = labelStyle.Bold(true)
		}
		prefixStyle := lipgloss.NewStyle().Foreground(labelColor)
		defaultHint := DimStyle.Render(" (default: " + field.defaultDisplay + ")")
		lines = append(lines, prefixStyle.Render(prefix)+labelStyle.Render(field.label)+defaultHint)

		// Value line
		if isToggleOrCycle {
			lines = append(lines, "  "+renderSetupOptionSelector(field.options, val, isFocused))
		} else {
			if val == "" && !isFocused {
				// Show placeholder
				lines = append(lines, "  "+DimStyle.Render("["+field.defaultDisplay+"]"))
			} else {
				cursor := ""
				if isFocused {
					cursor = lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
				}
				lines = append(lines, "  "+DimStyle.Render("[")+val+cursor+DimStyle.Render("]"))
			}
		}

		// Help text
		if field.help != "" {
			lines = append(lines, "   "+DimStyle.Render(field.help))
		}

		// External network access warning
		if field.key == "networkAccess" && val == "External" {
			warnBold := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true)
			warnNorm := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))
			lines = append(lines, "   "+warnBold.Render("Warning: ")+warnNorm.Render(
				"Security measures were not audited by a specialist. No authentication \u2014 anyone who reaches this port has full access."))
		}

		// Spacing between fields (match TS marginBottom={1})
		if idx < fieldCount-1 {
			lines = append(lines, "")
		}
	}

	// Step footer (e.g., regex examples)
	if currentStep.footer != "" {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render(currentStep.footer))
	}

	// Error
	if m.errorMsg != "" {
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	// Saving indicator
	if m.saving {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Render("Saving configuration..."))
	}

	// Navigation hints
	lines = append(lines, "")
	if m.step < totalSteps-1 {
		var hintLeft string
		if m.step > 0 {
			hintLeft = DimStyle.Render("Esc: Back")
		}
		hintRight := DimStyle.Render("\u2191/\u2193: Fields  \u2190/\u2192: Toggle  Tab/Enter: Next")
		if hintLeft != "" {
			hintLeft += strings.Repeat(" ", max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("\u2191/\u2193: Fields  \u2190/\u2192: Toggle  Tab/Enter: Next")))
			lines = append(lines, hintLeft+hintRight)
		} else {
			lines = append(lines, strings.Repeat(" ", max(0, contentW-runewidth.StringWidth("\u2191/\u2193: Fields  \u2190/\u2192: Toggle  Tab/Enter: Next")))+hintRight)
		}
	} else {
		// Last step: highlight "Tab: Finish"
		var hintLeft string
		if m.step > 0 {
			hintLeft = DimStyle.Render("Esc: Back")
		}
		navHint := DimStyle.Render("\u2191/\u2193: Fields  \u2190/\u2192: Toggle  ")
		finishHint := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Tab: Finish")
		rightSide := navHint + finishHint
		if hintLeft != "" {
			gap := max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("\u2191/\u2193: Fields  \u2190/\u2192: Toggle  Tab: Finish"))
			lines = append(lines, hintLeft+strings.Repeat(" ", gap)+rightSide)
		} else {
			lines = append(lines, rightSide)
		}
	}

	content := strings.Join(lines, "\n")

	h := min(m.height-2, m.height)
	if h < 10 {
		h = 10
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(contentW).
		Height(h).
		Render(content)

	return centerBox(box, m.width, m.height)
}

// renderSetupOptionSelector renders toggle/cycle options like [Yes] / No.
func renderSetupOptionSelector(options []string, selected string, focused bool) string {
	var parts []string
	for _, opt := range options {
		if opt == selected {
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

// ensure runewidth is used
var _ = runewidth.StringWidth

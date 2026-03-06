package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// setupMode identifies which wizard flow is active.
type setupMode int

const (
	setupModeSelect   setupMode = iota // Mode selection screen
	setupModeSimple                    // Quick Setup
	setupModeAdvanced                  // Advanced Setup
)

// setupSimpleStage identifies the current stage in simplified setup.
type setupSimpleStage int

const (
	setupSimpleCookies  setupSimpleStage = iota // Cookie acquisition
	setupSimpleChannels                         // Channel management
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

// Advanced setup steps — 8 sections matching settings.go
var advancedSetupSteps = []setupStepDef{
	{
		title:    "Network",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"port", "Dashboard port", "774", "Web dashboard & API port (tries nearby if busy)", setupFieldNumber, nil},
			{"networkAccess", "Network access", "Localhost", "Who can access the dashboard", setupFieldCycle, []string{"Localhost", "LAN"}},
			{"httpsEnabled", "HTTPS enabled", "No", "Enable HTTPS for the dashboard", setupFieldToggle, []string{"No", "Yes"}},
		},
	},
	{
		title:    "Paths",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"outputDir", "Output directory", "./output", "Where finished downloads are saved", setupFieldText, nil},
			{"outputTemplate", "Filename template", "${channel}/${start_date} ${title} [${id}]", "Variables: ${title} ${id} ${channel} ${start_date} ${start_time}", setupFieldText, nil},
			{"stagingDir", "Staging directory", "./staging", "Temporary directory for segments during download", setupFieldText, nil},
			{"databasePath", "Database path", "./moombox.db", "Job database file location", setupFieldText, nil},
			{"ffmpegPath", "FFmpeg path", "system PATH", "Leave empty to use system ffmpeg", setupFieldText, nil},
		},
	},
	{
		title:    "Logs",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"logLevel", "Log level", "INFO", "Verbosity of log output", setupFieldCycle, []string{"DEBUG", "INFO", "WARN", "ERROR"}},
			{"logFilePath", "Log file path", "./moombox.log", "Where log file is written", setupFieldText, nil},
			{"logMaxSize", "Max log file size", "10485760", "Maximum bytes before log rotation (10MB)", setupFieldNumber, nil},
			{"logMaxFiles", "Max log files", "5", "Number of rotated log files to keep", setupFieldNumber, nil},
		},
	},
	{
		title:    "Monitors",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"maxFeedItems", "Max feed items", "15", "RSS feed items to check per channel", setupFieldNumber, nil},
			{"feedCheckInterval", "Feed check interval", "10", "Minutes between feed checks", setupFieldNumber, nil},
			{"hideAge", "Hide finished after (days)", "30", "Finished jobs older than this move to Archived", setupFieldNumber, nil},
		},
	},
	{
		title:    "Downloader",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"maxRes", "Max resolution", "2160", "Maximum video dimension (width or height)", setupFieldNumber, nil},
			{"prefer60fps", "Prefer 60fps", "Yes", "When same resolution, prefer 60fps. Resolution always wins", setupFieldToggle, []string{"Yes", "No"}},
			{"numParallel", "Parallel downloads", "2", "How many streams to download simultaneously", setupFieldNumber, nil},
			{"downloadChat", "Download chat", "Yes", "Save live chat as JSON alongside video", setupFieldToggle, []string{"Yes", "No"}},
		},
	},
	{
		title:    "Cookies",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"cookieFile", "Cookie file (manual)", "./cookies.txt", "Netscape-format cookie file", setupFieldText, nil},
			{"autoCookies", "Auto cookie login", "No", "Opens browser for login, grabs cookies automatically", setupFieldToggle, []string{"No", "Yes"}},
		},
	},
	{
		title:    "Channels",
		subtitle: "Add channels to monitor",
		fields:   nil, // Channel sub-editor
	},
	{
		title:    "Integrations",
		subtitle: "Optional integrations",
		fields: []setupFieldDef{
			{"installYtdlpPlugin", "Install yt-dlp plugin", "No", "Install a yt-dlp plugin so it uses Moombox for PO tokens", setupFieldToggle, []string{"No", "Yes"}},
		},
	},
}

// SetupWizardModel manages the first-run setup wizard.
type SetupWizardModel struct {
	visible bool
	width   int
	height  int

	mode       setupMode
	modeChoice int // 0=Quick, 1=Advanced on mode selection screen
	values     map[string]string
	saving       bool
	errorMsg     string

	// Advanced setup huh form
	advancedForm     *huh.Form
	advancedFormDone bool
	advancedInitCmd  tea.Cmd // pending Init cmd from buildAdvancedForm

	// Simplified setup state
	simpleStage  setupSimpleStage
	cookieFocus  int  // 0=YouTube, 1=Twitch, 2=Skip
	cookieActive bool // true when browser is open
	cookiePlatform string
	cookieYTDone bool
	cookieTWDone bool

	// Channel sub-editor (shared by simple and advanced)
	channels          []config.ChannelConfig
	channelIndex      int
	channelMode       string // "list" or "edit"
	channelEditValues map[string]string
	channelEditField  int
	channelDeleteConf bool

	// Shared text input component (holds the currently-active text field)
	textInput textinput.Model

	// Loading spinner (shown during cookie extraction and saving)
	spinner spinner.Model

	// Callbacks for finishing setup
	OnComplete         func(cfg *config.MoomboxConfig) error
	OnInstallYtdlp     func(port int)
	OnStartAutoCookie  func(platform string)
	OnFinishAutoCookie func() (bool, bool)
	OnCancelAutoCookie func()
	OnRestart          func()
}

// NewSetupWizardModel creates a new setup wizard model.
func NewSetupWizardModel() *SetupWizardModel {
	return &SetupWizardModel{
		values:      make(map[string]string),
		channelMode: "list",
		textInput:   newTextInput(),
		spinner:     newSpinner(),
	}
}

// Open shows the setup wizard.
func (m *SetupWizardModel) Open() {
	m.visible = true
	m.mode = setupModeSelect
	m.modeChoice = 0
	m.values = make(map[string]string)
	m.saving = false
	m.errorMsg = ""
	m.advancedForm = nil
	m.advancedFormDone = false
	m.advancedInitCmd = nil
	m.simpleStage = setupSimpleCookies
	m.cookieFocus = 0
	m.cookieActive = false
	m.cookiePlatform = ""
	m.cookieYTDone = false
	m.cookieTWDone = false
	m.channels = nil
	m.channelIndex = 0
	m.channelMode = "list"
	m.channelEditValues = nil
	m.channelEditField = 0
	m.channelDeleteConf = false
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

// buildAdvancedForm creates a huh.Form from advancedSetupSteps.
func (m *SetupWizardModel) buildAdvancedForm() {
	// Initialize default values for toggle/cycle fields so Select has a starting value.
	for _, step := range advancedSetupSteps {
		for _, f := range step.fields {
			if f.ftype == setupFieldToggle || f.ftype == setupFieldCycle {
				if _, ok := m.values[f.key]; !ok {
					m.values[f.key] = f.defaultDisplay
				}
			}
		}
	}

	var groups []*huh.Group
	for _, step := range advancedSetupSteps {
		if step.fields == nil {
			continue // Skip Channels — handled as sub-editor
		}
		var fields []huh.Field
		for _, f := range step.fields {
			switch f.ftype {
			case setupFieldText:
				fields = append(fields,
					huh.NewInput().
						Key(f.key).
						Title(f.label).
						Description(f.help).
						Placeholder(f.defaultDisplay).
						Accessor(&MapAccessor{M: m.values, Key: f.key}),
				)
			case setupFieldNumber:
				fields = append(fields,
					huh.NewInput().
						Key(f.key).
						Title(f.label).
						Description(f.help).
						Placeholder(f.defaultDisplay).
						Validate(func(s string) error { return validateDigitsOnly(s) }).
						Accessor(&MapAccessor{M: m.values, Key: f.key}),
				)
			case setupFieldToggle, setupFieldCycle:
				var opts []huh.Option[string]
				for _, o := range f.options {
					opts = append(opts, huh.NewOption(o, o))
				}
				fields = append(fields,
					huh.NewSelect[string]().
						Key(f.key).
						Title(f.label).
						Description(f.help).
						Options(opts...).
						Filtering(false).
						Accessor(&MapAccessor{M: m.values, Key: f.key}),
				)
			}
		}
		groups = append(groups, huh.NewGroup(fields...).Title(step.title).Description(step.subtitle))
	}

	m.advancedForm = huh.NewForm(groups...).
		WithTheme(moomboxTheme()).
		WithShowHelp(false)
	m.advancedForm.SubmitCmd = nil // Don't quit on submit
	m.advancedInitCmd = m.advancedForm.Init()
}

// UpdateComponents routes tea.Msg to the embedded textinput, huh form, or spinner and syncs.
func (m *SetupWizardModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}

	// Route spinner when saving or cookie active
	if m.saving || m.cookieActive {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}

	// Advanced mode: route to huh form or channel editor textinput
	if m.mode == setupModeAdvanced {
		if m.advancedForm != nil {
			// Deliver any pending Init cmd first
			if m.advancedInitCmd != nil {
				initCmd := m.advancedInitCmd
				m.advancedInitCmd = nil
				model, cmd := m.advancedForm.Update(msg)
				m.advancedForm = model.(*huh.Form)
				return tea.Batch(initCmd, cmd)
			}
			model, cmd := m.advancedForm.Update(msg)
			m.advancedForm = model.(*huh.Form)
			if m.advancedForm.State == huh.StateCompleted {
				// Transition to channel editing
				m.advancedForm = nil
				m.advancedFormDone = true
				m.channelMode = "list"
				m.channels = nil
				m.channelIndex = 0
			}
			return cmd
		}
		// Channel editing mode — route to textinput
		if m.advancedFormDone && m.channelMode == "edit" && m.textInput.Focused() {
			prev := m.textInput.Value()
			var cmd tea.Cmd
			m.textInput, cmd = m.textInput.Update(msg)
			if m.textInput.Value() != prev {
				m.syncFromTextInput()
			}
			return cmd
		}
		return nil
	}

	// Simple mode / other: route to textinput
	if !m.textInput.Focused() {
		return nil
	}
	prev := m.textInput.Value()
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prev {
		m.syncFromTextInput()
	}
	return cmd
}

func (m *SetupWizardModel) syncFromTextInput() {
	val := m.textInput.Value()

	// Channel edit mode (used by both simple and advanced channel editors)
	if m.channelMode == "edit" {
		fields := m.visibleSetupChannelFields()
		if m.channelEditField < len(fields) {
			field := fields[m.channelEditField]
			if field.ftype == fieldText {
				m.channelEditValues[field.key] = val
			}
		}
	}
}

func (m *SetupWizardModel) updateTextInputForField() {
	// Channel edit mode (used by both simple and advanced channel editors)
	if m.channelMode == "edit" {
		fields := m.visibleSetupChannelFields()
		if m.channelEditField < len(fields) {
			field := fields[m.channelEditField]
			if field.ftype == fieldText {
				m.textInput.EchoMode = textinput.EchoNormal
				m.textInput.Validate = nil
				m.textInput.SetValue(m.channelEditValues[field.key])
				m.textInput.Focus()
				return
			}
		}
		m.textInput.Blur()
		return
	}

	m.textInput.Blur()
}

// HandleKey processes key input. Returns "complete" when setup finishes.
func (m *SetupWizardModel) HandleKey(key string) string {
	if m.saving {
		return ""
	}
	if m.errorMsg != "" {
		m.errorMsg = ""
	}

	switch m.mode {
	case setupModeSelect:
		return m.handleModeSelectKey(key)
	case setupModeSimple:
		return m.handleSimpleKey(key)
	case setupModeAdvanced:
		return m.handleAdvancedKey(key)
	}
	return ""
}

// --- Mode Selection ---

func (m *SetupWizardModel) handleModeSelectKey(key string) string {
	switch key {
	case keyUp:
		if m.modeChoice > 0 {
			m.modeChoice--
		}
	case keyDown:
		if m.modeChoice < 1 {
			m.modeChoice++
		}
	case keyEnter, keyTab:
		if m.modeChoice == 0 {
			m.mode = setupModeSimple
			m.simpleStage = setupSimpleCookies
			m.cookieFocus = 0
		} else {
			m.mode = setupModeAdvanced
			m.buildAdvancedForm()
		}
	}
	return ""
}

// --- Simplified Setup ---

func (m *SetupWizardModel) handleSimpleKey(key string) string {
	switch m.simpleStage {
	case setupSimpleCookies:
		return m.handleSimpleCookieKey(key)
	case setupSimpleChannels:
		return m.handleSimpleChannelKey(key)
	}
	return ""
}

func (m *SetupWizardModel) handleSimpleCookieKey(key string) string {
	if m.cookieActive {
		// Waiting for user to finish login
		switch key {
		case keyEnter:
			// Finish auto-cookie
			if m.OnFinishAutoCookie != nil {
				yt, tw := m.OnFinishAutoCookie()
				if m.cookiePlatform == "youtube" {
					m.cookieYTDone = yt
				} else {
					m.cookieTWDone = tw
				}
			}
			m.cookieActive = false
			m.cookiePlatform = ""
		case keyEsc:
			if m.OnCancelAutoCookie != nil {
				m.OnCancelAutoCookie()
			}
			m.cookieActive = false
			m.cookiePlatform = ""
		}
		return ""
	}

	switch key {
	case keyEsc:
		m.mode = setupModeSelect
	case keyUp:
		if m.cookieFocus > 0 {
			m.cookieFocus--
		}
	case keyDown:
		if m.cookieFocus < 2 {
			m.cookieFocus++
		}
	case keyEnter, keyTab:
		switch m.cookieFocus {
		case 0: // YouTube
			if m.OnStartAutoCookie != nil {
				m.OnStartAutoCookie("youtube")
				m.cookieActive = true
				m.cookiePlatform = "youtube"
				m.spinner = newSpinner()
			}
		case 1: // Twitch
			if m.OnStartAutoCookie != nil {
				m.OnStartAutoCookie("twitch")
				m.cookieActive = true
				m.cookiePlatform = "twitch"
				m.spinner = newSpinner()
			}
		case 2: // Skip / Next
			m.simpleStage = setupSimpleChannels
			m.channelIndex = 0
			m.channelMode = "list"
		}
	}
	return ""
}

func (m *SetupWizardModel) handleSimpleChannelKey(key string) string {
	if m.channelMode == "edit" {
		return m.handleChannelEditKey(key)
	}

	// Channel list mode with delete confirmation
	if m.channelDeleteConf {
		if key == "d" || key == "D" {
			if m.channelIndex < len(m.channels) {
				m.channels = append(m.channels[:m.channelIndex], m.channels[m.channelIndex+1:]...)
				if m.channelIndex >= len(m.channels) && m.channelIndex > 0 {
					m.channelIndex--
				}
			}
			m.channelDeleteConf = false
		} else {
			m.channelDeleteConf = false
		}
		return ""
	}

	switch key {
	case keyEsc:
		m.simpleStage = setupSimpleCookies
	case keyUp:
		if m.channelIndex > 0 {
			m.channelIndex--
		}
	case keyDown:
		if m.channelIndex < len(m.channels)-1 {
			m.channelIndex++
		}
	case "a", "A":
		m.channelEditValues = map[string]string{
			"id": "", "name": "", "platform": "youtube",
			"enabled": "Yes", "terms": "",
			"include_non_live": "No", "quality_preference": "best",
		}
		m.channelEditField = 0
		m.channelIndex = len(m.channels)
		m.channelMode = "edit"
		m.updateTextInputForField()
	case keyEnter:
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			m.channelMode = "edit"
			m.channelEditValues = channelToValues(m.channels[m.channelIndex])
			m.channelEditField = 0
			m.updateTextInputForField()
		}
	case "d", "D":
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			m.channelDeleteConf = true
		}
	case keyTab:
		// Finish simplified setup
		return m.finishSimpleSetup()
	}
	return ""
}

// visibleSetupChannelFields returns channel fields filtered by platform.
func (m *SetupWizardModel) visibleSetupChannelFields() []channelFieldDef {
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

func (m *SetupWizardModel) handleChannelEditKey(key string) string {
	fields := m.visibleSetupChannelFields()
	if len(fields) == 0 {
		return ""
	}
	if m.channelEditField >= len(fields) {
		m.channelEditField = len(fields) - 1
	}
	field := fields[m.channelEditField]

	switch key {
	case keyEsc:
		m.channelMode = "list"
		if m.channelIndex >= len(m.channels) && len(m.channels) > 0 {
			m.channelIndex = len(m.channels) - 1
		} else if len(m.channels) == 0 {
			m.channelIndex = 0
		}
		m.textInput.Blur()
		return ""
	case keyEnter:
		if id := strings.TrimSpace(m.channelEditValues["id"]); id == "" {
			return ""
		}
		ch := valuesToChannel(m.channelEditValues)
		if m.channelIndex < len(m.channels) {
			m.channels[m.channelIndex] = ch
		} else {
			m.channels = append(m.channels, ch)
		}
		m.channelMode = "list"
		m.textInput.Blur()
		return ""
	case keyUp:
		if m.channelEditField > 0 {
			m.channelEditField--
			m.updateTextInputForField()
		}
		return ""
	case keyDown, keyTab:
		if m.channelEditField < len(fields)-1 {
			m.channelEditField++
			m.updateTextInputForField()
		}
		return ""
	case "left":
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelFieldReverse(field)
		}
		return ""
	case "right":
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelField(field)
		}
		return ""
	}
	return ""
}

func (m *SetupWizardModel) cycleChannelField(field channelFieldDef) {
	if field.options == nil {
		return
	}
	cur := m.channelEditValues[field.key]
	for i, opt := range field.options {
		if strings.EqualFold(opt, cur) {
			m.channelEditValues[field.key] = field.options[(i+1)%len(field.options)]
			return
		}
	}
	if len(field.options) > 0 {
		m.channelEditValues[field.key] = field.options[0]
	}
}

func (m *SetupWizardModel) cycleChannelFieldReverse(field channelFieldDef) {
	if field.options == nil {
		return
	}
	cur := m.channelEditValues[field.key]
	for i, opt := range field.options {
		if strings.EqualFold(opt, cur) {
			idx := (i - 1 + len(field.options)) % len(field.options)
			m.channelEditValues[field.key] = field.options[idx]
			return
		}
	}
	if len(field.options) > 0 {
		m.channelEditValues[field.key] = field.options[len(field.options)-1]
	}
}

func (m *SetupWizardModel) finishSimpleSetup() string {
	m.saving = true
	m.errorMsg = ""

	cfg := config.Defaults()

	// Enable auto-cookies if any platform was set up
	if m.cookieYTDone || m.cookieTWDone {
		cfg.Cookies.AutoEnabled = true
		var platforms []string
		if m.cookieYTDone {
			platforms = append(platforms, "youtube")
		}
		if m.cookieTWDone {
			platforms = append(platforms, "twitch")
		}
		cfg.Cookies.Platforms = platforms
	}

	// Channels
	cfg.Channels = m.channels

	if m.OnComplete != nil {
		if err := m.OnComplete(cfg); err != nil {
			m.errorMsg = fmt.Sprintf("Failed to save: %v", err)
			m.saving = false
			return ""
		}
	}

	m.saving = false

	// Trigger restart so all services re-init with new config
	if m.OnRestart != nil {
		m.OnRestart()
	}

	return "complete"
}

// --- Advanced Setup ---

func (m *SetupWizardModel) handleAdvancedKey(key string) string {
	if m.advancedForm != nil {
		// Form is active — only handle Esc for back navigation.
		// All other keys are handled by the form via UpdateComponents.
		if key == keyEsc {
			m.advancedForm = nil
			m.advancedInitCmd = nil
			m.mode = setupModeSelect
			return ""
		}
		return ""
	}

	// Channel editing mode (after form completes)
	if m.advancedFormDone {
		return m.handleAdvancedChannelKey(key)
	}

	return ""
}

func (m *SetupWizardModel) handleAdvancedChannelKey(key string) string {
	if m.channelMode == "edit" {
		return m.handleChannelEditKey(key)
	}

	if m.channelDeleteConf {
		if key == "d" || key == "D" {
			if m.channelIndex < len(m.channels) {
				m.channels = append(m.channels[:m.channelIndex], m.channels[m.channelIndex+1:]...)
				if m.channelIndex >= len(m.channels) && m.channelIndex > 0 {
					m.channelIndex--
				}
			}
			m.channelDeleteConf = false
		} else {
			m.channelDeleteConf = false
		}
		return ""
	}

	switch key {
	case keyEsc:
		// Back to mode select from channel editor
		m.advancedFormDone = false
		m.mode = setupModeSelect
	case keyUp:
		if m.channelIndex > 0 {
			m.channelIndex--
		}
	case keyDown:
		if m.channelIndex < len(m.channels)-1 {
			m.channelIndex++
		}
	case "a", "A":
		m.channelEditValues = map[string]string{
			"id": "", "name": "", "platform": "youtube",
			"enabled": "Yes", "terms": "",
			"include_non_live": "No", "quality_preference": "best",
		}
		m.channelEditField = 0
		m.channelIndex = len(m.channels)
		m.channelMode = "edit"
		m.updateTextInputForField()
	case keyEnter:
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			m.channelMode = "edit"
			m.channelEditValues = channelToValues(m.channels[m.channelIndex])
			m.channelEditField = 0
			m.updateTextInputForField()
		}
	case "d", "D":
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			m.channelDeleteConf = true
		}
	case keyTab:
		return m.finishAdvancedSetup()
	}
	return ""
}

func (m *SetupWizardModel) finishAdvancedSetup() string {
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

	netAccessMap := map[string]string{
		"Localhost": "localhost",
		"LAN":      "lan",
	}
	networkAccess := netAccessMap[m.values["networkAccess"]]
	if networkAccess == "" {
		networkAccess = "localhost"
	}

	cfg := config.Defaults()

	// Network
	cfg.Network.NetworkAccess = networkAccess
	if port := vNum("port"); port > 0 {
		cfg.Network.Port = port
	}
	cfg.Network.HTTPSEnabled = vBool("httpsEnabled", false)

	// Paths
	if s := v("logFilePath"); s != "" {
		cfg.Paths.LogFilePath = s
	}
	if s := v("databasePath"); s != "" {
		cfg.Paths.DatabasePath = s
	}
	if s := v("outputDir"); s != "" {
		cfg.Paths.OutputDirectory = s
	}
	if s := v("stagingDir"); s != "" {
		cfg.Paths.StagingDirectory = s
	}
	if s := v("ffmpegPath"); s != "" {
		cfg.Paths.FfmpegPath = s
	}

	// Logs
	if s := m.values["logLevel"]; s != "" {
		cfg.Logs.LogLevel = s
	}
	if n := vNum("logMaxSize"); n > 0 {
		cfg.Logs.LogMaxFileSize = n
	}
	if n := vNum("logMaxFiles"); n > 0 {
		cfg.Logs.LogMaxFiles = n
	}

	// Monitors
	if n := vNum("maxFeedItems"); n > 0 {
		cfg.Monitors.MaxFeedItems = n
	}
	if n := vNum("feedCheckInterval"); n > 0 {
		cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: float64(n)}
	}
	if s := v("hideAge"); s != "" {
		hideAge, _ := strconv.Atoi(s)
		if hideAge >= 0 {
			cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: float64(hideAge)}
		}
	}

	// Downloader
	if s := v("outputTemplate"); s != "" {
		cfg.Downloader.OutputTemplate = s
	}
	if n := vNum("numParallel"); n > 0 {
		cfg.Downloader.NumParallelDownloads = n
	}
	if n := vNum("maxRes"); n > 0 {
		cfg.Downloader.MaxVideoResolution = n
	}
	cfg.Downloader.Prefer60fps = vBool("prefer60fps", true)
	cfg.Downloader.DownloadChat = vBool("downloadChat", true)

	// Cookies
	if s := v("cookieFile"); s != "" {
		cfg.Cookies.CookieFile = s
	}
	cfg.Cookies.AutoEnabled = vBool("autoCookies", false)

	// Channels
	cfg.Channels = m.channels

	if m.OnComplete != nil {
		if err := m.OnComplete(cfg); err != nil {
			m.errorMsg = fmt.Sprintf("Failed to save: %v", err)
			m.saving = false
			return ""
		}
	}

	// Post-save actions (run before restart since they're fast and synchronous)
	if vBool("installYtdlpPlugin", false) && m.OnInstallYtdlp != nil {
		m.OnInstallYtdlp(cfg.Network.Port)
	}

	// Note: auto-cookies are enabled via config (auto_enabled: true).
	// The auto-cookie service will initialize on restart — no need to
	// call OnStartAutoCookie here (the restart would kill it immediately).

	m.saving = false

	// Trigger restart
	if m.OnRestart != nil {
		m.OnRestart()
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

	switch m.mode {
	case setupModeSelect:
		return m.viewModeSelect()
	case setupModeSimple:
		return m.viewSimple()
	case setupModeAdvanced:
		return m.viewAdvanced()
	}
	return ""
}

// --- Mode Selection View ---

func (m *SetupWizardModel) viewModeSelect() string {
	boxW := min(60, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4

	var lines []string

	// Header
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Welcome to Moombox"))
	lines = append(lines, DimStyle.Render("Choose how to set up your archiver"))
	lines = append(lines, "")

	// Quick Setup card
	quickPrefix := "  "
	quickColor := ColorWhite
	if m.modeChoice == 0 {
		quickPrefix = "> "
		quickColor = ColorCyan
	}
	quickStyle := lipgloss.NewStyle().Foreground(quickColor)
	if m.modeChoice == 0 {
		quickStyle = quickStyle.Bold(true)
	}
	lines = append(lines, quickStyle.Render(quickPrefix+"Quick Setup"))
	lines = append(lines, DimStyle.Render("   Defaults for everything. Set up cookies and channels."))
	lines = append(lines, "")

	// Advanced Setup card
	advPrefix := "  "
	advColor := ColorWhite
	if m.modeChoice == 1 {
		advPrefix = "> "
		advColor = ColorCyan
	}
	advStyle := lipgloss.NewStyle().Foreground(advColor)
	if m.modeChoice == 1 {
		advStyle = advStyle.Bold(true)
	}
	lines = append(lines, advStyle.Render(advPrefix+"Advanced Setup"))
	lines = append(lines, DimStyle.Render("   Walk through all configuration sections."))
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	lines = append(lines, DimStyle.Render("\u2191/\u2193: Select  Enter: Continue"))

	content := strings.Join(lines, "\n")

	h := m.height - 2
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

// --- Simplified Setup Views ---

func (m *SetupWizardModel) viewSimple() string {
	switch m.simpleStage {
	case setupSimpleCookies:
		return m.viewSimpleCookies()
	case setupSimpleChannels:
		return m.viewSimpleChannels()
	}
	return ""
}

func (m *SetupWizardModel) viewSimpleCookies() string {
	boxW := min(65, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4

	var lines []string

	// Header
	titleRendered := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Quick Setup")
	stepRendered := DimStyle.Render("Step 1/2")
	titlePad := contentW - runewidth.StringWidth("Quick Setup") - runewidth.StringWidth("Step 1/2")
	if titlePad < 1 {
		titlePad = 1
	}
	lines = append(lines, titleRendered+strings.Repeat(" ", titlePad)+stepRendered)

	// Step indicator
	step1 := lipgloss.NewStyle().Foreground(ColorCyan).Render("[>] 1")
	step2 := lipgloss.NewStyle().Foreground(ColorGray).Render("[ ] 2")
	lines = append(lines, step1+DimStyle.Render(" - ")+step2)

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Cookie Setup"))
	lines = append(lines, DimStyle.Render("Log in to platforms to enable cookie-based access"))
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

	if m.cookieActive {
		// Browser is open, waiting for login
		platformName := "YouTube"
		if m.cookiePlatform == "twitch" {
			platformName = "Twitch"
		}
		lines = append(lines, m.spinner.View()+" "+lipgloss.NewStyle().Foreground(ColorCyan).Render(
			fmt.Sprintf("Browser opened for %s login.", platformName)))
		lines = append(lines, "")
		lines = append(lines, "Sign in, then press "+
			lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Enter")+" to extract cookies.")
		lines = append(lines, DimStyle.Render("Press Esc to cancel."))
	} else {
		// Platform selection
		options := []struct {
			label  string
			done   bool
		}{
			{"YouTube", m.cookieYTDone},
			{"Twitch", m.cookieTWDone},
			{"Skip / Next", false},
		}

		for i, opt := range options {
			prefix := "  "
			color := ColorWhite
			if i == m.cookieFocus {
				prefix = "> "
				color = ColorCyan
			}
			label := opt.label
			if opt.done {
				label += " " + lipgloss.NewStyle().Foreground(ColorGreen).Render("\u2713")
			}
			style := lipgloss.NewStyle().Foreground(color)
			if i == m.cookieFocus {
				style = style.Bold(true)
			}
			lines = append(lines, style.Render(prefix+label))
			if i < len(options)-1 {
				lines = append(lines, "")
			}
		}
	}

	// Navigation
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	hintLeft := DimStyle.Render("Esc: Back")
	hintRight := DimStyle.Render("Enter: Select")
	gap := max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("Enter: Select"))
	lines = append(lines, hintLeft+strings.Repeat(" ", gap)+hintRight)

	content := strings.Join(lines, "\n")

	h := m.height - 2
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

func (m *SetupWizardModel) viewSimpleChannels() string {
	boxW := min(65, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4

	var lines []string

	// Header
	titleRendered := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Quick Setup")
	stepRendered := DimStyle.Render("Step 2/2")
	titlePad := contentW - runewidth.StringWidth("Quick Setup") - runewidth.StringWidth("Step 2/2")
	if titlePad < 1 {
		titlePad = 1
	}
	lines = append(lines, titleRendered+strings.Repeat(" ", titlePad)+stepRendered)

	// Step indicator
	step1 := lipgloss.NewStyle().Foreground(ColorGreen).Render("[+] 1")
	step2 := lipgloss.NewStyle().Foreground(ColorCyan).Render("[>] 2")
	lines = append(lines, step1+DimStyle.Render(" - ")+step2)

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Channels"))
	lines = append(lines, DimStyle.Render("Add channels to monitor for live streams"))
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

	if m.channelMode == "edit" {
		lines = append(lines, m.renderChannelEditor(contentW)...)
	} else {
		lines = append(lines, m.renderChannelList(contentW)...)
	}

	// Error
	if m.errorMsg != "" {
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}
	if m.saving {
		lines = append(lines, m.spinner.View()+" Saving configuration...")
	}

	// Navigation
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	if m.channelMode == "edit" {
		lines = append(lines, DimStyle.Render("Esc: Cancel  Enter: Save  \u2191/\u2193: Fields"))
	} else {
		hintLeft := DimStyle.Render("Esc: Back")
		navHint := DimStyle.Render("A: Add  Enter: Edit  D: Delete  ")
		finishHint := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Tab: Finish")
		rightSide := navHint + finishHint
		gap := max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("A: Add  Enter: Edit  D: Delete  Tab: Finish"))
		lines = append(lines, hintLeft+strings.Repeat(" ", gap)+rightSide)
	}

	content := strings.Join(lines, "\n")

	h := m.height - 2
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

// --- Advanced Setup View ---

func (m *SetupWizardModel) viewAdvanced() string {
	boxW := min(72, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4

	h := m.height - 2
	if h < 10 {
		h = 10
	}

	if m.advancedForm != nil {
		// Render huh form
		m.advancedForm = m.advancedForm.WithWidth(contentW).WithHeight(h - 4)

		var lines []string
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Advanced Setup"))
		lines = append(lines, "")
		lines = append(lines, m.advancedForm.View())

		if m.errorMsg != "" {
			lines = append(lines, ErrorStyle.Render(m.errorMsg))
		}
		lines = append(lines, DimStyle.Render("Esc: Back to mode select"))

		content := strings.Join(lines, "\n")

		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorCyan).
			Width(contentW).
			Height(h).
			Render(content)

		return centerBox(box, m.width, m.height)
	}

	if m.advancedFormDone {
		// Render channel editor
		var lines []string
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Advanced Setup \u2014 Channels"))
		lines = append(lines, DimStyle.Render("Add channels to monitor for live streams"))
		lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

		if m.channelMode == "edit" {
			lines = append(lines, m.renderChannelEditor(contentW)...)
		} else {
			lines = append(lines, m.renderChannelList(contentW)...)
		}

		if m.errorMsg != "" {
			lines = append(lines, ErrorStyle.Render(m.errorMsg))
		}
		if m.saving {
			lines = append(lines, m.spinner.View()+" Saving configuration...")
		}

		// Navigation hints
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
		if m.channelMode == "edit" {
			lines = append(lines, DimStyle.Render("Esc: Cancel  Enter: Save  \u2191/\u2193: Fields"))
		} else {
			hintLeft := DimStyle.Render("Esc: Back")
			navHint := DimStyle.Render("A: Add  Enter: Edit  D: Delete  ")
			finishHint := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Tab: Finish")
			rightSide := navHint + finishHint
			gap := max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("A: Add  Enter: Edit  D: Delete  Tab: Finish"))
			lines = append(lines, hintLeft+strings.Repeat(" ", gap)+rightSide)
		}

		content := strings.Join(lines, "\n")

		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorCyan).
			Width(contentW).
			Height(h).
			Render(content)

		return centerBox(box, m.width, m.height)
	}

	return ""
}

// --- Shared Channel Rendering ---

func (m *SetupWizardModel) renderChannelList(contentW int) []string {
	var lines []string

	if len(m.channels) == 0 {
		lines = append(lines, DimStyle.Render("No channels added yet."))
		lines = append(lines, DimStyle.Render("Press A to add a channel."))
	} else {
		for i, ch := range m.channels {
			prefix := "  "
			color := ColorWhite
			if i == m.channelIndex {
				prefix = "> "
				color = ColorCyan
			}
			name := ch.Name
			if name == "" {
				name = ch.ID
			}
			platform := ch.GetPlatform()
			platBadge := lipgloss.NewStyle().Foreground(ColorUpcoming).Render("[" + platform + "]")
			if platform == "twitch" {
				platBadge = lipgloss.NewStyle().Foreground(ColorTwitch).Render("[twitch]")
			}

			style := lipgloss.NewStyle().Foreground(color)
			if i == m.channelIndex {
				style = style.Bold(true)
			}
			lines = append(lines, style.Render(prefix+name)+" "+platBadge)
		}
	}

	if m.channelDeleteConf {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
			"Press D again to confirm delete"))
	}

	return lines
}

func (m *SetupWizardModel) renderChannelEditor(contentW int) []string {
	var lines []string
	fields := m.visibleSetupChannelFields()

	for i, f := range fields {
		isFocused := i == m.channelEditField
		val := m.channelEditValues[f.key]

		prefix := "  "
		color := ColorWhite
		if isFocused {
			prefix = "> "
			color = ColorCyan
		}

		labelStyle := lipgloss.NewStyle().Foreground(color)
		if isFocused {
			labelStyle = labelStyle.Bold(true)
		}

		if f.ftype == fieldToggle || f.ftype == fieldCycle {
			lines = append(lines, labelStyle.Render(prefix+f.label)+": "+
				renderSetupOptionSelector(f.options, val, isFocused))
		} else if isFocused {
			m.textInput.Width = contentW - runewidth.StringWidth(prefix+f.label+": ") - 1
			lines = append(lines, labelStyle.Render(prefix+f.label)+": "+m.textInput.View())
		} else {
			lines = append(lines, labelStyle.Render(prefix+f.label)+": "+renderInactiveInput(val, contentW-runewidth.StringWidth(prefix+f.label+": "), ColorWhite))
		}

		if f.help != "" && isFocused {
			lines = append(lines, "   "+DimStyle.Render(f.help))
		}
	}

	return lines
}

// renderSetupOptionSelector renders toggle/cycle options like [Yes] / No.
func renderSetupOptionSelector(options []string, selected string, focused bool) string {
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


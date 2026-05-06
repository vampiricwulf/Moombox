package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// cookieCountdownTickMsg is sent every second to decrement the cookie timeout countdown.
type cookieCountdownTickMsg struct{}

// cookieCountdownTick returns a tea.Cmd that fires cookieCountdownTickMsg after 1 second.
func cookieCountdownTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return cookieCountdownTickMsg{}
	})
}

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
	setupFieldText setupFieldType = iota
	setupFieldNumber
	setupFieldToggle
	setupFieldCycle
	setupFieldPassword
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
}

// Advanced setup steps — 8 sections matching settings.go
var advancedSetupSteps = []setupStepDef{
	{
		title:    "Network",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"port", "Dashboard port", "774", "Web dashboard & API port (tries nearby if busy)", setupFieldNumber, nil},
			{"networkAccess", "Network access", "Localhost", "Who can access the dashboard", setupFieldCycle, []string{"Localhost", "LAN", "External"}},
			{"password", "Password", "", "Required for external access (min 8 chars, stored hashed)", setupFieldPassword, nil},
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
			{"numParallel", "Parallel downloads", "2", "2-4 recommended, higher uses more CPU/network", setupFieldNumber, nil},
			{"downloadChat", "Download chat", "Yes", "Save live chat as JSON alongside video", setupFieldToggle, []string{"Yes", "No"}},
			{"segmentRetryDelayCap", "Retry delay cap (sec)", "60", "Max seconds between segment retry attempts", setupFieldNumber, nil},
			{"segmentLiveCheckRetries", "Live check retries", "16", "Retries before declaring stream ended", setupFieldNumber, nil},
		},
	},
	{
		title:    "Cookies",
		subtitle: "Leave fields empty to use defaults",
		fields: []setupFieldDef{
			{"cookieFile", "Cookie file (manual)", "./cookies.txt", "Netscape-format cookie file", setupFieldText, nil},
		},
	},
	{
		title:    "Cookie Login",
		subtitle: "Log in via browser",
		fields:   nil, // Interactive cookie browser login
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
	isDark  bool // mirrors App.isDark for theme construction

	mode       setupMode
	modeChoice int // 0=Quick, 1=Advanced on mode selection screen
	values     map[string]string
	errorMsg   string

	// Advanced setup huh form
	advancedForm     *huh.Form
	advancedFormDone bool
	advancedInitCmd  tea.Cmd // pending Init cmd from buildAdvancedForm

	// Simplified setup state
	simpleStage     setupSimpleStage
	cookieFocus     int  // 0=YouTube, 1=Twitch, 2=Skip
	cookieActive    bool // true when browser is open
	cookieFinishing bool // true while async cookie extraction runs
	cookiePlatform  string
	cookieYTDone    bool
	cookieTWDone    bool
	cookieCountdown int  // seconds remaining for cookie timeout
	cookieTimedOut  bool // true when countdown reaches 0

	// Channel sub-editor (shared by simple and advanced)
	channels          []config.ChannelConfig
	channelIndex      int
	channelMode       string // "list" or "edit"
	channelEditValues map[string]string
	channelEditField  int
	channelDeleteConf bool

	// Esc confirmation state for advanced mode (prevents accidental data loss)
	escConfirm bool

	// Advanced mode: interactive cookie step (between form and channels)
	advancedCookieDone bool

	// FFmpeg status (checked on open)
	ffmpegStatus string // "found: v7.1", "not found", or "" if not checked

	// Saving state (async save in progress)
	saving        bool
	pendingConfig *config.MoomboxConfig // config to save asynchronously
	pendingYtdlp  bool                  // install yt-dlp plugin after save

	// Shared text input component (holds the currently-active text field)
	textInput textinput.Model

	// Loading spinner (shown during cookie extraction or saving)
	spinner spinner.Model

	// Callbacks for finishing setup
	OnComplete         func(cfg *config.MoomboxConfig) error
	OnInstallYtdlp     func(port int, httpsEnabled bool)
	OnStartAutoCookie  func(platform string) error
	OnFinishAutoCookie func() (bool, bool, error)
	OnCancelAutoCookie func()
	OnRestart          func()
	OnCheckFFmpeg      func() (valid bool, version string) // returns FFmpeg status
	OnHashPassword     func(password string) (string, error)
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
	m.errorMsg = ""
	m.advancedForm = nil
	m.advancedFormDone = false
	m.advancedInitCmd = nil
	m.simpleStage = setupSimpleCookies
	m.cookieFocus = 0
	m.cookieActive = false
	m.cookieFinishing = false
	m.cookiePlatform = ""
	m.cookieYTDone = false
	m.cookieTWDone = false
	m.cookieCountdown = 0
	m.cookieTimedOut = false
	m.channels = nil
	m.channelIndex = 0
	m.channelMode = "list"
	m.channelEditValues = nil
	m.channelEditField = 0
	m.channelDeleteConf = false
	m.escConfirm = false
	m.advancedCookieDone = false
	m.saving = false
	m.textInput.Blur()

	// Check FFmpeg availability
	if m.OnCheckFFmpeg != nil {
		valid, version := m.OnCheckFFmpeg()
		if valid {
			m.ffmpegStatus = "\u2713 " + version
		} else {
			m.ffmpegStatus = "\u2717 not found"
		}
	}
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

	// Update text input width if channel editor is active
	if m.channelMode == "edit" && m.textInput.Focused() {
		m.updateTextInputWidth()
	}
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
			case setupFieldPassword:
				fields = append(fields,
					huh.NewInput().
						Key(f.key).
						Title(f.label).
						Description(f.help).
						Placeholder(f.defaultDisplay).
						EchoMode(huh.EchoModePassword).
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
		WithTheme(moomboxTheme(m.isDark)).
		WithShowHelp(false)
	m.advancedForm.SubmitCmd = nil // Don't quit on submit
	m.advancedInitCmd = m.advancedForm.Init()
}

// UpdateComponents routes tea.Msg to the embedded textinput, huh form, or spinner and syncs.
func (m *SetupWizardModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}

	// Handle cookie countdown tick
	if _, ok := msg.(cookieCountdownTickMsg); ok {
		if m.cookieActive && !m.cookieFinishing && m.cookieCountdown > 0 {
			m.cookieCountdown--
			if m.cookieCountdown <= 0 {
				m.cookieTimedOut = true
				m.cookieActive = false
				if m.OnCancelAutoCookie != nil {
					m.OnCancelAutoCookie()
				}
				return nil
			}
			return cookieCountdownTick()
		}
		return nil
	}

	// Route spinner when cookie extraction browser is active
	if m.cookieActive {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}

	// Advanced mode: route to huh form or channel editor textinput
	if m.mode == setupModeAdvanced {
		if m.advancedForm != nil {
			model, cmd := m.advancedForm.Update(msg)
			m.advancedForm = model.(*huh.Form)
			if m.advancedForm.State == huh.StateCompleted {
				// Transition to interactive cookie step
				m.advancedForm = nil
				m.advancedFormDone = true
				m.advancedCookieDone = false
				m.escConfirm = false
				m.cookieFocus = 0
			}
			return cmd
		}
		// Channel editing mode — route to textinput (only after cookies are done)
		if m.advancedFormDone && m.advancedCookieDone && m.channelMode == "edit" && m.textInput.Focused() {
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
				m.updateTextInputWidth()
				return
			}
		}
		m.textInput.Blur()
		return
	}

	m.textInput.Blur()
}

// updateTextInputWidth sets the text input width based on current dimensions and field context.
func (m *SetupWizardModel) updateTextInputWidth() {
	if m.width == 0 {
		return
	}

	// Determine box width based on which mode we're in
	maxW := 65
	if m.mode == setupModeAdvanced {
		maxW = 72
	}
	_, contentW := dialogBox(maxW, m.width)

	// Calculate label width for the current field
	fields := m.visibleSetupChannelFields()
	if m.channelEditField < len(fields) {
		f := fields[m.channelEditField]
		prefix := "> " // focused field always has "> " prefix
		m.textInput.SetWidth(contentW - runewidth.StringWidth(prefix+f.label+": "))
	}
}

// HandleKey processes key input. Returns "save" to trigger async config save,
// "finish_cookie" for async cookie extraction, or "" for no action.
func (m *SetupWizardModel) HandleKey(key string) string {
	// Block all input during async save (prevents duplicate saves and error clearing)
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
		if m.modeChoice < 2 {
			m.modeChoice++
		}
	case keyEnter, keyTab:
		switch m.modeChoice {
		case 0: // Quick Setup
			m.mode = setupModeSimple
			m.simpleStage = setupSimpleCookies
			m.cookieFocus = 0
		case 1: // Advanced Setup
			m.mode = setupModeAdvanced
			m.buildAdvancedForm()
		case 2: // Use Defaults (skip)
			return m.finishWithDefaults()
		}
	}
	return ""
}

// finishWithDefaults saves a default config and triggers restart.
func (m *SetupWizardModel) finishWithDefaults() string {
	m.errorMsg = ""
	m.saving = true
	m.pendingConfig = config.Defaults()
	m.pendingYtdlp = false
	return "save"
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
		if m.cookieFinishing {
			return "" // extraction in progress, ignore keys
		}
		switch key {
		case keyEnter:
			// Signal app.go to run async cookie extraction
			m.cookieFinishing = true
			return "finish_cookie"
		case keyEsc:
			if m.OnCancelAutoCookie != nil {
				m.OnCancelAutoCookie()
			}
			m.cookieActive = false
			m.cookiePlatform = ""
		}
		return ""
	}

	if m.cookieTimedOut {
		switch key {
		case "r", "R":
			m.cookieTimedOut = false
			if m.OnStartAutoCookie != nil {
				if err := m.OnStartAutoCookie(m.cookiePlatform); err != nil {
					m.errorMsg = fmt.Sprintf("Cookie retry: %v", err)
					return ""
				}
				m.cookieActive = true
				m.cookieCountdown = 60
				m.spinner = newSpinner()
			}
			return ""
		case "s", "S":
			m.cookieTimedOut = false
			m.cookiePlatform = ""
			return ""
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
				if err := m.OnStartAutoCookie("youtube"); err != nil {
					m.errorMsg = fmt.Sprintf("YouTube cookies: %v", err)
				} else {
					m.cookieActive = true
					m.cookiePlatform = "youtube"
					m.cookieCountdown = 60
					m.cookieTimedOut = false
					m.spinner = newSpinner()
				}
			}
		case 1: // Twitch
			if m.OnStartAutoCookie != nil {
				if err := m.OnStartAutoCookie("twitch"); err != nil {
					m.errorMsg = fmt.Sprintf("Twitch cookies: %v", err)
				} else {
					m.cookieActive = true
					m.cookiePlatform = "twitch"
					m.cookieCountdown = 60
					m.cookieTimedOut = false
					m.spinner = newSpinner()
				}
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
	return m.handleChannelListKey(key,
		func() string { m.simpleStage = setupSimpleCookies; return "" },
		func() string { return m.finishSimpleSetup() },
	)
}

// handleChannelListKey is the shared channel list handler used by both simple and advanced flows.
// onEsc is called when Esc is pressed, onFinish when Tab is pressed.
func (m *SetupWizardModel) handleChannelListKey(key string, onEsc func() string, onFinish func() string) string {
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
		return onEsc()
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
		return onFinish()
	}
	return ""
}

// visibleSetupChannelFields returns channel fields filtered by platform.
func (m *SetupWizardModel) visibleSetupChannelFields() []channelFieldDef {
	return filterChannelFieldsByPlatform(channelFields, m.channelEditValues)
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
		id := strings.TrimSpace(m.channelEditValues["id"])
		if id == "" {
			m.errorMsg = "Channel ID is required"
			return ""
		}
		// Check for duplicate channel ID
		for i, existing := range m.channels {
			if strings.EqualFold(existing.ID, id) && i != m.channelIndex {
				m.errorMsg = fmt.Sprintf("Channel %q already added", id)
				return ""
			}
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
	case keyLeft:
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelFieldReverse(field)
			m.clampChannelEditField()
		}
		return ""
	case keyRight:
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelField(field)
			m.clampChannelEditField()
		}
		return ""
	}
	return ""
}

// clampChannelEditField adjusts channelEditField when cycling platform may
// have changed the visible field count (e.g., youtube→twitch drops include_non_live).
func (m *SetupWizardModel) clampChannelEditField() {
	fields := m.visibleSetupChannelFields()
	if m.channelEditField >= len(fields) && len(fields) > 0 {
		m.channelEditField = len(fields) - 1
		m.updateTextInputForField()
	}
}

func (m *SetupWizardModel) cycleChannelField(field channelFieldDef) {
	cycleFieldOption(m.channelEditValues, field.key, field.options, 1)
}

func (m *SetupWizardModel) cycleChannelFieldReverse(field channelFieldDef) {
	cycleFieldOption(m.channelEditValues, field.key, field.options, -1)
}

func (m *SetupWizardModel) finishSimpleSetup() string {
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
		cfg.Cookies.ActivePlatforms = platforms
	}

	// Channels
	cfg.Channels = m.channels

	m.saving = true
	m.pendingConfig = cfg
	m.pendingYtdlp = false
	return "save"
}

// --- Advanced Setup ---

func (m *SetupWizardModel) handleAdvancedKey(key string) string {
	// Clear Esc confirmation on any non-Esc key
	if key != keyEsc && m.escConfirm {
		m.escConfirm = false
	}

	if m.advancedForm != nil {
		// Form is active — double-Esc to abandon (prevents accidental data loss).
		// All other keys are handled by the form via UpdateComponents.
		if key == keyEsc {
			if m.escConfirm {
				m.advancedForm = nil
				m.advancedInitCmd = nil
				m.mode = setupModeSelect
				m.escConfirm = false
				return ""
			}
			m.escConfirm = true
			return ""
		}
		return ""
	}

	// Interactive cookie step (after form, before channels)
	if m.advancedFormDone && !m.advancedCookieDone {
		return m.handleAdvancedCookieKey(key)
	}

	// Channel editing mode (after cookies)
	if m.advancedFormDone && m.advancedCookieDone {
		return m.handleAdvancedChannelKey(key)
	}

	return ""
}

func (m *SetupWizardModel) handleAdvancedCookieKey(key string) string {
	// Reuse the simple cookie key handler with custom Esc/Next behavior
	if m.cookieActive {
		if m.cookieFinishing {
			return "" // extraction in progress, ignore keys
		}
		switch key {
		case keyEnter:
			m.cookieFinishing = true
			return "finish_cookie"
		case keyEsc:
			if m.OnCancelAutoCookie != nil {
				m.OnCancelAutoCookie()
			}
			m.cookieActive = false
			m.cookiePlatform = ""
		}
		return ""
	}

	if m.cookieTimedOut {
		switch key {
		case "r", "R":
			m.cookieTimedOut = false
			if m.OnStartAutoCookie != nil {
				if err := m.OnStartAutoCookie(m.cookiePlatform); err != nil {
					m.errorMsg = fmt.Sprintf("Cookie retry: %v", err)
					return ""
				}
				m.cookieActive = true
				m.cookieCountdown = 60
				m.spinner = newSpinner()
			}
			return ""
		case "s", "S":
			m.cookieTimedOut = false
			m.cookiePlatform = ""
			return ""
		}
		return ""
	}

	switch key {
	case keyEsc:
		// Go back to the form (rebuild with current values preserved)
		m.advancedFormDone = false
		m.buildAdvancedForm()
		return ""
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
				if err := m.OnStartAutoCookie("youtube"); err != nil {
					m.errorMsg = fmt.Sprintf("YouTube cookies: %v", err)
				} else {
					m.cookieActive = true
					m.cookiePlatform = "youtube"
					m.cookieCountdown = 60
					m.cookieTimedOut = false
					m.spinner = newSpinner()
				}
			}
		case 1: // Twitch
			if m.OnStartAutoCookie != nil {
				if err := m.OnStartAutoCookie("twitch"); err != nil {
					m.errorMsg = fmt.Sprintf("Twitch cookies: %v", err)
				} else {
					m.cookieActive = true
					m.cookiePlatform = "twitch"
					m.cookieCountdown = 60
					m.cookieTimedOut = false
					m.spinner = newSpinner()
				}
			}
		case 2: // Skip / Next → advance to channels
			m.advancedCookieDone = true
			m.channelMode = "list"
			// Don't reset m.channels — preserve channels if user went back from channels to cookies
		}
	}
	return ""
}

func (m *SetupWizardModel) handleAdvancedChannelKey(key string) string {
	return m.handleChannelListKey(key,
		func() string {
			// Go back to cookie step (not destructive — channels are preserved)
			m.advancedCookieDone = false
			m.cookieFocus = 2 // Focus "Skip / Next" since user already passed cookies
			return ""
		},
		func() string { return m.finishAdvancedSetup() },
	)
}

func (m *SetupWizardModel) finishAdvancedSetup() string {
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
		"LAN":       "lan",
		"External":  "external",
	}
	networkAccess := netAccessMap[m.values["networkAccess"]]
	if networkAccess == "" {
		networkAccess = "localhost"
	}

	// External access requires a password (min 8 chars)
	password := v("password")
	if networkAccess == "external" && len(password) < 8 {
		m.errorMsg = "A password (min 8 characters) is required for external access"
		return ""
	}

	// Validate numeric ranges before building config (must match API validateConfigUpdates)
	if port := vNum("port"); port != 0 && (port < 1 || port > 65535) {
		m.errorMsg = "Network: Port must be between 1 and 65535"
		return ""
	}
	if n := vNum("logMaxSize"); n != 0 && (n < 1024 || n > 1073741824) {
		m.errorMsg = "Logs: Max file size must be between 1024 and 1073741824 bytes"
		return ""
	}
	if n := vNum("logMaxFiles"); n != 0 && (n < 1 || n > 100) {
		m.errorMsg = "Logs: Max files must be between 1 and 100"
		return ""
	}
	if n := vNum("maxFeedItems"); n != 0 && (n < 1 || n > 100) {
		m.errorMsg = "Monitors: Max feed items must be between 1 and 100"
		return ""
	}
	if n := vNum("feedCheckInterval"); n != 0 && (n < 1 || n > 1440) {
		m.errorMsg = "Monitors: Feed check interval must be between 1 and 1440 minutes"
		return ""
	}
	if n := vNum("numParallel"); n != 0 && n < 1 {
		m.errorMsg = "Downloader: Parallel downloads must be at least 1"
		return ""
	}
	if n := vNum("maxRes"); n != 0 && n < 1 {
		m.errorMsg = "Downloader: Max resolution must be at least 1"
		return ""
	}
	if n := vNum("segmentRetryDelayCap"); n != 0 && (n < 1 || n > 300) {
		m.errorMsg = "Downloader: Retry delay cap must be between 1 and 300 seconds"
		return ""
	}
	if n := vNum("segmentLiveCheckRetries"); n != 0 && (n < 1 || n > 100) {
		m.errorMsg = "Downloader: Live check retries must be between 1 and 100"
		return ""
	}

	cfg := config.Defaults()

	// Network
	cfg.Network.NetworkAccess = networkAccess
	if port := vNum("port"); port > 0 {
		cfg.Network.Port = port
	}
	cfg.Network.HTTPSEnabled = vBool("httpsEnabled", false)
	if networkAccess == "external" && password != "" {
		// Hash password before saving (only relevant for external access)
		if m.OnHashPassword != nil {
			hash, err := m.OnHashPassword(password)
			if err != nil {
				m.errorMsg = fmt.Sprintf("Failed to hash password: %v", err)
				return ""
			}
			cfg.Network.PasswordHash = hash
		} else {
			m.errorMsg = "Password hashing unavailable"
			return ""
		}
	}

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
	if n := vNum("segmentRetryDelayCap"); n > 0 {
		cfg.Downloader.SegmentRetryDelayCap = n
	}
	if n := vNum("segmentLiveCheckRetries"); n > 0 {
		cfg.Downloader.SegmentLiveCheckRetries = n
	}

	// Cookies
	if s := v("cookieFile"); s != "" {
		cfg.Cookies.CookieFile = s
	}
	// Enable auto-cookies if any platform was set up via browser login
	if m.cookieYTDone || m.cookieTWDone {
		cfg.Cookies.AutoEnabled = true
		var platforms []string
		if m.cookieYTDone {
			platforms = append(platforms, "youtube")
		}
		if m.cookieTWDone {
			platforms = append(platforms, "twitch")
		}
		cfg.Cookies.ActivePlatforms = platforms
	}

	// Channels
	cfg.Channels = m.channels

	m.saving = true
	m.pendingConfig = cfg
	m.pendingYtdlp = vBool("installYtdlpPlugin", false)
	return "save"
}

// View renders the setup wizard.
func (m *SetupWizardModel) View() string {
	if !m.visible {
		return ""
	}
	if m.width == 0 || m.height == 0 {
		return ""
	}

	// Show saving indicator overlay
	if m.saving {
		boxW, _ := dialogBox(40, m.width)
		h := max(m.height-2, 10)
		content := "\n\n" + lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).
			Render("  Saving configuration...")
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorCyan).
			Width(boxW).
			Height(h + 2).
			Render(content)
		return centerBox(box, m.width, m.height)
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
	boxW, contentW := dialogBox(60, m.width)

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
	lines = append(lines, quickStyle.Render(quickPrefix+"Quick Setup (recommended)"))
	lines = append(lines, DimStyle.Render("   Best for most users — takes ~2 minutes"))
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
	lines = append(lines, DimStyle.Render("   Full control over every setting"))
	lines = append(lines, "")

	// Use Defaults card
	defPrefix := "  "
	defColor := ColorWhite
	if m.modeChoice == 2 {
		defPrefix = "> "
		defColor = ColorCyan
	}
	defStyle := lipgloss.NewStyle().Foreground(defColor)
	if m.modeChoice == 2 {
		defStyle = defStyle.Bold(true)
	}
	lines = append(lines, defStyle.Render(defPrefix+"Use Defaults"))
	lines = append(lines, DimStyle.Render("   Save default config and start. Configure later in settings."))
	lines = append(lines, "")

	// FFmpeg status
	if m.ffmpegStatus != "" {
		lines = append(lines, DimStyle.Render("FFmpeg: "+m.ffmpegStatus))
		lines = append(lines, "")
	}

	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	lines = append(lines, DimStyle.Render("\u2191/\u2193: Select  Enter: Continue"))

	content := strings.Join(lines, "\n")

	h := max(m.height-2, 10)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(h + 2).
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
	boxW, contentW := dialogBox(65, m.width)

	var lines []string

	// Header
	titleRendered := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Quick Setup")
	stepRendered := DimStyle.Render("Step 1/2")
	titlePad := max(contentW-runewidth.StringWidth("Quick Setup")-runewidth.StringWidth("Step 1/2"), 1)
	lines = append(lines, titleRendered+strings.Repeat(" ", titlePad)+stepRendered)

	// Step indicator
	step1 := lipgloss.NewStyle().Foreground(ColorCyan).Render("[>] 1")
	step2 := lipgloss.NewStyle().Foreground(ColorGray).Render("[ ] 2")
	lines = append(lines, step1+DimStyle.Render(" - ")+step2)

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Cookie Setup"))
	lines = append(lines, DimStyle.Render("Log in to platforms to enable cookie-based access"))
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

	if m.cookieTimedOut {
		lines = append(lines, "")
		lines = append(lines, YellowStyle.Render(
			"Cookie extraction timed out."))
		lines = append(lines, "")
		lines = append(lines, "  R  Try Again")
		lines = append(lines, "  S  Skip")
	} else if m.cookieActive {
		platformName := "YouTube"
		if m.cookiePlatform == "twitch" {
			platformName = "Twitch"
		}
		if m.cookieFinishing {
			// Extracting cookies from browser
			lines = append(lines, m.spinner.View()+" "+lipgloss.NewStyle().Foreground(ColorCyan).Render(
				fmt.Sprintf("Extracting %s cookies...", platformName)))
		} else {
			// Browser is open, waiting for login — show countdown
			countdownText := fmt.Sprintf("Waiting for %s login... (%ds remaining)", platformName, m.cookieCountdown)
			if m.cookieCountdown <= 10 {
				lines = append(lines, m.spinner.View()+" "+YellowStyle.Render(countdownText))
			} else {
				lines = append(lines, m.spinner.View()+" "+lipgloss.NewStyle().Foreground(ColorCyan).Render(countdownText))
			}
			lines = append(lines, "")
			lines = append(lines, "Sign in, then press "+
				lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Enter")+" to extract cookies.")
			lines = append(lines, DimStyle.Render("Press Esc to cancel."))
		}
	} else {
		// Platform selection
		options := []struct {
			label string
			done  bool
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

	// Error
	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	// Navigation
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	hintLeft := DimStyle.Render("Esc: Back")
	hintRight := DimStyle.Render("Enter: Select")
	gap := max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("Enter: Select"))
	lines = append(lines, hintLeft+strings.Repeat(" ", gap)+hintRight)

	content := strings.Join(lines, "\n")

	h := max(m.height-2, 10)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(h + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
}

func (m *SetupWizardModel) viewSimpleChannels() string {
	boxW, contentW := dialogBox(65, m.width)

	var lines []string

	// Header
	titleRendered := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Quick Setup")
	stepRendered := DimStyle.Render("Step 2/2")
	titlePad := max(contentW-runewidth.StringWidth("Quick Setup")-runewidth.StringWidth("Step 2/2"), 1)
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

	h := max(m.height-2, 10)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(h + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
}

// templatePreview renders a sample output path from a template string.
func templatePreview(value string) string {
	if value == "" {
		return ""
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
}

// --- Advanced Setup View ---

func (m *SetupWizardModel) viewAdvanced() string {
	boxW, contentW := dialogBox(72, m.width)

	h := max(m.height-2, 10)

	if m.advancedForm != nil {
		// Render huh form
		m.advancedForm = m.advancedForm.WithWidth(contentW).WithHeight(h - 4)

		var lines []string
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Advanced Setup"))
		lines = append(lines, "")
		lines = append(lines, m.advancedForm.View())

		// Template preview — only when the Paths group is visible
		if focused := m.advancedForm.GetFocusedField(); focused != nil {
			switch focused.GetKey() {
			case "outputDir", "outputTemplate", "stagingDir", "databasePath", "ffmpegPath":
				val := m.values["outputTemplate"]
				if val == "" {
					val = "${channel}/${start_date} ${title} [${id}]"
				}
				if preview := templatePreview(val); preview != "" {
					lines = append(lines, DimStyle.Render(preview))
				}
			}
		}

		if m.errorMsg != "" {
			lines = append(lines, ErrorStyle.Render(m.errorMsg))
		}
		if m.escConfirm {
			lines = append(lines, YellowStyle.Render(
				"Press Esc again to abandon setup"))
		} else {
			lines = append(lines, DimStyle.Render("Esc: Back to mode select"))
		}

		content := strings.Join(lines, "\n")

		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorCyan).
			Width(boxW).
			Height(h + 2).
			Render(content)

		return centerBox(box, m.width, m.height)
	}

	// Interactive cookie step (after form, before channels)
	if m.advancedFormDone && !m.advancedCookieDone {
		return m.viewAdvancedCookies(contentW, boxW, h)
	}

	// Channel editor (after cookies)
	if m.advancedFormDone && m.advancedCookieDone {
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
			Width(boxW).
			Height(h + 2).
			Render(content)

		return centerBox(box, m.width, m.height)
	}

	return ""
}

func (m *SetupWizardModel) viewAdvancedCookies(contentW, boxW, h int) string {
	var lines []string

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Advanced Setup \u2014 Cookie Login"))
	lines = append(lines, DimStyle.Render("Log in to platforms to enable cookie-based access"))
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

	if m.cookieTimedOut {
		lines = append(lines, "")
		lines = append(lines, YellowStyle.Render(
			"Cookie extraction timed out."))
		lines = append(lines, "")
		lines = append(lines, "  R  Try Again")
		lines = append(lines, "  S  Skip")
	} else if m.cookieActive {
		platformName := "YouTube"
		if m.cookiePlatform == "twitch" {
			platformName = "Twitch"
		}
		if m.cookieFinishing {
			lines = append(lines, m.spinner.View()+" "+lipgloss.NewStyle().Foreground(ColorCyan).Render(
				fmt.Sprintf("Extracting %s cookies...", platformName)))
		} else {
			// Browser is open, waiting for login — show countdown
			countdownText := fmt.Sprintf("Waiting for %s login... (%ds remaining)", platformName, m.cookieCountdown)
			if m.cookieCountdown <= 10 {
				lines = append(lines, m.spinner.View()+" "+YellowStyle.Render(countdownText))
			} else {
				lines = append(lines, m.spinner.View()+" "+lipgloss.NewStyle().Foreground(ColorCyan).Render(countdownText))
			}
			lines = append(lines, "")
			lines = append(lines, "Sign in, then press "+
				lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Enter")+" to extract cookies.")
			lines = append(lines, DimStyle.Render("Press Esc to cancel."))
		}
	} else {
		options := []struct {
			label string
			done  bool
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

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	hintLeft := DimStyle.Render("Esc: Back")
	hintRight := DimStyle.Render("Enter: Select")
	gap := max(1, contentW-runewidth.StringWidth("Esc: Back")-runewidth.StringWidth("Enter: Select"))
	lines = append(lines, hintLeft+strings.Repeat(" ", gap)+hintRight)

	content := strings.Join(lines, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(h + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
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
		lines = append(lines, YellowStyle.Render(
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

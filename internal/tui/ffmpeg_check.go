package tui

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// ffmpegMode identifies which view the FFmpeg check overlay is in.
type ffmpegMode int

const (
	ffmpegMain    ffmpegMode = iota // Main menu: Install / Custom path / Quit
	ffmpegInstall                   // Install sub-menu: Choco / Choco+Install / Winget / Cancel
	ffmpegCustom                    // Custom path text entry
	ffmpegReview                    // Script review: Trust / Distrust
	ffmpegManual                    // Manual install: download link + custom path
)

// FFmpegCheckModel manages the FFmpeg validation overlay.
type FFmpegCheckModel struct {
	visible bool
	width   int
	height  int

	mode ffmpegMode

	// Shared text input component
	textInput textinput.Model

	// Custom path
	customPath   string
	customResult string // result message after check
	customValid  bool

	// Install state
	installing    bool
	installResult string
	installError  bool

	// Success state — any keypress dismisses the overlay
	successDismiss bool

	// Version warning (e.g. outdated FFmpeg)
	warning string

	// Script review state (elevation required)
	reviewScript   string
	reviewToken    string
	reviewFocus    int // 0=Trust, 1=Distrust
	reviewViewport viewport.Model

	// Manual install state
	manualPath   string
	manualResult string
	manualValid  bool

	// Path check in progress (custom or manual mode)
	checking bool

	// Loading spinner
	spinner spinner.Model

	// Huh form for main/install menu selection
	form     *huh.Form
	formInit tea.Cmd // pending Init cmd from form creation
	values   map[string]string
	isDark   bool

	// Callbacks — only OnCheckPrereqs runs synchronously (fast LookPath),
	// the others are dispatched as tea.Cmd by App.
	OnCheckPrereqs func() (bool, bool)
}

// NewFFmpegCheckModel creates a new FFmpeg check model.
func NewFFmpegCheckModel() *FFmpegCheckModel {
	return &FFmpegCheckModel{
		textInput: newTextInput(),
		spinner:   newSpinner(),
	}
}

// Open shows the FFmpeg check overlay.
func (m *FFmpegCheckModel) Open() {
	m.visible = true
	m.mode = ffmpegMain
	m.customPath = ""
	m.customResult = ""
	m.customValid = false
	m.installing = false
	m.installResult = ""
	m.installError = false
	m.successDismiss = false
	m.warning = ""
	m.reviewScript = ""
	m.reviewToken = ""
	m.reviewFocus = 0
	m.manualPath = ""
	m.manualResult = ""
	m.manualValid = false
	m.checking = false
	m.textInput.Blur()
	m.textInput.SetValue("")
	m.values = make(map[string]string)
	m.formInit = m.buildMainForm()
}

// Close hides the overlay.
func (m *FFmpegCheckModel) Close() {
	m.visible = false
}

// IsVisible returns true if the overlay is shown.
func (m *FFmpegCheckModel) IsVisible() bool {
	return m.visible
}

// SetSize updates dimensions.
func (m *FFmpegCheckModel) SetSize(w, h int) {
	m.width = w
	m.height = h

	// Update text input width for modes that use it
	if m.mode == ffmpegCustom || m.mode == ffmpegManual {
		m.updateTextInputWidth()
	}

	if m.mode == ffmpegReview {
		_, contentW := dialogBox(60, w)
		m.reviewViewport.SetWidth(contentW - 4)
		m.reviewViewport.SetHeight(max(min(h-20, 6), 3))
	}
}

// updateTextInputWidth sets the text input width based on current dimensions.
func (m *FFmpegCheckModel) updateTextInputWidth() {
	if m.width == 0 {
		return
	}
	_, contentW := dialogBox(60, m.width)
	m.textInput.SetWidth(contentW - 2)
}

// UpdateComponents routes tea.Msg to the embedded textinput, viewport, or spinner and syncs.
func (m *FFmpegCheckModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}

	// Route spinner when installing or checking a path
	if m.installing || m.checking {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}

	// Route huh form in main/install modes.
	// Uses the same pattern as setup_wizard.go: Update returns (tea.Model, tea.Cmd),
	// type-assert back to *huh.Form.
	if m.mode == ffmpegMain || m.mode == ffmpegInstall {
		// Deliver pending Init cmd from form creation (cursor blink, focus setup)
		if m.formInit != nil {
			cmd := m.formInit
			m.formInit = nil
			return cmd
		}
		if m.form != nil {
			model, cmd := m.form.Update(msg)
			m.form = model.(*huh.Form)
			return cmd
		}
		return nil
	}

	// Review mode: only forward non-key messages and scroll keys to viewport
	if m.mode == ffmpegReview {
		if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
			// Only forward scroll-related keys to the viewport
			switch keyMsg.String() {
			case keyUp, keyDown, "pgup", "pgdown", "ctrl+u", "ctrl+d", keyHome, keyEnd:
				// Allow scroll keys through
			default:
				return nil
			}
		}
		var cmd tea.Cmd
		m.reviewViewport, cmd = m.reviewViewport.Update(msg)
		return cmd
	}

	// Other modes: route to textinput
	if !m.textInput.Focused() {
		return nil
	}
	prev := m.textInput.Value()
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prev {
		switch m.mode {
		case ffmpegCustom:
			m.customPath = m.textInput.Value()
			m.customResult = ""
		case ffmpegManual:
			m.manualPath = m.textInput.Value()
			m.manualResult = ""
		}
	}
	return cmd
}

// HandleKey processes key input. Returns an action string:
//
//	"quit"                 — user chose Quit
//	"dismiss"              — user dismissed the success message
//	"prepare:<method>"     — user chose an install method, needs elevation check (run async)
//	"check_custom:<path>"  — user pressed Enter on a custom path (run async)
//	"confirm:<token>"      — user approved the elevated install script
//	"reject:<token>"       — user declined the elevated install script
//	""                     — no action
func (m *FFmpegCheckModel) HandleKey(key string) string {
	if m.successDismiss {
		m.Close()
		return "dismiss" // Any key closes after success
	}
	if m.installing {
		return "" // Block input while installing
	}
	if m.checking {
		return "" // Block input while checking path
	}

	switch m.mode {
	case ffmpegMain:
		return m.handleMainKey(key)
	case ffmpegInstall:
		return m.handleInstallKey(key)
	case ffmpegCustom:
		return m.handleCustomKey(key)
	case ffmpegReview:
		return m.handleReviewKey(key)
	case ffmpegManual:
		return m.handleManualKey(key)
	}
	return ""
}

func (m *FFmpegCheckModel) handleMainKey(key string) string {
	if key == keyEsc {
		return "quit"
	}
	// Check if huh form completed (state set by UpdateComponents on same cycle)
	if m.form != nil && m.form.State == huh.StateCompleted {
		action := m.values["action"]
		m.form = nil
		switch action {
		case "install":
			m.mode = ffmpegInstall
			m.installResult = ""
			m.installError = false
			m.formInit = m.buildInstallForm()
		case "custom":
			m.mode = ffmpegCustom
			m.customPath = ""
			m.customResult = ""
			m.customValid = false
			m.textInput.SetValue("")
			m.textInput.Focus()
			m.updateTextInputWidth()
		case "skip":
			return "skip"
		case "quit":
			return "quit"
		}
	}
	return ""
}

// buildMainForm creates the huh Select form for the main menu.
// Returns a tea.Cmd from Init() that must be delivered to bubbletea.
func (m *FFmpegCheckModel) buildMainForm() tea.Cmd {
	m.values["action"] = ""
	f := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose an option:").
				Options(
					huh.NewOption("Install FFmpeg", "install"),
					huh.NewOption("Custom FFmpeg path", "custom"),
					huh.NewOption("Skip for now", "skip"),
					huh.NewOption("Quit Moombox", "quit"),
				).
				Accessor(&MapAccessor{M: m.values, Key: "action"}),
		),
	).WithTheme(moomboxTheme(m.isDark)).WithShowHelp(false)
	f.SubmitCmd = nil
	m.form = f
	return m.form.Init()
}

// buildInstallForm creates the huh Select form for install method selection.
// Returns a tea.Cmd from Init() that must be delivered to bubbletea.
func (m *FFmpegCheckModel) buildInstallForm() tea.Cmd {
	m.values["method"] = ""

	chocoAvail, wingetAvail := false, false
	if m.OnCheckPrereqs != nil {
		chocoAvail, wingetAvail = m.OnCheckPrereqs()
	}

	var opts []huh.Option[string]
	if chocoAvail {
		opts = append(opts, huh.NewOption("Install via Chocolatey", "choco"))
	} else {
		opts = append(opts, huh.NewOption("Install Chocolatey + FFmpeg", "choco-install"))
	}
	if wingetAvail {
		opts = append(opts, huh.NewOption("Install via Winget", "winget"))
	}
	opts = append(opts, huh.NewOption("Cancel", ""))

	f := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose install method:").
				Options(opts...).
				Accessor(&MapAccessor{M: m.values, Key: "method"}),
		),
	).WithTheme(moomboxTheme(m.isDark)).WithShowHelp(false)
	f.SubmitCmd = nil
	m.form = f
	return m.form.Init()
}

func (m *FFmpegCheckModel) handleInstallKey(key string) string {
	if key == keyEsc {
		m.mode = ffmpegMain
		m.form = nil
		m.formInit = m.buildMainForm()
		return ""
	}
	// Check if huh form completed (state set by UpdateComponents on same cycle)
	if m.form != nil && m.form.State == huh.StateCompleted {
		method := m.values["method"]
		m.form = nil
		if method == "" {
			// Cancel option
			m.mode = ffmpegMain
			m.formInit = m.buildMainForm()
			return ""
		}
		// Signal the install to run async — App handles via tea.Cmd
		m.installing = true
		m.spinner = newSpinner()
		m.installResult = "Checking permissions..."
		m.installError = false
		return "prepare:" + method
	}
	return ""
}

// SetInstallResult updates the install result display. Called by App after async install completes.
func (m *FFmpegCheckModel) SetInstallResult(result string, isError bool) {
	m.installResult = result
	m.installError = isError
	m.installing = false
	m.checking = false
}

// SetCustomResult updates the custom path check result. Called by App after async check completes.
func (m *FFmpegCheckModel) SetCustomResult(result string, valid bool) {
	m.customResult = result
	m.customValid = valid
	m.checking = false
}

func (m *FFmpegCheckModel) handleCustomKey(key string) string {
	switch key {
	case keyEsc:
		m.mode = ffmpegMain
		m.textInput.Blur()
	case keyEnter:
		if m.customPath == "" || m.checking {
			return ""
		}
		m.checking = true
		m.spinner = newSpinner()
		return "check_custom:" + m.customPath
	}
	return ""
}

// ShowReview switches to script review mode for elevated installs.
func (m *FFmpegCheckModel) ShowReview(script, token string) {
	m.mode = ffmpegReview
	m.reviewScript = script
	m.reviewToken = token
	m.reviewFocus = 0
	m.installing = false
	m.installResult = ""
	m.textInput.Blur()

	// Initialize review viewport — guard against zero width before first SetSize.
	vpW := 0
	if m.width > 0 {
		_, contentW := dialogBox(60, m.width)
		vpW = contentW - 4
	}
	vpW = max(vpW, 0)
	m.reviewViewport = viewport.New(viewport.WithWidth(vpW), viewport.WithHeight(6))
	m.reviewViewport.SetContent(script)
}

// SetManualResult updates the manual path check result.
func (m *FFmpegCheckModel) SetManualResult(result string, valid bool) {
	m.manualResult = result
	m.manualValid = valid
	m.checking = false
}

// ShowManual switches to manual install mode with a fresh text input.
func (m *FFmpegCheckModel) ShowManual() {
	m.mode = ffmpegManual
	m.manualPath = ""
	m.manualResult = ""
	m.textInput.SetValue("")
	m.textInput.Focus()
	m.updateTextInputWidth()
}

// ShowInstallOptions resets state and switches back to the install options view.
func (m *FFmpegCheckModel) ShowInstallOptions() {
	m.mode = ffmpegInstall
	m.installResult = ""
	m.installError = false
	m.installing = false
	m.textInput.Blur()
	m.form = nil
	m.formInit = m.buildInstallForm()
}

func (m *FFmpegCheckModel) handleReviewKey(key string) string {
	if m.installing {
		return "" // Ignore input while installing
	}
	// After a failed install the token is consumed — only allow Esc to go back.
	if m.installError {
		if key == keyEsc {
			return "cancel:" + m.reviewToken
		}
		return ""
	}
	switch key {
	case keyLeft:
		m.reviewFocus = 0
	case keyRight:
		m.reviewFocus = 1
	case keyEnter:
		if m.reviewFocus == 0 {
			// Trust & Continue
			m.installing = true
			m.spinner = newSpinner()
			m.installResult = "Installing with administrator privileges..."
			return "confirm:" + m.reviewToken
		}
		// Distrust — switch to manual install
		return "reject:" + m.reviewToken
	case keyEsc:
		// Cancel — reject the token but go back to install options (not manual).
		// Distrust (Enter on focus 1) goes to manual install instead.
		return "cancel:" + m.reviewToken
	}
	return ""
}

func (m *FFmpegCheckModel) handleManualKey(key string) string {
	switch key {
	case keyEsc:
		m.mode = ffmpegMain
		m.manualPath = ""
		m.manualResult = ""
		m.textInput.Blur()
	case keyEnter:
		if m.manualPath == "" || m.checking {
			return ""
		}
		m.checking = true
		m.spinner = newSpinner()
		return "check_custom:" + m.manualPath
	}
	return ""
}

// View renders the FFmpeg check overlay.
func (m *FFmpegCheckModel) View() string {
	if !m.visible {
		return ""
	}
	if m.width == 0 || m.height == 0 {
		return ""
	}

	boxW, contentW := dialogBox(60, m.width)

	var lines []string

	// Warning header
	warnIcon := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true).Render("\u26a0")
	warnTitle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true).Render(" FFmpeg Not Found")
	lines = append(lines, warnIcon+warnTitle)
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("FFmpeg is required for muxing video and audio."))
	lines = append(lines, DimStyle.Render("Install it or provide a custom path below."))
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))

	switch m.mode {
	case ffmpegMain:
		if m.form != nil {
			lines = append(lines, m.form.View())
		}
		// Warning when skip is selected
		if m.values["action"] == "skip" {
			lines = append(lines, "")
			lines = append(lines, lipgloss.NewStyle().Foreground(ColorWarning).Render("\u26a0 Muxing will fail until FFmpeg is installed."))
			lines = append(lines, DimStyle.Render("You can install it later from Settings \u2192 Paths."))
		}

	case ffmpegInstall:
		if m.form != nil {
			lines = append(lines, m.form.View())
		}

		if m.installResult != "" {
			lines = append(lines, "")
			resultColor := ColorGreen
			if m.installError {
				resultColor = ColorError
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(resultColor).Render(m.installResult))
		}

		if m.installing {
			lines = append(lines, "")
			lines = append(lines, m.spinner.View()+" Installing... please wait")
		}

	case ffmpegCustom:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Enter FFmpeg path:"))
		lines = append(lines, "")
		lines = append(lines, m.textInput.View())

		if m.checking {
			lines = append(lines, "")
			lines = append(lines, m.spinner.View()+" Checking path...")
		} else if m.customResult != "" {
			lines = append(lines, "")
			resultColor := ColorGreen
			if !m.customValid {
				resultColor = ColorError
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(resultColor).Render(m.customResult))
		}

	case ffmpegReview:
		lockIcon := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true).Render("\u26a0")
		lines = append(lines, lockIcon+" "+lipgloss.NewStyle().Foreground(ColorWarning).Bold(true).Render("Administrator privileges required"))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("The following script will run elevated."))
		lines = append(lines, DimStyle.Render("Review it before proceeding:"))
		lines = append(lines, "")

		// Scrollable script display via viewport
		m.reviewViewport.SetWidth(contentW - 4)
		scriptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
		for sl := range strings.SplitSeq(m.reviewViewport.View(), "\n") {
			lines = append(lines, scriptStyle.Render("  "+sl))
		}
		if m.reviewViewport.ScrollPercent() < 1.0 {
			lines = append(lines, DimStyle.Render("  ... (scroll with \u2191/\u2193)"))
		}

		lines = append(lines, "")
		if m.installing {
			lines = append(lines, m.spinner.View()+" Installing... please wait")
		} else if m.installResult != "" {
			resultColor := ColorGreen
			if m.installError {
				resultColor = ColorError
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(resultColor).Render(m.installResult))
			if m.installError {
				lines = append(lines, DimStyle.Render("Press Esc to go back"))
			}
		} else {
			// Two options: Trust / Distrust
			trustPrefix, distrustPrefix := "  ", "  "
			trustColor, distrustColor := ColorWhite, ColorWhite
			if m.reviewFocus == 0 {
				trustPrefix = "> "
				trustColor = ColorGreen
			} else {
				distrustPrefix = "> "
				distrustColor = ColorCyan
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(trustColor).Bold(m.reviewFocus == 0).Render(trustPrefix+"Trust & Continue"))
			lines = append(lines, lipgloss.NewStyle().Foreground(distrustColor).Bold(m.reviewFocus == 1).Render(distrustPrefix+"Distrust & Manually Install"))
		}

	case ffmpegManual:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Manual FFmpeg Install"))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("Download from:"))
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Render("  https://www.gyan.dev/ffmpeg/builds/"))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("Recommended: ffmpeg-release-full-shared.7z"))
		lines = append(lines, DimStyle.Render("(under \"release builds\")"))
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Enter FFmpeg path:"))
		lines = append(lines, m.textInput.View())

		if m.checking {
			lines = append(lines, "")
			lines = append(lines, m.spinner.View()+" Checking path...")
		} else if m.manualResult != "" {
			lines = append(lines, "")
			resultColor := ColorGreen
			if !m.manualValid {
				resultColor = ColorError
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(resultColor).Render(m.manualResult))
		}
	}

	// Version warning (e.g. outdated FFmpeg)
	if m.warning != "" {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWarning).Render(m.warning))
	}

	// Navigation hints
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
	if m.successDismiss {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorGreen).Render("Press any key to continue"))
	} else {
		switch m.mode {
		case ffmpegMain:
			lines = append(lines, DimStyle.Render("Enter: Choose  Esc: Quit"))
		case ffmpegInstall:
			lines = append(lines, DimStyle.Render("Enter: Choose  Esc: Back"))
		case ffmpegCustom:
			lines = append(lines, DimStyle.Render("Esc: Back  Enter: Check path"))
		case ffmpegReview:
			if !m.installing {
				lines = append(lines, DimStyle.Render("\u2190/\u2192: Select  \u2191/\u2193: Scroll  Enter: Choose  Esc: Cancel"))
			}
		case ffmpegManual:
			lines = append(lines, DimStyle.Render("Esc: Back  Enter: Check path"))
		}
	}

	content := strings.Join(lines, "\n")

	h := max(m.height-2, 10)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorWarning).
		Width(boxW).
		Height(h + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
}

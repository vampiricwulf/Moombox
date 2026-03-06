package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

	mode         ffmpegMode
	mainFocus    int // 0=Install, 1=Custom path, 2=Quit
	installFocus int

	// Shared text input component
	textInput textinput.Model

	// Custom path
	customPath   string
	customResult string // result message after check
	customValid  bool

	// Install state
	installOptions []installOption
	installing     bool
	installResult  string
	installError   bool

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

	// Loading spinner
	spinner spinner.Model

	// Callbacks — only OnCheckPrereqs runs synchronously (fast LookPath),
	// the others are dispatched as tea.Cmd by App.
	OnCheckPrereqs func() (bool, bool)
}

type installOption struct {
	label  string
	method string
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
	m.mainFocus = 0
	m.installFocus = 0
	m.customPath = ""
	m.customResult = ""
	m.customValid = false
	m.installing = false
	m.installResult = ""
	m.installError = false
	m.installOptions = nil
	m.successDismiss = false
	m.warning = ""
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

	if m.mode == ffmpegReview {
		boxW := min(60, w)
		if boxW < 40 {
			boxW = 40
		}
		contentW := boxW - 4
		m.reviewViewport.Width = contentW - 4
	}
}

// UpdateComponents routes tea.Msg to the embedded textinput, viewport, or spinner and syncs.
func (m *FFmpegCheckModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}

	// Route spinner when installing
	if m.installing {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}

	// Review mode: route to viewport (no textinput active)
	if m.mode == ffmpegReview {
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
	switch key {
	case keyUp:
		if m.mainFocus > 0 {
			m.mainFocus--
		}
	case keyDown:
		if m.mainFocus < 2 {
			m.mainFocus++
		}
	case keyEnter:
		switch m.mainFocus {
		case 0: // Install FFmpeg
			m.mode = ffmpegInstall
			m.installFocus = 0
			m.installResult = ""
			m.installError = false
			m.buildInstallOptions()
		case 1: // Custom path
			m.mode = ffmpegCustom
			m.customPath = ""
			m.customResult = ""
			m.customValid = false
			m.textInput.SetValue("")
			m.textInput.Focus()
		case 2: // Quit
			return "quit"
		}
	}
	return ""
}

func (m *FFmpegCheckModel) buildInstallOptions() {
	m.installOptions = nil

	chocoAvail, wingetAvail := false, false
	if m.OnCheckPrereqs != nil {
		chocoAvail, wingetAvail = m.OnCheckPrereqs()
	}

	if chocoAvail {
		m.installOptions = append(m.installOptions, installOption{
			label:  "Install via Chocolatey",
			method: "choco",
		})
	} else {
		m.installOptions = append(m.installOptions, installOption{
			label:  "Install Chocolatey + FFmpeg",
			method: "choco-install",
		})
	}

	if wingetAvail {
		m.installOptions = append(m.installOptions, installOption{
			label:  "Install via Winget",
			method: "winget",
		})
	}

	m.installOptions = append(m.installOptions, installOption{
		label:  "Cancel",
		method: "",
	})
}

func (m *FFmpegCheckModel) handleInstallKey(key string) string {
	switch key {
	case keyEsc:
		m.mode = ffmpegMain
	case keyUp:
		if m.installFocus > 0 {
			m.installFocus--
		}
	case keyDown:
		if m.installFocus < len(m.installOptions)-1 {
			m.installFocus++
		}
	case keyEnter:
		if m.installFocus >= len(m.installOptions) {
			return ""
		}
		opt := m.installOptions[m.installFocus]
		if opt.method == "" {
			// Cancel
			m.mode = ffmpegMain
			return ""
		}

		// Signal the install to run async — App handles via tea.Cmd
		m.installing = true
		m.spinner = newSpinner()
		m.installResult = "Checking permissions..."
		m.installError = false
		return "prepare:" + opt.method
	}
	return ""
}

// SetInstallResult updates the install result display. Called by App after async install completes.
func (m *FFmpegCheckModel) SetInstallResult(result string, isError bool) {
	m.installResult = result
	m.installError = isError
	m.installing = false
}

// SetCustomResult updates the custom path check result. Called by App after async check completes.
func (m *FFmpegCheckModel) SetCustomResult(result string, valid bool) {
	m.customResult = result
	m.customValid = valid
}

func (m *FFmpegCheckModel) handleCustomKey(key string) string {
	switch key {
	case keyEsc:
		m.mode = ffmpegMain
		m.textInput.Blur()
	case keyEnter:
		if m.customPath == "" {
			return ""
		}
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

	// Initialize review viewport
	boxW := min(60, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4
	m.reviewViewport = viewport.New(contentW-4, 6)
	m.reviewViewport.SetContent(script)
}

// SetManualResult updates the manual path check result.
func (m *FFmpegCheckModel) SetManualResult(result string, valid bool) {
	m.manualResult = result
	m.manualValid = valid
}

// ShowManual switches to manual install mode with a fresh text input.
func (m *FFmpegCheckModel) ShowManual() {
	m.mode = ffmpegManual
	m.manualPath = ""
	m.manualResult = ""
	m.textInput.SetValue("")
	m.textInput.Focus()
}

func (m *FFmpegCheckModel) handleReviewKey(key string) string {
	if m.installing {
		return "" // Ignore input while installing
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
		return "reject:" + m.reviewToken
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
		if m.manualPath == "" {
			return ""
		}
		return "check_custom:" + m.manualPath
	}
	return ""
}

// View renders the FFmpeg check overlay.
func (m *FFmpegCheckModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := min(60, m.width)
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4

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
		options := []string{"Install FFmpeg", "Custom FFmpeg path", "Quit"}
		for i, opt := range options {
			prefix := "  "
			color := ColorWhite
			if i == m.mainFocus {
				prefix = "> "
				color = ColorCyan
			}
			style := lipgloss.NewStyle().Foreground(color)
			if i == m.mainFocus {
				style = style.Bold(true)
			}
			lines = append(lines, style.Render(prefix+opt))
			if i < len(options)-1 {
				lines = append(lines, "")
			}
		}

	case ffmpegInstall:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Choose install method:"))
		lines = append(lines, "")

		for i, opt := range m.installOptions {
			prefix := "  "
			color := ColorWhite
			if i == m.installFocus {
				prefix = "> "
				color = ColorCyan
			}
			style := lipgloss.NewStyle().Foreground(color)
			if i == m.installFocus {
				style = style.Bold(true)
			}
			lines = append(lines, style.Render(prefix+opt.label))
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
		m.textInput.Width = contentW - 2
		lines = append(lines, m.textInput.View())

		if m.customResult != "" {
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
		m.reviewViewport.Width = contentW - 4
		scriptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
		for _, sl := range strings.Split(m.reviewViewport.View(), "\n") {
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
		m.textInput.Width = contentW - 2
		lines = append(lines, m.textInput.View())

		if m.manualResult != "" {
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
			lines = append(lines, DimStyle.Render("\u2191/\u2193: Select  Enter: Choose"))
		case ffmpegInstall:
			lines = append(lines, DimStyle.Render("Esc: Back  \u2191/\u2193: Select  Enter: Install"))
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

	h := m.height - 2
	if h < 10 {
		h = 10
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorWarning).
		Width(contentW).
		Height(h).
		Render(content)

	return centerBox(box, m.width, m.height)
}

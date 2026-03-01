package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ffmpegMode identifies which view the FFmpeg check overlay is in.
type ffmpegMode int

const (
	ffmpegMain    ffmpegMode = iota // Main menu: Install / Custom path / Quit
	ffmpegInstall                   // Install sub-menu: Choco / Choco+Install / Winget / Cancel
	ffmpegCustom                    // Custom path text entry
)

// FFmpegCheckModel manages the FFmpeg validation overlay.
type FFmpegCheckModel struct {
	visible bool
	width   int
	height  int

	mode         ffmpegMode
	mainFocus    int // 0=Install, 1=Custom path, 2=Quit
	installFocus int

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
	return &FFmpegCheckModel{}
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
}

// HandleKey processes key input. Returns an action string:
//
//	"quit"                — user chose Quit
//	"install:<method>"    — user chose an install method (run async)
//	"check_custom:<path>" — user pressed Enter on a custom path (run async)
//	""                    — no action
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
		m.installResult = "Installing FFmpeg..."
		m.installError = false
		return "install:" + opt.method
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
	case keyEnter:
		// Signal the check to run async — App handles via tea.Cmd
		if m.customPath == "" {
			return ""
		}
		return "check_custom:" + m.customPath
	case "backspace", "delete":
		if len(m.customPath) > 0 {
			runes := []rune(m.customPath)
			m.customPath = string(runes[:len(runes)-1])
		}
		m.customResult = ""
	case "ctrl+v":
		if clip := readClipboard(); clip != "" {
			if idx := strings.IndexAny(clip, "\r\n"); idx >= 0 {
				clip = clip[:idx]
			}
			clip = strings.TrimSpace(clip)
			if clip != "" {
				m.customPath += clip
			}
		}
		m.customResult = ""
	default:
		if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			m.customPath += key
			m.customResult = ""
		}
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
			lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Render("Installing... please wait"))
		}

	case ffmpegCustom:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Enter FFmpeg path:"))
		lines = append(lines, "")
		cursor := lipgloss.NewStyle().Foreground(ColorCyan).Render("_")
		lines = append(lines, DimStyle.Render("[")+m.customPath+cursor+DimStyle.Render("]"))

		if m.customResult != "" {
			lines = append(lines, "")
			resultColor := ColorGreen
			if !m.customValid {
				resultColor = ColorError
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(resultColor).Render(m.customResult))
		}
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

package tui

import (
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// ImportDialogModel wraps bubbles/filepicker for browsing and selecting .zip
// archive files, then shows a metadata step for optional title/channel before
// importing.
type ImportDialogModel struct {
	visible bool
	width   int
	height  int
	step    int // 0=filepicker, 1=metadata, 2=importing

	// File picker
	picker filepicker.Model

	// Selected file info
	filePath string

	// Metadata (optional)
	title     string
	channel   string
	metaFocus int // 0=title, 1=channel

	// Shared text input
	textInput textinput.Model

	// Error state
	errorMsg string

	// Loading spinner
	spinner spinner.Model
}

// NewImportDialogModel creates a new model with initialized filepicker and
// textinput.
func NewImportDialogModel() *ImportDialogModel {
	fp := filepicker.New()
	fp.AllowedTypes = []string{".zip"}
	fp.ShowSize = true
	fp.ShowPermissions = false
	fp.AutoHeight = false

	return &ImportDialogModel{
		picker:    fp,
		textInput: newTextInput(),
		spinner:   newSpinner(),
	}
}

// Open opens the dialog at the given starting directory.
func (m *ImportDialogModel) Open(startDir string) tea.Cmd {
	m.visible = true
	m.step = 0
	m.filePath = ""
	m.title = ""
	m.channel = ""
	m.metaFocus = 0
	m.errorMsg = ""
	m.picker.CurrentDirectory = startDir
	m.picker.SetHeight(max(1, m.height-8))
	return m.picker.Init()
}

// Close hides the dialog.
func (m *ImportDialogModel) Close() {
	m.visible = false
}

// IsVisible returns true if the dialog is shown.
func (m *ImportDialogModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the dialog dimensions.
func (m *ImportDialogModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.picker.SetHeight(max(1, h-8))
}

// SetError sets the error and goes back to metadata step.
func (m *ImportDialogModel) SetError(err string) {
	m.errorMsg = err
	m.step = 1
}

// UpdateComponents routes tea.Msg to the active component.
func (m *ImportDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	switch m.step {
	case 0:
		// Filepicker
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		// Check if a file was selected
		if didSelect, path := m.picker.DidSelectFile(msg); didSelect {
			m.filePath = path
			m.step = 1
			m.metaFocus = 0
			configureTextInput(&m.textInput, "", nil, textinput.EchoNormal)
		}
		return cmd
	case 1:
		// Metadata text input
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
	case 2:
		// Importing — route spinner
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
	return nil
}

// syncFromTextInput saves the text input value to the appropriate field.
func (m *ImportDialogModel) syncFromTextInput() {
	if m.metaFocus == 0 {
		m.title = m.textInput.Value()
	} else {
		m.channel = m.textInput.Value()
	}
}

// syncToTextInput loads the appropriate field value into the text input.
func (m *ImportDialogModel) syncToTextInput() {
	if m.metaFocus == 0 {
		configureTextInput(&m.textInput, m.title, nil, textinput.EchoNormal)
	} else {
		configureTextInput(&m.textInput, m.channel, nil, textinput.EchoNormal)
	}
}

// HandleKey processes key input. Returns (action, data).
// action="import" with data=filePath when the user confirms import.
func (m *ImportDialogModel) HandleKey(key string) (string, string) {
	switch m.step {
	case 0:
		// File picker handles most keys via UpdateComponents
		if key == keyEsc {
			m.Close()
		}
		return "", ""
	case 1:
		// Metadata step
		switch key {
		case keyEsc:
			m.step = 0 // Back to file picker
			m.textInput.Blur()
			return "", ""
		case keyTab:
			m.syncFromTextInput()
			m.metaFocus = 1 - m.metaFocus // Toggle between title/channel
			m.syncToTextInput()
			return "", ""
		case keyEnter:
			m.syncFromTextInput()
			m.step = 2
			m.spinner = newSpinner()
			m.textInput.Blur()
			return "import", m.filePath
		}
	}
	return "", ""
}

// SpinnerInit returns the spinner's initial tick command when importing.
func (m *ImportDialogModel) SpinnerInit() tea.Cmd { return spinnerTickCmd(m.spinner) }

// GetImportTitle returns the optional title metadata.
func (m *ImportDialogModel) GetImportTitle() string {
	return m.title
}

// GetImportChannel returns the optional channel metadata.
func (m *ImportDialogModel) GetImportChannel() string {
	return m.channel
}

// View renders the dialog as a centered overlay.
func (m *ImportDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := min(max(min(70, m.width-4), 40), m.width)
	boxH := max(min(m.height-4, 30), 10)
	contentW := boxW - 2

	var content strings.Builder

	switch m.step {
	case 0:
		content.WriteString(TitleStyle.Render("Import Archive") + "\n")
		content.WriteString(DimStyle.Render("Select a .zip archive to import") + "\n\n")
		content.WriteString(m.picker.View() + "\n")
		content.WriteString(DimStyle.Render("↑↓: Navigate  ←/Backspace: Back  →/Enter: Open/Select  Esc: Cancel"))

	case 1:
		content.WriteString(TitleStyle.Render("Import Archive — Metadata") + "\n")
		content.WriteString(DimStyle.Render("Optional: override title and channel") + "\n\n")

		// Show selected file
		content.WriteString(lipgloss.NewStyle().Foreground(ColorGreen).Render("File: ") +
			truncateString(filepath.Base(m.filePath), contentW-6) + "\n\n")

		// Title field
		titleLabel := "Title:   "
		if m.metaFocus == 0 {
			content.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render("> " + titleLabel))
			content.WriteString(m.textInput.View())
		} else {
			content.WriteString("  " + DimStyle.Render(titleLabel))
			content.WriteString(renderInactiveInput(m.title, contentW-runewidth.StringWidth(titleLabel)-2, ColorGray))
		}
		content.WriteString("\n")

		// Channel field
		channelLabel := "Channel: "
		if m.metaFocus == 1 {
			content.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render("> " + channelLabel))
			content.WriteString(m.textInput.View())
		} else {
			content.WriteString("  " + DimStyle.Render(channelLabel))
			content.WriteString(renderInactiveInput(m.channel, contentW-runewidth.StringWidth(channelLabel)-2, ColorGray))
		}
		content.WriteString("\n")

		if m.errorMsg != "" {
			content.WriteString("\n" + ErrorStyle.Render(m.errorMsg))
		}
		content.WriteString("\n" + DimStyle.Render("Tab: Switch field  Enter: Import  Esc: Back"))

	case 2:
		content.WriteString(TitleStyle.Render("Importing...") + "\n\n")
		content.WriteString(m.spinner.View() + " Please wait...")
	}

	borderColor := ColorGreen
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(boxW).
		Height(boxH + 2).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}

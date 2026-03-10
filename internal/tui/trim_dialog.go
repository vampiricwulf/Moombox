package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TrimMode represents create vs delete mode.
type TrimMode int

const (
	TrimModeCreate TrimMode = iota
	TrimModeDelete
)

// TrimInfo represents an existing trim for delete mode.
type TrimInfo struct {
	ID        string
	StartTime float64
	EndTime   float64
	Duration  float64
	FileSize  int64
	Filename  string
}

// TrimDialogModel manages the trim dialog.
type TrimDialogModel struct {
	visible bool
	width   int
	height  int

	jobID    string
	jobTitle string
	mode     TrimMode

	// Create mode state
	createStep     int // 0=time input, 1=confirmation, 2=progress
	startTimeInput string
	endTimeInput   string
	activeField    int // 0=start, 1=end
	textInput      textinput.Model
	errorMsg       string
	loading        bool

	// Progress state (step 2)
	progressBar     progress.Model
	progressPercent float64
	progressElapsed time.Duration

	// Job metadata for display
	lengthSeconds float64
	fileSize      int64

	// Delete mode state
	trims            []TrimInfo
	selectedTrimIdx  int
	deleteConfirmID  string

	// Parsed values (after validation)
	parsedStart float64
	parsedEnd   float64

	// Loading spinner
	spinner spinner.Model
}

// NewTrimDialogModel creates a new trim dialog.
func NewTrimDialogModel() *TrimDialogModel {
	pb := progress.New(progress.WithoutPercentage())
	pb.Full = '\u2588'  // █
	pb.Empty = '\u2591' // ░
	pb.EmptyColor = string(ColorGray)
	return &TrimDialogModel{
		textInput:   newTextInput(),
		spinner:     newSpinner(),
		progressBar: pb,
	}
}

// Open shows the dialog for a specific job.
func (m *TrimDialogModel) Open(jobID, jobTitle string) {
	m.visible = true
	m.jobID = jobID
	m.jobTitle = jobTitle
	m.mode = TrimModeCreate
	m.createStep = 0
	m.startTimeInput = ""
	m.endTimeInput = ""
	m.activeField = 0
	m.textInput.Validate = validateTimeChars
	m.textInput.SetValue("")
	m.textInput.Focus()
	m.errorMsg = ""
	m.loading = false
	m.selectedTrimIdx = 0
	m.deleteConfirmID = ""
	m.parsedStart = 0
	m.parsedEnd = 0
}

// Close hides the dialog.
func (m *TrimDialogModel) Close() {
	m.visible = false
}

// IsVisible returns true if the dialog is shown.
func (m *TrimDialogModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the dialog dimensions.
func (m *TrimDialogModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// SetJobMetadata provides job metadata for display.
func (m *TrimDialogModel) SetJobMetadata(lengthSeconds float64, fileSize int64) {
	m.lengthSeconds = lengthSeconds
	m.fileSize = fileSize
}

// SetTrims provides existing trims for delete mode.
func (m *TrimDialogModel) SetTrims(trims []TrimInfo) {
	m.trims = trims
}

// SetLoading sets the loading state.
func (m *TrimDialogModel) SetLoading(loading bool) {
	if loading {
		m.spinner = newSpinner()
	}
	m.loading = loading
}

// StartProgress transitions to the progress step (createStep 2).
func (m *TrimDialogModel) StartProgress() {
	m.createStep = 2
	m.loading = true
	m.spinner = newSpinner()
	m.progressPercent = 0
	m.progressElapsed = 0
	m.errorMsg = ""
}

// SetProgress updates the progress display.
func (m *TrimDialogModel) SetProgress(percent float64, elapsed time.Duration) {
	m.progressPercent = percent
	m.progressElapsed = elapsed
}

// SetError sets an error message on the dialog.
func (m *TrimDialogModel) SetError(msg string) {
	m.errorMsg = msg
}

// RemoveTrim removes a trim by ID from the dialog's list after successful deletion.
func (m *TrimDialogModel) RemoveTrim(trimID string) {
	for i, t := range m.trims {
		if t.ID == trimID {
			m.trims = append(m.trims[:i], m.trims[i+1:]...)
			if m.selectedTrimIdx >= len(m.trims) && m.selectedTrimIdx > 0 {
				m.selectedTrimIdx--
			}
			m.deleteConfirmID = ""
			break
		}
	}
}

// StartTime returns the start time input.
func (m *TrimDialogModel) StartTime() string { return m.startTimeInput }

// EndTime returns the end time input.
func (m *TrimDialogModel) EndTime() string { return m.endTimeInput }

// ParsedStartSeconds returns the validated start time in seconds.
func (m *TrimDialogModel) ParsedStartSeconds() float64 { return m.parsedStart }

// ParsedEndSeconds returns the validated end time in seconds.
func (m *TrimDialogModel) ParsedEndSeconds() float64 { return m.parsedEnd }

// JobID returns the job ID being trimmed.
func (m *TrimDialogModel) JobID() string { return m.jobID }

// SelectedTrimID returns the trim ID selected for deletion.
func (m *TrimDialogModel) SelectedTrimID() string {
	if m.selectedTrimIdx >= 0 && m.selectedTrimIdx < len(m.trims) {
		return m.trims[m.selectedTrimIdx].ID
	}
	return ""
}

// HandleKey processes key input. Returns action:
// "submit" for create, "delete" for delete confirm, "background" to dismiss progress overlay, "" for no action.
func (m *TrimDialogModel) HandleKey(key string) string {
	if key == keyEsc {
		return m.handleEscape()
	}

	// Block mode toggle during progress
	if key == "m" || key == "M" {
		if m.mode == TrimModeCreate && m.createStep == 2 {
			return ""
		}
		m.toggleMode()
		return ""
	}

	if m.mode == TrimModeCreate {
		return m.handleCreateKey(key)
	}
	return m.handleDeleteKey(key)
}

func (m *TrimDialogModel) handleEscape() string {
	if m.mode == TrimModeCreate && m.createStep == 2 {
		// During progress: dismiss overlay, continue in background
		m.Close()
		return "background"
	}
	if m.mode == TrimModeCreate && m.createStep > 0 {
		m.createStep--
		m.errorMsg = ""
		if m.createStep == 0 {
			m.textInput.Focus()
		}
		return ""
	}
	// In DELETE mode, Esc returns to CREATE mode (match TS behavior)
	if m.mode == TrimModeDelete {
		if m.deleteConfirmID != "" {
			m.deleteConfirmID = "" // First clear pending confirmation
			return ""
		}
		m.mode = TrimModeCreate
		m.createStep = 0
		m.errorMsg = ""
		if m.activeField == 0 {
			m.textInput.SetValue(m.startTimeInput)
		} else {
			m.textInput.SetValue(m.endTimeInput)
		}
		m.textInput.Focus()
		return ""
	}
	m.Close()
	return ""
}

func (m *TrimDialogModel) toggleMode() {
	if m.mode == TrimModeCreate {
		m.mode = TrimModeDelete
		m.selectedTrimIdx = 0
		m.deleteConfirmID = ""
	} else {
		m.mode = TrimModeCreate
		m.createStep = 0
		m.startTimeInput = ""
		m.endTimeInput = ""
		m.parsedStart = 0
		m.parsedEnd = 0
		m.activeField = 0
		m.textInput.SetValue("")
		m.textInput.Validate = validateTimeChars
		m.textInput.Focus()
	}
	m.errorMsg = ""
}

func (m *TrimDialogModel) handleCreateKey(key string) string {
	switch m.createStep {
	case 0: // Time input
		switch key {
		case keyTab:
			m.activeField = 1 - m.activeField
			if m.activeField == 0 {
				m.textInput.SetValue(m.startTimeInput)
			} else {
				m.textInput.SetValue(m.endTimeInput)
			}
		case keyEnter:
			return m.validateAndAdvance()
		}

	case 1: // Confirmation
		if key == keyEnter {
			return "submit"
		}

	case 2: // Progress — only Esc works (handled in HandleKey before this)
		return ""
	}
	return ""
}

func (m *TrimDialogModel) validateAndAdvance() string {
	// Parse start time (R2)
	if m.startTimeInput == "" {
		m.errorMsg = "Start time is required"
		return ""
	}
	start, err := parseTimeToSeconds(m.startTimeInput)
	if err != nil {
		m.errorMsg = "Invalid start time format (use HH:MM:SS, MM:SS, or seconds)"
		return ""
	}
	if start < 0 {
		m.errorMsg = "Start time cannot be negative"
		return ""
	}

	// Parse end time
	if m.endTimeInput == "" {
		m.errorMsg = "End time is required"
		return ""
	}
	end, err := parseTimeToSeconds(m.endTimeInput)
	if err != nil {
		m.errorMsg = "Invalid end time format (use HH:MM:SS, MM:SS, or seconds)"
		return ""
	}

	// Range validation
	if end <= start {
		m.errorMsg = "End time must be after start time"
		return ""
	}
	if m.lengthSeconds > 0 && end > m.lengthSeconds {
		m.errorMsg = fmt.Sprintf("End exceeds duration (%s)", formatTimeSeconds(m.lengthSeconds))
		return ""
	}
	if end-start < 1 {
		m.errorMsg = "Trim must be at least 1 second"
		return ""
	}

	m.parsedStart = start
	m.parsedEnd = end
	m.createStep = 1
	m.errorMsg = ""
	return ""
}

func (m *TrimDialogModel) handleDeleteKey(key string) string {
	if len(m.trims) == 0 {
		return ""
	}

	switch key {
	case keyUp:
		if m.selectedTrimIdx > 0 {
			m.selectedTrimIdx--
		} else {
			m.selectedTrimIdx = len(m.trims) - 1 // wrap around (match TS)
		}
		m.deleteConfirmID = ""
		m.errorMsg = "" // clear error on navigate (match TS)
	case keyDown:
		if m.selectedTrimIdx < len(m.trims)-1 {
			m.selectedTrimIdx++
		} else {
			m.selectedTrimIdx = 0 // wrap around (match TS)
		}
		m.deleteConfirmID = ""
		m.errorMsg = "" // clear error on navigate (match TS)
	case keyEnter:
		if len(m.trims) == 0 {
			return ""
		}
		if m.selectedTrimIdx >= len(m.trims) {
			m.selectedTrimIdx = len(m.trims) - 1
		}
		trim := m.trims[m.selectedTrimIdx]
		if m.deleteConfirmID == trim.ID {
			// Second press: execute delete
			return "delete"
		}
		// First press: confirm
		m.deleteConfirmID = trim.ID
	}
	return ""
}

// UpdateComponents routes tea.Msg to the embedded textinput/spinner and syncs.
func (m *TrimDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	// Route spinner when loading
	if m.loading {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
	if m.mode != TrimModeCreate || m.createStep != 0 {
		return nil
	}
	if !m.textInput.Focused() {
		return nil
	}
	prev := m.textInput.Value()
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prev {
		if m.activeField == 0 {
			m.startTimeInput = m.textInput.Value()
		} else {
			m.endTimeInput = m.textInput.Value()
		}
	}
	return cmd
}

// View renders the trim dialog.
func (m *TrimDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := min(60, m.width-4)
	boxH := min(18, m.height-4)
	if boxW < 30 {
		boxW = 30
	}
	if boxH < 8 {
		boxH = 8
	}

	contentW := boxW - 2

	borderColor := ColorCyan
	if m.mode == TrimModeDelete {
		borderColor = ColorCookies // magenta for delete mode
	}

	var content string
	if m.mode == TrimModeCreate {
		content = m.renderCreateMode(contentW, boxH)
	} else {
		content = m.renderDeleteMode(contentW, boxH)
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(contentW).
		Height(boxH).
		Render(content)

	return centerBox(box, m.width, m.height)
}

func (m *TrimDialogModel) renderCreateMode(w, h int) string {
	var lines []string

	switch m.createStep {
	case 0: // Time input (R6)
		stepStr := "Step 1/3"
		lines = append(lines, TitleStyle.Render("Create Trim")+" "+DimStyle.Render("("+stepStr+")"))
		lines = append(lines, "")

		if m.jobTitle != "" {
			lines = append(lines, DimStyle.Render(truncateString(m.jobTitle, w)))
		}
		if m.lengthSeconds > 0 {
			lines = append(lines, DimStyle.Render(fmt.Sprintf("Duration: %s", formatDuration(time.Duration(m.lengthSeconds*float64(time.Second))))))
		}
		lines = append(lines, "")

		lines = append(lines, renderTimeInputPair(m.startTimeInput, m.endTimeInput, m.activeField, m.textInput, w, ColorCyan)...)
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("Format: HH:MM:SS, MM:SS, or seconds"))

	case 1: // Confirmation (R3)
		stepStr := "Step 2/3"
		lines = append(lines, TitleStyle.Render("Confirm Trim")+" "+DimStyle.Render("("+stepStr+")"))
		lines = append(lines, "")

		duration := m.parsedEnd - m.parsedStart
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorGreen).Render(
			fmt.Sprintf("  Time Range: %s - %s", formatTimeSeconds(m.parsedStart), formatTimeSeconds(m.parsedEnd)),
		))
		lines = append(lines, fmt.Sprintf("  Duration:   %s", formatDuration(time.Duration(duration*float64(time.Second)))))

		// Estimated file size (R5)
		if m.fileSize > 0 && m.lengthSeconds > 0 {
			bytesPerSec := float64(m.fileSize) / m.lengthSeconds
			estSize := int64(bytesPerSec * duration)
			lines = append(lines, fmt.Sprintf("  Est. Size:  %s", formatFileSize(estSize)))
		}

		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
			"  ⚠ This will create a new file with re-encoded video/audio",
		))

	case 2: // Progress
		lines = append(lines, TitleStyle.Render("Creating Trim")+" "+DimStyle.Render("(Step 3/3)"))
		lines = append(lines, "")

		// Spinner + percentage
		pctStr := fmt.Sprintf("%.1f%%", m.progressPercent)
		lines = append(lines, m.spinner.View()+" Encoding... "+lipgloss.NewStyle().Foreground(ColorCyan).Render(pctStr))
		lines = append(lines, "")

		// Progress bar
		barWidth := w - 4
		if barWidth < 10 {
			barWidth = 10
		}
		m.progressBar.Width = barWidth
		// Mux gradient: green (encoding) → yellow (muxing)
		progress.WithGradient(string(ColorDownloading), string(ColorMuxing))(&m.progressBar)
		lines = append(lines, "  "+m.progressBar.ViewAs(m.progressPercent/100))
		lines = append(lines, "")

		// Elapsed time
		elapsed := m.progressElapsed.Truncate(time.Second)
		lines = append(lines, DimStyle.Render(fmt.Sprintf("  Elapsed: %s", elapsed)))
	}

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	lines = append(lines, "")
	switch m.createStep {
	case 0:
		lines = append(lines, DimStyle.Render("Tab: Switch field | Enter: Continue | M: Delete mode | Esc: Cancel"))
	case 1:
		lines = append(lines, DimStyle.Render("Enter: Create Trim | Esc: Back"))
	case 2:
		lines = append(lines, DimStyle.Render("Esc: Continue In Background"))
	}

	return strings.Join(lines, "\n")
}

func (m *TrimDialogModel) renderDeleteMode(w, h int) string {
	var lines []string

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCookies).Bold(true).Render("Delete Trim"))
	lines = append(lines, "")

	if len(m.trims) == 0 {
		lines = append(lines, DimStyle.Render("No trims available to delete"))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("M: Create mode | Esc: Close"))
		return strings.Join(lines, "\n")
	}

	for i, tr := range m.trims {
		prefix := "  "
		if i == m.selectedTrimIdx {
			prefix = "▸ "
		}

		timeRange := fmt.Sprintf("%s - %s", formatTimeSeconds(tr.StartTime), formatTimeSeconds(tr.EndTime))
		dur := formatDuration(time.Duration(tr.Duration * float64(time.Second)))
		size := "?"
		if tr.FileSize > 0 {
			size = formatFileSize(tr.FileSize)
		}

		line := fmt.Sprintf("%s%s (%s, %s)", prefix, timeRange, dur, size)

		var style lipgloss.Style
		if m.deleteConfirmID == tr.ID {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))
		} else if i == m.selectedTrimIdx {
			style = lipgloss.NewStyle().Foreground(ColorCookies)
		} else {
			style = lipgloss.NewStyle()
		}
		lines = append(lines, style.Render(truncateString(line, w)))
	}

	if m.deleteConfirmID != "" {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
			"  ⚠ Press Enter again to confirm deletion",
		))
	}

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	if m.loading {
		lines = append(lines, "")
		lines = append(lines, m.spinner.View()+" Deleting...")
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("↑↓: Navigate | Enter: Delete | M: Create mode | Esc: Close"))

	return strings.Join(lines, "\n")
}

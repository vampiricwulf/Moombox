package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
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
	createStep     int // 0=time input, 1=confirmation
	startTimeInput string
	endTimeInput   string
	activeField    int // 0=start, 1=end
	errorMsg       string
	loading        bool

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
}

// NewTrimDialogModel creates a new trim dialog.
func NewTrimDialogModel() *TrimDialogModel {
	return &TrimDialogModel{}
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
	m.loading = loading
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
// "submit" for create, "delete" for delete confirm, "" for no action.
func (m *TrimDialogModel) HandleKey(key string) string {
	if key == keyEsc {
		return m.handleEscape()
	}

	// Mode toggle (R1)
	if key == "m" || key == "M" {
		m.toggleMode()
		return ""
	}

	if m.mode == TrimModeCreate {
		return m.handleCreateKey(key)
	}
	return m.handleDeleteKey(key)
}

func (m *TrimDialogModel) handleEscape() string {
	if m.mode == TrimModeCreate && m.createStep > 0 {
		m.createStep--
		m.errorMsg = ""
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
	}
	m.errorMsg = ""
}

func (m *TrimDialogModel) handleCreateKey(key string) string {
	switch m.createStep {
	case 0: // Time input
		switch key {
		case keyTab:
			m.activeField = 1 - m.activeField
		case keyEnter:
			return m.validateAndAdvance()
		default:
			if key != keyEnter {
				m.errorMsg = ""
			}
			if m.activeField == 0 {
				m.handleTextInput(key, &m.startTimeInput)
			} else {
				m.handleTextInput(key, &m.endTimeInput)
			}
		}

	case 1: // Confirmation
		if key == keyEnter {
			return "submit"
		}
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

func (m *TrimDialogModel) handleTextInput(key string, target *string) {
	switch key {
	case "backspace":
		if len(*target) > 0 {
			runes := []rune(*target)
			*target = string(runes[:len(runes)-1])
		}
	case "ctrl+v":
		if text := readClipboard(); text != "" {
			// Filter pasted text to only valid time characters
			for _, ch := range text {
				if (ch >= '0' && ch <= '9') || ch == ':' || ch == '.' {
					*target += string(ch)
				}
			}
		}
	default:
		// Only allow digits, colons, and dots for time input (match TS /^[0-9:.]$/)
		if len(key) == 1 && (key[0] >= '0' && key[0] <= '9' || key[0] == ':' || key[0] == '.') {
			*target += key
		}
	}
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
		stepStr := "Step 1/2"
		lines = append(lines, TitleStyle.Render("Create Trim")+" "+DimStyle.Render("("+stepStr+")"))
		lines = append(lines, "")

		if m.jobTitle != "" {
			lines = append(lines, DimStyle.Render(truncateString(m.jobTitle, w)))
		}
		if m.lengthSeconds > 0 {
			lines = append(lines, DimStyle.Render(fmt.Sprintf("Duration: %s", formatDuration(time.Duration(m.lengthSeconds*float64(time.Second))))))
		}
		lines = append(lines, "")

		startLabel := "  Start: "
		endLabel := "  End:   "
		if m.activeField == 0 {
			startLabel = "> Start: "
		}
		if m.activeField == 1 {
			endLabel = "> End:   "
		}

		startStyle := DimStyle
		endStyle := DimStyle
		if m.activeField == 0 {
			startStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}
		if m.activeField == 1 {
			endStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		lines = append(lines, startStyle.Render(startLabel)+renderInputBox(m.startTimeInput, w-12, false))
		lines = append(lines, endStyle.Render(endLabel)+renderInputBox(m.endTimeInput, w-12, false))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("Format: HH:MM:SS, MM:SS, or seconds"))

	case 1: // Confirmation (R3)
		stepStr := "Step 2/2"
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
	}

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	if m.loading {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("⏳ Creating trim, please wait..."))
	}

	lines = append(lines, "")
	switch m.createStep {
	case 0:
		lines = append(lines, DimStyle.Render("Tab: Switch field | Enter: Continue | M: Delete mode | Esc: Cancel"))
	case 1:
		lines = append(lines, DimStyle.Render("Enter: Create Trim | Esc: Back"))
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
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("⏳ Deleting..."))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("↑↓: Navigate | Enter: Delete | M: Create mode | Esc: Close"))

	return strings.Join(lines, "\n")
}

// ensure runewidth is used
var _ = runewidth.StringWidth

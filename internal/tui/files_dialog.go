package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// OrphanedFileEntry represents an orphaned file for TUI display.
type OrphanedFileEntry struct {
	Path      string
	RelPath   string
	Type      string // "staging", "output", "trim"
	Size      int64
	Modified  string
	JobID     string
	JobTitle  string
	JobStatus string
}

// FilesDialogModel manages the orphaned files dialog.
type FilesDialogModel struct {
	visible         bool
	width, height   int
	files           []OrphanedFileEntry
	selectedIdx     int
	scrollOffset    int
	deleteConfirmID string // path of file pending double-press confirm
	confirmTimer    time.Time
	loading         bool
	errorMsg        string
	feedbackMsg     string
}

// NewFilesDialogModel creates a new files dialog.
func NewFilesDialogModel() *FilesDialogModel {
	return &FilesDialogModel{}
}

// Open shows the dialog and sets loading state.
func (m *FilesDialogModel) Open() {
	m.visible = true
	m.loading = true
	m.files = nil
	m.selectedIdx = 0
	m.scrollOffset = 0
	m.deleteConfirmID = ""
	m.confirmTimer = time.Time{}
	m.errorMsg = ""
	m.feedbackMsg = ""
}

// Close hides the dialog.
func (m *FilesDialogModel) Close() {
	m.visible = false
}

// IsVisible returns true if the dialog is shown.
func (m *FilesDialogModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the dialog dimensions.
func (m *FilesDialogModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// SetFiles populates the file list after async fetch.
func (m *FilesDialogModel) SetFiles(files []OrphanedFileEntry) {
	m.files = files
	m.loading = false
	m.errorMsg = ""
	if m.selectedIdx >= len(files) {
		m.selectedIdx = 0
	}
}

// SetError sets an error message.
func (m *FilesDialogModel) SetError(msg string) {
	m.errorMsg = msg
	m.loading = false
}

// SelectedFile returns the currently selected file entry.
func (m *FilesDialogModel) SelectedFile() *OrphanedFileEntry {
	if m.selectedIdx >= 0 && m.selectedIdx < len(m.files) {
		return &m.files[m.selectedIdx]
	}
	return nil
}

// RemoveFile removes a file by path from the list after successful deletion.
func (m *FilesDialogModel) RemoveFile(path string) {
	for i, f := range m.files {
		if f.Path == path {
			m.files = append(m.files[:i], m.files[i+1:]...)
			if m.selectedIdx >= len(m.files) && m.selectedIdx > 0 {
				m.selectedIdx--
			}
			m.deleteConfirmID = ""
			break
		}
	}
}

// HandleKey processes key input. Returns action string:
// "close", "refresh", "delete", or "" for no action.
func (m *FilesDialogModel) HandleKey(key string) string {
	// Confirmation timeout check
	if m.deleteConfirmID != "" && !m.confirmTimer.IsZero() && time.Now().After(m.confirmTimer) {
		m.deleteConfirmID = ""
		m.confirmTimer = time.Time{}
		m.feedbackMsg = ""
	}

	switch key {
	case keyEsc:
		m.Close()
		return "close"
	case "r", "R":
		m.loading = true
		m.errorMsg = ""
		return "refresh"
	case keyUp:
		if m.selectedIdx > 0 {
			m.selectedIdx--
		} else if len(m.files) > 0 {
			m.selectedIdx = len(m.files) - 1
		}
		m.deleteConfirmID = ""
		m.feedbackMsg = ""
		m.ensureVisible()
	case keyDown:
		if m.selectedIdx < len(m.files)-1 {
			m.selectedIdx++
		} else {
			m.selectedIdx = 0
		}
		m.deleteConfirmID = ""
		m.feedbackMsg = ""
		m.ensureVisible()
	case "d", "D":
		if len(m.files) == 0 {
			return ""
		}
		sel := m.SelectedFile()
		if sel == nil {
			return ""
		}
		if m.deleteConfirmID == sel.Path && !m.confirmTimer.IsZero() && time.Now().Before(m.confirmTimer) {
			// Second press: execute delete
			m.deleteConfirmID = ""
			m.confirmTimer = time.Time{}
			return "delete"
		}
		// First press: set confirmation
		m.deleteConfirmID = sel.Path
		m.confirmTimer = time.Now().Add(3 * time.Second)
		m.feedbackMsg = fmt.Sprintf("Press D again to delete \"%s\"", sel.RelPath)
	}
	return ""
}

func (m *FilesDialogModel) ensureVisible() {
	boxH := min(m.height-4, 24)
	listH := boxH - 6 // header + footer lines
	if listH < 1 {
		listH = 1
	}
	if m.selectedIdx < m.scrollOffset {
		m.scrollOffset = m.selectedIdx
	}
	if m.selectedIdx >= m.scrollOffset+listH {
		m.scrollOffset = m.selectedIdx - listH + 1
	}
}

// View renders the files dialog overlay.
func (m *FilesDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := min(80, m.width-4)
	boxH := min(24, m.height-4)
	if boxW < 40 {
		boxW = 40
	}
	if boxH < 10 {
		boxH = 10
	}

	contentW := boxW - 2

	var lines []string

	lines = append(lines, TitleStyle.Render("Orphaned Files")+" "+DimStyle.Render(fmt.Sprintf("(%d files)", len(m.files))))
	lines = append(lines, "")

	if m.loading {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("  Scanning..."))
	} else if m.errorMsg != "" {
		lines = append(lines, ErrorStyle.Render("  "+m.errorMsg))
	} else if len(m.files) == 0 {
		lines = append(lines, DimStyle.Render("  No orphaned files found."))
	} else {
		listH := boxH - 6 // Reserve for header, footer, padding
		if listH < 1 {
			listH = 1
		}

		end := m.scrollOffset + listH
		if end > len(m.files) {
			end = len(m.files)
		}
		start := m.scrollOffset

		for i := start; i < end; i++ {
			f := m.files[i]
			prefix := "  "
			if i == m.selectedIdx {
				prefix = "▸ "
			}

			// Type badge
			typeStr := fmt.Sprintf("[%s]", f.Type)
			typeStyle := DimStyle
			switch f.Type {
			case "staging":
				typeStyle = lipgloss.NewStyle().Foreground(ColorMuxing)
			case "output":
				typeStyle = lipgloss.NewStyle().Foreground(ColorCyan)
			case "trim":
				typeStyle = lipgloss.NewStyle().Foreground(ColorGray)
			}

			sizeStr := formatFileSize(f.Size)
			line := fmt.Sprintf("%s%s %s (%s)", prefix, typeStyle.Render(fmt.Sprintf("%-9s", typeStr)), f.RelPath, sizeStr)

			var style lipgloss.Style
			if m.deleteConfirmID == f.Path {
				style = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))
			} else if i == m.selectedIdx {
				style = lipgloss.NewStyle().Foreground(ColorCyan)
			} else {
				style = lipgloss.NewStyle()
			}
			lines = append(lines, style.Render(truncateString(line, contentW)))
		}

		// Scroll indicator
		if len(m.files) > listH {
			pct := 0
			maxScroll := len(m.files) - listH
			if maxScroll > 0 {
				pct = m.scrollOffset * 100 / maxScroll
			}
			lines = append(lines, DimStyle.Render(fmt.Sprintf("  [%d%%] %d files", pct, len(m.files))))
		}
	}

	if m.feedbackMsg != "" {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("  "+m.feedbackMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("↑↓: Navigate | D: Delete | R: Refresh | Esc: Close"))

	content := strings.Join(lines, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(contentW).
		Height(boxH).
		Render(content)

	return centerBox(box, m.width, m.height)
}

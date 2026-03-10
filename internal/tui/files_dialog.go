package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
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

// fileItem wraps OrphanedFileEntry as a list.Item.
type fileItem struct {
	entry OrphanedFileEntry
}

func (f fileItem) FilterValue() string { return f.entry.RelPath }

// fileDelegate renders file list items with type badges and delete confirmation.
type fileDelegate struct {
	dialog *FilesDialogModel
}

func (d fileDelegate) Height() int                             { return 1 }
func (d fileDelegate) Spacing() int                            { return 0 }
func (d fileDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d fileDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	fi, ok := item.(fileItem)
	if !ok {
		return
	}
	f := fi.entry

	prefix := "  "
	if index == m.Index() {
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
	typeTag := fmt.Sprintf("%-9s", typeStr)
	suffix := " (" + sizeStr + ")"

	// Truncate the variable-width path to fit (truncate plain text BEFORE
	// assembling with styled type badge — truncateString uses runewidth
	// which miscounts ANSI escape sequences).
	fixedW := 2 + 9 + 1 + len(suffix) // prefix + typeTag + space + suffix (all ASCII)
	pathW := m.Width() - fixedW
	if pathW < 5 {
		pathW = 5
	}
	relPath := truncateString(f.RelPath, pathW)

	var style lipgloss.Style
	if d.dialog != nil && d.dialog.deleteConfirmID == f.Path {
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))
	} else if index == m.Index() {
		style = lipgloss.NewStyle().Foreground(ColorCyan)
	} else {
		style = lipgloss.NewStyle()
	}

	// Use the outer style for type tag when selected/confirm, otherwise per-type color
	renderTypeStyle := typeStyle
	if (d.dialog != nil && d.dialog.deleteConfirmID == f.Path) || index == m.Index() {
		renderTypeStyle = style
	}

	line := prefix + renderTypeStyle.Render(typeTag) + " " + relPath + suffix
	fmt.Fprint(w, style.Render(line))
}

// FilesDialogModel manages the orphaned files dialog.
type FilesDialogModel struct {
	visible         bool
	width, height   int
	list            list.Model
	deleteConfirmID string // path of file pending double-press confirm
	confirmTimer    time.Time
	loading         bool
	errorMsg        string
	feedbackMsg     string

	// Loading spinner
	spinner spinner.Model
}

// NewFilesDialogModel creates a new files dialog.
func NewFilesDialogModel() *FilesDialogModel {
	m := &FilesDialogModel{
		spinner: newSpinner(),
	}
	m.list = m.newFileList()
	return m
}

func (m *FilesDialogModel) newFileList() list.Model {
	delegate := fileDelegate{dialog: m}
	l := list.New(nil, delegate, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowFilter(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	l.InfiniteScrolling = true

	// Only use arrow keys for navigation (avoid letter key conflicts)
	km := l.KeyMap
	km.CursorUp.SetKeys(keyUp)
	km.CursorDown.SetKeys(keyDown)
	km.NextPage.SetKeys(keyPgDown)
	km.PrevPage.SetKeys(keyPgUp)
	km.GoToStart.SetKeys(keyHome)
	km.GoToEnd.SetKeys(keyEnd)
	l.KeyMap = km

	return l
}

// Open shows the dialog and sets loading state.
func (m *FilesDialogModel) Open() {
	m.visible = true
	m.loading = true
	m.spinner = newSpinner()
	m.list.SetItems(nil)
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
	boxW := min(80, w-4)
	boxH := min(24, h-4)
	if boxW < 40 {
		boxW = 40
	}
	if boxH < 10 {
		boxH = 10
	}
	listH := boxH - 6
	if listH < 1 {
		listH = 1
	}
	m.list.SetSize(boxW-2, listH)
}

// SetFiles populates the file list after async fetch.
func (m *FilesDialogModel) SetFiles(files []OrphanedFileEntry) tea.Cmd {
	m.loading = false
	m.errorMsg = ""
	items := make([]list.Item, len(files))
	for i, f := range files {
		items[i] = fileItem{entry: f}
	}
	return m.list.SetItems(items)
}

// SetError sets an error message.
func (m *FilesDialogModel) SetError(msg string) {
	m.errorMsg = msg
	m.loading = false
}

// SelectedFile returns the currently selected file entry.
func (m *FilesDialogModel) SelectedFile() *OrphanedFileEntry {
	sel := m.list.SelectedItem()
	if sel == nil {
		return nil
	}
	fi, ok := sel.(fileItem)
	if !ok {
		return nil
	}
	return &fi.entry
}

// RemoveFile removes a file by path from the list after successful deletion.
func (m *FilesDialogModel) RemoveFile(path string) {
	for i, item := range m.list.Items() {
		fi, ok := item.(fileItem)
		if ok && fi.entry.Path == path {
			m.list.RemoveItem(i)
			m.deleteConfirmID = ""
			break
		}
	}
}

// UpdateComponents routes tea.Msg to the spinner when loading.
func (m *FilesDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	if m.loading {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
	return nil
}

// SpinnerInit returns the spinner's initial tick command.
func (m *FilesDialogModel) SpinnerInit() tea.Cmd { return m.spinner.Tick }

// HandleKey processes key input. Returns action string and optional command:
// "close", "refresh", "delete", or "" for no action.
func (m *FilesDialogModel) HandleKey(msg tea.KeyMsg) (string, tea.Cmd) {
	// Confirmation timeout check
	if m.deleteConfirmID != "" && !m.confirmTimer.IsZero() && time.Now().After(m.confirmTimer) {
		m.deleteConfirmID = ""
		m.confirmTimer = time.Time{}
		m.feedbackMsg = ""
	}

	key := msg.String()
	switch key {
	case keyEsc:
		m.Close()
		return "close", nil
	case "r", "R":
		m.loading = true
		m.errorMsg = ""
		return "refresh", nil
	case "d", "D":
		if len(m.list.Items()) == 0 {
			return "", nil
		}
		sel := m.SelectedFile()
		if sel == nil {
			return "", nil
		}
		if m.deleteConfirmID == sel.Path && !m.confirmTimer.IsZero() && time.Now().Before(m.confirmTimer) {
			// Second press: execute delete
			m.deleteConfirmID = ""
			m.confirmTimer = time.Time{}
			return "delete", nil
		}
		// First press: set confirmation
		m.deleteConfirmID = sel.Path
		m.confirmTimer = time.Now().Add(3 * time.Second)
		m.feedbackMsg = fmt.Sprintf("Press D again to delete \"%s\"", sel.RelPath)
		return "", nil
	}

	// Reset confirm on navigation
	if key == keyUp || key == keyDown {
		m.deleteConfirmID = ""
		m.feedbackMsg = ""
	}

	// Route to list for navigation
	if !m.loading && len(m.list.Items()) > 0 {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return "", cmd
	}
	return "", nil
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
	fileCount := len(m.list.Items())

	var lines []string

	lines = append(lines, TitleStyle.Render("Orphaned Files")+" "+DimStyle.Render(fmt.Sprintf("(%d files)", fileCount)))
	lines = append(lines, "")

	if m.loading {
		lines = append(lines, "  "+m.spinner.View()+" Scanning...")
	} else if m.errorMsg != "" {
		lines = append(lines, ErrorStyle.Render("  "+m.errorMsg))
	} else if fileCount == 0 {
		lines = append(lines, DimStyle.Render("  No orphaned files found."))
	} else {
		lines = append(lines, m.list.View())
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

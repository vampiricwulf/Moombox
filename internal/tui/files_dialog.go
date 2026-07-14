package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// OrphanedHistoryEntry is a processing-history row with no matching job, shown
// in the same overlay as orphaned files. While it remains, the monitor treats
// the video as already-processed and won't re-discover it; removing it unblocks
// the video.
type OrphanedHistoryEntry struct {
	VideoID string
	AddedAt string
}

// historyItem wraps OrphanedHistoryEntry as a list.Item.
type historyItem struct {
	entry OrphanedHistoryEntry
}

func (h historyItem) FilterValue() string { return h.entry.VideoID }

// sectionHeaderItem is a non-selectable divider between the files and history
// groups. Navigation skips it and it is only inserted when both groups are
// non-empty, so the cursor never starts on it.
type sectionHeaderItem struct {
	label string
}

func (s sectionHeaderItem) FilterValue() string { return "" }

// fileDelegate renders file list items with type badges and delete confirmation.
type fileDelegate struct {
	dialog *FilesDialogModel
}

func (d fileDelegate) Height() int                             { return 1 }
func (d fileDelegate) Spacing() int                            { return 0 }
func (d fileDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d fileDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	switch it := item.(type) {
	case sectionHeaderItem:
		fmt.Fprint(w, DimStyle.Render("  "+it.label))
	case historyItem:
		d.renderHistory(w, m, index, it.entry)
	case fileItem:
		d.renderFile(w, m, index, it.entry)
	}
}

// renderHistory draws one orphaned-history row: a [history] badge, the video ID,
// and when it entered the history table.
func (d fileDelegate) renderHistory(w io.Writer, m list.Model, index int, h OrphanedHistoryEntry) {
	prefix := "  "
	if index == m.Index() {
		prefix = "▸ "
	}

	var style lipgloss.Style
	if d.dialog != nil && d.dialog.deleteConfirmID == h.VideoID {
		style = YellowStyle
	} else if index == m.Index() {
		style = lipgloss.NewStyle().Foreground(ColorCyan)
	} else {
		style = lipgloss.NewStyle()
	}

	badgeStyle := lipgloss.NewStyle().Foreground(ColorGray)
	if (d.dialog != nil && d.dialog.deleteConfirmID == h.VideoID) || index == m.Index() {
		badgeStyle = style
	}

	added := h.AddedAt
	if len(added) >= 16 {
		added = strings.Replace(added[:16], "T", " ", 1)
	}
	badgeTag := fmt.Sprintf("%-9s", "[history]")
	line := prefix + badgeStyle.Render(badgeTag) + " " + h.VideoID + "  " + added
	fmt.Fprint(w, style.Render(line))
}

func (d fileDelegate) renderFile(w io.Writer, m list.Model, index int, f OrphanedFileEntry) {
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
	pathW := max(m.Width()-fixedW, 5)
	relPath := truncateString(f.RelPath, pathW)

	var style lipgloss.Style
	if d.dialog != nil && d.dialog.deleteConfirmID == f.Path {
		style = YellowStyle
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
	deleteConfirmID string // path (file) or video ID (history) pending confirm
	confirmTimer    time.Time
	loading         bool
	filesErr        string // orphaned-files fetch failed (empty = ok)
	historyErr      string // orphaned-history fetch failed (empty = ok)
	actionErr       string // transient error from a delete; the list stays visible
	feedbackMsg     string

	// Two async sources feed one list: orphaned files and orphaned history.
	files         []OrphanedFileEntry
	history       []OrphanedHistoryEntry
	filesLoaded   bool
	historyLoaded bool

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
	m.filesErr = ""
	m.historyErr = ""
	m.actionErr = ""
	m.feedbackMsg = ""
	m.files = nil
	m.history = nil
	m.filesLoaded = false
	m.historyLoaded = false
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
	boxW := max(min(80, w-4), 40)
	boxH := max(min(24, h-4), 10)
	listH := max(boxH-6, 1)
	m.list.SetSize(boxW-2, listH)
}

// SetFiles records the orphaned-files result and rebuilds the combined list.
func (m *FilesDialogModel) SetFiles(files []OrphanedFileEntry) tea.Cmd {
	m.files = files
	m.filesErr = ""
	m.filesLoaded = true
	return m.finishLoad()
}

// SetHistory records the orphaned-history result and rebuilds the combined list.
func (m *FilesDialogModel) SetHistory(history []OrphanedHistoryEntry) tea.Cmd {
	m.history = history
	m.historyErr = ""
	m.historyLoaded = true
	return m.finishLoad()
}

// finishLoad clears the loading state once BOTH sources have reported, and
// rebuilds the list. Called after each source arrives.
func (m *FilesDialogModel) finishLoad() tea.Cmd {
	if m.filesLoaded && m.historyLoaded {
		m.loading = false
	}
	return m.rebuildList()
}

// rebuildList assembles the single list: files, then a divider (only when both
// groups are non-empty), then history entries.
func (m *FilesDialogModel) rebuildList() tea.Cmd {
	items := make([]list.Item, 0, len(m.files)+len(m.history)+1)
	for _, f := range m.files {
		items = append(items, fileItem{entry: f})
	}
	if len(m.files) > 0 && len(m.history) > 0 {
		items = append(items, sectionHeaderItem{label: "── Orphaned History Entries ──"})
	}
	for _, h := range m.history {
		items = append(items, historyItem{entry: h})
	}
	return m.list.SetItems(items)
}

// SetFilesError records a failure to load the orphaned files. The two sources
// are independent: if the history loaded, it is still shown, and this error
// appears only as an inline warning (or as the main message when there is
// nothing else to show). Mirrors SetHistoryError.
func (m *FilesDialogModel) SetFilesError(msg string) tea.Cmd {
	m.filesErr = msg
	m.files = nil
	m.filesLoaded = true
	return m.finishLoad()
}

// SetHistoryError records a failure to load the orphaned history. Symmetric to
// SetFilesError — the files section still shows.
func (m *FilesDialogModel) SetHistoryError(msg string) tea.Cmd {
	m.historyErr = msg
	m.history = nil
	m.historyLoaded = true
	return m.finishLoad()
}

// SetActionError shows a transient error from a delete action without hiding the
// list, which remains valid.
func (m *FilesDialogModel) SetActionError(msg string) {
	m.actionErr = msg
	m.feedbackMsg = ""
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

// SelectedHistory returns the currently selected history entry, or nil if the
// selection is not a history row.
func (m *FilesDialogModel) SelectedHistory() *OrphanedHistoryEntry {
	sel := m.list.SelectedItem()
	if sel == nil {
		return nil
	}
	hi, ok := sel.(historyItem)
	if !ok {
		return nil
	}
	return &hi.entry
}

// RemoveHistory removes a history entry by video ID from the list after
// successful deletion, then drops the divider if the history group is now empty.
func (m *FilesDialogModel) RemoveHistory(videoID string) {
	for i, h := range m.history {
		if h.VideoID == videoID {
			m.history = append(m.history[:i], m.history[i+1:]...)
			break
		}
	}
	m.deleteConfirmID = ""
	m.actionErr = ""
	m.rebuildList()
}

// RemoveFile removes a file by path from the backing slice after successful
// deletion, then rebuilds so the divider drops when the files group empties and
// the file cannot reappear on a later rebuild (m.files is the source of truth).
func (m *FilesDialogModel) RemoveFile(path string) {
	for i, f := range m.files {
		if f.Path == path {
			m.files = append(m.files[:i], m.files[i+1:]...)
			break
		}
	}
	m.deleteConfirmID = ""
	m.actionErr = ""
	m.rebuildList()
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
func (m *FilesDialogModel) SpinnerInit() tea.Cmd { return spinnerTickCmd(m.spinner) }

// HandleKey processes key input. Returns action string and optional command:
// "close", "refresh", "delete", or "" for no action.
func (m *FilesDialogModel) HandleKey(msg tea.KeyPressMsg) (string, tea.Cmd) {
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
		m.filesErr = ""
		m.historyErr = ""
		m.actionErr = ""
		m.filesLoaded = false
		m.historyLoaded = false
		return "refresh", nil
	case "d", "D":
		if len(m.list.Items()) == 0 {
			return "", nil
		}
		m.actionErr = "" // starting a fresh delete clears any prior failure
		// Files: two-press confirm, returns "delete" (routed to file deletion).
		if f := m.SelectedFile(); f != nil {
			if m.deleteConfirmID == f.Path && !m.confirmTimer.IsZero() && time.Now().Before(m.confirmTimer) {
				m.deleteConfirmID = ""
				m.confirmTimer = time.Time{}
				return "delete", nil
			}
			m.deleteConfirmID = f.Path
			m.confirmTimer = time.Now().Add(3 * time.Second)
			m.feedbackMsg = fmt.Sprintf("Press D again to delete \"%s\"", f.RelPath)
			return "", nil
		}
		// History: two-press confirm, returns "delete-history".
		if h := m.SelectedHistory(); h != nil {
			if m.deleteConfirmID == h.VideoID && !m.confirmTimer.IsZero() && time.Now().Before(m.confirmTimer) {
				m.deleteConfirmID = ""
				m.confirmTimer = time.Time{}
				return "delete-history", nil
			}
			m.deleteConfirmID = h.VideoID
			m.confirmTimer = time.Now().Add(3 * time.Second)
			m.feedbackMsg = fmt.Sprintf("Press D again to remove history for %s", h.VideoID)
			return "", nil
		}
		return "", nil // section divider selected — nothing to delete
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
		// The section divider is non-selectable; step past it in the same
		// direction so the cursor never lands on it.
		if _, isHeader := m.list.SelectedItem().(sectionHeaderItem); isHeader {
			m.list, _ = m.list.Update(msg)
		}
		return "", cmd
	}
	return "", nil
}

// loadErrorText composes a single message from the two independent fetch
// errors. Empty when both sources loaded. When only one failed it names that
// source, so a files-fetch failure never hides loaded history (and vice versa).
func (m *FilesDialogModel) loadErrorText() string {
	switch {
	case m.filesErr != "" && m.historyErr != "":
		return "Couldn't load orphaned files or history."
	case m.filesErr != "":
		return "Couldn't scan orphaned files: " + m.filesErr
	case m.historyErr != "":
		return "Couldn't load orphaned history: " + m.historyErr
	default:
		return ""
	}
}

// View renders the files dialog overlay.
func (m *FilesDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := max(min(80, m.width-4), 40)
	boxH := max(min(24, m.height-4), 10)

	total := len(m.list.Items())
	loadErr := m.loadErrorText()

	var lines []string

	lines = append(lines, TitleStyle.Render("Orphaned")+" "+DimStyle.Render(fmt.Sprintf("(%d files, %d history)", len(m.files), len(m.history))))
	lines = append(lines, "")

	if m.loading {
		lines = append(lines, "  "+m.spinner.View()+" Scanning...")
	} else if total == 0 {
		// Nothing to list: show the load error if any, else the empty message.
		if loadErr != "" {
			lines = append(lines, ErrorStyle.Render("  "+loadErr))
		} else {
			lines = append(lines, DimStyle.Render("  No orphaned files or history entries found."))
		}
	} else {
		lines = append(lines, m.list.View())
		// One source loaded, the other failed — keep the list, warn inline.
		if loadErr != "" {
			lines = append(lines, "", ErrorStyle.Render("  ⚠ "+loadErr))
		}
	}

	if m.actionErr != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render("  "+m.actionErr))
	}

	if m.feedbackMsg != "" {
		lines = append(lines, "")
		lines = append(lines, YellowStyle.Render("  "+m.feedbackMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("↑↓: Navigate | D: Delete | R: Refresh | Esc: Close"))

	content := strings.Join(lines, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
}

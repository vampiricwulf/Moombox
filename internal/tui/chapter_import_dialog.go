package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// ChapterImportRow is one chapter of a ChapterImportView: a span on the
// player's global timeline, in seconds, and its title.
type ChapterImportRow struct {
	Start, End float64
	Title      string
}

// ChapterImportView is what the chapter import's preview, save and restore
// answer (routes.ChapterImportView, converted in cmd/moombox): the list as
// it would be (or was) saved, the job's current chapters, whether an
// original is kept to restore, and the preview's reason when the paste
// yields no chapter.
type ChapterImportView struct {
	Chapters             []ChapterImportRow
	Current              []ChapterImportRow
	HasOriginal          bool
	OffsetSeconds        float64
	DefaultOffsetSeconds float64
	Split                bool
	Error                string
	Embedded             bool
	EmbedError           string
}

// The steps of the A H overlay.
const (
	chapterImportStepEdit    = iota // pasting / editing, the preview following
	chapterImportStepConfirm        // Ctrl+S pressed: y saves, anything else goes back
	chapterImportStepSaving         // App.OnSaveChapters is in flight
)

// chapterPreviewDebounce is how long the preview waits after the last edit
// before it asks: a paste is one message, typing is many, and each preview
// probes the archive's length. The Web dialog's CHAPTER_PREVIEW_DEBOUNCE_MS.
const chapterPreviewDebounce = 300 * time.Millisecond

// chapterPreviewTickMsg is the debounce firing for edit number Seq; only the
// newest edit's tick asks for a preview.
type chapterPreviewTickMsg struct{ Seq int }

// ChapterImportDialogModel is the A H overlay (owner decision CI-4): a
// multi-line paste box (bubbles textarea) and the offset, with the parsed
// preview below them — the answer of the same preview the Web dialog asks
// for (App.OnPreviewChapters → routes.ChapterService.Preview), never a parse
// of its own — and a save that needs a confirm keypress.
//
// Like every overlay here, it never feeds its components from HandleKey:
// App.Update's keypress arm reaches UpdateComponents first and HandleKey
// second with the same message (see CookieImportDialogModel.HandleKey).
type ChapterImportDialogModel struct {
	visible       bool
	width, height int
	jobID, title  string
	text          textarea.Model
	offset        textinput.Model
	focusOffset   bool
	step          int
	// seq numbers the edits; a preview answers for the edit it was asked
	// for, and only the newest one is drawn.
	seq        int
	view       *ChapterImportView
	previewErr string
	saveErr    string
}

// NewChapterImportDialogModel creates the chapter import overlay.
func NewChapterImportDialogModel() *ChapterImportDialogModel {
	ta := textarea.New()
	ta.Placeholder = "Paste a timestamp comment: 0:00 Intro, 12:34 First song, …"
	ta.ShowLineNumbers = false
	ta.SetHeight(8)
	ti := newTextInput()
	ti.Prompt = "Offset: "
	ti.Placeholder = "0 (seconds or ±H:MM:SS)"
	return &ChapterImportDialogModel{text: ta, offset: ti}
}

// IsVisible returns true if the dialog is shown.
func (m *ChapterImportDialogModel) IsVisible() bool { return m.visible }

// IsSaving reports whether the overlay waits for the save IT started.
func (m *ChapterImportDialogModel) IsSaving() bool {
	return m.visible && m.step == chapterImportStepSaving
}

// SetSize updates the dialog dimensions.
func (m *ChapterImportDialogModel) SetSize(w, h int) {
	m.width, m.height = w, h
	if _, contentW := dialogBox(90, w); contentW > 0 {
		m.text.SetWidth(max(contentW-2, 10))
		m.offset.SetWidth(max(contentW-12, 10))
	}
	m.text.SetHeight(max(min(h/3, 12), 3))
}

// Open shows the dialog for job with an empty paste and asks for the first
// preview at once (it brings the current chapters and the offset to start
// at). The returned command is the textarea's focus plus that preview's tick.
func (m *ChapterImportDialogModel) Open(job *database.Job) tea.Cmd {
	m.visible = true
	m.jobID, m.title = job.ID, job.Title
	m.step = chapterImportStepEdit
	m.view, m.previewErr, m.saveErr = nil, "", ""
	m.text.SetValue("")
	m.offset.SetValue("")
	m.focusOffset = false
	m.offset.Blur()
	m.seq++
	seq := m.seq
	return tea.Batch(m.text.Focus(), func() tea.Msg { return chapterPreviewTickMsg{Seq: seq} })
}

// Close hides the dialog.
func (m *ChapterImportDialogModel) Close() {
	m.visible = false
	m.seq++
	m.text.Blur()
	m.offset.Blur()
}

// Request is the job, paste and offset a preview or save is for.
func (m *ChapterImportDialogModel) Request() (jobID, text, offset string) {
	return m.jobID, m.text.Value(), m.offset.Value()
}

// SetPreview draws the answer to the preview asked for edit seq, if it is
// still the newest. The opening preview's offset fills an empty field.
func (m *ChapterImportDialogModel) SetPreview(seq int, v ChapterImportView, err error) {
	if !m.visible || seq != m.seq {
		return
	}
	if err != nil {
		m.view, m.previewErr = nil, err.Error()
		return
	}
	m.view, m.previewErr = &v, ""
	if m.offset.Value() == "" && v.DefaultOffsetSeconds != 0 {
		loadTextInput(&m.offset, fmt.Sprintf("%g", v.DefaultOffsetSeconds))
	}
}

// previewOK reports whether the drawn preview is a list Save can write.
func (m *ChapterImportDialogModel) previewOK() bool {
	return m.view != nil && m.view.Error == "" && len(m.view.Chapters) > 0 && m.text.Value() != ""
}

// SetSaving moves to the saving step.
func (m *ChapterImportDialogModel) SetSaving() {
	m.step = chapterImportStepSaving
	m.saveErr = ""
}

// SetSaveError returns to editing with the save's refusal shown.
func (m *ChapterImportDialogModel) SetSaveError(err error) {
	m.step = chapterImportStepEdit
	m.saveErr = err.Error()
}

// HandleKey processes one keypress. It returns "save" when the operator
// confirmed a save, "close" when the overlay closed, and "" otherwise.
func (m *ChapterImportDialogModel) HandleKey(key string) string {
	switch m.step {
	case chapterImportStepConfirm:
		if key == "y" || key == keyEnter {
			return "save"
		}
		m.step = chapterImportStepEdit
		return ""
	case chapterImportStepSaving:
		if key == keyEsc {
			m.Close()
			return "close"
		}
		return ""
	}
	switch key {
	case keyEsc:
		m.Close()
		return "close"
	case keyTab, keyShiftTab:
		m.focusOffset = !m.focusOffset
		if m.focusOffset {
			m.text.Blur()
			m.offset.Focus()
		} else {
			m.offset.Blur()
			m.text.Focus()
		}
	case "ctrl+s":
		if m.previewOK() {
			m.step = chapterImportStepConfirm
		}
	}
	return ""
}

// UpdateComponents feeds the focused field while editing. An edit that
// changed the paste or the offset starts the preview's debounce: the
// returned command fires chapterPreviewTickMsg for this edit after
// chapterPreviewDebounce, and App.Update asks for the preview only if no
// later edit came first. The overlay's own keys (Tab, Ctrl+S, Esc) are not
// fed: they are structural, and a textarea would take a Tab as text.
func (m *ChapterImportDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible || m.step != chapterImportStepEdit {
		return nil
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case keyTab, keyShiftTab, "ctrl+s", keyEsc:
			return nil
		}
	}
	before := m.text.Value() + "\x00" + m.offset.Value()
	var cmd tea.Cmd
	if m.focusOffset {
		m.offset, cmd = m.offset.Update(msg)
	} else {
		m.text, cmd = m.text.Update(msg)
	}
	if m.text.Value()+"\x00"+m.offset.Value() == before {
		return cmd
	}
	m.seq++
	seq := m.seq
	m.saveErr = ""
	return tea.Batch(cmd, tea.Tick(chapterPreviewDebounce, func(time.Time) tea.Msg { return chapterPreviewTickMsg{Seq: seq} }))
}

// chapterClock is a chapter start as the Web dialog writes it
// (formatTimestamp): M:SS, or H:MM:SS from the first hour.
func chapterClock(sec float64) string {
	s := max(int64(sec), 0)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// View renders the overlay.
func (m *ChapterImportDialogModel) View() string {
	if !m.visible {
		return ""
	}
	boxW, contentW := dialogBox(90, m.width)
	boxH := max(m.height-4, 16)

	var b strings.Builder
	b.WriteString(TitleStyle.Render("Import Chapters"))
	b.WriteString("\n")
	b.WriteString(DimStyle.Render(truncateString(m.title, max(contentW, 10))))
	b.WriteString("\n\n")
	b.WriteString(m.text.View())
	b.WriteString("\n")
	b.WriteString(m.offset.View())
	b.WriteString("\n\n")

	// The rows the preview may take: what is left of the box below the
	// fields, its own headings and the key line.
	rowsLeft := max(boxH-m.text.Height()-12, 3)
	switch {
	case m.previewErr != "":
		b.WriteString(ErrorStyle.Render(m.previewErr))
		b.WriteString("\n")
	case m.view == nil:
		b.WriteString(DimStyle.Render("Reading the job's chapters…"))
		b.WriteString("\n")
	case m.text.Value() == "":
		b.WriteString(DimStyle.Render("Paste a timestamp comment to preview its chapters"))
		b.WriteString("\n")
	case m.view.Error != "":
		b.WriteString(ErrorStyle.Render(m.view.Error))
		b.WriteString("\n")
	default:
		b.WriteString(fmt.Sprintf("Preview: %d chapters", len(m.view.Chapters)))
		if m.view.OffsetSeconds != 0 {
			b.WriteString(DimStyle.Render(fmt.Sprintf("  (offset %gs)", m.view.OffsetSeconds)))
		}
		b.WriteString("\n")
		b.WriteString(m.renderRows(m.view.Chapters, rowsLeft, contentW))
	}
	if m.view != nil {
		b.WriteString(DimStyle.Render(fmt.Sprintf("Current: %d chapters — replaced on save", len(m.view.Current))))
		if m.view.HasOriginal {
			b.WriteString(DimStyle.Render(" · the original can be restored (A U)"))
		}
		b.WriteString("\n")
	}
	if m.saveErr != "" {
		b.WriteString(ErrorStyle.Render("Not saved: " + m.saveErr))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	switch m.step {
	case chapterImportStepConfirm:
		n, cur := 0, 0
		if m.view != nil {
			n, cur = len(m.view.Chapters), len(m.view.Current)
		}
		b.WriteString(YellowStyle.Render(fmt.Sprintf("Save %d chapters, replacing the current %d? y: Save  any other key: Back", n, cur)))
	case chapterImportStepSaving:
		b.WriteString("Saving — rewriting the archive with the chapters…\n")
		b.WriteString(DimStyle.Render("Esc: Close (the save keeps running)"))
	default:
		b.WriteString(DimStyle.Render("Tab: Paste/Offset  Ctrl+S: Save  Esc: Cancel"))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH).
		Render(b.String())
	return centerBox(box, m.width, m.height)
}

// renderRows lists chapters "start  title", at most limit rows (the rest
// counted), each cut to the box.
func (m *ChapterImportDialogModel) renderRows(rows []ChapterImportRow, limit, width int) string {
	var b strings.Builder
	for i, r := range rows {
		if i == limit-1 && len(rows) > limit {
			b.WriteString(DimStyle.Render(fmt.Sprintf("  … and %d more", len(rows)-i)))
			b.WriteString("\n")
			break
		}
		line := fmt.Sprintf("  %8s  %s", chapterClock(r.Start), r.Title)
		b.WriteString(truncateString(line, max(width, 10)))
		b.WriteString("\n")
	}
	return b.String()
}

// chapterImportable is A H's and A U's job rule: a job that is not active and
// has an archive (owner decision CI-1) — Finished, or an Error or Cancelled
// row that names a file or has parts. routes.ChapterService re-checks it, and
// that the file is there, before it writes anything.
func chapterImportable(j *database.Job) bool {
	switch j.Status {
	case database.StatusFinished, database.StatusError, database.StatusCancelled:
		return j.OutputFile != "" || j.Filename != "" || len(j.Segments) > 0
	}
	return false
}

// previewChaptersCmd asks OnPreviewChapters for the paste as it stands, for
// edit seq, off the update goroutine: the preview probes the archive.
func (a *App) previewChaptersCmd(seq int) tea.Cmd {
	fn := a.OnPreviewChapters
	jobID, text, offset := a.chapterDlg.Request()
	return safeCmdOr(func() tea.Msg {
		if fn == nil {
			return chapterPreviewResultMsg{Seq: seq, Err: fmt.Errorf("chapter import is not available in this process")}
		}
		v, err := fn(jobID, text, offset)
		return chapterPreviewResultMsg{Seq: seq, View: v, Err: err}
	}, func(text string) tea.Msg { return chapterPreviewResultMsg{Seq: seq, Err: fmt.Errorf("%s", text)} })
}

// saveChaptersCmd runs OnSaveChapters for the overlay's paste: the sidecar,
// then the archive's re-mux, which takes as long as a copy of the file.
func (a *App) saveChaptersCmd() tea.Cmd {
	fn := a.OnSaveChapters
	jobID, text, offset := a.chapterDlg.Request()
	title := a.chapterDlg.title
	return safeCmdOr(func() tea.Msg {
		if fn == nil {
			return chapterSaveResultMsg{Title: title, Err: fmt.Errorf("chapter import is not available in this process")}
		}
		v, err := fn(jobID, text, offset)
		return chapterSaveResultMsg{Title: title, View: v, Err: err}
	}, func(text string) tea.Msg { return chapterSaveResultMsg{Title: title, Err: fmt.Errorf("%s", text)} })
}

// restoreChaptersCmd runs OnRestoreChapters for job (A U).
func (a *App) restoreChaptersCmd(job *database.Job) tea.Cmd {
	fn := a.OnRestoreChapters
	id, title := job.ID, job.Title
	return safeCmdOr(func() tea.Msg {
		v, err := fn(id)
		return chapterSaveResultMsg{Title: title, Restore: true, View: v, Err: err}
	}, func(text string) tea.Msg { return chapterSaveResultMsg{Title: title, Restore: true, Err: fmt.Errorf("%s", text)} })
}

// handleChapterSaveResult reports a save or a restore. A refused save goes
// back into the overlay that started it, which keeps the paste; anything
// else — a success, or a result whose overlay was closed with Esc while it
// ran — is the feedback line's. A save the re-mux could not embed is still a
// save (the list is kept beside the archive, which the player reads), and
// the line says so as a warning.
func (a *App) handleChapterSaveResult(msg chapterSaveResultMsg) (tea.Model, tea.Cmd) {
	verb := "Imported"
	if msg.Restore {
		verb = "Restored the original"
	}
	if msg.Err != nil {
		if !msg.Restore && a.chapterDlg.IsSaving() {
			a.chapterDlg.SetSaveError(msg.Err)
			return a, nil
		}
		a.setFeedbackWithSeverity(fmt.Sprintf("Chapters not saved (%s): %s", msg.Title, msg.Err), severityWarning)
		return a, nil
	}
	if !msg.Restore && a.chapterDlg.IsSaving() {
		a.chapterDlg.Close()
	}
	line := fmt.Sprintf("%s %d chapters: %s", verb, len(msg.View.Chapters), msg.Title)
	switch {
	case msg.View.EmbedError != "":
		a.setFeedbackWithSeverity(line+" — saved beside the file, which could not be rewritten: "+msg.View.EmbedError, severityWarning)
	case msg.View.Split:
		a.setFeedback(line + " (saved beside the parts)")
	default:
		a.setFeedback(line)
	}
	return a, nil
}

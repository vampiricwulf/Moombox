package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The bare form of Add Video, drawn below the normal layout
// (overlay_bare.go): one sheet a step, a title row, the body, the keys.
//
//	URL      the prompt, the input, the Advanced checkbox. An error is under
//	         them, and where the terminal is too short for all of it the
//	         title, then the prompt, then the checkbox give way. Never the
//	         input.
//	formats  the renditions, one a row, drawn by hand (the table cannot be
//	         drawn in 30 columns): its number, the columns, and the marks.
//	         "*" is a Best pick and "!" a damaged or segmented rendition, on
//	         EVERY row that has one: a digit key takes a row the cursor is
//	         not on. Under the rows, the highlighted row's note in words, or
//	         what the marks mean while the table is on Auto.
//	times    the two fields, then the error or the format hint
//	confirm  the video's ID in the title; the video pick, the audio pick and
//	         the time range, always; the title where a row is left
//
// The format tables keep their own size: nothing here sets a column, a width
// or a height on them, so the normal form finds them as it left them. The
// window of rows is derived from the cursor on every draw, and PgUp/PgDn
// move by the rows that are drawn (barePage).

// The marks of a bare format row.
const (
	bareFormatBest   = "*"
	bareFormatMarked = "!"
)

// bareFormatLegend says what the marks mean. 27 cells: whole at the compact
// floor.
const bareFormatLegend = "* Best  ! damaged/segmented"

// bareNoneNone is the confirm step's one error in the words that fit a row
// of the compact floor.
const bareNoneNone = "Video and audio are both None"

// bare reports whether the dialog draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *AddVideoModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareLeave gives the text input back the width the normal URL step draws
// it with, and lays its value out for it.
func (m *AddVideoModel) bareLeave() {
	bareInputWidth(&m.textInput, max(min(80, m.width-4), 30)-4)
}

// bareFormatTable is the format step's table, nil off that step or before
// the formats arrived.
func (m *AddVideoModel) bareFormatTable() (t *table.Model, picking bool) {
	if m.loading || m.formats == nil {
		return nil, false
	}
	switch m.step {
	case AddStepVideoFormat:
		return &m.videoTable, m.videoPicking
	case AddStepAudioFormat:
		return &m.audioTable, m.audioPicking
	}
	return nil, false
}

// bareFormatRows is how many renditions the format step draws: every row but
// the title, the note and the keys.
func (m *AddVideoModel) bareFormatRows() int {
	return max(m.height-3, 1)
}

// barePage is PgUp, PgDn and the half-page keys on the bare format step:
// the cursor moves by the rows that are drawn, where the table would move
// it by its own height. It reports whether msg was one of them.
func (m *AddVideoModel) barePage(msg tea.Msg) bool {
	kp, ok := msg.(tea.KeyPressMsg)
	t, _ := m.bareFormatTable()
	if !ok || t == nil || !m.bare() {
		return false
	}
	rows := m.bareFormatRows()
	switch km := t.KeyMap; {
	case key.Matches(kp, km.PageUp):
		t.MoveUp(rows)
	case key.Matches(kp, km.PageDown):
		t.MoveDown(rows)
	case key.Matches(kp, km.HalfPageUp):
		t.MoveUp(max(rows/2, 1))
	case key.Matches(kp, km.HalfPageDown):
		t.MoveDown(max(rows/2, 1))
	default:
		return false
	}
	return true
}

// bareNote is a format row's note in w cells: as the table writes it, and
// with the two marks shortened where that does not fit.
func bareNote(note string, w int) string {
	if ansi.StringWidth(note) > w {
		note = strings.NewReplacer("damaged", "dmg", "segmented", "seg").Replace(note)
	}
	return truncateString(note, w)
}

// bareFormatLines draws rows [top, top+n) of a format table in w cells. A
// row is its cells but the note, each column as wide as its widest cell,
// then the marks. The bitrate column (the last before the note in the video
// table, the first after the number in the audio one) is left out of every
// row when the rows do not fit with it; the marks are never left out.
func bareFormatLines(t *table.Model, picking bool, top, n, w int) []string {
	rows := t.Rows()
	if len(rows) == 0 {
		return nil
	}
	cols := len(rows[0]) - 1 // without the note
	cell := func(r table.Row, c int) string { return strings.TrimSuffix(r[c], "bps") }
	widths := make([]int, cols)
	for _, r := range rows {
		for c := range cols {
			widths[c] = max(widths[c], ansi.StringWidth(cell(r, c)))
		}
	}
	bitrate := -1
	for c, col := range t.Columns()[:cols] {
		if col.Title == "Bitrate" {
			bitrate = c
		}
	}
	total := 2 + 2 // the cursor mark, and the marks with their space
	for _, cw := range widths {
		total += cw + 1
	}
	drop := -1
	if total > w {
		drop = bitrate
	}

	selected := lipgloss.NewStyle().Bold(true).Foreground(ColorWhite).Background(lipgloss.Color("#333355"))
	out := make([]string, 0, n)
	for i := top; i < len(rows) && i < top+n; i++ {
		r := rows[i]
		var sb strings.Builder
		on := picking && i == t.Cursor()
		if on {
			sb.WriteString("> ")
		} else {
			sb.WriteString("  ")
		}
		for c := range cols {
			if c == drop {
				continue
			}
			text := cell(r, c)
			sb.WriteString(text)
			writeSpaces(&sb, widths[c]-ansi.StringWidth(text)+1)
		}
		note := r[cols]
		marks := ""
		if strings.Contains(note, "Best") {
			marks += bareFormatBest
		}
		if strings.Contains(note, "damaged") || strings.Contains(note, "segmented") {
			marks += bareFormatMarked
		}
		// The marks are the last thing cut: the columns give way first.
		line := truncateWidth(sb.String(), max(w-len(marks), 0), "") + marks
		switch {
		case on:
			line = selected.Render(line)
		case strings.Contains(marks, bareFormatMarked):
			line = YellowStyle.Render(line)
		}
		out = append(out, line)
	}
	return out
}

// bareView draws the bare form: exactly width x height cells.
func (m *AddVideoModel) bareView() string {
	switch m.step {
	case AddStepVideoFormat, AddStepAudioFormat:
		return m.bareFormatView()
	case AddStepTimestamps:
		return m.bareTimesView()
	case AddStepConfirm:
		return m.bareConfirmView()
	}
	return m.bareURLView()
}

// bareKeys is the keys row: the submit's spinner while one is in flight.
func (m *AddVideoModel) bareKeys(full, short string) string {
	if m.submitting {
		return truncateWidth(m.submittingLine(), m.width, "")
	}
	if ansi.StringWidth(full) > m.width {
		full = short
	}
	return DimStyle.Render(full)
}

func (m *AddVideoModel) bareURLView() string {
	w, h := m.width, m.height
	accent := ColorCyan
	if m.advancedEnabled {
		accent = ColorCookies
	}
	// As the normal form does on every draw: the input in the mode's colour.
	st := m.textInput.Styles()
	st.Focused.Text = lipgloss.NewStyle().Foreground(accent)
	st.Blurred.Text = lipgloss.NewStyle().Foreground(accent)
	st.Cursor.Color = accent
	m.textInput.SetStyles(st)
	if want := max(w-3, 1); m.textInput.Width() != want {
		bareInputWidth(&m.textInput, want)
	}
	input := lipgloss.NewStyle().Foreground(accent).Render("> ") + m.textInput.View()

	title := TitleStyle.Render("Add Video")
	prompt := "URL, video ID or Twitch channel:"
	if len(prompt) > w {
		prompt = "URL, ID or Twitch channel:"
	}
	check, checkStyle := "[ ]", lipgloss.NewStyle()
	if m.advancedEnabled {
		check, checkStyle = "[✓]", lipgloss.NewStyle().Foreground(ColorCookies)
	}
	advanced := check + " Advanced: formats, time range (Tab)"
	if ansi.StringWidth(advanced) > w {
		advanced = check + " Advanced (Tab)"
	}
	advanced = checkStyle.Render(advanced)

	// The input and the error come first; the checkbox, the prompt and the
	// title take the rows that are left, in that order.
	room := h - 2 // every row but the keys and the input
	var errRows []string
	if m.errorMsg != "" {
		errRows = bareStyled(bareWrap(m.errorMsg, w, max(room, 1)), ErrorStyle)
	}
	room -= len(errRows)
	var rows []string
	if room >= 3 {
		rows = append(rows, title)
	}
	if room >= 2 {
		rows = append(rows, prompt)
	}
	rows = append(rows, input)
	if room >= 1 {
		rows = append(rows, advanced)
	}
	rows = append(rows, errRows...)

	sheet := bareSheet{left: rows[0], body: rows[1:]}
	if m.advancedEnabled {
		sheet.keys = m.bareKeys("Enter: Next  Tab: Advanced  Esc: Cancel", "Enter: Next  Esc: Cancel")
	} else {
		sheet.keys = m.bareKeys("Enter: Add  Tab: Advanced  Esc: Cancel", "Enter: Add  Esc: Cancel")
	}
	return sheet.render(w, h)
}

func (m *AddVideoModel) bareFormatView() string {
	w, h := m.width, m.height
	name, step := "Video", "1/4"
	if m.step == AddStepAudioFormat {
		name, step = "Audio", "2/4"
	}
	sheet := bareSheet{left: TitleStyle.Render(name) + " " + DimStyle.Render(step)}
	if m.loading {
		sheet.body = []string{m.spinner.View() + " Fetching formats..."}
		sheet.keys = DimStyle.Render("Esc: Back")
		return sheet.render(w, h)
	}
	t, picking := m.bareFormatTable()
	if t == nil {
		// The fetch failed: its error, and the two picks that need no list.
		sheet.keys = DimStyle.Render("A:Auto N:None Esc:Back")
		sheet.errorUnder(m.errorMsg, YellowStyle, w, h)
		return sheet.render(w, h)
	}

	total := len(t.Rows())
	rows := m.bareFormatRows()
	top := min(max(t.Cursor()-rows/2, 0), max(total-rows, 0))
	sheet.body = bareRows(bareFormatLines(t, picking, top, rows, w), rows)

	// The row under the list: the fetch's error first, then the highlighted
	// row's note in words, then what the marks mean.
	note := DimStyle.Render(truncateString(bareFormatLegend, w))
	if picking {
		sheet.right = DimStyle.Render(fmt.Sprintf("%d/%d", t.Cursor()+1, total))
		if sel := t.SelectedRow(); sel != nil && sel[len(sel)-1] != "" {
			note = bareNote(sel[len(sel)-1], w)
		}
		sheet.keys = m.bareKeys("Enter:Pick A:Auto N:None 1-9 Esc:Back", "Ent:Pick A:Auto N:None 1-9 Esc")
	} else {
		sheet.right = DimStyle.Render("Enter: Auto")
		sheet.keys = m.bareKeys("Enter/A:Auto ↑↓:Pick N:None 1-9 Esc", "Ent/A:Auto ↑↓ N:None 1-9 Esc")
	}
	if m.errorMsg != "" {
		note = YellowStyle.Render(truncateString(m.errorMsg, w))
	}
	sheet.body = append(sheet.body, note)
	return sheet.render(w, h)
}

func (m *AddVideoModel) bareTimesView() string {
	w, h := m.width, m.height
	sheet := bareSheet{left: TitleStyle.Render("Times") + " " + DimStyle.Render("3/4 (blank = all)")}
	sheet.keys = m.bareKeys("Tab: Field  Enter: Next  Esc: Back", "Tab Enter:Next Esc:Back")
	sheet.body = renderTimeInputPair(m.startTimeInput, m.endTimeInput, m.timeInputFocus, m.textInput, w, ColorCookies)
	if m.errorMsg != "" {
		sheet.errorUnder(m.errorMsg, ErrorStyle, w, h)
	} else {
		sheet.noteUnder("H:MM:SS, MM:SS or seconds", DimStyle, w, h)
	}
	return sheet.render(w, h)
}

// bareVideoPick and bareAudioPick word the two picks for the confirm.
func (m *AddVideoModel) bareVideoPick() string {
	switch itag := m.selectedVideoItag; {
	case itag == nil:
		return "Auto (best)"
	case *itag == -1:
		return "None (audio only)"
	default:
		if m.formats != nil {
			for _, f := range m.formats.VideoFormats {
				if f.Itag != *itag {
					continue
				}
				label := fmt.Sprintf("%dx%d", f.Width, f.Height)
				if f.Fps > 0 {
					label += fmt.Sprintf("@%d", f.Fps)
				}
				if strings.Contains(strings.ToLower(f.MimeType), "webm") {
					return label + " WEBM"
				}
				return label + " MP4"
			}
		}
		return fmt.Sprintf("itag %d", *itag)
	}
}

func (m *AddVideoModel) bareAudioPick() string {
	switch itag := m.selectedAudioItag; {
	case itag == nil:
		return "Auto (best)"
	case *itag == -1:
		return "None (video only)"
	default:
		if m.formats != nil {
			for _, f := range m.formats.AudioFormats {
				if f.Itag != *itag {
					continue
				}
				// The codec as the normal confirm names it.
				codec := "AAC"
				if mime := strings.ToLower(f.MimeType); strings.Contains(mime, "opus") || strings.Contains(mime, "webm") {
					codec = "OPUS"
				}
				return fmt.Sprintf("%dkbps %s", f.Bitrate/1000, codec)
			}
		}
		return fmt.Sprintf("itag %d", *itag)
	}
}

func (m *AddVideoModel) bareConfirmView() string {
	w, h := m.width, m.height
	sheet := bareSheet{left: TitleStyle.Render("Add") + " " + DimStyle.Render("4/4") + "  " + m.videoID}
	sheet.keys = m.bareKeys("Enter: Add  Esc: Back", "Enter: Add  Esc: Back")
	sheet.body = []string{
		truncateString("V: "+m.bareVideoPick(), w),
		truncateString("A: "+m.bareAudioPick(), w),
	}
	if m.startTimeInput != "" || m.endTimeInput != "" {
		start, end := "0:00", "(end)"
		if m.startTimeInput != "" {
			start = m.startTimeInput
		}
		if m.endTimeInput != "" {
			end = m.endTimeInput
		}
		sheet.body = append(sheet.body, truncateString("T: "+start+" - "+end, w))
	}
	errText := m.errorMsg
	if errText == "Cannot select None for both video and audio" && len(errText) > w {
		errText = bareNoneNone
	}
	errRows := 0
	if errText != "" {
		errRows = len(bareWrap(errText, w, len(errText)+1))
	}
	// The video's title is the first thing to give way.
	if m.formats != nil && m.formats.Title != "" && h-2-len(sheet.body)-errRows >= 1 {
		sheet.body = append([]string{DimStyle.Render(truncateString(m.formats.Title, w))}, sheet.body...)
	}
	sheet.errorUnder(errText, ErrorStyle, w, h)
	return sheet.render(w, h)
}

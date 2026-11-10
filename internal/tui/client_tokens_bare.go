package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The bare form of the client tokens dialog, drawn below the normal layout
// (overlay_bare.go). A fixed frame, so nothing can push the keys off:
//
//	row 0        the title and the count
//	rows 1..h-3  the list: a cursor mark, the label, when it was last used
//	row h-2      the selected token's last address, or the armed revoke
//	row h-1      the keys
//
// The armed revoke is shortened to `D again: revoke "label"` (owner ruling
// R-C11): the marked, yellow row it is about stays on screen. It is derived
// from revokeConfirmID on every draw, never from the worded feedbackMsg. An
// error is wrapped over the list rows. The dialog shows labels, times and
// addresses only, never a token.

// bare reports whether the dialog draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *ClientTokensDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareListRows is how many rows the list gets in the bare form.
func (m *ClientTokensDialogModel) bareListRows() int {
	return max(m.height-3, 1)
}

// tokenLastUsed is when a token was last used, as the list words it.
func tokenLastUsed(ct *database.ClientToken) string {
	if ct.LastUsedAt != "" {
		if t, err := time.Parse(time.RFC3339, ct.LastUsedAt); err == nil {
			return relativeTime(t)
		}
	}
	return "never"
}

// bareRenderItem draws one row of the bare list: the cursor mark, the label
// cut to what is left, and the last use against the right edge.
func (m *ClientTokensDialogModel) bareRenderItem(w io.Writer, l list.Model, index int, ct *database.ClientToken) {
	width := l.Width()
	lastUsed := tokenLastUsed(ct)
	mark := " "
	if index == l.Index() {
		mark = ">"
	}
	label := truncateString(ct.Label, max(width-2-lipgloss.Width(lastUsed), 1))
	gap := strings.Repeat(" ", max(width-1-lipgloss.Width(label)-lipgloss.Width(lastUsed), 1))

	switch {
	case m.revokeConfirmID == ct.ID:
		fmt.Fprint(w, YellowStyle.Render(mark+label+gap+lastUsed))
	case index == l.Index():
		fmt.Fprint(w, lipgloss.NewStyle().Foreground(ColorCyan).Render(mark+label+gap+lastUsed))
	default:
		fmt.Fprint(w, mark+label+gap+DimStyle.Render(lastUsed))
	}
}

// bareDetail is the row under the list: the armed revoke, else the result of
// the last one, else the selected token's last address.
func (m *ClientTokensDialogModel) bareDetail() string {
	sel := m.SelectedToken()
	switch {
	case sel != nil && m.revokeConfirmID == sel.ID:
		const head, tail = `D again: revoke "`, `"`
		label := truncateString(sel.Label, max(m.width-len(head)-len(tail), 1))
		return YellowStyle.Render(truncateString(head+label+tail, m.width))
	case m.feedbackMsg != "" && !strings.HasPrefix(m.feedbackMsg, "Press "):
		return YellowStyle.Render(truncateString(m.feedbackMsg, m.width))
	case sel != nil && sel.LastIP != "":
		return DimStyle.Render(bareMiddleCut(sel.LastIP, m.width))
	}
	return ""
}

// bareView draws the bare form: exactly width x height cells.
func (m *ClientTokensDialogModel) bareView() string {
	count := len(m.list.Items())
	sheet := bareSheet{
		left: TitleStyle.Render("Tokens") + " " + DimStyle.Render(fmt.Sprintf("(%d)", count)),
		keys: DimStyle.Render("D revoke  R refresh  Esc"),
	}
	switch {
	case m.loading:
		sheet.body = []string{m.spinner.View() + " Loading..."}
	case m.errorMsg != "":
		sheet.keys = DimStyle.Render("R refresh  Esc")
		sheet.setError(m.errorMsg, m.width, m.height)
	default:
		var rows []string
		if count == 0 {
			rows = []string{DimStyle.Render("No client tokens.")}
			sheet.keys = DimStyle.Render("R refresh  Esc")
		} else {
			rows = strings.Split(m.list.View(), "\n")
		}
		sheet.body = append(bareRows(rows, m.bareListRows()), m.bareDetail())
	}
	return sheet.render(m.width, m.height)
}

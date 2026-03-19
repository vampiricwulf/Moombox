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

	"github.com/vampiricwulf/Moombox/internal/database"
)

// clientTokenItem wraps a ClientToken as a list.Item.
type clientTokenItem struct {
	token *database.ClientToken
}

func (c clientTokenItem) FilterValue() string { return c.token.Label }

// clientTokenDelegate renders token list items.
type clientTokenDelegate struct {
	dialog *ClientTokensDialogModel
}

func (d clientTokenDelegate) Height() int                             { return 1 }
func (d clientTokenDelegate) Spacing() int                            { return 0 }
func (d clientTokenDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d clientTokenDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	ci, ok := item.(clientTokenItem)
	if !ok {
		return
	}
	ct := ci.token

	prefix := "  "
	if index == m.Index() {
		prefix = "▸ "
	}

	// Format last used as relative time
	lastUsed := "never"
	if ct.LastUsedAt != "" {
		if t, err := time.Parse(time.RFC3339, ct.LastUsedAt); err == nil {
			lastUsed = relativeTime(t)
		}
	}

	ipStr := ct.LastIP
	if ipStr == "" {
		ipStr = "—"
	}

	// Compute label width from actual suffix widths (truncate plain text
	// BEFORE assembling with styled parts — truncateString uses runewidth
	// which miscounts ANSI escape sequences from DimStyle.Render).
	suffixW := 2 + lipgloss.Width(lastUsed) + 2 + lipgloss.Width(ipStr) // "  " + lastUsed + "  " + ipStr
	maxLabelW := max(m.Width()-2-suffixW, 10) // 2 for prefix
	label := truncateString(ct.Label, maxLabelW)

	var style lipgloss.Style
	highlighted := false
	if d.dialog != nil && d.dialog.revokeConfirmID == ct.ID {
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))
		highlighted = true
	} else if index == m.Index() {
		style = lipgloss.NewStyle().Foreground(ColorCyan)
		highlighted = true
	} else {
		style = lipgloss.NewStyle()
	}

	// Use the highlight style for metadata when selected/confirm, otherwise DimStyle
	metaStyle := DimStyle
	if highlighted {
		metaStyle = style
	}

	line := fmt.Sprintf("%s%s  %s  %s", prefix, label, metaStyle.Render(lastUsed), metaStyle.Render(ipStr))
	fmt.Fprint(w, style.Render(line))
}

// ClientTokensDialogModel manages the client tokens dialog.
type ClientTokensDialogModel struct {
	visible         bool
	width, height   int
	list            list.Model
	revokeConfirmID string
	confirmTimer    time.Time
	loading         bool
	errorMsg        string
	feedbackMsg     string
	spinner         spinner.Model
}

// NewClientTokensDialogModel creates a new client tokens dialog.
func NewClientTokensDialogModel() *ClientTokensDialogModel {
	m := &ClientTokensDialogModel{
		spinner: newSpinner(),
	}
	m.list = m.newTokenList()
	return m
}

func (m *ClientTokensDialogModel) newTokenList() list.Model {
	delegate := clientTokenDelegate{dialog: m}
	l := list.New(nil, delegate, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowFilter(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	l.InfiniteScrolling = true

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
func (m *ClientTokensDialogModel) Open() {
	m.visible = true
	m.loading = true
	m.spinner = newSpinner()
	m.list.SetItems(nil)
	m.revokeConfirmID = ""
	m.confirmTimer = time.Time{}
	m.errorMsg = ""
	m.feedbackMsg = ""
}

// Close hides the dialog.
func (m *ClientTokensDialogModel) Close() {
	m.visible = false
}

// IsVisible returns true if the dialog is shown.
func (m *ClientTokensDialogModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the dialog dimensions.
func (m *ClientTokensDialogModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	boxW := max(min(80, w-4), 40)
	boxH := max(min(24, h-4), 10)
	listH := max(boxH-6, 1)
	m.list.SetSize(boxW-2, listH)
}

// SetTokens populates the token list after async fetch.
func (m *ClientTokensDialogModel) SetTokens(tokens []*database.ClientToken) tea.Cmd {
	m.loading = false
	m.errorMsg = ""
	items := make([]list.Item, len(tokens))
	for i, ct := range tokens {
		items[i] = clientTokenItem{token: ct}
	}
	return m.list.SetItems(items)
}

// SetError sets an error message.
func (m *ClientTokensDialogModel) SetError(msg string) {
	m.errorMsg = msg
	m.loading = false
}

// SelectedToken returns the currently selected token entry.
func (m *ClientTokensDialogModel) SelectedToken() *database.ClientToken {
	sel := m.list.SelectedItem()
	if sel == nil {
		return nil
	}
	ci, ok := sel.(clientTokenItem)
	if !ok {
		return nil
	}
	return ci.token
}

// RemoveToken removes a token by ID from the list.
func (m *ClientTokensDialogModel) RemoveToken(id string) {
	for i, item := range m.list.Items() {
		ci, ok := item.(clientTokenItem)
		if ok && ci.token.ID == id {
			m.list.RemoveItem(i)
			m.revokeConfirmID = ""
			break
		}
	}
}

// UpdateComponents routes tea.Msg to the spinner when loading.
func (m *ClientTokensDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
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
func (m *ClientTokensDialogModel) SpinnerInit() tea.Cmd {
	return func() tea.Msg { return m.spinner.Tick() }
}

// HandleKey processes key input. Returns action string and optional command.
func (m *ClientTokensDialogModel) HandleKey(msg tea.KeyPressMsg) (string, tea.Cmd) {
	// Confirmation timeout
	if m.revokeConfirmID != "" && !m.confirmTimer.IsZero() && time.Now().After(m.confirmTimer) {
		m.revokeConfirmID = ""
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
		sel := m.SelectedToken()
		if sel == nil {
			return "", nil
		}
		if m.revokeConfirmID == sel.ID && !m.confirmTimer.IsZero() && time.Now().Before(m.confirmTimer) {
			m.revokeConfirmID = ""
			m.confirmTimer = time.Time{}
			return "revoke", nil
		}
		m.revokeConfirmID = sel.ID
		m.confirmTimer = time.Now().Add(3 * time.Second)
		m.feedbackMsg = "Press D again to revoke this token"
		return "", nil
	}

	// Reset confirm on navigation
	if key == keyUp || key == keyDown {
		m.revokeConfirmID = ""
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

// View renders the client tokens dialog overlay.
func (m *ClientTokensDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := max(min(80, m.width-4), 40)
	boxH := max(min(24, m.height-4), 10)

	tokenCount := len(m.list.Items())

	var lines []string

	lines = append(lines, TitleStyle.Render("Connected Clients")+" "+DimStyle.Render(fmt.Sprintf("(%d tokens)", tokenCount)))
	lines = append(lines, "")

	if m.loading {
		lines = append(lines, "  "+m.spinner.View()+" Loading...")
	} else if m.errorMsg != "" {
		lines = append(lines, ErrorStyle.Render("  "+m.errorMsg))
	} else if tokenCount == 0 {
		lines = append(lines, DimStyle.Render("  No client tokens."))
	} else {
		lines = append(lines, m.list.View())
	}

	if m.feedbackMsg != "" {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("  "+m.feedbackMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("↑↓: Navigate | D: Revoke | R: Refresh | Esc: Close"))

	content := strings.Join(lines, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
}

// relativeTime formats a time as a short relative duration.
func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

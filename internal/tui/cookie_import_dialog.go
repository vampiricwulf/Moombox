package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The three steps of the E I overlay.
const (
	cookieImportStepPath    = iota // typing the path
	cookieImportStepRunning        // App.OnImportCookieFile is in flight
	cookieImportStepResult         // the per-platform outcome is on screen
)

// CookieImportDialogModel is the E I overlay: a path prompt, then the import
// runs through App.OnImportCookieFile, then the per-platform outcome is shown
// in place.
//
// IT NEVER SEES COOKIE CONTENT. The path is the only thing typed, the only
// thing stored on this model and the only thing this package ever hands to a
// callback; the file is read in cmd/moombox, one call deep, and what comes
// back is cookies.ImportResult — four enum values and three bools. That is
// deliberate and is the whole reason the dialog does not own the file read: a
// model that held the bytes would render them the first time someone added a
// "preview" step, and a credential on a terminal is a credential in the
// operator's scrollback.
//
// The result is worded off ImportOutcome AND the verdict, never flattened to
// pass/fail — see platformOutcomeLine for why either one alone is a lie.
type CookieImportDialogModel struct {
	visible       bool
	width, height int
	step          int
	input         textinput.Model
	errorMsg      string
	spinner       spinner.Model
	result        cookies.ImportResult
	resultErr     error
}

// NewCookieImportDialogModel creates the import-cookie-file overlay.
func NewCookieImportDialogModel() *CookieImportDialogModel {
	ti := newTextInput()
	ti.Prompt = "> "
	ti.Placeholder = "~/Downloads/cookies.txt"
	return &CookieImportDialogModel{input: ti, spinner: newSpinner()}
}

// IsVisible returns true if the dialog is shown.
func (m *CookieImportDialogModel) IsVisible() bool { return m.visible }

// IsImporting reports whether the overlay is on the spinner step, waiting for
// the import IT started.
//
// Narrower than IsVisible on purpose: Esc during a slow import and then E I
// again leaves a VISIBLE dialog that is a fresh path prompt, and the first
// import's result must not overwrite it with an outcome for a different file.
func (m *CookieImportDialogModel) IsImporting() bool {
	return m.visible && m.step == cookieImportStepRunning
}

// SetSize updates the dialog dimensions.
func (m *CookieImportDialogModel) SetSize(w, h int) {
	m.width, m.height = w, h
	if _, contentW := dialogBox(70, w); contentW > 0 {
		m.input.SetWidth(max(contentW-4, 10))
	}
}

// Open resets to the path prompt and shows the dialog.
func (m *CookieImportDialogModel) Open() tea.Cmd {
	m.visible = true
	m.step = cookieImportStepPath
	m.errorMsg = ""
	m.result = cookies.ImportResult{}
	m.resultErr = nil
	m.spinner = newSpinner()
	m.input.SetValue("")
	return m.input.Focus()
}

// Close hides the dialog.
func (m *CookieImportDialogModel) Close() {
	m.visible = false
	m.input.Blur()
}

// SetImporting moves to the spinner step. The App fires the command; this only
// says what the overlay shows while it runs.
func (m *CookieImportDialogModel) SetImporting() {
	m.step = cookieImportStepRunning
	m.errorMsg = ""
}

// SpinnerInit returns the spinner's initial tick command.
func (m *CookieImportDialogModel) SpinnerInit() tea.Cmd { return spinnerTickCmd(m.spinner) }

// SetResult renders the outcome (or the error) in place; Esc closes.
func (m *CookieImportDialogModel) SetResult(r cookies.ImportResult, err error) {
	m.step = cookieImportStepResult
	m.result, m.resultErr = r, err
}

// expandHome turns a leading "~" — alone, "~/…" or the backslash form — into
// the home directory; anything else is returned unchanged, including the
// "~user" form, which Go cannot resolve and which must not be silently
// rewritten into a path under the wrong account.
//
// Nothing else in the tree expands "~" and bubbles' filepicker does not
// either, but this is the one prompt where an operator types a path by hand
// rather than browsing to it, and "~/Downloads/cookies.txt" is exactly what a
// browser's export leaves behind.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, "~\\") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// HandleKey processes one keypress. It returns ("import", path) when a valid
// file was confirmed, ("close", "") when the overlay closed, and ("", "")
// otherwise.
//
// The existence check happens HERE rather than inside the command, so a typo
// is answered inline over the still-filled prompt instead of round-tripping
// through the callback and coming back as a failed import.
//
// IT CANNOT TOUCH THE TEXTINPUT, and that is structural rather than a rule to
// remember: it is handed the derived key STRING, exactly as ImportDialogModel
// is (import_dialog.go), so there is no KeyPressMsg here to feed a component
// with. App.Update's keypress arm calls routeComponentMsg AND handleKey for
// the same message, so the input is fed once already — by UpdateComponents,
// which runs FIRST. Feeding it a second time would insert every rune twice
// ("abc" → "aabbcc") and make one Backspace eat two characters, and no test
// that calls HandleKey directly could see it. Pinned by
// TestCookieImportTypingIsNotDoubled, which drives App.Update.
//
// Because UpdateComponents ran first, m.input.Value() on Enter already
// includes everything typed up to and including the keypress before it.
func (m *CookieImportDialogModel) HandleKey(key string) (string, string) {
	if key == keyEsc || (m.step == cookieImportStepResult && (key == "q" || key == keyEnter)) {
		m.Close()
		return "close", ""
	}
	if m.step == cookieImportStepPath && key == keyEnter {
		return m.confirmPath()
	}
	return "", ""
}

// confirmPath validates what was typed and either returns the import action or
// leaves an inline message over the unchanged prompt.
//
// The path is put on its own line rather than interpolated into the sentence:
// a Windows profile path is most of a dialog wide on its own, and the phrase
// the operator has to read must not be what gets word-wrapped away.
func (m *CookieImportDialogModel) confirmPath() (string, string) {
	path := expandHome(strings.TrimSpace(m.input.Value()))
	if path == "" {
		m.errorMsg = "Enter a path to a cookies.txt file"
		return "", ""
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		// The stat error itself is not quoted — it repeats the path a second
		// time and, on Windows, in a sentence of its own.
		m.errorMsg = "That path does not exist:\n  " + path
	case info.IsDir():
		m.errorMsg = "That path is a directory, not a file:\n  " + path
	default:
		m.errorMsg = ""
		return "import", path
	}
	return "", ""
}

// UpdateComponents routes ticks to whichever component the current step owns.
func (m *CookieImportDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	var cmd tea.Cmd
	if m.step == cookieImportStepRunning {
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// View renders the overlay.
func (m *CookieImportDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW, _ := dialogBox(70, m.width)
	boxH := max(min(m.height-4, 20), 10)

	var b strings.Builder
	b.WriteString(TitleStyle.Render("Import Cookie File"))
	b.WriteString("\n")

	switch m.step {
	case cookieImportStepPath:
		b.WriteString(DimStyle.Render("Path to a Netscape-format cookies.txt exported from your browser"))
		b.WriteString("\n\n")
		b.WriteString(m.input.View())
		b.WriteString("\n")
		if m.errorMsg != "" {
			b.WriteString("\n")
			b.WriteString(ErrorStyle.Render(m.errorMsg))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(DimStyle.Render("Enter: Import  Esc: Cancel"))

	case cookieImportStepRunning:
		b.WriteString("\n")
		b.WriteString(m.spinner.View())
		b.WriteString(" Importing, then verifying each platform...\n")
		b.WriteString("\n")
		b.WriteString(DimStyle.Render("Esc: Close (the import keeps running)"))

	case cookieImportStepResult:
		b.WriteString("\n")
		if m.resultErr != nil {
			b.WriteString(ErrorStyle.Render("Import failed"))
			b.WriteString("\n")
			b.WriteString("  ")
			b.WriteString(m.resultErr.Error())
			b.WriteString("\n")
		} else {
			b.WriteString(platformOutcomeLine("YouTube", m.result.YouTubeOutcome, m.result.YouTube, m.result.YouTubeAccepted))
			b.WriteString("\n")
			b.WriteString(platformOutcomeLine("Twitch", m.result.TwitchOutcome, m.result.Twitch, m.result.TwitchAccepted))
			b.WriteString("\n")
			// ON THE OUTCOME, never on RollbackProtected. That flag says a
			// rollback was POSSIBLE — a pre-write snapshot succeeded over an
			// existing cookies.txt — which is true of virtually every import
			// on an install that already has credentials. Gated on it, this
			// past-tense sentence followed two clean "imported (authenticates)"
			// lines and told the operator a platform had stopped
			// authenticating. The Web keeps RollbackProtected off the wire
			// entirely and words its rollback toast off the outcome string
			// alone; this is the same rule.
			if m.result.YouTubeOutcome == cookies.ImportRolledBack || m.result.TwitchOutcome == cookies.ImportRolledBack {
				b.WriteString("\n")
				b.WriteString(DimStyle.Render("A platform that stopped authenticating kept its previous cookies (rolled back)."))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
		b.WriteString(DimStyle.Render("Esc/Enter: Close"))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH).
		Render(b.String())

	return centerBox(box, m.width, m.height)
}

// platformOutcomePhrase words one platform's result off the two facts the
// import reports, and never off one of them.
//
// What happened to the ROWS (cookies.ImportOutcome) and whether the platform
// AUTHENTICATES afterwards (the verdict, or the accepted flag for a check that
// could not reach the site) are independent, and the pair that proves it is
// (RefreshOK, ImportRolledBack): the platform is alive precisely because the
// paste was thrown out. A single pass/fail cannot say that, which is why the
// Web import's payload keeps the same two fields apart.
func platformOutcomePhrase(outcome cookies.ImportOutcome, verdict cookies.RefreshVerdict, accepted bool) string {
	auth := verdict.String()
	if accepted {
		auth = "authenticates"
	}
	return outcome.String() + " (" + auth + ")"
}

// platformOutcomeLine is that phrase as one column-aligned overlay row.
func platformOutcomeLine(name string, outcome cookies.ImportOutcome, verdict cookies.RefreshVerdict, accepted bool) string {
	return fmt.Sprintf("  %-9s %s", name+":", platformOutcomePhrase(outcome, verdict, accepted))
}

// importResultSummary is the same answer on ONE line, for the feedback bar.
//
// It exists because Esc during a slow import used to throw the outcome away:
// the import is not cancellable, so closing the overlay only closed the place
// the answer was going to be written. Same words as the overlay's rows, from
// the same helper, so the two surfaces cannot drift — and, like them, it
// carries outcomes, verdicts and sentinel error text only. Never cookie
// content: every ImportCookies failure is a sentinel or a sentinel wrapped
// with a platform name, and none of them quotes the file.
func importResultSummary(r cookies.ImportResult, err error) string {
	if err != nil {
		return "Cookie import failed: " + err.Error()
	}
	return fmt.Sprintf("Cookie import: YouTube %s; Twitch %s",
		platformOutcomePhrase(r.YouTubeOutcome, r.YouTube, r.YouTubeAccepted),
		platformOutcomePhrase(r.TwitchOutcome, r.Twitch, r.TwitchAccepted))
}

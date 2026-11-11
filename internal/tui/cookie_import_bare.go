package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The bare form of the cookie file import, drawn below the normal layout
// (overlay_bare.go): a title row, the body, the keys.
//
//	path step   the input on the first body row, always; under it the error
//	            (its sentence, then the path that was tried) or, with none,
//	            what the file is
//	running     the spinner, and that Esc closes without cancelling
//	result      one row per platform with both facts (platformOutcomePhrase),
//	            or the import's error wrapped, which takes the keys row
//	            and the title before it is cut
//
// As in the normal form the model holds the path only: nothing of the file
// is ever read here or drawn.

// bareCookieFullLabels is the narrowest terminal that spells the platforms
// out: "YouTube: rolled-back (authenticates)" is 36 cells.
const bareCookieFullLabels = 36

// bare reports whether the overlay draws its bare form: its own size is
// below the normal layout. Derived on every call, never stored.
func (m *CookieImportDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// barePathError is the path step's inline error as at most rows rows of w
// cells: the sentence wrapped, then the path on rows of its own. A path too
// long for the rows left is cut at its head, so the file name stays.
func barePathError(errorMsg string, w, rows int) []string {
	sentence, path, hasPath := strings.Cut(errorMsg, "\n")
	// The one sentence too long for 30 columns has a short form.
	if sentence == "That path is a directory, not a file:" {
		sentence = "That is a directory:"
	}
	if !hasPath {
		return bareWrap(sentence, w, rows)
	}
	out := bareWrap(sentence, w, max(rows-1, 1))
	left := rows - len(out)
	if left <= 0 {
		return out
	}
	path = bareHeadCut(strings.TrimSpace(path), left*w)
	for path != "" {
		row := truncateWidth(path, w, "")
		if row == "" {
			break
		}
		out = append(out, row)
		path = path[len(row):]
	}
	return out
}

// barePlatformRow is one platform's result row: the full name from
// bareCookieFullLabels columns, two letters below it, and the phrase the
// normal form and the feedback line use.
func barePlatformRow(w int, name, short string, outcome cookies.ImportOutcome, verdict cookies.RefreshVerdict, accepted bool) string {
	phrase := platformOutcomePhrase(outcome, verdict, accepted)
	if w >= bareCookieFullLabels {
		return name + ": " + strings.Repeat(" ", len("YouTube")-len(name)) + phrase
	}
	return short + " " + phrase
}

// bareView draws the bare form: exactly width x height cells.
func (m *CookieImportDialogModel) bareView() string {
	w := m.width
	rows := max(m.height-2, 1) // the body: every row but the title and the keys
	var sheet bareSheet
	switch m.step {
	case cookieImportStepPath:
		sheet.left = TitleStyle.Render("Import cookies.txt")
		sheet.keys = DimStyle.Render("Enter import  Esc cancel")
		// The prompt is two cells and the input draws a cursor cell after
		// its text.
		sheet.body = []string{bareInput(m.input, w-3)}
		if m.errorMsg != "" {
			sheet.body = append(sheet.body, bareStyled(barePathError(m.errorMsg, w, rows-1), ErrorStyle)...)
		} else if rows > 1 {
			about := bareWrap("Path to a Netscape-format cookies.txt exported from your browser", w, rows-1)
			sheet.body = append(sheet.body, bareStyled(about, DimStyle)...)
		}

	case cookieImportStepRunning:
		sheet.left = TitleStyle.Render("Cookie import")
		sheet.keys = DimStyle.Render("Esc close (import continues)")
		spin := m.spinner.View() + " "
		text := "Importing, then verifying each platform..."
		if ansi.StringWidth(spin)+len(text) > w {
			text = "Importing, verifying..."
		}
		sheet.body = []string{spin + text}

	case cookieImportStepResult:
		sheet.left = TitleStyle.Render("Cookie import")
		sheet.keys = DimStyle.Render("Enter/Esc close")
		if m.resultErr != nil {
			// No row is held back for a heading: the error is in the body
			// where it fits there, and takes the keys row and the title
			// before any of it is cut (owner ruling R-C11). "Import failed"
			// leads it while the whole still fits the screen.
			msg := "Import failed: " + m.resultErr.Error()
			if len(bareWrap(msg, w, len(msg)+1)) > m.height {
				msg = m.resultErr.Error()
			}
			sheet.setError(msg, w, m.height)
			break
		}
		r := m.result
		sheet.body = []string{
			barePlatformRow(w, "YouTube", "YT", r.YouTubeOutcome, r.YouTube, r.YouTubeAccepted),
			barePlatformRow(w, "Twitch", "TW", r.TwitchOutcome, r.Twitch, r.TwitchAccepted),
		}
		// On the outcome, as in the normal form, never on RollbackProtected.
		if r.YouTubeOutcome == cookies.ImportRolledBack || r.TwitchOutcome == cookies.ImportRolledBack {
			sheet.body = append(sheet.body, bareStyled(bareWrap("rolled back = old cookies kept", w, max(rows-2, 1)), DimStyle)...)
		}
	}
	for i, row := range sheet.body {
		if ansi.StringWidth(row) > w {
			sheet.body[i] = truncateWidth(row, w, "")
		}
	}
	return sheet.render(w, m.height)
}

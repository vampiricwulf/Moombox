package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// releaseNotesOverlay is a modal that shows release notes for a pending
// update. Scrollable via arrow keys / pgup-pgdn (handled by the
// embedded viewport; letter-key bindings disabled to avoid conflict
// with app chords). Opened via R N chord; from within, U applies the
// update, S skips it (only while pending is set), or Esc/Q closes.
type releaseNotesOverlay struct {
	open_    bool
	tag      string
	rawNotes string
	width    int
	height   int
	// pending marks that the notes being shown are for a still-skippable
	// update (set via setPending by the R N open site when a.updateAvailable
	// is non-nil), so the footer offers the S key. Reset on close so a
	// later open() for an arbitrary version's notes defaults to false.
	pending bool
	// isDark mirrors App.isDark (terminal background detection) so the
	// markdown renderer matches the terminal, exactly as the huh themes
	// already do. Defaults true: dark is Moombox's historical assumption
	// and the value BackgroundColorMsg installs before any overlay opens.
	isDark bool
	vp     viewport.Model
}

// newReleaseNotesOverlay returns a closed overlay ready for use.
func newReleaseNotesOverlay() *releaseNotesOverlay {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.KeyMap = helpViewportKeyMap() // reuse help.go's safe key map (no letter keys)
	return &releaseNotesOverlay{vp: vp, isDark: true}
}

// isOpen reports whether the overlay is currently visible.
func (o *releaseNotesOverlay) isOpen() bool { return o.open_ }

// setPending marks whether the notes currently shown belong to a still
// pending (skippable) update. The R N chord calls this right after open()
// with a.updateAvailable != nil; fetching an arbitrary version's notes (no
// update pending) leaves it false.
func (o *releaseNotesOverlay) setPending(p bool) { o.pending = p }

// open prepares and shows the overlay. width/height are the terminal
// dimensions; the overlay sizes itself to ~80% of those.
func (o *releaseNotesOverlay) open(tag, rawNotes string, width, height int) {
	o.open_ = true
	o.tag = tag
	o.rawNotes = rawNotes
	o.applySize(width, height)
	o.vp.GotoTop()
}

// applySize sizes the viewport for the given terminal dimensions and
// re-renders the body at that width (renderBody bakes its word wrap into the
// rendered text, so the content must be regenerated whenever the width
// changes — resizing the viewport alone would leave the old wrap points).
//
// The min() against the terminal is what keeps the box on screen. The ~80%
// target has a floor (40 columns wide, 8 rows tall) so the notes stay
// readable on a normal terminal, but that floor used to be applied without
// any upper bound, so a 30-column window produced a 46-column box — and
// centerBox can only pad, never clip, so it spilled past the right edge.
func (o *releaseNotesOverlay) applySize(width, height int) {
	o.width = width
	o.height = height

	// -4 borders + padding, -6 borders + title + footer.
	vpWidth := max(min(max(width*8/10, 40), width)-4, 1)
	vpHeight := max(min(max(height*8/10, 8), height)-6, 1)

	o.vp.SetWidth(vpWidth)
	o.vp.SetHeight(vpHeight)
	o.vp.SetContent(o.renderBody(vpWidth))
}

// setSize reflows an open overlay for a new terminal size, preserving the
// reader's scroll position. recalcLayout calls this on every resize: the
// overlay used to size itself only in open(), so resizing the terminal
// while the notes were up left the text wrapped for the old width.
func (o *releaseNotesOverlay) setSize(width, height int) {
	if !o.open_ {
		o.width, o.height = width, height
		return
	}
	off := o.vp.YOffset()
	o.applySize(width, height)
	o.vp.SetYOffset(off)
}

// close hides the overlay and clears its state.
func (o *releaseNotesOverlay) close() {
	o.open_ = false
	o.tag = ""
	o.rawNotes = ""
	o.pending = false
}

// Update routes a tea.Msg to the embedded viewport for scroll handling.
// Returns the tea.Cmd from the viewport (typically nil).
func (o *releaseNotesOverlay) Update(msg tea.Msg) tea.Cmd {
	if !o.open_ {
		return nil
	}
	var cmd tea.Cmd
	o.vp, cmd = o.vp.Update(msg)
	return cmd
}

// View returns the rendered overlay frame. Empty string when closed.
func (o *releaseNotesOverlay) View() string {
	if !o.open_ {
		return ""
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorGreen).Padding(0, 1)
	footerStyle := lipgloss.NewStyle().Faint(true).Padding(0, 1)
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorGreen)

	// The title and footer are laid out beside a viewport that was already
	// sized to the terminal, so THEY decide the box width whenever they are
	// the widest line — and the full footer is 44 columns, which is what
	// made a 30-column terminal render a 46-column box. Both now shrink to
	// the viewport's width: the footer steps down through shorter spellings
	// (keeping Esc, the one binding a reader must not be stranded without)
	// and the title truncates.
	inner := o.vp.Width()
	// Viewer mode (R N with no pending update — the running version's own
	// notes) offers no U: there is nothing to apply, and U used to close the
	// notes being read to say so. The Web's viewer hides "Update Now" too.
	footerText := "↑/↓: Scroll  Esc/Q: Close"
	candidates := []string{
		footerText,
		"↑/↓ scroll · Esc close",
		"↑/↓ · Esc",
		"Esc",
	}
	if o.pending {
		footerText = "U: Apply update  S: Skip  ↑/↓: Scroll  Esc/Q: Close"
		candidates = []string{
			footerText,
			"U update · S skip · ↑/↓ scroll · Esc close",
			"U · S · ↑/↓ · Esc",
			"Esc",
		}
	}
	for _, cand := range candidates {
		footerText = cand
		if lipgloss.Width(cand)+2 <= inner { // +2 for the style's horizontal padding
			break
		}
	}
	title := titleStyle.Render(truncateString("Release Notes — "+o.tag, max(inner-2, 1)))
	footer := footerStyle.Render(footerText)

	body := o.vp.View()
	joined := lipgloss.JoinVertical(lipgloss.Left, title, body, footer)
	box := borderStyle.Render(joined)

	// Backstop: centerBox pads but never clips, so anything still too wide
	// would spill off-screen rather than being cut.
	return centerBox(lipgloss.NewStyle().MaxWidth(o.width).Render(box), o.width, o.height)
}

// renderBody runs glamour over the raw markdown to produce ANSI text
// sized to the given width. Returns a fallback message for empty notes.
// An explicit standard style is used instead of WithAutoStyle() so that
// glamour always produces styled output — WithAutoStyle falls back to
// no-op ASCII mode when no TTY is detected (e.g. in tests or when
// TERM is unset), which would leave raw "## Heading" syntax visible.
// Which style is chosen follows the detected terminal background: the
// dark palette on a light terminal renders low-contrast body text, and
// the app already routes the same signal into its huh themes, so the
// release notes were the one themed surface ignoring it.
//
// Glamour renders UNWRAPPED and wrapReleaseNotes wraps its output. Glamour's
// own wrap put a long bullet's continuation rows flush with the bullet, so
// every release note — which is nothing but long bullets — read as a column
// of ragged paragraphs with a dot on the first line of each. Glamour has no
// hanging indent to ask for, so the wrap is done here, where the list items
// can be told apart from everything else; see wrapReleaseNotes.
func (o *releaseNotesOverlay) renderBody(width int) string {
	if strings.TrimSpace(o.rawNotes) == "" {
		return "No release notes available for this update."
	}
	style := releaseNotesStyle(o.isDark)
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(0), // off: wrapReleaseNotes wraps, below
	)
	if err != nil {
		return o.rawNotes
	}
	rendered, err := r.Render(o.rawNotes)
	if err != nil {
		return o.rawNotes
	}
	// The right margin glamour kept when it wrapped: it wrapped to the width
	// less its document margin on BOTH sides and indented by one of them, so
	// the text stopped a margin short of the edge. Same edge here, so a
	// paragraph breaks where it always did.
	margin := 0
	if style.Document.Margin != nil {
		margin = int(*style.Document.Margin)
	}
	limit := width - margin
	if limit < 1 {
		limit = width
	}
	return wrapReleaseNotes(rendered, limit)
}

// releaseNotesListMark tags the hanging-indent column of every list item in
// glamour's output: U+E000, a private-use rune no release note contains,
// appended to the bullet ("• "), the enumeration's ". " and a task's checkbox
// so it lands exactly where the item's text begins. wrapReleaseNotes reads
// the column off it and removes it before anything is shown.
//
// A mark rather than a pattern over the text because the text cannot tell a
// list item from a code line that happens to begin "1. " — and code blocks
// must wrap exactly as they did, with no hang.
const releaseNotesListMark = "\uE000"

// releaseNotesQuoteMark tags the end of a blockquote's bar the same way:
// U+E001, appended to the "│ " glamour starts every line of a quote with, so
// wrapReleaseNotes can repeat the bar on every row the line wraps to — as
// glamour's own wrap did — and removes it before anything is shown. A rune of
// its own rather than the list mark, because a list inside a quote carries
// both on one line, and the bar is repeated where a bullet is not.
const releaseNotesQuoteMark = "\uE001"

// releaseNotesStyle is glamour's standard dark or light style with the list
// mark added to every list-item prefix: the bullet, the enumeration's ". ",
// and a task item's two checkboxes, which glamour draws in the bullet's place;
// and the quote mark added to the blockquote's bar. A copy: those are values
// inside the StyleConfig, so the package-level config glamour's other users
// read is untouched — the bar is a *string the copy shares with it, so it is
// replaced by a new string, never written through.
func releaseNotesStyle(dark bool) gansi.StyleConfig {
	cfg := styles.LightStyleConfig
	if dark {
		cfg = styles.DarkStyleConfig
	}
	cfg.Item.BlockPrefix += releaseNotesListMark
	cfg.Enumeration.BlockPrefix += releaseNotesListMark
	cfg.Task.Ticked += releaseNotesListMark
	cfg.Task.Unticked += releaseNotesListMark
	bar := " " // what glamour indents a quote with when a style names no bar
	if cfg.BlockQuote.IndentToken != nil {
		bar = *cfg.BlockQuote.IndentToken
	}
	bar += releaseNotesQuoteMark
	cfg.BlockQuote.IndentToken = &bar
	return cfg
}

// wrapReleaseNotes wraps glamour's unwrapped output so no row is wider than
// limit, ANSI-aware (lipgloss.Wrap carries a style across the rows it breaks),
// breaking at spaces and hyphens — the rule glamour applied to paragraphs and
// headings. Unwrapped, glamour emits one line per block line, so each line is
// one heading, one paragraph, one code line, or one source line of a list
// item, and it decides where that line's continuation rows start:
//
//   - a list item continues UNDER ITS TEXT, at the column the list mark
//     sits in — past the bullet, the "1. " or the checkbox, at any nesting
//     depth;
//   - so does a list item's text that its SOURCE broke over several lines —
//     a hard-wrapped Markdown bullet, or a lazy continuation line. A
//     paragraph's soft breaks glamour turns into spaces, but an item's it
//     keeps, and it starts each later line at the item's BULLET column with
//     no mark; that line is moved under the item's text and hangs there too.
//     See releaseNotesItem for how a line is told to be one;
//   - a blockquote's line continues after its BAR, and every row repeats the
//     bar, as glamour's own wrap did ("  │ " down the whole quote). Behind
//     the bar the quote's lines follow these same rules, so a list in a quote
//     hangs under its text with the bar in front; see wrapReleaseNotesQuote;
//   - everything else continues at the margin glamour indented the block by,
//     the run of plain spaces ahead of the line's first escape sequence. That
//     is where glamour's own wrap put it: a heading or paragraph continues at
//     its first column, and a code line at the document margin rather than
//     under the code, exactly as before.
func wrapReleaseNotes(rendered string, limit int) string {
	return strings.Join(wrapReleaseNotesLines(strings.Split(rendered, "\n"), limit), "\n")
}

// releaseNotesUnmark removes both marks from a line before it is shown.
var releaseNotesUnmark = strings.NewReplacer(releaseNotesListMark, "", releaseNotesQuoteMark, "")

// wrapReleaseNotesLines is wrapReleaseNotes over lines already split, returning
// the rows. A blockquote's lines come back through it without their bar, as a
// document of their own.
func wrapReleaseNotesLines(lines []string, limit int) []string {
	out := make([]string, 0, len(lines))
	var open []releaseNotesItem // the items a line may still belong to, outermost first
	// owner closes the items a line starting at visible column indent has
	// left — every one whose bullet sits deeper — and reports the text column
	// of the item it starts at the bullet column of: the column the line
	// belongs under.
	owner := func(indent int) (text int, ok bool) {
		for len(open) > 0 && open[len(open)-1].bullet > indent {
			open = open[:len(open)-1]
		}
		// Not when the text column leaves no room: hangingWrap drops such a
		// hang, and the spaces added for it would stay on the first row, past
		// the edge. The line keeps glamour's indent, as the item's first line
		// keeps its bullet.
		if n := len(open); n > 0 && open[n-1].bullet == indent && open[n-1].text < limit {
			return open[n-1].text, true
		}
		return 0, false
	}
	for j := 0; j < len(lines); j++ {
		line := lines[j]
		hang := len(line) - len(strings.TrimLeft(line, " "))
		visible := ansi.Strip(line)
		indent := len(visible) - len(strings.TrimLeft(visible, " "))
		i := strings.Index(line, releaseNotesListMark)
		switch q := strings.Index(line, releaseNotesQuoteMark); {
		case q >= 0 && (i < 0 || q < i):
			// A blockquote, as far as glamour starts lines with this same
			// bar: its blank lines and the blocks nested in it included.
			bar := line[:q+len(releaseNotesQuoteMark)]
			end := j + 1
			for end < len(lines) && strings.HasPrefix(lines[end], bar) {
				end++
			}
			shift := 0
			if text, ok := owner(indent); ok {
				shift = text - indent
			}
			out = append(out, wrapReleaseNotesQuote(lines[j:end], len(bar), shift, limit)...)
			j = end - 1
			continue
		case i >= 0:
			hang = ansi.StringWidth(line[:i])
			open = append(open, releaseNotesItem{bullet: indent, text: hang})
			line = releaseNotesUnmark.Replace(line)
		case strings.TrimSpace(visible) == "":
			// Glamour closes every list with a blank line, and every block
			// after one opens past it.
			open = open[:0]
		default:
			if text, ok := owner(indent); ok {
				// TruncateLeft drops glamour's indent cells, styled or not,
				// and keeps their escape sequences.
				hang = text
				line = strings.Repeat(" ", hang) + ansi.TruncateLeft(line, indent, "")
			}
		}
		out = append(out, hangingWrap(line, hang, limit)...)
	}
	return out
}

// wrapReleaseNotesQuote wraps one blockquote: lines that all open with the same
// bar, barLen bytes of it, the quote mark last. Each line loses its bar, the
// rest is wrapped by wrapReleaseNotesLines as a document of its own — narrower
// by the bar, with list items of its own, so a list in the quote hangs as one
// outside it does and a nested quote repeats its own bar — and EVERY row comes
// back with the bar in front. Glamour's own wrap repeated it on every row
// ("  │ …"), which is how a reader tells where a quote ends; the margin rule
// alone started each row after a line's first at the document margin, with
// no bar at all.
//
// shift moves the bar under the text of the list item the quote sits in:
// glamour starts it at the item's BULLET column, as it does the item's later
// source lines, and those move under the text too.
//
// A bar that leaves no room for text (a box narrower than the bar) is dropped,
// shift and all: each line wraps whole from the left edge, as hangingWrap
// drops a hang, so the narrowest overlay still never spills.
func wrapReleaseNotesQuote(lines []string, barLen, shift, limit int) []string {
	bar := strings.Repeat(" ", shift) + lines[0][:barLen-len(releaseNotesQuoteMark)]
	width := ansi.StringWidth(bar)
	if width >= limit {
		var out []string
		for _, line := range lines {
			out = append(out, hangingWrap(releaseNotesUnmark.Replace(line), 0, limit)...)
		}
		return out
	}
	inner := make([]string, len(lines))
	for k, line := range lines {
		inner[k] = line[barLen:]
	}
	rows := wrapReleaseNotesLines(inner, limit-width)
	for k, row := range rows {
		// The bar leaves its style open — the mark sits inside the span — and
		// a wrapped row need not open one of its own before its text.
		rows[k] = bar + ansi.ResetStyle + row
	}
	return rows
}

// releaseNotesItem is a list item a later unmarked line may still belong to:
// the visible column its bullet starts at and the column its text starts at.
//
// Glamour starts an item's later source lines at its BULLET column, so a line
// that does belongs to the item: a nested item's line sits two columns deeper,
// a code block inside the item deeper still (it keeps the margin rule, as a
// top-level one does), and the enclosing item's shallower — which is why the
// open items are a stack. Glamour ends a nested list with no blank line before
// the enclosing item's next paragraph, so a shallower line closes every item
// deeper than it and lands on the one it belongs to; a blank line closes them
// all. Only the top is ever compared, so a sibling the newest item replaced
// is never popped on its own: it sits under that item, as deep or deeper, and
// leaves with it.
type releaseNotesItem struct {
	bullet, text int
}

// hangingWrap breaks one line into rows no wider than limit: the first keeps
// its leading hang columns, and every later one starts with hang spaces. A
// hang that leaves no room for text (a box narrower than a bullet) is dropped,
// so the narrowest overlay still never spills.
func hangingWrap(line string, hang, limit int) []string {
	if ansi.StringWidth(line) <= limit {
		return []string{line}
	}
	if hang >= limit {
		hang = 0
	}
	head := ansi.Truncate(line, hang, "")
	// TruncateLeft drops the head's cells but keeps its escape sequences, so
	// the body opens in the style it was in.
	rows := strings.Split(lipgloss.Wrap(ansi.TruncateLeft(line, hang, ""), limit-hang, ""), "\n")
	pad := strings.Repeat(" ", hang)
	for i := range rows {
		if i == 0 {
			rows[i] = head + rows[i]
		} else {
			rows[i] = pad + rows[i]
		}
	}
	return rows
}

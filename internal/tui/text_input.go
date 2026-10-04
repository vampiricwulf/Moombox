package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// newSpinner creates a spinner with Moombox styling.
func newSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorCyan)
	return s
}

// spinnerTickCmd returns a tea.Cmd that emits the spinner's initial Tick
// message. Shared helper for dialog Open() flows that need to start the
// spinner animation (see reports/tui.md Finding 15 — previously duplicated
// as a SpinnerInit method on every dialog type).
func spinnerTickCmd(s spinner.Model) tea.Cmd {
	return func() tea.Msg { return s.Tick() }
}

// newTextInput creates a text input with Moombox styling.
func newTextInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Blurred.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Cursor.Color = ColorCyan
	ti.SetStyles(s)
	return ti
}

// loadTextInput seats a text input on a field the operator is about to edit:
// the value replaced and the cursor parked at its end.
//
// SetValue alone is not enough, because bubbles only moves the cursor when the
// old offset no longer fits inside the new value. A port edited to "774" leaves
// the cursor at offset 3; a move down to output_directory loads "./output",
// offset 3 still fits, and the first keystroke lands mid-value ("./oxutput").
// Every site that (re)loads a field for editing goes through here. Mid-edit
// syncs do not, so a cursor the operator moved on purpose stays where it is.
func loadTextInput(ti *textinput.Model, value string) {
	ti.SetValue(value)
	ti.CursorEnd()
}

// configureTextInput resets a text input for a new field context.
func configureTextInput(ti *textinput.Model, value string, validate textinput.ValidateFunc, echoMode textinput.EchoMode) {
	loadTextInput(ti, value)
	ti.Validate = validate
	ti.EchoMode = echoMode
	ti.Focus()
}

// validateTimeChars rejects strings containing non-time characters.
func validateTimeChars(s string) error {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || r == ':' || r == '.') {
			return fmt.Errorf("invalid character for time input")
		}
	}
	return nil
}

// validateDigitsOnly rejects strings containing non-digit characters.
func validateDigitsOnly(s string) error {
	for _, r := range s {
		if r < '0' || r > '9' {
			return fmt.Errorf("only digits allowed")
		}
	}
	return nil
}

// validateDecimal accepts a bare non-negative decimal number — digits and at
// most one ".". The six FlexDuration-backed number fields need it: "0.5" is
// a valid twelve-hour / thirty-second value that the config file and the Web
// UI both accept, and validateDigitsOnly made it untypeable in the terminal
// (CORE-7). Range and NaN/Inf checking stays in applyValues; this is only
// the keystroke filter.
func validateDecimal(s string) error {
	seenDot := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !seenDot:
			seenDot = true
		default:
			return fmt.Errorf("only digits and one decimal point allowed")
		}
	}
	return nil
}

// renderInactiveInput renders a text value styled but without a cursor.
func renderInactiveInput(value string, w int, c color.Color) string {
	display := value
	if runewidth.StringWidth(display) > w {
		display = truncateString(display, w)
	}
	return lipgloss.NewStyle().Foreground(c).Render(display)
}

// renderPasswordDots renders a masked password string (dots).
func renderPasswordDots(value string) string {
	return strings.Repeat("•", utf8.RuneCountInString(value))
}

// filterChannelFieldsByPlatform returns channel fields filtered by the current
// platform value in vals. Fields with an empty platformFilter always pass.
func filterChannelFieldsByPlatform(fields []channelFieldDef, vals map[string]string) []channelFieldDef {
	platform := "youtube"
	if vals != nil {
		if p, ok := vals["platform"]; ok {
			platform = p
		}
	}
	var out []channelFieldDef
	for _, f := range fields {
		if f.platformFilter == "" || f.platformFilter == platform {
			out = append(out, f)
		}
	}
	return out
}

// cycleFieldOption cycles a map value through the given options list.
// direction=1 goes forward, direction=-1 goes backward.
func cycleFieldOption(vals map[string]string, key string, options []string, direction int) {
	if len(options) == 0 {
		return
	}
	cur := vals[key]
	idx := 0
	found := false
	for i, opt := range options {
		if strings.EqualFold(opt, cur) {
			idx = i
			found = true
			break
		}
	}
	if !found {
		if direction == 1 {
			vals[key] = options[0]
		} else {
			vals[key] = options[len(options)-1]
		}
		return
	}
	next := (idx + direction + len(options)) % len(options)
	vals[key] = options[next]
}

// cycleNumberPreset steps a NUMBER field's value through its preset options.
//
// It is the numeric sibling of cycleFieldOption, and it exists for one
// difference: a number row's value need not be one of the options at all (the
// operator can type anything), and cycleFieldOption's unrecognised-value
// fallback jumps to options[0] or options[len-1] — for the resolution ladder
// that turns a right-arrow on a typed 1234 into "0", i.e. unbounded. Here an
// off-ladder value steps to the nearest preset strictly ABOVE it going
// forward, or strictly BELOW it going back, wrapping at the ends exactly as
// cycleFieldOption does. For a value that IS a preset the two are identical.
//
// options must be ascending numeric strings; a value that does not parse falls
// back to the ends, which is what a half-typed field wants.
func cycleNumberPreset(vals map[string]string, key string, options []string, direction int) {
	if len(options) == 0 {
		return
	}
	cur, err := strconv.Atoi(strings.TrimSpace(vals[key]))
	if err != nil {
		if direction >= 0 {
			vals[key] = options[0]
		} else {
			vals[key] = options[len(options)-1]
		}
		return
	}
	if direction >= 0 {
		for _, opt := range options {
			if n, err := strconv.Atoi(opt); err == nil && n > cur {
				vals[key] = opt
				return
			}
		}
		vals[key] = options[0]
		return
	}
	for i := len(options) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(options[i]); err == nil && n < cur {
			vals[key] = options[i]
			return
		}
	}
	vals[key] = options[len(options)-1]
}

// dialogBox computes standard dialog box and content widths from a maximum
// width and the current screen width.
func dialogBox(maxW, screenW int) (boxW, contentW int) {
	boxW = min(max(min(maxW, screenW-4), 40), screenW)
	contentW = max(0, boxW-4)
	return
}

// renderTimeInputPair renders a start/end time input pair with consistent
// styling. activeField is 0 for start, 1 for end. ti is the focused textinput
// model. accentColor is the color for the active label.
func renderTimeInputPair(
	startValue, endValue string,
	activeField int,
	ti textinput.Model,
	w int,
	accentColor color.Color,
) []string {
	startLabel := "  Start: "
	endLabel := "  End:   "
	if activeField == 0 {
		startLabel = "> Start: "
	}
	if activeField == 1 {
		endLabel = "> End:   "
	}

	startStyle := DimStyle
	endStyle := DimStyle
	if activeField == 0 {
		startStyle = lipgloss.NewStyle().Foreground(accentColor)
	}
	if activeField == 1 {
		endStyle = lipgloss.NewStyle().Foreground(accentColor)
	}

	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Blurred.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Cursor.Color = ColorCyan
	ti.SetStyles(s)
	ti.SetWidth(w - 12)

	var lines []string
	if activeField == 0 {
		lines = append(lines, startStyle.Render(startLabel)+ti.View())
		lines = append(lines, endStyle.Render(endLabel)+renderInactiveInput(endValue, w-12, ColorCyan))
	} else {
		lines = append(lines, startStyle.Render(startLabel)+renderInactiveInput(startValue, w-12, ColorCyan))
		lines = append(lines, endStyle.Render(endLabel)+ti.View())
	}
	return lines
}

// MapAccessor implements huh.Accessor[string] for map-backed form values.
type MapAccessor struct {
	M   map[string]string
	Key string
}

// Get returns the value from the map.
func (a *MapAccessor) Get() string { return a.M[a.Key] }

// Set stores the value in the map.
func (a *MapAccessor) Set(v string) { a.M[a.Key] = v }

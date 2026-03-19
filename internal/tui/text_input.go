package tui

import (
	"fmt"
	"image/color"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
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

// configureTextInput resets a text input for a new field context.
func configureTextInput(ti *textinput.Model, value string, validate textinput.ValidateFunc, echoMode textinput.EchoMode) {
	ti.SetValue(value)
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

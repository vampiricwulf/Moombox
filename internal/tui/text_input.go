package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// newSpinner creates a spinner with Moombox styling.
func newSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorCyan)
	return s
}

// newTextInput creates a text input with Moombox styling and a static cursor.
func newTextInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(ColorCyan)
	ti.TextStyle = lipgloss.NewStyle().Foreground(ColorCyan)
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
func renderInactiveInput(value string, w int, color lipgloss.Color) string {
	display := value
	if runewidth.StringWidth(display) > w {
		display = truncateString(display, w)
	}
	return lipgloss.NewStyle().Foreground(color).Render(display)
}

// renderPasswordDots renders a masked password string (dots).
func renderPasswordDots(value string) string {
	dots := ""
	for range []rune(value) {
		dots += "•"
	}
	return dots
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

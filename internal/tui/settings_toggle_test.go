package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A toggle marked its value by colour and a faint unselected half only, so
// with colour stripped — NO_COLOR, a colour-blind reader, a pasted screenshot
// — "Yes / No" read the same either way. The selected half is bracketed now,
// as on cycle rows.
//
// Mutant: renderToggle without the brackets — the stripped text is the same
// for both values.
func TestToggleShowsItsValueWithoutColour(t *testing.T) {
	if got := ansi.Strip(renderToggle("Yes")); got != "[Yes] / No" {
		t.Errorf("Yes renders as %q, want %q", got, "[Yes] / No")
	}
	if got := ansi.Strip(renderToggle("No")); got != "Yes / [No]" {
		t.Errorf("No renders as %q, want %q", got, "Yes / [No]")
	}
}

// The click geometry follows the brackets: the "Yes" half is five columns
// wide while selected, three otherwise.
//
// Mutant: the old fixed geometry (Yes below column 3, No from column 6) —
// clicking the "]" of a selected "[Yes]" leaves the value alone, and a click
// on the "N" of "No" after "[Yes] / " lands in the old separator.
func TestToggleClickFollowsTheBrackets(t *testing.T) {
	m := NewSettingsModel()
	m.values = map[string]string{}
	fd := fieldDef{key: "k", ftype: fieldToggle}
	const labelWidth = 10
	click := func(value string, valueX int) string {
		m.values["k"] = value
		m.handleToggleClick(fd, 2+labelWidth+2+valueX, labelWidth)
		return m.values["k"]
	}
	for _, tc := range []struct {
		value  string
		valueX int
		want   string
	}{
		{"No", 0, "Yes"},  // "Y" of "Yes / [No]"
		{"No", 4, "No"},   // the separator
		{"No", 7, "No"},   // inside "[No]"
		{"Yes", 4, "Yes"}, // the "]" of "[Yes]"
		{"Yes", 6, "Yes"}, // the separator after "[Yes]"
		{"Yes", 8, "No"},  // "N" of "[Yes] / No"
	} {
		if got := click(tc.value, tc.valueX); got != tc.want {
			t.Errorf("value %q, click at %d: got %q, want %q", tc.value, tc.valueX, got, tc.want)
		}
	}
}

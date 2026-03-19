package tui

import (
	"image/color"

	"charm.land/bubbles/v2/table"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Status colors.
var (
	ColorDownloading = lipgloss.Color("#2ecc71")
	ColorLive        = lipgloss.Color("#2ecc71")
	ColorMuxing      = lipgloss.Color("#f1c40f")
	ColorUpcoming    = lipgloss.Color("#9333ea")
	ColorFinished    = lipgloss.Color("#1abc9c")
	ColorError       = lipgloss.Color("#e74c3c")
	ColorCancelled   = lipgloss.Color("#6b7280")
	ColorCookies     = lipgloss.Color("#d946ef")
	ColorTwitch      = lipgloss.Color("#e91e63")

	ColorWarning = lipgloss.Color("#f39c12")
	ColorCyan    = lipgloss.Color("#00bcd4")
	ColorGray    = lipgloss.Color("#95a5a6")
	ColorWhite = lipgloss.Color("#ffffff")
	ColorGreen = lipgloss.Color("#2ecc71")
	ColorRed   = lipgloss.Color("#e74c3c")

	// Log level colors.
	ColorLogError = lipgloss.Color("#e74c3c")
	ColorLogWarn  = lipgloss.Color("#f1c40f")
	ColorLogInfo  = lipgloss.Color("#ffffff")
	ColorLogDebug = lipgloss.Color("#95a5a6")
)

// Panel border styles.
var (
	FocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorCyan)

	UnfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorGray)
)

// Text styles.
var (
	TitleStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	DimStyle   = lipgloss.NewStyle().Faint(true)
	BoldStyle  = lipgloss.NewStyle().Bold(true)

	HeaderStyle    = lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	SeparatorStyle = lipgloss.NewStyle().Foreground(ColorGray)
	ErrorStyle     = lipgloss.NewStyle().Foreground(ColorError)
	SelectedStyle  = lipgloss.NewStyle().Bold(true)
)

// moomboxTheme returns a huh theme matching the Moombox TUI color scheme.
// isDark should reflect the terminal background; Moombox styles are dark-optimized
// but passing the detected value allows huh.ThemeBase to set correct base defaults.
// Returns huh.ThemeFunc which implements huh.Theme for use with WithTheme().
func moomboxTheme(isDark bool) huh.ThemeFunc {
	return huh.ThemeFunc(func(_ bool) *huh.Styles {
		t := huh.ThemeBase(isDark)

		// Focused field styles
		t.Focused.Base = lipgloss.NewStyle().PaddingLeft(1).
			BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).
			BorderForeground(ColorCyan)
		t.Focused.Title = lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
		t.Focused.Description = DimStyle
		t.Focused.ErrorIndicator = lipgloss.NewStyle().SetString(" *").Foreground(ColorError)
		t.Focused.ErrorMessage = lipgloss.NewStyle().Foreground(ColorError)
		t.Focused.SelectSelector = lipgloss.NewStyle().SetString("> ").Foreground(ColorCyan)
		t.Focused.Option = lipgloss.NewStyle().Foreground(ColorWhite)
		t.Focused.TextInput.Cursor = lipgloss.NewStyle().Foreground(ColorCyan)
		t.Focused.TextInput.CursorText = lipgloss.NewStyle().Foreground(ColorCyan)
		t.Focused.TextInput.Placeholder = lipgloss.NewStyle().Foreground(ColorGray)
		t.Focused.TextInput.Text = lipgloss.NewStyle().Foreground(ColorCyan)
		t.Focused.FocusedButton = lipgloss.NewStyle().
			Foreground(lipgloss.Color("0")).Background(ColorCyan).
			Padding(0, 2).MarginRight(1)
		t.Focused.BlurredButton = lipgloss.NewStyle().
			Foreground(ColorGray).Background(lipgloss.Color("0")).
			Padding(0, 2).MarginRight(1)

		// Blurred field styles
		t.Blurred = t.Focused
		t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
		t.Blurred.Title = lipgloss.NewStyle().Foreground(ColorGray)
		t.Blurred.TextInput.Text = lipgloss.NewStyle().Foreground(ColorGray)

		// Group styles
		t.Group.Title = lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
		t.Group.Description = DimStyle

		return t
	})
}

// formatTableStyles returns table styles matching the Moombox TUI color scheme.
func formatTableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorCyan).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(ColorGray)
	s.Selected = lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorWhite).
		Background(lipgloss.Color("#333355"))
	s.Cell = lipgloss.NewStyle()
	return s
}

// StatusColor returns the lipgloss color for a job status.
func StatusColor(status string) color.Color {
	switch database.JobStatus(status) {
	case database.StatusDownloading, database.StatusLive:
		return ColorDownloading
	case database.StatusMuxing:
		return ColorMuxing
	case database.StatusUpcoming:
		return ColorUpcoming
	case database.StatusFinished:
		return ColorFinished
	case database.StatusError:
		return ColorError
	case database.StatusCancelled:
		return ColorCancelled
	case database.StatusCookies:
		return ColorCookies
	default:
		return ColorWhite
	}
}

// StatusIcon returns a display icon for a job status.
func StatusIcon(status string) string {
	switch database.JobStatus(status) {
	case database.StatusLive, database.StatusDownloading:
		return "▼"
	case database.StatusMuxing:
		return "⎈"
	case database.StatusFinished:
		return "✓"
	case database.StatusError:
		return "✗"
	case database.StatusCookies:
		return "⏣"
	case database.StatusCancelled:
		return "⊘"
	case database.StatusUpcoming:
		return "○"
	default:
		return "○"
	}
}

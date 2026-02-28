package tui

import "github.com/charmbracelet/lipgloss"

// Status colors.
var (
	ColorDownloading = lipgloss.Color("#2ecc71")
	ColorLive        = lipgloss.Color("#2ecc71")
	ColorMuxing      = lipgloss.Color("#f1c40f")
	ColorUpcoming    = lipgloss.Color("#3498db")
	ColorFinished    = lipgloss.Color("#1abc9c")
	ColorError       = lipgloss.Color("#e74c3c")
	ColorCancelled   = lipgloss.Color("#95a5a6")
	ColorCookies     = lipgloss.Color("#e91e63")
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

// StatusColor returns the lipgloss color for a job status.
func StatusColor(status string) lipgloss.Color {
	switch status {
	case "Downloading", "Live":
		return ColorDownloading
	case "Muxing":
		return ColorMuxing
	case "Upcoming":
		return ColorUpcoming
	case "Finished":
		return ColorFinished
	case "Error":
		return ColorError
	case "Cancelled":
		return ColorCancelled
	case "COOKIES?":
		return ColorCookies
	default:
		return ColorWhite // match TS: white for unknown statuses
	}
}

// StatusIcon returns a display icon for a job status.
func StatusIcon(status string) string {
	switch status {
	case "Live":
		return "●"
	case "Downloading":
		return "▼"
	case "Muxing":
		return "⚙"
	case "Finished":
		return "✓"
	case "Error":
		return "✗"
	case "COOKIES?":
		return "⚠"
	case "Upcoming", "Cancelled":
		return "○"
	default:
		return "○" // match TS: empty circle for unknown statuses
	}
}

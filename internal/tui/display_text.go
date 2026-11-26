package tui

import (
	"strings"
	"unicode"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// displayText returns s with every control character as one space, for a
// title or a channel name the TUI draws. A tab measures as no cells and is
// drawn as several, a newline breaks a one-line row in two, and an escape
// can carry a style or a hyperlink into the frame: none of them has any
// business on screen, so the text is measured as it is drawn. The stored
// value is not changed; a string with nothing to replace comes back as it
// was, without a copy.
func displayText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// displayTitle is job's title as the TUI draws it, its video ID when it has
// no title.
func displayTitle(job *database.Job) string {
	if job.Title == "" {
		return displayText(job.VideoID)
	}
	return displayText(job.Title)
}

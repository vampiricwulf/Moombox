package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// The bare form of the yt-dlp plugin overlay, drawn below the normal layout
// (overlay_bare.go): the verdict in the title ("installed" or "not
// installed"), then the rows that say whether the plugin matches this
// process, then the paths, then the keys.
//
// The verdict rows are never dropped and never reworded into a different
// verdict: they are the same facts of ytdlpplugin.Info the normal form
// shows. A path is shown only where it fits whole (wrapped at its
// separators), the plugin path first because it is the one a manual
// --plugin-dirs run takes; a path that does not fit is replaced by a row
// that says so. A cut path is never shown.

// bare reports whether the overlay draws its bare form: its own size is
// below the normal layout. Derived on every call, never stored.
func (m *YtdlpDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// barePathRows is one path as bare rows under a short label, wrapped at its
// separators with the continuation rows under the value.
func barePathRows(label, path string, w int) []string {
	indent := len(label) + 1
	lines := strings.Split(ansi.Wrap(path, max(w-indent, 1), "/\\"), "\n")
	rows := make([]string, len(lines))
	for i, line := range lines {
		if i == 0 {
			rows[i] = label + " " + line
		} else {
			rows[i] = strings.Repeat(" ", indent) + line
		}
	}
	return rows
}

// bareStatusRows is the loaded body for a sheet of w cells with room for
// rows body rows.
func (m *YtdlpDialogModel) bareStatusRows(w, rows int) []string {
	https := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	out := []string{fmt.Sprintf("Moombox %d (https: %s)", m.info.CurrentPort, https(m.info.HTTPSEnabled))}
	if m.info.InstalledPort != nil {
		points := fmt.Sprintf("Plugin  %d", *m.info.InstalledPort)
		if m.info.InstalledScheme != "" {
			points += fmt.Sprintf(" (https: %s)", https(m.info.InstalledScheme == "https"))
		}
		out = append(out, points)
	}
	if m.info.Unparseable {
		out = append(out, YellowStyle.Render("Not recognized: I reinstalls"))
	}
	if m.info.PortMismatch {
		text := "Mismatch: I rewrites it"
		if w >= 40 {
			text = "Mismatch: I rewrites it to match"
		}
		out = append(out, YellowStyle.Render(text))
	}

	// The paths, each whole or not at all.
	var path, dir []string
	if m.info.ExtractedPath != "" {
		path = barePathRows("Path", m.info.ExtractedPath, w)
	}
	if m.info.PluginDir != "" {
		dir = barePathRows("Dir ", m.info.PluginDir, w)
	}
	left := rows - len(out)
	hidden := func(what string) []string {
		return []string{DimStyle.Render(what + ": enlarge to see")}
	}
	switch {
	case path == nil && dir == nil:
	case path != nil && dir != nil && len(path)+len(dir) <= left:
		out = append(append(out, path...), dir...)
	case path != nil && dir != nil && len(path)+1 <= left:
		out = append(append(out, path...), hidden("Dir")...)
	case path != nil && dir != nil:
		out = append(out, hidden("Paths")...)
	case path != nil && len(path) <= left:
		out = append(out, path...)
	case path != nil:
		out = append(out, hidden("Path")...)
	case len(dir) <= left:
		out = append(out, dir...)
	default:
		out = append(out, hidden("Dir")...)
	}
	return out
}

// bareView draws the bare form: exactly width x height cells.
func (m *YtdlpDialogModel) bareView() string {
	sheet := bareSheet{
		left: TitleStyle.Render("yt-dlp plugin"),
		keys: DimStyle.Render("I install  R refresh  Esc"),
	}
	rows := max(m.height-2, 1)
	switch {
	case m.installing:
		sheet.body = []string{m.spinner.View() + " Installing..."}
	case m.loading:
		sheet.body = []string{m.spinner.View() + " Loading..."}
	case m.errorMsg != "":
		sheet.setError(m.errorMsg, m.width, m.height)
	default:
		verdict := ": not installed"
		if m.info.Installed {
			verdict = ": installed"
		}
		sheet.left += verdict
		sheet.body = m.bareStatusRows(m.width, rows)
	}
	return sheet.render(m.width, m.height)
}

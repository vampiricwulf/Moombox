package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// The plugin paths are long, and at the 60-column floor the box is 54 wide:
// they wrapped back to column 0, under the labels. A value now wraps at a
// path separator with every continuation indented under the value column,
// and nothing is lost.
//
// Mutant: ytdlpRow printing the value unwrapped again, or its continuation
// lines without the indent.
func TestYtdlpDialogWrapsLongPathsUnderTheValueColumn(t *testing.T) {
	dir := "/home/someone/.config/yt-dlp/plugins/a-rather-long-directory-name/and-deeper-still"
	m := NewYtdlpDialogModel()
	m.SetSize(60, 30)
	m.visible = true
	m.info = ytdlpplugin.Info{PluginDir: dir, CurrentPort: 7743}
	v := ansi.Strip(m.View())

	var rows []string
	in := false
	for line := range strings.SplitSeq(v, "\n") {
		if w := ansi.StringWidth(line); w > 60 {
			t.Errorf("a line is %d columns: %q", w, line)
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "│"), "│")
		switch {
		case strings.Contains(inner, "Plugin dir:"):
			in = true
			_, val, _ := strings.Cut(inner, "Plugin dir:")
			rows = append(rows, strings.TrimSpace(val))
		case in && strings.HasPrefix(inner, strings.Repeat(" ", ytdlpRowIndent)) && strings.TrimSpace(inner) != "":
			rows = append(rows, strings.TrimSpace(inner))
		default:
			in = false
		}
	}
	if len(rows) < 2 {
		t.Fatalf("the long dir did not wrap under its value column at 60 columns:\n%s", v)
	}
	if got := strings.Join(rows, ""); got != dir {
		t.Errorf("the wrapped value reads %q, want %q", got, dir)
	}
}

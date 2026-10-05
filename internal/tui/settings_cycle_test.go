package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A cycle field is budgeted one row, but renderCycleOptions drew every option
// whatever the width: at the 60-column floor "[localhost] / lan / external"
// left "external" alone on the next row, and a channel's fifteen-option
// quality preference wrapped at any width. Past its width it shows only the
// selected option, between ‹ ›.
//
// Mutant: renderCycleOptions ignoring maxW, or the compact form dropping the
// selected option.
func TestCycleOptionsStayOneRow(t *testing.T) {
	quality := []string{"best", "2160p60", "2160p", "1440p60", "1440p", "1080p60", "1080p", "900p60", "900p", "720p60", "720p", "480p", "360p", "160p", "audio_only"}
	for _, maxW := range []int{20, 40, 70} {
		got := ansi.Strip(renderCycleOptions(quality, "720p", true, maxW))
		if w := ansi.StringWidth(got); w > maxW {
			t.Errorf("maxW %d: %d columns: %q", maxW, w, got)
		}
		if !strings.Contains(got, "[720p]") {
			t.Errorf("maxW %d: the selected option is gone: %q", maxW, got)
		}
	}
	if got := ansi.Strip(renderCycleOptions([]string{"localhost", "lan", "external"}, "lan", false, 80)); got != "localhost / [lan] / external" {
		t.Errorf("with room every option is listed: %q", got)
	}

	m := newSettingsModelForSave(t)
	m.SetSize(60, 24)
	v := ansi.Strip(m.View())
	for line := range strings.SplitSeq(v, "\n") {
		inner := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "│"))
		if inner == "external" || strings.HasPrefix(inner, "external ") {
			t.Errorf("a cycle option wrapped onto its own row at 60 columns:\n%s", v)
		}
	}
	if !strings.Contains(v, "‹ [localhost] ›") {
		t.Errorf("Network access is not shown compactly at 60 columns:\n%s", v)
	}
}

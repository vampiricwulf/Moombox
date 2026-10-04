package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestFFmpegPathFieldTypesEveryLetter: the FFmpeg path field is edited inline,
// and a bare "i" used to open the installer instead of typing — so
// /usr/local/bin/ffmpeg could not be entered, and each attempt closed the
// settings overlay. The installer is Ctrl+O now; every printable key types.
//
// Mutant: the old `key == "i"` binding — HandleKey returns open_ffmpeg at the
// first i and the field stops at "/usr/local/b".
func TestFFmpegPathFieldTypesEveryLetter(t *testing.T) {
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.Open(cfg)
	m.values["ffmpeg_path"] = ""
	m.focusFieldForTest(t, "ffmpeg_path")

	const path = "/usr/local/bin/ffmpeg"
	for _, r := range path {
		key := string(r)
		if action := m.HandleKey(key); action != "" {
			t.Fatalf("typing %q returned action %q, want it typed", key, action)
		}
		m.UpdateComponents(tea.KeyPressMsg{Code: r, Text: key})
	}
	if got := m.values["ffmpeg_path"]; got != path {
		t.Errorf("ffmpeg_path = %q, want %q", got, path)
	}

	ctrlO := tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	if ctrlO.String() != keyCtrlO {
		t.Fatalf("premise lost: Ctrl+O renders as %q, want %q", ctrlO.String(), keyCtrlO)
	}
	if action := m.HandleKey(keyCtrlO); action != "open_ffmpeg" {
		t.Errorf("Ctrl+O on the FFmpeg path returned %q, want open_ffmpeg", action)
	}
	m.UpdateComponents(ctrlO)
	if got := m.values["ffmpeg_path"]; got != path {
		t.Errorf("Ctrl+O changed the field to %q", got)
	}
	if hint := m.renderHintText(); !strings.Contains(hint, "Ctrl+O: Install FFmpeg") {
		t.Errorf("hint %q does not name Ctrl+O", hint)
	}
}

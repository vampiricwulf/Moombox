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
	// The typed path is an unsaved edit, so Ctrl+O asks first (see
	// TestCtrlOAsksBeforeDroppingUnsavedEdits); Esc backs out of the prompt.
	if action := m.HandleKey(keyCtrlO); action != "" || !m.closeConfirm {
		t.Errorf("Ctrl+O with an unsaved path returned %q (prompt=%v), want the save prompt", action, m.closeConfirm)
	}
	m.UpdateComponents(ctrlO)
	if got := m.values["ffmpeg_path"]; got != path {
		t.Errorf("Ctrl+O changed the field to %q", got)
	}
	m.HandleKey(keyEsc)
	if hint := m.renderHintText(); !strings.Contains(hint, "Ctrl+O: Install FFmpeg") {
		t.Errorf("hint %q does not name Ctrl+O", hint)
	}
}

// TestCtrlOAsksBeforeDroppingUnsavedEdits: the installer replaces the
// settings panel, and Ctrl+O used to close it outright — the one close path
// without the "Save changes?" prompt, so every unsaved edit was thrown away.
// It now asks; Discard or Save then hands back open_ffmpeg, Esc stays put,
// and with nothing unsaved Ctrl+O opens the installer at once.
//
// Mutant: return "open_ffmpeg" regardless of m.dirty — the first check fails.
func TestCtrlOAsksBeforeDroppingUnsavedEdits(t *testing.T) {
	open := func(t *testing.T) *SettingsModel {
		cfg := config.Defaults()
		m := NewSettingsModel()
		m.configStore = config.NewStore(cfg, "")
		m.cfg = cfg
		m.Open(cfg)
		m.focusFieldForTest(t, "ffmpeg_path")
		return m
	}

	m := open(t)
	if action := m.HandleKey(keyCtrlO); action != "open_ffmpeg" {
		t.Fatalf("Ctrl+O with nothing unsaved returned %q, want open_ffmpeg", action)
	}

	m = open(t)
	m.dirty = true // an edit elsewhere in the panel
	if action := m.HandleKey(keyCtrlO); action != "" || !m.closeConfirm {
		t.Fatalf("Ctrl+O with unsaved edits returned %q (prompt=%v), want the save prompt", action, m.closeConfirm)
	}
	if action := m.HandleKey(keyEsc); action != "" || m.closeConfirm || !m.IsVisible() {
		t.Errorf("Esc at the prompt returned %q, prompt=%v visible=%v; want it to stay in Settings", action, m.closeConfirm, m.IsVisible())
	}
	// Esc dropped the pending installer: closing normally now just closes.
	m.HandleKey(keyCtrlO)
	if action := m.HandleKey("n"); action != "open_ffmpeg" {
		t.Errorf("Discard at the prompt returned %q, want open_ffmpeg", action)
	}
	if m.dirty || m.IsVisible() {
		t.Errorf("after Discard: dirty=%v visible=%v, want a clean, closed panel", m.dirty, m.IsVisible())
	}
}

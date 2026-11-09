package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// findInView returns the cell of the first occurrence of s in the rendered
// overlay, at or below row from, or (-1,-1).
func findInView(view, s string, from int) (int, int) {
	for row, line := range strings.Split(view, "\n") {
		if row < from {
			continue
		}
		if before, _, ok := strings.Cut(line, s); ok {
			return len([]rune(before)), row
		}
	}
	return -1, -1
}

// TestSettingsClicksDoNotLeaveAnOpenSubEditor is K26: the key path routes
// every key to an open sub-editor (channel editor, channel-removal prompt,
// notification editor or delete confirm, security set/remove prompt), so
// Tab cannot switch section and Esc/Enter answer the editor. The mouse
// ignored those modes: a click on a header tab switched section with the
// editor still half-finished behind it, and a click on Save/Return closed the
// overlay — saving or dropping a half-typed password — without a word.
//
// Mutant: drop the subEditorActive guard — the tab click switches section,
// or the button click closes the overlay.
func TestSettingsClicksDoNotLeaveAnOpenSubEditor(t *testing.T) {
	cases := []struct {
		name    string
		section string
		open    func(m *SettingsModel)
		still   func(m *SettingsModel) bool
	}{
		{"channel editor", "Channels", func(m *SettingsModel) { m.HandleKey("a") },
			func(m *SettingsModel) bool { return m.channelMode == "edit" }},
		{"channel removal prompt", "Channels", func(m *SettingsModel) {
			m.channels = append(m.channels, config.ChannelConfig{ID: "UCxxxxxxxxxxxxxxxxxxxxxx", Name: "Chan"})
			m.channelIndex = 0
			m.beginChannelRemoval()
		}, func(m *SettingsModel) bool { return m.channelDeleteConf }},
		{"notification editor", "Integrations", func(m *SettingsModel) { m.HandleKey("a") },
			func(m *SettingsModel) bool { return m.notifMode == "edit" }},
		{"notification delete confirm", "Integrations", func(m *SettingsModel) {
			m.notifications = append(m.notifications, config.NotificationConfig{URL: "https://example.invalid/hook"})
			m.notifIndex = 0
			m.notifDeleteConf = true
		}, func(m *SettingsModel) bool { return m.notifDeleteConf }},
		{"security set prompt", "Network", func(m *SettingsModel) { m.secMode = securitySet },
			func(m *SettingsModel) bool { return m.secMode == securitySet }},
		{"security remove prompt", "Network", func(m *SettingsModel) { m.secMode = securityRemove },
			func(m *SettingsModel) bool { return m.secMode == securityRemove }},
	}
	for _, tc := range cases {
		for _, target := range []string{"tab", "button"} {
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				cfg := config.Defaults()
				m := NewSettingsModel()
				m.configStore = config.NewStore(cfg, "")
				m.cfg = cfg
				m.SetSize(200, 50)
				m.Open(cfg)
				m.switchSection(sectionIndexByName(t, tc.section))
				tc.open(m)
				if !tc.still(m) {
					t.Fatalf("premise lost: the %s did not open", tc.name)
				}
				m.dirty = true // Save & Return is the button drawn
				view := stripANSI(m.View())
				before := m.sectionIndex

				var x, y int
				if target == "tab" {
					other := "Logs"
					x, y = findInView(view, other, 0)
				} else {
					x, y = findInView(view, "[ Save & Return ]", 0)
					if y < 0 {
						x, y = findInView(view, "[ Return ]", 0)
					}
					if y < 0 {
						t.Skipf("no button drawn under the %s", tc.name)
					}
					x += 2
					// The click must land on the row the hit-test treats as the
					// buttons, or it never reaches the guard: a channel-editor
					// row wider than the box wrapped and pushed the drawn
					// buttons a row below lastButtonContentY.
					if got := y - settingsBoxTop(m) - 1 - 2; got != m.lastButtonContentY {
						t.Fatalf("buttons drawn at contentY %d, hit-test expects %d:\n%s", got, m.lastButtonContentY, view)
					}
				}
				if y < 0 {
					t.Fatalf("target %s not drawn:\n%s", target, view)
				}
				m.HandleMouse(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})

				if !m.IsVisible() {
					t.Fatalf("the %s click closed the overlay with the %s open", target, tc.name)
				}
				if m.sectionIndex != before {
					t.Errorf("the %s click switched to %q with the %s open", target, sections[m.sectionIndex].name, tc.name)
				}
				if !tc.still(m) {
					t.Errorf("the %s click dropped the %s", target, tc.name)
				}
			})
		}
	}

	// The guard is only for sub-editors: with none open, the same tab click
	// still switches section.
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.SetSize(200, 50)
	m.Open(cfg)
	m.switchSection(sectionIndexByName(t, "Channels"))
	x, y := findInView(stripANSI(m.View()), "Logs", 0)
	m.HandleMouse(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if sections[m.sectionIndex].name != "Logs" {
		t.Errorf("with no sub-editor open, the tab click opened %q, want Logs", sections[m.sectionIndex].name)
	}
}

// settingsBoxTop is the overlay box's top row, as HandleMouse computes it.
func settingsBoxTop(m *SettingsModel) int {
	h := max(m.height-2, 10)
	return max((m.height-(h+2))/2, 0)
}

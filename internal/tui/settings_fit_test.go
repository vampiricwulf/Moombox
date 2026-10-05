package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestSettingsFieldSectionsFitTheScreen focuses every field of every field
// section at the two supported floor sizes and checks the frame fits. The
// focused field's help line (up to ~170 characters) was appended as one row
// the content budget counted once, and the box word-wrapped it into two or
// three: at 80x24 "Segment workers" drew 25 lines, and bubbletea drops the
// overflow from the TOP — the header and tab strip went first.
//
// Mutant: dropping infoRows from settingsContentHeight — the long-help fields
// overflow again.
func TestSettingsFieldSectionsFitTheScreen(t *testing.T) {
	for _, size := range [][2]int{{60, 20}, {80, 24}} {
		w, h := size[0], size[1]
		m := newSettingsModelForSave(t)
		m.SetSize(w, h)
		for si, sec := range sections {
			if sec.fields == nil {
				continue // Channels / Integrations render their own lists
			}
			for fi := range sec.fields {
				m.sectionIndex, m.fieldIndex, m.scrollOffset = si, fi, 0
				m.ensureFieldVisible()
				view := m.View()
				where := fmt.Sprintf("%dx%d %s/%s", w, h, sec.name, sec.fields[fi].label)
				if got := lipgloss.Height(view); got > h {
					t.Errorf("%s: frame is %d rows on a %d-row screen", where, got, h)
				}
				if !strings.Contains(view, "Settings") {
					t.Errorf("%s: the header is not on screen", where)
				}
				if !strings.Contains(view, sec.fields[fi].label) {
					t.Errorf("%s: the focused field is not on screen", where)
				}
			}
		}
	}
}

// TestSettingsListRowsStayOneRowWide fills Channels and Integrations with the
// widest rows they can draw. Each row was built from fixed clips — 92 cells
// for a disabled channel with a filter, 118+ for a muted webhook with a
// mention in edit mode — and the box word-wrapped every one of them, so a
// full list overflowed by a dozen rows (the header dropped off the top) and
// listWindowStart's one-row-per-entry window and the mouse map were wrong.
// The flags that matter (disabled, Muted) must survive the cut.
//
// Mutant: dropping the ansi.Truncate on either row — the frame overflows.
func TestSettingsListRowsStayOneRowWide(t *testing.T) {
	off := false
	cfg := config.Defaults()
	for i := range 14 {
		cfg.Channels = append(cfg.Channels, config.ChannelConfig{
			ID:       fmt.Sprintf("UCabcdefghijklmnopqrstu%d", i),
			Name:     fmt.Sprintf("A Long Channel Name %02d", i),
			Platform: "youtube",
			Enabled:  &off,
			Terms:    config.ChannelTerms{Simple: "(?i)karaoke|singing|utawaku"},
		})
	}
	for i := range 10 {
		cfg.Notifications = append(cfg.Notifications, config.NotificationConfig{
			URL:     fmt.Sprintf("https://discord.com/api/webhooks/1234567890123456%02d/abcdefghijklmnopqrstuvwxyz", i),
			Enabled: &off,
			Events:  []string{"finished", "error"},
			Mention: "<@&123456789012345678>",
			Mode:    "edit",
		})
	}
	for _, size := range [][2]int{{60, 20}, {80, 24}} {
		w, h := size[0], size[1]
		for _, tc := range []struct{ section, flag string }{{"Channels", "(disabled)"}, {"Integrations", "Muted"}} {
			m := NewSettingsModel()
			m.configStore = config.NewStore(cfg, "")
			m.Open(cfg)
			m.SetSize(w, h)
			m.sectionIndex = sectionIndexByName(t, tc.section)
			view := m.View()
			where := fmt.Sprintf("%dx%d %s", w, h, tc.section)
			if got := lipgloss.Height(view); got > h {
				t.Errorf("%s: frame is %d rows on a %d-row screen", where, got, h)
			}
			if !strings.Contains(view, "Settings") {
				t.Errorf("%s: the header is not on screen", where)
			}
			if !strings.Contains(view, tc.flag) {
				t.Errorf("%s: no row shows %q any more", where, tc.flag)
			}
		}
	}
}

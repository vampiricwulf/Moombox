package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func newHeaderTestSettings() *SettingsModel {
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.Open(cfg)
	return m
}

// TestSettingsHeaderKeepsActiveSectionVisible: the twelve section names need
// about 140 cells, and the header used to be one line cut at the box edge, so
// on an 80-column terminal Updates through Integrations were off screen even
// while active, and the "N/12" counter always was. Now every section, stepped
// to from the left and back from the right, is on screen when active, and the
// counter is drawn at the right edge.
//
// Mutant: render every tab from 0 again (start pinned to 0) — Updates and
// everything after it are missing at width 72.
func TestSettingsHeaderKeepsActiveSectionVisible(t *testing.T) {
	m := newHeaderTestSettings()
	for _, w := range []int{72, 100} {
		check := func(i int) {
			t.Helper()
			m.switchSection(i)
			header := stripANSI(m.renderHeader(w))
			if got := lipgloss.Width(header); got > w {
				t.Errorf("w=%d, %s active: header is %d cells wide:\n%s", w, sections[i].name, got, header)
			}
			if !strings.Contains(header, sections[i].name) {
				t.Errorf("w=%d: active section %q is not in the header:\n%s", w, sections[i].name, header)
			}
			counter := fmt.Sprintf("%d/%d", i+1, len(sections))
			if !strings.HasSuffix(header, counter) {
				t.Errorf("w=%d: header does not end with the counter %q:\n%s", w, counter, header)
			}
		}
		for i := range sections {
			check(i)
		}
		for i := len(sections) - 1; i >= 0; i-- {
			check(i)
		}
	}
}

// TestSettingsHeaderShowsEveryTabWhenTheyFit: a terminal wide enough for the
// whole strip gets it, with no ‹ / › markers.
func TestSettingsHeaderShowsEveryTabWhenTheyFit(t *testing.T) {
	m := newHeaderTestSettings()
	m.switchSection(len(sections) - 1)
	header := stripANSI(m.renderHeader(200))
	for _, sec := range sections {
		if !strings.Contains(header, sec.name) {
			t.Errorf("section %q missing from a 200-cell header:\n%s", sec.name, header)
		}
	}
	if strings.ContainsAny(header, "\u2039\u203a") {
		t.Errorf("a header that fits shows a hidden-tab marker:\n%s", header)
	}
}

// TestSettingsHeaderWindowSlidesOneStep: moving one section past the window's
// edge slides it by one, not to a re-centred position — the tabs a user just
// passed stay where they were.
//
// Mutant: ignore prevStart (start from the active tab every render) — stepping
// right from Network drops every tab left of the active one.
func TestSettingsHeaderWindowSlidesOneStep(t *testing.T) {
	const avail = 60
	start, end := settingsTabWindow(0, 0, avail)
	if start != 0 {
		t.Fatalf("first window starts at %d, want 0", start)
	}
	for active := 1; active < len(sections); active++ {
		prevStart, prevEnd := start, end
		start, end = settingsTabWindow(prevStart, active, avail)
		if active < start || active >= end {
			t.Fatalf("active %d outside window [%d,%d)", active, start, end)
		}
		if active < prevEnd && start != prevStart {
			t.Errorf("active %d was already visible in [%d,%d) but the window moved to [%d,%d)",
				active, prevStart, prevEnd, start, end)
		}
		if active == prevEnd && end != active+1 && end != len(sections) {
			t.Errorf("stepping past the edge to %d gave [%d,%d); want it to be the last tab shown",
				active, start, end)
		}
	}
}

// TestSettingsHeaderClickFollowsTheWindow: clicks map through the window the
// header drew, and the ‹ / › markers open the hidden neighbour.
//
// Mutant: hit-test from section 0 as before — a click on the first visible
// tab of a slid window opens Network.
func TestSettingsHeaderClickFollowsTheWindow(t *testing.T) {
	m := newHeaderTestSettings()
	const w = 72
	m.switchSection(len(sections) - 2) // Channels
	header := stripANSI(m.renderHeader(w))
	if m.headerTabStart == 0 {
		t.Fatalf("premise lost: the window did not slide at width %d:\n%s", w, header)
	}

	// Every visible tab, clicked on its first cell.
	for i := m.headerTabStart; i < m.headerTabEnd; i++ {
		x := strings.Index(header, sections[i].name)
		if x < 0 {
			t.Fatalf("tab %q not drawn:\n%s", sections[i].name, header)
		}
		x = lipgloss.Width(header[:x])
		m.handleMouseTabClick(x)
		if m.sectionIndex != i {
			t.Errorf("click at x=%d opened %q, want %q", x, sections[m.sectionIndex].name, sections[i].name)
		}
		m.switchSection(len(sections) - 2)
		m.renderHeader(w)
	}

	// The ‹ marker opens the section just left of the window.
	hidden := m.headerTabStart - 1
	m.handleMouseTabClick(settingsHeaderPrefixW)
	if m.sectionIndex != hidden {
		t.Errorf("click on ‹ opened %q, want %q", sections[m.sectionIndex].name, sections[hidden].name)
	}
}

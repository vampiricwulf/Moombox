package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Status-bar density tiers, richest to poorest. Both halves of the bar
// carry a tier ladder and View picks the richest pair that actually fits
// the measured width (see fitTiers), so the bar adapts to real content —
// a long backfill channel name, "12 selected", OFFLINE — instead of to a
// guessed column count. The previous rule was a cliff: one width
// threshold flipped both halves between two renderings, and any remaining
// overflow dropped the ENTIRE chord-hint half, so a narrow window showed
// no keybinds at all.
type barTier int

const (
	tierFull      barTier = iota // full labels: "Tab Focus", "Disk 45% (120G free)"
	tierCompact                  // shortened labels: "Tab", "D:45% 120G"
	tierKeys                     // chords lose their names; right keeps alerts + counts
	tierTight                    // chords space-separated; right keeps alerts only
	tierEssential                // only the chords that lead everywhere else; right only if warning
	tierNone                     // half omitted entirely — last resort
)

// Package-level styles for status bar rendering (avoid alloc per render).
var (
	statusBarBgStyle  = lipgloss.NewStyle().Background(lipgloss.Color("#1a1a2e")).Foreground(ColorWhite)
	statusBarKeyStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	statusBarRedStyle = lipgloss.NewStyle().Foreground(ColorRed)
	statusBarGrnStyle = lipgloss.NewStyle().Foreground(ColorGreen)
	statusBarWrnStyle = lipgloss.NewStyle().Foreground(ColorWarning)
	statusBarYelStyle = lipgloss.NewStyle().Foreground(ColorYellow)
)

// CookieStatus represents the authentication state for a platform.
type CookieStatus int

const (
	CookieStatusNone CookieStatus = iota
	CookieStatusOK
	CookieStatusCookiesOnly
	CookieStatusRelogin
)

// StatusBarModel renders the bottom status bar.
type StatusBarModel struct {
	width    int
	ytCookie CookieStatus
	twCookie CookieStatus
	ytActive bool
	twActive bool
	// Jobs list for detecting COOKIES? status (B1)
	jobs []*database.Job
	// Disk status
	diskFree    uint64
	diskUsedPct float64
	diskWarn    string // "ok", "warn", "critical"
	// In-flight backfill scan (spec §11 progress surfacing). Scans are
	// strictly serial, so one slot IS the whole "scanning" state; empty
	// backfillChannel hides the indicator. Terminal states clear the slot —
	// done/idle are quiet by design and error is already loud in the log
	// panel (the R B overlay owns detailed per-channel state).
	backfillChannel string // channel ID of the in-flight scan ("" = none)
	backfillName    string // display name resolved by the App handler
	backfillTab     string
	backfillPages   int
	// ShowChordHint shows a newcomer hint instead of the full chord reference.
	ShowChordHint bool
	// SelectedCount tracks batch-selected jobs for status bar display.
	SelectedCount int
	// offline indicates that internet connectivity is unavailable.
	offline bool
}

// NewStatusBarModel creates a new status bar model.
func NewStatusBarModel() *StatusBarModel {
	return &StatusBarModel{diskWarn: "ok"}
}

// SetWidth updates the bar width.
func (m *StatusBarModel) SetWidth(w int) {
	m.width = w
}

// SetCookieStatus sets the cookie status for a platform.
func (m *StatusBarModel) SetCookieStatus(yt, tw CookieStatus) {
	m.ytCookie = yt
	m.twCookie = tw
}

// SetActivePlatforms sets which platform indicators are visible.
func (m *StatusBarModel) SetActivePlatforms(yt, tw bool) {
	m.ytActive = yt
	m.twActive = tw
}

// SetJobs updates the jobs reference for COOKIES? detection (B1).
func (m *StatusBarModel) SetJobs(jobs []*database.Job) {
	m.jobs = jobs
}

// SetDiskStatus updates the disk space display.
func (m *StatusBarModel) SetDiskStatus(free uint64, usedPct float64, warn string) {
	m.diskFree = free
	m.diskUsedPct = usedPct
	m.diskWarn = warn
}

// SetBackfillStatus updates the backfill scan indicator from a
// BackfillStatusMsg. "scanning" occupies the single slot; any other state
// clears it — but only for the channel that owns it, so a seeded terminal
// state for another channel can't blank an in-flight scan's display.
func (m *StatusBarModel) SetBackfillStatus(chID, name, tab string, pages int, state string) {
	if state == "scanning" {
		m.backfillChannel = chID
		m.backfillName = name
		m.backfillTab = tab
		m.backfillPages = pages
		return
	}
	if m.backfillChannel == chID {
		m.backfillChannel = ""
		m.backfillName = ""
		m.backfillTab = ""
		m.backfillPages = 0
	}
}

// View renders the status bar.
func (m *StatusBarModel) View() string {
	if m.width <= 0 {
		return ""
	}

	left, right := m.fitTiers()

	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)

	// At least one column between the halves so they never abut; fitTiers
	// budgeted for it. max() also keeps Repeat's count non-negative in the
	// pathological case where even tierNone/tierNone overflows (a width of
	// 1-2 columns), which the MaxWidth clamp below then trims.
	padding := max(m.width-leftW-rightW, 1)

	bar := left + strings.Repeat(" ", padding) + right

	if barW := lipgloss.Width(bar); barW < m.width {
		bar += strings.Repeat(" ", m.width-barW)
	}

	// MaxWidth is the hard guard: an over-wide bar used to WRAP, pushing a
	// second line into the terminal and corrupting the frame below it.
	// Clamping (ANSI-aware, so styled runs are cut without severing escape
	// sequences) degrades to a clipped bar instead of a broken layout.
	return statusBarBgStyle.MaxWidth(m.width).Render(bar)
}

// fitTiers picks the richest (left, right) tier pair whose combined width
// fits m.width, including the one-column gap between them.
//
// The right half yields first and the two then alternate, so a narrow
// window loses status verbosity before it loses keybinds — but neither
// side is amputated outright. Both ladders end at tierNone, so the loop
// always terminates, with an empty bar in the degenerate case.
func (m *StatusBarModel) fitTiers() (string, string) {
	lefts := m.controlTiers()
	rights := m.metricTiers()

	fits := func(l, r barTier) bool {
		return lipgloss.Width(lefts[l])+1+lipgloss.Width(rights[r]) <= m.width
	}

	li, ri := tierFull, tierFull
	for !fits(li, ri) {
		switch {
		// The RIGHT half yields first, then the two alternate (ri <= li
		// flips the turn each step). The chord hints are the reason this
		// bar exists — losing "Tab Focus" costs the operator a keybind
		// they may not know, while losing "Backfill Foo: videos p3" costs
		// them a detail they can read in the log panel. The li == tierNone
		// arm lets the right keep degrading alone once the left is spent.
		case ri < tierNone && (ri <= li || li == tierNone):
			ri++
		case li < tierNone:
			li++
		default:
			// Even tierNone/tierNone overflows (a 1-2 column window).
			// View's MaxWidth clamp is the backstop.
			return lefts[tierNone], rights[tierNone]
		}
	}

	// Climb back. Alternating descent can OVERSHOOT: it steps one side down
	// when the other side's step alone would have freed enough room, so the
	// first fitting pair is not always the richest fitting pair (at 120
	// columns the chord hints lost their labels with 37 columns still
	// empty). Re-upgrade greedily against the width that is actually left,
	// chord hints first — same priority as the descent, so leftover room
	// buys back keybind names before it buys back status verbosity.
	for li > tierFull && fits(li-1, ri) {
		li--
	}
	for ri > tierFull && fits(li, ri-1) {
		ri--
	}
	return lefts[li], rights[ri]
}

// controlTiers renders the chord hints at every density, richest first,
// indexed by barTier. Every tier keeps the leading space that separates the
// hints from the terminal edge.
//
// The ladder drops, in order: the long labels on the single-glyph chords
// ("Tab Focus" → "Tab"), then the remaining chord names ("A Action" → "A"),
// then the pipe separators, then all but the two chords that reach every
// other feature — M opens the menu and ? opens help, so those two survive
// longest. Only tierNone hides the keybinds outright, and fitTiers reaches
// it only when the window cannot seat even a single glyph plus the gap.
func (m *StatusBarModel) controlTiers() []string {
	if m.ShowChordHint {
		// Newcomer hint: same ladder shape, prose instead of chords.
		return []string{
			" " + DimStyle.Render("Press ? for help · M for menu · A for actions"),
			" " + DimStyle.Render("? help · M menu · A actions"),
			" " + DimStyle.Render("? help · M menu"),
			" " + DimStyle.Render("? for help"),
			" " + DimStyle.Render("?"),
			"",
		}
	}

	key := statusBarKeyStyle
	named := []string{
		key.Render("A") + " Action",
		key.Render("R") + " Request",
		key.Render("O") + " Open",
		key.Render("F") + " Filter",
		key.Render("M") + " Menu",
	}
	glyphs := []string{key.Render("Tab"), key.Render("`"), key.Render("?")}
	bare := []string{
		key.Render("A"), key.Render("R"), key.Render("O"),
		key.Render("F"), key.Render("M"),
	}

	full := append(append([]string{}, named...),
		key.Render("Tab")+" Focus",
		key.Render("`")+" Settings",
		key.Render("?")+" Help",
	)
	compact := append(append([]string{}, named...), glyphs...)
	keysOnly := append(append([]string{}, bare...), glyphs...)

	return []string{
		" " + strings.Join(full, " | "),
		" " + strings.Join(compact, " | "),
		" " + strings.Join(keysOnly, " | "),
		" " + strings.Join(keysOnly, " "),
		" " + key.Render("M") + " " + key.Render("?"),
		"",
	}
}

// metricTiers renders the right half (metrics + auth indicators) at every
// density, richest first, indexed by barTier.
//
// What survives longest is what the operator cannot afford to miss:
// OFFLINE, a disk warning, and a re-login prompt are alerts, while the
// backfill scan, the selection count, and the active-download tally are
// informational. So the informational items drop first, then the routine
// (non-warning) disk and cookie readouts, leaving a bar that is silent
// when everything is healthy and still shouts when it isn't.
func (m *StatusBarModel) metricTiers() []string {
	tiers := make([]string, tierNone+1)
	for t := tierFull; t <= tierNone; t++ {
		tiers[t] = m.renderMetrics(t) + m.renderCookieStatus(t)
	}
	return tiers
}

// renderMetrics renders disk usage and activity indicators at tier t.
func (m *StatusBarModel) renderMetrics(t barTier) string {
	if t >= tierNone {
		return ""
	}
	var parts []string

	// Connectivity indicator — an alert, so it outlives every counter and
	// only abbreviates.
	if m.offline {
		if t >= tierTight {
			parts = append(parts, statusBarRedStyle.Render("OFF"))
		} else {
			parts = append(parts, statusBarRedStyle.Render("OFFLINE"))
		}
	}

	// Batch selection count — informational; the count itself is the point,
	// so it abbreviates to a bare number before disappearing.
	if m.SelectedCount > 0 && t <= tierKeys {
		if t == tierKeys {
			parts = append(parts, statusBarYelStyle.Render(fmt.Sprintf("%d sel", m.SelectedCount)))
		} else {
			parts = append(parts, statusBarYelStyle.Render(fmt.Sprintf("%d selected", m.SelectedCount)))
		}
	}

	// Backfill scan in flight (green, like the Active indicator — a routine
	// background activity, not a warning), so it is the first thing dropped.
	if m.backfillChannel != "" && t <= tierCompact {
		if t == tierCompact {
			parts = append(parts, statusBarGrnStyle.Render(
				fmt.Sprintf("BF:%s p%d", m.backfillTab, m.backfillPages)))
		} else {
			name := m.backfillName
			if r := []rune(name); len(r) > 16 {
				name = string(r[:15]) + "…"
			}
			parts = append(parts, statusBarGrnStyle.Render(
				fmt.Sprintf("Backfill %s: %s p%d", name, m.backfillTab, m.backfillPages)))
		}
	}

	// Disk indicator (only shown once we have data). At tierEssential only a
	// warning/critical reading is worth the columns — a healthy disk says
	// nothing there.
	warning := m.diskWarn == "warn" || m.diskWarn == "critical"
	if (m.diskFree > 0 || m.diskUsedPct > 0) && (t < tierEssential || warning) {
		var style lipgloss.Style
		switch m.diskWarn {
		case "critical":
			style = statusBarRedStyle
		case "warn":
			style = statusBarWrnStyle
		default:
			style = statusBarGrnStyle
		}

		freeGB := float64(m.diskFree) / (1024 * 1024 * 1024)
		pct := int(m.diskUsedPct)
		switch {
		case t >= tierTight:
			parts = append(parts, style.Render(fmt.Sprintf("D:%d%%", pct)))
		case t >= tierCompact:
			parts = append(parts, style.Render(fmt.Sprintf("D:%d%% %.0fG", pct, freeGB)))
		default:
			parts = append(parts, style.Render(fmt.Sprintf("Disk %d%% (%.0fG free)", pct, freeGB)))
		}
	}

	// Active download count (StatusQueued is deliberately absent — a queued
	// job is waiting for an archive slot, not an active download).
	if t <= tierKeys {
		activeCount := 0
		for _, j := range m.jobs {
			switch j.Status {
			case database.StatusDownloading, database.StatusLive, database.StatusMuxing:
				activeCount++
			}
		}
		if activeCount > 0 {
			if t >= tierCompact {
				parts = append(parts, statusBarGrnStyle.Render(fmt.Sprintf("▶%d", activeCount)))
			} else {
				parts = append(parts, statusBarGrnStyle.Render(fmt.Sprintf("Active: %d", activeCount)))
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// renderCookieStatus renders auth indicators and warnings (B2) at tier t.
//
// A platform whose auth is HEALTHY is dropped at tierEssential — the green
// "YT" is reassurance, not information — while a re-login prompt or a
// rejected-cookie indicator survives to the last tier, abbreviated. That
// keeps the narrowest useful bar showing only what needs acting on.
func (m *StatusBarModel) renderCookieStatus(t barTier) string {
	if t >= tierNone || (!m.ytActive && !m.twActive) {
		return ""
	}

	var parts []string

	// Check if any job has COOKIES? status (B1)
	cookiesRejected := false
	for _, j := range m.jobs {
		if j.Status == database.StatusCookies {
			cookiesRejected = true
			break
		}
	}

	// healthy reports whether a platform's indicator is pure reassurance —
	// dropped once space is scarce (tierEssential).
	healthy := t < tierEssential

	// YouTube status (only if active)
	if m.ytActive {
		switch {
		case m.ytCookie == CookieStatusRelogin:
			if t >= tierTight {
				parts = append(parts, statusBarRedStyle.Render("YT!"))
			} else {
				parts = append(parts, statusBarRedStyle.Render("YT: Re-login"))
			}
		case cookiesRejected || m.ytCookie == CookieStatusCookiesOnly:
			parts = append(parts, statusBarRedStyle.Render("YT"))
		case m.ytCookie == CookieStatusNone:
			if healthy {
				parts = append(parts, statusBarYelStyle.Render("YT"))
			}
		case m.ytCookie == CookieStatusOK:
			if healthy {
				parts = append(parts, statusBarGrnStyle.Render("YT"))
			}
		default:
			if healthy {
				parts = append(parts, DimStyle.Render("YT"))
			}
		}
	}

	// Twitch status (only if active)
	if m.twActive {
		switch m.twCookie {
		case CookieStatusRelogin:
			if t >= tierTight {
				parts = append(parts, statusBarRedStyle.Render("TW!"))
			} else {
				parts = append(parts, statusBarRedStyle.Render("TW: Re-login"))
			}
		case CookieStatusCookiesOnly:
			parts = append(parts, statusBarRedStyle.Render("TW"))
		case CookieStatusOK:
			if healthy {
				parts = append(parts, statusBarGrnStyle.Render("TW"))
			}
		default:
			if healthy {
				parts = append(parts, DimStyle.Render("TW"))
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

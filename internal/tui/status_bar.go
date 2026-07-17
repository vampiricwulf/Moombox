package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

const statusBarCompactThreshold = 100

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

	bg := statusBarBgStyle

	left := m.renderControls()
	right := m.renderMetrics() + m.renderCookieStatus()

	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)

	// If both sides don't fit, drop the left controls so the disk/cookie
	// indicators stay visible (mirrors the task-list header overflow drop).
	if leftW+1+rightW > m.width {
		left = ""
		leftW = 0
	}

	padding := max(m.width-leftW-rightW, 1)

	bar := left + strings.Repeat(" ", padding) + right

	barW := lipgloss.Width(bar)
	if barW < m.width {
		bar += strings.Repeat(" ", m.width-barW)
	}

	return bg.Render(bar)
}

// renderControls renders uniform chord hints in the status bar.
func (m *StatusBarModel) renderControls() string {
	if m.ShowChordHint {
		return " " + DimStyle.Render("Press ? for help · M for menu · A for actions")
	}

	compact := m.width < statusBarCompactThreshold
	key := statusBarKeyStyle

	var parts []string
	if compact {
		parts = append(parts,
			key.Render("A")+" Action",
			key.Render("R")+" Request",
			key.Render("O")+" Open",
			key.Render("F")+" Filter",
			key.Render("M")+" Menu",
			key.Render("Tab"),
			key.Render("`"),
			key.Render("?"),
		)
	} else {
		parts = append(parts,
			key.Render("A")+" Action",
			key.Render("R")+" Request",
			key.Render("O")+" Open",
			key.Render("F")+" Filter",
			key.Render("M")+" Menu",
			key.Render("Tab")+" Focus",
			key.Render("`")+" Settings",
			key.Render("?")+" Help",
		)
	}
	return " " + strings.Join(parts, " | ")
}

// renderMetrics renders disk usage and active download count indicators.
func (m *StatusBarModel) renderMetrics() string {
	var parts []string
	compact := m.width < statusBarCompactThreshold

	// Connectivity indicator
	if m.offline {
		parts = append(parts, statusBarRedStyle.Render("OFFLINE"))
	}

	// Batch selection count
	if m.SelectedCount > 0 {
		selText := fmt.Sprintf("%d selected", m.SelectedCount)
		parts = append(parts, statusBarYelStyle.Render(selText))
	}

	// Backfill scan in flight (green, like the Active indicator — a routine
	// background activity, not a warning). Compact drops the channel name.
	if m.backfillChannel != "" {
		if compact {
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

	// Disk indicator (only shown once we have data)
	if m.diskFree > 0 || m.diskUsedPct > 0 {
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
		if compact {
			parts = append(parts, style.Render(fmt.Sprintf("D:%d%% %.0fG", pct, freeGB)))
		} else {
			parts = append(parts, style.Render(fmt.Sprintf("Disk %d%% (%.0fG free)", pct, freeGB)))
		}
	}

	// Active download count (StatusQueued is deliberately absent — a queued
	// job is waiting for an archive slot, not an active download)
	activeCount := 0
	for _, j := range m.jobs {
		switch j.Status {
		case database.StatusDownloading, database.StatusLive, database.StatusMuxing:
			activeCount++
		}
	}
	if activeCount > 0 {
		style := statusBarGrnStyle
		if compact {
			parts = append(parts, style.Render(fmt.Sprintf("▶%d", activeCount)))
		} else {
			parts = append(parts, style.Render(fmt.Sprintf("Active: %d", activeCount)))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// renderCookieStatus renders auth indicators and warnings (B2).
func (m *StatusBarModel) renderCookieStatus() string {
	if !m.ytActive && !m.twActive {
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

	// YouTube status (only if active)
	if m.ytActive {
		switch {
		case m.ytCookie == CookieStatusRelogin:
			parts = append(parts, statusBarRedStyle.Render("YT: Re-login"))
		case m.ytCookie == CookieStatusNone:
			parts = append(parts, statusBarYelStyle.Render("YT"))
		case cookiesRejected || m.ytCookie == CookieStatusCookiesOnly:
			parts = append(parts, statusBarRedStyle.Render("YT"))
		case m.ytCookie == CookieStatusOK:
			parts = append(parts, statusBarGrnStyle.Render("YT"))
		default:
			parts = append(parts, DimStyle.Render("YT"))
		}
	}

	// Twitch status (only if active)
	if m.twActive {
		switch m.twCookie {
		case CookieStatusRelogin:
			parts = append(parts, statusBarRedStyle.Render("TW: Re-login"))
		case CookieStatusCookiesOnly:
			parts = append(parts, statusBarRedStyle.Render("TW"))
		case CookieStatusOK:
			parts = append(parts, statusBarGrnStyle.Render("TW"))
		default:
			parts = append(parts, DimStyle.Render("TW"))
		}
	}

	return strings.Join(parts, " ") + " "
}

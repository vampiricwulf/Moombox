package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

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
	statusBarYelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))
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
	padding := m.width - leftW - rightW
	if padding < 1 {
		padding = 1
	}

	bar := left + strings.Repeat(" ", padding) + right

	barW := lipgloss.Width(bar)
	if barW < m.width {
		bar += strings.Repeat(" ", m.width-barW)
	}

	return bg.Render(bar)
}

// renderControls renders uniform chord hints in the status bar.
func (m *StatusBarModel) renderControls() string {
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

	// Active download count
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
		switch {
		case m.twCookie == CookieStatusRelogin:
			parts = append(parts, statusBarRedStyle.Render("TW: Re-login"))
		case m.twCookie == CookieStatusCookiesOnly:
			parts = append(parts, statusBarRedStyle.Render("TW"))
		case m.twCookie == CookieStatusOK:
			parts = append(parts, statusBarGrnStyle.Render("TW"))
		default:
			parts = append(parts, DimStyle.Render("TW"))
		}
	}

	return strings.Join(parts, " ") + " "
}

package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The bare form of Statistics, drawn below the normal layout
// (overlay_bare.go): a header row over a list of short "label value"
// figures, no border. The order is what a small screen is opened for: how
// full the disk is, what is running, then the totals. Two figures share a
// row from 40 columns where both fit. The list scrolls (arrows, PgUp/PgDn,
// the wheel) where it is taller than the screen, and the header then says
// where the reader is.
//
// An error is shown whole, wrapped, in place of the list.

// bareStatsSecondCol is the cell the second figure of a row starts at.
const bareStatsSecondCol = 21

// bareStatsPairWidth is the narrowest terminal that puts two figures on a row.
const bareStatsPairWidth = 40

// bare reports whether the overlay draws its bare form: its own size is
// below the normal layout. Derived on every call, never stored.
func (m *StatsDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// newBareStatsViewport is the viewport the bare list scrolls in, with the
// help overlay's keys: the arrows and the paging keys, no letters.
func newBareStatsViewport() viewport.Model {
	vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(1))
	vp.KeyMap = helpViewportKeyMap()
	vp.SoftWrap = false
	vp.FillHeight = true
	return vp
}

// bareDiskRows is the disk reading in the bare form: the percentage and the
// free space on the first row, with a bar between "Disk" and the percentage
// where there is room for one, and used-of-total on the second. With no
// reading it is the normal form's one-line notice.
func (m *StatsDialogModel) bareDiskRows(w int) []string {
	d := m.snap.Disk
	if d.Total == 0 {
		return []string{DimStyle.Render(truncateString("No disk reading available", w))}
	}
	used := max(int64(d.Total)-int64(d.Free), 0)
	level := ColorFinished
	switch d.WarnLevel {
	case "critical":
		level = ColorError
	case "warn":
		level = ColorWarning
	}
	pct := fmt.Sprintf("%.1f%%", d.UsedPct)
	free := "  " + utils.FormatFileSize(int64(d.Free)) + " free"
	first := "Disk "
	if barW := min(w-len(first)-1-len(pct)-len(free), 50); w >= bareStatsPairWidth && barW >= 4 {
		bar := m.bar
		bar.SetWidth(barW)
		bar.FullColor = level
		first += bar.ViewAs(d.UsedPct/100) + " "
	}
	first += lipgloss.NewStyle().Foreground(level).Render(pct) + free
	return []string{
		truncateString(first, w),
		truncateString(utils.FormatFileSize(used)+" used of "+utils.FormatFileSize(int64(d.Total)), w),
	}
}

// bareRows is the whole bare list for a terminal w cells wide.
func (m *StatsDialogModel) bareRows(w int) []string {
	rows := m.bareDiskRows(w)
	s := m.snap
	pairs := [][2][2]string{
		{{"Active", groupThousands(int64(s.ActiveDownloads))}, {"Muxing", groupThousands(int64(s.ActiveMuxing))}},
		{{"Recorded", utils.FormatFileSize(s.TotalSize)}, {"Jobs", groupThousands(int64(s.JobCount))}},
		{{"YouTube", utils.FormatFileSize(s.SizeByPlatform["youtube"])}, {"Twitch", utils.FormatFileSize(s.SizeByPlatform["twitch"])}},
		{{"Finished", utils.FormatFileSize(s.SizeByStatus["finished"])}, {"Error", utils.FormatFileSize(s.SizeByStatus["error"])}},
		{{"Cancelled", utils.FormatFileSize(s.SizeByStatus["cancelled"])}, {"Archived", groupThousands(int64(s.TotalFinished))}},
		{{"Rec time", formatHMS(s.TotalDuration)}, {"Chat", groupThousands(s.TotalChatMessages)}},
		{{"YT jobs", groupThousands(int64(s.CountByPlatform["youtube"]))}, {"TW jobs", groupThousands(int64(s.CountByPlatform["twitch"]))}},
	}
	for _, p := range pairs {
		left, right := p[0][0]+" "+p[0][1], p[1][0]+" "+p[1][1]
		if w >= bareStatsPairWidth && len(left) < bareStatsSecondCol && len(right) <= w-bareStatsSecondCol {
			rows = append(rows, left+strings.Repeat(" ", bareStatsSecondCol-len(left))+right)
			continue
		}
		rows = append(rows, truncateString(left, w), truncateString(right, w))
	}
	if s.Uptime > 0 {
		rows = append(rows, truncateString("Uptime "+formatHMS(int64(s.Uptime.Seconds())), w))
	}
	return rows
}

// syncBare sizes the bare viewport and fills it from the snapshot or the
// error. The reader's place is kept; the viewport clamps it.
func (m *StatsDialogModel) syncBare() {
	w := max(m.width, 1)
	m.bareVP.SetWidth(w)
	m.bareVP.SetHeight(max(m.height-1, 1)) // the header row
	var rows []string
	switch {
	case m.errorMsg != "":
		for _, line := range strings.Split(ansi.Wrap(m.errorMsg, w, ""), "\n") {
			rows = append(rows, ErrorStyle.Render(line))
		}
	case m.haveSnap:
		rows = m.bareRows(w)
	}
	top := m.bareVP.YOffset()
	m.bareVP.SetContentLines(rows)
	m.bareVP.SetYOffset(top)
}

// bareScroll moves the bare list by n rows (negative is up): the wheel's
// path. In the normal form there is nothing to scroll.
func (m *StatsDialogModel) bareScroll(n int) {
	if !m.bare() || n == 0 {
		return
	}
	if n < 0 {
		m.bareVP.ScrollUp(-n)
	} else {
		m.bareVP.ScrollDown(n)
	}
}

// bareUpdate feeds a key to the bare list's viewport.
func (m *StatsDialogModel) bareUpdate(msg tea.Msg) {
	if _, isKey := msg.(tea.KeyPressMsg); isKey {
		m.bareVP, _ = m.bareVP.Update(msg)
	}
}

// bareView draws the bare form: exactly width x height cells.
func (m *StatsDialogModel) bareView() string {
	left := TitleStyle.Render("Stats")
	keys := "R refresh · Esc close"
	if m.width < bareStatsPairWidth {
		keys = "R refresh·Esc"
	}
	var body []string
	if m.loading && !m.haveSnap && m.errorMsg == "" {
		body = []string{m.spinner.View() + " Loading..."}
	} else {
		if total := m.bareVP.TotalLineCount(); total > m.bareVP.Height() {
			left += " " + DimStyle.Render(fmt.Sprintf("%d/%d", m.bareVP.YOffset()+1, total))
		}
		body = strings.Split(m.bareVP.View(), "\n")
	}
	return bareSheet{left: left, right: DimStyle.Render(keys), body: body}.render(m.width, m.height)
}

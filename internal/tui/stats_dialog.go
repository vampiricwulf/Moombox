package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

// statsDialogMaxWidth is the content box's preferred width — wide enough for
// the disk bar plus a two-column stat row without cramming (see the sibling
// dialogs' 60-72 range in ffmpeg_check.go/setup_wizard.go/ytdlp_dialog.go).
const statsDialogMaxWidth = 76

// StatsDialogModel is the R T overlay: the Web Stats tab's disk bar, six
// storage figures, seven activity figures and uptime, from the same
// stats.Snapshot the /api/stats handler renders.
//
// It owns no textinput or list. App.Update's keypress arm calls
// routeComponentMsg AND handleKey for the same message, so like
// YtdlpDialogModel, UpdateComponents only ticks the spinner while loading —
// HandleKey decides on the key string alone and never feeds a component.
type StatsDialogModel struct {
	visible       bool
	width, height int
	loading       bool
	snap          stats.Snapshot
	haveSnap      bool
	errorMsg      string
	spinner       spinner.Model
	bar           progress.Model
}

// NewStatsDialogModel creates the statistics overlay.
func NewStatsDialogModel() *StatsDialogModel {
	pb := progress.New(progress.WithoutPercentage())
	pb.Full = '█'
	pb.Empty = '░'
	pb.EmptyColor = ColorGray
	return &StatsDialogModel{spinner: newSpinner(), bar: pb}
}

// IsVisible returns true if the dialog is shown.
func (m *StatsDialogModel) IsVisible() bool { return m.visible }

// SetSize updates the dialog dimensions and the disk bar's width to match.
func (m *StatsDialogModel) SetSize(w, h int) {
	m.width, m.height = w, h
	boxW, _ := dialogBox(statsDialogMaxWidth, w)
	m.bar.SetWidth(max(min(boxW-8, 50), 10))
}

// Open shows the overlay in its loading state and returns the spinner's
// first tick. The last snapshot stays on screen while a refresh is in
// flight — "Loading" only shows before the first SetSnapshot.
func (m *StatsDialogModel) Open() tea.Cmd {
	m.visible = true
	m.loading = true
	m.errorMsg = ""
	m.spinner = newSpinner()
	return spinnerTickCmd(m.spinner)
}

// Close hides the dialog.
func (m *StatsDialogModel) Close() { m.visible = false }

// SetSnapshot renders a freshly built snapshot, ending the loading wait.
func (m *StatsDialogModel) SetSnapshot(s stats.Snapshot) {
	m.snap, m.haveSnap = s, true
	m.loading = false
	m.errorMsg = ""
}

// SetError renders a message in place of the body and ends the loading wait.
func (m *StatsDialogModel) SetError(msg string) {
	m.loading = false
	m.errorMsg = msg
}

// UpdateComponents ticks the spinner while loading. Keys never come here —
// HandleKey decides on the key string alone (see the type comment).
func (m *StatsDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.loading {
		return nil
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return cmd
}

// HandleKey processes one keypress and returns "close", "refresh" or "".
func (m *StatsDialogModel) HandleKey(key string) string {
	switch key {
	case "esc", "q", "Q":
		m.Close()
		return "close"
	case "r", "R":
		return "refresh"
	}
	return ""
}

// View renders the overlay.
func (m *StatsDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW, _ := dialogBox(statsDialogMaxWidth, m.width)
	boxH := max(min(m.height-4, 34), 22)

	var b strings.Builder
	b.WriteString(TitleStyle.Render("Statistics"))
	b.WriteString("\n\n")
	switch {
	case m.loading && !m.haveSnap:
		fmt.Fprintf(&b, "  %s Loading...\n", m.spinner.View())
	case m.errorMsg != "":
		fmt.Fprintf(&b, "  %s\n", ErrorStyle.Render(m.errorMsg))
	default:
		m.writeStorage(&b)
		b.WriteString("\n")
		m.writeActivity(&b)
	}
	b.WriteString("\n")
	b.WriteString(DimStyle.Render("R: Refresh   Esc/Q: Close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH).
		Render(b.String())

	return centerBox(box, m.width, m.height)
}

// writeStorage is the Web Stats tab's Storage card, flattened to rows: the
// disk bar, a used/total/percent line, then the six size figures.
func (m *StatsDialogModel) writeStorage(b *strings.Builder) {
	d := m.snap.Disk
	fmt.Fprintf(b, "%s\n", HeaderStyle.Render("Storage"))

	used := int64(d.Total) - int64(d.Free)
	if used < 0 {
		used = 0
	}
	bar := m.bar
	switch d.WarnLevel {
	case "critical":
		bar.FullColor = ColorError
	case "warn":
		bar.FullColor = ColorWarning
	default:
		bar.FullColor = ColorFinished
	}
	fmt.Fprintf(b, "  %s\n", bar.ViewAs(d.UsedPct/100))
	fmt.Fprintf(b, "  %s used of %s (%.1f%%)\n", formatSize(used), formatSize(int64(d.Total)), d.UsedPct)

	writeStatRows(b, [][2]string{
		{"Total Recorded", formatSize(m.snap.TotalSize)},
		{"Total Jobs", groupThousands(int64(m.snap.JobCount))},
		{"YouTube Storage", formatSize(m.snap.SizeByPlatform["youtube"])},
		{"Twitch Storage", formatSize(m.snap.SizeByPlatform["twitch"])},
		{"Finished", formatSize(m.snap.SizeByStatus["finished"])},
		{"Error", formatSize(m.snap.SizeByStatus["error"])},
	})
}

// writeActivity is the Web Stats tab's Activity card, flattened the same
// way, plus Uptime — the one figure the Web derives client-side instead
// (stats.Snapshot doc comment) that the TUI has for free from its own
// process start time.
func (m *StatsDialogModel) writeActivity(b *strings.Builder) {
	fmt.Fprintf(b, "%s\n", HeaderStyle.Render("Activity"))

	rows := [][2]string{
		{"Streams Archived", groupThousands(int64(m.snap.TotalFinished))},
		{"Total Recording Time", formatHMS(m.snap.TotalDuration)},
		{"Chat Messages", groupThousands(m.snap.TotalChatMessages)},
		{"Active Downloads", groupThousands(int64(m.snap.ActiveDownloads))},
		{"Muxing", groupThousands(int64(m.snap.ActiveMuxing))},
		{"YouTube Jobs", groupThousands(int64(m.snap.CountByPlatform["youtube"]))},
		{"Twitch Jobs", groupThousands(int64(m.snap.CountByPlatform["twitch"]))},
	}
	if m.snap.Uptime > 0 {
		rows = append(rows, [2]string{"Uptime", formatUptime(m.snap.Uptime)})
	}
	writeStatRows(b, rows)
}

// writeStatRows prints one label/value pair per line (the Web's card grid,
// flattened for a terminal column instead of a wrapping grid of cards).
func writeStatRows(b *strings.Builder, rows [][2]string) {
	for _, r := range rows {
		fmt.Fprintf(b, "  %-22s %s\n", r[0], r[1])
	}
}

// formatSize renders bytes with the space-separated unit the stats overlay
// uses ("512.0 GB", "250.0 MB") — the same scale tiers as
// utils.FormatFileSize, whose concatenated form ("512.0GB") suits the
// inline per-file rows it was written for (job_details.go) but reads
// cramped next to this dialog's other space-separated stat rows.
func formatSize(bytes int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatHMS is the Web's formatDurationSeconds: "3h 25m 7s", "1m 0s", "59s".
func formatHMS(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	h, mins, s := seconds/3600, (seconds%3600)/60, seconds%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, mins, s)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, s)
	}
	return fmt.Sprintf("%ds", s)
}

// formatUptime: "2d 1h 30m", "5h 3m", "5m" — the one figure with no Web
// counterpart (stats.Snapshot doc comment: the Web derives uptime
// client-side from /api/status instead).
func formatUptime(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// groupThousands renders 1234567 as "1,234,567" (the Web's toLocaleString).
func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

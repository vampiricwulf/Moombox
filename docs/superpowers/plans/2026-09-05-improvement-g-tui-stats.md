# Arc G — TUI Statistics Overlay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `R T` Statistics overlay in the TUI that shows exactly what the Web dashboard's Stats tab shows — the disk bar, six storage figures, seven activity figures and uptime — from the same numbers, refreshed on the same cadence.

**Architecture:** One derivation source in a neutral package: `stats.Build(js *database.JobStats, d *stats.Disk) stats.Snapshot` (new `internal/stats`, importing only `internal/database`) holds the sums the `/api/stats` handler used to compute inline; the handler keeps its JSON byte-for-byte by rendering its map from the struct, and the TUI callback `App.OnGetStats` returns the same struct (plus uptime) straight from `db.GetJobStats()` and `routes.SharedDiskStatus`. `internal/tui` imports `internal/stats`, never `internal/web/routes` (Arc F's Fable review: the routes import drags `internal/bgutils/embed` into the TUI build graph, which fails on any blob-less checkout). The overlay `StatsDialogModel` follows the package's async-dialog shape (`client_tokens_dialog.go`: Open → spinner → `SetSnapshot`/`SetError` → three-way View) with a `bubbles/progress` disk bar, refreshes every 60 s while open (the Web's poll) and on `r`. Chord `R T` follows `R N`'s "ask the server, show an overlay" precedent. Text only — the Web tab has no charts.

**Tech Stack:** Go 1.27.1, Bubble Tea v2, `charm.land/bubbles/v2/progress` (already used in `job_details.go:72-76`), `charm.land/bubbles/v2/spinner`, `internal/utils/format.go`.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §9 (Arc G — "own brainstorm"); the brainstorm's survey and rulings G1–G5 are in `.superpowers/sdd/2026-09-05-improvement-g-tui-stats/design-notes.md`. Anchors below were read at main `27a6380`; Arc F (merged since) added sibling dialogs (`cookieImportDlg`, `ytdlpDlg`) to the same App files — anchor by TEXT (the sibling names), never by line.

## Global Constraints

- Worktree `D:/Git/Moombox/.worktrees/improvement-g-tui-stats`, branch `improvement-g-tui-stats`, cut from `main` after this plan's commit. Never push. Never `cd` to the main checkout. Git is read-only beyond `git add`/`git commit`; never edit a file you are not tasked with.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Implementers run only the packages their task names; the controller runs the one full `go test -count=1 ./...`.
- `gofmt -l ./cmd ./internal ./tools ./web` prints nothing; `go vet ./...` silent; `staticcheck ./...` (2026.2.1) prints nothing (hard CI gate). Go files stay LF (`perl -0777 -ne 'print tr/\r//' <file>` → 0). Define test helpers in the task that first USES them (an unused helper trips U1000).
- **`/api/stats` JSON is byte-for-byte unchanged** — keys, nesting, value types (`storage.byPlatform`/`byStatus` are `int64` bytes; `activity.byPlatform` are `int` counts; zeros when the DB query fails; `warnLevel` `"ok"` when no disk reading exists; `twitchHints` when a worker is wired). `internal/web/routes/stats_test.go` pins it and must pass unchanged.
- **Layering:** `internal/tui` must not import `internal/web/routes` (`go list -deps ./internal/tui/ | grep -c 'internal/web/routes\|internal/bgutils'` → 0; a blob-less `git archive` export must `go vet ./internal/tui/`). The snapshot type lives in `internal/stats`, which imports only `internal/database` (and stdlib).
- **Chord rules:** one `ActionMenuItem` in `buildMenuItems()` (`internal/tui/app_actions.go`) + one `dispatchAction` case; gated `if a.OnGetStats != nil` like `R I`/`R Y`; symmetric wired/unwired test; `TestHelpCoversEveryChord` green.
- **Every new full-screen dialog needs the four coordinated additions**, each placed beside the `ytdlpDlg` sibling: (1) App field + `NewApp()` construction + `hasActiveOverlay()`; (2) `App.View()` overlay chain in `app_layout.go`; (3) the `app_keys.go` dialog intercept (before the search intercepts) + `routeComponentMsg` in `app_update.go`; (4) result-message arms. **`HandleKey` never feeds a component** — `App.Update` runs `routeComponentMsg` AND `handleKey` for every key (Arc F Task 2's Critical); this dialog has only a spinner, so `UpdateComponents` ticks it and `HandleKey(key string)` decides on the key string alone.
- No new poll paths for disk: the snapshot reads `routes.SharedDiskStatus.Load()` in `cmd/moombox` (refreshed by main.go's ticker). The stats query is cached 5 s in the DB layer; the overlay never refreshes faster than 60 s.
- Docs cited by `internal/docs/citations_test.go` must resolve. Both trailers on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
  Write the message to a file and `git commit -F <file>` (never `-F -`).

---

### Task 1: `internal/stats` — `Snapshot`, `Disk`, `Build`; the route renders from the struct

**Files:**
- Create: `internal/stats/stats.go`, `internal/stats/stats_test.go`
- Modify: `internal/web/routes/stats.go:71-144` (`StatsRoutes` handler)
- Test: `internal/web/routes/stats_test.go` (existing tests unchanged)

**Interfaces:**
- Produces (package `stats`):
  ```go
  // Disk is the disk reading both UIs show — the same four facts
  // routes.DiskStatus carries; kept separate so this package never imports
  // the HTTP layer.
  type Disk struct {
  	Free     uint64
  	Total    uint64
  	UsedPct  float64
  	WarnLevel string // "ok", "warn", "critical"; "" when no reading exists yet
  }

  // Snapshot is every number the Web Stats tab and the TUI R T overlay show,
  // derived once here from the job aggregate and the disk reading.
  type Snapshot struct {
  	Disk Disk

  	TotalSize      int64            // finished + error + cancelled bytes
  	JobCount       int              // finished + error + cancelled + active + muxing (queued excluded, as the route always did)
  	SizeByPlatform map[string]int64 // "youtube", "twitch"
  	SizeByStatus   map[string]int64 // "finished", "error", "cancelled"

  	TotalFinished     int
  	TotalDuration     int64 // seconds
  	TotalChatMessages int64
  	ActiveDownloads   int
  	ActiveMuxing      int
  	CountByPlatform   map[string]int // "youtube", "twitch"

  	Uptime time.Duration // filled by the TUI callback only; the Web derives uptime client-side from /api/status
  }

  // Build derives a Snapshot. nil js (query failed) → zeros with the maps
  // present; nil d → the zero Disk.
  func Build(js *database.JobStats, d *Disk) Snapshot
  ```

- [ ] **Step 1: Write the failing test** — `internal/stats/stats_test.go`:

```go
package stats

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestBuildMatchesTheRouteDerivations pins the one derivation both UIs read:
// the sums and groupings the /api/stats handler used to compute inline.
func TestBuildMatchesTheRouteDerivations(t *testing.T) {
	js := &database.JobStats{
		FinishedCount: 3, ActiveCount: 1, MuxingCount: 1, ErrorCount: 2, CancelledCount: 1, QueuedCount: 4,
		YouTubeCount: 5, TwitchCount: 3,
		FinishedSize: 100, ErrorSize: 20, CancelledSize: 5, YouTubeSize: 90, TwitchSize: 35,
		TotalDuration: 3600, TotalChatMessages: 42,
	}
	d := &Disk{Free: 10, Total: 100, UsedPct: 90, WarnLevel: "warn"}
	s := Build(js, d)
	if s.TotalSize != 125 || s.JobCount != 8 {
		t.Errorf("TotalSize/JobCount = %d/%d, want 125/8 (queued excluded)", s.TotalSize, s.JobCount)
	}
	if s.SizeByPlatform["youtube"] != 90 || s.SizeByPlatform["twitch"] != 35 {
		t.Errorf("SizeByPlatform = %v", s.SizeByPlatform)
	}
	if s.SizeByStatus["finished"] != 100 || s.SizeByStatus["error"] != 20 || s.SizeByStatus["cancelled"] != 5 {
		t.Errorf("SizeByStatus = %v", s.SizeByStatus)
	}
	if s.TotalFinished != 3 || s.TotalDuration != 3600 || s.TotalChatMessages != 42 || s.ActiveDownloads != 1 || s.ActiveMuxing != 1 {
		t.Errorf("activity = %+v", s)
	}
	if s.CountByPlatform["youtube"] != 5 || s.CountByPlatform["twitch"] != 3 {
		t.Errorf("CountByPlatform = %v", s.CountByPlatform)
	}
	if s.Disk != *d {
		t.Errorf("Disk = %+v", s.Disk)
	}
	z := Build(nil, nil)
	if z.TotalSize != 0 || z.JobCount != 0 || z.SizeByPlatform["youtube"] != 0 || z.SizeByStatus["finished"] != 0 || z.CountByPlatform["twitch"] != 0 || z.Disk != (Disk{}) {
		t.Errorf("zero snapshot = %+v", z)
	}
	if z.SizeByPlatform == nil || z.SizeByStatus == nil || z.CountByPlatform == nil {
		t.Error("maps must be present (with zero entries) so callers never nil-check")
	}
}
```

- [ ] **Step 2: Run** — `go test -count=1 ./internal/stats/` → FAIL to compile.

- [ ] **Step 3: Create `internal/stats/stats.go`**

```go
// Package stats derives the figures both dashboards show — the Web Stats tab
// and the TUI's R T overlay — from the job aggregate and the disk reading.
// It imports only the database package so the TUI can use it without the
// HTTP layer.
package stats

import (
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// (Disk, Snapshot as in Interfaces)

func Build(js *database.JobStats, d *Disk) Snapshot {
	s := Snapshot{
		SizeByPlatform:  map[string]int64{"youtube": 0, "twitch": 0},
		SizeByStatus:    map[string]int64{"finished": 0, "error": 0, "cancelled": 0},
		CountByPlatform: map[string]int{"youtube": 0, "twitch": 0},
	}
	if d != nil {
		s.Disk = *d
	}
	if js == nil {
		return s
	}
	s.TotalSize = js.FinishedSize + js.ErrorSize + js.CancelledSize
	s.SizeByPlatform["youtube"] = js.YouTubeSize
	s.SizeByPlatform["twitch"] = js.TwitchSize
	s.SizeByStatus["finished"] = js.FinishedSize
	s.SizeByStatus["error"] = js.ErrorSize
	s.SizeByStatus["cancelled"] = js.CancelledSize
	s.JobCount = js.FinishedCount + js.ErrorCount + js.CancelledCount + js.ActiveCount + js.MuxingCount
	s.TotalFinished = js.FinishedCount
	s.TotalDuration = js.TotalDuration
	s.TotalChatMessages = js.TotalChatMessages
	s.ActiveDownloads = js.ActiveCount
	s.ActiveMuxing = js.MuxingCount
	s.CountByPlatform["youtube"] = js.YouTubeCount
	s.CountByPlatform["twitch"] = js.TwitchCount
	return s
}
```

- [ ] **Step 4: The route renders from the snapshot** — `internal/web/routes/stats.go` handler body (`:73-131`) becomes:

```go
		var js *database.JobStats
		if got, err := deps.DB.GetJobStats(); err == nil {
			js = got
		}
		var disk *stats.Disk
		if ds := SharedDiskStatus.Load(); ds != nil {
			disk = &stats.Disk{Free: ds.Free, Total: ds.Total, UsedPct: ds.UsedPct, WarnLevel: ds.WarnLevel}
		}
		snap := stats.Build(js, disk)
		warn := snap.Disk.WarnLevel
		if warn == "" {
			warn = "ok" // the route's historical default when no reading exists yet
		}
		diskResp := map[string]any{
			"free": snap.Disk.Free, "total": snap.Disk.Total, "usedPct": snap.Disk.UsedPct, "warnLevel": warn,
		}
		storageResp := map[string]any{
			"totalSize":  snap.TotalSize,
			"byPlatform": snap.SizeByPlatform,
			"byStatus":   snap.SizeByStatus,
			"jobCount":   snap.JobCount,
		}
		activityResp := map[string]any{
			"totalFinished":     snap.TotalFinished,
			"totalDuration":     snap.TotalDuration,
			"totalChatMessages": snap.TotalChatMessages,
			"activeDownloads":   snap.ActiveDownloads,
			"activeMuxing":      snap.ActiveMuxing,
			"byPlatform":        snap.CountByPlatform,
		}
```
keeping the `resp`/`twitchHints`/`jsonResponse` tail unchanged. Check `GetJobStats`'s return type (pointer or value) and adapt `js`. (Old code emitted untyped `0`/`0.0` for a missing disk reading; the struct emits `uint64(0)`/`float64(0)` — identical JSON, and the tests compare decoded `float64`s.) Import alias: the package name `stats` collides with the local variable the old code used — rename locals as above.

- [ ] **Step 5: Gates** — `go test -count=1 ./internal/stats/ ./internal/web/routes/` → two `ok` (routes' `/api/stats` tests unchanged and green proves the JSON); `gofmt`/`vet`/`staticcheck` silent; `go list -deps ./internal/stats/ | grep -c 'internal/web\|internal/bgutils'` → 0.

- [ ] **Step 6: Commit**
```
refactor(stats): the /api/stats derivations live in internal/stats

/api/stats renders its map from stats.Build's Snapshot — the struct the
TUI's statistics overlay will read — so both UIs show the same sums; the
JSON is unchanged (pinned). The package imports only internal/database.
```

---

### Task 2: `StatsDialogModel` — the overlay, rendered from a snapshot

**Files:**
- Create: `internal/tui/stats_dialog.go`, `internal/tui/stats_dialog_test.go`

**Interfaces:**
- Consumes: `stats.Snapshot`, `stats.Disk`; `utils.FormatFileSize(int64) string`; `progress.New(progress.WithoutPercentage())` with `Full='█'`, `Empty='░'` (as `job_details.go:72-76`); the package's `TitleStyle`/`ErrorStyle`/`HelpStyle`/`DimStyle` and the dialog frame helper the sibling dialogs use (grep `client_tokens_dialog.go`'s `View`).
- Produces: `NewStatsDialogModel() *StatsDialogModel`, `IsVisible() bool`, `SetSize(w, h int)`, `Open() tea.Cmd` (loading=true; returns spinner tick), `Close()`, `SetSnapshot(stats.Snapshot)`, `SetError(string)`, `UpdateComponents(tea.Msg) tea.Cmd`, `HandleKey(key string) string` (`"close"` | `"refresh"` | `""`), `View() string`, `formatHMS(seconds int64) string`, `formatUptime(time.Duration) string`, `groupThousands(int64) string`.

- [ ] **Step 1: Write the failing tests** — `internal/tui/stats_dialog_test.go`:

```go
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

func sampleSnapshot() stats.Snapshot {
	return stats.Snapshot{
		Disk:              stats.Disk{Free: 250 << 30, Total: 1000 << 30, UsedPct: 75, WarnLevel: "warn"},
		TotalSize:         512 << 30,
		JobCount:          1234,
		SizeByPlatform:    map[string]int64{"youtube": 400 << 30, "twitch": 112 << 30},
		SizeByStatus:      map[string]int64{"finished": 500 << 30, "error": 12 << 30, "cancelled": 0},
		TotalFinished:     1200,
		TotalDuration:     3*3600 + 25*60 + 7,
		TotalChatMessages: 98765,
		ActiveDownloads:   2,
		ActiveMuxing:      1,
		CountByPlatform:   map[string]int{"youtube": 900, "twitch": 334},
		Uptime:            49*time.Hour + 30*time.Minute,
	}
}

// TestStatsDialogRendersEveryWebCard: the overlay shows the same thirteen
// figures the Web Stats tab shows, plus uptime, in the Web's units.
func TestStatsDialogRendersEveryWebCard(t *testing.T) {
	m := NewStatsDialogModel()
	m.SetSize(100, 40)
	m.Open()
	if !strings.Contains(m.View(), "Loading") {
		t.Fatalf("loading state not rendered:\n%s", m.View())
	}
	m.SetSnapshot(sampleSnapshot())
	v := stripANSI(m.View())
	for _, want := range []string{
		"Storage", "Activity",
		"750.0 GB", "1000.0 GB", "75.0%", // disk used / total / pct
		"Total Recorded", "512.0 GB",
		"Total Jobs", "1,234",
		"YouTube Storage", "400.0 GB", "Twitch Storage", "112.0 GB",
		"Finished", "500.0 GB", "Error", "12.0 GB",
		"Streams Archived", "1,200",
		"Total Recording Time", "3h 25m 7s",
		"Chat Messages", "98,765",
		"Active Downloads", "2", "Muxing", "1",
		"YouTube Jobs", "900", "Twitch Jobs", "334",
		"Uptime", "2d 1h 30m",
		"█", "░", // the disk bar
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	for line := range strings.SplitSeq(v, "\n") {
		if w := runewidth.StringWidth(line); w > 100 {
			t.Errorf("line wider than the dialog (%d): %q", w, line)
		}
	}
}

// TestStatsDialogWarnColoursAndErrors: warn/critical change the bar's colour
// (not its text); an error replaces the body; keys map to actions.
func TestStatsDialogWarnColoursAndErrors(t *testing.T) {
	m := NewStatsDialogModel()
	m.SetSize(80, 30)
	m.Open()
	s := sampleSnapshot()
	s.Disk.WarnLevel = "ok"
	m.SetSnapshot(s)
	okView := m.View()
	s.Disk.WarnLevel = "critical"
	m.SetSnapshot(s)
	if m.View() == okView {
		t.Error("critical and ok renders are identical — the bar colour must follow warnLevel")
	}
	m.SetError("stats unavailable: db closed")
	if !strings.Contains(m.View(), "db closed") {
		t.Errorf("error not rendered:\n%s", m.View())
	}
	if got := m.HandleKey("r"); got != "refresh" {
		t.Errorf("r → %q, want refresh", got)
	}
	if got := m.HandleKey("esc"); got != "close" || m.IsVisible() {
		t.Errorf("esc → %q visible=%v, want close/false", got, m.IsVisible())
	}
}

func TestStatsFormatting(t *testing.T) {
	for in, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m 0s", 3661: "1h 1m 1s", 90061: "25h 1m 1s"} {
		if got := formatHMS(in); got != want {
			t.Errorf("formatHMS(%d) = %q, want %q", in, got, want)
		}
	}
	if got := formatUptime(49*time.Hour + 30*time.Minute); got != "2d 1h 30m" {
		t.Errorf("formatUptime = %q", got)
	}
	if got := formatUptime(5 * time.Minute); got != "5m" {
		t.Errorf("formatUptime(5m) = %q", got)
	}
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}
```
`stripANSI` exists in the package (used by `watched_test.go`). `go-runewidth` is already a dependency (`task_list.go`).

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Create `internal/tui/stats_dialog.go`**

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// statsRefreshInterval matches the Web Stats tab's setInterval(60000): the
// job aggregate is cached 5 s in the DB layer and the disk reading is the
// shared ticker's, so nothing here is worth polling faster.
const statsRefreshInterval = 60 * time.Second

// StatsDialogModel is the R T overlay: the Web Stats tab's disk bar, six
// storage figures, seven activity figures and uptime, from the same
// stats.Snapshot the /api/stats handler renders.
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

func NewStatsDialogModel() *StatsDialogModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	pb := progress.New(progress.WithoutPercentage())
	pb.Full = '█'
	pb.Empty = '░'
	pb.EmptyColor = ColorGray
	return &StatsDialogModel{spinner: sp, bar: pb}
}

func (m *StatsDialogModel) IsVisible() bool { return m.visible }

func (m *StatsDialogModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.bar.SetWidth(max(min(w-12, 60), 10))
}

// Open shows the overlay; the App fires the fetch. The last snapshot stays
// on screen while a refresh is in flight — "Loading" only before the first.
func (m *StatsDialogModel) Open() tea.Cmd {
	m.visible = true
	m.loading = true
	m.errorMsg = ""
	return m.spinner.Tick
}

func (m *StatsDialogModel) Close() { m.visible = false }

func (m *StatsDialogModel) SetSnapshot(s stats.Snapshot) {
	m.snap, m.haveSnap = s, true
	m.loading = false
	m.errorMsg = ""
}

func (m *StatsDialogModel) SetError(msg string) {
	m.loading = false
	m.errorMsg = msg
}

// UpdateComponents ticks the spinner while loading. Keys never come here —
// HandleKey decides on the key string alone (App.Update delivers every
// KeyPressMsg to both paths).
func (m *StatsDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.loading {
		return nil
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return cmd
}

// HandleKey: "close" on esc/q, "refresh" on r, "" otherwise.
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

func (m *StatsDialogModel) View() string {
	var b strings.Builder
	b.WriteString(TitleStyle.Render("Statistics"))
	b.WriteString("\n\n")
	switch {
	case m.loading && !m.haveSnap:
		b.WriteString(m.spinner.View())
		b.WriteString(" Loading...\n")
	case m.errorMsg != "":
		b.WriteString(ErrorStyle.Render(m.errorMsg))
		b.WriteString("\n")
	default:
		m.writeStorage(&b)
		b.WriteString("\n")
		m.writeActivity(&b)
	}
	b.WriteString("\n")
	b.WriteString(HelpStyle.Render("R: Refresh  Esc/Q: Close"))
	return dialogBox(b.String(), m.width, m.height)
}

func (m *StatsDialogModel) writeStorage(b *strings.Builder) {
	d := m.snap.Disk
	b.WriteString(sectionStyle.Render("Storage"))
	b.WriteString("\n")
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
	b.WriteString("  ")
	b.WriteString(bar.ViewAs(d.UsedPct / 100))
	b.WriteString("\n")
	fmt.Fprintf(b, "  %s used of %s (%.1f%%)\n", utils.FormatFileSize(used), utils.FormatFileSize(int64(d.Total)), d.UsedPct)
	writeRows(b, [][2]string{
		{"Total Recorded", utils.FormatFileSize(m.snap.TotalSize)},
		{"Total Jobs", groupThousands(int64(m.snap.JobCount))},
		{"YouTube Storage", utils.FormatFileSize(m.snap.SizeByPlatform["youtube"])},
		{"Twitch Storage", utils.FormatFileSize(m.snap.SizeByPlatform["twitch"])},
		{"Finished", utils.FormatFileSize(m.snap.SizeByStatus["finished"])},
		{"Error", utils.FormatFileSize(m.snap.SizeByStatus["error"])},
	})
}

func (m *StatsDialogModel) writeActivity(b *strings.Builder) {
	b.WriteString(sectionStyle.Render("Activity"))
	b.WriteString("\n")
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
	writeRows(b, rows)
}

// writeRows prints one label/value pair per line (the Web's card grid,
// flattened — ruling G2: readability over layout parity).
func writeRows(b *strings.Builder, rows [][2]string) {
	for _, r := range rows {
		fmt.Fprintf(b, "  %-22s %s\n", r[0], r[1])
	}
}

// formatHMS is the Web's formatDurationSeconds: "3h 25m 7s", "1m 0s", "59s".
func formatHMS(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	h, m, s := seconds/3600, (seconds%3600)/60, seconds%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// formatUptime: "2d 1h 30m", "5h 3m", "5m".
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
```
Adapt to the package's real names: `TitleStyle`, `ErrorStyle`, `HelpStyle`, a sub-heading style (`sectionStyle` — define `var sectionStyle = lipgloss.NewStyle().Bold(true)` in this file if the package has none; grep first), `ColorGray`/`ColorError`/`ColorWarning`/`ColorFinished` (grep `styles.go`), the dialog frame helper (`dialogBox` or whatever the sibling dialogs call), and `progress.Model`'s v2 API (`SetWidth`, `FullColor` — check `go doc charm.land/bubbles/v2/progress Model` and how `job_details.go` sets width/colour). Keep every identifier the tests reference. Use `WriteString`/`Fprintf` rather than `+` concatenation inside `WriteString` (gopls `writestring` hint).

- [ ] **Step 4: Run** — the three tests PASS; `go test -count=1 ./internal/tui/` ok.

- [ ] **Step 5: Commit**
```
feat(tui): StatsDialogModel renders the Web Stats tab's figures from a snapshot

Disk bar (bubbles progress, coloured by warn level), six storage and seven
activity rows plus uptime, in the Web's units and groupings.
```

---

### Task 3: `R T` chord, App wiring, 60 s refresh, `cmd/moombox` callback

**Files:**
- Modify: `internal/tui/app.go` (field `statsDlg *StatsDialogModel`, callback `OnGetStats func() (stats.Snapshot, error)`, msgs `statsSnapshotMsg{Snap stats.Snapshot; Err error}`, `statsRefreshTickMsg struct{}`, `NewApp()`, `hasActiveOverlay()`), `app_layout.go`, `app_keys.go`, `app_update.go`, `app_commands.go`, `app_actions.go`
- Modify: `cmd/moombox/tui_wiring.go`
- Test: `internal/tui/stats_chord_test.go`

**Interfaces:**
- Consumes Task 2's model and Task 1's `stats.Build`/`stats.Disk`; `db.GetJobStats()`; `routes.SharedDiskStatus.Load()` (in `cmd/moombox` only); the process start time `cmd/moombox` already passes to the jobs route deps (`grep -n 'StartTime' cmd/moombox/*.go`).

- [ ] **Step 1: Write the failing test** — `internal/tui/stats_chord_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

func rtOffered(app *App) bool {
	for _, it := range app.buildMenuItems() {
		if it.Chord == "R T" {
			return true
		}
	}
	return false
}

// TestStatsChordOpensLoadsAndRefreshes: R T exists only with the callback;
// dispatch opens the overlay loading, the fetch fills it, r re-fetches, the
// 60 s tick re-fetches while open and is ignored once closed; a fetch error
// renders in the overlay.
func TestStatsChordOpensLoadsAndRefreshes(t *testing.T) {
	app := NewApp()
	app.OnGetStats = nil
	if rtOffered(app) {
		t.Fatal("R T offered without OnGetStats")
	}
	calls := 0
	app.OnGetStats = func() (stats.Snapshot, error) {
		calls++
		s := sampleSnapshot()
		s.JobCount = 1000 + calls
		return s, nil
	}
	if !rtOffered(app) {
		t.Fatal("R T missing with OnGetStats wired")
	}
	_, cmd := app.dispatchAction("R T", nil)
	if !app.statsDlg.IsVisible() || !strings.Contains(app.statsDlg.View(), "Loading") {
		t.Fatal("R T must open the overlay loading")
	}
	runCmd(t, cmd) // drains the batch and feeds the messages back through app.Update — see the helper's real signature
	if calls != 1 || !strings.Contains(stripANSI(app.statsDlg.View()), "1,001") {
		t.Fatalf("after open: calls=%d view=%s", calls, app.statsDlg.View())
	}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	runCmd(t, cmd)
	if calls != 2 || !strings.Contains(stripANSI(app.statsDlg.View()), "1,002") {
		t.Fatalf("after r: calls=%d", calls)
	}
	_, cmd = app.Update(statsRefreshTickMsg{})
	runCmd(t, cmd)
	if calls != 3 {
		t.Fatalf("the refresh tick must re-fetch while open, calls=%d", calls)
	}
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.statsDlg.IsVisible() {
		t.Fatal("esc must close")
	}
	_, cmd = app.Update(statsRefreshTickMsg{})
	runCmd(t, cmd)
	if calls != 3 {
		t.Fatal("a tick after close must not fetch")
	}
	app.OnGetStats = func() (stats.Snapshot, error) { return stats.Snapshot{}, errors.New("db closed") }
	_, cmd = app.dispatchAction("R T", nil)
	runCmd(t, cmd)
	if !strings.Contains(app.statsDlg.View(), "db closed") {
		t.Fatal("fetch error must render in the overlay")
	}
}
```
`runCmd` — Arc F's `ytdlp_dialog_test.go` defines a batch-draining helper (`runCmd(t, cmd)` or with the app as an argument — read it and call it exactly as it is defined; if it does not feed messages back through `app.Update`, wrap: `for _, msg := range drained { app.Update(msg) }`). Never define a second helper with the same name. The refresh tick is a real `tea.Tick(statsRefreshInterval, …)` in production; the test injects `statsRefreshTickMsg{}` directly.

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Wire it** (each beside the `ytdlpDlg` sibling):
- `app.go`: field `statsDlg *StatsDialogModel`; callback with doc `// OnGetStats returns the numbers the Web Stats tab shows (the R T chord); nil deletes the chord.`; msgs; `NewApp()`: `statsDlg: NewStatsDialogModel()`; `hasActiveOverlay()`: `|| a.statsDlg.IsVisible()`.
- `app_layout.go`: `if a.statsDlg.IsVisible() { return a.viewWithMode(a.statsDlg.View()) }` beside the ytdlp branch.
- `app_commands.go`:
  ```go
  // fetchStatsCmd runs OnGetStats off the UI goroutine.
  func (a *App) fetchStatsCmd() tea.Cmd {
  	fn := a.OnGetStats
  	return safeCmd(func() tea.Msg {
  		snap, err := fn()
  		return statsSnapshotMsg{Snap: snap, Err: err}
  	})
  }

  // statsRefreshTick schedules the overlay's 60 s refresh (the Web's poll).
  func statsRefreshTick() tea.Cmd {
  	return tea.Tick(statsRefreshInterval, func(time.Time) tea.Msg { return statsRefreshTickMsg{} })
  }
  ```
- `app_keys.go` intercept (beside ytdlp's), passing the derived key string:
  ```go
  	if a.statsDlg.IsVisible() {
  		if a.statsDlg.HandleKey(key) == "refresh" {
  			return a, tea.Batch(a.statsDlg.Open(), a.fetchStatsCmd())
  		}
  		return a, nil
  	}
  ```
- `app_update.go`: `routeComponentMsg` branch `if a.statsDlg.IsVisible() { return a.statsDlg.UpdateComponents(msg) }`; arms:
  ```go
  	case statsSnapshotMsg:
  		if !a.statsDlg.IsVisible() {
  			return a, nil
  		}
  		if msg.Err != nil {
  			a.statsDlg.SetError("Statistics unavailable: " + msg.Err.Error())
  		} else {
  			a.statsDlg.SetSnapshot(msg.Snap)
  		}
  		// One tick chain per fetch result; closing the overlay ends it at the
  		// next tick. Open→close→reopen inside 60 s can overlap one extra tick —
  		// one 5 s-cached query, accepted.
  		return a, statsRefreshTick()
  	case statsRefreshTickMsg:
  		if !a.statsDlg.IsVisible() || a.OnGetStats == nil {
  			return a, nil
  		}
  		return a, a.fetchStatsCmd()
  ```
- `app_actions.go` `buildMenuItems()` beside the `R Y` gate: `if a.OnGetStats != nil { items = append(items, ActionMenuItem{Chord: "R T", Label: "Statistics", HintLabel: "Stats", Category: "Request"}) }`; `dispatchAction`:
  ```go
  	case "R T":
  		if a.OnGetStats == nil {
  			a.setFeedback("Statistics are unavailable")
  			return a, nil
  		}
  		a.clearFeedback()
  		a.statsDlg.SetSize(a.width, a.height)
  		return a, tea.Batch(a.statsDlg.Open(), a.fetchStatsCmd())
  ```
- `cmd/moombox/tui_wiring.go` (beside `OnListClientTokens`):
  ```go
  	app.OnGetStats = func() (stats.Snapshot, error) {
  		js, err := s.db.GetJobStats()
  		if err != nil {
  			return stats.Snapshot{}, err
  		}
  		var disk *stats.Disk
  		if ds := routes.SharedDiskStatus.Load(); ds != nil {
  			disk = &stats.Disk{Free: ds.Free, Total: ds.Total, UsedPct: ds.UsedPct, WarnLevel: ds.WarnLevel}
  		}
  		snap := stats.Build(js, disk)
  		snap.Uptime = time.Since(<startTime>)
  		return snap, nil
  	}
  ```
  where `<startTime>` is the same value `routes_wiring.go` passes as the jobs route deps' `StartTime`.

- [ ] **Step 4: Tests** — Step 1's test PASS; `go test -count=1 ./internal/tui/ ./cmd/moombox/` ok (`TestHelpCoversEveryChord`: `R T` is `Request`); `go list -deps ./internal/tui/ | grep -c 'internal/web/routes\|internal/bgutils'` → 0.

- [ ] **Step 5: Commit**
```
feat(tui): R T opens the statistics overlay, refreshed every minute

The callback reads db.GetJobStats and the shared disk reading through
stats.Build — the same sums /api/stats serves — plus uptime; r refreshes
now, a tea.Tick refreshes while the overlay is open.
```

---

### Task 4: Docs and the chord-catalog drift check

**Files:**
- Modify: `CLAUDE.md:109` (R chords sentence: add `` `R T` (Statistics — the Web Stats tab's figures, refreshed every minute) ``), `README.md` Request table (`| R T | Statistics |`), `docs/spec/user-interfaces.md`: Request chord table row `| \`R T\` | Statistics | Stats callback is configured |`; Module/Overlay Feature Mapping row `| Statistics | \`modules/stats.js\` | \`StatsDialogModel\` (\`internal/tui/stats_dialog.go\`) |` (replacing "N/A (data available via API)"); the `GET /api/stats` route row gains ` Derivations shared with the TUI via \`stats.Build\` (\`internal/stats\`).`; `docs/spec/architecture.md` — if it lists internal packages, add `internal/stats` in one line.
- Verify: every row of the chord tables in `docs/spec/user-interfaces.md:218-271` and `README.md:403-449` against `buildMenuItems()` — Arc F's fix wave corrected the known drift; list any row still wrong and fix it here.

- [ ] **Step 1: Edit the docs.** **Step 2:** `go test -count=1 ./internal/docs/` ok (citations: `StatsDialogModel`, `internal/tui/stats_dialog.go`, `stats.Build`, `internal/stats`, `modules/stats.js`). **Step 3:** drift check — `grep -n 'Chord: "' internal/tui/app_actions.go | sed -E 's/.*Chord: "([^"]+)", Label: "([^"]+)".*/\1 | \2/'` vs the docs' tables; fix mismatches; record which rows you changed. **Step 4:** `gofmt`/`vet`/`staticcheck` (no Go change expected) and commit:
```
docs: R T Statistics in the chord tables; the overlay mapping row filled in

The chord catalog is re-verified against buildMenuItems().
```

---

## Self-Review

**Spec coverage (§9 G + rulings G1–G5):** overlay mirroring the Web Stats tab (disk, storage, activity, uptime) ✔ Tasks 2–3; the disk bar is the one visual and it translates to a `progress` bar ✔; refresh cadence 60 s + manual ✔ Task 3; one derivation source in a neutral package with the route's JSON pinned ✔ Task 1; layering constraint (no tui→routes) ✔ Global Constraints + Task 3 gate; chord `R T` ✔; docs ✔ Task 4. **Placeholders:** none — each step carries code or exact text; package-specific names (styles, frame helper, progress v2 API, start time, `runCmd` signature) are flagged with where to read them. **Type consistency:** `stats.Snapshot`/`stats.Disk` fields identical in Task 1 builder/test, Task 2 view/fixture, Task 3 callback/wiring; `OnGetStats func() (stats.Snapshot, error)` in test, App, cmd; `HandleKey(key string) string` in model, test, intercept; `statsSnapshotMsg`/`statsRefreshTickMsg` in cmds, arms, test.

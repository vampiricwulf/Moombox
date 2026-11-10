package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// compactRig is an App with every callback a chord is gated on, so
// buildMenuItems lists every chord there is, and a record of what the
// destructive ones were asked to do.
type compactRig struct {
	app       *App
	deleted   []string
	cancelled []string
	reinit    []string
	restarted int
}

// compactJobs is one job of every status. No row shows a figure that moves
// with the clock, so a frame of them is the same on every run.
func compactJobs() []*database.Job {
	now := time.Now().UTC().Format(time.RFC3339)
	return []*database.Job{
		{ID: "down", Title: "Some stream title that is long enough to be cut", Status: database.StatusDownloading, Percent: 42, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
		{ID: "mux", Title: "A recording being muxed", Status: database.StatusMuxing, Percent: 7, Platform: "youtube", VideoID: "bbbbbbbbbbb"},
		{ID: "err", Title: "A broken stream", Status: database.StatusError, Platform: "youtube", VideoID: "ccccccccccc"},
		{ID: "cook", Title: "Members only", Status: database.StatusCookies, Platform: "youtube", VideoID: "ddddddddddd"},
		{ID: "que", Title: "A backlog video", Status: database.StatusQueued, Platform: "youtube", VideoID: "eeeeeeeeeee"},
		{ID: "canc", Title: "A cancelled job", Status: database.StatusCancelled, Platform: "youtube", VideoID: "fffffffffff", UpdatedAt: now},
		{ID: "done", Title: "An archive from yesterday", Status: database.StatusFinished, Watched: true, Platform: "youtube", VideoID: "ggggggggggg", UpdatedAt: now, OutputFile: "/out/g.mp4", Filename: "g.mp4"},
	}
}

func newCompactRig(t *testing.T, w, h int) *compactRig {
	t.Helper()
	r := &compactRig{}
	a := NewApp()
	a.SetConfigStore(config.NewStore(config.Defaults(), ""))
	a.SetSetupCallbacks(
		func(*config.MoomboxConfig) error { return nil },
		func(int, bool) {},
		func(string) error { return nil },
		func() (cookies.SetupResult, error) { return cookies.SetupResult{}, nil },
		func() {},
		func() {},
	)
	a.OnDeleteJob = func(id string) { r.deleted = append(r.deleted, id) }
	a.OnCancelJob = func(id string) (bool, error) { r.cancelled = append(r.cancelled, id); return true, nil }
	a.OnReinitializeJob = func(id string) error { r.reinit = append(r.reinit, id); return nil }
	a.OnResumeJob = func(string) error { return nil }
	a.OnMuxJob = func(string) error { return nil }
	a.OnRecoverAsides = func(string) error { return nil }
	a.OnSetWatched = func([]string, bool) error { return nil }
	a.OnOpenFolder = func(string) {}
	a.HasStagingFiles = func(string) bool { return true }
	a.HasSegmentFiles = func(string) bool { return true }
	a.HasUnmuxedParts = func(string) bool { return true }
	a.JobAsides = func(string) AsideSummary { return AsideSummary{Asides: []AsideEntry{{}}} }
	a.OnPreviewChapters = func(string, string, string) (ChapterImportView, error) { return ChapterImportView{}, nil }
	a.OnSaveChapters = func(string, string, string) (ChapterImportView, error) { return ChapterImportView{}, nil }
	a.OnRestoreChapters = func(string) (ChapterImportView, error) { return ChapterImportView{}, nil }
	a.HasOriginalChapters = func(string) bool { return true }
	a.OnListClientTokens = func() ([]*database.ClientToken, error) { return nil, nil }
	a.OnBackfillRescan = func() {}
	a.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
		return cookies.RefreshVerdict(0), cookies.RefreshVerdict(0), "", ""
	}
	a.OnForceRefreshCookies = func() (cookies.RefreshResult, error) { return cookies.RefreshResult{}, nil }
	a.OnCheckUpdate = func() (*UpdateStatusMsg, error) { return nil, nil }
	a.OnForceCheck = func() {}
	a.OnApplyUpdate = func(string) (string, bool) { return "", false }
	a.OnVerifySignature = func() (bool, error) { return true, nil }
	a.OnRestart = func() { r.restarted++ }
	a.OnFetchReleaseNotes = func(string) (string, string, error) { return "v1", "notes", nil }
	a.OnGetJobLogs = func(string) []string { return nil }
	a.OnImportCookieFile = func(string) (cookies.ImportResult, error) { return cookies.ImportResult{}, nil }
	a.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) { return ytdlpplugin.Info{}, nil }
	a.OnGetStats = func() (stats.Snapshot, error) { return stats.Snapshot{}, nil }
	a.updateAvailable = &UpdateStatusMsg{TagName: "v9.9.9", ReleaseNotes: "# notes"}
	a.seenChordHint = true

	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	a.Update(JobsUpdateMsg{Jobs: compactJobs()})
	r.app = a
	return r
}

// press sends keys through Update, the route a real key takes, and runs
// nothing they return.
func (r *compactRig) press(keys ...string) {
	for _, k := range keys {
		r.app.Update(namedKey(k))
	}
}

// pressRunLast sends one key through handleKey and runs the command it
// returns (a delete or a restart runs off the update loop, in that command).
func (r *compactRig) pressRunLast(key string) {
	_, cmd := r.app.handleKey(namedKey(key))
	if cmd != nil {
		cmd()
	}
}

func namedKey(name string) tea.KeyPressMsg {
	switch name {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	return keyMsg(name)
}

// selectJob puts the cursor on the job with this ID.
func (r *compactRig) selectJob(t *testing.T, id string) {
	t.Helper()
	r.app.taskList.GoToStart()
	for range len(r.app.taskList.list.Items()) {
		if j := r.app.taskList.SelectedJob(); j != nil && j.ID == id {
			r.app.updateSelectedJob()
			return
		}
		r.app.taskList.MoveDown()
	}
	t.Fatalf("job %q is not in the list", id)
}

func frameLines(a *App) []string {
	return strings.Split(a.View().Content, "\n")
}

func plainFrame(a *App) string {
	lines := frameLines(a)
	for i := range lines {
		lines[i] = strings.TrimRight(ansi.Strip(lines[i]), " ")
	}
	return strings.Join(lines, "\n")
}

// The size table of owner rulings R-C4 and R-C6: which layout each size
// draws, and that what it draws fits the terminal.
func TestLayoutBySize(t *testing.T) {
	tests := []struct {
		w, h int
		want layoutMode
	}{
		{29, 6, layoutTooSmall},
		{30, 5, layoutTooSmall},
		{30, 6, layoutCompact},
		{40, 10, layoutCompact},
		{59, 20, layoutCompact},
		{60, 19, layoutCompact},
		{60, 20, layoutNormal},
		{59, 200, layoutCompact},
		{200, 19, layoutCompact},
		{200, 5, layoutTooSmall},
		{29, 200, layoutTooSmall},
		{0, 0, layoutNormal}, // no size reported yet: "Initializing..."
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%dx%d", tt.w, tt.h), func(t *testing.T) {
			if got := layoutModeFor(tt.w, tt.h); got != tt.want {
				t.Fatalf("layoutModeFor(%d, %d) = %d, want %d", tt.w, tt.h, got, tt.want)
			}
			if tt.w == 0 {
				return
			}
			r := newCompactRig(t, tt.w, tt.h)
			a := r.app
			// A feedback line and a batch selection: the frame must fit with
			// everything it can carry.
			a.setFeedback("Press D to confirm delete \"" + strings.Repeat("a long title ", 10) + "\" (3s)")
			a.taskList.ToggleSelection("down")

			lines := frameLines(a)
			plain := plainFrame(a)
			for i, line := range lines {
				if w := ansi.StringWidth(line); w > tt.w {
					t.Errorf("row %d is %d cells, the terminal has %d: %q", i, w, tt.w, ansi.Strip(line))
				}
			}
			switch tt.want {
			case layoutTooSmall:
				if !strings.Contains(plain, "Terminal too small") {
					t.Errorf("no too-small notice:\n%s", plain)
				}
				if len(lines) > tt.h {
					t.Errorf("the notice is %d rows in a %d-row terminal", len(lines), tt.h)
				}
			case layoutCompact:
				if len(lines) != tt.h {
					t.Errorf("the compact frame is %d rows, the terminal has %d:\n%s", len(lines), tt.h, plain)
				}
				for i, line := range lines {
					if w := ansi.StringWidth(line); w != tt.w {
						t.Errorf("row %d is %d cells, want exactly %d", i, w, tt.w)
					}
				}
				if strings.Contains(plain, "Details") || strings.Contains(plain, "Logs") || strings.Contains(plain, "╭") {
					t.Errorf("the compact frame draws a panel:\n%s", plain)
				}
				// The long confirm wraps upward from the last row, at most
				// half the terminal; a short line is the last row alone,
				// under the status line and the rule.
				first := tt.h - tt.h/2
				if !strings.HasPrefix(ansi.Strip(lines[first]), "Press D to confirm") && !frameShows(a, a.feedback.msg) {
					t.Errorf("the confirm does not fill the bottom rows:\n%s", plain)
				}
				a.setFeedback("Press D to confirm (3s)")
				lines = frameLines(a)
				if !strings.HasPrefix(ansi.Strip(lines[tt.h-1]), "Press D to confirm") {
					t.Errorf("the feedback line is not the last row: %q", ansi.Strip(lines[tt.h-1]))
				}
				if !strings.Contains(ansi.Strip(lines[tt.h-3]), strings.Repeat("─", tt.w)) {
					t.Errorf("no rule above the status line: %q", ansi.Strip(lines[tt.h-3]))
				}
			case layoutNormal:
				if len(lines) != tt.h {
					t.Errorf("the frame is %d rows, the terminal has %d", len(lines), tt.h)
				}
				if !strings.Contains(plain, "Details") || !strings.Contains(plain, "Logs") {
					t.Errorf("the normal frame lacks its panels:\n%s", plain)
				}
			}
		})
	}
}

// The compact frame with the "/" box open, and with no jobs, still fits.
func TestCompactFrameFitsWithTheSearchBoxAndWhenEmpty(t *testing.T) {
	for _, size := range [][2]int{{30, 6}, {40, 10}, {59, 19}} {
		w, h := size[0], size[1]
		r := newCompactRig(t, w, h)
		check := func(what string) {
			t.Helper()
			lines := frameLines(r.app)
			if len(lines) != h {
				t.Errorf("%dx%d %s: %d rows", w, h, what, len(lines))
			}
			for i, line := range lines {
				if got := ansi.StringWidth(line); got != w {
					t.Errorf("%dx%d %s: row %d is %d cells", w, h, what, i, got)
				}
			}
		}
		r.press("/")
		if !r.app.taskList.IsSearching() {
			t.Fatalf("%dx%d: / did not open the filter box", w, h)
		}
		check("search box open")
		r.press("z", "z", "z", "q")
		check("no match")
		if !strings.Contains(plainFrame(r.app), "No tasks match") {
			t.Errorf("%dx%d: no empty-filter line:\n%s", w, h, plainFrame(r.app))
		}
		r.press("esc")
		r.app.Update(JobsUpdateMsg{Jobs: nil})
		check("no jobs")
		if !strings.Contains(plainFrame(r.app), "No tasks.") {
			t.Errorf("%dx%d: no empty-list line:\n%s", w, h, plainFrame(r.app))
		}
	}
}

// The approved mock-up of owner ruling R-C7, at 40 columns.
func TestCompactRowsMatchTheMockUp(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	m := NewTaskListModel()
	m.setCompact(true)
	m.SetSize(40, 4)

	rows := []struct {
		job  *database.Job
		want string
	}{
		{&database.Job{ID: "1", Title: "Some stream title that is long enough", Status: database.StatusDownloading, Percent: 42},
			"DOWN Some stream title that is lon…  42%"},
		{&database.Job{ID: "2", Title: "Another channel live now", Status: database.StatusLive, StreamStartTime: at(-(time.Hour + 12*time.Minute + 5*time.Second))},
			"LIVE Another channel live now    1:12:05"},
		{&database.Job{ID: "3", Title: "Scheduled premiere tomorrow", Status: database.StatusUpcoming, StreamStartTime: at(3*time.Hour + 20*time.Minute)},
			"WAIT Scheduled premiere tomorrow   in 3h"},
		{&database.Job{ID: "4", Title: "An archive from yesterday", Status: database.StatusFinished, Watched: true},
			"DONE An archive from yesterday         •"},
		{&database.Job{ID: "5", Title: "Not watched", Status: database.StatusFinished},
			"DONE Not watched                        "},
		{&database.Job{ID: "6", Title: "A hundred", Status: database.StatusMuxing, Percent: 100},
			"MUX  A hundred                      100%"},
		{&database.Job{ID: "7", Title: "Waiting on the end", Status: database.StatusDownloading, Percent: 50, Progress: "Waiting: verifying the stream ended"},
			""},
		{&database.Job{ID: "8", Title: "Live capture", Status: database.StatusDownloading, CreatedAt: at(-(5*time.Minute + 9*time.Second))},
			"DOWN Live capture                   5:09"},
		{&database.Job{ID: "9", Title: "Soon", Status: database.StatusUpcoming, StreamStartTime: at(90 * time.Second)},
			"WAIT Soon                          in 1m"},
		{&database.Job{ID: "10", Title: "", VideoID: "vid12345678", Status: database.StatusError},
			"ERR  vid12345678                        "},
	}
	for _, row := range rows {
		got := ansi.Strip(m.renderCompactJob(row.job, false, false, 40, now))
		if w := ansi.StringWidth(got); w != 40 {
			t.Errorf("%s: the row is %d cells, want 40: %q", row.job.ID, w, got)
		}
		if row.want != "" && got != row.want {
			t.Errorf("row %s:\n got %q\nwant %q", row.job.ID, got, row.want)
		}
	}

	// The tag carries the status colour, and the cursor's row the list's
	// highlight.
	job := rows[0].job
	plain := m.renderCompactJob(job, false, false, 40, now)
	if want := statusMarkStyle(job, false).Render("DOWN"); !strings.HasPrefix(plain, want) {
		t.Errorf("the tag is not in the status colour: %q", plain)
	}
	sel := m.renderCompactJob(job, true, false, 40, now)
	if sel == plain || !strings.Contains(sel, taskSelectedBgStyle.Render(" Some stream title that is lon…  42%")) {
		t.Errorf("the cursor's row is not highlighted: %q", sel)
	}
	m.selected["1"] = true
	if got := ansi.Strip(m.renderCompactJob(job, false, false, 40, now)); !strings.HasPrefix(got, "DOWN✓Some") {
		t.Errorf("a batch-selected row has no ✓: %q", got)
	}
}

// Every status has a tag of at most four letters, and no two share one.
func TestEveryStatusHasACompactTag(t *testing.T) {
	seen := map[string]database.JobStatus{}
	for _, s := range []database.JobStatus{
		database.StatusQueued, database.StatusUpcoming, database.StatusLive, database.StatusDownloading,
		database.StatusMuxing, database.StatusFinished, database.StatusError, database.StatusCancelled,
		database.StatusCookies,
	} {
		tag := compactStatusTag(s)
		if n := len(tag); n < 3 || n > compactTagWidth {
			t.Errorf("%s: tag %q is %d letters", s, tag, n)
		}
		if other, dup := seen[tag]; dup {
			t.Errorf("%s and %s share the tag %q", s, other, tag)
		}
		seen[tag] = s
	}
	if got := compactStatusTag("Paused"); got != "PAUS" {
		t.Errorf("an unknown status: tag %q, want its first four letters", got)
	}
}

// Golden frames (owner ruling R-C4): the list filling the screen, a rule,
// the status line, the feedback line.
func TestCompactGoldenFrames(t *testing.T) {
	t.Run("40x10", func(t *testing.T) {
		r := newCompactRig(t, 40, 10)
		r.press("down")
		r.app.setFeedback("Checking all monitors now…")
		want := strings.Join([]string{
			"ERR  A broken stream",
			"AUTH Members only",
			"DOWN Some stream title that is lon…  42%",
			"MUX  A recording being muxed          7%",
			"QUE  A backlog video",
			"CANC A cancelled job",
			"DONE An archive from yesterday         •",
			"────────────────────────────────────────",
			" 1▼ 1⎈ 1⋯ 1✗ 1⏣ 1⊘ 1✓               UPD",
			"Checking all monitors now…",
		}, "\n")
		if got := plainFrame(r.app); got != want {
			t.Errorf("the 40x10 frame:\n%s\nwant:\n%s", got, want)
		}
		// The cursor is on the second row.
		if line := frameLines(r.app)[1]; !strings.Contains(line, taskSelectedBgStyle.Render(" Members only"+strings.Repeat(" ", 23))) {
			t.Errorf("row 1 is not the highlighted cursor row: %q", line)
		}
	})
	t.Run("30x6", func(t *testing.T) {
		r := newCompactRig(t, 30, 6)
		r.app.statusBar.offline = true
		want := strings.Join([]string{
			"ERR  A broken stream",
			"AUTH Members only",
			"DOWN Some stream title t…  42%",
			"──────────────────────────────",
			" 1▼ 1⎈ 1⋯ 1✗ 1⏣ 1⊘ 1✓ OFF UPD",
			"",
		}, "\n")
		if got := plainFrame(r.app); got != want {
			t.Errorf("the 30x6 frame:\n%s\nwant:\n%s", got, want)
		}
	})
}

// The status line shows the warnings (owner ruling R-C4): connection,
// cookies, a pending update, and the two banners the compact layout has no
// rows for.
func TestCompactStatusLineShowsTheWarnings(t *testing.T) {
	r := newCompactRig(t, 59, 12)
	a := r.app
	line := func() string { return ansi.Strip(frameLines(a)[a.height-2]) }

	if got := line(); !strings.Contains(got, "UPD") {
		t.Errorf("no update mark: %q", got)
	}
	a.updateAvailable = nil
	if got := line(); strings.Contains(got, "UPD") {
		t.Errorf("the update mark outlived the update: %q", got)
	}
	a.statusBar.offline = true
	if got := line(); !strings.Contains(got, "OFF") {
		t.Errorf("no offline mark: %q", got)
	}
	a.statusBar.SetActivePlatforms(true, true)
	a.statusBar.SetCookieStatus(CookieStatusRelogin, CookieStatusOK)
	if got := line(); !strings.Contains(got, "YT!") || strings.Contains(got, "TW") {
		t.Errorf("cookie alerts: want YT! and no healthy TW: %q", got)
	}
	a.restartPending = true
	if got := line(); !strings.Contains(got, "RESTART") {
		t.Errorf("no restart mark: %q", got)
	}
	a.taskList.ToggleSelection("down")
	if got := line(); !strings.Contains(got, "1 sel") {
		t.Errorf("no selection count: %q", got)
	}
	a.taskList.applyQuery("stream")
	if got := line(); !strings.Contains(got, "/stream") {
		t.Errorf("no filter mark: %q", got)
	}

	// With every warning showing, the left half gives up whole segments,
	// the counts first: the batch selection count is never cut.
	for _, w := range []int{59, 40, 30} {
		r := newCompactRig(t, w, 12)
		a := r.app
		a.statusBar.offline = true
		a.statusBar.SetActivePlatforms(true, true)
		a.statusBar.SetCookieStatus(CookieStatusRelogin, CookieStatusRelogin)
		a.restartPending = true
		a.taskList.ToggleSelection("down")
		a.taskList.ToggleSelection("err")
		got := ansi.Strip(frameLines(a)[a.height-2])
		if ansi.StringWidth(got) != w {
			t.Errorf("%d columns: the status line is %d cells: %q", w, ansi.StringWidth(got), got)
		}
		for _, mark := range []string{"OFF", "YT!", "TW!", "RESTART", "UPD"} {
			if !strings.Contains(got, mark) {
				t.Errorf("%d columns: no %s: %q", w, mark, got)
			}
		}
		left, _, _ := strings.Cut(got, "OFF")
		fields := strings.Fields(left)
		if w >= 40 && !strings.HasPrefix(strings.TrimSpace(left), "2 sel") {
			t.Errorf("%d columns: the selection count is not first and whole: %q", w, got)
		}
		// Every count shown is a whole one of the full line's, and a
		// dropped one leaves an ellipsis of its own, never a cut token.
		full := strings.Fields(ansi.Strip(a.taskList.statusSummary))
		for _, f := range fields {
			if f == "2" || f == "sel" || f == "…" {
				continue
			}
			whole := false
			for _, c := range full {
				whole = whole || f == c
			}
			if !whole {
				t.Errorf("%d columns: %q is not a whole segment: %q", w, f, got)
			}
		}
	}
	// A long filter is shortened; the selection count before it is not.
	r = newCompactRig(t, 30, 12)
	r.app.taskList.applyQuery("stream stream stream stream stream stream")
	r.app.taskList.ToggleSelection("down")
	got := ansi.Strip(frameLines(r.app)[r.app.height-2])
	if !strings.Contains(got, " 1 sel /stream stream") || !strings.Contains(got, "…") || !strings.Contains(got, "UPD") || ansi.StringWidth(got) != 30 {
		t.Errorf("a long filter: %q", got)
	}
}

// chordCompact classifies every chord for the compact layout (owner ruling
// R-C5 as amended): "works" (it opens no overlay), "bare WxH" (it opens an
// overlay that is drawn bare from W x H up, and is refused below that), or
// "refused" (its overlay has no bare form and needs the normal layout,
// 60x20). A chord added to buildMenuItems must be added here.
var chordCompact = map[string]string{
	"A A": "bare 30x6", "A Z": "bare 30x6", "A T": "bare 30x6", "A O": "bare 30x6", "A K": "bare 30x6", "A C": "bare 30x10",
	"A R": "works", "A I": "works", "A M": "works", "A S": "works", "A X": "works", "A D": "works", "A W": "works", "A U": "works",
	"R B": "works", "R C": "works", "R F": "works", "R V": "works", "R M": "works", "R U": "works", "R S": "works", "R P": "works",
	"R N": "bare 30x6",
	"O F": "works", "O S": "works", "O W": "works", "O C": "works", "O G": "works",
	"O L": "bare 30x6",
	"E Y": "bare 30x6", "E L": "refused", "E I": "bare 30x6", "E T": "bare 30x6",
	"F": "works", "Q Q": "works",
	"`": "refused", "?": "bare 30x6",
}

// chordBare reads a chordCompact class: the minimum of a "bare WxH" one.
func chordBare(class string) (w, h int, ok bool) {
	n, _ := fmt.Sscanf(class, "bare %dx%d", &w, &h)
	return w, h, n == 2
}

// chordRefusedAt reports whether chordCompact says chord is refused in a
// compact terminal of w x h, and the size its refusal names.
func chordRefusedAt(chord string, w, h int) (refused bool, needW, needH int) {
	class := chordCompact[chord]
	if class == "works" {
		return false, 0, 0
	}
	if bw, bh, ok := chordBare(class); ok {
		return w < bw || h < bh, bw, bh
	}
	return true, minTermWidth, minTermHeight
}

func chordKeys(chord string) []string {
	return strings.Split(strings.ToLower(chord), " ")
}

func TestEveryChordIsClassifiedForCompact(t *testing.T) {
	t.Chdir(t.TempDir()) // A Z makes its import folder in the working directory
	items := newCompactRig(t, 40, 10).app.buildMenuItems()
	if len(items) != len(chordCompact) {
		t.Errorf("buildMenuItems lists %d chords, chordCompact classifies %d", len(items), len(chordCompact))
	}
	for _, item := range items {
		want, ok := chordCompact[item.Chord]
		if !ok {
			t.Errorf("chord %q is not classified in chordCompact", item.Chord)
			continue
		}
		t.Run(item.Chord, func(t *testing.T) {
			// The one rule, asked directly: also the only check for the
			// chords a test must not run (O S, O W and O G launch a browser).
			r := newCompactRig(t, 40, 10)
			refused, needW, needH := chordRefusedAt(item.Chord, 40, 10)
			if got := r.app.refusedForSize(item.Chord); got != refused {
				t.Fatalf("refusedForSize(%q) = %v, classified %q", item.Chord, got, want)
			}
			if (item.Opens != "") != (want != "works") {
				t.Fatalf("Opens = %q, classified %q", item.Opens, want)
			}
			if want == "works" {
				return
			}
			// The class is the registry's bare answer for the overlay.
			entry := overlayEntryNamed(item.Opens)
			if entry == nil {
				t.Fatalf("Opens %q is no overlayRegistry entry", item.Opens)
			}
			if w, h := entry.openNeed(); w != needW || h != needH {
				t.Fatalf("classified %q, and overlay %q opens from %dx%d", want, item.Opens, w, h)
			}

			if !refused {
				// Typed: it opens, drawn bare, filling the terminal.
				r = newCompactRig(t, 40, 10)
				r.selectJob(t, "done")
				r.press(chordKeys(item.Chord)...)
				a := r.app
				o := a.activeOverlay()
				if o == nil || o.name != item.Opens {
					t.Fatalf("did not open %q: feedback %q", item.Opens, a.feedback.msg)
				}
				if a.overlayDrawFor(o) != drawBare || a.screenHeld() {
					t.Errorf("overlay %q is not drawn bare at 40x10", o.name)
				}
				lines := frameLines(a)
				if len(lines) != 10 {
					t.Errorf("the bare frame is %d rows, want 10", len(lines))
				}
				for i, line := range lines {
					if w := ansi.StringWidth(line); w != 40 {
						t.Errorf("row %d of the bare frame is %d cells, want 40", i, w)
					}
				}
				return
			}
			// Typed: refused on the feedback line whatever is selected,
			// nothing opened, nothing left armed.
			for _, jobID := range []string{"done", "down"} {
				r = newCompactRig(t, 40, 10)
				r.selectJob(t, jobID)
				r.press(chordKeys(item.Chord)...)
				a := r.app
				if a.feedback.msg != compactRefusal(needW, needH) {
					t.Errorf("on %s: feedback %q, want %q", jobID, a.feedback.msg, compactRefusal(needW, needH))
				}
				if a.hasActiveOverlay() {
					t.Errorf("on %s: overlay %q opened", jobID, a.activeOverlay().name)
				}
				if a.chord.prefix != "" {
					t.Errorf("on %s: the chord is still armed", jobID)
				}
				if last := ansi.Strip(frameLines(a)[a.height-1]); !strings.HasPrefix(last, compactRefusal(needW, needH)) {
					t.Errorf("on %s: the refusal is not on the feedback line: %q", jobID, last)
				}
			}
			// And by the route the action menu takes.
			r = newCompactRig(t, 40, 10)
			r.app.dispatchAction(item.Chord, r.app.taskList.GetJobByID("done"))
			if r.app.hasActiveOverlay() || r.app.feedback.msg != compactRefusal(needW, needH) {
				t.Errorf("dispatchAction opened %v, feedback %q", r.app.hasActiveOverlay(), r.app.feedback.msg)
			}
		})
	}
}

// The M key opens the action menu, an overlay that is no entry of
// buildMenuItems. It has a bare form that draws at every compact size.
func TestTheActionMenuOpensInTheCompactLayout(t *testing.T) {
	for _, size := range [][2]int{{30, 6}, {40, 10}, {200, 8}, {30, 50}} {
		r := newCompactRig(t, size[0], size[1])
		r.press("m")
		if !r.app.actionMenu.IsVisible() || r.app.screenHeld() || r.app.feedback.msg != "" {
			t.Errorf("%v: M: menu open = %v, held = %v, feedback %q", size, r.app.actionMenu.IsVisible(), r.app.screenHeld(), r.app.feedback.msg)
		}
		wantExact(t, fmt.Sprintf("the menu at %v", size), r.app.View().Content, size[0], size[1])
	}
}

// ActionMenuItem.Opens is the one statement of which actions open an
// overlay. Run at the normal size, every chord opens the overlay it names
// and the others open none.
func TestEveryChordOpensWhatItSaysItOpens(t *testing.T) {
	// Not run: each launches a browser or leaves the program.
	skip := map[string]string{
		"O S": "opens the stream page in a browser",
		"O W": "opens the dashboard in a browser",
		"O G": "opens the project page in a browser",
		"Q Q": "quits",
	}
	registry := map[string]bool{}
	for _, e := range overlayRegistry {
		registry[e.name] = true
	}
	t.Chdir(t.TempDir()) // A Z makes its import folder in the working directory
	for _, item := range newCompactRig(t, 120, 40).app.buildMenuItems() {
		if item.Opens != "" && !registry[item.Opens] {
			t.Errorf("%s: Opens %q is no overlayRegistry entry", item.Chord, item.Opens)
		}
		if _, skipped := skip[item.Chord]; skipped {
			if item.Opens != "" {
				t.Errorf("%s is skipped here but says it opens %q", item.Chord, item.Opens)
			}
			continue
		}
		r := newCompactRig(t, 120, 40)
		a := r.app
		a.dispatchAction(item.Chord, a.taskList.GetJobByID("done"))
		got := ""
		if o := a.activeOverlay(); o != nil {
			got = o.name
		}
		if got != item.Opens {
			t.Errorf("%s: opened %q, its Opens says %q", item.Chord, got, item.Opens)
		}
	}
}

// The destructive chords work in the compact layout, with the confirm on
// the feedback line (owner ruling R-C5).
func TestCompactDestructiveChordsConfirmOnTheFeedbackLine(t *testing.T) {
	tests := []struct {
		chord, job, prompt string
		done               func(r *compactRig) bool
	}{
		{"A D", "done", "Press D to confirm delete", func(r *compactRig) bool { return len(r.deleted) == 1 && r.deleted[0] == "done" }},
		{"A X", "down", "Press X to confirm cancel", func(r *compactRig) bool { return len(r.cancelled) == 1 && r.cancelled[0] == "down" }},
		{"A I", "err", "Press I to confirm reinit", func(r *compactRig) bool { return len(r.reinit) == 1 && r.reinit[0] == "err" }},
		{"R P", "done", "Press P to confirm restart", func(r *compactRig) bool { return r.restarted == 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.chord, func(t *testing.T) {
			r := newCompactRig(t, 40, 10)
			r.selectJob(t, tt.job)
			keys := chordKeys(tt.chord)
			r.press(keys...)
			if !strings.HasPrefix(r.app.feedback.msg, tt.prompt) || !frameShows(r.app, r.app.feedback.msg) {
				t.Fatalf("the screen does not show the whole confirm %q:\n%s", r.app.feedback.msg, plainFrame(r.app))
			}
			if tt.done(r) {
				t.Fatal("the action ran before its confirm")
			}
			r.pressRunLast(keys[1])
			if !tt.done(r) {
				t.Errorf("the confirm key did not run %s (feedback %q)", tt.chord, r.app.feedback.msg)
			}
		})
	}
}

// Navigation, batch selection and the "/" filter work in the compact layout
// (owner ruling R-C5), and the one panel has the focus.
func TestCompactNavigationSelectionAndFilter(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	a := r.app
	a.setFocus(PanelLogs)
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if a.focusedPanel != PanelTasks {
		t.Fatalf("focus = %d in the compact layout, want the task list", a.focusedPanel)
	}

	first := a.taskList.SelectedJob().ID
	r.press("down")
	second := a.taskList.SelectedJob().ID
	if second == first {
		t.Fatal("Down did not move the cursor")
	}
	if a.details.job == nil || a.details.job.ID != second {
		t.Error("the hidden details panel did not follow the cursor")
	}
	r.press("tab")
	if a.focusedPanel != PanelTasks {
		t.Error("Tab moved the focus off the only panel")
	}

	r.press("space")
	if a.taskList.SelectedCount() != 1 {
		t.Fatal("Space did not select the row")
	}
	if !strings.Contains(plainFrame(a), "✓") || !strings.Contains(plainFrame(a), "1 sel") {
		t.Errorf("the selection is not on screen:\n%s", plainFrame(a))
	}
	r.press("esc")
	if a.taskList.SelectedCount() != 0 {
		t.Error("Esc did not clear the selection")
	}

	r.press("/", "b", "a", "c", "k", "enter")
	if a.taskList.Query() != "back" || len(a.taskList.list.Items()) != 1 {
		t.Fatalf("the filter: query %q, %d rows", a.taskList.Query(), len(a.taskList.list.Items()))
	}
	if got := plainFrame(a); !strings.Contains(got, "A backlog video") || strings.Contains(got, "A broken stream") {
		t.Errorf("the filtered frame:\n%s", got)
	}
	r.press("esc")
	if a.taskList.Query() != "" {
		t.Error("Esc did not clear the filter")
	}

	// F cycles the status filter, as in the Tasks panel.
	before := len(a.taskList.list.Items())
	r.press("f")
	if len(a.taskList.list.Items()) == before {
		t.Error("F did not cycle the status filter")
	}
}

// The mouse in the compact layout: the wheel moves through the list and a
// click selects a row; nothing else.
func TestCompactMouse(t *testing.T) {
	r := newCompactRig(t, 40, 10)
	a := r.app
	rowID := func(i int) string { return a.taskList.list.Items()[i].(taskItem).job.ID }

	a.Update(tea.MouseClickMsg{X: 5, Y: 4, Button: tea.MouseLeft})
	if got := a.taskList.SelectedJob().ID; got != rowID(4) {
		t.Errorf("a click on row 4 selected %q, want %q", got, rowID(4))
	}
	if a.details.job == nil || a.details.job.ID != rowID(4) {
		t.Error("the details panel did not follow the click")
	}
	// The rule, the status line and the feedback line are not rows.
	for _, y := range []int{7, 8, 9} {
		a.Update(tea.MouseClickMsg{X: 5, Y: y, Button: tea.MouseLeft})
		if got := a.taskList.SelectedJob().ID; got != rowID(4) {
			t.Errorf("a click on row %d moved the cursor to %q", y, got)
		}
	}
	a.Update(tea.MouseWheelMsg{X: 5, Y: 2, Button: tea.MouseWheelUp})
	if got := a.taskList.SelectedJob().ID; got != rowID(1) {
		t.Errorf("a notch up from row 4 is on %q, want %q", got, rowID(1))
	}
	a.Update(tea.MouseWheelMsg{X: 5, Y: 2, Button: tea.MouseWheelDown})
	if got := a.taskList.SelectedJob().ID; got != rowID(4) {
		t.Errorf("a notch down is on %q, want %q", got, rowID(4))
	}
}

// Below the compact floor only Ctrl+C works (owner ruling R-C6): the keys
// that once cancelled a recording with nothing on screen do nothing.
func TestKeysAreIgnoredBelowTheFloor(t *testing.T) {
	for _, size := range [][2]int{{29, 6}, {30, 5}, {10, 3}} {
		r := newCompactRig(t, 40, 10)
		a := r.app
		r.selectJob(t, "done")
		a.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		cursor := a.taskList.list.Index()

		for _, k := range []string{"a", "d", "d"} {
			_, cmd := a.handleKey(namedKey(k))
			if cmd != nil {
				cmd()
			}
		}
		r.press("a", "x", "x", "a", "i", "i", "r", "p", "p", "q", "q", "space", "down", "up", "enter", "/", "z", "m", "?", "`", "f", "tab")
		if len(r.deleted)+len(r.cancelled)+len(r.reinit)+r.restarted != 0 {
			t.Errorf("%v: a chord acted: deleted %v cancelled %v reinit %v restarts %d", size, r.deleted, r.cancelled, r.reinit, r.restarted)
		}
		if a.chord.prefix != "" || a.feedback.msg != "" {
			t.Errorf("%v: a key armed a chord or wrote feedback (%q, %q)", size, a.chord.prefix, a.feedback.msg)
		}
		if a.taskList.list.Index() != cursor || a.taskList.SelectedCount() != 0 || a.taskList.IsSearching() || a.taskList.Query() != "" {
			t.Errorf("%v: a key moved, selected or filtered the list", size)
		}
		if a.hasActiveOverlay() {
			t.Errorf("%v: a key opened %q", size, a.activeOverlay().name)
		}
		a.Update(tea.MouseClickMsg{X: 1, Y: 2, Button: tea.MouseLeft})
		a.Update(tea.MouseWheelMsg{X: 1, Y: 0, Button: tea.MouseWheelUp})
		if a.taskList.list.Index() != cursor {
			t.Errorf("%v: the mouse moved the cursor", size)
		}
		if _, cmd := a.handleKey(ctrlCMsg()); !isQuit(cmd) {
			t.Errorf("%v: Ctrl+C did not quit", size)
		}
	}
}

// A chord armed above the floor is disarmed when the terminal crosses below
// it: its confirm key must not act when the terminal grows back.
func TestCrossingBelowTheFloorDisarmsAChord(t *testing.T) {
	for _, from := range [][2]int{{120, 40}, {40, 10}} {
		r := newCompactRig(t, from[0], from[1])
		a := r.app
		r.selectJob(t, "done")
		r.press("a", "d")
		if a.chord.action != "d" {
			t.Fatalf("premise lost: A D did not arm (%q)", a.feedback.msg)
		}
		a.Update(tea.WindowSizeMsg{Width: 29, Height: 6})
		if a.chord.prefix != "" || a.feedback.msg != "" {
			t.Errorf("from %v: still armed below the floor (%q, %q)", from, a.chord.prefix, a.feedback.msg)
		}
		a.Update(tea.WindowSizeMsg{Width: from[0], Height: from[1]})
		r.pressRunLast("d")
		if len(r.deleted) != 0 {
			t.Errorf("from %v: the confirm key deleted %v after the terminal grew back", from, r.deleted)
		}
	}
	// A shrink into the compact layout keeps it: the prompt is still on
	// screen there.
	r := newCompactRig(t, 120, 40)
	r.selectJob(t, "done")
	r.press("a", "d")
	r.app.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if r.app.chord.action != "d" || !strings.Contains(plainFrame(r.app), "Press D to confirm") {
		t.Errorf("a shrink into the compact layout dropped the confirm:\n%s", plainFrame(r.app))
	}
}

// An overlay open when the terminal drops below 60x20 is kept with its
// state, under a notice; only Esc and Ctrl+C work until it grows back
// (owner ruling R-C6).
func TestAnOverlayIsKeptAcrossAShrink(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	a := r.app
	// The cookie login (E L) is the setup wizard, which has no bare form.
	r.press("e", "l")
	if !a.setupWiz.IsVisible() {
		t.Fatalf("premise lost: E L did not open the cookie login: %q", a.feedback.msg)
	}
	before := a.setupWiz.View()

	for _, size := range [][2]int{{59, 20}, {60, 19}, {30, 6}} {
		a.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		plain := plainFrame(a)
		if !strings.HasPrefix(plain, heldOverlayNotice(60, 20)) {
			t.Fatalf("%v: no notice over the kept overlay:\n%s", size, plain)
		}
		for i, line := range frameLines(a) {
			if w := ansi.StringWidth(line); w > size[0] || i >= size[1] {
				t.Errorf("%v: the notice does not fit (row %d, %d cells)", size, i, w)
			}
		}
		// Nothing typed, pasted, clicked or scrolled reaches it.
		typeIntoApp(a, "zzz")
		r.press("enter", "tab", "down", "m", "?")
		a.Update(tea.PasteMsg{Content: "pasted"})
		a.Update(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelDown})
		a.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
		if !a.setupWiz.IsVisible() {
			t.Fatalf("%v: a key closed the kept overlay", size)
		}
	}

	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if got := a.setupWiz.View(); got != before {
		t.Errorf("the overlay did not come back as it was:\n%s\nwas:\n%s", got, before)
	}
	if got := a.View().Content; got != before {
		t.Error("the screen is not the overlay again after the terminal grew back")
	}

	// Esc under the notice is the overlay's own Esc: the login closes, and
	// the compact list is back.
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	r.press("esc")
	if a.setupWiz.IsVisible() {
		t.Fatal("Esc did not close the kept overlay")
	}
	if got := plainFrame(a); strings.Contains(got, heldOverlayNotice(60, 20)) || !strings.Contains(got, "ERR  A broken stream") {
		t.Errorf("the compact list is not back:\n%s", got)
	}
	// Ctrl+C quits from under the notice too.
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r.press("e", "l")
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if !a.setupWiz.IsVisible() || !a.screenHeld() {
		t.Fatal("premise lost: the cookie login is not kept under the notice")
	}
	if _, cmd := a.handleKey(ctrlCMsg()); !isQuit(cmd) {
		t.Error("Ctrl+C did not quit from under the notice")
	}
}

// The memoised compact frame is the frame a fresh render draws, after each
// thing that can change it.
func TestCompactCachedFramesMatchFreshFrames(t *testing.T) {
	r := newCompactRig(t, 40, 10)
	a := r.app
	fresh := func() string {
		a.compact = compactFrame{}
		a.taskList.invalidate()
		return a.View().Content
	}
	steps := []struct {
		name  string
		apply func()
	}{
		{"progress", func() { a.taskList.progressStore.Set("down", &ProgressData{Progress: "V:9 A:9", Percent: 73}) }},
		{"cursor", func() { a.Update(namedKey("down")) }},
		{"selection", func() { a.Update(namedKey("space")) }},
		{"feedback", func() { a.setFeedback("Checking all monitors now…") }},
		{"feedback severity", func() { a.setFeedbackWithSeverity("Checking all monitors now…", severityError) }},
		{"feedback cleared", func() { a.clearFeedback() }},
		{"offline", func() { a.statusBar.offline = true }},
		{"update gone", func() { a.updateAvailable = nil }},
		{"restart pending", func() { a.restartPending = true }},
		{"job change", func() {
			jobs := compactJobs()
			jobs[4].Status = database.StatusUpcoming
			a.Update(JobsUpdateMsg{Jobs: jobs})
		}},
		{"filter", func() { a.taskList.applyQuery("stream") }},
		{"narrower", func() { a.Update(tea.WindowSizeMsg{Width: 34, Height: 10}) }},
		{"shorter", func() { a.Update(tea.WindowSizeMsg{Width: 34, Height: 7}) }},
	}
	for _, step := range steps {
		before := a.View().Content
		step.apply()
		got := a.View().Content
		if want := fresh(); got != want {
			t.Errorf("%s: the memoised frame differs from a fresh one:\n%s\nfresh:\n%s", step.name, ansi.Strip(got), ansi.Strip(want))
		}
		if got == before {
			t.Errorf("%s: the frame did not change, so the step proves nothing", step.name)
		}
	}
	// And the two memos do not serve each other's frames across a resize.
	compact := a.View().Content
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	normal := a.View().Content
	if normal == compact || !strings.Contains(ansi.Strip(normal), "Details") {
		t.Error("the normal frame was not drawn after growing out of the compact layout")
	}
	a.Update(tea.WindowSizeMsg{Width: 34, Height: 7})
	if got := a.View().Content; got != compact {
		t.Error("the compact frame was not drawn after shrinking back")
	}
}

// The fast progress tick runs while progress is on screen: in the compact
// layout a downloading row shows its percent, under a notice nothing does.
func TestProgressTickClassFollowsTheLayout(t *testing.T) {
	r := newCompactRig(t, 40, 10)
	a := r.app
	if !a.hasActiveDownloads() {
		t.Fatal("premise lost: the fixture has no active download")
	}
	if !a.wantsFastProgress() {
		t.Error("compact layout with a download: the tick is not at the fast class")
	}
	a.Update(tea.WindowSizeMsg{Width: 29, Height: 6})
	if a.wantsFastProgress() {
		t.Error("below the floor: the tick is still at the fast class")
	}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r.press("e", "l") // the cookie login has no bare form
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if !a.screenHeld() {
		t.Fatal("premise lost: the cookie login is not kept under the notice")
	}
	if a.wantsFastProgress() {
		t.Error("a kept overlay under the notice: the tick is still at the fast class")
	}
	r.press("esc")
	if !a.wantsFastProgress() {
		t.Error("back on the compact list: the tick is not at the fast class")
	}

	// Growing back out of a notice upshifts in the same Update.
	a.Update(tea.WindowSizeMsg{Width: 29, Height: 6})
	a.progressTicking, a.progressInterval = true, progressIdleInterval
	gen := a.progressGen
	if _, cmd := a.Update(tea.WindowSizeMsg{Width: 40, Height: 10}); cmd == nil || a.progressGen == gen {
		t.Error("growing back to the compact layout did not restart the tick at the fast class")
	}

	// No marquee runs in the compact layout: a title is cut, never scrolled.
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 6})
	r.selectJob(t, "down")
	if a.taskList.marquee.NeedsScroll() || a.ensureMarqueeTicking() != nil {
		t.Error("the marquee runs in the compact layout")
	}
}

// heldEscBase is what Esc does to each overlay, as overlayCases opens it,
// while it is kept under the notice. A new overlay must state its answer,
// unless it is never held: one whose bare form draws at every compact size
// (Help, Release Notes) has no row.
var heldEscBase = map[string]heldEscKind{
	"settings":       heldEscCloses,
	"ffmpeg check":   heldEscOff, // its Esc quits Moombox
	"setup wizard":   heldEscOff, // nothing to step back to on the mode choice
	"chapter import": heldEscCloses,
}

// neverHeld reports whether o is drawn bare at every compact size, so no
// notice ever covers it.
func neverHeld(o *overlayEntry) bool {
	return o.bare != nil && o.bare.now == nil &&
		o.bare.openW <= compactMinWidth && o.bare.openH <= compactMinHeight
}

// heldSizeFor is a compact size at which o, in its current state, is kept
// under the notice: 40x10 where that is too small for it, else one row (or
// one column) less than it needs.
func heldSizeFor(t *testing.T, a *App, o *overlayEntry) tea.WindowSizeMsg {
	t.Helper()
	w, h := a.overlayNeed(o)
	switch {
	case w > 40 || h > 10:
		return tea.WindowSizeMsg{Width: 40, Height: 10}
	case h > compactMinHeight:
		return tea.WindowSizeMsg{Width: max(w, compactMinWidth), Height: h - 1}
	case w > compactMinWidth:
		return tea.WindowSizeMsg{Width: w - 1, Height: max(h, compactMinHeight)}
	}
	t.Fatalf("overlay %q needs %dx%d: no compact size holds it", o.name, w, h)
	return tea.WindowSizeMsg{}
}

// checkHeldEsc shrinks a, over its open overlay, to a size that overlay is
// held at (heldSizeFor) and checks that the notice names the size the
// overlay needs, that its Esc line is want's, and that Esc then does what
// that line says and never quits.
func checkHeldEsc(t *testing.T, name string, a *App, visible func(*App) bool, want heldEscKind) {
	t.Helper()
	o := a.activeOverlay()
	if o == nil || !visible(a) {
		t.Fatalf("%s: premise lost: the overlay is not open", name)
	}
	held := heldSizeFor(t, a, o)
	a.Update(held)
	plain := plainFrame(a)
	if !strings.Contains(plain, heldEscLine(want)) || !strings.Contains(plain, "Open: "+o.title) {
		t.Errorf("%s: the notice does not say %q under its title:\n%s", name, heldEscLine(want), plain)
	}
	if !strings.HasPrefix(plain, heldOverlayNotice(a.overlayNeed(o))) {
		t.Errorf("%s: the notice does not name the size the overlay needs:\n%s", name, plain)
	}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	before := a.View().Content
	a.Update(held)

	_, cmd := a.Update(namedKey("esc"))
	if isQuit(cmd) {
		t.Errorf("%s: Esc under the notice quit Moombox", name)
	}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	switch want {
	case heldEscCloses:
		if visible(a) {
			t.Errorf("%s: the notice said Esc closes it, and it is still open", name)
		}
	case heldEscBack:
		if !visible(a) {
			t.Errorf("%s: the notice said Esc steps back, and the overlay closed", name)
		}
		if a.View().Content == before {
			t.Errorf("%s: the notice said Esc steps back, and nothing changed", name)
		}
	case heldEscOff:
		if !visible(a) || a.View().Content != before {
			t.Errorf("%s: the notice said Esc is off, and Esc changed the overlay", name)
		}
	}
}

// Under the notice Esc does what the notice says for every overlay, and
// never leaves the program: the FFmpeg check's own Esc quits, so it is not
// delivered (owner ruling R-C6).
func TestHeldEscCensus(t *testing.T) {
	cases := overlayCases(t)
	rows := 0
	for i, c := range cases {
		want, ok := heldEscBase[c.name]
		if overlayRegistry[i].title == "" {
			t.Errorf("overlay %q has no title for the notice", c.name)
		}
		if neverHeld(&overlayRegistry[i]) {
			if ok {
				t.Errorf("overlay %q is drawn bare at every compact size: it is never held, remove its heldEscBase row", c.name)
			}
			if overlayRegistry[i].heldEsc != nil {
				t.Errorf("overlay %q is never held and still has a heldEsc func", c.name)
			}
			continue
		}
		rows++
		if !ok {
			t.Errorf("overlay %q: say in heldEscBase what Esc does to it under the notice", c.name)
			continue
		}
		a := overlayTestApp(t)
		c.open(a)
		checkHeldEsc(t, c.name, a, c.visible, want)
	}
	if len(heldEscBase) != rows {
		t.Errorf("heldEscBase has %d overlays, %d can be held", len(heldEscBase), rows)
	}
}

// The states in which an overlay's own Esc does something other than close
// it: the notice says which, and nothing happens unseen.
func TestHeldEscFollowsTheOverlayState(t *testing.T) {
	settingsOpen := func(a *App) bool { return a.settings.IsVisible() }

	// Settings with unsaved edits: Esc would only raise the hidden
	// "Save changes?" prompt.
	a := overlayTestApp(t)
	a.settings.Open(a.cfg)
	a.settings.dirty = true
	checkHeldEsc(t, "settings, unsaved edits", a, settingsOpen, heldEscOff)
	if a.settings.closeConfirm {
		t.Error("Esc under the notice raised the Save changes prompt")
	}
	// And with that prompt already up, Esc would dismiss it unseen.
	a.settings.closeConfirm = true
	checkHeldEsc(t, "settings, save prompt", a, settingsOpen, heldEscOff)

	// The wizard's advanced form: two Escs abandon what was typed.
	a = overlayTestApp(t)
	a.setupWiz.Open()
	a.setupWiz.mode = setupModeAdvanced
	a.setupWiz.buildAdvancedForm()
	if a.setupWiz.advancedForm == nil {
		t.Fatal("premise lost: no advanced form")
	}
	checkHeldEsc(t, "setup wizard, advanced form", a, func(a *App) bool { return a.setupWiz.IsVisible() }, heldEscOff)
	if a.setupWiz.escConfirm {
		t.Error("Esc under the notice armed the wizard's abandon prompt")
	}

	// The FFmpeg check off its main menu: Esc goes back to it, and from
	// there it is off, so no run of Escs quits.
	a = overlayTestApp(t)
	a.ffmpegCheck.Open()
	a.ffmpegCheck.ShowManual()
	ffOpen := func(a *App) bool { return a.ffmpegCheck.IsVisible() }
	checkHeldEsc(t, "ffmpeg check, manual", a, ffOpen, heldEscBack)
	checkHeldEsc(t, "ffmpeg check, main", a, ffOpen, heldEscOff)
}

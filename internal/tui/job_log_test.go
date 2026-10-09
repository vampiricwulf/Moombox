package tui

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// fakeJobLogs stands in for the database's per-job buffers behind
// OnGetJobLogs: a copy per read, like db.GetJobLogs, and the 200→100 trim of
// capLogLines (internal/database) when append runs it past 200.
type fakeJobLogs struct {
	mu    sync.Mutex
	bufs  map[string][]string
	reads []string // the job ID of every read, in order
}

func newFakeJobLogs() *fakeJobLogs { return &fakeJobLogs{bufs: map[string][]string{}} }

func (f *fakeJobLogs) get(jobID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, jobID)
	return slices.Clone(f.bufs[jobID])
}

func (f *fakeJobLogs) append(jobID string, lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range lines {
		f.bufs[jobID] = append(f.bufs[jobID], l)
		if len(f.bufs[jobID]) > 200 {
			f.bufs[jobID] = slices.Clone(f.bufs[jobID][len(f.bufs[jobID])-100:])
		}
	}
}

func jobLine(jobID string, i int) string {
	return fmt.Sprintf("2026-10-09 12:00:00 INFO worker: job %s segment %d fetched", jobID, i)
}

// jobLogApp is an App sized like a real terminal, holding two jobs, with the
// fake buffers wired in and the cursor on the first job.
func jobLogApp(t *testing.T) (*App, *fakeJobLogs) {
	t.Helper()
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{
		{ID: "job1", Title: "First stream", Status: database.StatusDownloading, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
		{ID: "job2", Title: "Second stream", Status: database.StatusError, Platform: "youtube", VideoID: "bbbbbbbbbbb"},
	}})
	logs := newFakeJobLogs()
	a.OnGetJobLogs = logs.get
	return a, logs
}

// openJobLog presses O L on the job with the given ID and lands its first
// read, returning the number of refresh chains the open scheduled.
func openJobLog(t *testing.T, a *App, jobID string) int {
	t.Helper()
	job := a.taskList.GetJobByID(jobID)
	if job == nil {
		t.Fatalf("setup: no job %s", jobID)
	}
	_, cmd := a.dispatchAction("O L", job)
	if !a.jobLog.IsVisible() {
		t.Fatal("O L must open the job log overlay")
	}
	return applyStatsCmd(a, cmd)
}

func jobLogOffered(a *App) *ActionMenuItem {
	items := a.buildMenuItems()
	for i := range items {
		if items[i].Chord == "O L" {
			return &items[i]
		}
	}
	return nil
}

// TestJobLogChordExistsOnlyWhenWired: O L reads the database's per-job
// buffer and nothing else, so without OnGetJobLogs there is nothing to open
// and the chord is not offered; with it, it is an Open chord that needs a
// job and takes any job — the dashboard shows "Job Logs" for every job.
//
// Mutant: drop the `a.OnGetJobLogs != nil` gate in buildMenuItems — the
// chord is offered on an App that cannot read a log.
func TestJobLogChordExistsOnlyWhenWired(t *testing.T) {
	a := NewApp()
	if jobLogOffered(a) != nil {
		t.Fatal("O L offered without OnGetJobLogs")
	}
	a.OnGetJobLogs = func(string) []string { return nil }
	item := jobLogOffered(a)
	if item == nil {
		t.Fatal("O L missing with OnGetJobLogs wired")
	}
	if item.Category != "Open" || !item.NeedsJob || item.JobFilter != nil || item.NeedsConfirm {
		t.Errorf("O L = %+v, want an unfiltered, unconfirmed Open chord that needs a job", *item)
	}
	a.help.SetMenuItems(a.buildMenuItems())
	a.help.SetSize(120, 60)
	a.help.Toggle()
	if !strings.Contains(stripANSI(a.help.View()), "Open Job Log") {
		t.Error("the help overlay must list O L under Open")
	}
}

// TestJobLogReadsThatJobsOwnBuffer: O L opens loading, its first read asks
// for the job the chord named and nobody else, and the overlay shows that
// job's lines under its title — never another job's, never the global log.
//
// Mutants: fetchJobLogCmd reading a fixed or empty ID — the reads name the
// wrong job and job1's lines never show; the jobLogLinesMsg arm not calling
// SetLines — the overlay stays "Loading logs..."; dispatch passing the cursor
// job instead of the chord's — job2's lines show for job1.
func TestJobLogReadsThatJobsOwnBuffer(t *testing.T) {
	a, logs := jobLogApp(t)
	logs.append("job1", jobLine("job1", 1), jobLine("job1", 2))
	logs.append("job2", jobLine("job2", 1))
	a.logs.AddLine("2026-10-09 12:00:00 INFO server: unrelated global line")

	job := a.taskList.GetJobByID("job1")
	_, cmd := a.dispatchAction("O L", job)
	if got := stripANSI(a.View().Content); !strings.Contains(got, "Loading logs...") {
		t.Fatalf("before its first read the overlay must say it is loading:\n%s", got)
	}
	if ticks := applyStatsCmd(a, cmd); ticks != 1 {
		t.Fatalf("O L must start exactly one refresh chain, got %d", ticks)
	}
	if !slices.Equal(logs.reads, []string{"job1"}) {
		t.Fatalf("reads = %v, want one read of job1", logs.reads)
	}
	got := stripANSI(a.View().Content)
	for _, want := range []string{"Job Log — First stream (2)", jobLine("job1", 1), jobLine("job1", 2)} {
		if !strings.Contains(got, want) {
			t.Errorf("the overlay lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{jobLine("job2", 1), "unrelated global line"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the overlay shows %q, which is not job1's", unwanted)
		}
	}
}

// TestJobLogSaysWhenTheJobHasNoLines: an empty buffer is the dashboard
// section's "No logs for this job yet." once read — not "Loading logs..."
// for ever, and not the global panel's "No logs yet.".
//
// Mutant: drop the first-read redisplay in SetLines — an empty first read
// changes no line, and the overlay keeps saying it is loading.
func TestJobLogSaysWhenTheJobHasNoLines(t *testing.T) {
	a, _ := jobLogApp(t)
	openJobLog(t, a, "job2")
	got := stripANSI(a.View().Content)
	if !strings.Contains(got, "No logs for this job yet.") || strings.Contains(got, "Loading") {
		t.Fatalf("an empty job log must say so once read:\n%s", got)
	}
}

// TestJobLogFollowsTheBufferWhileOpen: while O L is open its job's buffer is
// re-read every jobLogRefreshInterval and new lines appear — including after
// the job turned terminal, which is when an A S recovery writes to it. One
// chain per open: a read never arms one, a tick re-arms exactly one, and
// after Esc, or a reopen on another job, the old open's tick and a read it
// had in flight are both dropped.
//
// Mutants: the tick arm not re-reading — the new line never shows; the lines
// arm re-arming a tick — two chains; dropping the epoch test in either arm —
// the stale tick reads again or the stale read paints job1's lines into
// job2's overlay; dropping the jobLogEpoch++ in the close path — the closed
// open's tick keeps reading.
func TestJobLogFollowsTheBufferWhileOpen(t *testing.T) {
	a, logs := jobLogApp(t)
	logs.append("job1", jobLine("job1", 1))
	openJobLog(t, a, "job1")
	openEpoch := a.jobLogEpoch

	if _, cmd := a.Update(jobLogLinesMsg{Epoch: openEpoch, Lines: logs.get("job1")}); cmd != nil {
		t.Fatal("a read must not schedule anything")
	}

	logs.append("job1", jobLine("job1", 2))
	_, cmd := a.Update(jobLogRefreshTickMsg{Epoch: openEpoch})
	if ticks := applyStatsCmd(a, cmd); ticks != 1 {
		t.Fatalf("the tick must re-arm exactly one chain, got %d", ticks)
	}
	if !strings.Contains(stripANSI(a.View().Content), jobLine("job1", 2)) {
		t.Fatal("a line the job logged after the open must appear on the next tick")
	}

	// Terminal is not the end: the overlay keeps reading.
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: &database.Job{ID: "job1", Title: "First stream", Status: database.StatusFinished, Platform: "youtube", VideoID: "aaaaaaaaaaa", Version: 99}}})
	if j := a.taskList.GetJobByID("job1"); j == nil || j.Status != database.StatusFinished {
		t.Fatal("setup: job1 must now be Finished")
	}
	logs.append("job1", jobLine("job1", 3))
	_, cmd = a.Update(jobLogRefreshTickMsg{Epoch: openEpoch})
	applyStatsCmd(a, cmd)
	if !strings.Contains(stripANSI(a.View().Content), jobLine("job1", 3)) {
		t.Fatal("a terminal job's log must still be followed — A S reports through it")
	}

	// Esc closes and retires the open.
	a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.jobLog.IsVisible() {
		t.Fatal("Esc must close the overlay")
	}
	if a.jobLogEpoch == openEpoch {
		t.Fatal("closing must retire the open's epoch")
	}
	reads := len(logs.reads)
	if _, cmd = a.Update(jobLogRefreshTickMsg{Epoch: openEpoch}); cmd != nil {
		t.Fatal("a tick after close must not produce a command")
	}

	// Reopen on job2: job1's late tick and late read are both dead.
	openJobLog(t, a, "job2")
	if _, cmd = a.Update(jobLogRefreshTickMsg{Epoch: openEpoch}); cmd != nil {
		t.Fatal("a tick from the previous open must be dropped, not re-armed")
	}
	a.Update(jobLogLinesMsg{Epoch: openEpoch, Lines: logs.get("job1")})
	if got := stripANSI(a.View().Content); strings.Contains(got, jobLine("job1", 1)) || !strings.Contains(got, "Second stream") {
		t.Fatalf("a read from the previous open must not paint job1's log into job2's:\n%s", got)
	}
	if want := reads + 2; len(logs.reads) != want { // job2's own read, and the test's manual job1 read
		t.Fatalf("reads after close = %v, want only job2's and the test's own", logs.reads[reads:])
	}
}

// TestJobLogPausedViewSurvivesTheJobRingTrim is the W24-12 rule applied to
// the overlay: the job's buffer trims to its last 100 lines once it passes
// 200, and a reader paused on lines that survived the trim stays on them; a
// reader paused on lines the trim evicted lands on the oldest survivor.
//
// Mutant: SyncLines replacing the buffer wholesale (Clear + AddLines, or a
// ringDelta that always answers drop=len(prev)) — the paused view jumps to
// the top of the new buffer in the first case, or keeps its offset over 101
// fewer rows and shows line 200's neighbourhood in the other.
func TestJobLogPausedViewSurvivesTheJobRingTrim(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		n    int
		want func(before string) string
	}{
		// 150 lines under 36 rows: one page up puts line 78 at the top,
		// which the trim below evicts (lines 0..100 go).
		{"top line evicted", tea.KeyPressMsg{Code: tea.KeyPgUp}, 1, func(string) string { return jobLine("job1", 101) }},
		// Five rows up puts line 109 at the top, which survives.
		{"top line survives", tea.KeyPressMsg{Code: tea.KeyUp}, 5, func(before string) string { return before }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, logs := jobLogApp(t)
			for i := range 150 {
				logs.append("job1", jobLine("job1", i))
			}
			openJobLog(t, a, "job1")
			for range tc.n {
				a.Update(tc.key)
			}
			if a.jobLog.log.autoScroll {
				t.Fatal("setup: scrolling up must pause the overlay")
			}
			before := topRow(a.jobLog.log)

			for i := 150; i <= 200; i++ { // the 201st line trims the buffer to its last 100
				logs.append("job1", jobLine("job1", i))
			}
			if n := len(logs.bufs["job1"]); n != 100 {
				t.Fatalf("setup: the fake buffer holds %d lines, want the trimmed 100", n)
			}
			_, cmd := a.Update(jobLogRefreshTickMsg{Epoch: a.jobLogEpoch})
			applyStatsCmd(a, cmd)

			if got, want := topRow(a.jobLog.log), tc.want(before); got != want {
				t.Errorf("after the trim the paused view's top row is %q, want %q (was %q)", got, want, before)
			}
			if a.jobLog.log.autoScroll {
				t.Error("a read must not un-pause the overlay")
			}
			if a.jobLog.log.rawCount != 100 {
				t.Errorf("the overlay holds %d lines, want the buffer's 100", a.jobLog.log.rawCount)
			}
		})
	}
}

// TestRingDelta pins the read-to-read diff SyncLines applies.
//
// Mutants: taking the LARGEST drop that lines up — the run of identical
// lines reads as an eviction; dropping the length guard — the cleared buffer
// slices past the end of cur and panics.
func TestRingDelta(t *testing.T) {
	a, b, c, d, e := "a", "b", "c", "d", "e"
	for _, tc := range []struct {
		name      string
		prev, cur []string
		drop      int
		tail      []string
	}{
		{"first read", nil, []string{a, b}, 0, []string{a, b}},
		{"unchanged", []string{a, b}, []string{a, b}, 0, []string{}},
		{"appended", []string{a, b}, []string{a, b, c}, 0, []string{c}},
		{"trimmed and appended", []string{a, b, c}, []string{c, d, e}, 2, []string{d, e}},
		{"trimmed only", []string{a, b, c}, []string{b, c}, 1, []string{}},
		{"identical lines appended", []string{a, a}, []string{a, a, a}, 0, []string{a}},
		{"cleared", []string{a, b, c}, nil, 3, nil},
		{"replaced", []string{a, b}, []string{d, e}, 2, []string{d, e}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drop, tail := ringDelta(tc.prev, tc.cur)
			if drop != tc.drop || !slices.Equal(tail, tc.tail) {
				t.Errorf("ringDelta(%v, %v) = %d, %v; want %d, %v", tc.prev, tc.cur, drop, tail, tc.drop, tc.tail)
			}
		})
	}
}

// TestJobLogKeysAreTheLogPanels: scrolling, paging, / search with n/N, End
// and Esc work in the overlay as on the log panel, through App.Update — and
// no key falls through to the chord system or the panels underneath. The
// header says so whatever the job is called: a stream title as long as the
// terminal is wide still leaves room for [PAUSED] and the search's count.
//
// Mutants: drop the overlay arm in routeComponentMsg — PgUp no longer
// scrolls; drop the overlay intercept in handleKey — "a" arms the Action
// prefix and "/" opens the log panel's search instead; drop the End case —
// End leaves the overlay paused; cut the title to the count alone again
// (drop `- reserve` in LogViewerModel.View) — the long title fills the
// header, and neither [PAUSED] nor "(2 matches)" is shown.
func TestJobLogKeysAreTheLogPanels(t *testing.T) {
	t.Run("short title", func(t *testing.T) { testJobLogKeys(t, "First stream") })
	t.Run("long title", func(t *testing.T) {
		testJobLogKeys(t, "【歌枠】Late night karaoke with chat requests — 3 hours of songs!! #VTuber #karaoke")
	})
}

func testJobLogKeys(t *testing.T, title string) {
	a, logs := jobLogApp(t)
	a.taskList.GetJobByID("job1").Title = title
	for i := range 120 {
		line := jobLine("job1", i)
		if i == 30 || i == 90 {
			line += " needle"
		}
		logs.append("job1", line)
	}
	openJobLog(t, a, "job1")
	press := func(k tea.KeyPressMsg) { a.Update(k) }
	text := func(s string) {
		for _, r := range s {
			press(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}

	press(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if a.jobLog.log.autoScroll || !strings.Contains(stripANSI(a.View().Content), "[PAUSED]") {
		t.Fatal("PgUp must scroll the overlay up and pause it")
	}
	press(tea.KeyPressMsg{Code: tea.KeyEnd})
	if !a.jobLog.log.autoScroll {
		t.Fatal("End must resume following the job's log")
	}

	text("a")
	if a.chord.prefix != "" {
		t.Fatal("a key in the overlay armed a chord underneath it")
	}

	text("/")
	if !a.jobLog.log.IsSearching() || a.logs.IsSearching() {
		t.Fatal("/ must open the overlay's own search box")
	}
	text("needle")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.jobLog.log.matchCount != 2 || !strings.Contains(stripANSI(a.View().Content), "[/needle] (2 matches)") {
		t.Fatalf("the search must find both of the job's matches, got %d", a.jobLog.log.matchCount)
	}
	first := a.jobLog.log.View()
	text("n")
	if a.jobLog.log.View() == first {
		t.Error("n must move to the next match")
	}
	text("N")
	if a.jobLog.log.View() != first {
		t.Error("N must step back to the match n left")
	}

	press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !a.jobLog.IsVisible() || a.jobLog.log.searchQuery != "" {
		t.Fatal("the first Esc clears the search and leaves the overlay open")
	}
	press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.jobLog.IsVisible() {
		t.Fatal("the second Esc closes the overlay")
	}
	if a.chord.prefix != "" || a.logs.searchQuery != "" {
		t.Error("closing the overlay must leave the main view untouched")
	}
}

// TestJobLogScrollsUnderTheWheel: the mouse wheel scrolls the overlay as it
// scrolls the log panel, and a wheel notch never reaches the panels hidden
// beneath.
//
// Mutant: drop the overlay arm in handleMouse — the wheel does nothing.
func TestJobLogScrollsUnderTheWheel(t *testing.T) {
	a, logs := jobLogApp(t)
	for i := range 120 {
		logs.append("job1", jobLine("job1", i))
	}
	openJobLog(t, a, "job1")
	bottom := a.jobLog.log.viewport.YOffset()
	a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: 10})
	if got := a.jobLog.log.viewport.YOffset(); got != bottom-3 || a.jobLog.log.autoScroll {
		t.Fatalf("a wheel notch up moved the overlay %d → %d (paused=%v), want 3 rows up and paused", bottom, got, !a.jobLog.log.autoScroll)
	}
	// The pause hint took a row, so the way back down is a row longer.
	a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 10, Y: 10})
	a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 10, Y: 10})
	if !a.jobLog.log.autoScroll {
		t.Fatal("wheeling back down to the bottom must follow the log again")
	}
}

// TestJobLogClosesWhenItsJobIsDeleted: the dashboard closes its job dialog
// on job_deleted; the overlay closes too, rather than blanking under the
// reader when the next read finds the buffer gone. Another job's deletion
// leaves it open. A bulk delete arrives only as a JobsUpdateMsg snapshot.
//
// Mutants: drop the call in handleJobDeleted, or the snapshot check in the
// JobsUpdateMsg arm — the overlay stays open over a deleted job.
func TestJobLogClosesWhenItsJobIsDeleted(t *testing.T) {
	a, _ := jobLogApp(t)
	openJobLog(t, a, "job1")
	a.Update(JobDeletedMsg{Deleted: &database.JobDeleted{JobID: "job2"}})
	if !a.jobLog.IsVisible() {
		t.Fatal("another job's deletion closed the overlay")
	}
	epoch := a.jobLogEpoch
	a.Update(JobDeletedMsg{Deleted: &database.JobDeleted{JobID: "job1"}})
	if a.jobLog.IsVisible() || a.jobLogEpoch == epoch {
		t.Fatal("deleting the overlay's job must close it and retire its open")
	}
	if !strings.Contains(a.feedback.msg, "job was deleted") {
		t.Errorf("feedback = %q, want it to say why the overlay closed", a.feedback.msg)
	}

	a, _ = jobLogApp(t)
	openJobLog(t, a, "job1")
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{
		{ID: "job1", Title: "First stream", Status: database.StatusDownloading, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
	}})
	if !a.jobLog.IsVisible() {
		t.Fatal("a snapshot that still holds the job closed the overlay")
	}
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{
		{ID: "job2", Title: "Second stream", Status: database.StatusError, Platform: "youtube", VideoID: "bbbbbbbbbbb"},
	}})
	if a.jobLog.IsVisible() {
		t.Fatal("a snapshot without the overlay's job (a bulk delete) must close it")
	}
}

// TestJobLogHeaderLeavesRoomForItsSuffixes: the overlay's header names the
// job, and an ordinary stream title is longer than the room a terminal of
// 60 to 120 columns leaves it. It used to be cut to the header's width less
// the count, which left nothing for the suffixes after it, so each one was
// dropped for not fitting: a search that found nothing said nothing, and a
// reader who had scrolled up saw no [PAUSED]. The title now gives way to
// them, down to "Job Log — " and the first few cells of the job's name, and
// keeps one width while the reader scrolls.
//
// Mutants: drop `- reserve` in LogViewerModel.View — no "(0 matches)" and no
// [PAUSED] at any size; drop the minHeaderTitleWidth floor — at 60 columns
// the name is cut to "Min…"; reserve the scroll percentage's own width
// instead of the widest — the title grows a cell between the first PgUp's
// percentage and [0%].
func TestJobLogHeaderLeavesRoomForItsSuffixes(t *testing.T) {
	titles := []struct{ name, title string }{
		{"latin", "Minecraft hardcore day 12 — building the castle"},
		{"cjk", "【歌枠】久しぶりのアコースティック歌枠！リクエストも受け付けます ♪ #新衣装 #karaoke"},
	}
	for _, size := range [][2]int{{minTermWidth, minTermHeight}, {80, 24}, {120, 40}} {
		for _, tt := range titles {
			title := tt.title
			t.Run(fmt.Sprintf("%dx%d %s", size[0], size[1], tt.name), func(t *testing.T) {
				a, logs := jobLogApp(t)
				a.width, a.height = size[0], size[1]
				a.recalcLayout()
				a.taskList.GetJobByID("job1").Title = title
				for i := range 120 {
					logs.append("job1", jobLine("job1", i))
				}
				openJobLog(t, a, "job1")
				header := func() string {
					return strings.Split(stripANSI(a.View().Content), "\n")[1]
				}
				for _, r := range "/absent" {
					a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
				}
				a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if h := header(); !strings.Contains(h, "[/absent] (0 matches)") {
					t.Errorf("a search with no hit must say so: %q", h)
				}
				a.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
				paused := header()
				if !strings.Contains(paused, "[PAUSED]") || !strings.Contains(paused, "(0 matches)") {
					t.Errorf("a paused, searched view must show both: %q", paused)
				}
				// The title keeps "Job Log — " and the first five cells
				// of the name at the narrowest terminal.
				if prefix := jobLogTitlePrefix + truncateWidth(title, 5, ""); !strings.Contains(paused, prefix) {
					t.Errorf("the title must keep %q: %q", prefix, paused)
				}
				for range 20 {
					a.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
				}
				top := header()
				if !strings.Contains(top, "[0%]") && size[0] > minTermWidth {
					t.Fatalf("setup: twenty PgUp presses must reach the top: %q", top)
				}
				before, _, _ := strings.Cut(paused, " (120)")
				after, _, _ := strings.Cut(top, " (120)")
				if before != after {
					t.Errorf("the title moved as the reader scrolled: %q, then %q", before, after)
				}
				if w := lipgloss.Width(top); w != size[0] {
					t.Errorf("the header row is %d columns wide, want %d: %q", w, size[0], top)
				}
			})
		}
	}
}

// TestJobLogFitsTheTerminal: the overlay is exactly the terminal's height at
// the TUI's floor and at a normal size, and no row is wider than it — a long
// job title is cut in the header rather than wrapping it, and the key line
// steps down to a spelling that fits.
//
// Mutant: drop the truncateString in LogViewerModel.View's header — the long
// title wraps and the frame grows a row.
func TestJobLogFitsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{minTermWidth, minTermHeight}, {120, 40}} {
		a, logs := jobLogApp(t)
		a.width, a.height = size[0], size[1]
		a.recalcLayout()
		job := a.taskList.GetJobByID("job1")
		job.Title = strings.Repeat("A very long stream title ", 12)
		for i := range 50 {
			logs.append("job1", jobLine("job1", i))
		}
		openJobLog(t, a, "job1")
		for _, searching := range []bool{false, true} {
			if searching {
				a.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
			}
			frame := a.View().Content
			rows := strings.Split(frame, "\n")
			if len(rows) != size[1] {
				t.Errorf("%dx%d (searching=%v): the overlay is %d rows, want %d", size[0], size[1], searching, len(rows), size[1])
			}
			for i, r := range rows {
				if w := lipgloss.Width(r); w > size[0] {
					t.Errorf("%dx%d (searching=%v): row %d is %d columns wide: %q", size[0], size[1], searching, i, w, stripANSI(r))
				}
			}
		}
	}
}

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// inSameSecond runs fn and reports whether the wall-clock second held still
// for the whole of it. Both render caches key on time.Now().Unix() — the
// header's monitor countdowns and the details panel's wall-clock text have to
// be free to move once a second (spec §5) — so an observation that straddles
// a second boundary says nothing about whether the cache was consulted.
// observeInOneSecond retries rather than racing; a retry is immediate and
// nothing sleeps.
func inSameSecond(fn func()) bool {
	before := time.Now().Unix()
	fn()
	return time.Now().Unix() == before
}

func observeInOneSecond(t *testing.T, fn func()) {
	t.Helper()
	for range 20 {
		if inSameSecond(fn) {
			return
		}
	}
	t.Fatal("20 attempts all straddled a second boundary — the observation window is far too slow to say anything about the cache")
}

// bubbletea calls View() after EVERY message, so a frame that changes
// nothing must cost nothing. The probe mutates a job IN PLACE, which no
// production path does without ending in rebuildVirtualList: if the second
// View() shows the new title, the cache was not consulted (CORE-2).
//
// Mutant: deleting the cache lookup at the top of TaskListModel.View().
func TestTaskListViewIsCachedBetweenIdenticalFrames(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	job := &database.Job{ID: "a", Title: "ORIGINAL", Status: database.StatusLive}
	m.SetJobs([]*database.Job{job})

	var first, second string
	observeInOneSecond(t, func() {
		job.Title = "ORIGINAL"
		m.renderCache = ""
		first = m.View()
		job.Title = "MUTATED"
		second = m.View()
	})

	if second != first {
		t.Errorf("an unchanged task list must return the cached frame:\n%s", stripANSI(second))
	}

	// A rebuild is the real invalidator.
	m.SetJobs([]*database.Job{job})
	if got := stripANSI(m.View()); !strings.Contains(got, "MUTATED") {
		t.Errorf("a rebuild must invalidate the cache, got:\n%s", got)
	}
}

// The task list renders each active row's live percent straight out of the
// progress store, and a progress-only job update never touches the task list
// model (app_update.go gates UpdateJob on hasDisplayChange). The store's
// revision counter is therefore part of the cache key.
//
// Mutant: dropping progressRev from taskListKey — the row keeps showing 10%.
// Mutant: not bumping rev in ProgressStore.Set — same stale row.
func TestTaskListCacheFollowsTheProgressStore(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	job := &database.Job{ID: "a", Title: "A", Status: database.StatusDownloading}
	m.SetJobs([]*database.Job{job})
	m.progressStore.Set("a", &ProgressData{Progress: "V:1 A:1", Percent: 10})
	if got := stripANSI(m.View()); !strings.Contains(got, "10%") {
		t.Fatalf("want the seeded 10%% in the row, got:\n%s", got)
	}

	m.progressStore.Set("a", &ProgressData{Progress: "V:2 A:2", Percent: 55})
	if got := stripANSI(m.View()); !strings.Contains(got, "55%") {
		t.Errorf("a progress-store write must invalidate the row cache, got:\n%s", got)
	}
}

// Delete and Clear move the rendered percent exactly as Set does (the row
// falls back to the job's own fields, or drops the cell), so they carry the
// revision too.
//
// Mutant: not bumping rev in ProgressStore.Delete or Clear.
func TestProgressStoreRevisionMovesOnEveryWrite(t *testing.T) {
	s := NewProgressStore()
	base := s.Rev()

	s.Set("a", &ProgressData{Percent: 1})
	afterSet := s.Rev()
	if afterSet == base {
		t.Errorf("Set must move the revision, still %d", afterSet)
	}
	s.Delete("a")
	afterDelete := s.Rev()
	if afterDelete == afterSet {
		t.Errorf("Delete must move the revision, still %d", afterDelete)
	}
	s.Clear()
	if s.Rev() == afterDelete {
		t.Errorf("Clear must move the revision, still %d", s.Rev())
	}
}

// renderHeader draws live monitor countdowns (time.Until), so the key
// carries the wall-clock second — otherwise the countdown freezes for as
// long as nothing else changes (spec §5). View() compares whole keys with
// ==, so a key that tracks the second is a frame that follows it.
//
// Mutant: dropping sec from taskListKey, or pinning it to a constant.
func TestTaskListCacheKeyCarriesTheWallClockSecond(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(80, 12)
	m.SetJobs([]*database.Job{{ID: "a", Title: "A", Status: database.StatusLive}})
	m.NextFeedCheck = time.Now().Add(90 * time.Second)

	var k1, k2 taskListKey
	var sampled int64
	observeInOneSecond(t, func() {
		k1 = m.taskListKey()
		k2 = m.taskListKey()
		sampled = time.Now().Unix()
	})
	if k1 != k2 {
		t.Fatalf("two keys sampled in the same second must be equal:\n%+v\n%+v", k1, k2)
	}
	if k1.sec != sampled {
		t.Errorf("key.sec = %d, want the current wall-clock second %d", k1.sec, sampled)
	}
}

// The details panel renders wall-clock-derived text baked in by
// updateViewportContent, and the 1 Hz RefreshRelativeTimes that recomputes it
// is itself gated — so this frame must be allowed to change once a second
// regardless (spec §5).
//
// Mutant: dropping sec from jobDetailsKey, or pinning it to a constant.
func TestJobDetailsCacheKeyCarriesTheWallClockSecond(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(60, 20)
	m.SetJob(&database.Job{ID: "a", Title: "A", Status: database.StatusLive})

	var k1, k2 jobDetailsKey
	var sampled int64
	observeInOneSecond(t, func() {
		k1 = m.jobDetailsKey()
		k2 = m.jobDetailsKey()
		sampled = time.Now().Unix()
	})
	if k1 != k2 {
		t.Fatalf("two keys sampled in the same second must be equal:\n%+v\n%+v", k1, k2)
	}
	if k1.sec != sampled {
		t.Errorf("key.sec = %d, want the current wall-clock second %d", k1.sec, sampled)
	}
}

// The details panel is memoised on the same rule. The probe writes straight
// into the viewport, keeping the line count identical so nothing in the key
// moves; only updateViewportContent (the single funnel, which bumps
// contentSeq) may make a change visible.
//
// Mutant: deleting the cache lookup at the top of JobDetailsModel.View() —
// the injected text appears in the second frame.
func TestJobDetailsViewIsCachedBetweenIdenticalFrames(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(60, 20)
	m.SetJob(&database.Job{ID: "a", Title: "ORIGINAL", Status: database.StatusLive, Platform: "youtube"})

	var first, second string
	observeInOneSecond(t, func() {
		m.updateViewportContent() // restore the real rows before a retry
		m.renderCache = ""
		first = m.View()

		inject := make([]string, m.viewport.TotalLineCount())
		for i := range inject {
			inject[i] = "MUTATED"
		}
		m.viewport.SetContentLines(inject)
		second = m.View()
	})

	if !strings.Contains(stripANSI(first), "ORIGINAL") {
		t.Fatalf("the first frame must show the job's title, got:\n%s", stripANSI(first))
	}
	if second != first {
		t.Errorf("an unchanged details panel must return the cached frame:\n%s", stripANSI(second))
	}

	// Not vacuous: the injection IS visible once the cache is dropped. The
	// field is poked directly rather than through a helper — the details
	// model has no production reason to invalidate out of band, and an
	// unexported method with only a test caller is a staticcheck U1000 hit.
	m.renderCache = ""
	if got := stripANSI(m.View()); !strings.Contains(got, "MUTATED") {
		t.Fatalf("the probe must be visible without the cache, got:\n%s", got)
	}

	// The real funnel restores the rows and invalidates through contentSeq.
	m.updateViewportContent()
	if got := stripANSI(m.View()); !strings.Contains(got, "ORIGINAL") {
		t.Errorf("a content rebuild must invalidate the cache, got:\n%s", got)
	}
}

// renderStep is one move of one panel's state, applied with the cache warm.
// covers names the key field the step is claimed to pin; runSteps then fails
// the step if it leaves the screen unchanged, because a step that renders
// identically cannot distinguish a pinned field from a live one. Two steps
// were vacuous exactly that way (review B1): a marquee tick on titles that
// never overflow, and the setup flag on a non-empty list. Steps that move
// several key fields at once are realistic traffic and claim nothing.
type renderStep[M any] struct {
	name   string
	covers string
	apply  func(M)
}

// runSteps drives one model through its steps, asserting after each that the
// frame the operator sees (memoised) is byte-for-byte the frame a renderer
// with no cache would produce. Equality across every step is also the
// byte-identity claim against the pre-change renderer, which is exactly the
// cache-defeated path. view/defeat are the model's View and its cache reset —
// the two panels have no common interface, and a two-line closure pair is
// cheaper than inventing one.
func runSteps[M any](t *testing.T, m M, steps []renderStep[M], view func() string, defeat func()) {
	t.Helper()
	for _, step := range steps {
		before := view() // warm the cache on the pre-step frame
		step.apply(m)
		got := view() // what the operator sees
		defeat()
		want := view() // the same frame with the cache defeated
		if got != want {
			t.Errorf("%s: the memoised frame differs from a fresh render\ncached:\n%s\nfresh:\n%s",
				step.name, stripANSI(got), stripANSI(want))
		}
		if step.covers != "" && want == before {
			t.Errorf("%s: the step renders identically, so it cannot cover %s — the claim is vacuous",
				step.name, step.covers)
		}
	}
}

// The cache may change what a frame COSTS, never what it says.
//
// Mutant: pinning any of taskListKey's covered fields to a constant —
// rebuildSeq, progressRev, width, height, focused, cursor, selectedCount,
// marqueeOffset, summary, justSetup, nextFeed, nextDecapi, nextTwitch — makes
// the step that names it below serve a stale frame that differs from the
// fresh one. The remaining three (items, page, query) are redundant insurance
// with no isolated production mover: every writer of the list's items, of the
// paginator's page and of m.tokens ends in rebuildVirtualList, so rebuildSeq
// already covers them and no step here claims them. sec has its own test.
func TestTaskListCachedFramesMatchFreshFrames(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	// Every title overflows the column at every width used below, so
	// whichever row the cursor is resting on has something to scroll — the
	// rows are sorted by status priority, so which one is row 0 is not
	// obvious from the order written here.
	m.SetJobs([]*database.Job{
		{ID: "a", Title: strings.Repeat("aaa scrolling title ", 6), Status: database.StatusLive},
		{ID: "b", Title: strings.Repeat("bbb scrolling title ", 6), Status: database.StatusDownloading, Percent: 12},
		{ID: "c", Title: strings.Repeat("ccc scrolling title ", 6), Status: database.StatusFinished, UpdatedAt: "2000-01-02T03:04:05Z"},
	})

	steps := []renderStep[*TaskListModel]{
		// One dimension at a time: a resize that moves both leaves each
		// field covering the other, and both pins survive.
		{"narrower", "width", func(m *TaskListModel) { m.SetSize(48, 12) }},
		{"shorter", "height", func(m *TaskListModel) { m.SetSize(48, 9) }},
		{"focus", "focused", func(m *TaskListModel) { m.SetFocused(true) }},
		{"cursor down", "cursor", func(m *TaskListModel) { m.MoveDown() }},
		// Back onto the overflowing first row, which also re-anchors the
		// marquee there for the step below.
		{"cursor up", "cursor", func(m *TaskListModel) { m.MoveUp() }},
		{"marquee step", "marqueeOffset", func(m *TaskListModel) {
			// Reset arms a marqueeWaitTicks pause, so a single Tick only
			// burns one tick of it and the offset never moves — which is
			// what made the original step vacuous. Tick past the pause and
			// insist the offset really advanced.
			for range marqueeWaitTicks + 2 {
				m.marquee.Tick()
			}
			if m.marquee.offset == 0 {
				t.Fatalf("the marquee did not advance (needsScroll=%v) — the step is vacuous again",
					m.marquee.NeedsScroll())
			}
		}},
		{"batch select", "selectedCount", func(m *TaskListModel) { m.ToggleSelection("b") }},
		{"clear selection", "selectedCount", func(m *TaskListModel) { m.ClearSelection() }},
		{"progress write", "progressRev", func(m *TaskListModel) {
			m.progressStore.Set("b", &ProgressData{Progress: "V:9 A:9", Percent: 73})
		}},
		{"archive sweep", "summary", func(m *TaskListModel) {
			// The once-a-minute sweep refreshes the header counts even when
			// it does not rebuild (the header counts every job, the dirty
			// check only looks at rows the filter passes), so statusSummary
			// is the one input that moves with no rebuildSeq behind it. The
			// in-place status change is the probe frame_counts_test.go uses.
			m.Jobs()[0].Status = database.StatusMuxing
			if m.ResweepArchive() {
				t.Fatal("the fixture must not cross the archive boundary — the sweep rebuilt, so this step no longer isolates summary")
			}
		}},
		{"status change", "rebuildSeq", func(m *TaskListModel) {
			m.UpdateJob(&database.Job{ID: "b", Title: "B", Status: database.StatusError})
		}},
		// The three monitor countdowns are assigned straight onto the model
		// by the monitor callbacks, so each is its own isolated mover. The
		// checking sentinel rather than a real future time: it moves the
		// field in the key exactly as a countdown does, but renders as a
		// static "…" so the two frames cannot disagree merely because the
		// second rolled between them. A live countdown's dependency on the
		// second is what TestTaskListCacheKeyCarriesTheWallClockSecond pins.
		{"feed checking", "nextFeed", func(m *TaskListModel) { m.NextFeedCheck = MonitorCheckingTime() }},
		{"decapi checking", "nextDecapi", func(m *TaskListModel) { m.NextDecapiCheck = MonitorCheckingTime() }},
		{"twitch checking", "nextTwitch", func(m *TaskListModel) { m.NextTwitchCheck = MonitorCheckingTime() }},
		// Realistic traffic that moves several fields at once; claims none.
		{"filter cycle", "", func(m *TaskListModel) { m.CycleFilter() }},
		{"query", "", func(m *TaskListModel) { m.applyQuery("c") }},
		{"query cleared", "", func(m *TaskListModel) { m.applyQuery("") }},
		{"archive toggle", "", func(m *TaskListModel) { m.ToggleArchive() }},
		{"empty", "", func(m *TaskListModel) { m.SetJobs(nil) }},
		// Only reachable on an empty list with no query: View() reads the
		// flag inside that branch alone, which is why this step follows
		// "empty" and "query cleared" rather than preceding them.
		{"setup flag", "justSetup", func(m *TaskListModel) { m.JustCompletedSetup = true }},
	}

	runSteps(t, m, steps, m.View, func() { m.renderCache = "" })
}

// The details twin of the differential above.
//
// Mutant: pinning any of jobDetailsKey's covered fields to a constant —
// contentSeq, focused, version, update, yOffset — makes the step that names
// it serve a stale frame. Six more (width, height, job, hideDesc, totalLines,
// vpHeight) are redundant insurance: every production path that moves one of
// them goes through updateViewportContent, so contentSeq covers it and no
// step here claims it. status is defensive rather than redundant — it has no
// production mover at all — and is pinned on its own by
// TestJobDetailsCacheFollowsAnInPlaceStatusChange. sec has its own test.
func TestJobDetailsCachedFramesMatchFreshFrames(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(60, 14)
	job := &database.Job{
		ID:          "a",
		Title:       strings.Repeat("a long title that scrolls ", 3),
		ChannelName: "chan",
		Status:      database.StatusDownloading,
		Platform:    "youtube",
		Percent:     12,
		Description: strings.Repeat("a description that wraps. ", 20),
	}
	m.SetJob(job)

	steps := []renderStep[*JobDetailsModel]{
		{"focus", "focused", func(m *JobDetailsModel) { m.SetFocused(true) }},
		// Before the scroll: the Title row is the first line of the content,
		// so once the viewport has scrolled off the top the marquee is no
		// longer on screen and the step below renders identically.
		{"marquee frame", "contentSeq", func(m *JobDetailsModel) {
			// The one mover that touches contentSeq and nothing else: the
			// same rows, the same line count, the same scroll offset, one
			// step further into the scrolling title. Tick past the
			// marqueeWaitTicks pause or the offset never moves and the
			// re-render is identical.
			for range marqueeWaitTicks + 2 {
				m.marquee.Tick()
			}
			if m.marquee.offset == 0 {
				t.Fatalf("the title marquee did not advance (needsScroll=%v) — the step is vacuous",
					m.marquee.NeedsScroll())
			}
			m.RefreshMarqueeFrame()
		}},
		{"scroll", "yOffset", func(m *JobDetailsModel) { m.ScrollDown() }},
		{"version", "version", func(m *JobDetailsModel) { m.version = "9.9.9" }},
		{"update badge", "update", func(m *JobDetailsModel) {
			m.updateInfo = &UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9"}
		}},
		// Realistic traffic that moves several fields at once; claims none.
		{"progress", "", func(m *JobDetailsModel) { m.SetProgress(&ProgressData{Progress: "V:5 A:5", Percent: 44}) }},
		{"resize", "", func(m *JobDetailsModel) { m.SetSize(44, 12) }},
		{"hide description", "", func(m *JobDetailsModel) { m.ToggleDescription() }},
		{"relative times", "", func(m *JobDetailsModel) { m.RefreshRelativeTimes() }},
		{"status change", "", func(m *JobDetailsModel) {
			next := *job
			next.Status = database.StatusFinished
			m.SetJob(&next)
		}},
		{"no job", "", func(m *JobDetailsModel) { m.SetJob(nil) }},
	}

	runSteps(t, m, steps, m.View, func() { m.renderCache = "" })
}

// jobDetailsKey.status is the one key field with no production mover: every
// path that changes a job's status replaces the *database.Job pointer and
// rebuilds the rows, so job and contentSeq already cover it. It is there to
// catch a future in-place mutation — and an unpinned key field is what the
// rest of this file exists to avoid, so here is its pin. The flip is
// invisible to the rows (View() reads m.job.Status directly, for the border
// and title colour), which is exactly the case the field guards.
//
// Mutant: pinning jobDetailsKey.status to "" — the frame keeps the Live
// colours after the job goes to Error.
func TestJobDetailsCacheFollowsAnInPlaceStatusChange(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(60, 20)
	m.SetFocused(true) // the status colour reaches the border only when focused
	job := &database.Job{ID: "a", Title: "A", Status: database.StatusLive}
	m.SetJob(job)

	var first, second string
	observeInOneSecond(t, func() {
		job.Status = database.StatusLive
		m.renderCache = ""
		first = m.View()
		job.Status = database.StatusError
		second = m.View()
	})
	if second == first {
		t.Error("an in-place status change must invalidate the details cache: the border and title are drawn in the job's status colour")
	}
}

// The search box renders a blinking textinput cursor that is not in the key,
// so the panel is rendered every frame while the box is open — and nothing
// is left memoised for the frame after it closes.
//
// Mutant: dropping the !m.searching guard from View().
func TestTaskListSearchBoxBypassesTheCache(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	m.SetJobs([]*database.Job{{ID: "a", Title: "ORIGINAL", Status: database.StatusLive}})
	m.View()
	m.StartSearch()

	if m.View(); m.renderCache != "" {
		t.Error("an open search box must leave nothing memoised")
	}
	m.searchInput.SetValue("xyz")
	if got := stripANSI(m.View()); !strings.Contains(got, "xyz") {
		t.Errorf("the search box must render live, got:\n%s", got)
	}
}

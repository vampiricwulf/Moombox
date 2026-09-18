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

// taskListSteps moves one keyed input at a time, starting from a warm cache.
// Every entry is a real production mutator (or the one field the monitor
// callbacks assign directly).
func taskListSteps() []struct {
	name  string
	apply func(*TaskListModel)
} {
	return []struct {
		name  string
		apply func(*TaskListModel)
	}{
		{"resize", func(m *TaskListModel) { m.SetSize(48, 10) }},
		{"focus", func(m *TaskListModel) { m.SetFocused(true) }},
		{"cursor down", func(m *TaskListModel) { m.MoveDown() }},
		{"batch select", func(m *TaskListModel) { m.ToggleSelection("b") }},
		{"clear selection", func(m *TaskListModel) { m.ClearSelection() }},
		{"progress write", func(m *TaskListModel) {
			m.progressStore.Set("b", &ProgressData{Progress: "V:9 A:9", Percent: 73})
		}},
		{"marquee step", func(m *TaskListModel) { m.marquee.Tick() }},
		{"archive sweep", func(m *TaskListModel) {
			// The once-a-minute sweep refreshes the header counts even when
			// it does not rebuild (the header counts every job, the dirty
			// check only looks at rows the filter passes), so statusSummary
			// is the one input that moves with no rebuildSeq behind it. The
			// in-place status change is the probe frame_counts_test.go uses.
			m.Jobs()[0].Status = database.StatusMuxing
			if m.ResweepArchive() {
				panic("the fixture must not cross the archive boundary")
			}
		}},
		{"status change", func(m *TaskListModel) {
			m.UpdateJob(&database.Job{ID: "b", Title: "B", Status: database.StatusError})
		}},
		{"filter cycle", func(m *TaskListModel) { m.CycleFilter() }},
		{"query", func(m *TaskListModel) { m.applyQuery("c") }},
		{"archive toggle", func(m *TaskListModel) { m.ToggleArchive() }},
		// The checking sentinel rather than a real future time: it moves
		// nextFeed in the key exactly as a countdown does, but renders as a
		// static "…" so the two frames below cannot disagree merely because
		// the second rolled between them. A live countdown's dependency on
		// the second is what TestTaskListCacheKeyCarriesTheWallClockSecond
		// pins.
		{"monitor checking", func(m *TaskListModel) { m.NextFeedCheck = MonitorCheckingTime() }},
		{"setup flag", func(m *TaskListModel) { m.JustCompletedSetup = true }},
		{"empty", func(m *TaskListModel) { m.SetJobs(nil) }},
	}
}

// The cache may change what a frame COSTS, never what it says. Each step
// moves one input with the cache already warm; the memoised frame is then
// compared byte-for-byte against the same model rendered with the cache
// defeated. Equality across every step is also the byte-identity claim
// against the pre-change renderer, which is exactly the cache-defeated path.
//
// Mutant: dropping any field from taskListKey (rebuildSeq, progressRev,
// width, height, focused, cursor, selectedCount, marqueeOffset, summary,
// query, justSetup, nextFeed) — the step that moves it then serves a stale
// frame that differs from the fresh one. summary is the one with no
// rebuildSeq behind it; the "archive sweep" step is what kills that mutant.
func TestTaskListCachedFramesMatchFreshFrames(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	m.SetJobs([]*database.Job{
		{ID: "a", Title: "aaa", Status: database.StatusLive},
		{ID: "b", Title: "bbb", Status: database.StatusDownloading, Percent: 12},
		{ID: "c", Title: "ccc", Status: database.StatusFinished, UpdatedAt: "2000-01-02T03:04:05Z"},
	})

	for _, step := range taskListSteps() {
		m.View() // warm the cache on the pre-step frame
		step.apply(m)
		got := m.View()
		m.renderCache = ""
		want := m.View()
		if got != want {
			t.Errorf("%s: the memoised frame differs from a fresh render\ncached:\n%s\nfresh:\n%s",
				step.name, stripANSI(got), stripANSI(want))
		}
	}
}

// The details twin of the differential above.
//
// Mutant: dropping any field from jobDetailsKey (contentSeq, width, height,
// focused, hideDesc, job, status, version, update, yOffset, totalLines,
// vpHeight) — the step that moves it serves a stale frame.
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

	steps := []struct {
		name  string
		apply func(*JobDetailsModel)
	}{
		{"focus", func(m *JobDetailsModel) { m.SetFocused(true) }},
		{"progress", func(m *JobDetailsModel) { m.SetProgress(&ProgressData{Progress: "V:5 A:5", Percent: 44}) }},
		{"scroll", func(m *JobDetailsModel) { m.ScrollDown() }},
		{"resize", func(m *JobDetailsModel) { m.SetSize(44, 12) }},
		{"hide description", func(m *JobDetailsModel) { m.ToggleDescription() }},
		{"version", func(m *JobDetailsModel) { m.version = "9.9.9" }},
		{"update badge", func(m *JobDetailsModel) { m.updateInfo = &UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9"} }},
		{"marquee frame", func(m *JobDetailsModel) {
			m.marquee.Tick()
			m.RefreshMarqueeFrame()
		}},
		{"relative times", func(m *JobDetailsModel) { m.RefreshRelativeTimes() }},
		{"status change", func(m *JobDetailsModel) {
			next := *job
			next.Status = database.StatusFinished
			m.SetJob(&next)
		}},
		{"no job", func(m *JobDetailsModel) { m.SetJob(nil) }},
	}

	for _, step := range steps {
		m.View() // warm the cache on the pre-step frame
		step.apply(m)
		got := m.View()
		m.renderCache = ""
		want := m.View()
		if got != want {
			t.Errorf("%s: the memoised frame differs from a fresh render\ncached:\n%s\nfresh:\n%s",
				step.name, stripANSI(got), stripANSI(want))
		}
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

package tui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// frameBenchJobs is the task-list depth CORE-2 was measured at — a long-lived
// install's real archive, not a toy list.
const frameBenchJobs = 1000

// frameBenchApp builds the shape CORE-2 measured: 1,000 jobs at 200x60 with
// the log panel at its maxLogLines cap. Everything a real frame reads is
// populated (progress entries for the active rows, a selected job in the
// details panel), so View() exercises all four panels.
func frameBenchApp(tb testing.TB) *App {
	tb.Helper()
	a := NewApp()
	a.width, a.height = 200, 60
	a.recalcLayout()

	jobs := make([]*database.Job, 0, frameBenchJobs)
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range frameBenchJobs {
		status := database.StatusFinished
		if i%5 == 0 {
			status = database.StatusDownloading
		}
		j := &database.Job{
			ID:          fmt.Sprintf("job-%04d", i),
			VideoID:     fmt.Sprintf("vid%08d", i),
			Title:       fmt.Sprintf("A reasonably long stream title number %d", i),
			ChannelName: fmt.Sprintf("Channel %d", i%37),
			Platform:    "youtube",
			Status:      status,
			UpdatedAt:   now,
			CreatedAt:   now,
		}
		jobs = append(jobs, j)
		if status == database.StatusDownloading {
			a.progressStore.Set(j.ID, &ProgressData{Progress: "V:1234 A:1234", Percent: float64(i % 100)})
		}
	}
	a.taskList.SetJobs(jobs)
	a.statusBar.SetJobs(jobs)
	a.details.SetJob(jobs[0])

	batch := make([]string, maxLogLines)
	for i := range batch {
		batch[i] = fmt.Sprintf("2026-09-17 12:00:00 INFO job-%04d segment fetched seq=%d size=123456 speed=4.2MiB/s", i%frameBenchJobs, i)
	}
	a.logs.AddLines(batch)

	// A budget measured against the wrong frame is worthless: if an overlay
	// were visible, or the terminal were judged too small, View() would take
	// an early return and the numbers below would describe one line of text.
	// Assert the fixture really renders all four panels before anything is
	// measured against it.
	plain := stripANSI(a.View().Content)
	for _, want := range []string{"Tasks (", "Details", fmt.Sprintf("Logs (%d)", maxLogLines)} {
		if !strings.Contains(plain, want) {
			tb.Fatalf("fixture frame is missing %q — View() took an early return:\n%s", want, plain)
		}
	}
	if got := strings.Count(plain, "\n") + 1; got != a.height {
		tb.Fatalf("fixture frame is %d rows, want %d (the status bar is the last one)", got, a.height)
	}
	return a
}

// BenchmarkFrameAtLogCap is one bubbletea frame: the whole View() with every
// panel populated and the log panel at its cap. This is the number CORE-2 is
// about — bubbletea calls View() after EVERY message, ~120/s with one active
// download.
func BenchmarkFrameAtLogCap(b *testing.B) {
	a := frameBenchApp(b)
	a.View() // warm the caches, as a running TUI always is
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.View()
	}
}

// BenchmarkLogPanelAtLogCap isolates the panel that dominated the frame
// before CORE-2 (60% of it), with its cache defeated each iteration so the
// pre-wrap win is what is measured rather than the memoisation.
//
// The win here is CPU, not allocation: soft-wrapping walked every buffered
// line calling ansi.StringWidth, which scans and allocates nothing, so this
// render costs 5,892 allocations on either side of CORE-2 while the time
// falls 1.98 ms -> 0.32 ms. That is why no allocation budget is asserted on
// it — TestLogLinesAreWrappedAtInsertion is the pin on SoftWrap, and this
// benchmark is the record of what the pre-wrap bought.
func BenchmarkLogPanelAtLogCap(b *testing.B) {
	a := frameBenchApp(b)
	a.logs.View()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.logs.invalidate()
		a.logs.View()
	}
}

// BenchmarkProgressFrameAtLogCap is the real steady-state message: one
// progress-store write followed by the frame bubbletea renders for it. It is
// the protected-behaviour side of CORE-2 — a frame whose inputs moved is
// still a full render, so this number must NOT collapse.
func BenchmarkProgressFrameAtLogCap(b *testing.B) {
	a := frameBenchApp(b)
	a.View()
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		i++
		a.progressStore.Set("job-0000", &ProgressData{Progress: "V:1234 A:1234", Percent: float64(i % 100)})
		a.View()
	}
}

// maxCachedFrameAllocs bounds a WHOLE bubbletea frame that carries no change,
// which is what the 60 Hz tick delivers most of the time. On main this frame
// cost 20,623 allocations because all four panels re-rendered unconditionally;
// with the CORE-2 caches it is three key comparisons, the status bar and two
// lipgloss joins — 100, of which the status bar (the one panel with no cache,
// because it tallies every job on every frame) is 70. The budget is 4x the
// measured number: room for a Go or lipgloss release that allocates
// differently, none for a panel that renders itself again.
//
// Asserted on allocations, not nanoseconds: allocation counts are
// deterministic across machines and CI runners, wall time is not.
const maxCachedFrameAllocs = 400

// maxCachedLogPanelAllocs bounds the log panel ALONE on an unchanged frame.
// Returning the memo allocates nothing at all (measured 0); the headroom is
// for a future lipgloss that copies on the way out, not for a re-render,
// which is 5,892.
//
// This is the log panel's half of the frame budget above, localised so a
// failure says which panel stopped being cached. It is NOT a pin on the
// pre-wrap: see BenchmarkLogPanelAtLogCap.
const maxCachedLogPanelAllocs = 8

// frameProbeRuns is how many back-to-back calls one allocation probe averages
// over. Allocation counts on a fixed code path are exact, so this divides
// away the probe's own noise rather than doing statistics.
const frameProbeRuns = 64

// steadyStateAllocs reports the heap allocations and bytes one call of body
// costs in steady state.
//
// Hermeticity (the reason this is not a bare testing.Benchmark): the task
// list and details caches key on the wall-clock second, so the FIRST frame
// after a second rolls is a legitimate full render, and a bare benchmark's
// answer would depend on the ambient -benchtime. The probe therefore warms
// inside the window, measures frameProbeRuns further calls, and — through
// observeInOneSecond, the same guard task 3's cache tests use — throws the
// sample away and retries if the second moved at any point during it.
// Nothing sleeps; a retry is immediate.
func steadyStateAllocs(t *testing.T, body func()) (allocs, bytes uint64) {
	t.Helper()
	var before, after runtime.MemStats
	observeInOneSecond(t, func() {
		body()
		runtime.ReadMemStats(&before)
		for range frameProbeRuns {
			body()
		}
		runtime.ReadMemStats(&after)
	})
	return (after.Mallocs - before.Mallocs) / frameProbeRuns,
		(after.TotalAlloc - before.TotalAlloc) / frameProbeRuns
}

// TestFrameCostAtLogCap is the regression pin for CORE-2: an unchanged frame
// at the 1,000-job / 1,000-line shape must cost almost nothing. ns/op is
// recorded from the benchmarks above in the commit body and the arc ledger,
// and is never asserted — wall time is not comparable across runners.
//
// Mutant: deleting the cache lookup at the top of TaskListModel.View(), or
// JobDetailsModel.View(), or LogViewerModel.View() — the whole-frame budget
// fails by more than an order of magnitude. Measured on this fixture: 100
// allocations cached, 8,304 with the task list's cache gone, 6,527 with the
// details panel's, 5,993 with the log panel's.
// Mutant: deleting the cache lookup in LogViewerModel.View() specifically —
// the log-panel budget fails as well, naming the panel (5,892).
func TestFrameCostAtLogCap(t *testing.T) {
	a := frameBenchApp(t)
	frameAllocs, frameBytes := steadyStateAllocs(t, func() { a.View() })
	t.Logf("cached frame: %d allocs/op, %d B/op", frameAllocs, frameBytes)
	if frameAllocs > maxCachedFrameAllocs {
		t.Errorf("a no-change frame allocates %d times, budget %d — a panel render cache is not being consulted",
			frameAllocs, maxCachedFrameAllocs)
	}

	l := frameBenchApp(t)
	logAllocs, logBytes := steadyStateAllocs(t, func() { l.logs.View() })
	t.Logf("cached log panel: %d allocs/op, %d B/op", logAllocs, logBytes)
	if logAllocs > maxCachedLogPanelAllocs {
		t.Errorf("an unchanged log panel allocates %d times, budget %d — LogViewerModel.View() is rendering instead of returning its memo",
			logAllocs, maxCachedLogPanelAllocs)
	}
}

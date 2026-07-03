package tui

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The demand-driven tick loops (marquee, log flush) and the event-driven
// terminal title are pure Update-loop state machines with no existing
// coverage. These tests pin the guard invariants that keep a 24/7 dashboard
// from re-rendering the whole screen when there's nothing to animate.

func TestMarqueeTickIsDemandDriven(t *testing.T) {
	app := NewApp()

	// Nothing overflows → the loop must not start.
	if cmd := app.ensureMarqueeTicking(); cmd != nil {
		t.Fatal("expected no marquee cmd when no title overflows")
	}
	if app.marqueeTicking {
		t.Fatal("marqueeTicking should stay false when nothing overflows")
	}

	// A selected title wider than its column → the loop starts exactly once.
	app.taskList.marquee.Reset("a very very very long stream title", 5)
	if !app.taskList.marquee.NeedsScroll() {
		t.Fatal("setup: title should overflow width 5")
	}
	if cmd := app.ensureMarqueeTicking(); cmd == nil || !app.marqueeTicking {
		t.Fatal("expected the marquee loop to start when a title overflows")
	}
	// Idempotent: a redundant call must not stack a second ticker.
	if cmd := app.ensureMarqueeTicking(); cmd != nil {
		t.Fatal("expected nil (no second loop) while one is already running")
	}

	// A full-screen overlay hides the list → the running loop self-stops on
	// its next tick and must not restart while the overlay is up.
	app.setupWiz.Open()
	if _, c := app.Update(marqueeTickMsg{}); c != nil {
		t.Fatal("expected the marquee loop to stop while an overlay covers the list")
	}
	if app.marqueeTicking {
		t.Fatal("marqueeTicking should be false after stopping under an overlay")
	}
	if cmd := app.ensureMarqueeTicking(); cmd != nil {
		t.Fatal("expected no marquee cmd while an overlay is active, even with a long title")
	}
	app.setupWiz.Close()

	// An overlay closed by an ASYNC completion (no key/mouse event) must
	// resume the paused marquee in the same Update — not wait for the 1s
	// backstop (the marquee pause is tick-counted, so backstop delay would
	// be additive). addVideoResultMsg is one of the four async-close paths.
	if _, c := app.Update(addVideoResultMsg{}); c == nil || !app.marqueeTicking {
		t.Fatal("expected the marquee loop to resume on an async dialog close")
	}
	app.marqueeTicking = false // reset for the next scenario

	// Selection moving to titles that fit → self-stop on the next tick.
	app.marqueeTicking = true
	app.taskList.marquee.Reset("short", 40)
	app.details.marquee.Reset("short", 40)
	if _, c := app.Update(marqueeTickMsg{}); c != nil {
		t.Fatal("expected the marquee loop to stop when no title overflows")
	}
	if app.marqueeTicking {
		t.Fatal("marqueeTicking should be false once no title overflows")
	}
}

func TestLogFlushLeadingEdgeThenTrailing(t *testing.T) {
	app := NewApp()

	if app.logFlushScheduled {
		t.Fatal("logFlushScheduled should start false (nothing armed at rest)")
	}

	// Leading edge: the first batch after a quiet period renders IMMEDIATELY
	// (real-time principle) — buffer drained inline, no trailing flush armed.
	app.Update(LogBatchMsg{Lines: []string{"line1", "line2"}})
	if app.logFlushScheduled {
		t.Fatal("first batch after idle must flush inline, not arm a trailing timer")
	}
	if len(app.logBuffer) != 0 {
		t.Fatalf("first batch after idle must drain inline; %d lines still buffered", len(app.logBuffer))
	}
	if app.lastLogFlush.IsZero() {
		t.Fatal("inline flush must stamp lastLogFlush for the throttle window")
	}

	// Follow-ups inside the open window buffer and arm exactly one trailing
	// flush (no double-arm).
	app.Update(LogBatchMsg{Lines: []string{"line3"}})
	if !app.logFlushScheduled {
		t.Fatal("expected a trailing flush armed for a batch inside the window")
	}
	if len(app.logBuffer) != 1 {
		t.Fatalf("expected 1 buffered line awaiting the trailing flush, got %d", len(app.logBuffer))
	}
	app.Update(LogBatchMsg{Lines: []string{"line4"}})
	if len(app.logBuffer) != 2 {
		t.Fatalf("expected 2 buffered lines, got %d", len(app.logBuffer))
	}
	if app.scheduleLogFlush() != nil {
		t.Fatal("scheduleLogFlush must be a no-op while a flush is already pending")
	}

	// The trailing flush drains the buffer and disarms so idle runs no ticks.
	app.Update(logFlushMsg{})
	if app.logFlushScheduled {
		t.Fatal("expected logFlushScheduled=false after the trailing flush")
	}
	if len(app.logBuffer) != 0 {
		t.Fatalf("expected the buffer drained, got %d", len(app.logBuffer))
	}

	// After the window expires, the next batch takes the leading edge again.
	app.lastLogFlush = app.lastLogFlush.Add(-2 * logFlushInterval)
	app.Update(LogBatchMsg{Lines: []string{"line5"}})
	if app.logFlushScheduled || len(app.logBuffer) != 0 {
		t.Fatal("batch after an expired window must flush inline again")
	}
}

func TestProgressTickIsDemandDriven(t *testing.T) {
	app := NewApp()

	// No jobs → nothing live → the loop must not start.
	if cmd := app.ensureProgressTicking(); cmd != nil {
		t.Fatal("expected no progress cmd when no job is live")
	}
	if app.progressTicking {
		t.Fatal("progressTicking should stay false with no live jobs")
	}

	// An Upcoming job counts as live — its "Starts In" countdown advances with
	// wall-clock time even though nothing is downloading yet. (Guards against
	// the tempting but wrong hasActiveDownloads-only gate.)
	app.statusMap["u"] = database.StatusUpcoming
	if !app.hasLiveContent() {
		t.Fatal("an Upcoming job must count as live content")
	}
	if cmd := app.ensureProgressTicking(); cmd == nil || !app.progressTicking {
		t.Fatal("expected the progress loop to start for a live job")
	}
	// Idempotent: no second ticker.
	if cmd := app.ensureProgressTicking(); cmd != nil {
		t.Fatal("expected nil (no second loop) while one is already running")
	}

	// Everything terminal → self-stop on the next tick.
	app.statusMap["u"] = database.StatusFinished
	if _, c := app.Update(progressTickMsg{}); c != nil {
		t.Fatal("expected the progress loop to stop once every job is terminal")
	}
	if app.progressTicking {
		t.Fatal("progressTicking should be false when nothing is live")
	}
}

func TestProgressCadenceUpshiftIsImmediate(t *testing.T) {
	app := NewApp()
	app.statusMap["u"] = database.StatusUpcoming
	if cmd := app.ensureProgressTicking(); cmd == nil {
		t.Fatal("expected the loop to start for an Upcoming job")
	}
	if app.progressInterval != progressIdleInterval {
		t.Fatalf("expected the idle 500ms class, got %v", app.progressInterval)
	}
	prevGen := app.progressGen

	// The job goes live: the ensure hook must supersede the pending 500ms
	// tick with a fresh 16ms schedule NOW instead of waiting it out
	// (real-time principle — 60fps progress from the first frame).
	app.statusMap["u"] = database.StatusDownloading
	if cmd := app.ensureProgressTicking(); cmd == nil {
		t.Fatal("expected an upshift cmd when a download starts mid-loop")
	}
	if app.progressInterval != progressFastInterval {
		t.Fatalf("expected the fast 16ms class after upshift, got %v", app.progressInterval)
	}
	if app.progressGen == prevGen {
		t.Fatal("upshift must bump the generation to invalidate the pending 500ms tick")
	}

	// The superseded tick arrives late: dropped without re-arming and
	// without disturbing the current loop.
	if _, c := app.Update(progressTickMsg{gen: prevGen}); c != nil {
		t.Fatal("stale-generation tick must be dropped without re-arming")
	}
	if !app.progressTicking {
		t.Fatal("dropping a stale tick must not stop the current loop")
	}

	// Already at the fast class: no duplicate schedule.
	if cmd := app.ensureProgressTicking(); cmd != nil {
		t.Fatal("no upshift cmd when already at the fast class")
	}
}

func TestArchiveResweepDetectsAgingCrossing(t *testing.T) {
	m := NewTaskListModel()
	m.hideFinishedAgeDays = 1
	jobs := []*database.Job{{
		ID: "a", Title: "Done", VideoID: "v",
		Status:    database.StatusFinished,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	m.SetJobs(jobs)
	if m.ResweepArchive() {
		t.Fatal("fresh Finished job hasn't crossed the age boundary — resweep must be a no-op")
	}

	// Simulate the dashboard sitting idle while the job ages past the
	// boundary (no rebuild-triggering event arrives).
	jobs[0].UpdatedAt = time.Now().Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if !m.ResweepArchive() {
		t.Fatal("a job aged past the boundary must trigger a re-bucket")
	}
	if !m.archivedSet["a"] {
		t.Fatal("job should be classified archived after the resweep")
	}
	if m.ResweepArchive() {
		t.Fatal("second resweep with no further aging must be a no-op")
	}
}

func TestTerminalTitleIsEventDrivenNotPolled(t *testing.T) {
	app := NewApp()
	// The 1s tick no longer recomputes the title — it's refreshed by the
	// job-lifecycle handlers on status change instead.
	app.windowTitle = "sentinel"
	app.Update(tickMsg{})
	if app.windowTitle != "sentinel" {
		t.Fatalf("tickMsg must not recompute the title; got %q", app.windowTitle)
	}
}

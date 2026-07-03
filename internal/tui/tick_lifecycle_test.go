package tui

import (
	"testing"

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

func TestLogFlushIsDemandDriven(t *testing.T) {
	app := NewApp()

	if app.logFlushScheduled {
		t.Fatal("logFlushScheduled should start false (loop not armed at rest)")
	}

	// A log batch arrives → the flush arms and the lines buffer.
	app.Update(LogBatchMsg{Lines: []string{"line1", "line2"}})
	if !app.logFlushScheduled {
		t.Fatal("expected logFlushScheduled=true after a log batch arrived")
	}
	if len(app.logBuffer) != 2 {
		t.Fatalf("expected 2 buffered lines, got %d", len(app.logBuffer))
	}

	// A second batch while one is pending must not double-arm.
	app.Update(LogBatchMsg{Lines: []string{"line3"}})
	if len(app.logBuffer) != 3 {
		t.Fatalf("expected 3 buffered lines, got %d", len(app.logBuffer))
	}
	if app.scheduleLogFlush() != nil {
		t.Fatal("scheduleLogFlush must be a no-op while a flush is already pending")
	}

	// The flush drains the buffer and disarms so idle periods run no ticks.
	app.Update(logFlushMsg{})
	if app.logFlushScheduled {
		t.Fatal("expected logFlushScheduled=false after the flush")
	}
	if len(app.logBuffer) != 0 {
		t.Fatalf("expected the buffer drained, got %d", len(app.logBuffer))
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

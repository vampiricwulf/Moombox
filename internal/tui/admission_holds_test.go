package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// TestQueuedRowsShowTheirHold pins owner ruling R-D3 in the TUI: a Queued
// row's task-list tag and its details "Queue" line say why the scheduler is
// not admitting it, from the AdmissionHoldsMsg snapshot, by the one
// precedence (jobstate.HoldPrecedence); a row only waiting its turn has no
// tag and reads "Waiting in queue...".
//
// Mutants: the Queued arm of progressCellText dropped; the AdmissionHoldsMsg
// case not storing the snapshot; the details Queue line not added.
func TestQueuedRowsShowTheirHold(t *testing.T) {
	later := time.Now().Add(20 * time.Minute)
	yt := &database.Job{ID: "q", Platform: "youtube", Status: database.StatusQueued, Title: "Q"}
	for _, tc := range []struct {
		name  string
		holds jobstate.AdmissionHolds
		tag   string
		line  string
	}{
		{"nothing", jobstate.AdmissionHolds{}, "", "Waiting in queue..."},
		{"offline", jobstate.AdmissionHolds{Offline: true, DiskHeld: true}, "⏸ offline ", "Held: offline"},
		{"disk", jobstate.AdmissionHolds{DiskHeld: true, RateLimitedUntil: later}, "⏸ disk ", "Held: disk full"},
		{"rate limit", jobstate.AdmissionHolds{RateLimitedUntil: later}, "⏸ rate limit ", "Held: YouTube rate limit until " + later.Format("15:04")},
		{"retry", jobstate.AdmissionHolds{Jobs: map[string]jobstate.BacklogHold{"q": {Attempt: 2, Of: 3, Until: later}}},
			"⟳ 2/3 ", "Retry 2 of 3 at " + later.Format("15:04")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp()
			a.Update(AdmissionHoldsMsg{Holds: tc.holds})
			if text, _ := a.taskList.progressCellText(yt); text != tc.tag {
				t.Errorf("row tag = %q, want %q", text, tc.tag)
			}
			a.details.SetSize(120, 40)
			a.details.SetJob(yt)
			var got string
			for _, r := range a.details.rows {
				if r.kind == rowField && r.label == "Queue" {
					got = r.value
				}
			}
			if got != tc.line {
				t.Errorf("details Queue = %q, want %q", got, tc.line)
			}
		})
	}
}

// TestDiskIndicatorSaysHoldingWhileTheGateHolds: the backlog's disk gate
// holds until usage is back under the recovery margin, so a reading that has
// stepped down to "warn" still holds admission, and the bar says "holding"
// then (the dashboard's gauge does the same); at critical the red says it,
// and once the gate reopens the word goes.
//
// Mutant: SetDiskHeld not consulted by the disk indicator.
func TestDiskIndicatorSaysHoldingWhileTheGateHolds(t *testing.T) {
	m := NewStatusBarModel()
	m.SetDiskStatus(12*1024*1024*1024, 94, "warn")
	m.SetWidth(200)
	m.SetDiskHeld(true)
	if out := stripANSI(m.View()); !strings.Contains(out, "holding") {
		t.Errorf("a held gate at warn: %q, want holding", out)
	}
	m.SetDiskHeld(false)
	if out := stripANSI(m.View()); strings.Contains(out, "holding") {
		t.Errorf("an open gate still says holding: %q", out)
	}
	m.SetDiskStatus(12*1024*1024*1024, 96, "critical")
	m.SetDiskHeld(true)
	if out := stripANSI(m.View()); strings.Contains(out, "holding") {
		t.Errorf("critical already says it; holding added: %q", out)
	}
}

// TestDiskIndicatorMarksHoldingAtEveryTier: the narrow tiers shorten the disk
// reading, and a held gate at warn must still read differently from an
// ordinary warn there — the dashboard's indicator always says it (owner
// ruling R-D3). The short forms carry a ⏸ marker; critical carries none,
// as the red says it already.
//
// Mutant: the compact or tight form without the marker.
func TestDiskIndicatorMarksHoldingAtEveryTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier barTier
		held string
		open string
	}{
		{"compact", tierCompact, "D:94% 12G⏸", "D:94% 12G"},
		{"tight", tierTight, "D:94%⏸", "D:94%"},
		{"essential", tierEssential, "D:94%⏸", "D:94%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewStatusBarModel()
			m.SetDiskStatus(12*1024*1024*1024, 94, "warn")
			m.SetDiskHeld(true)
			if out := stripANSI(m.renderMetrics(tc.tier, barJobCounts{})); !strings.Contains(out, tc.held) {
				t.Errorf("held at warn: %q, want %q", out, tc.held)
			}
			m.SetDiskHeld(false)
			if out := stripANSI(m.renderMetrics(tc.tier, barJobCounts{})); !strings.Contains(out, tc.open) || strings.Contains(out, "⏸") {
				t.Errorf("open at warn: %q, want %q with no marker", out, tc.open)
			}
			m.SetDiskStatus(12*1024*1024*1024, 96, "critical")
			m.SetDiskHeld(true)
			if out := stripANSI(m.renderMetrics(tc.tier, barJobCounts{})); strings.Contains(out, "⏸") {
				t.Errorf("critical already says it; marker added: %q", out)
			}
		})
	}
}

// TestInitAsksForTheHoldsAgain: the TUI is seeded with the holds before Run,
// and a change between that seed and Run went through Send while it was a
// no-op. Init's commands run once the program does; one of them asks the
// push for the snapshot again (OnAdmissionHoldsResync), and none is added
// without the hook.
//
// Mutants: Init not batching the resync command; the command not calling
// the hook.
func TestInitAsksForTheHoldsAgain(t *testing.T) {
	without := NewApp()
	batchLen := func(a *App) int {
		t.Helper()
		msg := a.Init()()
		b, ok := msg.(tea.BatchMsg)
		if !ok {
			t.Fatalf("Init returned %T, want a tea.BatchMsg", msg)
		}
		return len(b)
	}
	base := batchLen(without)
	if without.admissionHoldsResyncCmd() != nil {
		t.Fatal("no hook: want no resync command")
	}

	with := NewApp()
	calls := 0
	with.OnAdmissionHoldsResync = func() { calls++ }
	if got := batchLen(with); got != base+1 {
		t.Errorf("Init batch with the hook = %d commands, want %d", got, base+1)
	}
	if msg := with.admissionHoldsResyncCmd()(); msg != nil {
		t.Errorf("resync command returned %T, want nil", msg)
	}
	if calls != 1 {
		t.Errorf("hook calls = %d, want 1", calls)
	}
}

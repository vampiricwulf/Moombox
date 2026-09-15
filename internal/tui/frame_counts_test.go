package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The header's icon counts come from the cache the rebuild fills, not from a
// fresh walk per frame. The probe is a job mutated IN PLACE, which no
// production path does without a rebuild: if the second View() reports the
// new status, renderHeader recomputed instead of reading m.statusSummary.
func TestHeaderStatusSummaryIsCached(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 10)
	job := &database.Job{ID: "a", Title: "A", Status: database.StatusLive}
	m.SetJobs([]*database.Job{job})

	liveIcon := "1" + StatusIcon(string(database.StatusLive))
	if got := stripANSI(m.View()); !strings.Contains(got, liveIcon) {
		t.Fatalf("header must show %q, got:\n%s", liveIcon, got)
	}

	job.Status = database.StatusFinished
	if got := stripANSI(m.View()); !strings.Contains(got, liveIcon) {
		t.Errorf("renderHeader recomputed the summary instead of reading the cache:\n%s", got)
	}

	// Any real change goes through a rebuild, which refreshes the cache.
	m.SetJobs([]*database.Job{job})
	doneIcon := "1" + StatusIcon(string(database.StatusFinished))
	if got := stripANSI(m.View()); !strings.Contains(got, doneIcon) {
		t.Errorf("a rebuild must refresh the summary, want %q, got:\n%s", doneIcon, got)
	}
}

// ResweepArchive refreshes the summary even when it does not rebuild: the
// header counts every job, while the sweep's dirty check only looks at rows
// that pass the filter, so a hidden Finished job aging past the boundary
// moves the counts without dirtying the buckets. This is also the one-minute
// staleness bound on the cache.
//
// Mutant: putting the refresh inside the `if dirty` branch.
func TestResweepArchiveRefreshesTheSummary(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 10)
	job := &database.Job{ID: "a", Title: "A", Status: database.StatusLive}
	m.SetJobs([]*database.Job{job})

	job.Status = database.StatusMuxing
	if m.ResweepArchive() {
		t.Fatal("nothing crossed the archive boundary; the sweep must not rebuild")
	}
	want := "1" + StatusIcon(string(database.StatusMuxing))
	if got := stripANSI(m.View()); !strings.Contains(got, want) {
		t.Errorf("the sweep must refresh the summary unconditionally, want %q, got:\n%s", want, got)
	}
}

// tallyJobs is the one walk per frame. Empty Platform counts as YouTube —
// the rule parkedCookieJobs documents and every other platform test in the
// TUI follows.
func TestTallyJobsCountsOnce(t *testing.T) {
	m := NewStatusBarModel()
	m.SetJobs([]*database.Job{
		{Status: database.StatusDownloading},
		{Status: database.StatusLive},
		{Status: database.StatusMuxing},
		{Status: database.StatusQueued},
		{Status: database.StatusFinished},
		{Status: database.StatusCookies, Platform: ""},
		{Status: database.StatusCookies, Platform: "twitch"},
	})
	got := m.tallyJobs()
	if got.active != 3 {
		t.Errorf("active = %d, want 3 (Downloading+Live+Muxing; Queued is waiting for a slot)", got.active)
	}
	if !got.ytParked || !got.twParked {
		t.Errorf("parked = (yt %v, tw %v), want both — an empty Platform counts as YouTube", got.ytParked, got.twParked)
	}
}

// The renderers read the tally they are handed. Mutant: either one walking
// m.jobs again — with no jobs at all, a re-derived tally renders nothing.
func TestStatusBarRenderersReadThePassedTally(t *testing.T) {
	m := NewStatusBarModel()
	m.SetWidth(200)
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)

	if got := stripANSI(m.renderMetrics(tierFull, barJobCounts{active: 7})); !strings.Contains(got, "Active: 7") {
		t.Errorf("renderMetrics re-derived the active count from m.jobs, got %q", got)
	}
	got := m.renderCookieStatus(tierFull, barJobCounts{ytParked: true})
	if !strings.Contains(got, statusBarRedStyle.Render("YT")) {
		t.Errorf("renderCookieStatus re-derived the park from m.jobs, got %q", stripANSI(got))
	}
	if !strings.Contains(got, statusBarGrnStyle.Render("TW")) {
		t.Errorf("only the parked platform escalates; TW must stay green, got %q", stripANSI(got))
	}
}

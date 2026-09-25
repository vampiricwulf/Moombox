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

// tallyJobs is the one walk per SetJobs. Empty Platform counts as YouTube —
// the rule parkedCookieJobs documents and every other platform test in the
// TUI follows.
//
// Mutant: counting Queued as active (it is admitted to Upcoming by the
// worker's archive-slots scheduler, so it is waiting, not running);
// attributing an empty Platform to Twitch, or to neither, which loses the
// YouTube re-login prompt for every job written before the column existed.
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

// twoDownloading returns two distinct Downloading jobs from the fixture, the
// rows both status pins below transition.
func twoDownloading(t *testing.T, a *App) []*database.Job {
	t.Helper()
	var pick []*database.Job
	for _, j := range a.taskList.Jobs() {
		if j.Status == database.StatusDownloading {
			pick = append(pick, j)
			if len(pick) == 2 {
				return pick
			}
		}
	}
	t.Fatalf("fixture: want two Downloading jobs, found %d", len(pick))
	return nil
}

// A live status transition must re-tally the status bar. Now that the tally
// is stored rather than recomputed per frame, the bar is only as fresh as its
// last SetJobs — and the routine transition path never called it:
// handleJobUpdate applies the change through TaskListModel.UpdateJob, which
// writes m.jobs[idx] = job into the backing array statusBar.jobs ALIASES
// (TaskListModel.Jobs returns the live storage). A per-frame tally saw that
// write on the next frame; a stored one does not. Measured before the fix:
// Active: 200 -> 200 on a Downloading -> Finished, and the B1 badge never lit.
//
// Both halves of barJobCounts are pinned, because both froze: the active
// counter and the parked-COOKIES? attribution.
//
// MUTANT: delete the statusBar.SetJobs call from handleJobUpdate's
// hasDisplayChange branch — the bar stays at Active: 200 and YT stays unlit.
func TestJobStatusTransitionRetalliesTheStatusBar(t *testing.T) {
	a := frameBenchApp(t)
	// The auth badges render only for a configured platform, and the fixture
	// configures none; ytParked is half of what this test pins.
	a.statusBar.SetActivePlatforms(true, false)
	pick := twoDownloading(t, a)

	if got := stripANSI(a.statusBar.View()); !strings.Contains(got, "Active: 200") {
		t.Fatalf("fixture: want 200 active downloads, got:\n%s", got)
	}

	// The counter. A fresh Job pointer with a new status, exactly as
	// UpdateJobFields hands one to the OnJobUpdate subscriber.
	fin := *pick[0]
	fin.Status = database.StatusFinished
	a.handleJobUpdate(&database.JobChange{Job: &fin, Changes: []string{"status"}})
	if got := stripANSI(a.statusBar.View()); !strings.Contains(got, "Active: 199") {
		t.Errorf("a Downloading -> Finished transition left the bar's tally frozen; want \"Active: 199\":\n%s", got)
	}

	// The B1 parked badge, which is the alert the whole attribution rule
	// exists for: a download stopped for want of usable credentials.
	park := *pick[1]
	park.Status = database.StatusCookies
	a.handleJobUpdate(&database.JobChange{Job: &park, Changes: []string{"status"}})
	if !a.statusBar.counts.ytParked {
		t.Error("a job parking in COOKIES? left counts.ytParked false — the tally was not refreshed")
	}
	if !strings.Contains(a.statusBar.View(), statusBarRedStyle.Render("YT")) {
		t.Errorf("a parked job must escalate the YT badge to red (B1):\n%s", stripANSI(a.statusBar.View()))
	}
	if got := stripANSI(a.statusBar.View()); !strings.Contains(got, "Active: 198") {
		t.Errorf("a park also leaves the active set; want \"Active: 198\":\n%s", got)
	}
}

// The other side of the same gate, and the one the ~60 Hz rule cares about:
// an update that moves no column the tally reads must NOT re-tally. Cheaper,
// never rarer — the transition above re-tallies once, these do not at all.
//
// Two rows, and they are stopped by two different things, which is why both
// are here:
//
//   - progress lands ~10 times a second per active download and is not a
//     display column, so it never reaches the hasDisplayChange branch. This
//     row pins that, and would survive the gate being deleted.
//   - a title write IS a display column: it rebuilds the row, reaches the
//     branch, and is stopped only by the change-key gate. This row is the
//     gate's pin. chat_status is the same shape and does fire during a live
//     download.
//
// The sentinel is a value tallyJobs can never produce, so its survival is
// proof no SetJobs ran; the positive control at the end proves the sentinel
// can actually be cleared, so a green result here is never vacuous.
//
// MUTANT: drop the change-key gate (call SetJobs for every display change) —
// the title row fails, and a walk of the whole job list returns to every
// row-rebuilding write.
func TestNonTallyUpdatesLeaveTheStatusBarTallyAlone(t *testing.T) {
	a := frameBenchApp(t)
	job := twoDownloading(t, a)[0]

	const sentinel = -1

	for _, tc := range []struct {
		name    string
		changes []string
		mutate  func(j *database.Job)
	}{
		{"progress tick", []string{"progress", "percent", "speed", "last_video_seq"}, func(j *database.Job) {
			j.Progress, j.Percent = "V:9999 A:9999", 42
		}},
		{"title rewrite", []string{"title"}, func(j *database.Job) { j.Title = "A retitled stream" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a.statusBar.counts.active = sentinel
			upd := *job
			tc.mutate(&upd)
			a.handleJobUpdate(&database.JobChange{Job: &upd, Changes: tc.changes})
			if a.statusBar.counts.active != sentinel {
				t.Errorf("a %s re-tallied the whole job list: counts.active is %d, want the "+
					"untouched sentinel %d", tc.name, a.statusBar.counts.active, sentinel)
			}
		})
	}

	// Positive control: the same sentinel IS cleared by a real transition.
	fin := *job
	fin.Status = database.StatusFinished
	a.handleJobUpdate(&database.JobChange{Job: &fin, Changes: []string{"status"}})
	if a.statusBar.counts.active == sentinel {
		t.Error("a status transition did not re-tally either — the assertions above cannot fail, " +
			"so they pin nothing")
	}
}

// tallyProbe is one column's claim: mutate writes the field that column
// carries, and moves says whether the status bar's stored tally is allowed to
// follow it. The fixture is per-row because platform only moves the tally for
// a job that is already parked — parkedCookieJobs filters on Status first, so
// a platform flip on a Downloading job is correctly invisible, and a probe
// that used one shared fixture would report platform as a non-input.
type tallyProbe struct {
	column string
	job    func() *database.Job
	mutate func(*database.Job)
	moves  bool
}

// TestTallyColumnsMatchWhatTheTallyReads is the coupling pin tallyColumns
// exists for. Arc C's fix gated handleJobUpdate's SetJobs call on two keys
// typed out beside the call; nothing tied those keys to tallyJobs or
// parkedCookieJobs, so a new field in barJobCounts could quietly freeze the
// bar for whatever column feeds it and every test would stay green.
//
// Both directions are checked, per column. The DERIVATION: does tallyJobs'
// answer actually move when this field moves? The SET: is the column in
// tallyColumns? They must agree, so a column can neither sit in the set
// without being an input nor be an input without sitting in the set. The
// count check at the end closes the third hole — a column added to the set
// with no probe beside it.
//
// Mutants this kill: deleting "platform" from tallyColumns (the platform
// row's set half fires); adding any of the eleven other display columns to it
// (that row's set half fires, and TestNonTallyUpdatesLeaveTheStatusBarTally
// Alone/title_rewrite fires too for "title"); adding a column to the set with
// no probe (the count check fires).
func TestTallyColumnsMatchWhatTheTallyReads(t *testing.T) {
	downloading := func() *database.Job {
		return &database.Job{ID: "a", Status: database.StatusDownloading, Platform: "youtube"}
	}
	probes := []tallyProbe{
		// The two inputs.
		{"status", downloading, func(j *database.Job) { j.Status = database.StatusFinished }, true},
		{"platform", func() *database.Job {
			return &database.Job{ID: "a", Status: database.StatusCookies, Platform: ""}
		}, func(j *database.Job) { j.Platform = "twitch" }, true},

		// The eleven other display columns, each moved on a job the tally
		// does count, so a false positive would show.
		{"title", downloading, func(j *database.Job) { j.Title = "retitled" }, false},
		{"channel_name", downloading, func(j *database.Job) { j.ChannelName = "other" }, false},
		{"thumbnail_url", downloading, func(j *database.Job) { j.ThumbnailURL = "https://example.test/t.jpg" }, false},
		{"description", downloading, func(j *database.Job) { j.Description = "some text" }, false},
		{"stream_start_time", downloading, func(j *database.Job) { j.StreamStartTime = "2026-09-24T00:00:00Z" }, false},
		{"stream_end_time", downloading, func(j *database.Job) { j.StreamEndTime = "2026-09-24T01:00:00Z" }, false},
		{"error", downloading, func(j *database.Job) { j.Error = "boom" }, false},
		{"output_file", downloading, func(j *database.Job) { j.OutputFile = "D:/out/show.mp4" }, false},
		{"filename", downloading, func(j *database.Job) { j.Filename = "show.mp4" }, false},
		{"is_vod", downloading, func(j *database.Job) { j.IsVod = true }, false},
		{"chat_status", downloading, func(j *database.Job) { j.ChatStatus = "incomplete" }, false},

		// Not a display column at all, and the ~10/sec one: it must not move
		// the tally either, or the 60 Hz rule is broken at the source.
		{"progress", downloading, func(j *database.Job) { j.Progress, j.Percent = "V:9 A:9", 42 }, false},
	}

	inputs := 0
	for _, p := range probes {
		if p.moves {
			inputs++
		}
		t.Run(p.column, func(t *testing.T) {
			m := NewStatusBarModel()
			job := p.job()
			m.SetJobs([]*database.Job{job})

			before := m.tallyJobs()
			p.mutate(job)
			after := m.tallyJobs()
			if moved := after != before; moved != p.moves {
				t.Errorf("mutating the field %s carries moved the tally = %v, want %v (before %+v, after %+v)",
					p.column, moved, p.moves, before, after)
			}

			_, inSet := tallyColumns[p.column]
			if inSet != p.moves {
				t.Errorf("tallyColumns[%q] = %v but the derivation says the tally does%s follow it — "+
					"the gate and tallyJobs/parkedCookieJobs disagree",
					p.column, inSet, map[bool]string{true: "", false: " not"}[p.moves])
			}
		})
	}

	if len(tallyColumns) != inputs {
		t.Errorf("tallyColumns has %d entries but only %d probed columns move the tally — a column was "+
			"added to the set with no probe beside it, so nothing checks that it is really an input",
			len(tallyColumns), inputs)
	}
}

// TestHasTallyChange is the gate's own table, the twin of TestHasDisplayChange,
// and the reason handleJobUpdate can read a named set instead of spelling two
// keys out at the call site.
//
// Mutant this kills: hasTallyChange returning true on the first key it sees
// rather than on a key in the set — "title only" and "unknown column" fire.
func TestHasTallyChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []string
		want    bool
	}{
		{"nil", nil, false},
		{"empty slice", []string{}, false},
		{"progress tick", []string{"progress", "percent", "speed", "eta"}, false},
		{"title only", []string{"title"}, false},
		{"status transition", []string{"status"}, true},
		{"platform", []string{"platform"}, true},
		{"mixed: progress + status", []string{"progress", "status", "eta"}, true},
		{"mixed: title + platform", []string{"title", "platform"}, true},
		{"unknown column", []string{"some_unknown_column"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasTallyChange(tc.changes); got != tc.want {
				t.Errorf("hasTallyChange(%v) = %v, want %v", tc.changes, got, tc.want)
			}
		})
	}
}

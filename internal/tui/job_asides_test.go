package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// asideRows returns the details panel's Set-aside Recordings block: its header
// row and every field row after it, up to the next separator.
func asideRows(m *JobDetailsModel) []detailRow {
	var out []detailRow
	in := false
	for _, r := range m.rows {
		if r.kind == rowHeader && strings.HasPrefix(r.label, "Set-aside Recordings") {
			in = true
			out = append(out, r)
			continue
		}
		if !in {
			continue
		}
		if r.kind == rowSeparator || r.kind == rowHeader {
			break
		}
		out = append(out, r)
	}
	return out
}

// TestDetailsPanelListsSetAsideRecordings is the TUI half of D-1: the terminal
// must say the same thing the dashboard does about a preserved staging dir.
//
// Mutants this kills:
//   - no section at all: asideRows is empty and the TUI stays silent about
//     footage the Web names.
//   - the count dropped from the header: the header assertion fails, and the
//     operator cannot tell one restart from five without counting rows.
//   - the sidecar state dropped: both recordings read the same, so the row
//     says nothing the count did not already say.
func TestDetailsPanelListsSetAsideRecordings(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(100, 40)
	m.SetJob(&database.Job{ID: "j1", Title: "t", VideoID: "v", Status: database.StatusFinished, Platform: "youtube"})
	m.SetAsides(AsideSummary{
		Asides: []AsideEntry{
			{Timestamp: "2023-11-14T22:13:20Z", Size: 536870912, HasResumeSidecar: true},
			{Timestamp: "2023-11-14T22:15:00Z", Size: 1048576, HasResumeSidecar: false},
		},
		KeptChatSidecar: true,
	})

	rows := asideRows(m)
	if len(rows) == 0 {
		t.Fatal("the details panel shows no Set-aside Recordings section for a job that has two")
	}
	if rows[0].label != "Set-aside Recordings (2)" {
		t.Errorf("header = %q, want %q", rows[0].label, "Set-aside Recordings (2)")
	}

	body := ""
	for _, r := range rows[1:] {
		body += r.label + " " + r.value + "\n"
	}
	// formatFileSize is utils.FormatFileSize, which emits "512.0MB" with no
	// space. Assert what the shared formatter produces — never a prettier
	// spacing, and never a second size formatter to produce it.
	for _, want := range []string{"512.0MB", "1.0MB", "resume sidecar", "no resume sidecar", "kept in staging"} {
		if !strings.Contains(body, want) {
			t.Errorf("the section does not mention %q; rows were:\n%s", want, body)
		}
	}
}

// TestDetailsPanelShowsNoSectionWithoutAsides keeps the panel unchanged for
// the overwhelming majority of jobs.
//
// Mutant: emitting the header unconditionally — every job in the fleet grows
// an empty section, and the chat-status and progress-gate tests' row indices
// move with it.
func TestDetailsPanelShowsNoSectionWithoutAsides(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(100, 40)
	m.SetJob(&database.Job{ID: "j1", Title: "t", VideoID: "v", Status: database.StatusFinished, Platform: "youtube"})
	if rows := asideRows(m); len(rows) != 0 {
		t.Errorf("a job with no asides grew a %d-row Set-aside Recordings section", len(rows))
	}
	m.SetAsides(AsideSummary{KeptChatSidecar: true})
	if rows := asideRows(m); len(rows) != 0 {
		t.Errorf("a kept chat capture with no asides grew a %d-row section — there is nothing to recover", len(rows))
	}
}

// TestSelectingAnotherJobDropsTheOldSummary: SetJob is the only thing that
// knows the selection moved, so it has to clear a summary that belongs to the
// job the cursor just left.
//
// Mutant: keeping m.asides across a job switch — the panel shows job A's
// recordings under job B until the next probe lands, which is the worst
// possible lie on a screen whose whole purpose is telling you what is on disk.
func TestSelectingAnotherJobDropsTheOldSummary(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(100, 40)
	m.SetJob(&database.Job{ID: "j1", Title: "t", VideoID: "v", Status: database.StatusFinished, Platform: "youtube"})
	m.SetAsides(AsideSummary{Asides: []AsideEntry{{Timestamp: "2023-11-14T22:13:20Z", Size: 1, HasResumeSidecar: false}}})
	if len(asideRows(m)) == 0 {
		t.Fatal("the fixture did not render a section to begin with")
	}

	m.SetJob(&database.Job{ID: "j2", Title: "t2", VideoID: "v2", Status: database.StatusFinished, Platform: "youtube"})
	if rows := asideRows(m); len(rows) != 0 {
		t.Errorf("job j2 inherited j1's %d-row section", len(rows))
	}
}

// TestAsidesForProbesOncePerSelectedJob: updateSelectedJob runs on every
// cursor move AND on every JobsUpdateMsg, so an unmemoised probe turns a busy
// fleet into a ReadDir per database write — the CORE-9 failure mode one level
// down.
//
// Mutants this kills:
//   - dropping the memo: the counter climbs on every call.
//   - dropping the active-status gate: a Downloading job's churning staging
//     dir is read while it is being written.
//   - dropping invalidateAsides: a completed recovery keeps rendering the
//     recordings it just consumed.
//   - keying the memo on "something has been probed" rather than on the job
//     (`a.asidesJobID != ""`): the second inactive job the cursor lands on
//     renders the FIRST job's recordings, which is the same lie
//     TestSelectingAnotherJobDropsTheOldSummary pins one level up. The probe
//     stub answers with the ID it was handed so the payload, not just the
//     counter, names the job it came from.
func TestAsidesForProbesOncePerSelectedJob(t *testing.T) {
	a := NewApp()
	probes := 0
	a.JobAsides = func(id string) AsideSummary {
		probes++
		return AsideSummary{Asides: []AsideEntry{{Timestamp: id, Size: 1}}}
	}

	done := &database.Job{ID: "j1", Title: "t", Status: database.StatusFinished, Platform: "youtube"}
	if got := a.asidesFor(done); len(got.Asides) != 1 {
		t.Fatalf("asidesFor returned %+v, want the probe's answer", got)
	}
	a.asidesFor(done)
	a.asidesFor(done)
	if probes != 1 {
		t.Errorf("asidesFor probed %d times for one job, want 1", probes)
	}

	live := &database.Job{ID: "j2", Title: "t", Status: database.StatusDownloading, Platform: "youtube"}
	if got := a.asidesFor(live); len(got.Asides) != 0 {
		t.Errorf("asidesFor on a Downloading job returned %+v, want an empty summary with no probe", got)
	}
	if probes != 1 {
		t.Errorf("asidesFor probed an active job (%d probes total)", probes)
	}

	a.invalidateAsides("j1")
	a.asidesFor(done)
	if probes != 2 {
		t.Errorf("asidesFor did not re-probe after invalidateAsides (%d probes)", probes)
	}

	// A second INACTIVE job: the memo holds one job's answer, so landing on
	// another one has to probe again AND answer with that job's recordings.
	other := &database.Job{ID: "j3", Title: "t3", Status: database.StatusCancelled, Platform: "youtube"}
	if got := a.asidesFor(other); len(got.Asides) != 1 || got.Asides[0].Timestamp != "j3" {
		t.Errorf("selecting a second inactive job returned %+v — the memo served the previous job's recordings", got)
	}
	if probes != 3 {
		t.Errorf("asidesFor probed %d times over two inactive jobs and one invalidation, want 3", probes)
	}
	// ...and the new job's answer is memoised in its turn.
	if got := a.asidesFor(other); probes != 3 || got.Asides[0].Timestamp != "j3" {
		t.Errorf("the second job's answer was not memoised: %d probes, %+v", probes, got)
	}
}

// TestRecoverAsidesChordIsOneMenuEntry pins the chord table's single-source
// rule for the new verb, and the gate it carries.
//
// Mutants this kills:
//   - a JobFilter that does not consult JobAsides: a job with nothing set
//     aside is offered, and the operator picks a row that can only fail.
//   - a JobFilter that ignores status: an active job is offered.
//   - NeedsConfirm dropped: a single keystroke starts an FFmpeg.
func TestRecoverAsidesChordIsOneMenuEntry(t *testing.T) {
	a := NewApp()
	a.JobAsides = func(id string) AsideSummary {
		if id == "has" {
			return AsideSummary{Asides: []AsideEntry{{Timestamp: "2023-11-14T22:13:20Z", Size: 1}}}
		}
		return AsideSummary{}
	}

	items := a.buildMenuItems()
	var item *ActionMenuItem
	seen := 0
	for i := range items {
		if items[i].Chord == "A S" {
			item = &items[i]
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("buildMenuItems has %d entries for A S, want exactly 1", seen)
	}
	if !item.NeedsJob || !item.NeedsConfirm || item.SupportsBatch {
		t.Errorf("A S = {NeedsJob:%v NeedsConfirm:%v SupportsBatch:%v}, want {true true false}",
			item.NeedsJob, item.NeedsConfirm, item.SupportsBatch)
	}
	if item.JobFilter == nil || item.StatusFilter == nil {
		t.Fatal("A S must carry BOTH filters: the cheap status-only twin for open, the probing one for selection")
	}

	has := &database.Job{ID: "has", Title: "t", Status: database.StatusFinished, Platform: "youtube"}
	none := &database.Job{ID: "none", Title: "t", Status: database.StatusFinished, Platform: "youtube"}
	live := &database.Job{ID: "has", Title: "t", Status: database.StatusDownloading, Platform: "youtube"}

	if !item.JobFilter(has) {
		t.Error("A S refuses a terminal job that HAS set-aside recordings")
	}
	if item.JobFilter(none) {
		t.Error("A S offers a job with nothing set aside")
	}
	if item.JobFilter(live) {
		t.Error("A S offers an active job")
	}
	if !item.StatusFilter(has) || item.StatusFilter(live) {
		t.Error("A S's StatusFilter must pass a terminal job and refuse an active one, on status alone")
	}
}

// TestRecoverAsidesChordDispatchesAndInvalidates: the chord calls the callback
// and drops the memo, so the panel stops offering recordings the recovery is
// consuming.
//
// Mutants this kills:
//   - no dispatch case: the callback is never called and the chord is inert.
//   - the error swallowed: a refusal (an active job, a second run) reports
//     "Recovering…" at an operator whose recovery never started.
//   - invalidateAsides dropped: the stale summary keeps rendering.
func TestRecoverAsidesChordDispatchesAndInvalidates(t *testing.T) {
	job := &database.Job{ID: "j1", Title: "Some Stream", Status: database.StatusFinished, Platform: "youtube"}

	t.Run("accepted", func(t *testing.T) {
		a := NewApp()
		a.JobAsides = func(string) AsideSummary {
			return AsideSummary{Asides: []AsideEntry{{Timestamp: "2023-11-14T22:13:20Z", Size: 1}}}
		}
		called := ""
		a.OnRecoverAsides = func(id string) error { called = id; return nil }
		a.asidesFor(job) // seed the memo

		a.dispatchAction("A S", job)
		if called != "j1" {
			t.Errorf("OnRecoverAsides called with %q, want %q", called, "j1")
		}
		if a.asidesJobID != "" {
			t.Error("the aside memo survived a dispatched recovery")
		}
		if !strings.Contains(a.feedback.msg, "Some Stream") {
			t.Errorf("feedback = %q, want it to name the job", a.feedback.msg)
		}
	})

	t.Run("refused", func(t *testing.T) {
		a := NewApp()
		a.OnRecoverAsides = func(string) error { return errTestRefused }
		a.dispatchAction("A S", job)
		if !strings.Contains(a.feedback.msg, "refused by the worker") {
			t.Errorf("feedback = %q, want the refusal surfaced", a.feedback.msg)
		}
	})
}

// TestOrphanOverlayCountsAsides: the sweep has named a staging dir's set-aside
// recordings since sweep-2 and the TUI's entry type had nowhere to put them,
// so the terminal offered captured footage as if it were scratch space.
//
// Mutant: dropping the count from the row suffix — the row is
// indistinguishable from an ordinary staging orphan.
func TestOrphanOverlayCountsAsides(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()
	m.SetFiles([]OrphanedFileEntry{
		{Path: "D:\\staging\\job-9", RelPath: "job-9", Type: "staging", Size: 4096, Modified: "2023-11-14T22:13:20Z",
			Asides: []string{"video.mp4.restart-1700000000", "audio_stream.restart-1700000000"}},
	})
	m.SetHistory(nil)

	if view := m.View(); !strings.Contains(view, "2 asides") {
		t.Errorf("the orphan overlay does not count the set-aside recordings; view:\n%s", view)
	}
}

// errTestRefused stands in for any of the worker's typed refusals; the TUI
// only ever renders the message.
var errTestRefused = errors.New("refused by the worker")

// TestAsidesMemoFollowsTheJobsOwnTransitions: the memo was keyed on the job ID
// alone, and an active job answered empty WITHOUT touching it, so a job that
// was resumed, set a recording aside and failed again still showed the "none"
// probed before the resume — while A S, which probes afresh, offered the
// recovery the panel said was not there.
//
// Mutants: key the memo on the ID only (the second terminal visit is served
// from the memo); stop dropping it while the job is active (same).
func TestAsidesMemoFollowsTheJobsOwnTransitions(t *testing.T) {
	a := NewApp()
	asides := 0
	probes := 0
	a.JobAsides = func(id string) AsideSummary {
		probes++
		s := AsideSummary{}
		for range asides {
			s.Asides = append(s.Asides, AsideEntry{Timestamp: id, Size: 1})
		}
		return s
	}

	job := &database.Job{ID: "x", Title: "t", Status: database.StatusError, Platform: "youtube", UpdatedAt: "2026-10-04T10:00:00Z"}
	if got := a.asidesFor(job); len(got.Asides) != 0 {
		t.Fatalf("premise lost: %+v", got)
	}
	// Resumed: the panel refreshes while it downloads.
	job.Status, job.UpdatedAt = database.StatusDownloading, "2026-10-04T10:01:00Z"
	a.asidesFor(job)
	// The engine sets a recording aside; the job fails again.
	asides = 1
	job.Status, job.UpdatedAt = database.StatusError, "2026-10-04T10:05:00Z"
	if got := a.asidesFor(job); len(got.Asides) != 1 {
		t.Errorf("after resume → aside → Error the panel shows %d asides, want 1 (probes=%d)", len(got.Asides), probes)
	}
	// And an unchanged row is still served from the memo.
	before := probes
	a.asidesFor(job)
	if probes != before {
		t.Errorf("an unchanged row re-probed (%d → %d)", before, probes)
	}
}

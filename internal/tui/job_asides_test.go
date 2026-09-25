package tui

import (
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
func TestAsidesForProbesOncePerSelectedJob(t *testing.T) {
	a := NewApp()
	probes := 0
	a.JobAsides = func(string) AsideSummary {
		probes++
		return AsideSummary{Asides: []AsideEntry{{Timestamp: "2023-11-14T22:13:20Z", Size: 1}}}
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
}

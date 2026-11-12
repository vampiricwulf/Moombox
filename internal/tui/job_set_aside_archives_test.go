package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// setAsideArchiveRows is the details panel's "Set Aside by Reinitialize"
// block: its header row and the rows under it.
func setAsideArchiveRows(m *JobDetailsModel) []detailRow {
	for i, r := range m.rows {
		if r.kind != rowHeader || !strings.HasPrefix(r.label, "Set Aside by Reinitialize") {
			continue
		}
		end := i + 1
		for end < len(m.rows) && m.rows[end].kind == rowField {
			end++
		}
		return m.rows[i:end]
	}
	return nil
}

// The details panel lists the archives a Reinitialize of the job set aside
// (owner ruling R2-W1b), each on the line the dashboard shows
// (setAsideArchiveText, pinned on the same file and size in
// web/tests/job-set-aside-archives.test.mjs), and drops the block when the
// cursor moves to another job.
//
// Mutants: no block; the list not cleared by SetJob on a job switch (the next
// job shows this one's files).
func TestDetailsPanelListsTheArchivesAReinitializeSetAside(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(80, 40)
	m.SetJob(&database.Job{ID: "j1", Title: "t", VideoID: "v", Status: database.StatusUpcoming, Platform: "youtube"})
	if rows := setAsideArchiveRows(m); len(rows) != 0 {
		t.Fatalf("a job with no set-aside archive grew a %d-row block", len(rows))
	}

	m.SetSetAsideArchives([]SetAsideArchiveEntry{{Name: "Show.restart-1700000000.mp4", Size: 3 << 30}})
	rows := setAsideArchiveRows(m)
	if len(rows) < 2 || rows[0].label != "Set Aside by Reinitialize (1)" {
		t.Fatalf("rows = %+v, want the block with its count", rows)
	}
	var joined string
	for _, r := range rows[1:] {
		joined += r.value
	}
	if want := "Show.restart-1700000000.mp4 (3.0GB, kept, never replaced)"; joined != want {
		t.Errorf("the block reads %q, want %q", joined, want)
	}

	m.SetJob(&database.Job{ID: "j2", Title: "t", VideoID: "v2", Status: database.StatusFinished, Platform: "youtube"})
	if rows := setAsideArchiveRows(m); len(rows) != 0 {
		t.Errorf("another job shows the first one's set-aside archives: %+v", rows)
	}
}

// The probe behind the block reads a directory, and updateSelectedJob runs on
// every cursor move and every JobsUpdateMsg: it is asked once per job and
// status. An active job is asked too: the new recording is when the operator
// looks for the archive the reset set aside.
//
// Mutants: no memo (a probe per update); the status left out of the key (the
// archive a Reinitialize has just set aside is not shown until the cursor
// moves); an active job not asked.
func TestSetAsideArchivesAreProbedOncePerJobAndStatus(t *testing.T) {
	a := NewApp()
	probes := 0
	a.JobSetAsideArchives = func(id string) []SetAsideArchiveEntry {
		probes++
		return []SetAsideArchiveEntry{{Name: id, Size: 1}}
	}
	job := &database.Job{ID: "j1", Status: database.StatusError, Platform: "youtube"}
	a.setAsideArchivesFor(job)
	a.setAsideArchivesFor(job)
	if probes != 1 {
		t.Fatalf("probed %d times for one job in one status, want 1", probes)
	}
	job.Status = database.StatusDownloading
	if got := a.setAsideArchivesFor(job); probes != 2 || len(got) != 1 || got[0].Name != "j1" {
		t.Errorf("after the status changed: %d probes, answer %+v; want a second probe and its answer for the active job", probes, got)
	}
	if got := a.setAsideArchivesFor(&database.Job{ID: "j2", Status: database.StatusDownloading}); probes != 3 || got[0].Name != "j2" {
		t.Errorf("another job: %d probes, answer %+v; want its own", probes, got)
	}
}

package worker

import (
	"os"
	"path/filepath"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// HasStagingFiles returns true if the staging directory for a job exists and
// holds anything a run left behind.
//
// The yt:stretch display-aspect record (displayAspectFile) does not count: it
// is written before the download's first request (ExecuteWithChat), so a
// stretched video whose run failed or was cancelled before any media byte
// would otherwise read as preserved staging — the failure notice saying
// "Resume available", both UIs and the /resume route offering Resume and
// delete-staging, channel removal counting the row as footage — on a
// directory holding nothing recoverable. It is metadata about a recording,
// never a recording.
func HasStagingFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != displayAspectFile {
			return true
		}
	}
	return false
}

// ListedJob is a job row as the jobs list carries it — GET /api/jobs and
// /api/jobs/archived, and the dashboard's WebSocket rows (initial_state,
// jobs_update, job_update) — with HasStaging (HasStagingFiles) for a row in a
// status Resume can be offered from (owner decision PE-5): the dashboard's
// row icon is Resume only where the job is resumable and has staging to
// resume, else Reinitialize, and with no flag on the list's rows it offered
// Resume on every resumable row and let the server answer "use
// Reinitialize". One directory read per such row, computed server-side, so
// the dashboard makes no extra request per row. Absent (nil) for a row in
// any other status — a live capture's staging is its own — which the
// dashboard reads as unknown and keeps what it last held. The job's own
// fields, and its JobVersion (the hub's frame ordering), come through the
// embedded row unchanged. The flag is a snapshot taken when the row is sent:
// a Finished row broadcast at its Muxing → Finished write can still say true
// after cleanupStagingAfterMux empties staging, until its next update —
// harmless, since a Finished row offers Resume only with incomplete_tail,
// which keeps its staging.
type ListedJob struct {
	*database.Job
	HasStaging *bool `json:"hasStaging,omitempty"`
}

// stagingFlagStatuses are the statuses ListedJob carries HasStaging for:
// the dashboard's resumable ones (Error, Cancelled, COOKIES?) and Finished,
// which a capture with an incomplete tail resumes from.
var stagingFlagStatuses = map[database.JobStatus]bool{
	database.StatusError:     true,
	database.StatusCancelled: true,
	database.StatusCookies:   true,
	database.StatusFinished:  true,
}

// ListJob is job as the jobs list carries it (ListedJob), its staging read
// from stagingBase when its status is one stagingFlagStatuses names.
func ListJob(stagingBase string, job *database.Job) ListedJob {
	row := ListedJob{Job: job}
	if job != nil && stagingFlagStatuses[job.Status] {
		has := HasStagingFiles(stagingBase, job.ID)
		row.HasStaging = &has
	}
	return row
}

// ListJobs is ListJob over a list, in order; never nil.
func ListJobs(stagingBase string, jobs []*database.Job) []ListedJob {
	rows := make([]ListedJob, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, ListJob(stagingBase, j))
	}
	return rows
}

// HasSegmentFiles returns true if the staging directory contains recognized
// segment files, either at the root or inside a quality-split seg_N
// subdirectory (post-split jobs keep their newest data there — without the
// subdirectory check, the Mux recovery action stays hidden for them).
//
// Recognition is delegated to discoverStagingMedia so this visibility probe
// and the actual mux recovery (muxFromStaging) can never disagree about what
// counts as recoverable media — and the part dirs come from stagedSegDirs for
// the same reason: a dir a merge tombstoned holds media already inside the
// merged part, whose RemoveAll failed (a Windows handle) or never ran, and
// counting it kept offering Mux (and muxOnRestart) on a finished job.
func HasSegmentFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	if discoverStagingMedia(dir) != nil {
		return true
	}
	for _, seg := range stagedSegDirs(dir) {
		if discoverStagingMedia(seg.dir) != nil {
			return true
		}
	}
	return false
}

// HasUnmuxedParts reports whether a job's staging still holds a split part
// whose media has no segment row — one that both its stream-end mux and the
// finalize backstop (muxUnrecordedSegments) failed to mux. The job still
// finalizes Finished, from the parts that did mux, and cleanupStagingAfterMux
// keeps its staging and names the Mux action as the way back; this is the
// predicate that makes the Mux verb (the REST route, the TUI's A M and the
// dashboard's details dialog, through enrichJob) actually offer itself on that
// Finished row. Set-aside recordings are not counted: they have their own verb
// (RecoverAsides).
func HasUnmuxedParts(db *database.Database, stagingBase, jobID string) bool {
	return hasUnmuxedSegmentParts(db, jobID, filepath.Join(stagingBase, jobID))
}

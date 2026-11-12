package worker

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The archive an earlier recording of a job left (owner ruling R2-W1).
//
// A Reinitialize clears output_file, and a row can reach one still naming a
// single-file archive: a Finished job with an incomplete tail that was
// resumed (by the operator, or by the monitor on re-detect) and whose resume
// then failed or was cancelled. After the reset nothing named that file — the
// Files tab offered it under Delete All, and the new recording's finalize
// resolved the same name and wrote over it, however much shorter it was.
//
// The reset now moves the path to prior_output_file (resetFieldsKeepingArchive),
// so the row still owns the file (jobFileLocations), and the new recording's
// finalize renames it aside beside itself before it writes anything
// (setAsidePriorArchive), under the name a recovered set-aside recording takes
// — "<stem>.restart-<unix ts>.mp4" — and stores that name on the row. Both
// UIs' job details show the column. Moombox never replaces or deletes the
// file; removing it is the operator's to do by hand.

// priorArchiveSetAside reports whether path is already an archive the
// finalize renamed aside (it carries the set-aside name), as opposed to one a
// reset recorded that still sits under its own name.
func priorArchiveSetAside(path string) bool {
	_, ok := asideSiblingStem(path)
	return ok
}

// archiveKeptByReset returns the single-file archive a Reinitialize of job
// keeps rather than disowns, "" when there is none: the file output_file
// names, when the job has no recorded parts (a part is deleted by the reset,
// and counted by the preview as one — clearJobParts), or the one an earlier
// reset already recorded that no finalize has set aside yet. Only a file that
// is on disk counts. hasParts is whether the job has segment rows.
//
// The one rule PreviewReinitialize names the archive by and the reset keeps
// it by.
func archiveKeptByReset(job *database.Job, hasParts bool) string {
	if job == nil {
		return ""
	}
	if job.OutputFile != "" {
		if !hasParts && fileExists(job.OutputFile) {
			return job.OutputFile
		}
		return ""
	}
	if p := job.PriorOutputFile; p != "" && !priorArchiveSetAside(p) && fileExists(p) {
		return p
	}
	return ""
}

// resetFieldsKeepingArchive adds to a reset's fields (reinitResetFields) the
// column that keeps the row's single-file archive owned once output_file is
// cleared. prior is the row as read just before the reset's claim, under the
// staging claim the verb holds.
//
// A row can hold both an archive of its own and an earlier one no finalize
// managed to set aside (the rename failed and the two did not share a name).
// The column holds one path, so the earlier one is set aside here, before the
// column moves on to the newer archive; a failure is logged and leaves that
// file where it is.
func (w *DownloadWorker) resetFieldsKeepingArchive(fields map[string]any, prior *database.Job) map[string]any {
	if prior == nil {
		return fields
	}
	segs, err := w.db.GetSegments(prior.ID)
	keep := archiveKeptByReset(prior, err != nil || len(segs) > 0)
	if keep == "" {
		return fields
	}
	if old := prior.PriorOutputFile; old != "" && !priorArchiveSetAside(old) &&
		normalizePath(old) != normalizePath(keep) && fileExists(old) {
		if dst, err := moveArchiveAside(old); err != nil {
			w.logger.Warn("reinitialize: an earlier archive could not be set aside; it stays where it is, on no row",
				"path", old, "err", err, "jobID", prior.ID)
		} else {
			w.logger.Warn("reinitialize: an earlier archive was set aside; it is kept, not deleted",
				"from", old, "to", dst, "jobID", prior.ID)
		}
	}
	fields["prior_output_file"] = keep
	return fields
}

// moveArchiveAside renames an archive to a free set-aside name beside it,
// "<stem>.restart-<unix ts>[-N].mp4" (asideOutputPath), and returns that name.
// The stamp is the moment it was set aside, as the engine's own set-asides
// are stamped.
func moveArchiveAside(path string) (string, error) {
	dir := filepath.Dir(path)
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	dst, ok := asideOutputPath(dir, stem, strconv.FormatInt(time.Now().Unix(), 10), map[string]bool{})
	if !ok {
		return "", fmt.Errorf("no free set-aside name beside %s", path)
	}
	if err := utils.ReplaceFile(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// setAsidePriorArchive renames the archive an earlier recording of the job
// left (prior_output_file) aside, before this finalize writes its own: the
// new recording never replaces it. Called once per finalize, ahead of both
// finalize shapes — the single-file mux writes "<name>.mp4" and a
// single-part split is renamed to it (renameSinglePartToPlain), which is
// exactly where the earlier archive sits when the name resolved the same.
//
// The row is read here rather than taken from jobCtx, whose copy can be from
// before the reset. An archive already set aside is left alone (a second
// finalize after a failed mux), and one that is no longer on disk — removed
// by hand — is dropped from the row.
//
// A rename that fails stops the finalize only when the earlier archive is at
// the name this one writes: going on would replace it. The staging is kept,
// so a Mux tries again. Anywhere else it is logged and the row goes on
// naming the file where it is.
func (o *DownloadOrchestrator) setAsidePriorArchive(jobCtx *JobContext) error {
	job, err := o.db.GetJob(jobCtx.Job.ID)
	if err != nil || job == nil {
		return nil
	}
	prior := job.PriorOutputFile
	if prior == "" || priorArchiveSetAside(prior) {
		return nil
	}
	if !fileExists(prior) {
		o.db.UpdateJobFields(job.ID, map[string]any{"prior_output_file": ""})
		return nil
	}
	dst, err := moveArchiveAside(prior)
	if err != nil {
		target := filepath.Join(jobCtx.OutputDir, jobCtx.Filename+".mp4")
		if normalizePath(target) == normalizePath(prior) {
			return fmt.Errorf("set the earlier archive aside (%s): %w", filepath.Base(prior), err)
		}
		o.logger.Warn("the archive an earlier recording of this job left could not be set aside; it stays where it is",
			"path", prior, "err", err, "jobID", job.ID)
		return nil
	}
	o.db.UpdateJobFields(job.ID, map[string]any{"prior_output_file": dst})
	o.logger.Warn("the archive an earlier recording of this job left was set aside; it is kept beside the new one, not replaced",
		"from", prior, "to", dst, "jobID", job.ID)
	return nil
}

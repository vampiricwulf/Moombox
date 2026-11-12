package worker

import (
	"fmt"
	"os"
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
// so the row still owns the file and the sidecars beside it — its chat,
// thumbnail and description, whose own columns the reset clears
// (jobFileLocations, priorArchiveSidecars) — and the new recording's finalize
// renames them aside before it writes anything (setAsidePriorArchive), under
// the name a recovered set-aside recording takes —
// "<stem>.restart-<unix ts>.mp4", its chat "<stem>.restart-<unix ts>.chat.json"
// — and stores the archive's new name on the row. Both UIs' job details show
// the column. Moombox never replaces or deletes these files; removing them is
// the operator's to do by hand, after which the Files sweep drops the column
// (forgetMissingPriorArchives).

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

// priorArchiveSidecarExts are the files a single-file finalize writes beside
// its archive under the archive's stem (copyAssets): the chat and its raw
// sidecar, the description and the thumbnail. They go wherever the archive
// goes: a live chat cannot be fetched again, and the new recording's finalize
// writes its own at the same names.
var priorArchiveSidecarExts = []string{".chat.json", chatRawSidecarSuffix, ".description", ".jpg", ".webp", ".png"}

// priorArchiveSidecars returns where the sidecars of the archive at path sit,
// whether or not each exists. The row owns them while it names the archive
// (jobFileLocations): the reset clears their own columns.
func priorArchiveSidecars(path string) []string {
	if path == "" {
		return nil
	}
	stem := strings.TrimSuffix(path, filepath.Ext(path))
	out := make([]string, 0, len(priorArchiveSidecarExts))
	for _, ext := range priorArchiveSidecarExts {
		out = append(out, stem+ext)
	}
	return out
}

// resetFieldsKeepingArchive adds to a reset's fields (reinitResetFields) the
// column that keeps the row's single-file archive owned once output_file is
// cleared. prior is the row as read just before the reset's claim, under the
// staging claim the verb holds. It touches no file.
//
// A row can hold both an archive of its own and an earlier one no finalize
// managed to set aside (the rename failed and the two did not share a name).
// The column holds one path and moves on to the newer archive; older is that
// earlier one, "" when there is none, for the caller to set aside once its
// claim has applied (setOlderArchiveAside), and keep is the archive the
// column now names.
func (w *DownloadWorker) resetFieldsKeepingArchive(fields map[string]any, prior *database.Job) (out map[string]any, older, keep string) {
	if prior == nil {
		return fields, "", ""
	}
	segs, err := w.db.GetSegments(prior.ID)
	keep = archiveKeptByReset(prior, err != nil || len(segs) > 0)
	if keep == "" {
		return fields, "", ""
	}
	if old := prior.PriorOutputFile; old != "" && !priorArchiveSetAside(old) &&
		normalizePath(old) != normalizePath(keep) && fileExists(old) {
		older = old
	}
	fields["prior_output_file"] = keep
	return fields, older, keep
}

// setOlderArchiveAside renames the archive a reset's column moved on from
// (resetFieldsKeepingArchive's older) to a set-aside name, with its sidecars.
// Called only after the reset's conditional claim applied: a refused reset
// leaves the file, and the row that names it, as they were.
//
// In the directory of keep, the archive the column names now, it takes that
// archive's stem, so the row goes on owning it as a set-aside sibling of the
// archive it names (newOutputOwners). In another directory it is renamed
// where it is and no row names it: the log line is the record of where it
// went, and the Files tab lists it.
func (w *DownloadWorker) setOlderArchiveAside(jobID, older, keep string) {
	if older == "" {
		return
	}
	stem := strings.TrimSuffix(filepath.Base(older), filepath.Ext(older))
	owned := normalizePath(filepath.Dir(older)) == normalizePath(filepath.Dir(keep))
	if owned {
		stem = strings.TrimSuffix(filepath.Base(keep), filepath.Ext(keep))
	}
	dst, err := moveArchiveAsideAs(older, stem)
	switch {
	case err != nil:
		w.logger.Warn("reinitialize: an archive from before the one this job now keeps could not be set aside; it stays where it is, on no row, and job details do not list it",
			"path", older, "err", err, "jobID", jobID)
	case owned:
		moveSidecarsAside(older, dst, w.logger, jobID)
		w.logger.Warn("reinitialize: an archive from before the one this job now keeps was set aside beside it; it is kept, not deleted, and job details list only the newer one",
			"from", older, "to", dst, "jobID", jobID)
	default:
		moveSidecarsAside(older, dst, w.logger, jobID)
		w.logger.Warn("reinitialize: an archive from before the one this job now keeps was set aside; it is kept, not deleted, but no row names it now: job details do not list it and the Files tab does",
			"from", older, "to", dst, "jobID", jobID)
	}
}

// renameArchive is the rename a set-aside makes: a seam, so a test can have
// it fail. Production never assigns it.
var renameArchive = utils.ReplaceFile

// moveArchiveAside renames an archive to a free set-aside name beside it,
// "<stem>.restart-<unix ts>[-N].mp4" (asideOutputPath), and returns that name.
// The stamp is the moment it was set aside, as the engine's own set-asides
// are stamped.
func moveArchiveAside(path string) (string, error) {
	return moveArchiveAsideAs(path, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
}

// moveArchiveAsideAs is moveArchiveAside under another stem, in the same
// directory.
func moveArchiveAsideAs(path, stem string) (string, error) {
	dst, ok := asideOutputPath(filepath.Dir(path), stem, strconv.FormatInt(time.Now().Unix(), 10), map[string]bool{})
	if !ok {
		return "", fmt.Errorf("no free set-aside name beside %s", path)
	}
	if err := renameArchive(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// moveSidecarsAside renames the sidecars of the archive that was at from
// (priorArchiveSidecars) to the stem of to, the set-aside name the archive
// took: "<stem>.restart-<ts>.chat.json" is the name a recovered set-aside
// recording's chat has too (asideSiblingStem). One whose new name is taken
// stays where it is, as does one whose rename fails; each is logged.
func moveSidecarsAside(from, to string, log logger, jobID string) {
	src, dst := priorArchiveSidecars(from), priorArchiveSidecars(to)
	for i := range src {
		if !fileExists(src[i]) {
			continue
		}
		if _, err := os.Lstat(dst[i]); err == nil {
			log.Warn("a file of the earlier recording was not set aside with its archive: the name is taken",
				"path", src[i], "taken", dst[i], "jobID", jobID)
			continue
		}
		if err := utils.ReplaceFile(src[i], dst[i]); err != nil {
			log.Error("a file of the earlier recording could not be set aside with its archive; a new recording under the same name replaces it",
				"path", src[i], "err", err, "jobID", jobID)
		}
	}
}

// priorArchiveGone reports whether the archive at path is definitely not
// there: its directory can be read and holds no such file. An archive on a
// volume that is not mounted is not gone.
func priorArchiveGone(path string) bool {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return false
	}
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

// setAsideNameOf returns the set-aside archive beside path that carries its
// stem ("<stem>.restart-<ts>[-N].mp4"), the latest by name when there are
// several, "" when there is none.
func setAsideNameOf(path string) string {
	dir := filepath.Dir(path)
	want := normalizePath(strings.TrimSuffix(path, filepath.Ext(path)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || !strings.EqualFold(filepath.Ext(p), ".mp4") {
			continue
		}
		if stem, ok := asideSiblingStem(p); ok && stem == want && p > found {
			found = p
		}
	}
	return found
}

// setAsidePriorArchive renames the archive an earlier recording of the job
// left (prior_output_file) aside, with its sidecars (moveSidecarsAside),
// before this finalize writes its own: the new recording never replaces them.
// Called once per finalize, ahead of both finalize shapes — the single-file
// mux writes "<name>.mp4" and a single-part split is renamed to it
// (renameSinglePartToPlain), which is exactly where the earlier archive sits
// when the name resolved the same.
//
// The row is read here rather than taken from jobCtx, whose copy can be from
// before the reset. An archive already set aside is left alone (a second
// finalize after a failed mux).
//
// One that is not at the path the row names:
//
//   - a set-aside file with its stem is beside it: the row takes that name.
//     This is a process that died between the rename and the column write.
//     (A recording the engine set aside and the finalize recovered has the
//     same form; a row whose archive was removed by hand then names that
//     one, which is also an earlier file of the job that is kept);
//   - its directory can be read and the file is not there: removed by hand,
//     and dropped from the row;
//   - anything else (the volume is not mounted): the row keeps naming it.
//
// A rename that fails does not stop the finalize here: the path is recorded
// on jobCtx, and the writes that would replace a file at it refuse
// (blockedByPriorArchive). A finalize that writes other names — several
// parts — goes on, and the row keeps naming the file where it is.
func (o *DownloadOrchestrator) setAsidePriorArchive(jobCtx *JobContext) {
	jobCtx.priorArchiveStuck, jobCtx.priorArchiveStuckErr = "", nil
	job, err := o.db.GetJob(jobCtx.Job.ID)
	if err != nil || job == nil {
		return
	}
	prior := job.PriorOutputFile
	if prior == "" || priorArchiveSetAside(prior) {
		return
	}
	if !fileExists(prior) {
		switch aside := setAsideNameOf(prior); {
		case aside != "":
			o.db.UpdateJobFields(job.ID, map[string]any{"prior_output_file": aside})
			moveSidecarsAside(prior, aside, o.logger, job.ID)
			o.logger.Info("the archive an earlier recording of this job left is already set aside; the row names it there",
				"was", prior, "now", aside, "jobID", job.ID)
		case priorArchiveGone(prior):
			o.db.UpdateJobFields(job.ID, map[string]any{"prior_output_file": ""})
		default:
			o.logger.Warn("the archive an earlier recording of this job left cannot be reached; the row goes on naming it",
				"path", prior, "jobID", job.ID)
		}
		return
	}
	dst, err := moveArchiveAside(prior)
	if err != nil {
		jobCtx.priorArchiveStuck, jobCtx.priorArchiveStuckErr = prior, err
		o.logger.Warn("the archive an earlier recording of this job left could not be set aside; it stays where it is, and nothing is written over it",
			"path", prior, "err", err, "jobID", job.ID)
		return
	}
	o.db.UpdateJobFields(job.ID, map[string]any{"prior_output_file": dst})
	moveSidecarsAside(prior, dst, o.logger, job.ID)
	o.logger.Warn("the archive an earlier recording of this job left was set aside; it is kept, not replaced",
		"from", prior, "to", dst, "jobID", job.ID)
}

// blockedByPriorArchive returns an error when path is where the archive an
// earlier recording left still sits because it could not be renamed aside
// (setAsidePriorArchive): writing there would replace it. nil otherwise.
func (jobCtx *JobContext) blockedByPriorArchive(path string) error {
	if jobCtx.priorArchiveStuck == "" || normalizePath(path) != normalizePath(jobCtx.priorArchiveStuck) {
		return nil
	}
	return fmt.Errorf("set the earlier archive aside (%s): %w", filepath.Base(path), jobCtx.priorArchiveStuckErr)
}

// forgetMissingPriorArchives drops prior_output_file from every row whose
// set-aside archive is definitely gone (priorArchiveGone): removing that file
// by hand is how the operator gets rid of it, and both UIs' job details went
// on showing "Earlier file" for it. Run by the Files sweep, which reads every
// row anyway, on the rows it read.
//
// Only a row that is Finished, Error or Cancelled, written only while it
// still reads that status: every reset that gives the column a new archive
// moves the row to Upcoming, so this never clears a path a reset just wrote.
// One still waiting for its finalize (not yet a set-aside name) is the
// finalize's to settle. A cleared row's copy in jobs is cleared too.
func forgetMissingPriorArchives(db *database.Database, jobs []*database.Job) {
	for _, job := range jobs {
		p := job.PriorOutputFile
		if p == "" || !priorArchiveSetAside(p) || !priorArchiveGone(p) {
			continue
		}
		switch job.Status {
		case database.StatusFinished, database.StatusError, database.StatusCancelled:
		default:
			continue
		}
		if db.UpdateJobFieldsIf(job.ID, job.Status, map[string]any{"prior_output_file": ""}) {
			job.PriorOutputFile = ""
		}
	}
}

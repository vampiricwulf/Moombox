package worker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// opBootCleanup holds a job's staging claim while the boot sweep decides about
// it, so a Mux or an aside recovery started at the same moment is refused
// rather than read a directory being deleted under it.
const opBootCleanup = "boot cleanup"

// bootSweepClaimed, when set, is called with each job ID whose staging claim
// the boot sweep took. A test seam; nil in production.
var bootSweepClaimed func(jobID string)

// reclaimBootLeftovers deletes, once per start, the staging leftovers that are
// PROVABLY redundant, and nothing else:
//
//   - a set-aside recording marked recovered (asideRecoveredMarker) whose
//     muxed sibling is on disk — the copy was verified before the marker was
//     written, so the aside is footage that already exists beside the archive;
//   - the staging dir of a Finished job whose archive file(s) exist, when
//     cleanupStagingAfterMux would have deleted it (decideStagingCleanup says
//     removeStaging) and no aside marked recovered is left in it. Its own
//     cleanup did not run or did not finish — the process died between the
//     Finished write and the RemoveAll, or Windows held a handle on a file in
//     it.
//
// Every deletion is logged with its path and the reason. Anything that is not
// provable — a job row that cannot be read or is gone, an archive that is not
// where the row says, a shield the cleanup would have kept the dir for, a
// marker whose sibling is missing — is left exactly as it is, for the orphan
// sweep and its confirm-before-delete (ScanOrphanedFiles).
//
// Runs on its own goroutine from Start, so a slow delete never holds up the
// queue. A job that is active, or whose staging an off-queue operation holds
// (the per-job claim, and the output claims a recovery takes), is skipped.
func (w *DownloadWorker) reclaimBootLeftovers() {
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	entries, err := os.ReadDir(stagingBase)
	if err != nil {
		if !os.IsNotExist(err) {
			w.logger.Warn("boot cleanup: could not read the staging directory; nothing reclaimed",
				"path", stagingBase, "err", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			w.reclaimJobLeftovers(e.Name(), filepath.Join(stagingBase, e.Name()))
		}
	}
}

// reclaimJobLeftovers is reclaimBootLeftovers for one staging dir, named by
// the job ID the layout gives it.
func (w *DownloadWorker) reclaimJobLeftovers(jobID, stagingDir string) {
	if outputClaimOwner(stagingDir) != "" {
		return
	}
	// An active row is skipped BEFORE the claim is taken, never under it: the
	// sweep runs beside enqueueExistingJobs, whose restart mux of a Muxing
	// row takes this same claim, and a refusal there falls back to resetting
	// the row to Downloading — the re-download that truncated a complete
	// recording (sweep-2 ENGINE-1).
	if job, err := w.db.GetJob(jobID); err != nil || (job != nil && IsActiveJobStatus(job.Status)) {
		return
	}
	release, err := w.claimJobOperation(jobID, opBootCleanup)
	if err != nil {
		return
	}
	defer release()
	if bootSweepClaimed != nil {
		bootSweepClaimed(jobID)
	}

	// Read again under the claim: the row may have been revived since.
	job, err := w.db.GetJob(jobID)
	if err != nil || (job != nil && IsActiveJobStatus(job.Status)) {
		return
	}
	// A recovered aside is redundant whatever became of its row: the proof is
	// the sibling on disk, not the job.
	keptAside := w.removeVerifiedRecoveredAsides(jobID, stagingDir)
	if job == nil || job.Status != database.StatusFinished {
		return
	}
	// decideStagingCleanup does not see a marked aside (stagedRestartAsides
	// skips it as already muxed), which is right straight after a finalize
	// that has just written its sibling and wrong here: one this sweep could
	// not prove redundant may be the only copy of that footage, and the
	// RemoveAll below would take it with the directory.
	if keptAside {
		w.logger.Debug("boot cleanup: keeping a finished job's staging; a set-aside recording marked recovered is still in it",
			"path", stagingDir, "jobID", jobID)
		return
	}
	if missing := missingArchiveFile(w.db, job); missing != "" {
		w.logger.Debug("boot cleanup: keeping a finished job's staging; its archive is not where the row says",
			"path", stagingDir, "archive", missing, "jobID", jobID)
		return
	}
	if decideStagingCleanup(w.db, job, jobID, stagingDir).keep != removeStaging {
		return
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("boot cleanup: could not remove a finished job's staging directory",
			"path", stagingDir, "jobID", jobID, "err", err)
		return
	}
	w.logger.Info("boot cleanup: removed a finished job's staging directory",
		"path", stagingDir, "jobID", jobID,
		"reason", "the job is Finished with its archive on disk, and its post-mux cleanup would have removed this directory (chat and tail complete, nothing set aside, no unmuxed part, no unused root recording)")
}

// missingArchiveFile returns the first archive file a Finished row names that
// is not on disk as a non-empty file — its output_file, then every part's file
// — or "" when all are there. A row that names no archive at all, or whose
// parts cannot be read, answers with a placeholder: either way the staging
// cannot be shown redundant.
func missingArchiveFile(db *database.Database, job *database.Job) string {
	if job.OutputFile == "" {
		return "(no output_file recorded)"
	}
	if !nonEmptyFile(job.OutputFile) {
		return job.OutputFile
	}
	segs, err := db.GetSegments(job.ID)
	if err != nil {
		return "(parts unreadable)"
	}
	for _, s := range segs {
		if s.FilePath == "" {
			return "(a part with no file recorded)"
		}
		if !nonEmptyFile(s.FilePath) {
			return s.FilePath
		}
	}
	return ""
}

// nonEmptyFile reports whether path is a regular file holding something.
func nonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// removeVerifiedRecoveredAsides deletes each set-aside recording under a
// staging tree (the root and every seg_N dir, tombstoned ones included) that
// carries a recovered marker naming a sibling that exists — the aside, its
// resume twin and the marker — logging each with its path and the sibling it
// was recovered to. It is removeRecoveredAsides with the proof added: that
// one runs inside a finalize that has just written the siblings, this one at
// boot, where the sibling may since have been moved or deleted, and then the
// aside is the only copy left and stays.
//
// kept reports whether a marked aside is still on disk when it returns — its
// sibling missing or empty, its marker unreadable, or its removal failed — so
// the caller leaves the rest of the directory alone too: nothing else in the
// sweep counts a marked aside as footage (stagedRestartAsides skips it).
func (w *DownloadWorker) removeVerifiedRecoveredAsides(jobID, stagingDir string) (kept bool) {
	dirs := []string{stagingDir}
	for _, sd := range segDirsOf(stagingDir, true) {
		dirs = append(dirs, sd.dir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), asideRecoveredMarker)
			if !ok || e.IsDir() || !engine.IsStagedRestartPath(name) {
				continue
			}
			aside := filepath.Join(dir, name)
			w.removeVerifiedRecoveredAside(jobID, aside)
			// An aside still here is one this sweep could not prove
			// redundant, or could not remove. A marker whose aside is already
			// gone (its removal went through, the marker's did not) keeps
			// nothing: what is left is not footage.
			if fileExists(aside) {
				kept = true
			}
		}
	}
	return kept
}

// removeVerifiedRecoveredAside is removeVerifiedRecoveredAsides for one aside
// whose recovered marker sits beside it.
func (w *DownloadWorker) removeVerifiedRecoveredAside(jobID, aside string) {
	marker := aside + asideRecoveredMarker
	raw, err := os.ReadFile(marker)
	if err != nil {
		return
	}
	sibling := strings.TrimSpace(string(raw))
	if sibling == "" || !nonEmptyFile(sibling) {
		w.logger.Debug("boot cleanup: keeping a recovered set-aside recording; the sibling its marker names is not on disk",
			"aside", aside, "sibling", sibling, "jobID", jobID)
		return
	}
	for _, p := range []string{aside, engine.StagedRestartSidecar(aside), marker} {
		remove := os.Remove
		if p == aside {
			// The seam the finalize's own aside removals use, so a test can
			// hold the aside the way a Windows handle does.
			remove = removeAsideFile
		}
		if err := remove(p); err != nil && !os.IsNotExist(err) {
			w.logger.Warn("boot cleanup: could not remove a recovered set-aside recording's leftover",
				"path", p, "jobID", jobID, "err", err)
			return
		}
	}
	w.logger.Info("boot cleanup: removed a set-aside recording already recovered beside the archive",
		"path", aside, "jobID", jobID,
		"reason", "its recovered marker names a sibling that is on disk: "+sibling)
}

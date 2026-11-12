package worker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The archive a job already has when it is reinitialized (owner ruling
// R2-W1b).
//
// A Reinitialize clears output_file, and a row can reach one still naming a
// single-file archive: a Finished job with an incomplete tail that was
// resumed (by the operator, or by the monitor on re-detect) and whose resume
// then failed or was cancelled. After the reset nothing named that file — the
// Files tab offered it under Delete All, and the new recording's finalize
// resolved the same name and wrote over it, however much shorter it was.
//
// The reset renames it aside there and then (setArchiveAside), once its
// claim has applied: "<stem>.restart-<unix ts>[-N].mp4" beside where it was,
// the name a recovered set-aside recording takes (asideOutputPath), with its
// chat, description and thumbnail under the same name. It stays there whether
// or not a new recording ever succeeds. Moombox never replaces or deletes it.
//
// Who owns it. A recovered set-aside recording is owned by stem: it belongs
// to the row that names "<stem>.mp4" (outputOwners.stems). That rule cannot
// own this file. The reset has just cleared every path column of the row, so
// until the new recording finishes no row names the stem at all, and it need
// never name it: the title can change before the finalize resolves the name
// again, the output directory can follow a changed setting, and the new
// recording can fail. So the reset writes one more file under the aside's
// name, "<stem>.restart-<unix ts>.owner", holding the job's ID, and the sweep
// and the delete count an aside whose marker names an existing job as that
// job's (outputOwners.asideOwner). The marker is beside the files it speaks
// for, so it moves with them when the archive tree is moved, and it needs no
// column. Once the job row is deleted the aside and its marker are listed in
// Files like any recovered recording whose archive is gone.

// asideOwnerSuffix is the extension of the marker that names the job an
// archive was set aside for at its Reinitialize.
const asideOwnerSuffix = ".owner"

// archiveSidecarExts are the files a single-file finalize writes beside its
// archive under the archive's stem (copyAssets): the chat and its raw
// sidecar, the description and the thumbnail. They go wherever the archive
// goes: a live chat cannot be fetched again, and the new recording's finalize
// writes its own at the same names.
var archiveSidecarExts = []string{".chat.json", chatRawSidecarSuffix, ".description", ".jpg", ".webp", ".png"}

// archiveSetAsideByReset returns the single-file archive a Reinitialize of
// job sets aside, "" when there is none: the file output_file names, when the
// job has no recorded parts (a part is deleted by the reset, and counted by
// the preview as one — clearJobParts). hasParts is whether the job has
// segment rows.
//
// A file that is definitely gone (archiveGone) is not one. A file that
// cannot be reached — its drive is unplugged — is: the confirm names it
// without a size, the rename fails and is logged, and the next finalize does
// not write over it (foreignArchiveAt).
//
// The one rule PreviewReinitialize names the archive by and the reset sets it
// aside by.
func archiveSetAsideByReset(job *database.Job, hasParts bool) string {
	if job == nil || job.OutputFile == "" || hasParts || archiveGone(job.OutputFile) {
		return ""
	}
	return job.OutputFile
}

// archiveGone reports whether the archive at path is definitely not there:
// its directory can be read and holds no such file. An archive on a volume
// that is not mounted is not gone.
func archiveGone(path string) bool {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return false
	}
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

// archiveOfReset is archiveSetAsideByReset for the row a reset verb read just
// before its claim. A failed read of the parts sets nothing aside: the row
// may have them.
func (w *DownloadWorker) archiveOfReset(prior *database.Job) string {
	if prior == nil {
		return ""
	}
	segs, err := w.db.GetSegments(prior.ID)
	return archiveSetAsideByReset(prior, err != nil || len(segs) > 0)
}

// renameArchive is the rename a set-aside makes: a seam, so a test can have
// it fail. Production never assigns it.
var renameArchive = utils.ReplaceFile

// asideBase returns path less its extension, the compound one of a chat
// archive or its raw sidecar included: the name every file of one set-aside
// recording shares ("<stem>.restart-<ts>").
func asideBase(path string) string {
	lower := strings.ToLower(path)
	for _, ext := range []string{".chat.json", chatRawSidecarSuffix} {
		if strings.HasSuffix(lower, ext) {
			return path[:len(path)-len(ext)]
		}
	}
	return strings.TrimSuffix(path, filepath.Ext(path))
}

// asideOwnerID returns the job ID in the marker of the set-aside recording
// path is a file of, "" when it has none.
func asideOwnerID(path string) string {
	f, err := os.Open(asideBase(path) + asideOwnerSuffix)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 256))
	return strings.TrimSpace(string(b))
}

// setArchiveAside renames the archive a reset found on the row
// (archiveOfReset) to a free set-aside name beside it, with its marker and
// its sidecars. Called only after the reset's conditional claim applied: a
// refused reset leaves the file, and the row that names it, as they were.
//
// The marker is written first and taken back when the rename fails, so an
// aside is never on disk without it. A rename that fails does not undo the
// reset: the file stays exactly where it is, on no row, one Warn names it,
// and the next finalize does not write over it (foreignArchiveAt). A sidecar
// that cannot follow stays where it is and is logged.
func (w *DownloadWorker) setArchiveAside(jobID, archive string) {
	if archive == "" {
		return
	}
	ext := filepath.Ext(archive)
	stem := strings.TrimSuffix(filepath.Base(archive), ext)
	dst, err := func() (string, error) {
		name, ok := asideOutputPath(filepath.Dir(archive), stem, strconv.FormatInt(time.Now().Unix(), 10), map[string]bool{})
		if !ok {
			return "", fmt.Errorf("no free set-aside name beside it")
		}
		// asideOutputPath names an .mp4, which every archive a finalize
		// writes is; an imported one keeps its own container.
		if !strings.EqualFold(ext, ".mp4") {
			name = strings.TrimSuffix(name, ".mp4") + ext
			if _, err := os.Lstat(name); err == nil {
				return "", fmt.Errorf("the set-aside name %s is taken", filepath.Base(name))
			}
		}
		marker := asideBase(name) + asideOwnerSuffix
		markerErr := os.WriteFile(marker, []byte(jobID+"\n"), 0o644)
		if err := renameArchive(archive, name); err != nil {
			if markerErr == nil {
				os.Remove(marker)
			}
			return "", err
		}
		if markerErr != nil {
			w.logger.Warn("reinitialize: the set-aside archive could not be marked as this job's; it is kept, and the Files tab lists it until a new recording of the same name finishes",
				"path", name, "err", markerErr, "jobID", jobID)
		}
		return name, nil
	}()
	if err != nil {
		w.logger.Warn("reinitialize: the job's existing archive could not be set aside; it stays where it is, on no row, and a new recording will not be written over it",
			"path", archive, "err", err, "jobID", jobID)
		return
	}
	from, to := strings.TrimSuffix(archive, ext), asideBase(dst)
	for _, sidecar := range archiveSidecarExts {
		src, target := from+sidecar, to+sidecar
		if !fileExists(src) {
			continue
		}
		if _, err := os.Lstat(target); err == nil {
			w.logger.Warn("reinitialize: a file of the existing archive was not set aside with it: the name is taken; a new recording under the same name replaces it",
				"path", src, "taken", target, "jobID", jobID)
			continue
		}
		if err := utils.ReplaceFile(src, target); err != nil {
			w.logger.Warn("reinitialize: a file of the existing archive could not be set aside with it; a new recording under the same name replaces it",
				"path", src, "err", err, "jobID", jobID)
		}
	}
	w.logger.Info("reinitialize: the job's existing archive was set aside; it is kept, never replaced",
		"from", archive, "to", dst, "jobID", jobID)
}

// foreignArchiveAt returns an error when a finalize of jobID must not write
// path because a file it did not create is already there, nil otherwise. A
// finalize renames its copy onto the archive's name (copyStaged), which
// replaces whatever holds it: right for the job's own archive, and how the
// archive a reset could not set aside (setArchiveAside) would be lost.
//
// The file is the job's own when the row names it (a re-finalize), or when it
// is no older than the run that is finalizing (download_started_at): a
// finalize that was interrupted after its copy and before its Finished write
// left its archive on no row, and the boot's re-mux lands on it. A file
// older than the run is from before this recording: an archive a reset left,
// another recording's under a reused name.
//
// A row with no run start to compare against is written as before. A reset
// clears the stamp and the next run writes it before it downloads anything
// (ExecuteWithChat, ExecuteTwitch, the clip download), so a finalize after a
// reset always has one.
func (o *DownloadOrchestrator) foreignArchiveAt(jobID, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	job, err := o.db.GetJob(jobID)
	if err != nil || job == nil {
		return nil
	}
	if job.OutputFile != "" && normalizePath(job.OutputFile) == normalizePath(path) {
		return nil
	}
	if started, err := time.Parse(time.RFC3339, job.DownloadStartedAt); err != nil || !info.ModTime().Before(started) {
		return nil
	}
	return fmt.Errorf("%s already exists and is not this job's recording: it is kept, not replaced; move or rename it, then use Mux", filepath.Base(path))
}

// SetAsideArchive is one archive a Reinitialize of a job set aside
// (setArchiveAside), as both UIs' job details list it.
type SetAsideArchive struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ScanSetAsideArchives lists the archives a Reinitialize of job set aside
// that are still where it put them: derived from the disk on each call, never
// stored. It reads the directory of the archive the row names and the one the
// job's next finalize resolves to (outputTargetOf, from the global output
// directory and the output template), and returns every set-aside archive
// there whose marker names the job, oldest first. One whose directory the job
// no longer resolves to (the title's template subdirectory or the output
// directory changed since) is not listed here; the Files sweep still counts
// it as the job's. Never nil: the dashboard reads .length off it.
//
// Takes the two settings rather than a worker, as ScanAsides takes the
// staging base: the REST layer reads them out of the config store.
func ScanSetAsideArchives(job *database.Job, globalOutputDir, template string) []SetAsideArchive {
	out := []SetAsideArchive{}
	if job == nil {
		return out
	}
	outputDir, filename := outputTargetOf(job, globalOutputDir, template)
	dirs := []string{filepath.Dir(filepath.Join(outputDir, filename))}
	if job.OutputFile != "" {
		dirs = append(dirs, filepath.Dir(job.OutputFile))
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[normalizePath(dir)] {
			continue
		}
		seen[normalizePath(dir)] = true
		entries, err := os.ReadDir(dir) // sorted by name: the stamp orders one stem's asides
		if err != nil {
			continue
		}
		mine := map[string]bool{}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if _, ok := asideSiblingStem(p); ok && strings.EqualFold(filepath.Ext(p), asideOwnerSuffix) {
				mine[asideBase(p)] = asideOwnerID(p) == job.ID
			}
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			switch strings.ToLower(filepath.Ext(p)) {
			case ".mp4", ".mkv", ".webm", ".ts":
			default:
				continue
			}
			if !mine[asideBase(p)] {
				continue
			}
			if info, err := e.Info(); err == nil && !info.IsDir() {
				out = append(out, SetAsideArchive{Name: e.Name(), Path: p, Size: info.Size()})
			}
		}
	}
	return out
}

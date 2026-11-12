package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
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
// and the delete count the files that share a marker's name as the job's
// while that job exists (outputOwners.asideOwner). The marker is beside the
// files it speaks for, so it moves with them when the archive tree is moved,
// and it needs no column. Once the job row is deleted the files and their
// marker are listed in Files like any recovered recording whose archive is
// gone.
//
// The same marker covers what the stem rule stops covering at that moment:
// the recovered set-aside recordings already beside the archive each get one
// (adoptStemAsides), and an archive whose rename failed gets "<stem>.owner"
// where it is, so Files does not offer it while the job exists.
//
// A limit: the archive is looked for where output_file says it is. One whose
// tree was moved since (rowAbsoluteLocations) is not found, so it is not
// renamed and gets no marker; nothing writes over it there, and Files lists
// it once the reset has cleared the row.
//
// A window: the reset writes its claim, then renames. A process that dies in
// between leaves the archive at its name, on no row and with no marker. The
// next finalize does not write over it (setForeignFileAside sets it aside),
// but until then Files lists it, and its Delete All would take it.

// asideOwnerSuffix is the extension of the marker that names the job its
// files were kept for at a Reinitialize.
const asideOwnerSuffix = ".owner"

// asideMarkerPrefix opens a marker's one line, ahead of the job ID: what
// tells a marker Moombox wrote from any other *.owner file in the output
// tree, a one-word one included.
const asideMarkerPrefix = "moombox-owner "

// archiveSidecarExts are the files a single-file finalize writes beside its
// archive under the archive's stem (copyAssets): the chat and its raw
// sidecar, the description and the thumbnail. They go wherever the archive
// goes: a live chat cannot be fetched again, and the new recording's finalize
// writes its own at the same names.
var archiveSidecarExts = []string{".chat.json", chatRawSidecarSuffix, ".description", ".jpg", ".webp", ".png"}

// archiveSetAsideByReset returns the single-file archive a reset of job sets
// aside, "" when there is none: the file output_file names (only a finalize
// writes that column), when the job has no recorded parts, or when its one
// part IS that file — a job that ended with a single part, which the
// finalize renamed to the plain name (renameSinglePartToPlain) and which
// reads as one archive everywhere else. The parts of a split job are deleted
// by an operator's Reinitialize and counted by the preview as parts
// (clearJobParts). partsKept is the automatic reinitialize, which keeps every
// part for the capture to continue from, so it sets none of them aside.
//
// A file that is definitely gone (archiveGone) is not one. A file that
// cannot be reached — its drive is unplugged — is: the confirm names it
// without a size, the rename fails and is logged, and the next finalize sets
// it aside itself, or stops when it cannot (setForeignFileAside).
//
// The one rule PreviewReinitialize names the archive by and the reset sets it
// aside by.
func archiveSetAsideByReset(job *database.Job, parts []database.Segment, partsKept bool) string {
	if job == nil || job.OutputFile == "" || archiveGone(job.OutputFile) {
		return ""
	}
	onePart := !partsKept && len(parts) == 1 && normalizePath(parts[0].FilePath) == normalizePath(job.OutputFile)
	if len(parts) > 0 && !onePart {
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
func (w *DownloadWorker) archiveOfReset(prior *database.Job, partsKept bool) string {
	if prior == nil {
		return ""
	}
	segs, err := w.db.GetSegments(prior.ID)
	if err != nil {
		return ""
	}
	return archiveSetAsideByReset(prior, segs, partsKept)
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

// asideOwnerID returns the job ID in the marker beside path, the one that
// shares its name less the extension, "" when there is none.
func asideOwnerID(path string) string {
	id, _ := readAsideMarker(asideBase(path) + asideOwnerSuffix)
	return id
}

// asideMarkerMaxID bounds the job ID a marker holds. The longest IDs are a
// Twitch clip's slug behind its prefix, well under it.
const asideMarkerMaxID = 200

// readAsideMarker reads the marker file at markerPath: the job ID it holds,
// and whether it is a marker at all — a regular file named *.owner whose
// whole content is what writeAsideOwner writes, asideMarkerPrefix and one job
// ID on one line (no spaces, no control characters, at most asideMarkerMaxID
// bytes; the line end may be missing or CRLF, for a marker that went through
// an editor). Any other *.owner file in the output tree is the operator's: it
// owns nothing, the Files sweep never lists it and its delete refuses it.
func readAsideMarker(markerPath string) (jobID string, ok bool) {
	if !strings.EqualFold(filepath.Ext(markerPath), asideOwnerSuffix) {
		return "", false
	}
	if info, err := os.Lstat(markerPath); err != nil || !info.Mode().IsRegular() || info.Size() > int64(len(asideMarkerPrefix))+asideMarkerMaxID+2 {
		return "", false
	}
	b, err := os.ReadFile(markerPath)
	if err != nil {
		return "", false
	}
	id, ok := strings.CutPrefix(strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), asideMarkerPrefix)
	if !ok || id == "" || len(id) > asideMarkerMaxID {
		return "", false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] == 0x7f {
			return "", false
		}
	}
	return id, true
}

// writeAsideOwner makes the files named base.* jobID's: it creates the marker
// base.owner, and never writes over a file already at that name. A marker
// there that names jobID already is the one wanted (created false, no
// error): two files of one job can share a base, an archive and the chat that
// could not follow it. Anything else there — another job's marker, a file of
// the operator's — is left as it is and is the error. created tells the
// caller whether the marker is its own to take back.
func writeAsideOwner(base, jobID string) (created bool, err error) {
	marker := base + asideOwnerSuffix
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if id, ok := readAsideMarker(marker); ok && id == jobID {
			return false, nil
		}
		if os.IsExist(err) {
			return false, fmt.Errorf("%s is there and is not this job's marker", filepath.Base(marker))
		}
		return false, err
	}
	_, err = f.WriteString(asideMarkerPrefix + jobID + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(marker)
		return false, err
	}
	return true, nil
}

// asideBaseFreeFor reports whether a file of jobID may be set aside under
// base: no marker is there, or the one there names jobID. A base under
// another job's marker, or beside an *.owner file that is no marker, is
// passed over, so a set-aside file is never counted as another job's and
// nobody's file is taken for a marker.
func asideBaseFreeFor(base, jobID string) bool {
	if _, err := os.Lstat(base + asideOwnerSuffix); err != nil {
		return true
	}
	id, ok := readAsideMarker(base + asideOwnerSuffix)
	return ok && id == jobID
}

// freeAsideName is asideOutputPath for an archive of any container: a free
// "<stem>.restart-<stamp>[-N]<ext>" beside it. asideOutputPath names an
// .mp4, which every archive a finalize writes is; an imported one keeps its
// own extension, and a counter whose name is taken in that extension is
// passed over like one taken as an .mp4, and so is one whose marker name is
// not jobID's to use (asideBaseFreeFor).
func freeAsideName(dir, stem, ext, stamp, jobID string) (string, bool) {
	used := map[string]bool{}
	for {
		name, ok := asideOutputPath(dir, stem, stamp, used)
		if !ok {
			return "", false
		}
		name = strings.TrimSuffix(name, ".mp4") + ext
		if _, err := os.Lstat(name); err != nil && asideBaseFreeFor(asideBase(name), jobID) {
			return name, true
		}
	}
}

// asideMove is what renameAside did with one file and its sidecars.
type asideMove struct {
	// dst is where the file went, "" when it was not renamed; err is why not.
	dst string
	err error
	// markerErr is why the aside has no marker. The files moved all the same.
	markerErr error
	// left is each sidecar that could not follow, with the reason; it stays
	// where it was.
	left []asideLeft
}

type asideLeft struct {
	path string
	err  error
}

// renameAside renames file to a free set-aside name beside it,
// "<stem>.restart-<unix ts>[-N]<ext>", under a marker that names jobID, and
// then each file that shares its stem under one of sidecars to the same
// name. The one rename both a Reinitialize (setArchiveAside) and a finalize
// that finds a file it did not write at its name (setForeignFileAside) make.
//
// The marker is written first and taken back when the rename fails, so an
// aside is never on disk without it; a crash between the two leaves a marker
// alone, which nothing lists until its job is deleted. A marker that was
// already there for jobID (another file of the job set aside under the same
// base in the same second) is used as it is and never taken back. Nothing is
// ever renamed onto a name that is taken.
func renameAside(jobID, file string, sidecars []string) asideMove {
	from := asideBase(file)
	dst, ok := freeAsideName(filepath.Dir(file), filepath.Base(from), file[len(from):], strconv.FormatInt(time.Now().Unix(), 10), jobID)
	if !ok {
		return asideMove{err: fmt.Errorf("no free set-aside name beside it")}
	}
	to := asideBase(dst)
	created, markerErr := writeAsideOwner(to, jobID)
	mv := asideMove{dst: dst, markerErr: markerErr}
	if err := renameArchive(file, dst); err != nil {
		if created {
			os.Remove(to + asideOwnerSuffix)
		}
		return asideMove{err: err}
	}
	for _, sidecar := range sidecars {
		src, target := from+sidecar, to+sidecar
		if !fileExists(src) {
			continue
		}
		err := fmt.Errorf("the name %s is taken", filepath.Base(target))
		if _, statErr := os.Lstat(target); statErr != nil {
			err = utils.ReplaceFile(src, target)
		}
		if err != nil {
			mv.left = append(mv.left, asideLeft{path: src, err: err})
		}
	}
	return mv
}

// setArchiveAside renames the archive a reset found on the row
// (archiveOfReset) to a free set-aside name beside it, with its marker and
// its sidecars (renameAside). Called only after the reset's conditional claim
// applied: a refused reset leaves the file, and the row that names it, as
// they were.
//
// A rename that fails does not undo the reset: the file stays exactly where
// it is, on no row, one Warn names it, it gets a marker under its own name so
// Files does not offer it, and the next finalize sets it aside before it
// writes its own archive (setForeignFileAside). A sidecar that cannot follow
// stays where it is under the same in-place marker, and is logged.
func (w *DownloadWorker) setArchiveAside(jobID, archive string) {
	if archive == "" {
		return
	}
	from := asideBase(archive)
	w.adoptStemAsides(jobID, from)
	// keepInPlace marks what stays under the archive's own name as the job's.
	keepInPlace := func() string {
		if _, err := writeAsideOwner(from, jobID); err != nil {
			return "; it could not be marked as this job's either, so the Files tab lists it"
		}
		return ""
	}
	mv := renameAside(jobID, archive, archiveSidecarExts)
	if mv.err != nil {
		w.logger.Warn("reinitialize: the job's existing archive could not be set aside; it stays where it is, on no row, and the next recording's finalize sets it aside before it writes its own"+keepInPlace(),
			"path", archive, "err", mv.err, "jobID", jobID)
		return
	}
	if mv.markerErr != nil {
		w.logger.Warn("reinitialize: the set-aside archive could not be marked as this job's; it is kept, and the Files tab lists it until a new recording of the same name finishes",
			"path", mv.dst, "err", mv.markerErr, "jobID", jobID)
	}
	for _, l := range mv.left {
		w.logger.Warn("reinitialize: a file of the existing archive could not be set aside with it; it stays where it is, and a new chat is not written over an old one"+keepInPlace(),
			"path", l.path, "err", l.err, "jobID", jobID)
	}
	w.logger.Info("reinitialize: the job's existing archive was set aside; it is kept, never replaced",
		"from", archive, "to", mv.dst, "jobID", jobID)
}

// adoptStemAsides marks the recovered set-aside recordings already beside
// the archive stem names ("<stem>.restart-<ts>[-N].*", from Recover
// Set-aside or a VOD superseding the parts) as jobID's. They were the job's
// through the stem rule, which stops covering them when the reset clears the
// row's paths. One that already has a marker keeps it.
func (w *DownloadWorker) adoptStemAsides(jobID, stem string) {
	entries, err := os.ReadDir(filepath.Dir(stem))
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(filepath.Dir(stem), e.Name())
		if s, ok := asideSiblingStem(p); !ok || s != normalizePath(stem) || e.IsDir() {
			continue
		}
		if base := asideBase(p); !fileExists(base + asideOwnerSuffix) {
			if _, err := writeAsideOwner(base, jobID); err != nil {
				w.logger.Warn("reinitialize: a recovered recording beside the archive could not be marked as this job's; the Files tab lists it",
					"path", p, "err", err, "jobID", jobID)
			}
		}
	}
}

// foreignFileAt reports whether path holds a file a finalize of jobID did not
// create. A finalize renames its copy onto the archive's name (copyStaged)
// and copies the chat capture onto the chat's (copyAssets), which replaces
// whatever holds them: right for the job's own files, and how another
// recording under the same name would be lost — the archive a reset could
// not set aside (setArchiveAside), or the file of a row that was deleted
// with its files kept before the same video was added again. A description
// and a thumbnail are written as before: the new ones are the same video's.
//
// The file is the job's own when the row names it (a re-finalize), or when it
// is no older than the run that is finalizing (download_started_at): a
// finalize that was interrupted after its copy and before its Finished write
// left its archive on no row, and the boot's re-mux lands on it. A file
// older than the run is from before this recording.
//
// Only a regular file is one. A directory or a link at the name is not set
// aside: the write meets it as it always did (a rename onto a directory
// fails, and the finalize ends there with its staging kept).
//
// A row with no run start to compare against is written as before. A reset
// clears the stamp and the next run writes it before it downloads anything
// (ExecuteWithChat, ExecuteTwitch, the clip download), so a finalize after a
// reset always has one.
func (o *DownloadOrchestrator) foreignFileAt(jobID, path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	job, err := o.db.GetJob(jobID)
	if err != nil || job == nil {
		return false
	}
	owned := []string{job.OutputFile, job.ChatFile}
	// A part row's file is the job's too: a job's only part sits at the
	// archive's own name once a finalize has renamed it there
	// (renameSinglePartToPlain). Parts that cannot be read name nothing
	// here; the age rule below still stands.
	if segs, err := o.db.GetSegments(jobID); err == nil {
		for _, s := range segs {
			owned = append(owned, s.FilePath, s.ChatFile)
		}
	}
	for _, own := range owned {
		if own != "" && normalizePath(own) == normalizePath(path) {
			return false
		}
	}
	started, err := time.Parse(time.RFC3339, job.DownloadStartedAt)
	return err == nil && info.ModTime().Before(started)
}

// otherRows is what every row but jobID's own names in the output tree
// (outputOwners, the rule the Files sweep and its delete use): the other
// jobs with their parts, and the trims. Read only once a finalize has found
// a file it did not write at its name, which is rare; a finalize that finds
// none reads nothing.
func (o *DownloadOrchestrator) otherRows(jobID string) (*outputOwners, error) {
	jobs, err := o.db.GetAllJobs()
	if err != nil {
		return nil, err
	}
	trims, err := o.db.GetAllTrims()
	if err != nil {
		return nil, err
	}
	others := make([]*database.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.ID != jobID {
			others = append(others, j)
		}
	}
	root := resolveOutputDir(&config.MoomboxConfig{})
	if o.globalOutputDir != nil {
		root = o.globalOutputDir()
	}
	return newOutputOwners(others, trims, root), nil
}

// setForeignFileAside makes path free for a finalize of jobID to write. A
// file the job did not create that is there (foreignFileAt) is set aside the
// way a Reinitialize sets an archive aside (renameAside): renamed to
// "<stem>.restart-<unix ts>[-N]<ext>" beside it, with those of sidecars that
// share its stem and are not the job's own either, under a marker naming
// jobID, so Files does not offer it while the job exists. One Warn names the
// old and the new path. Moombox never replaces or deletes it afterwards.
//
// Not a file another row names (otherRows): two jobs whose names resolve to
// one file — an output template without ${id} in a shared folder, two rows
// for one video. Renaming it would leave that row naming this job's
// recording, and its own under this job's marker. It is refused instead, as
// is a file whose owner cannot be read, and a sidecar another row names
// stays where it is.
//
// An error whenever the file is still at path afterwards — it is another
// row's, or the rename failed: the caller must not write there.
func (o *DownloadOrchestrator) setForeignFileAside(jobID, path string, sidecars []string) error {
	if !o.foreignFileAt(jobID, path) {
		return nil
	}
	others, err := o.otherRows(jobID)
	if err != nil {
		o.logger.Warn("finalize: a file that is not this job's holds the name it writes, and the rows could not be read to tell whose it is",
			"path", path, "err", err, "jobID", jobID)
		return fmt.Errorf("%s already exists and is not this job's: it is kept, not replaced; use Mux to try again", filepath.Base(path))
	}
	if row := others.fileRow(path); row != "" {
		o.logger.Warn("finalize: another row's file holds the name it writes; it is kept where it is",
			"path", path, "owner", row, "jobID", jobID)
		return fmt.Errorf("%s already exists and belongs to %s: it is kept, not replaced; give one of the two another name, then use Mux", filepath.Base(path), row)
	}
	stem := asideBase(path)
	var foreign []string
	for _, ext := range sidecars {
		if o.foreignFileAt(jobID, stem+ext) && others.fileRow(stem+ext) == "" {
			foreign = append(foreign, ext)
		}
	}
	mv := renameAside(jobID, path, foreign)
	if mv.err != nil {
		o.logger.Warn("finalize: a file that is not this job's holds the name it writes, and could not be set aside",
			"path", path, "err", mv.err, "jobID", jobID)
		return fmt.Errorf("%s already exists and is not this job's: it is kept, not replaced; move or rename it, then use Mux", filepath.Base(path))
	}
	// The marker a reset left under the file's own name when it could not
	// rename it (setArchiveAside) has done its work once nothing of that
	// reset's is left there: it would outlive the job as a stray in Files.
	if id, ok := readAsideMarker(stem + asideOwnerSuffix); ok && id == jobID && len(mv.left) == 0 {
		os.Remove(stem + asideOwnerSuffix)
	}
	args := []any{"from", path, "to", mv.dst, "jobID", jobID}
	if mv.markerErr != nil {
		args = append(args, "markerErr", mv.markerErr)
	}
	for _, l := range mv.left {
		args = append(args, "notMoved", l.path, "notMovedErr", l.err)
	}
	o.logger.Warn("finalize: a file from before this recording held the name it writes, and was set aside; it is kept, never replaced", args...)
	return nil
}

// SetAsideArchive is one file of a job that was set aside, by a Reinitialize
// (setArchiveAside) or by a finalize that found it at the name it writes
// (setForeignFileAside), as both UIs' job details list it.
type SetAsideArchive struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ScanSetAsideArchives lists the archives set aside for job that are still
// where they were put: derived from the disk on each call, never stored. It
// reads the directory of the archive the row names and the one the job's next
// finalize resolves to (outputTargetOf, from the global output directory and
// the output template), and returns every set-aside archive there whose
// marker names the job, oldest first — and a chat set aside with no archive
// (a finalize found only a chat at its name), which nothing else would name
// to the operator. One whose directory the job
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
		// The archive is the file under an owned name that is neither the
		// marker nor a sidecar, whatever its container: an imported archive
		// keeps its own extension when it is set aside. A name that holds no
		// archive is listed by its chat.
		hasArchive := map[string]bool{}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if mine[asideBase(p)] && !isAsideCompanion(p) && !e.IsDir() {
				hasArchive[asideBase(p)] = true
			}
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			base := asideBase(p)
			if !mine[base] || e.IsDir() {
				continue
			}
			loneChat := !hasArchive[base] && strings.HasSuffix(strings.ToLower(p), ".chat.json")
			if isAsideCompanion(p) && !loneChat {
				continue
			}
			if info, err := e.Info(); err == nil {
				out = append(out, SetAsideArchive{Name: e.Name(), Path: p, Size: info.Size()})
			}
		}
	}
	return out
}

// isAsideCompanion reports whether path is a set-aside archive's marker or
// one of its sidecars rather than the archive itself.
func isAsideCompanion(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range append([]string{asideOwnerSuffix}, archiveSidecarExts...) {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// normalizePath returns a cleaned absolute path suitable for map-key comparison.
// On Windows (case-insensitive filesystem), paths are lowercased.
func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs
}

// OrphanedEntry represents an orphaned file or directory found during scanning.
type OrphanedEntry struct {
	Path      string `json:"path"`                // Absolute path (for deletion)
	RelPath   string `json:"relPath"`             // Relative path (for display)
	Type      string `json:"type"`                // "staging", "output", or "trim"
	Size      int64  `json:"size"`                // Total size in bytes
	Modified  string `json:"modified"`            // ISO 8601 timestamp
	JobID     string `json:"jobId,omitempty"`     // Associated job ID (if found)
	JobTitle  string `json:"jobTitle,omitempty"`  // Job title (if found)
	JobStatus string `json:"jobStatus,omitempty"` // Job status (if found)
	// Asides names the set-aside recordings (engine.StagedRestartSuffix) a
	// staging dir still holds — media the engine preserved rather than
	// truncated, which no mux has consumed. Such a dir is only ever offered
	// once its preservation window has lapsed, and naming the files is what
	// tells the operator this is captured footage rather than scratch space.
	Asides []string `json:"asides,omitempty"`
}

// ScanOrphanedFiles scans staging and output directories for orphaned files.
func ScanOrphanedFiles(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	var entries []OrphanedEntry

	// Scan staging directories
	stagingEntries, err := scanStagingOrphans(db, cfg)
	if err == nil {
		entries = append(entries, stagingEntries...)
	}

	// Scan output files, trims included: a trim is a file under the output
	// directory like any other, told apart by the trims table and the
	// directories the trim service writes into (scanOutputOrphans), never by
	// a second walk that trusted a directory's name.
	outputEntries, err := scanOutputOrphans(db, cfg)
	if err == nil {
		entries = append(entries, outputEntries...)
	}

	return entries, nil
}

// activeJobStatuses lists statuses that mean a job is actively using its
// staging and/or output paths. Deletion must refuse these.
var activeJobStatuses = map[database.JobStatus]bool{
	database.StatusDownloading: true,
	database.StatusLive:        true,
	database.StatusUpcoming:    true,
	database.StatusMuxing:      true,
}

// IsActiveJobStatus reports whether a status means the job is actively using
// its staging and output paths. Exported so the REST layer can refuse a
// recovery on a live job without re-listing the four statuses — the list lives
// here, beside the deletion refusal that has always used it.
func IsActiveJobStatus(s database.JobStatus) bool { return activeJobStatuses[s] }

// jobNeedsStaging reports whether a Finished job's staging directory was
// deliberately preserved rather than cleaned up, and so must NOT be offered
// (or allowed) as a deletable orphan. Mirrors the exact carve-out applied at
// job-finish time (see (*DownloadWorker).cleanupStagingAfterMux in
// worker.go): a Finished job's staging only survives cleanup for five
// reasons — it's flagged IncompleteTail (tail is Resume-able), its chat
// capture ended incomplete (no verb re-pages from the capture kept in
// staging — Retry refuses a Finished job and Reinitialize starts over — but
// it can be the only copy of those comments when the archive's chat copy
// failed, an aside recovery carries it beside the recovered recording, and
// the operator can take it by hand), it still holds a recording the engine set
// aside rather than truncated (engine.StagedRestartSuffix), it still has
// an unmuxed captured part (recoverable via the Mux action), or its root
// holds a recording the finalize did not use (unusedRootRecording — the
// from-the-start download beside a job that finalized as parts, or a second
// recording beside the one a single-file finalize muxed). The tail and
// chat shields expire on one age rule — which is ON by default: the option
// behind it, downloader.incomplete_staging_expiry_days, ships at 7 days ("0 =
// preserve forever" describes the VALUE 0, not the default). The set-aside,
// unmuxed-part and unused-root shields have no age rule at all: each holds
// captured media that can exist nowhere else.
//
// This predicate must stay precise: any OTHER Finished job's staging is a
// genuine orphan (e.g. a stale dir left by an old/removed job) and must
// remain cleanable, so this does NOT protect Finished jobs unconditionally.
func jobNeedsStaging(db *database.Database, cfg *config.MoomboxConfig, job *database.Job, jobStagingDir string) bool {
	if job == nil || job.Status != database.StatusFinished {
		return false
	}
	// One age rule, read once, so that the doc's claim above — the two
	// expiring shields expire together — is visible in the expression.
	notExpired := !incompleteStagingExpired(cfg, job)
	// The aside shield is NOT on that rule (fix round 1, Important 2).
	// Finalize muxes every READABLE set-aside recording into its own sibling
	// file and deletes it, so one still sitting in staging is one FFmpeg could
	// not read — the only copy of footage that exists nowhere else, which is
	// the whole point of preserving rather than truncating it. An age rule
	// here would offer it for deletion seven days later on a stock install,
	// because incomplete_staging_expiry_days DEFAULTS TO 7; it is shielded
	// until it is muxed or the job is deleted. The unmuxed-PART shield below
	// is unconditional for the same reason.
	asideShield := len(stagedAsideRecordings(jobStagingDir)) > 0
	// The unused-root shield is cleanupStagingAfterMux's, read by the same
	// function, and off the age rule for the aside shield's reason: the
	// recording it keeps can be the longer copy (a complete VOD download the
	// parts did not take), and the cleanup kept the dir precisely so it would
	// not be lost. Without it the sweep offered that dir as an ordinary
	// orphan — one click from deleting what the cleanup had preserved.
	return (job.IncompleteTail && notExpired) ||
		(job.ChatStatus == chatStatusIncomplete && notExpired) ||
		asideShield ||
		hasUnmuxedSegmentParts(db, job.ID, jobStagingDir) ||
		unusedRootRecording(db, job.ID, jobStagingDir) != ""
}

// incompleteStagingExpired reports whether an incomplete_tail job's staging
// preservation window has lapsed (downloader.incomplete_staging_expiry_days;
// 0 = never expires). Only the disk-heavy staging shield expires — the flag
// itself, the honest "may be missing its tail" badge, is never cleared here:
// YouTube cannot resume a broadcast days later, so week-old interruption
// staging has zero resume value, while the badge's information keeps its
// value indefinitely (owner ruling 2026-08-21). Age is measured from the
// job's last update: any activity — an auto-resume attempt, a title
// refresh — restarts the window, which errs toward preservation. An
// unparseable timestamp also errs toward preservation (never expires).
func incompleteStagingExpired(cfg *config.MoomboxConfig, job *database.Job) bool {
	if cfg == nil {
		return false
	}
	days := cfg.Downloader.IncompleteStagingExpiryDays.Days()
	if days <= 0 {
		return false
	}
	updated, err := time.Parse(time.RFC3339, job.UpdatedAt)
	if err != nil {
		return false
	}
	return time.Since(updated) >= time.Duration(days*24)*time.Hour
}

// NotOrphanError is DeleteOrphanedFile's refusal of a path that is no longer
// an orphan: a job or trim row names it (or, for a directory, a file inside
// it), a running finalize is writing it, or it is the staging an active job
// uses or a finished one keeps. The sweep never lists such a path, so the
// list the delete came from was read before that owner appeared, and the
// answer is to refresh it — which is what the message says. The REST layer
// answers it 409; the terminal shows the message as it stands.
type NotOrphanError struct {
	Owner string // who owns the path now: "job <id>", "a trim of job <id>"
	How   string // how it owns it: "names it", "is writing it", "needs it", ...
}

// Error is the whole message, written for the operator: the dashboard toasts
// it and the terminal's Files dialog shows it as it stands.
func (e *NotOrphanError) Error() string {
	return "No longer an orphan: " + e.Owner + " " + e.How + " now. Refresh the list."
}

// DeleteOrphanedFile safely deletes a file or directory if it's under the
// configured directories. Immediately before deleting it re-reads the
// database and refuses, with a *NotOrphanError, a path that is no longer an
// orphan (orphanOwner) — the window between ScanOrphanedFiles and the
// operator's click, in which a job can restart and make its staging live
// again, a finalize can finish and name its archive, or a trim can be
// re-created over the file the listing offered.
func DeleteOrphanedFile(path string, db *database.Database, cfg *config.MoomboxConfig) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	// Verify the path is under the staging or output directory, comparing
	// canonical spellings on BOTH sides (symlinks, junctions, 8.3 short names;
	// a missing path through its deepest existing ancestor). Canonicalising
	// the candidate alone refused every legitimate delete on a short-named or
	// junctioned drive as a "symlink escape".
	stagingDir, outputDir := resolveStagingDir(cfg), resolveOutputDir(cfg)
	realPath, err := utils.CanonicalPath(absPath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	if !isUnderDirectory(realPath, canonicalDir(stagingDir)) && !isUnderDirectory(realPath, canonicalDir(outputDir)) {
		// The raw spelling sits inside the tree but the canonical one does
		// not: a link pointing out of it.
		if isUnderDirectory(absPath, stagingDir) || isUnderDirectory(absPath, outputDir) {
			return fmt.Errorf("path escapes configured directories via symlink")
		}
		return fmt.Errorf("path is not under staging or output directory")
	}

	// Recheck: the sweep filtered owned paths out, but its list is a
	// snapshot. The DB, read now, is authoritative.
	if db != nil {
		if notOrphan, err := orphanOwner(absPath, db, cfg); err != nil {
			return fmt.Errorf("check for an owner: %w", err)
		} else if notOrphan != nil {
			return notOrphan
		}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("path not found: %w", err)
	}

	if info.IsDir() {
		return os.RemoveAll(absPath)
	}
	return os.Remove(absPath)
}

// canonicalDir is CanonicalPath for a configured directory, falling back to
// the given spelling when it cannot be resolved (it need not exist yet).
func canonicalDir(dir string) string {
	if c, err := utils.CanonicalPath(dir); err == nil {
		return c
	}
	return dir
}

// orphanOwner returns why absPath is not an orphan now, or nil when it still
// is one. Three owners, each read at the moment of the delete:
//
//   - a running finalize, part mux, trim encode or aside recovery that is
//     writing it, or (for a directory) writing inside it — outputClaims, which
//     own a file before any column names it;
//   - the job whose staging it is, while that job is active or keeps its
//     staging (findActiveJobForPath);
//   - any job or trim row that names it, whatever the job's status
//     (outputOwners, the rule the sweep lists by).
//
// The last was once a recheck of ACTIVE jobs' columns and of trim rows alone,
// so a Finished job's archive that appeared after the listing — a mux that
// finished between the sweep and the click, a re-download onto a listed
// leftover's name — was deleted by a Delete click on the stale list. A
// genuine orphan is never named by a row, so refusing every row's file costs
// nothing deletable. The rows are read in one pass: every job with its parts
// (one GetAllJobs) and every trim (one GetAllTrims), no per-row query.
func orphanOwner(absPath string, db *database.Database, cfg *config.MoomboxConfig) (*NotOrphanError, error) {
	// One check covers both spellings: claimOutputStem records the stem in
	// its configured spelling and its canonical one.
	if id := outputClaimOwner(absPath); id != "" {
		return &NotOrphanError{Owner: "job " + id, How: "is writing it"}, nil
	}
	if id := outputClaimOwnerUnder(absPath); id != "" {
		return &NotOrphanError{Owner: "job " + id, How: "is writing a file in it"}, nil
	}
	if id, err := findActiveJobForPath(absPath, db, cfg); err != nil {
		return nil, err
	} else if id != "" {
		return &NotOrphanError{Owner: "job " + id, How: "needs it"}, nil
	}
	jobs, err := db.GetAllJobs()
	if err != nil {
		return nil, err
	}
	trims, err := db.GetAllTrims()
	if err != nil {
		return nil, err
	}
	if owner, how := newOutputOwners(jobs, trims, resolveOutputDir(cfg)).ownerOf(absPath); owner != "" {
		return &NotOrphanError{Owner: owner, How: how}, nil
	}
	return nil, nil
}

// findActiveJobForPath returns the ID of the job whose staging the given path
// is, while that job is active or keeps its staging (jobNeedsStaging), or ""
// otherwise. The job is the first path component under the staging directory
// (staging/<jobID>/...). An output path is never a staging path: the rows
// that name output files are orphanOwner's outputOwners.
//
// The lookup runs on the path as given against the configured directory,
// then again on its canonical spelling against the canonical directory —
// the both-sides rule DeleteOrphanedFile's containment check already uses.
// The first pass alone let a request spell an active job's staging through
// the real directory behind a symlinked or junctioned staging_directory: it
// passed containment (canonical on both sides), then filepath.Rel against the
// configured spelling found no job, and the job's staging was RemoveAll'd.
func findActiveJobForPath(absPath string, db *database.Database, cfg *config.MoomboxConfig) (string, error) {
	stagingDir := resolveStagingDir(cfg)
	if id, err := findActiveJobUnder(absPath, stagingDir, db, cfg); err != nil || id != "" {
		return id, err
	}
	realPath, err := utils.CanonicalPath(absPath)
	if err != nil {
		return "", nil
	}
	realStaging := canonicalDir(stagingDir)
	if realPath == absPath && realStaging == stagingDir {
		return "", nil // nothing spells differently; the first pass was the whole answer
	}
	return findActiveJobUnder(realPath, realStaging, db, cfg)
}

// findActiveJobUnder is findActiveJobForPath's lookup for one spelling of the
// path and the staging directory.
func findActiveJobUnder(absPath, stagingDir string, db *database.Database, cfg *config.MoomboxConfig) (string, error) {
	if rel, err := filepath.Rel(stagingDir, absPath); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		// Path under staging/ — the first component is the jobID
		parts := strings.SplitN(rel, string(filepath.Separator), 2)
		if len(parts) > 0 && parts[0] != "" {
			jobID := parts[0]
			job, err := db.GetJob(jobID)
			if err != nil {
				return "", err
			}
			if job != nil {
				jobStagingDir := filepath.Join(stagingDir, jobID)
				if activeJobStatuses[job.Status] || jobNeedsStaging(db, cfg, job, jobStagingDir) {
					return jobID, nil
				}
			}
		}
		return "", nil
	}
	return "", nil
}

func resolveStagingDir(cfg *config.MoomboxConfig) string {
	dir := cfg.Paths.StagingDirectory
	if dir == "" {
		dir = "./staging"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func resolveOutputDir(cfg *config.MoomboxConfig) string {
	dir := cfg.Paths.OutputDirectory
	if dir == "" {
		dir = "./output"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func isUnderDirectory(path, dir string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	// Normalize case on Windows for case-insensitive filesystem comparison
	if runtime.GOOS == "windows" {
		absPath = strings.ToLower(absPath)
		absDir = strings.ToLower(absDir)
	}
	// Normalize and ensure the path is strictly under the directory
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	// Reject paths that escape via ".."
	return !strings.HasPrefix(rel, "..") && rel != "."
}

// scanStagingOrphans scans the staging directory for orphaned subdirectories.
func scanStagingOrphans(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	stagingDir := cfg.Paths.StagingDirectory
	if stagingDir == "" {
		stagingDir = "./staging"
	}

	dirEntries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var entries []OrphanedEntry
	absStagingDir, _ := filepath.Abs(stagingDir)

	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}

		jobID := de.Name()
		absPath := filepath.Join(absStagingDir, jobID)
		if outputClaimOwner(absPath) != "" {
			continue // an off-queue recovery is reading it
		}

		// Check if job exists and its status
		job, err := db.GetJob(jobID)
		if err == nil && job != nil {
			if IsActiveJobStatus(job.Status) {
				// Active job — skip, staging is in use
				continue
			}
			if jobNeedsStaging(db, cfg, job, absPath) {
				// Finished but deliberately preserved (IncompleteTail, an
				// incomplete chat capture, a set-aside recording, an unmuxed
				// part, or a root recording the finalize did not use) — not
				// a genuine orphan, skip.
				continue
			}
		}

		// Compute directory size and modified time
		size, modified := dirSizeAndModified(absPath)

		relPath, _ := filepath.Rel(absStagingDir, absPath)
		if relPath == "" {
			relPath = jobID
		}

		entry := OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "staging",
			Size:     size,
			Modified: modified.UTC().Format(time.RFC3339),
		}

		// Name any set-aside recording the dir still holds. Reaching here
		// means its shield has lapsed (or the job row is gone entirely), so
		// this offer is real — and the difference between "scratch space" and
		// "footage FFmpeg could not read" is exactly what the operator needs
		// before clicking delete.
		for _, aside := range stagedAsideRecordings(absPath) {
			entry.Asides = append(entry.Asides, filepath.Base(aside))
		}

		if job != nil {
			entry.JobID = job.ID
			entry.JobTitle = job.Title
			entry.JobStatus = string(job.Status)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// scanOutputOrphans scans the output directory for files that no job and no
// trim references. One walk covers archives and trims alike: a file is owned
// when a job column names it or a trim row resolves to it (trimFileLocations),
// whatever directory it sits in, and an unowned file is typed "trim" when its
// directory is one the trim service writes into (trimDirsOf), else "output".
//
// The trim half used to be a second walk keyed on the directory's NAME: the
// output walk skipped every directory called "trim", and the trim walk offered
// every file in one that no TRIM row named — so a channel whose name sanitises
// to "trim" (the default template is "${channel}/...") had its archives,
// chat, thumbnails and descriptions offered for deletion as trims. And it
// resolved every trim row against the GLOBAL output directory, while the trim
// service writes beside the job's own output — under a per-channel or per-job
// output_directory, every live trim was offered too, and Delete removed it.
func scanOutputOrphans(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	outputDir := cfg.Paths.OutputDirectory
	if outputDir == "" {
		outputDir = "./output"
	}

	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(absOutputDir); os.IsNotExist(err) {
		return nil, nil
	}

	// What the rows own (outputOwners): every job with its parts, and every
	// trim in one query, not one per job (sweep-2 ENGINE-17). A failed read
	// fails the scan: without the trim rows every live trim reads as unowned.
	jobs, err := db.GetAllJobs()
	if err != nil {
		return nil, err
	}
	trims, err := db.GetAllTrims()
	if err != nil {
		return nil, err
	}
	owners := newOutputOwners(jobs, trims, absOutputDir)

	var entries, trimEntries []OrphanedEntry
	// Recovered set-aside recordings found in the walk, grouped by the stem
	// they belong to. They are never rows of their own: either the stem is a
	// known archive and the sibling is owned (dropped here), or the stem is
	// itself an orphan and the siblings are folded into ITS entry below.
	siblingsByStem := map[string]*orphanedSiblings{}

	err = filepath.Walk(absOutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			return nil
		}

		// Only check media, chat, thumbnail, and description files — except
		// in a trims directory, the trim service's own, where whatever an
		// encode left is offered whatever its extension (the trim walk's rule
		// before the two walks became one). A job's files there are still
		// owned: the known check below runs for every directory.
		absPath, _ := filepath.Abs(path)
		inTrimDir := owners.isTrimDir(filepath.Dir(absPath))
		ext := strings.ToLower(filepath.Ext(path))
		isMedia := ext == ".mp4" || ext == ".mkv" || ext == ".webm" || ext == ".ts"
		isThumbnail := ext == ".jpg" || ext == ".webp" || ext == ".png"
		isChat := strings.HasSuffix(strings.ToLower(path), ".chat.json")
		isDescription := ext == ".description"
		if !inTrimDir && !isMedia && !isThumbnail && !isChat && !isDescription {
			return nil
		}

		// Referenced by a job or a trim, in either spelling — the delete's own
		// rule (ownerOf). A recovered set-aside recording
		// (<stem>.restart-<ts>[-N].<ext>) is captured footage the finalize
		// muxed out of staging, and it is deliberately NOT a segment row —
		// which made it unreferenced by construction, and so a one-click
		// deletion from the Files tab the moment it was written (fix round 1,
		// Important 1). It belongs to the archive whose stem it carries: owned
		// here while that archive is known, and otherwise folded into the
		// archive's own entry below rather than offered as a row of its own.
		if owner, _ := owners.ownerOf(absPath); owner != "" {
			return nil
		}
		if outputClaimOwner(absPath) != "" {
			return nil // Being written by a finalize that has not named it yet
		}
		if stemPath, ok := asideSiblingStem(absPath); ok {
			group := siblingsByStem[stemPath]
			if group == nil {
				group = &orphanedSiblings{dir: filepath.Dir(absPath)}
				siblingsByStem[stemPath] = group
			}
			group.names = append(group.names, filepath.Base(absPath))
			return nil
		}

		relPath, _ := filepath.Rel(absOutputDir, absPath)

		entry := OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "output",
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		}
		if inTrimDir {
			entry.Type = "trim"
			trimEntries = append(trimEntries, entry)
			return nil
		}
		entries = append(entries, entry)

		return nil
	})
	if err != nil {
		return entries, err
	}

	// Trims after the archives, the order the two walks used to give (the
	// terminal's list shows the sweep in the order it comes).
	return append(appendOrphanedSiblings(entries, siblingsByStem, absOutputDir), trimEntries...), nil
}

// trimDirsOf returns the directories the trim service writes a job's trims
// into: "trim" beside the job's output file (CreateTrim), and beside each part
// for a job that finalized as parts (createMultiSegmentTrimInternal writes
// beside the first part the range touches) — each where its column names it
// and where the moved archive tree puts it (rowAbsoluteLocations).
func trimDirsOf(job *database.Job, absOutputDir string) []string {
	var dirs []string
	for _, p := range rowAbsoluteLocations(job, job.OutputFile, absOutputDir) {
		dirs = append(dirs, filepath.Join(filepath.Dir(p), "trim"))
	}
	for _, seg := range job.Segments {
		for _, p := range rowAbsoluteLocations(job, seg.FilePath, absOutputDir) {
			dirs = append(dirs, filepath.Join(filepath.Dir(p), "trim"))
		}
	}
	return dirs
}

// trimFileLocations returns the absolute paths a trim row can name. The row
// stores its file relative to the JOB's output directory — "trim/<name>"
// under the directory of the job's relative filename (CreateTrim) — which is
// the job's own output_directory when it has one and the global one
// otherwise; so that is the first base it resolves against. A job carries an
// output_directory far more often than an override implies: the monitor and
// an import store the global directory there at creation when the channel has
// none of its own (resolveOutputDir, cmd/moombox), and buildJobContext writes
// under it from then on. The file was written beside the job's output, or
// beside the part the range began in, so those spellings count too.
//
// The current global directory is a candidate as well, added to those rather
// than replaced by them: it is what owned these files before the job's own
// directory was read, and it is the only spelling that still finds a trim
// after the operator moves the archive tree and repoints
// paths.output_directory at it — the job's pinned directory and its absolute
// output_file both still name the old tree, while the archive (its relative
// filename joined to the global directory) stays owned. Without it every such
// trim was listed as an orphan and Delete removed the file its row names.
// A row whose job is gone resolves against the global directory, all it has;
// an absolute row is its own answer.
func trimFileLocations(tr database.TrimRecord, job *database.Job, absOutputDir string) []string {
	if tr.Filename == "" {
		return nil
	}
	if filepath.IsAbs(tr.Filename) {
		return []string{tr.Filename}
	}
	paths := rowRelativeLocations(job, tr.Filename, absOutputDir)
	if job == nil {
		return paths
	}
	name := filepath.Base(tr.Filename)
	for _, dir := range trimDirsOf(job, absOutputDir) {
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths
}

// rowRelativeLocations returns the absolute paths a column a row stores
// relative to its JOB's output directory can name — a trim row's filename,
// and the job's own filename and chat_filename: under the job's
// output_directory when it has one, the directory the player, the chat route
// and Open Folder join them to; and under the current global directory as
// well, for trimFileLocations' reason (a moved archive tree with
// paths.output_directory repointed at it, while the job still names the old
// tree). A job that is gone, or has no directory of its own, has only the
// global one. An empty column names nothing.
//
// The sweep joined the job's relative columns to the global directory alone
// (the class of W23-07, which trims had), so a file that a job under a
// per-channel or per-job output_directory names only by its relative column —
// no absolute output_file or chat_file naming the same spelling — was listed
// as an orphan, and Delete removed the archive the player still plays.
func rowRelativeLocations(job *database.Job, rel, absOutputDir string) []string {
	if rel == "" {
		return nil
	}
	var paths []string
	if job != nil && job.OutputDirectory != "" {
		if abs, err := filepath.Abs(job.OutputDirectory); err == nil && abs != absOutputDir {
			paths = append(paths, filepath.Join(abs, rel))
		}
	}
	return append(paths, filepath.Join(absOutputDir, rel))
}

// rowAbsoluteLocations returns the paths an absolute column of a job's row can
// name: the column as stored, and — when it lies under the job's
// output_directory and that is not the current global directory — the same
// path under the global one: rowRelativeLocations' "global as well" rule, for
// the columns that store the job's directory in their spelling. After the
// archive tree is moved and paths.output_directory repointed at it, these
// columns still name the old tree, and nothing else names a split job's parts
// (its filename is the base they share, which names no file) or any job's
// thumbnail and description; without the re-rooted spelling the sweep listed
// them and Delete removed the recordings. An empty column names nothing.
func rowAbsoluteLocations(job *database.Job, p, absOutputDir string) []string {
	if p == "" {
		return nil
	}
	paths := []string{p}
	if job == nil || job.OutputDirectory == "" {
		return paths
	}
	jobDir, err := filepath.Abs(job.OutputDirectory)
	if err != nil || jobDir == absOutputDir {
		return paths
	}
	rel, err := filepath.Rel(jobDir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return paths // not under the job's directory: the column's own spelling is all it has
	}
	return append(paths, filepath.Join(absOutputDir, rel))
}

// jobFileLocations returns the absolute paths a job row names: its absolute
// columns — the output, the chat, the thumbnail and the description, and each
// part's video and chat (a quality or gap split) — as rowAbsoluteLocations
// resolves them, and its relative ones (filename, chat_filename) as
// rowRelativeLocations resolves them. The relative ones must count too:
// imported jobs once set ONLY those, and without them their perfectly valid
// files were offered as orphans — deleting them left a broken Finished job.
func jobFileLocations(job *database.Job, absOutputDir string) []string {
	var paths []string
	for _, p := range []string{job.OutputFile, job.ChatFile, job.ThumbnailFile, job.DescriptionFile} {
		paths = append(paths, rowAbsoluteLocations(job, p, absOutputDir)...)
	}
	for _, rel := range []string{job.Filename, job.ChatFilename} {
		paths = append(paths, rowRelativeLocations(job, rel, absOutputDir)...)
	}
	for _, seg := range job.Segments {
		for _, p := range []string{seg.FilePath, seg.ChatFile} {
			paths = append(paths, rowAbsoluteLocations(job, p, absOutputDir)...)
		}
	}
	return paths
}

// outputOwners is what the job and trim rows own in the output tree — the one
// rule the sweep lists by (scanOutputOrphans) and the delete re-checks by
// (orphanOwner): both ask ownerOf, so a path the sweep would not list now is a
// path the delete refuses now. Keys are normalizePath spellings, each row path
// in two: as the row spells it, and through its directory's canonical
// spelling (spellings); a value names the row as NotOrphanError.Owner spells
// it. Not safe for concurrent use: spellings fills its cache as it goes.
type outputOwners struct {
	// files is every file a row names: a job's columns and parts
	// (jobFileLocations), and every place a trim row can resolve to
	// (trimFileLocations).
	files map[string]string
	// stems is files with the container extension off, plus the base a
	// job's parts share ("X" for "X - part2.mp4"): a recovered set-aside
	// recording is named after one (asideSiblingStem), and with no thumbnail
	// or description to carry a split job's stem its siblings were offered as
	// strays (pinnedPartLocation names them after the parts' base).
	stems map[string]string
	// dirs is every directory holding a file in files, and every directory
	// above it: deleting one takes that file along.
	dirs map[string]string
	// trimDirs is every directory the trim service writes into: "trim"
	// beside a job's output and each of its parts (trimDirsOf), and the
	// directory each trim row resolves to. An unowned file there is a trim —
	// a clip whose row DeleteTrim removed (it leaves the file for the sweep),
	// or an encode that died.
	trimDirs map[string]bool
	// canon caches canonicalDir by normalised directory, so each distinct
	// directory is resolved once however many rows and walked files sit in it.
	canon map[string]string
}

// newOutputOwners builds outputOwners from rows already read: every job with
// its parts, and every trim. A path two rows name keeps the first, a job's
// ahead of a trim's.
//
// Each row path is indexed in its canonical spelling as well as its own. The
// rows store whatever spelling the configuration had when they were written —
// a per-channel output_directory spelled through a link into the global tree,
// a global directory since respelled from a link to its target, a symlinked
// or junctioned output directory — while the walk and a delete request can
// name the same file another way. With the rows in their stored spelling
// alone, the sweep listed such a file (a Finished job's archive, thumbnail,
// description) and the delete refused it as "Refresh the list.", so the list
// could never be cleared; and a request naming a channel's directory, or a
// recovered set-aside recording, by its real path matched no row and was
// deleted with the archives in it.
func newOutputOwners(jobs []*database.Job, trims []database.TrimRecord, absOutputDir string) *outputOwners {
	o := &outputOwners{
		files: map[string]string{}, stems: map[string]string{}, dirs: map[string]string{},
		trimDirs: map[string]bool{}, canon: map[string]string{},
	}
	first := func(m map[string]string, k, owner string) {
		if _, ok := m[k]; !ok {
			m[k] = owner
		}
	}
	own := func(p, owner string) {
		for _, n := range o.spellings(p) {
			first(o.files, n, owner)
			first(o.stems, strings.TrimSuffix(n, filepath.Ext(n)), owner)
			// Up from the file until a directory already holds an owner:
			// every one above that does too.
			for d := filepath.Dir(n); ; d = filepath.Dir(d) {
				if _, ok := o.dirs[d]; ok {
					break
				}
				o.dirs[d] = owner
				if filepath.Dir(d) == d {
					break
				}
			}
		}
	}
	ownStem := func(p, owner string) {
		for _, n := range o.spellings(p) {
			first(o.stems, n, owner)
		}
	}
	trimDir := func(dir string) {
		for _, n := range o.spellings(dir) {
			o.trimDirs[n] = true
		}
	}
	jobsByID := make(map[string]*database.Job, len(jobs))
	for _, job := range jobs {
		jobsByID[job.ID] = job
		owner := "job " + job.ID
		for _, p := range jobFileLocations(job, absOutputDir) {
			own(p, owner)
		}
		for _, seg := range job.Segments {
			for _, p := range rowAbsoluteLocations(job, seg.FilePath, absOutputDir) {
				if dir, base, ok := recordedArchiveLocation(p); ok {
					ownStem(filepath.Join(dir, base), owner)
				}
			}
		}
		for _, dir := range trimDirsOf(job, absOutputDir) {
			trimDir(dir)
		}
	}
	for _, tr := range trims {
		owner := "a trim of job " + tr.JobID
		for _, p := range trimFileLocations(tr, jobsByID[tr.JobID], absOutputDir) {
			own(p, owner)
			trimDir(filepath.Dir(p))
		}
	}
	return o
}

// spellings returns p's normalised spelling and, when it differs, the one
// through its directory's canonical spelling (canonicalDir: symlinks,
// junctions, 8.3 short names; a missing directory through its deepest
// existing ancestor). The last element is kept as it is: deleting a link
// removes the link, never what it points at, so a link is its own entry. The
// rows' paths and the paths asked about go through the same function, so
// whichever spelling each side uses, they meet in the canonical one.
func (o *outputOwners) spellings(p string) []string {
	n := normalizePath(p)
	dir := filepath.Dir(n)
	c, ok := o.canon[dir]
	if !ok {
		c = normalizePath(canonicalDir(dir))
		o.canon[dir] = c
	}
	if c == dir {
		return []string{n}
	}
	return []string{n, filepath.Join(c, filepath.Base(n))}
}

// isTrimDir reports whether dir, in either spelling, is one the trim service
// writes into.
func (o *outputOwners) isTrimDir(dir string) bool {
	for _, n := range o.spellings(dir) {
		if o.trimDirs[n] {
			return true
		}
	}
	return false
}

// ownerOf returns the row that owns absPath and how it owns it, or "" when no
// row does. The path is matched in both its spellings against the rows' (see
// spellings):
//
//   - a row names the file;
//   - it is a recovered set-aside recording named after a file a row names;
//   - it is a directory holding a file a row names, which deleting it would
//     take along. The sweep never lists a directory under the output tree, so
//     only a request built by hand can name one.
//
// The sweep asks it of every file it walks and the delete of every path it is
// given, so the two cannot disagree about a row.
func (o *outputOwners) ownerOf(absPath string) (owner, how string) {
	for _, n := range o.spellings(absPath) {
		if row, ok := o.files[n]; ok {
			return row, "names it"
		}
		if stem, ok := asideSiblingStem(n); ok {
			if row, ok := o.stems[stem]; ok {
				return row, "names its archive"
			}
		}
		if row, ok := o.dirs[n]; ok {
			return row, "names a file in it"
		}
	}
	return "", ""
}

// asideSiblingStem returns the normalised stem a recovered set-aside
// recording (<stem>.restart-<ts>[-N].<ext>) is named after, and whether the
// path is one.
//
// A recovered recording's chat archive is <stem>.restart-<ts>.chat.json.
// RestartSiblingStem strips ONE extension, which leaves ".chat" glued to the
// timestamp and makes the name fail its digits rule — so without folding the
// compound extension first, the chat file recoverAsides writes beside a
// sibling was offered as a deletable orphan the moment it landed, while the
// sibling itself was correctly owned.
func asideSiblingStem(absPath string) (string, bool) {
	base := filepath.Base(absPath)
	if strings.HasSuffix(strings.ToLower(base), ".chat.json") {
		base = base[:len(base)-len(".json")]
	}
	stem, ok := engine.RestartSiblingStem(base)
	if !ok {
		return "", false
	}
	return normalizePath(filepath.Join(filepath.Dir(absPath), stem)), true
}

// appendOrphanedSiblings attaches each recovered set-aside recording to the
// orphaned archive it belongs to, and gives the ones whose archive is gone
// too a row of their own — named, so the operator can tell captured footage
// from scratch space before deleting it. Without that last part a sibling
// whose archive was already deleted would be invisible to the sweep AND
// unreachable from it: preserved forever with no way to reclaim the disk.
func appendOrphanedSiblings(entries []OrphanedEntry, siblingsByStem map[string]*orphanedSiblings, absOutputDir string) []OrphanedEntry {
	if len(siblingsByStem) == 0 {
		return entries
	}
	for i := range entries {
		stem := normalizePath(strings.TrimSuffix(entries[i].Path, filepath.Ext(entries[i].Path)))
		if group, ok := siblingsByStem[stem]; ok {
			entries[i].Asides = append(entries[i].Asides, group.names...)
			delete(siblingsByStem, stem)
		}
	}
	// Deterministic order: map iteration is not, and the sweep's output is
	// compared in tests and rendered in a table.
	stems := make([]string, 0, len(siblingsByStem))
	for stem := range siblingsByStem {
		stems = append(stems, stem)
	}
	sort.Strings(stems)
	for _, stem := range stems {
		group := siblingsByStem[stem]
		sort.Strings(group.names)
		for _, name := range group.names {
			abs := filepath.Join(group.dir, name)
			info, err := os.Stat(abs)
			if err != nil {
				continue // vanished between the walk and here
			}
			relPath, _ := filepath.Rel(absOutputDir, abs)
			entries = append(entries, OrphanedEntry{
				Path:     abs,
				RelPath:  relPath,
				Type:     "output",
				Size:     info.Size(),
				Modified: info.ModTime().UTC().Format(time.RFC3339),
				Asides:   []string{name},
			})
		}
	}
	return entries
}

// orphanedSiblings is one archive stem's recovered set-aside recordings, with
// the directory in its real spelling (the map key is normalised for Windows'
// case-insensitive comparison, which is not a path to hand back).
type orphanedSiblings struct {
	dir   string
	names []string
}

// dirSizeAndModified computes total size and latest modification time for a directory.
func dirSizeAndModified(dir string) (int64, time.Time) {
	var totalSize int64
	var latestMod time.Time

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		if info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
		}
		return nil
	})

	if latestMod.IsZero() {
		latestMod = time.Now()
	}

	return totalSize, latestMod
}

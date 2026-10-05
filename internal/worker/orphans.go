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

	// Scan output files
	outputEntries, err := scanOutputOrphans(db, cfg)
	if err == nil {
		entries = append(entries, outputEntries...)
	}

	// Scan trim files
	trimEntries, err := scanTrimOrphans(db, cfg)
	if err == nil {
		entries = append(entries, trimEntries...)
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
// worker.go): a Finished job's staging only survives cleanup for four
// reasons — it's flagged IncompleteTail (tail is Resume-able), its chat
// capture ended incomplete (no verb re-pages from the capture kept in
// staging — Retry refuses a Finished job and Reinitialize starts over — but
// it can be the only copy of those comments when the archive's chat copy
// failed, an aside recovery carries it beside the recovered recording, and
// the operator can take it by hand), it still holds a recording the engine set
// aside rather than truncated (engine.StagedRestartSuffix), or it still has
// an unmuxed captured part (recoverable via the Mux action). The tail and
// chat shields expire on one age rule — which is ON by default: the option
// behind it, downloader.incomplete_staging_expiry_days, ships at 7 days ("0 =
// preserve forever" describes the VALUE 0, not the default). The set-aside
// and unmuxed-part shields have no age rule at all: both hold captured media
// that exists nowhere else.
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
	return (job.IncompleteTail && notExpired) ||
		(job.ChatStatus == chatStatusIncomplete && notExpired) ||
		asideShield ||
		hasUnmuxedSegmentParts(db, job.ID, jobStagingDir)
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

// DeleteOrphanedFile safely deletes a file or directory if it's under the configured directories.
// Re-queries the database immediately before deletion and refuses if any currently-active job owns
// the path — closes the race window between ScanOrphanedFiles and the user's delete click during
// which a user might restart a job and make its staging/output path live again.
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

	// Recheck: refuse if the path is now owned by an active job. Scan filtered these out, but a job
	// could have been restarted between scan and delete. DB lookup is authoritative.
	if db != nil {
		if jobID, err := findActiveJobForPath(absPath, db, cfg); err != nil {
			return fmt.Errorf("check for active job: %w", err)
		} else if jobID != "" {
			return fmt.Errorf("refusing to delete: path is now owned by active job %s", jobID)
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

// findActiveJobForPath returns the ID of a currently-active job that owns the given path,
// or "" if the path is not associated with any active job.
//
// A path a running finalize is still writing (outputClaims) is owned by that
// job before any column names it. Otherwise, for staging paths, the jobID is
// the first path component under the staging directory (staging/<jobID>/...);
// for output paths, we scan all jobs and check their output/chat/
// thumbnail/description/segment file paths for a normalized match.
//
// The lookup runs on the path as given against the configured directories,
// then again on its canonical spelling against the canonical directories —
// the both-sides rule DeleteOrphanedFile's containment check already uses.
// The first pass alone let a request spell an active job's staging through
// the real directory behind a symlinked or junctioned staging_directory: it
// passed containment (canonical on both sides), then filepath.Rel against the
// configured spelling found no job, and the job's staging was RemoveAll'd.
func findActiveJobForPath(absPath string, db *database.Database, cfg *config.MoomboxConfig) (string, error) {
	// One check covers both spellings: claimOutputStem records the stem in
	// its configured spelling and its canonical one.
	if id := outputClaimOwner(absPath); id != "" {
		return id, nil
	}
	stagingDir, outputDir := resolveStagingDir(cfg), resolveOutputDir(cfg)
	if id, err := findActiveJobUnder(absPath, stagingDir, outputDir, db, cfg); err != nil || id != "" {
		return id, err
	}
	realPath, err := utils.CanonicalPath(absPath)
	if err != nil {
		return "", nil
	}
	realStaging, realOutput := canonicalDir(stagingDir), canonicalDir(outputDir)
	if realPath == absPath && realStaging == stagingDir && realOutput == outputDir {
		return "", nil // nothing spells differently; the first pass was the whole answer
	}
	return findActiveJobUnder(realPath, realStaging, realOutput, db, cfg)
}

// findActiveJobUnder is findActiveJobForPath's lookup for one spelling of the
// path and the two directories. A job column is matched in its stored
// spelling and in its canonical one, so the canonical pass recognises a file
// the row names through the configured (linked) directory.
func findActiveJobUnder(absPath, stagingDir, outputDir string, db *database.Database, cfg *config.MoomboxConfig) (string, error) {
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

	if rel, err := filepath.Rel(outputDir, absPath); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		// Path under output/ — scan active jobs for a matching file reference
		jobs, err := db.GetAllJobs()
		if err != nil {
			return "", err
		}
		target := normalizePath(absPath)
		names := func(candidate string) bool {
			return normalizePath(candidate) == target || normalizePath(canonicalDir(candidate)) == target
		}
		absOut, absErr := filepath.Abs(outputDir)
		for _, job := range jobs {
			if !activeJobStatuses[job.Status] {
				continue
			}
			for _, candidate := range []string{job.OutputFile, job.ChatFile, job.ThumbnailFile, job.DescriptionFile} {
				if candidate != "" && names(candidate) {
					return job.ID, nil
				}
			}
			// Relative-path columns too (imports set ONLY these). The
			// delete-time recheck deliberately considers these for ACTIVE jobs
			// only (the status filter above), mirroring how scanOutputOrphans
			// treats them for ALL jobs when building the orphan list — the
			// recheck just has to refuse deleting a file an active job still
			// owns, not reproduce the full scan set.
			if absErr == nil {
				for _, rel := range []string{job.Filename, job.ChatFilename} {
					if rel != "" && names(filepath.Join(absOut, rel)) {
						return job.ID, nil
					}
				}
			}
			for _, seg := range job.Segments {
				if seg.FilePath != "" && names(seg.FilePath) {
					return job.ID, nil
				}
				if seg.ChatFile != "" && names(seg.ChatFile) {
					return job.ID, nil
				}
			}
		}
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
				// incomplete chat capture, a set-aside recording, or an
				// unmuxed part) — not a genuine orphan, skip.
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

// scanOutputOrphans scans the output directory for files not referenced by any job.
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

	// Collect all known output and chat file paths from DB.
	// Uses normalizePath for case-insensitive comparison on Windows.
	jobs, err := db.GetAllJobs()
	if err != nil {
		return nil, err
	}

	knownFiles := make(map[string]bool)
	// knownStems is knownFiles with the container extension off, so a
	// recovered set-aside recording can be recognised as belonging to the
	// archive whose name it carries (see the sibling branch in the walk).
	knownStems := make(map[string]bool)
	known := func(p string) {
		n := normalizePath(p)
		knownFiles[n] = true
		knownStems[strings.TrimSuffix(n, filepath.Ext(n))] = true
	}
	for _, job := range jobs {
		if job.OutputFile != "" {
			known(job.OutputFile)
		}
		if job.ChatFile != "" {
			known(job.ChatFile)
		}
		if job.ThumbnailFile != "" {
			known(job.ThumbnailFile)
		}
		if job.DescriptionFile != "" {
			known(job.DescriptionFile)
		}
		// The RELATIVE-path columns must count too: imported jobs set ONLY
		// Filename/ChatFilename (no absolute OutputFile/ChatFile), so
		// without these their perfectly valid files would be offered as
		// orphans — and deleting them leaves a broken Finished job.
		if job.Filename != "" {
			known(filepath.Join(absOutputDir, job.Filename))
		}
		if job.ChatFilename != "" {
			known(filepath.Join(absOutputDir, job.ChatFilename))
		}
		// Include part (quality/gap split) files so they aren't flagged as
		// orphans — both the videos and their per-part chat files.
		for _, seg := range job.Segments {
			if seg.FilePath != "" {
				known(seg.FilePath)
			}
			if seg.ChatFile != "" {
				known(seg.ChatFile)
			}
		}
	}

	var entries []OrphanedEntry
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
			// Skip trim directories — handled separately
			if info.Name() == "trim" {
				return filepath.SkipDir
			}
			return nil
		}

		// Only check media, chat, thumbnail, and description files
		ext := strings.ToLower(filepath.Ext(path))
		isMedia := ext == ".mp4" || ext == ".mkv" || ext == ".webm" || ext == ".ts"
		isThumbnail := ext == ".jpg" || ext == ".webp" || ext == ".png"
		isChat := strings.HasSuffix(strings.ToLower(path), ".chat.json")
		isDescription := ext == ".description"
		if !isMedia && !isThumbnail && !isChat && !isDescription {
			return nil
		}

		absPath, _ := filepath.Abs(path)
		if knownFiles[normalizePath(absPath)] {
			return nil // Referenced by a job
		}
		if outputClaimOwner(absPath) != "" {
			return nil // Being written by a finalize that has not named it yet
		}

		// A recovered set-aside recording (<stem>.restart-<ts>[-N].<ext>) is
		// captured footage the finalize muxed out of staging, and it is
		// deliberately NOT a segment row — which made it unreferenced by
		// construction, and so a one-click deletion from the Files tab the
		// moment it was written (fix round 1, Important 1). It belongs to the
		// archive whose stem it carries: owned while that archive is known,
		// and otherwise folded into the archive's own entry rather than
		// offered as a row of its own.
		//
		// A recovered recording's chat archive is <stem>.restart-<ts>.chat.json.
		// RestartSiblingStem strips ONE extension, which leaves ".chat" glued to
		// the timestamp and makes the name fail its digits rule — so without
		// folding the compound extension first, the chat file recoverAsides
		// writes beside a sibling is offered as a deletable orphan the moment it
		// lands, while the sibling itself is correctly owned.
		sibBase := filepath.Base(absPath)
		if isChat {
			sibBase = strings.TrimSuffix(sibBase, ".json")
		}
		if stem, ok := engine.RestartSiblingStem(sibBase); ok {
			stemPath := normalizePath(filepath.Join(filepath.Dir(absPath), stem))
			if knownStems[stemPath] {
				return nil // its job still has the archive this belongs to
			}
			group := siblingsByStem[stemPath]
			if group == nil {
				group = &orphanedSiblings{dir: filepath.Dir(absPath)}
				siblingsByStem[stemPath] = group
			}
			group.names = append(group.names, filepath.Base(absPath))
			return nil
		}

		relPath, _ := filepath.Rel(absOutputDir, absPath)

		entries = append(entries, OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "output",
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})

		return nil
	})
	if err != nil {
		return entries, err
	}

	return appendOrphanedSiblings(entries, siblingsByStem, absOutputDir), nil
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

// scanTrimOrphans scans for trim files not referenced by any DB trim record.
func scanTrimOrphans(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
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

	// One query, not one per job (sweep-2 ENGINE-17).
	trims, err := db.GetAllTrims()
	if err != nil {
		return nil, err
	}

	knownTrimFiles := make(map[string]bool, len(trims))
	for _, tr := range trims {
		// Resolve trim path: relative to output dir
		trimAbs := tr.Filename
		if !filepath.IsAbs(trimAbs) {
			trimAbs = filepath.Join(absOutputDir, trimAbs)
		}
		knownTrimFiles[normalizePath(trimAbs)] = true
	}

	var entries []OrphanedEntry

	// Walk looking for */trim/*.mp4 patterns
	err = filepath.Walk(absOutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		// Only consider files inside "trim" directories
		dir := filepath.Dir(path)
		if filepath.Base(dir) != "trim" {
			return nil
		}

		absPath, _ := filepath.Abs(path)
		if knownTrimFiles[normalizePath(absPath)] {
			return nil // Referenced by a trim record
		}
		if outputClaimOwner(absPath) != "" {
			return nil // An encode that has not recorded its trim yet
		}

		relPath, _ := filepath.Rel(absOutputDir, absPath)

		entries = append(entries, OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "trim",
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})

		return nil
	})
	if err != nil {
		return entries, err
	}

	return entries, nil
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

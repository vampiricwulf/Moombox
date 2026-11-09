package worker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// ReinitializePreview counts what a Reinitialize of one job would delete
// (owner decision PR-M1): the one verb that destroys captured footage now
// says so before it runs. Both UIs ask GET /api/jobs/{id}/reinitialize/preview
// (the TUI through its ReinitializePreview callback) and name these counts in
// their confirm — the Web dialog, the TUI's A I I line — as the Files tab
// names what a staging delete takes.
//
// It counts exactly what reinitializeNow removes, read the same way:
//   - SetAsideRecordings: the recordings the engine set aside in staging
//     (ScanAsides — what A S would recover), deleted with the staging dir;
//   - RecordedParts / RecordedPartBytes: the split parts' files in the output
//     directory that clearJobParts removes (segment rows whose file is still
//     on disk);
//   - StagedFiles / StagedBytes: everything in the staging dir os.RemoveAll
//     takes, the set-asides included;
//   - ImportedChapters: the chapters of an imported chapter list
//     (utils.ReadImportedChaptersSidecar) that dropImportedArchiveChapters
//     or removeSplitArchiveChapters removes — a finalize's own list is
//     rebuilt by the re-download and is not counted.
//
// And what it cancels (owner ruling R-D2): PendingAutoMux / PendingAutoRetry
// count the jobs whose row is waiting on an automatic action
// (jobstate.PendingAutoAction). A Reinitialize takes the row out of Error, so
// the Twitch monitor no longer offers it — and for a pending mux it deletes
// the very staging the mux would have archived.
//
// Nothing here is a lock: the job can still change between the preview and
// the Reinitialize, which acts on what is there then. The counts are the
// operator's information, not a contract.
type ReinitializePreview struct {
	SetAsideRecordings int   `json:"setAsideRecordings"`
	RecordedParts      int   `json:"recordedParts"`
	RecordedPartBytes  int64 `json:"recordedPartBytes"`
	StagedFiles        int   `json:"stagedFiles"`
	StagedBytes        int64 `json:"stagedBytes"`
	ImportedChapters   int   `json:"importedChapters"`
	PendingAutoMux     int   `json:"pendingAutoMux"`
	PendingAutoRetry   int   `json:"pendingAutoRetry"`
}

// PreviewReinitialize builds job's ReinitializePreview. Takes (stagingBase,
// db) rather than a worker for the same reason ScanAsides does: the REST
// layer reads the base out of the config store and has no worker to ask.
// A file that cannot be read is simply not counted.
func PreviewReinitialize(db *database.Database, stagingBase string, job *database.Job) ReinitializePreview {
	var out ReinitializePreview
	if job == nil {
		return out
	}
	switch jobstate.PendingAutoAction(job) {
	case jobstate.AutoActionMux:
		out.PendingAutoMux = 1
	case jobstate.AutoActionRetry:
		out.PendingAutoRetry = 1
	}
	stagingDir := filepath.Join(stagingBase, job.ID)
	out.SetAsideRecordings = len(ScanAsides(stagingBase, job.ID).Groups)
	_ = filepath.WalkDir(stagingDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// The display-aspect record is metadata, never a recording
		// (HasStagingFiles): a job holding only it has nothing staged.
		if d.Name() == displayAspectFile {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			out.StagedFiles++
			out.StagedBytes += info.Size()
		}
		return nil
	})

	// The chapter lists the reset drops, each counted once: beside the first
	// part (removeSplitArchiveChapters) and beside the archive the row names
	// (dropImportedArchiveChapters) — the same file for a split row whose
	// output_file names part 1.
	lists := map[string]bool{}
	if db != nil {
		if segs, err := db.GetSegments(job.ID); err == nil {
			for _, s := range segs {
				if s.FilePath == "" {
					continue
				}
				if info, serr := os.Stat(s.FilePath); serr == nil && !info.IsDir() {
					out.RecordedParts++
					out.RecordedPartBytes += info.Size()
				}
			}
			if len(segs) > 0 {
				if p := ArchiveSidecarPath(segs[0].FilePath, utils.ChaptersSidecarSuffix); p != "" {
					lists[p] = true
				}
			}
		}
	}
	if p := ArchiveSidecarPath(job.OutputFile, utils.ChaptersSidecarSuffix); p != "" {
		lists[p] = true
	}
	for p := range lists {
		if ic, ok, _ := utils.ReadImportedChaptersSidecar(p); ok && ic != nil {
			out.ImportedChapters += len(ic.Chapters)
		}
	}
	return out
}

// Add sums o into p: a batch's confirm names the totals across its jobs.
func (p *ReinitializePreview) Add(o ReinitializePreview) {
	p.SetAsideRecordings += o.SetAsideRecordings
	p.RecordedParts += o.RecordedParts
	p.RecordedPartBytes += o.RecordedPartBytes
	p.StagedFiles += o.StagedFiles
	p.StagedBytes += o.StagedBytes
	p.ImportedChapters += o.ImportedChapters
	p.PendingAutoMux += o.PendingAutoMux
	p.PendingAutoRetry += o.PendingAutoRetry
}

// Summary is the confirm's clause naming what the reset deletes, e.g.
// "deletes 1 set-aside recording, 2 recorded parts (1.2GB), 40 staged files
// (850.0MB) and 5 imported chapters", or "deletes nothing
// recorded" when there is nothing to lose — led, when a job's row waits on
// an automatic action, by what the reset cancels: "cancels the pending
// automatic mux, and deletes …" (a batch: "cancels 2 pending automatic muxes
// and 1 pending automatic retry, and …"). The warning comes first because the
// TUI's A I confirm line reads Summary ahead of the job's title and that line
// is cut to the terminal's width, so its tail is what a narrow terminal
// loses. The dashboard's twin is reinitializeDeletesText
// (web/public/modules/utils.js), worded alike, sizes as utils.FormatFileSize writes them.
func (p ReinitializePreview) Summary() string {
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	var parts []string
	if p.SetAsideRecordings > 0 {
		parts = append(parts, plural(p.SetAsideRecordings, "set-aside recording", "set-aside recordings"))
	}
	if p.RecordedParts > 0 {
		parts = append(parts, plural(p.RecordedParts, "recorded part", "recorded parts")+" ("+utils.FormatFileSize(p.RecordedPartBytes)+")")
	}
	if p.StagedFiles > 0 {
		parts = append(parts, plural(p.StagedFiles, "staged file", "staged files")+" ("+utils.FormatFileSize(p.StagedBytes)+")")
	}
	if p.ImportedChapters > 0 {
		parts = append(parts, plural(p.ImportedChapters, "imported chapter", "imported chapters"))
	}
	var deletes string
	switch len(parts) {
	case 0:
		deletes = "deletes nothing recorded"
	case 1:
		deletes = "deletes " + parts[0]
	default:
		deletes = "deletes " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
	if c := p.cancelsClause(); c != "" {
		return c + ", and " + deletes
	}
	return deletes
}

// cancelsClause is Summary's head naming the automatic actions the reset
// cancels, "" when there are none.
func (p ReinitializePreview) cancelsClause() string {
	pending := func(n int, one, many string) string {
		if n == 1 {
			return "the pending automatic " + one
		}
		return fmt.Sprintf("%d pending automatic %s", n, many)
	}
	var c []string
	if p.PendingAutoMux > 0 {
		c = append(c, pending(p.PendingAutoMux, "mux", "muxes"))
	}
	if p.PendingAutoRetry > 0 {
		c = append(c, pending(p.PendingAutoRetry, "retry", "retries"))
	}
	if len(c) == 0 {
		return ""
	}
	return "cancels " + strings.Join(c, " and ")
}

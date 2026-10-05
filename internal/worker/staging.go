package worker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// HasStagingFiles returns true if the staging directory for a job exists and is non-empty.
func HasStagingFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// HasSegmentFiles returns true if the staging directory contains recognized
// segment files, either at the root or inside a quality-split seg_N
// subdirectory (post-split jobs keep their newest data there — without the
// subdirectory check, the Mux recovery action stays hidden for them).
//
// Recognition is delegated to discoverStagingMedia so this visibility probe
// and the actual mux recovery (muxFromStaging) can never disagree about what
// counts as recoverable media.
func HasSegmentFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	if discoverStagingMedia(dir) != nil {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "seg_") {
			if discoverStagingMedia(filepath.Join(dir, e.Name())) != nil {
				return true
			}
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

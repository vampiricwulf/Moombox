package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// vodRootMarkerFile, in a job's staging ROOT, says the root belongs to a
// from-the-start recording (a VOD-classified run: the whole-file download,
// the post-live manifest-free capture from sq=0, or a VOD DASH manifest) of a
// job that had ALREADY split into parts — a live capture that quality-split,
// was interrupted, and came back after the broadcast had ended.
//
// Such a job has two recordings and its staging layout could only describe
// one of them. The root is part 0 unless seg_0 exists, so the from-the-start
// download was either muxed as if it were part 0 or — once part 0 was
// recorded — ignored by finalizeMultiSegmentJob, which never looks at root
// media again, and then deleted with staging: the archive was the partial
// live parts and the complete recording was gone. The marker is what lets
// the finalize tell the two apart, and its content is the run's state:
//
//   - vodRootDownloading: written by claimStagingRootForVod when the run
//     starts. The root holds (or will hold) the from-the-start recording, so
//     a finalize that still takes the parts — the download failed and the
//     operator chose Mux — must not delete it (unusedRootRecording).
//   - vodRootComplete: written by markVodRootComplete when the download ends
//     with nothing missing. The recording is the archive, and the parts are
//     superseded (supersedePartsWithVod).
//
// Removed with the rest of staging; a finalize never deletes it on its own.
const vodRootMarkerFile = ".vod-root"

const (
	vodRootDownloading = "downloading"
	vodRootComplete    = "complete"
)

// vodRootState returns the marker's state, or "" when the root carries none.
func vodRootState(stagingDir string) string {
	b, err := os.ReadFile(filepath.Join(stagingDir, vodRootMarkerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeVodRootState writes the marker through utils.WriteFileAtomic: a torn
// marker would read as some third state that is neither.
func writeVodRootState(stagingDir, state string) error {
	return utils.WriteFileAtomic(filepath.Join(stagingDir, vodRootMarkerFile), []byte(state+"\n"), 0o644)
}

// jobHasParts reports whether a job has split into parts: a segment row, or a
// seg_N staging dir (a tombstoned one too — its part was merged, not undone).
// A segment read that fails answers from the dirs alone.
func jobHasParts(db *database.Database, jobID, stagingDir string) bool {
	if len(segDirsOf(stagingDir, true)) > 0 {
		return true
	}
	segs, err := db.GetSegments(jobID)
	return err == nil && len(segs) > 0
}

// claimStagingRootForVod readies a split job's staging root for a
// from-the-start run, before the strategy writes anything. A job that never
// split, or whose root a previous run already claimed, needs nothing moved.
//
// Part 0's live capture is what normally sits in the root, and it cannot stay
// there: the run either downloads beside it (and every discovery prefers the
// live-shape name) or, for the post-live capture, writes the SAME name and
// resumes or sets aside part 0's file. So it moves into seg_0, the dir the
// layout already reads as part 0 whenever one exists — a recorded part 0
// stays recorded (its row names its output, not its staging), an unrecorded
// one is muxed from there like any other part. seg_0 is created even when the
// root holds nothing, because "root is part 0 unless seg_0 exists" is what
// keeps every later finalize from muxing the from-the-start recording as
// part 0.
//
// When seg_0 already exists the root's live capture is the short span the
// split deliberately did not mux (under 10 s of the old quality), so it is
// set aside by the engine's convention instead — kept, and muxed into a
// sibling at finalize, never into a part.
func (o *DownloadOrchestrator) claimStagingRootForVod(jobCtx *JobContext) error {
	root := jobCtx.StagingDir
	if vodRootState(root) != "" {
		return writeVodRootState(root, vodRootDownloading)
	}
	if !jobHasParts(o.db, jobCtx.Job.ID, root) {
		return nil
	}
	seg0 := filepath.Join(root, "seg_0")
	if info, err := os.Stat(seg0); err == nil && info.IsDir() {
		asides, err := setAsideStagedMedia(root, liveShapeStagingNames)
		if len(asides) > 0 {
			o.logger.Warn("a short span the quality split skipped was set aside before the from-the-start download; it is muxed to its own file beside the archive",
				"asides", strings.Join(asides, " | "), "jobID", jobCtx.Job.ID)
		}
		if err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(seg0, 0o755); err != nil {
			return fmt.Errorf("create part 0 staging dir: %w", err)
		}
		for _, name := range liveShapeStagingNames {
			for _, p := range []string{filepath.Join(root, name), engine.StagedRestartSidecar(filepath.Join(root, name))} {
				if !fileExists(p) {
					continue
				}
				if err := utils.ReplaceFile(p, filepath.Join(seg0, filepath.Base(p))); err != nil {
					return fmt.Errorf("move part 0's capture out of the staging root: %w", err)
				}
			}
		}
	}
	if err := writeVodRootState(root, vodRootDownloading); err != nil {
		return fmt.Errorf("mark the staging root for the from-the-start download: %w", err)
	}
	o.logger.Info("job already split into parts; the from-the-start download takes the staging root and part 0 moves to seg_0",
		"jobID", jobCtx.Job.ID)
	return nil
}

// markVodRootComplete records that the from-the-start download finished with
// nothing missing, which is what lets the finalize supersede the parts with
// it. A no-op for a root no run claimed. A write that fails leaves the parts
// as the archive and the download shielded in staging (unusedRootRecording),
// which loses nothing.
func (o *DownloadOrchestrator) markVodRootComplete(jobCtx *JobContext) {
	if vodRootState(jobCtx.StagingDir) == "" {
		return
	}
	if err := writeVodRootState(jobCtx.StagingDir, vodRootComplete); err != nil {
		o.logger.Warn("could not mark the from-the-start download complete; the finalize keeps the parts and the download stays in staging",
			"err", err, "jobID", jobCtx.Job.ID)
	}
}

// supersedePartsWithVod retires a split job's parts in favour of the complete
// from-the-start recording in the staging root, which the single-file
// finalize then muxes as the archive.
//
// Nothing is deleted. Each part's file (and per-part chat) moves beside the
// archive as a sibling under the archive's own stem —
// <stem>.restart-<part start>.mp4, the name a recovered set-aside recording
// takes (asideOutputPath) — which the output sweep folds under the archive
// (engine.RestartSiblingStem) rather than offering it as a stray. A part's
// live capture is not guaranteed to be inside the VOD (YouTube trims long
// archives, and a creator can edit one), so it is kept; it is just no longer
// the archive. The rows and gap rows go, so the job reads as the single file
// it now is, and each recorded part's seg_N dir is tombstoned
// (mergeTombstoneFile) so no later finalize re-muxes its media into a part.
//
// Runs BEFORE the archive is muxed: a part renamed to the plain name by an
// earlier finalize sits exactly where the archive is about to be written.
// Re-entrant: a part whose file has already moved is skipped, so a crash
// between the moves and the row delete costs nothing. Any other failure
// returns with the rows intact.
func (o *DownloadOrchestrator) supersedePartsWithVod(jobCtx *JobContext) error {
	segments, err := o.db.GetSegments(jobCtx.Job.ID)
	if err != nil {
		return fmt.Errorf("read the parts to supersede: %w", err)
	}
	if len(segments) == 0 {
		return nil
	}
	// The same split muxAndFinalize makes of a template that may carry a
	// subdirectory ("${channel}/...").
	filenameBase := jobCtx.Filename
	outputDir := filepath.Dir(filepath.Join(jobCtx.OutputDir, filenameBase+".mp4"))
	filenameBase = filepath.Base(filenameBase)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	used := map[string]bool{}
	for _, seg := range segments {
		if seg.FilePath == "" || !fileExists(seg.FilePath) {
			o.logger.Warn("a superseded part's file is not where its row says; there is nothing of it to keep",
				"part", seg.SegmentIndex+1, "file", seg.FilePath, "jobID", jobCtx.Job.ID)
			continue
		}
		stamp := seg.UnixStart
		if stamp <= 0 {
			stamp = time.Now().Unix()
		}
		dst, ok := asideOutputPath(outputDir, filenameBase, strconv.FormatInt(stamp, 10), used)
		if !ok {
			return fmt.Errorf("no free name beside the archive for superseded part %d", seg.SegmentIndex+1)
		}
		if err := utils.ReplaceFile(seg.FilePath, dst); err != nil {
			return fmt.Errorf("move superseded part %d beside the archive: %w", seg.SegmentIndex+1, err)
		}
		if seg.ChatFile != "" && fileExists(seg.ChatFile) {
			chatDst := strings.TrimSuffix(dst, filepath.Ext(dst)) + ".chat.json"
			if err := utils.ReplaceFile(seg.ChatFile, chatDst); err != nil {
				return fmt.Errorf("move superseded part %d's chat beside the archive: %w", seg.SegmentIndex+1, err)
			}
		}
		o.logger.Warn("a part of this job was superseded by the complete VOD download; it is kept beside the archive, not deleted",
			"part", seg.SegmentIndex+1, "from", seg.FilePath, "to", dst, "jobID", jobCtx.Job.ID)
	}

	recorded := make(map[int]bool, len(segments))
	for _, s := range segments {
		recorded[s.SegmentIndex] = true
	}
	stamp := []byte(time.Now().UTC().Format(time.RFC3339))
	for _, sd := range stagedSegDirs(jobCtx.StagingDir) {
		if !recorded[sd.idx] {
			continue // never muxed: stays a part, and shielded, until it is
		}
		if err := os.WriteFile(filepath.Join(sd.dir, mergeTombstoneFile), stamp, 0o644); err != nil {
			return fmt.Errorf("tombstone superseded part %d's staging: %w", sd.idx+1, err)
		}
	}
	if err := o.db.ClearJobSegmentsAndGaps(jobCtx.Job.ID); err != nil {
		return fmt.Errorf("clear the superseded part rows: %w", err)
	}
	o.logger.Info("the complete VOD download replaces the job's parts as its archive",
		"parts", len(segments), "jobID", jobCtx.Job.ID)
	return nil
}

// unusedRootRecording returns the recording in a job's staging root that its
// finalize did not use, or "" when there is none. cleanupStagingAfterMux keeps
// the whole dir for one.
//
// For a job that finalized as PARTS, the root is part 0 (used, through its
// row) or the short span its split deliberately skipped (discarded by design)
// — unless it holds a from-the-start recording, which a finalize that took the
// parts did not use: the root a VOD run claimed (vodRootMarkerFile), or the
// whole-file pair, which only DownloadVod ever writes and no part is made
// from. That is the complete download finalizeMultiSegmentJob used to ignore
// and the cleanup then deleted.
//
// A single-file finalize muxed ONE recording from the root, so a root holding
// a live-shape capture AND the whole-file pair kept one it did not use.
// setAsideLiveShapesForVod stops a run producing that layout; a staging dir
// that already had it when that landed still holds the complete download
// beside the capture the restart mux preferred.
//
// A segment read that fails answers "unused", as hasUnmuxedSegmentParts does:
// keeping the dir is the side that cannot lose footage.
func unusedRootRecording(db *database.Database, jobID, stagingDir string) string {
	media := discoverStagingMedia(stagingDir)
	if media == nil {
		return ""
	}
	path := media.VideoPath
	if path == "" {
		path = media.AudioPath
	}
	var wholeFile string
	for _, name := range []string{"video.mp4", "audio.m4a"} {
		if p := filepath.Join(stagingDir, name); fileExists(p) {
			wholeFile = p
			break
		}
	}
	segs, err := db.GetSegments(jobID)
	if err != nil {
		return path
	}
	if len(segs) == 0 {
		if wholeFile != "" && (fileExists(filepath.Join(stagingDir, "video_stream")) || fileExists(filepath.Join(stagingDir, "video.ts"))) {
			return wholeFile
		}
		return ""
	}
	if vodRootState(stagingDir) != "" {
		return path
	}
	return wholeFile
}

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// partMerger implements the Tier 4 same-format part merge with its I/O calls
// (ffprobe, ffmpeg concat, filesystem rename, DB replace/update) as function
// fields, defaulting to the real implementations in mergeSameFormatParts.
// Tests inject stubs so the grouping/row-building/fallback/failure-recovery
// logic runs without a live ffmpeg toolchain — see part_merge_test.go.
type partMerger struct {
	ffprobePath string
	logger      logger

	probe      func(ctx context.Context, ffprobePath, filePath string) (*streamParams, error)
	concat     func(ctx context.Context, inputs []string, outputPath string) error
	replace    func(jobID string, segs []database.Segment) error
	updateFile func(id int, filename, filePath, chatFile string) error
	rename     func(oldpath, newpath string) error
}

// mergeSameFormatParts opportunistically concat-copies contiguous runs of
// parts whose stream parameters are identical (Tier 4 of the interruption
// spec). Decisions come from ffprobe on the actual files — the Segment
// rows' quality metadata lacks audio parameters — and any probe or concat
// failure returns the input untouched: the merge is an improvement, never
// a gate on finalize. Merged media is written next to the parts as
// "<base> - merged<runIndex>.mp4", committed to the DB at that temp path,
// then best-effort renamed onto the run's first part's name once that
// commit succeeds. Only once the row is confirmed to resolve to a real
// file does cleanup remove the now-superseded later parts: their output
// files, their chat files (but only when THIS run's chat actually merged —
// a failed chat merge always leaves every per-part chat file in place),
// and their pre-mux seg_N staging directories (removing those is what
// stops a later finalize re-entry from resurrecting and re-merging
// already-consumed content — see merge's doc comment for the full chain).
func (o *DownloadOrchestrator) mergeSameFormatParts(ctx context.Context, jobCtx *JobContext, segments []database.Segment) []database.Segment {
	pm := &partMerger{
		ffprobePath: o.muxer.FFprobePath(),
		logger:      o.logger,
		probe:       probeStreamParams,
		concat:      o.muxer.ConcatCopy,
		replace:     o.db.ReplaceJobSegments,
		updateFile:  o.db.UpdateSegmentFile,
		rename:      os.Rename,
	}
	return pm.merge(ctx, jobCtx.Job.ID, jobCtx.StagingDir, segments)
}

// mergeRun tracks one contiguous multi-part run through the merge/commit/
// cleanup pipeline: the concat-copy output (and, when chat merged, its
// sibling) start under temp names next to the parts, get folded into the
// single db.ReplaceJobSegments call alongside every other row, and only
// after that commit succeeds are they best-effort renamed onto the run's
// first part's original name and the superseded later parts removed.
//
// chatTemp doubles as the "did this run's chat actually merge" flag: it is
// "" both when no part in the run had chat AND when mergeChatFiles failed —
// either way, no per-part chat file may be deleted for this run.
type mergeRun struct {
	indices        []int // indices into the ORIGINAL segments slice
	videoTemp      string
	chatTemp       string // "" when no chat was merged for this run
	bytes          int64
	replacementIdx int // index into the replacement slice built by merge()
}

// merge is the injectable core of mergeSameFormatParts. See that method's
// doc comment for the overall algorithm.
//
// Crash-consistency choice: every part-merge row is committed to the DB
// BEFORE any original filename is touched. The merged row initially points
// at the (already-written, already-valid) temp concat output — never at a
// name that doesn't exist yet — so a crash or error at any point before the
// single db.ReplaceJobSegments call leaves the original parts completely
// untouched (the true "return input unchanged" contract). The alternative
// (rename the temp file onto the first part's original name BEFORE
// committing the DB row) was rejected: it would silently overwrite an
// original part's file the moment any LATER run in the same batch failed to
// concat, breaking the "input unchanged on failure" guarantee for the
// earlier, already-succeeded runs.
//
// Once the commit succeeds, the rename-onto-the-first-part-name step is
// best-effort polish, but the follow-up UpdateSegmentFile call is not
// treated as equally disposable: if the rename lands but UpdateSegmentFile
// fails, the committed row would be left naming a path that no longer
// exists (the file just moved out from under it) — the only copy of the
// recording sitting under a name nothing references. Rather than accept
// that dangling reference, this renames the file BACK onto its temp path so
// the already-committed row (which still names that temp path, since it was
// never updated) resolves to a real file again — at the cost of losing the
// pretty final name until a later successful merge or manual repair. Only
// when that undo ALSO fails (a narrow double-fault) does this skip the
// run's superseded-part cleanup entirely: at that point there is no
// reliable pointer to the merged content, and the later parts' still-intact
// independent files are the last remaining copy of their span.
//
// Superseded-part cleanup — deleting the later parts' output files, chat
// files, and pre-mux seg_N staging directories — only runs once the row is
// confirmed to resolve to a real file (see above). The seg_N staging
// cleanup specifically is what prevents a later finalize re-entry (a
// restart mid-finalize, the Mux action, an IncompleteTail Resume) from
// finding a superseded part's raw pre-mux media still sitting in staging
// with no segment row (ReplaceJobSegments just deleted that row),
// concluding via muxUnrecordedSegments that it was never muxed, and
// re-persisting + re-merging it — duplicating content that's already
// inside the merged output. The run's FIRST part keeps its own row
// identity (mergedSegmentRow keeps SegmentIndex = first's), so its own
// seg_N staging dir is never mistaken for unmuxed and is deliberately left
// alone here: cleaning it up too would need to special-case SegmentIndex 0
// living at the staging ROOT rather than a seg_N subdirectory, for a purely
// cosmetic space win the normal end-of-job staging cleanup already captures
// once every remaining part is accounted for.
func (pm *partMerger) merge(ctx context.Context, jobID, stagingDir string, segments []database.Segment) []database.Segment {
	if len(segments) < 2 {
		return segments
	}

	params := make([]*streamParams, len(segments))
	for i, seg := range segments {
		p, err := pm.probe(ctx, pm.ffprobePath, seg.FilePath)
		if err != nil {
			pm.logger.Debug("part merge: probe failed, skipping merge", "err", err, "jobID", jobID, "file", seg.FilePath)
			return segments
		}
		params[i] = p
	}

	runs := groupMergeRuns(params)
	hasMergeable := false
	for _, run := range runs {
		if len(run) > 1 {
			hasMergeable = true
			break
		}
	}
	if !hasMergeable {
		return segments
	}

	var tempFiles []string
	abort := func() []database.Segment {
		for _, f := range tempFiles {
			os.Remove(f)
		}
		return segments
	}

	replacement := make([]database.Segment, 0, len(runs))
	var pending []mergeRun

	for runIdx, run := range runs {
		if len(run) == 1 {
			replacement = append(replacement, segments[run[0]])
			continue
		}

		runSegs := make([]database.Segment, len(run))
		inputs := make([]string, len(run))
		for i, idx := range run {
			runSegs[i] = segments[idx]
			inputs[i] = segments[idx].FilePath
		}

		first := runSegs[0]
		base := mergeBaseName(first.Filename)
		dir := filepath.Dir(first.FilePath)
		videoTemp := filepath.Join(dir, fmt.Sprintf("%s - merged%d.mp4", base, runIdx))

		if err := pm.concat(ctx, inputs, videoTemp); err != nil {
			pm.logger.Debug("part merge: concat failed, aborting merge", "err", err, "jobID", jobID, "run", runIdx)
			return abort()
		}
		tempFiles = append(tempFiles, videoTemp)

		info, statErr := os.Stat(videoTemp)
		if statErr != nil {
			pm.logger.Debug("part merge: stat of concat output failed, aborting merge", "err", statErr, "jobID", jobID, "run", runIdx)
			return abort()
		}
		size := info.Size()

		row := mergedSegmentRow(runSegs, videoTemp, size)

		var chatPaths []string
		for _, s := range runSegs {
			if s.ChatFile != "" {
				chatPaths = append(chatPaths, s.ChatFile)
			}
		}
		chatTemp := ""
		if len(chatPaths) > 0 {
			candidate := mergedChatPath(videoTemp)
			if err := mergeChatFiles(chatPaths, candidate); err != nil {
				pm.logger.Warn("part merge: chat merge failed, keeping media merge without chat", "err", err, "jobID", jobID, "run", runIdx)
				row.ChatFile = ""
			} else {
				chatTemp = candidate
				tempFiles = append(tempFiles, candidate)
				row.ChatFile = candidate
			}
		}

		replacement = append(replacement, row)
		pending = append(pending, mergeRun{
			indices:        run,
			videoTemp:      videoTemp,
			chatTemp:       chatTemp,
			bytes:          size,
			replacementIdx: len(replacement) - 1,
		})
	}

	if err := pm.replace(jobID, replacement); err != nil {
		pm.logger.Warn("part merge: db replace failed, aborting merge", "err", err, "jobID", jobID)
		return abort()
	}

	for _, pr := range pending {
		first := segments[pr.indices[0]]
		finalVideo := first.FilePath
		safeToCleanup := true

		if err := pm.rename(pr.videoTemp, finalVideo); err != nil {
			pm.logger.Warn("part merge: rename merged file to final name failed, leaving temp name", "err", err, "jobID", jobID, "from", pr.videoTemp, "to", finalVideo)
		} else {
			finalChat := ""
			if pr.chatTemp != "" {
				finalChat = mergedChatPath(finalVideo)
				if err := pm.rename(pr.chatTemp, finalChat); err != nil {
					pm.logger.Warn("part merge: rename merged chat to final name failed, leaving temp name", "err", err, "jobID", jobID, "from", pr.chatTemp, "to", finalChat)
					finalChat = pr.chatTemp
				}
			}

			merged := &replacement[pr.replacementIdx]
			if err := pm.updateFile(merged.ID, filepath.Base(finalVideo), finalVideo, finalChat); err != nil {
				pm.logger.Warn("part merge: failed to update segment row to final path; restoring merged file to its committed temp path so the row stays resolvable", "err", err, "jobID", jobID, "segmentID", merged.ID)
				if undoErr := pm.rename(finalVideo, pr.videoTemp); undoErr != nil {
					pm.logger.Warn("part merge: failed to restore merged file after a failed row update; the committed row may point at a missing file — leaving this run's superseded parts in place for manual recovery", "err", undoErr, "jobID", jobID, "segmentID", merged.ID, "wantPath", pr.videoTemp)
					safeToCleanup = false
				} else if finalChat != "" && finalChat != pr.chatTemp {
					if chatUndoErr := pm.rename(finalChat, pr.chatTemp); chatUndoErr != nil {
						pm.logger.Warn("part merge: failed to restore merged chat file after a failed row update", "err", chatUndoErr, "jobID", jobID, "segmentID", merged.ID)
					}
				}
			} else {
				merged.Filename = filepath.Base(finalVideo)
				merged.FilePath = finalVideo
				merged.ChatFile = finalChat
			}
		}

		if safeToCleanup {
			for _, idx := range pr.indices[1:] {
				seg := segments[idx]
				if seg.FilePath != "" {
					if err := os.Remove(seg.FilePath); err != nil && !os.IsNotExist(err) {
						pm.logger.Warn("part merge: failed to delete superseded part file", "err", err, "jobID", jobID, "file", seg.FilePath)
					}
				}
				// Only remove a superseded part's own chat file when THIS
				// run's chat actually merged (pr.chatTemp != "") — a failed
				// chat merge already reset the row's ChatFile to "" and must
				// leave every per-part chat file exactly where it was (see
				// mergeChatFiles' contract), or those messages have nowhere
				// left to live.
				if pr.chatTemp != "" && seg.ChatFile != "" {
					if err := os.Remove(seg.ChatFile); err != nil && !os.IsNotExist(err) {
						pm.logger.Warn("part merge: failed to delete superseded part chat file", "err", err, "jobID", jobID, "file", seg.ChatFile)
					}
				}
				// The superseded part's pre-mux staging dir, keyed by its own
				// SegmentIndex (not its position in this slice — merges can
				// leave index gaps). See merge's doc comment for why this is
				// required, not cosmetic.
				if stagingDir != "" {
					segDir := filepath.Join(stagingDir, fmt.Sprintf("seg_%d", seg.SegmentIndex))
					if err := os.RemoveAll(segDir); err != nil {
						pm.logger.Warn("part merge: failed to delete superseded part's staging dir", "err", err, "jobID", jobID, "dir", segDir)
					}
				}
			}
		} else {
			pm.logger.Warn("part merge: skipping superseded-part cleanup for this run after a row-update failure",
				"jobID", jobID, "supersededParts", len(pr.indices)-1)
		}

		pm.logger.Info(fmt.Sprintf("merged %d same-format parts", len(pr.indices)),
			"jobID", jobID, "parts", len(pr.indices), "bytes", pr.bytes)
	}

	return replacement
}

// groupMergeRuns partitions params into contiguous runs of index positions
// sharing merge-compatible stream params (streamParams.equal). A nil entry
// — a probe failure marker, though mergeSameFormatParts already aborts the
// whole merge on any real probe error before reaching this — never equals
// anything (equal() is nil-safe both ways) and is always its own run of
// length 1, merging with neither its predecessor nor successor.
func groupMergeRuns(params []*streamParams) [][]int {
	var runs [][]int
	for i := range params {
		if i > 0 && params[i-1].equal(params[i]) {
			runs[len(runs)-1] = append(runs[len(runs)-1], i)
			continue
		}
		runs = append(runs, []int{i})
	}
	return runs
}

// mergedSegmentRow builds the replacement row for a contiguous run of parts
// already concat-copied to outPath (size bytes). Quality/dimensions and the
// job/segment identity fields come from the run's first part (struct copy);
// UnixEnd and DurationSeconds are recomputed across the whole run. ChatFile
// is set to outPath's chat sibling (mergedChatPath) whenever ANY part in the
// run carried a chat file, or "" when none did — the caller (merge) may
// still clear it back to "" afterward if the actual chat-merge I/O fails,
// without needing to rebuild the row.
func mergedSegmentRow(run []database.Segment, outPath string, size int64) database.Segment {
	first := run[0]
	last := run[len(run)-1]

	merged := first
	merged.FilePath = outPath
	merged.Filename = filepath.Base(outPath)
	merged.UnixEnd = last.UnixEnd

	var dur float64
	hasChat := false
	for _, s := range run {
		dur += s.DurationSeconds
		if s.ChatFile != "" {
			hasChat = true
		}
	}
	merged.DurationSeconds = dur

	fileSize := size
	merged.FileSize = &fileSize

	if hasChat {
		merged.ChatFile = mergedChatPath(outPath)
	} else {
		merged.ChatFile = ""
	}

	return merged
}

// mergeBaseName recovers the shared "<base>" from a part filename ("<base>
// - partN.mp4"), matching muxSegment/finalizeMultiSegmentJob's naming
// scheme (see partBaseRe). Falls back to the filename minus its extension
// when the pattern doesn't match (defensive — every part filename this
// merge sees was produced by muxSegment).
func mergeBaseName(filename string) string {
	if m := partBaseRe.FindStringSubmatch(filename); m != nil {
		return m[1]
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename))
}

// mergedChatPath derives a merged media file's chat sibling path, matching
// muxSegment's convention (partBase + ".chat.json").
func mergedChatPath(mediaPath string) string {
	return strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath)) + ".chat.json"
}

// mergeChatFiles reads each path as a chat.ChatData JSON file and writes a
// single merged chat.ChatData JSON to outPath: Messages are concatenated in
// file order, MessageCount is summed, and the first file's identity fields
// (VideoID, VideoTitle, ChannelName, StreamStartTime) are kept. DownloadedAt
// is refreshed to the merge time. A missing or corrupt input file aborts
// with an error and writes nothing — the caller falls back to no chat merge
// (media merge still proceeds, ChatFile "" on the row, and the per-part
// chat files are left on disk untouched).
func mergeChatFiles(paths []string, outPath string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no chat files to merge")
	}

	var merged chat.ChatData
	for i, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		var data chat.ChatData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		if i == 0 {
			merged.VideoID = data.VideoID
			merged.VideoTitle = data.VideoTitle
			merged.ChannelName = data.ChannelName
			merged.StreamStartTime = data.StreamStartTime
		}
		merged.Messages = append(merged.Messages, data.Messages...)
		merged.MessageCount += data.MessageCount
	}
	merged.DownloadedAt = time.Now().UTC().Format(time.RFC3339)

	return utils.WriteChatFileAtomic(outPath, &merged)
}

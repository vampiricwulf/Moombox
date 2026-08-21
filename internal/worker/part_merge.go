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
// (ffprobe, ffmpeg concat, DB replace/update) as function fields, defaulting
// to the real implementations in mergeSameFormatParts. Tests inject stubs so
// the grouping/row-building/fallback logic runs without a live ffmpeg
// toolchain — see part_merge_test.go.
type partMerger struct {
	ffprobePath string
	logger      logger

	probe      func(ctx context.Context, ffprobePath, filePath string) (*streamParams, error)
	concat     func(ctx context.Context, inputs []string, outputPath string) error
	replace    func(jobID string, segs []database.Segment) error
	updateFile func(id int, filename, filePath, chatFile string) error
}

// mergeSameFormatParts opportunistically concat-copies contiguous runs of
// parts whose stream parameters are identical (Tier 4 of the interruption
// spec). Decisions come from ffprobe on the actual files — the Segment
// rows' quality metadata lacks audio parameters — and any probe or concat
// failure returns the input untouched: the merge is an improvement, never
// a gate on finalize. Merged media is written next to the parts as
// "<base> - merged<runIndex>.mp4", then renamed over the run's first part
// name after row replacement succeeds; obsolete later-part files and their
// chat siblings are removed only after the DB replace commits.
func (o *DownloadOrchestrator) mergeSameFormatParts(ctx context.Context, jobCtx *JobContext, segments []database.Segment) []database.Segment {
	pm := &partMerger{
		ffprobePath: o.muxer.FFprobePath(),
		logger:      o.logger,
		probe:       probeStreamParams,
		concat:      o.muxer.ConcatCopy,
		replace:     o.db.ReplaceJobSegments,
		updateFile:  o.db.UpdateSegmentFile,
	}
	return pm.merge(ctx, jobCtx.Job.ID, segments)
}

// mergeRun tracks one contiguous multi-part run through the merge/commit/
// cleanup pipeline: the concat-copy output (and, when chat merged, its
// sibling) start under temp names next to the parts, get folded into the
// single db.ReplaceJobSegments call alongside every other row, and only
// after that commit succeeds are they best-effort renamed onto the run's
// first part's original name and the superseded later parts removed.
type mergeRun struct {
	indices        []int // indices into the ORIGINAL segments slice
	videoTemp      string
	chatTemp       string // "" when no chat was merged for this run
	bytes          int64
	replacementIdx int // index into the replacement slice built by merge()
}

// merge is the injectable core of mergeSameFormatParts. See that method's
// doc comment for the overall algorithm and crash-consistency notes.
//
// Crash-consistency choice: every part-merge row is committed to the DB
// BEFORE any original filename is touched. The merged row initially points
// at the (already-written, already-valid) temp concat output — never at a
// name that doesn't exist yet — so a crash or error at any point before the
// single db.ReplaceJobSegments call leaves the original parts completely
// untouched (the true "return input unchanged" contract). Once that call
// commits, the rename-onto-the-first-part-name + delete-superseded-files
// steps are best-effort polish: a crash mid-rename either leaves the
// committed row pointing at the (still valid) temp file, or — once the
// rename lands but before the follow-up updateFile call — leaves the row
// pointing at a name that no longer exists while the correct merged bytes
// sit safely under the final name (recoverable, not data loss). The
// alternative (rename the temp file onto the first part's original name
// BEFORE committing the DB row) was rejected: it would silently overwrite
// an original part's file the moment any LATER run in the same batch failed
// to concat, breaking the "input unchanged on failure" guarantee for the
// earlier, already-succeeded runs.
func (pm *partMerger) merge(ctx context.Context, jobID string, segments []database.Segment) []database.Segment {
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
		if err := os.Rename(pr.videoTemp, finalVideo); err != nil {
			pm.logger.Warn("part merge: rename merged file to final name failed, leaving temp name", "err", err, "jobID", jobID, "from", pr.videoTemp, "to", finalVideo)
		} else {
			finalChat := ""
			if pr.chatTemp != "" {
				finalChat = mergedChatPath(finalVideo)
				if err := os.Rename(pr.chatTemp, finalChat); err != nil {
					pm.logger.Warn("part merge: rename merged chat to final name failed, leaving temp name", "err", err, "jobID", jobID, "from", pr.chatTemp, "to", finalChat)
					finalChat = pr.chatTemp
				}
			}

			merged := &replacement[pr.replacementIdx]
			if err := pm.updateFile(merged.ID, filepath.Base(finalVideo), finalVideo, finalChat); err != nil {
				pm.logger.Warn("part merge: failed to update segment row to final path", "err", err, "jobID", jobID, "segmentID", merged.ID)
			} else {
				merged.Filename = filepath.Base(finalVideo)
				merged.FilePath = finalVideo
				merged.ChatFile = finalChat
			}
		}

		for _, idx := range pr.indices[1:] {
			seg := segments[idx]
			if seg.FilePath != "" {
				if err := os.Remove(seg.FilePath); err != nil && !os.IsNotExist(err) {
					pm.logger.Warn("part merge: failed to delete superseded part file", "err", err, "jobID", jobID, "file", seg.FilePath)
				}
			}
			if seg.ChatFile != "" {
				if err := os.Remove(seg.ChatFile); err != nil && !os.IsNotExist(err) {
					pm.logger.Warn("part merge: failed to delete superseded part chat file", "err", err, "jobID", jobID, "file", seg.ChatFile)
				}
			}
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

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// rows' quality metadata lacks audio parameters — and any probe, concat, or
// db-replace failure returns the input untouched: the merge is an
// improvement, never a gate on finalize. A per-run chat-merge failure is a
// narrower, run-scoped version of the same identity guarantee: THAT run's
// media is not merged either, so its parts/rows/chat files all stay exactly
// as they were, while any OTHER run in the same finalize still merges
// independently (see merge's doc comment). Merged media is written next to
// the parts as "<base> - merged<runIndex>.mp4", committed to the DB at that
// temp path, then best-effort renamed onto the run's first part's name once
// that commit succeeds. Only once the row is confirmed to resolve to a real
// file does cleanup remove the now-superseded later parts: their output
// files, their chat files, and their pre-mux seg_N staging directories
// (removing those is what stops a later finalize re-entry from
// resurrecting and re-merging already-consumed content — see merge's doc
// comment for the full chain).
func (o *DownloadOrchestrator) mergeSameFormatParts(ctx context.Context, jobCtx *JobContext, segments []database.Segment) []database.Segment {
	pm := &partMerger{
		ffprobePath: o.muxer.FFprobePath(),
		logger:      o.logger,
		probe:       probeStreamParamsFn,
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
// A run only ever becomes a mergeRun (i.e. reaches `pending`) once its
// video AND (if any part carried one) chat merge have both already
// succeeded — a chat-merge failure aborts the run before this struct is
// ever built (see merge's doc comment). So chatTemp here is simply "" when
// no part in the run had chat, and the merged chat's path whenever one did;
// it can no longer mean "chat merge failed" the way it once did.
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
// Chat-merge failure aborts the RUN, not the batch: if any part in a run
// carries a ChatFile and mergeChatFiles fails on it (the only production
// case today is a Twitch gap-split run — its chat is twitch.TwitchChatData,
// whose "message" field is a string, while mergeChatFiles unmarshals
// chat.ChatData, whose "message" is []MessagePart, so the unmarshal always
// errors), that run's video is discarded too (its temp concat output is
// removed) and its ORIGINAL per-part segments are pushed back into the
// replacement slice unchanged — identical to a len(run)==1 passthrough.
// Nothing about that run's parts, rows, or chat files is ever touched, and
// the run never reaches `pending` so post-commit cleanup never runs for it
// either. Other runs in the same finalize call are unaffected and still
// merge normally. If EVERY run in a batch aborts this way, `pending` ends
// up empty and merge returns `segments` untouched without ever calling
// db.ReplaceJobSegments at all — true whole-call identity, not just
// per-row.
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
	// Crash re-entry repair pass (I7 fix) — runs BEFORE the len<2
	// short-circuit below and regardless of segment count, because a job
	// that already fully collapsed to ONE row on a prior run carries
	// exactly the same crash signature and would otherwise never reach a
	// probe at all here (len<2 returns first) nor get recognized by
	// finalizeMultiSegmentJob's renameSinglePartToPlain (which looks for
	// the PLAIN job name, a different final name than a merge run-head's).
	// mergeTempRe is checked before ever touching disk, so a normal,
	// never-crashed segment (the overwhelming common case) costs nothing
	// extra here. See adoptMergedFinal's doc comment for the full repair.
	for i := range segments {
		if mergeTempRe.MatchString(segments[i].Filename) && !fileExists(segments[i].FilePath) {
			pm.adoptMergedFinal(ctx, &segments[i])
		}
	}

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
				// C1 ruling: chat-merge failure aborts THIS RUN's identity
				// entirely, not just its chat -- media is not merged
				// either, so the parts, their rows, and every per-part
				// chat file all stay exactly as they were. This is the
				// only way a Twitch gap-split run (the sole production
				// case with per-part ChatFile) ever reaches here today,
				// since its chat is twitch.TwitchChatData ("message" a
				// string) and mergeChatFiles unmarshals chat.ChatData
				// ("message" a []MessagePart) -- the schema mismatch
				// always errors. Other runs in this same finalize proceed
				// independently; see merge's doc comment.
				pm.logger.Warn("part merge: chat merge failed, aborting this run's merge -- media left split, every per-part chat file untouched", "err", err, "jobID", jobID, "run", runIdx)
				os.Remove(videoTemp) // this run never reaches pending, so nothing else will clean up its orphaned temp output
				for _, idx := range run {
					replacement = append(replacement, segments[idx])
				}
				continue
			}
			chatTemp = candidate
			tempFiles = append(tempFiles, candidate)
		}

		row := mergedSegmentRow(runSegs, videoTemp, size)

		replacement = append(replacement, row)
		pending = append(pending, mergeRun{
			indices:        run,
			videoTemp:      videoTemp,
			chatTemp:       chatTemp,
			bytes:          size,
			replacementIdx: len(replacement) - 1,
		})
	}

	if len(pending) == 0 {
		// Every mergeable run aborted (chat-merge failure) or none existed
		// beyond the len(run)==1 passthroughs already folded into
		// replacement unchanged -- there is nothing to commit. Return the
		// ORIGINAL segments slice directly rather than the reconstructed
		// (but content-identical) replacement: true whole-call identity,
		// with no db.ReplaceJobSegments call and no row restamping at all.
		return segments
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
				// run's chat actually merged (pr.chatTemp != ""). A run
				// whose chat merge FAILED never reaches this loop at all
				// any more (C1: the whole run aborts before pending is
				// built — see merge's doc comment), so in practice
				// pr.chatTemp == "" here only when no part in the run ever
				// had a chat file, in which case seg.ChatFile is already
				// "" too and this check is defensive belt-and-suspenders,
				// not load-bearing against a cleared-but-unmerged ChatFile
				// the way it once was.
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
					// Tombstone BEFORE attempting removal (I7 fix): a crash
					// mid-RemoveAll, or a locked-dir failure below, must
					// still leave a durable marker behind so
					// stagedSegDirs' three consumers never resurrect this
					// dir's already-merged content. Best-effort — if the
					// dir is already gone (or write fails for some other
					// reason) there's nothing left to resurrect anyway, so
					// this doesn't block the removal attempt that follows.
					if err := os.WriteFile(filepath.Join(segDir, mergeTombstoneFile), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil && !os.IsNotExist(err) {
						pm.logger.Warn("part merge: failed to write tombstone marker before removing superseded part's staging dir", "err", err, "jobID", jobID, "dir", segDir)
					}
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
// run carried a chat file, or "" when none did. Callers only reach this
// once any chat merge the run needed has already succeeded (a chat-merge
// failure aborts the whole run before mergedSegmentRow is ever called for
// it — see merge's doc comment), so the ChatFile this computes is always
// the row's final value, never provisional.
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

// mergeTempRe matches a Tier 4 merge's temp output filename ("<base> -
// mergedN.mp4") — the shape videoTemp carries from the moment merge()
// commits its row (db.ReplaceJobSegments) until the post-commit rename
// lands it on the run's first part's original name (merge's doc comment,
// "Crash-consistency choice"). Used by adoptMergedFinal to recognize a
// crashed re-entry.
var mergeTempRe = regexp.MustCompile(`^(.+) - merged\d+\.mp4$`)

// adoptMergedFinal attempts the I7 crash re-entry repair for a segment row
// whose probe just failed because its recorded FilePath doesn't exist:
// if the row's Filename LOOKS like a Tier 4 merge temp AND the run-head's
// final name — the run's first part's ORIGINAL filename, reconstructed as
// "<base> - part<SegmentIndex+1>.mp4" (mergedSegmentRow keeps
// SegmentIndex as the run's first part's — see its doc comment; muxSegment
// numbers parts as segIdx+1 — see partBase in orchestrator_mux.go) — exists
// on disk, this is exactly the signature merge's own doc comment warns
// about: a crash between the successful rename(videoTemp -> finalVideo)
// and the follow-up UpdateSegmentFile call, leaving the committed row
// naming a path that no longer exists while the real, already-renamed file
// sits unreferenced (and orphan-scanner-deletable). Mirrors
// renameSinglePartToPlain's adopt-and-repair for the single-part case
// (same crash window, different finalize path).
//
// On success, repairs the DB row AND seg itself (the caller's segments
// slice entry, via pointer) to the adopted final video/chat names — the
// caller's own subsequent probe pass then verifies the adopted file for
// real, rather than this function's own probe check (used only as an
// adopt/don't-adopt gate) being trusted as the final answer twice. On any
// failure to adopt (pattern mismatch, final name absent, adopted file
// doesn't probe cleanly, or the row update fails), returns false and
// leaves seg completely untouched, so the caller's normal probe-failure
// abort proceeds exactly as it would have without this repair attempt.
func (pm *partMerger) adoptMergedFinal(ctx context.Context, seg *database.Segment) bool {
	m := mergeTempRe.FindStringSubmatch(seg.Filename)
	if m == nil {
		return false
	}
	base := m[1]
	dir := filepath.Dir(seg.FilePath)
	finalVideo := filepath.Join(dir, fmt.Sprintf("%s - part%d.mp4", base, seg.SegmentIndex+1))
	if !fileExists(finalVideo) {
		return false
	}

	// A missing chat sibling doesn't block adopting the video — the row's
	// ChatFile just stays whatever it was (already-vanished temp path);
	// the video recovery is the load-bearing half here.
	finalChat := ""
	if seg.ChatFile != "" {
		if candidate := mergedChatPath(finalVideo); fileExists(candidate) {
			finalChat = candidate
		}
	}

	// Verify the candidate is actually a playable file before repairing
	// the row onto it — an adopt-and-repair that pointed the row at
	// something unverified would be worse than leaving the crash for
	// manual recovery.
	if _, err := pm.probe(ctx, pm.ffprobePath, finalVideo); err != nil {
		pm.logger.Debug("part merge: adopt-and-repair candidate found but does not probe cleanly, not adopting", "err", err, "jobID", seg.JobID, "segmentID", seg.ID, "candidate", finalVideo)
		return false
	}

	if err := pm.updateFile(seg.ID, filepath.Base(finalVideo), finalVideo, finalChat); err != nil {
		pm.logger.Warn("part merge: adopt-and-repair found the run-head's final file but failed to update the row", "err", err, "jobID", seg.JobID, "segmentID", seg.ID, "adopted", finalVideo)
		return false
	}

	pm.logger.Info("part merge: adopted crash-orphaned merge output onto its run-head final name", "jobID", seg.JobID, "segmentID", seg.ID, "from", seg.FilePath, "to", finalVideo)

	seg.FilePath = finalVideo
	seg.Filename = filepath.Base(finalVideo)
	// Unconditional, matching exactly what was just persisted above —
	// including clearing to "" when the chat sibling couldn't be found, so
	// seg never disagrees with the row about a chat reference that no
	// longer resolves.
	seg.ChatFile = finalChat
	return true
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
// with an error and writes nothing. Every part this package sees carrying a
// ChatFile in production is a Twitch part (twitch.TwitchChatData, whose
// "message" field is a string), while this unmarshals into chat.ChatData
// (whose "message" is []MessagePart) — so that case ALWAYS errors here.
// merge's caller-side response to an error is to abort the WHOLE run this
// chat merge belongs to (media included), not just fall back to a
// chat-less media merge — see merge's doc comment.
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

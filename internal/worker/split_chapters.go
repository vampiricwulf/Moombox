package worker

import (
	"context"
	"os"
	"path/filepath"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// ArchiveSidecarPath is the file beside the archive outputFile names that
// carries suffix after the archive's base name: a part's " - partN" comes off
// first, so a split job's chat archive, raw chat sidecar and chapter list
// ("X.chat.json", "X.chat.raw.jsonl", "X.chapters.json") sit under the base
// its parts share ("X - part1.mp4"), as copyAssets writes them. "" when
// outputFile is empty. The worker's finalize and GET /api/jobs/{id}/chapters
// both derive the split archive's chapter list this way (owner decisions
// FE-15, FE-16): it is named on no column.
func ArchiveSidecarPath(outputFile, suffix string) string {
	dir, base, ok := recordedArchiveLocation(outputFile)
	if !ok {
		return ""
	}
	return filepath.Join(dir, base+suffix)
}

// writeSplitArchiveChapters gives a split archive its chapter list (owner
// decision FE-15). A single-file archive embeds its chapters at the mux
// (prepareArchiveChapters); a split one's parts were muxed one by one as they
// finished, with none — the list is for the whole broadcast and does not
// exist yet when part 1 is muxed — and a recorded part is never re-muxed to
// add one. So the finalize writes the list W22 builds for a single file
// (archiveChapters: the same sources, the same fitting) beside the parts as
// <name>.chapters.json, moved onto the player's global timeline
// (splitArchiveChapters), where GET /api/jobs/{id}/chapters serves it when
// the parts carry no chapters of their own. Best effort, like the embed: a
// failure costs the player a chapter list and nothing else. A finalize that
// finds no list removes one an earlier finalize wrote.
func (o *DownloadOrchestrator) writeSplitArchiveChapters(ctx context.Context, jobCtx *JobContext, segments []database.Segment) {
	if len(segments) == 0 || jobCtx.Job == nil {
		return
	}
	path := ArchiveSidecarPath(segments[0].FilePath, utils.ChaptersSidecarSuffix)
	if path == "" {
		return
	}
	live := !jobCtx.Job.IsVod
	list := o.archiveChapters(ctx, jobCtx, func() int64 { return splitTimelineSpanMs(segments, live) })
	chapters := splitArchiveChapters(list, segments, live)
	if err := utils.WriteChaptersSidecar(path, chapters); err != nil {
		o.logger.Warn("could not write the split archive's chapter list beside its parts", "err", err, "jobID", jobCtx.Job.ID, "file", path)
		return
	}
	if len(chapters) > 0 {
		o.logger.Info("wrote the split archive's chapter list beside its parts", "jobID", jobCtx.Job.ID, "chapters", len(chapters), "file", filepath.Base(path))
	}
}

// splitTimelineSpanMs is how long the timeline a split archive's chapter
// list is read in runs, to fit the list to (utils.SanitizeChapters): for a
// live capture the wall-clock span from part 1's start to the end of the part
// that ends last, gaps between parts included, since its chapters were timed
// from the capture's start (liveTwitchChapters); for a VOD the parts' lengths
// laid end to end, the video's own timeline.
func splitTimelineSpanMs(segments []database.Segment, live bool) int64 {
	var total, end int64
	origin := segments[0].UnixStart * 1000
	for _, s := range segments {
		d := int64(s.DurationSeconds * 1000)
		total += d
		if s.UnixStart > 0 {
			end = max(end, s.UnixStart*1000+d)
		}
	}
	if live && origin > 0 && end > origin {
		return end - origin
	}
	return total
}

// splitArchiveChapters moves chapters (milliseconds of the capture's own
// timeline, already fitted) onto the player's global timeline: the parts laid
// end to end in segment order, each from the sum of the earlier parts'
// durationSeconds — SegmentPlayer.init's layout, which the chapters route
// shifts each part's own chapters by too.
//
// A VOD's timeline is the video's, which its parts cover end to end: the
// list is the global timeline as it stands. A live capture's is the wall
// clock from part 1's start, and the parts leave out whatever fell between
// them (an outage, a gap split), so each point is placed by the part whose
// span holds it: a point inside a part lands at that part's offset plus its
// distance into the part; one in a gap at the start of the part after it;
// one past the last part at the global end. A chapter that collapses to
// nothing — it lay wholly in a gap — is dropped; the title of the one
// running into the next part carries on there. A part with no start time or
// no duration holds no span and maps nothing.
func splitArchiveChapters(chapters []utils.Chapter, segments []database.Segment, live bool) []utils.Chapter {
	if len(chapters) == 0 || len(segments) == 0 {
		return nil
	}
	if !live || segments[0].UnixStart <= 0 {
		return chapters
	}
	origin := segments[0].UnixStart * 1000
	type span struct{ from, to, offset int64 }
	var spans []span
	var offset int64
	for _, s := range segments {
		d := int64(s.DurationSeconds * 1000)
		if s.UnixStart > 0 && d > 0 {
			from := s.UnixStart*1000 - origin
			spans = append(spans, span{from: from, to: from + d, offset: offset})
		}
		offset += d
	}
	place := func(t int64) int64 {
		for _, sp := range spans {
			switch {
			case t < sp.from:
				return sp.offset // in the gap before this part
			case t < sp.to:
				return sp.offset + t - sp.from
			}
		}
		return offset
	}
	var out []utils.Chapter
	for _, c := range chapters {
		start, end := place(c.StartMs), place(c.EndMs)
		if end <= start {
			continue
		}
		out = append(out, utils.Chapter{StartMs: start, EndMs: end, Title: c.Title})
	}
	return out
}

// removeSplitArchiveChapters removes the chapter list beside a split job's
// parts, when the parts stop being the archive: a Reinitialize that deletes
// them (clearJobParts), a complete VOD download that supersedes them
// (supersedePartsWithVod). Best effort; a list left behind is harmless.
func removeSplitArchiveChapters(segments []database.Segment, lg logger) {
	if len(segments) == 0 {
		return
	}
	path := ArchiveSidecarPath(segments[0].FilePath, utils.ChaptersSidecarSuffix)
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		lg.Warn("could not remove the parts' chapter list", "path", path, "err", err)
	}
}

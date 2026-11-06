package worker

import (
	"context"
	"math"
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
// FE-15, FE-16): it is named on no column. A chapter import's list beside a
// single-file archive (owner decision CI-1) is found the same way.
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
//
// A re-finalize of the same recording (jobCtx.Refinalize: MuxJob's, owner
// decision CJ-2, and a Resume's or a retry's, CK-1) that finds an import's
// list keeps it instead: the imported list and its original, refitted to the
// parts' timeline as it now stands — their durations laid end to end, the
// timeline the import was fitted to (chapterTargetFor) — and not replaced by
// the automatic list. The list is read only from beside the archive the row
// names (its output_file, part 1 of a split archive a finalize already
// recorded), as the single-file finalize reads it: a resumed single file
// that now splits into parts under a name the template resolves afresh
// keeps its import, moved beside the parts, and a list found beside parts
// at a name the row never held is another recording's, written over. A row
// that names no archive has recorded parts but never finalized them (the
// Mux action over a split capture that failed, a Resume of one): its parts
// are what an import was saved beside, so the list is read there.
func (o *DownloadOrchestrator) writeSplitArchiveChapters(ctx context.Context, jobCtx *JobContext, segments []database.Segment) {
	if len(segments) == 0 || jobCtx.Job == nil {
		return
	}
	path := ArchiveSidecarPath(segments[0].FilePath, utils.ChaptersSidecarSuffix)
	if path == "" {
		return
	}
	from := path
	if jobCtx.Job.OutputFile != "" {
		from = ArchiveSidecarPath(jobCtx.Job.OutputFile, utils.ChaptersSidecarSuffix)
	}
	if kept := keptImportFor(jobCtx, o.logger, from); kept != nil {
		var totalMs int64
		for _, s := range segments {
			totalMs += int64(math.Round(s.DurationSeconds * 1000))
		}
		kept.refit(totalMs)
		kept.write(path, o.logger)
		o.logger.Info("kept the split archive's imported chapter list", "jobID", jobCtx.Job.ID, "chapters", len(kept.chapters), "file", filepath.Base(path))
		return
	}
	live := !jobCtx.Job.IsVod
	list := o.archiveChapters(ctx, jobCtx, false, func() int64 { return splitTimelineSpanMs(segments, live) })
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

// refitSplitArchiveChapters keeps a split archive's chapter list true after a
// gap-fill re-finalize (owner decision D17) inserted recovered parts between,
// before or after the recorded ones. The list was laid on the global timeline
// of the parts as they stood (splitArchiveChapters), and the player lays the
// new parts end to end, so every point past an inserted part now reads early
// by that part's length. The list's wall-clock source is gone by now — the
// staging it was built from is reclaimed once the job finished — and the rows'
// unix_start is the session's clock, not the footage's (twitchGapFiller.commit
// keys the parts by the plan's coverage for that reason), so the list is not
// rebuilt: each point is shifted by the length of every recovered part
// inserted at or before it in the old timeline. A point that sat where a
// recovered part goes — a chapter that began in the lost span, which
// splitArchiveChapters placed at the start of the part after it — moves past
// the recovered part with the part it began in, so the chapter running into
// the gap covers it; the list's start (0) stays the start, so the first
// chapter covers a recovered opening. The list is written where the chapters
// route now looks (the new first part's base: a recovered opening becomes
// part 1) and one left under the old base is removed. No list, nothing to do;
// best effort, like the finalize's write.
//
// A chapter import's list (owner decision CJ-2) is the same recording's
// too — the fill adds footage, it does not re-record — so it is kept, not
// replaced or dropped: its points shift as an automatic list's do, and so do
// those of the original it keeps, which lies on the same timeline and which
// Restore must still put back where it was; both are written back with the
// original, so Restore is still offered.
//
// oldSegs are the parts in the order the list was laid on (a single-file
// archive its one synthesized part), newSegs the committed ones, recovered
// the paths of the parts the fill made.
func refitSplitArchiveChapters(oldSegs, newSegs []database.Segment, recovered map[string]bool, lg logger) {
	if len(oldSegs) == 0 || len(newSegs) == 0 {
		return
	}
	oldPath := ArchiveSidecarPath(oldSegs[0].FilePath, utils.ChaptersSidecarSuffix)
	newPath := ArchiveSidecarPath(newSegs[0].FilePath, utils.ChaptersSidecarSuffix)
	if oldPath == "" || newPath == "" {
		return
	}
	imp, imported, err := utils.ReadImportedChaptersSidecar(oldPath)
	if err != nil {
		lg.Warn("gap-fill: the split archive's chapter list is unreadable; it is left as it was", "path", oldPath, "err", err)
		return
	}
	var list, original []utils.SidecarChapter
	if imported {
		list, original = imp.Chapters, imp.Original
	} else if list, err = utils.ReadChaptersSidecar(oldPath); err != nil {
		lg.Warn("gap-fill: the split archive's chapter list is unreadable; it is left as it was", "path", oldPath, "err", err)
		return
	}
	if len(list) == 0 && len(original) == 0 {
		return
	}
	type insert struct{ at, dur int64 }
	var inserts []insert
	var oldOffset int64
	for _, s := range newSegs {
		d := int64(s.DurationSeconds * 1000)
		if recovered[s.FilePath] {
			inserts = append(inserts, insert{at: oldOffset, dur: d})
			continue
		}
		oldOffset += d
	}
	shift := func(p int64) int64 {
		out := p
		for _, in := range inserts {
			if p > 0 && in.at <= p {
				out += in.dur
			}
		}
		return out
	}
	shiftAll := func(in []utils.SidecarChapter) []utils.Chapter {
		out := make([]utils.Chapter, 0, len(in))
		for _, c := range utils.ChaptersFromSidecar(in) {
			out = append(out, utils.Chapter{StartMs: shift(c.StartMs), EndMs: shift(c.EndMs), Title: c.Title})
		}
		return out
	}
	chapters := shiftAll(list)
	if imported {
		err = utils.WriteImportedChaptersSidecar(newPath, chapters, utils.SidecarFromChapters(shiftAll(original)))
	} else {
		err = utils.WriteChaptersSidecar(newPath, chapters)
	}
	if err != nil {
		lg.Warn("gap-fill: could not rewrite the split archive's chapter list for its recovered parts", "path", newPath, "err", err)
		return
	}
	if oldPath != newPath {
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			lg.Warn("gap-fill: could not remove the chapter list under the archive's old base", "path", oldPath, "err", err)
		}
	}
}

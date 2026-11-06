package utils

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// ChaptersSidecarSuffix is the chapter list a split archive keeps beside its
// parts, "<name>.chapters.json" (owner decision FE-15): a single-file archive
// carries its chapters inside the MP4, but a split one's parts are muxed one
// by one as they finish, before any list for the whole broadcast exists, and
// a recorded part is never re-muxed to add one. <name> is the base the parts
// share — "X" for "X - part1.mp4" — as for the job's chat archive and its raw
// sidecar, and like them it is named on no column: it is found from the
// archive's own name (GET /api/jobs/{id}/chapters, the orphan sweep). A
// chapter import (owner decision CI-1) writes one beside a single-file archive
// too, marked by its "original" (WriteImportedChaptersSidecar): GET answers it
// ahead of the file's embedded chapters, and a finalize that writes a new
// archive at that name removes it. Not the staging dir's StagedChaptersFile,
// which is the capture's input to the mux.
const ChaptersSidecarSuffix = ".chapters.json"

// SidecarChapter is one chapter of a ChaptersSidecarSuffix file: a span on
// the player's global timeline (the parts laid end to end, in segment order),
// in seconds — GET /api/jobs/{id}/chapters' own shape, so the route serves
// the file as it reads it.
type SidecarChapter struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
}

// chaptersSidecar is the file's top level: an object, so a later field does
// not change how an older reader reads the list.
//
// Original is the chapter import's (owner decision CI-3): the chapters the
// archive had before its first import — embedded in the file or listed here
// by a split archive's finalize, possibly none — kept so "Restore original"
// can bring them back. Its presence (an empty list included) is what marks
// the file as an import's, whose Chapters are the archive's chapters
// whatever the file itself carries (GET /api/jobs/{id}/chapters reads them
// first). A finalize's own list (WriteChaptersSidecar) has none.
type chaptersSidecar struct {
	Chapters []SidecarChapter  `json:"chapters"`
	Original *[]SidecarChapter `json:"original,omitempty"`
}

// ImportedChapters is a ChaptersSidecarSuffix file an import wrote: the
// imported list and the archive's original one (owner decision CI-3), both
// on the player's global timeline.
type ImportedChapters struct {
	Chapters []SidecarChapter
	Original []SidecarChapter
}

// WriteImportedChaptersSidecar writes an import's file to path atomically:
// chapters (milliseconds, on the global timeline, already fitted) as the
// list, and original as the chapters to restore. Unlike WriteChaptersSidecar
// an empty list is written, not removed: an import that restores an archive
// with no chapters must still say so over the ones in the file.
func WriteImportedChaptersSidecar(path string, chapters []Chapter, original []SidecarChapter) error {
	out := chaptersSidecar{Chapters: make([]SidecarChapter, 0, len(chapters))}
	for _, c := range chapters {
		out.Chapters = append(out.Chapters, SidecarChapter{
			Start: float64(c.StartMs) / 1000, End: float64(c.EndMs) / 1000, Title: c.Title,
		})
	}
	orig := make([]SidecarChapter, 0, len(original))
	orig = append(orig, original...)
	out.Original = &orig
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// ChaptersFromSidecar is a sidecar list in milliseconds, the shape the
// fitting and the muxer work in.
func ChaptersFromSidecar(list []SidecarChapter) []Chapter {
	out := make([]Chapter, 0, len(list))
	for _, c := range list {
		out = append(out, Chapter{StartMs: int64(math.Round(c.Start * 1000)), EndMs: int64(math.Round(c.End * 1000)), Title: c.Title})
	}
	return out
}

// SidecarFromChapters is ChaptersFromSidecar's inverse, in seconds.
func SidecarFromChapters(list []Chapter) []SidecarChapter {
	out := make([]SidecarChapter, 0, len(list))
	for _, c := range list {
		out = append(out, SidecarChapter{Start: float64(c.StartMs) / 1000, End: float64(c.EndMs) / 1000, Title: c.Title})
	}
	return out
}

// RefitImportedChapters fits a list an import saved (or the original it
// kept) to a re-finalized archive of the same recording whose timeline is
// now durationMs long (owner decision CJ-2): a chapter that starts at or
// past the new end is dropped, one that ran past it is cut there, and the
// last one runs to the new end — the list was fitted to the archive it was
// imported into, whose last chapter ended at that archive's end, and a
// re-mux that carried more of the recording (the Mux action over staging
// the import never saw, a part recorded after it) extends that chapter
// rather than leave the new footage outside every chapter. Starts and
// titles are kept as they are: the timeline is the same recording's, so a
// point in it means what it meant. An unknown length (0) leaves the list
// as it is; nil when nothing is left.
func RefitImportedChapters(list []Chapter, durationMs int64) []Chapter {
	if durationMs <= 0 {
		return list
	}
	var out []Chapter
	for _, c := range list {
		if c.StartMs >= durationMs {
			continue
		}
		if c.EndMs <= 0 {
			c.EndMs = durationMs
		}
		c.EndMs = min(c.EndMs, durationMs)
		if c.EndMs <= c.StartMs {
			continue
		}
		out = append(out, c)
	}
	if n := len(out); n > 0 {
		out[n-1].EndMs = durationMs
	}
	return out
}

// ReadImportedChaptersSidecar reads a ChaptersSidecarSuffix file and reports
// whether an import wrote it (it carries an original list). A missing file,
// or a finalize's, is not one: nil and false, with no error.
func ReadImportedChaptersSidecar(path string) (*ImportedChapters, bool, error) {
	in, err := readChaptersSidecar(path)
	if err != nil || in == nil || in.Original == nil {
		return nil, false, err
	}
	return &ImportedChapters{Chapters: in.Chapters, Original: *in.Original}, true, nil
}

// WriteChaptersSidecar writes chapters (milliseconds, already on the global
// timeline) to path atomically. An empty list removes the file instead, so a
// re-finalize that found none does not leave an earlier list behind.
func WriteChaptersSidecar(path string, chapters []Chapter) error {
	if len(chapters) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	out := chaptersSidecar{Chapters: make([]SidecarChapter, 0, len(chapters))}
	for _, c := range chapters {
		out.Chapters = append(out.Chapters, SidecarChapter{
			Start: float64(c.StartMs) / 1000, End: float64(c.EndMs) / 1000, Title: c.Title,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// ReadChaptersSidecar reads a ChaptersSidecarSuffix file. A missing file is
// no chapters and no error.
func ReadChaptersSidecar(path string) ([]SidecarChapter, error) {
	in, err := readChaptersSidecar(path)
	if err != nil || in == nil {
		return nil, err
	}
	return in.Chapters, nil
}

// readChaptersSidecar reads the file whole; nil, nil when it is missing.
func readChaptersSidecar(path string) (*chaptersSidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var in chaptersSidecar
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("parse chapters sidecar: %w", err)
	}
	return &in, nil
}

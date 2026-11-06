package utils

import (
	"encoding/json"
	"fmt"
	"os"
)

// ChaptersSidecarSuffix is the chapter list a split archive keeps beside its
// parts, "<name>.chapters.json" (owner decision FE-15): a single-file archive
// carries its chapters inside the MP4, but a split one's parts are muxed one
// by one as they finish, before any list for the whole broadcast exists, and
// a recorded part is never re-muxed to add one. <name> is the base the parts
// share — "X" for "X - part1.mp4" — as for the job's chat archive and its raw
// sidecar, and like them it is named on no column: it is found from the
// archive's own name (GET /api/jobs/{id}/chapters, the orphan sweep). Not the
// staging dir's StagedChaptersFile, which is the capture's input to the mux.
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
type chaptersSidecar struct {
	Chapters []SidecarChapter `json:"chapters"`
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
	return in.Chapters, nil
}

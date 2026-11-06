package routes

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestChaptersSplitArchiveListBesideTheParts is owner decision FE-15's route
// half: a split archive's parts are muxed without chapters (they finish one
// by one, before a list for the broadcast exists, and are never re-muxed), so
// the finalize writes the list beside them as "<name>.chapters.json", on the
// player's global timeline, and GET /api/jobs/{id}/chapters answers it when
// the parts carry no chapters of their own — under the base the parts share,
// the first part's " - partN" taken off. Parts that do carry chapters are
// answered as before, the file left unread; and a file outside the output
// directory is not read.
//
// Mutants (each run): the fallback dropped (the split job answers nothing);
// the fallback taken even when the parts have chapters (the part's own list
// is replaced); the sidecar named from the first part's whole name, " -
// part1" kept (nothing is found).
func TestChaptersSplitArchiveListBesideTheParts(t *testing.T) {
	f := newChapterFixture(t)
	p1 := filepath.Join(f.outputDir, "Show - part1.mp4")
	p2 := filepath.Join(f.outputDir, "Show - part2.mp4")
	q1 := filepath.Join(f.outputDir, "Other - part1.mp4")
	for _, p := range []string{p1, p2, q1} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := []utils.Chapter{{StartMs: 0, EndMs: 90_000, Title: "Just Chatting"}, {StartMs: 90_000, EndMs: 150_500, Title: "Art"}}
	for _, base := range []string{"Show", "Other"} {
		if err := utils.WriteChaptersSidecar(filepath.Join(f.outputDir, base+utils.ChaptersSidecarSuffix), list); err != nil {
			t.Fatal(err)
		}
	}
	f.cache.probe = func(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
		if filepath.Base(file) == "Other - part1.mp4" {
			return []Chapter{{Start: 0, End: 5, Title: "embedded"}}, nil
		}
		return nil, nil
	}
	f.addJob(t, "tw_split", func(j *database.Job) {
		j.Platform = "twitch"
		j.OutputFile = p1
		j.Segments = []database.Segment{
			{SegmentIndex: 0, Filename: filepath.Base(p1), FilePath: p1, DurationSeconds: 100},
			{SegmentIndex: 1, Filename: filepath.Base(p2), FilePath: p2, DurationSeconds: 60},
		}
	})
	f.addJob(t, "tw_embedded", func(j *database.Job) {
		j.Platform = "twitch"
		j.Segments = []database.Segment{{SegmentIndex: 0, Filename: filepath.Base(q1), FilePath: q1, DurationSeconds: 10}}
	})

	want := []Chapter{{Start: 0, End: 90, Title: "Just Chatting"}, {Start: 90, End: 150.5, Title: "Art"}}
	if got := f.get(t, "tw_split"); !reflect.DeepEqual(got, want) {
		t.Errorf("split archive: %+v, want the list beside its parts %+v", got, want)
	}
	if got := f.get(t, "tw_embedded"); !slices.Equal(got, []Chapter{{Start: 0, End: 5, Title: "embedded"}}) {
		t.Errorf("parts with chapters: %+v, want their own", got)
	}

	// Outside the output directory: not read.
	outside := t.TempDir()
	o1 := filepath.Join(outside, "Far - part1.mp4")
	if err := os.WriteFile(o1, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := utils.WriteChaptersSidecar(filepath.Join(outside, "Far"+utils.ChaptersSidecarSuffix), list); err != nil {
		t.Fatal(err)
	}
	f.addJob(t, "tw_far", func(j *database.Job) {
		j.Segments = []database.Segment{{SegmentIndex: 0, Filename: filepath.Base(o1), FilePath: o1, DurationSeconds: 10}}
	})
	if got := f.get(t, "tw_far"); len(got) != 0 {
		t.Errorf("a list outside the output directory was read: %+v", got)
	}
}

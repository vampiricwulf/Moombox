package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestGapFillRefitsTheSplitArchiveChapters is owner decision FE-15 under a
// gap-fill re-finalize (D17): the split archive's <name>.chapters.json was
// laid on the parts as they stood, and the fill's commit inserts a recovered
// part, which the player lays end to end with the others. The list was left
// as it was, so every chapter after the filled gap showed early by the
// recovered part's length. It is now shifted past each recovered part
// inserted at or before it: the chapter running into the gap covers it, the
// one that began in it starts with the part after it, and the list's start
// stays 0, so a recovered opening falls to the first chapter. A recovered
// opening becomes part 1, so the list moves to the new first part's base,
// where the chapters route looks.
//
// Mutants: the commit's refit call removed (B still starts at 20 s); a point
// shifted only past an insert strictly before it (A's end and B's start stay
// at 20 s); the list's start shifted too (the opening's A starts at 30 s);
// the list written back under the old base (the route, reading the new first
// part's base, finds none).
func TestGapFillRefitsTheSplitArchiveChapters(t *testing.T) {
	read := func(t *testing.T, path string) []utils.SidecarChapter {
		t.Helper()
		list, err := utils.ReadChaptersSidecar(path)
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	write := func(t *testing.T, path string, list []utils.Chapter) {
		t.Helper()
		if err := utils.WriteChaptersSidecar(path, list); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("between recorded parts", func(t *testing.T) {
		f := newGapFillFixture(t)
		p1 := filepath.Join(f.dir, "Streamer - Big day - part1.mp4")
		p2 := filepath.Join(f.dir, "Streamer - Big day - part2.mp4")
		for _, p := range []string{p1, p2} {
			os.WriteFile(p, []byte("RECORDED"), 0o644)
		}
		if err := f.db.ReplaceJobSegments("tw_1", []database.Segment{
			{SegmentIndex: 0, Filename: filepath.Base(p1), FilePath: p1, DurationSeconds: 20, UnixStart: 900},
			{SegmentIndex: 1, Filename: filepath.Base(p2), FilePath: p2, DurationSeconds: 20, UnixStart: 100},
		}); err != nil {
			t.Fatal(err)
		}
		sidecar := filepath.Join(f.dir, "Streamer - Big day.chapters.json")
		write(t, sidecar, []utils.Chapter{{StartMs: 0, EndMs: 20_000, Title: "A"}, {StartMs: 20_000, EndMs: 40_000, Title: "B"}})
		f.plan(t, []stagedPartCoverage{covPart(0, 0, 20*time.Second, false), covPart(1, 40*time.Second, 60*time.Second, false)},
			[]gapSpan{{FromMs: ms(20 * time.Second), ToMs: ms(40 * time.Second), Kind: gapKindLost, AfterPart: 0}},
			gapStart.Add(25*time.Hour))
		row, _ := f.db.GetGapFill("tw_1")
		f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

		job, _ := f.db.GetJob("tw_1")
		if len(job.Segments) != 3 || job.Segments[1].DurationSeconds != 40 {
			t.Fatalf("segments %+v, want a 40 s recovered part between the two", job.Segments)
		}
		want := []utils.SidecarChapter{{Start: 0, End: 60, Title: "A"}, {Start: 60, End: 80, Title: "B"}}
		if got := read(t, sidecar); !reflect.DeepEqual(got, want) {
			t.Errorf("chapters = %+v, want %+v", got, want)
		}
	})

	t.Run("a recovered opening", func(t *testing.T) {
		f := newGapFillFixture(t)
		old := filepath.Join(f.dir, "Streamer - Big day.chapters.json")
		write(t, old, []utils.Chapter{{StartMs: 0, EndMs: 1_000_000, Title: "A"}, {StartMs: 1_000_000, EndMs: 3_000_000, Title: "B"}})
		f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
			[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
			gapStart.Add(25*time.Hour))
		row, _ := f.db.GetGapFill("tw_1")
		f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

		job, _ := f.db.GetJob("tw_1")
		if len(job.Segments) != 2 {
			t.Fatalf("segments %+v, want the recovered opening and the recording", job.Segments)
		}
		moved := ArchiveSidecarPath(job.Segments[0].FilePath, utils.ChaptersSidecarSuffix)
		want := []utils.SidecarChapter{{Start: 0, End: 1030, Title: "A"}, {Start: 1030, End: 3030, Title: "B"}}
		if got := read(t, moved); !reflect.DeepEqual(got, want) {
			t.Errorf("chapters under %s = %+v, want %+v", filepath.Base(moved), got, want)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Errorf("the list under the old base is still there (%v)", err)
		}
	})
}

package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestSplitArchiveChaptersOnTheGlobalTimeline: a split archive's chapter
// list is read in the capture's timeline and served on the player's, where
// the parts are laid end to end (owner decision FE-15). A VOD's parts cover
// the video end to end, so its list stands as it is. A live capture's parts
// leave out what fell between them: part 1 ran 0-100 s of the capture, part
// 2 from 160 s for 60 s (a minute-long outage between). A chapter starting
// inside the outage begins where part 2 does; one inside part 2 at its
// distance into it plus part 1's length; one wholly inside the outage is
// dropped; the end of the last is the global end.
//
// Mutants (each run): the VOD list mapped by the wall clock too (the live
// guard dropped); the distance into a part not added (Art starts at 100); a
// chapter that collapsed in the gap kept (Outage only, zero-length); the live
// span without the outage (220 s read as 160 s).
func TestSplitArchiveChaptersOnTheGlobalTimeline(t *testing.T) {
	const t0 = 1_800_000_000
	segs := []database.Segment{
		{SegmentIndex: 0, UnixStart: t0, DurationSeconds: 100},
		{SegmentIndex: 1, UnixStart: t0 + 160, DurationSeconds: 60},
	}
	list := []utils.Chapter{
		{StartMs: 0, EndMs: 110_000, Title: "Just Chatting"},
		{StartMs: 110_000, EndMs: 130_000, Title: "Outage only"},
		{StartMs: 130_000, EndMs: 170_000, Title: "Into part 2"},
		{StartMs: 170_000, EndMs: 220_000, Title: "Art"},
	}
	got := splitArchiveChapters(list, segs, true)
	want := []utils.Chapter{
		{StartMs: 0, EndMs: 100_000, Title: "Just Chatting"},
		{StartMs: 100_000, EndMs: 110_000, Title: "Into part 2"},
		{StartMs: 110_000, EndMs: 160_000, Title: "Art"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("live = %+v, want %+v", got, want)
	}
	if got := splitArchiveChapters(list, segs, false); !reflect.DeepEqual(got, list) {
		t.Errorf("VOD = %+v, want the list as it stands", got)
	}
	if got, want := splitTimelineSpanMs(segs, true), int64(220_000); got != want {
		t.Errorf("live span = %d, want %d (the outage included)", got, want)
	}
	if got, want := splitTimelineSpanMs(segs, false), int64(160_000); got != want {
		t.Errorf("VOD span = %d, want %d (the parts end to end)", got, want)
	}
}

// TestSplitArchiveFinalizeWritesItsChapterList is FE-15 through the real
// finalize: a split live Twitch capture whose parts were muxed without
// chapters gets <name>.chapters.json beside its parts — the base they share —
// from the categories its staging observed, on the global timeline; the parts
// themselves are left as they were (never re-muxed). The orphan sweep counts
// the file as the job's (FE-16). A re-finalize with no list removes it.
//
// Mutants (each run): the finalize's writeSplitArchiveChapters call removed
// (no file); the list written without the global-timeline move (Elden Ring
// at 600, the capture's wall clock, past part 1's 300 s).
func TestSplitArchiveFinalizeWritesItsChapterList(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	job := &database.Job{ID: "tw_split_ch", VideoID: "tw_split_ch", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusDownloading, StreamStartTime: start.Format(time.RFC3339)}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	staging := t.TempDir()
	// The capture began at the broadcast's start; the category changed ten
	// minutes in, which is 5 minutes after an outage that ran 300-600 s.
	tl := `{"recordingStartMs":` + strconv.FormatInt(start.UnixMilli(), 10) +
		`,"categories":[{"atMs":` + strconv.FormatInt(start.Add(2*time.Second).UnixMilli(), 10) + `,"title":"Just Chatting"},` +
		`{"atMs":` + strconv.FormatInt(start.Add(10*time.Minute).UnixMilli(), 10) + `,"title":"Elden Ring"}]}`
	if err := os.WriteFile(filepath.Join(staging, twitchTimelineFile), []byte(tl), 0o644); err != nil {
		t.Fatal(err)
	}
	var segments []database.Segment
	for i, sp := range []struct{ at, dur int64 }{{0, 300}, {600, 300}} {
		name := "Show - part" + strconv.Itoa(i+1) + ".mp4"
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, database.Segment{SegmentIndex: i, Filename: name, FilePath: p,
			UnixStart: start.Unix() + sp.at, DurationSeconds: float64(sp.dur)})
	}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: staging,
		OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for copyAssets' thumbnail fallback
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments); err != nil {
		t.Fatalf("finalizeMultiSegmentJob: %v", err)
	}

	sidecar := filepath.Join(root, "Show"+utils.ChaptersSidecarSuffix)
	got, err := utils.ReadChaptersSidecar(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	want := []utils.SidecarChapter{{Start: 0, End: 300, Title: "Just Chatting"}, {Start: 300, End: 600, Title: "Elden Ring"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chapters beside the parts = %+v, want %+v", got, want)
	}
	for _, s := range segments {
		if b, _ := os.ReadFile(s.FilePath); string(b) != "part" {
			t.Errorf("part %s was rewritten", s.Filename)
		}
	}

	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: root, StagingDirectory: t.TempDir()}}
	entries, err := ScanOrphanedFiles(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Path == sidecar {
			t.Errorf("the job's own chapter list is offered as an orphan")
		}
	}

	// No list now (the observed timeline gone): the stale file goes.
	if err := os.Remove(filepath.Join(staging, twitchTimelineFile)); err != nil {
		t.Fatal(err)
	}
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments); err != nil {
		t.Fatalf("re-finalize: %v", err)
	}
	if fileExists(sidecar) {
		t.Error("a re-finalize with no chapters left the earlier list")
	}
}

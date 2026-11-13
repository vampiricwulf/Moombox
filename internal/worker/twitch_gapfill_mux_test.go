package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestGapFillLeavesAPlanWhileItsJobIsMuxing: a look that finds the plan's job
// Muxing leaves the plan as it is, due, for the next sweep — the mux's own end
// keeps, replaces or drops the row. It used to drop it as "no longer
// finished", so a sweep that ticked during the operator's Mux of a Finished
// archive lost the fill. A job in any other status but Finished still drops
// its plan.
//
// Mutant: the Muxing case removed (the plan is deleted mid-mux).
func TestGapFillLeavesAPlanWhileItsJobIsMuxing(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusMuxing})
	before, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *before, gapStart.Add(70*time.Minute))

	after, _ := f.db.GetGapFill("tw_1")
	if p := f.row(t); p == nil || p.Attempts != 0 || after.NextCheckAt != before.NextCheckAt || f.fetched != 0 {
		t.Fatalf("plan %+v after %d fetches, want it untouched and still due while the job is Muxing", p, f.fetched)
	}

	f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusError})
	f.filler.step(context.Background(), *before, gapStart.Add(70*time.Minute))
	if g, _ := f.db.GetGapFill("tw_1"); g != nil {
		t.Errorf("the plan of a job left in Error was kept: %s", g.Plan)
	}
}

// TestMuxOfAFinishedTwitchJobKeepsItsPendingGapFill: a split live Twitch
// capture finished with one part unmuxed in staging and a late start planned
// for a fill. The operator's Mux of the leftover part re-finalizes the same
// recording, so the plan its finalize left is kept — it used to be deleted as
// "a capture that lacks nothing" — and the fill then runs on the re-finalized
// job: the recovered opening is added ahead of the recorded parts, the newly
// muxed one among them.
//
// Mutants: the Mux's DeleteGapFill run for a Finished prior (the plan is
// gone); the plan kept but the fill refused on the re-finalized row.
func TestMuxOfAFinishedTwitchJobKeepsItsPendingGapFill(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	outDir := t.TempDir()
	const id = "tw_1"
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: "1", URL: "https://twitch.tv/streamer", Platform: "twitch",
		Title: "Big day", Status: database.StatusFinished, OutputDirectory: outDir,
		StreamStartTime: gapStart.Format(time.RFC3339), TwitchQuality: "chunked"}); err != nil {
		t.Fatal(err)
	}
	// Part 0 was muxed and recorded by the capture's finalize; seg_1 was not.
	stageSplitParts(t, filepath.Join(stagingBase, id), "video.ts")
	part0 := filepath.Join(outDir, "Big day - part0.mp4")
	if err := os.WriteFile(part0, []byte("PART0"), 0o644); err != nil {
		t.Fatal(err)
	}
	size := int64(5)
	if applied, err := db.ReplaceJobSegmentsIf(id, database.StatusFinished, []database.Segment{{JobID: id, SegmentIndex: 0,
		Filename: filepath.Base(part0), FilePath: part0, FileSize: &size, DurationSeconds: 15,
		UnixStart: gapStart.Add(25 * time.Second).Unix(), UnixEnd: gapStart.Add(40 * time.Second).Unix()}}); err != nil || !applied {
		t.Fatalf("recording part 0: applied %v, %v", applied, err)
	}
	if !HasUnmuxedParts(db, stagingBase, id) {
		t.Fatal("the fixture has no unmuxed part")
	}
	plan := &twitchGapFillPlan{JobID: id, Login: "streamer", StreamStart: gapStart.Format(time.RFC3339),
		EndedAt: gapStart.Add(time.Hour).Format(time.RFC3339), Deadline: gapStart.Add(25 * time.Hour).Format(time.RFC3339),
		VariantName: "chunked",
		Parts:       []stagedPartCoverage{covPart(0, 25*time.Second, 40*time.Second, false), covPart(1, 40*time.Second, time.Minute, false)},
		Spans:       []gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}}}
	if err := saveTwitchGapFillPlan(db, plan, gapStart); err != nil {
		t.Fatal(err)
	}
	w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) { return nil, nil }

	if err := w.MuxJob(id); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	j, _ := db.GetJob(id)
	if j == nil || j.Status != database.StatusFinished || len(j.Segments) != 2 {
		t.Fatalf("muxed row = %v (error %q) with %d parts, want Finished with the leftover part recorded", statusOf(j), errorOf(j), len(j.Segments))
	}
	if st := TwitchGapFillStateOf(db, j); st == nil || st.State != GapFillPending || st.Spans != 1 {
		t.Fatalf("gapFill = %+v after the Mux of a Finished job, want its plan still pending", st)
	}

	// The fill still runs on the re-finalized job.
	f := newGapFillFixture(t)
	f.db, f.filler.db = db, db
	row, _ := db.GetGapFill(id)
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	if out := f.outcome(t); out == nil || out.State != GapFillFilled || out.Spans != 1 {
		t.Fatalf("outcome %+v, want the kept plan filled; logged %v", out, f.log.lines())
	}
	j, _ = db.GetJob(id)
	if len(j.Segments) != 3 || !strings.Contains(j.Segments[0].Filename, " - recovered1.mp4") || j.Segments[1].FilePath != part0 {
		var names []string
		for _, s := range j.Segments {
			names = append(names, s.Filename)
		}
		t.Errorf("parts %v, want the recovered opening, then part 0, then the part the Mux added", names)
	}
}

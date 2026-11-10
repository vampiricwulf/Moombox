package worker

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// readoutJob is a Muxing row carrying the line and percent a download's
// finalize phase leaves.
func readoutJob(t *testing.T, db *database.Database, id string) {
	t.Helper()
	job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusMuxing,
		Progress: "V:100% A:100% C: 7", Percent: 100}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob %s: %v", id, err)
	}
}

func progressOf(t *testing.T, db *database.Database, id string) (string, float64) {
	t.Helper()
	job, err := db.GetJob(id)
	if err != nil || job == nil {
		t.Fatalf("GetJob %s: %v", id, err)
	}
	return job.Progress, job.Percent
}

// TestMuxReadoutWritesTheLineAndNothingElse is owner ruling R-D1 of 2026-11-10's writer:
// each of FFmpeg's reports becomes the row's progress line — a percent while
// the output grows, "finalizing" after — written once per change, through
// the progress gate, and only while the row is Muxing. The percent (the bar)
// is the download's and is never written.
//
// Mutants: report writing with UpdateJobFields (the line lands on the
// Cancelled row); report writing "percent" (the bar moves to the copy's);
// report writing an unchanged line again (the write count grows).
func TestMuxReadoutWritesTheLineAndNothingElse(t *testing.T) {
	w, db := testWorkerSetup(t)
	readoutJob(t, db, "j")
	job, _ := db.GetJob("j")
	jobCtx := w.buildJobContext(job)
	r := w.orchestrator.newMuxReadout(jobCtx)
	if r == nil {
		t.Fatal("no readout for a complete Muxing job")
	}
	clock := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return clock }
	r.interval = 16 * time.Millisecond

	var mu sync.Mutex
	writes := 0
	stop := db.OnJobChange(func(c *database.JobChange) {
		mu.Lock()
		defer mu.Unlock()
		if slices.Equal(c.Changes, []string{"status"}) {
			return // this test's own Cancel
		}
		writes++
		if !slices.Equal(c.Changes, []string{"progress"}) {
			t.Errorf("the readout wrote %v, want the progress line alone", c.Changes)
		}
	})
	defer stop()
	wrote := func() int { mu.Lock(); defer mu.Unlock(); return writes }

	report := r.callback()
	report(engine.CopyProgress{OutputBytes: 420, InputBytes: 1000})
	if line, pct := progressOf(t, db, "j"); line != "Muxing 42%" || pct != 100 {
		t.Fatalf("after the first report: %q, percent %v; want \"Muxing 42%%\" and the download's 100", line, pct)
	}
	// A changed line inside the gate waits for the next report.
	clock = clock.Add(time.Millisecond)
	report(engine.CopyProgress{OutputBytes: 500, InputBytes: 1000})
	if line, _ := progressOf(t, db, "j"); line != "Muxing 42%" {
		t.Errorf("a line inside the progress gate was written: %q", line)
	}
	// The same percent again writes nothing.
	clock = clock.Add(time.Second)
	report(engine.CopyProgress{OutputBytes: 425, InputBytes: 1000})
	if n := wrote(); n != 1 {
		t.Errorf("%d writes after an unchanged line, want 1", n)
	}
	clock = clock.Add(time.Second)
	report(engine.CopyProgress{OutputBytes: 990, InputBytes: 1000, Finalizing: true})
	if line, pct := progressOf(t, db, "j"); line != jobstate.MuxFinalizingLine || pct != 100 {
		t.Fatalf("after the stall: %q, percent %v; want %q and 100", line, pct, jobstate.MuxFinalizingLine)
	}

	// An operator's Cancel: the next line is refused, and the readout stops.
	if !db.UpdateJobFieldsIf("j", database.StatusMuxing, map[string]any{"status": database.StatusCancelled}) {
		t.Fatal("could not cancel the row")
	}
	before := wrote()
	clock = clock.Add(time.Second)
	report(engine.CopyProgress{OutputBytes: 100, InputBytes: 1000})
	clock = clock.Add(time.Second)
	report(engine.CopyProgress{OutputBytes: 200, InputBytes: 1000})
	if line, _ := progressOf(t, db, "j"); line != jobstate.MuxFinalizingLine {
		t.Errorf("a line was written to the Cancelled row: %q", line)
	}
	if n := wrote(); n != before {
		t.Errorf("%d writes to a row that is no longer Muxing", n-before)
	}

	// The copy made no archive: the Cancelled row gets its line back.
	r.restore()
	if line, pct := progressOf(t, db, "j"); line != "V:100% A:100% C: 7" || pct != 100 {
		t.Errorf("restored line = %q, percent %v; want the download's", line, pct)
	}
}

// TestMuxReadoutRestoreLeavesARowThatMovedOn: the line is put back on a row
// still Muxing (a failed copy, about to read Error) and left alone on one a
// reset has taken back, and a readout that wrote nothing restores nothing.
func TestMuxReadoutRestoreLeavesARowThatMovedOn(t *testing.T) {
	w, db := testWorkerSetup(t)
	for _, tc := range []struct {
		id       string
		report   bool
		move     database.JobStatus
		progress string // written with the move
		want     string
	}{
		{"failed", true, "", "", "V:100% A:100% C: 7"},
		{"reset", true, database.StatusUpcoming, "", ""},
		{"silent", false, "", "", "V:100% A:100% C: 7"},
	} {
		readoutJob(t, db, tc.id)
		job, _ := db.GetJob(tc.id)
		r := w.orchestrator.newMuxReadout(w.buildJobContext(job))
		if tc.report {
			r.report(engine.CopyProgress{OutputBytes: 10, InputBytes: 100})
			if line, _ := progressOf(t, db, tc.id); line != "Muxing 10%" {
				t.Fatalf("%s: line = %q", tc.id, line)
			}
		}
		if tc.move != "" {
			db.UpdateJobFields(tc.id, map[string]any{"status": tc.move, "progress": tc.progress})
		}
		writes := 0
		stop := db.OnJobChange(func(*database.JobChange) { writes++ })
		r.restore()
		stop()
		if line, _ := progressOf(t, db, tc.id); line != tc.want {
			t.Errorf("%s: line after restore = %q, want %q", tc.id, line, tc.want)
		}
		if !tc.report && writes != 0 {
			t.Errorf("%s: a readout that wrote nothing restored (%d writes)", tc.id, writes)
		}
	}
	// And no readout at all is safe to use.
	var none *muxReadout
	if none.callback() != nil {
		t.Error("a nil readout handed the engine a callback")
	}
	none.restore()
}

// TestMuxReadoutDoesNotSaveAReadoutLine: a mux killed mid-copy leaves its
// readout line on the Muxing row, and the re-mux after the restart starts
// from that row. The line is not a download's, so a copy that then fails
// leaves the row with no line, not a percent for a mux that is not running.
//
// Mutant: newMuxReadout saving fresh.Progress as it is (the Error row reads
// "Muxing 42%").
func TestMuxReadoutDoesNotSaveAReadoutLine(t *testing.T) {
	w, db := testWorkerSetup(t)
	for _, stale := range []string{"Muxing 42%", jobstate.MuxFinalizingLine} {
		id := "stale-" + stale
		readoutJob(t, db, id)
		db.UpdateJobFields(id, map[string]any{"progress": stale})
		job, _ := db.GetJob(id)
		r := w.orchestrator.newMuxReadout(w.buildJobContext(job))
		if r == nil {
			t.Fatalf("%q: no readout", stale)
		}
		r.report(engine.CopyProgress{OutputBytes: 10, InputBytes: 100})
		if line, _ := progressOf(t, db, id); line != "Muxing 10%" {
			t.Fatalf("%q: line = %q", stale, line)
		}
		r.restore()
		if line, _ := progressOf(t, db, id); line != "" {
			t.Errorf("%q: line after a failed re-mux = %q, want none", stale, line)
		}
	}
}

// TestMuxReadoutFinalizingLineIsNotGated: FFmpeg's last report comes once,
// just before its pipe closes. With a long progress interval it lands inside
// the gate of the percent before it, and nothing repeats it, so the phase
// change is written at once; a percent in the same gate still waits.
//
// Mutant: report gating every line (the row stays on "Muxing 97%").
func TestMuxReadoutFinalizingLineIsNotGated(t *testing.T) {
	w, db := testWorkerSetup(t)
	readoutJob(t, db, "j")
	job, _ := db.GetJob("j")
	r := w.orchestrator.newMuxReadout(w.buildJobContext(job))
	clock := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return clock }
	r.interval = time.Hour

	r.report(engine.CopyProgress{OutputBytes: 970, InputBytes: 1000})
	clock = clock.Add(time.Millisecond)
	r.report(engine.CopyProgress{OutputBytes: 980, InputBytes: 1000})
	if line, _ := progressOf(t, db, "j"); line != "Muxing 97%" {
		t.Fatalf("a percent inside the gate was written: %q", line)
	}
	clock = clock.Add(time.Millisecond)
	r.report(engine.CopyProgress{OutputBytes: 1000, InputBytes: 1000, Finalizing: true})
	if line, _ := progressOf(t, db, "j"); line != jobstate.MuxFinalizingLine {
		t.Fatalf("line after the last report = %q, want %q", line, jobstate.MuxFinalizingLine)
	}
	writes := 0
	stop := db.OnJobChange(func(*database.JobChange) { writes++ })
	r.report(engine.CopyProgress{OutputBytes: 1000, InputBytes: 1000, Finalizing: true})
	stop()
	if writes != 0 {
		t.Errorf("a repeated finalizing report wrote %d times", writes)
	}
}

// TestArchiveMuxShowsItsReadout runs the real FFmpeg through muxAndFinalize
// and watches the row: while it is Muxing its progress line becomes the mux
// readout, the percent stays the download's, and the Finished write clears
// the line as before. A recording known to be missing its tail gets no
// readout: its honest line is on the row at every step and on the Finished
// row (keepIncompleteTailProgress).
//
// Mutants: newMuxReadout ignoring incomplete_tail (the second half sees a
// readout line and loses the honest one); muxAndFinalize passing copyStaged a
// nil callback (no readout line is seen).
func TestArchiveMuxShowsItsReadout(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	const honest = "V:1200/1300 A:1200/1300"
	for _, tc := range []struct {
		name       string
		incomplete bool
	}{{"complete", false}, {"incomplete tail", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			staging, _ := muxFixtureJob(t, w, db, "j-readout")
			writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 4)
			db.UpdateJobFields("j-readout", map[string]any{"progress": honest, "percent": 92.0, "incomplete_tail": tc.incomplete})

			var mu sync.Mutex
			var lines []string
			stop := db.OnJobChange(func(c *database.JobChange) {
				if c.Job == nil || c.Job.Status != database.StatusMuxing {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, c.Job.Progress)
				if c.Job.Percent != 92 {
					t.Errorf("percent moved to %v while Muxing", c.Job.Percent)
				}
			})
			job, _ := db.GetJob("j-readout")
			err := w.orchestrator.muxAndFinalize(context.Background(), w.buildJobContext(job),
				&DownloadResult{HasVideo: true, VideoPath: filepath.Join(staging, "video.mp4")})
			stop()
			if err != nil {
				t.Fatalf("muxAndFinalize: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			readouts := 0
			for _, line := range lines {
				if _, _, ok := jobstate.MuxReadout(line); ok {
					readouts++
				} else if line != honest {
					t.Errorf("a Muxing row read %q", line)
				}
			}
			final, err := db.GetJob("j-readout")
			if err != nil || final == nil || final.Status != database.StatusFinished {
				t.Fatalf("job after the finalize = %v, %v", statusOf(final), err)
			}
			if tc.incomplete {
				if readouts != 0 {
					t.Errorf("an incomplete-tail job showed %d readout lines: %q", readouts, lines)
				}
				if final.Progress != honest || final.Percent != 92 {
					t.Errorf("Finished row = %q, %v; want the honest line and percent kept", final.Progress, final.Percent)
				}
				return
			}
			if readouts == 0 {
				t.Errorf("no readout line was written while Muxing: %q", lines)
			}
			if final.Progress != "" || final.Percent != 100 {
				t.Errorf("Finished row = %q, %v; want the line cleared and 100", final.Progress, final.Percent)
			}
		})
	}
}

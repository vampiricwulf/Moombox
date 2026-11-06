package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestGapFillLeavesOutRefusedSegments: a VOD segment that still answers 403
// after every attempt is the VOD's lasting answer — on Twitch's VOD CDN a
// muted part neither yt-dlp nor streamlink can fetch — so it is left out of
// the span, logged, and the rest of the span is inserted (owner decision D17
// follow-up 2), instead of failing the fill and downloading the span again at
// every look for a day. A span whose every segment is refused stays a gap.
//
// Mutants: refusedVodSegment answering false (the 403 fails the fill, the
// plan is kept); the errSpanRefused branch removed (an empty part is muxed
// and inserted); the refused log line removed.
func TestGapFillLeavesOutRefusedSegments(t *testing.T) {
	t.Run("one segment refused", func(t *testing.T) {
		f := newGapFillFixture(t)
		f.refuse = map[string]bool{"https://vod.example/chunked/4.ts": true}
		f.plan(t, []stagedPartCoverage{covPart(0, 55*time.Second, time.Minute, false)},
			[]gapSpan{{FromMs: ms(0), ToMs: ms(55 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
			gapStart.Add(25*time.Hour))
		row, _ := f.db.GetGapFill("tw_1")
		f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

		if f.row(t) != nil {
			t.Error("the plan was kept; the refused segment failed the fill")
		}
		rec := filepath.Join(f.dir, "Streamer - Big day - recovered1.mp4")
		if data, _ := os.ReadFile(rec); string(data) != "MP4[v0][v1][v2][v3][v5]" {
			t.Errorf("recovered part holds %q, want every segment but the refused one", data)
		}
		job, _ := f.db.GetJob("tw_1")
		if len(job.Segments) != 2 || job.Segments[0].FilePath != rec || job.Segments[0].DurationSeconds != 50 {
			t.Errorf("segments %+v, want the 50 s recovered part first", job.Segments)
		}
		if _, logged := f.log.field("gap-fill: a recovered span leaves out VOD segments Twitch refuses to serve — a muted stretch neither tool can fetch", "refusedSegments"); !logged {
			t.Errorf("the refused segment was not logged; logged %v", f.log.lines())
		}
	})
	t.Run("every segment refused", func(t *testing.T) {
		f := newGapFillFixture(t)
		f.refuse = map[string]bool{"https://vod.example/chunked/4.ts": true}
		// 41-49 s, padded to 40-50 s, is segment 4 alone.
		f.plan(t, []stagedPartCoverage{covPart(0, 0, 41*time.Second, false), covPart(1, 49*time.Second, time.Minute, false)},
			[]gapSpan{{FromMs: ms(41 * time.Second), ToMs: ms(49 * time.Second), Kind: gapKindLost, AfterPart: 0}},
			gapStart.Add(25*time.Hour))
		row, _ := f.db.GetGapFill("tw_1")
		f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

		if f.row(t) != nil {
			t.Error("the plan was kept for a span the VOD will never serve")
		}
		if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
			t.Errorf("an empty span was written: %v", matches)
		}
		if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 || job.OutputFile != f.archive {
			t.Errorf("the job changed: %+v", job)
		}
	})
}

// TestGapFillDropsAPlanWhoseJobLeftFinished: a look at a plan whose job is no
// longer Finished — an operator's re-run, reset or delete since the capture
// finalized — drops the plan without reading anything.
//
// Mutant: step's status check reduced to a nil check (the fill runs on the
// re-run row).
func TestGapFillDropsAPlanWhoseJobLeftFinished(t *testing.T) {
	f := newGapFillFixture(t)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusDownloading})
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	if f.row(t) != nil {
		t.Error("the plan was kept for a job that is no longer Finished")
	}
	if f.fetched != 0 {
		t.Errorf("%d fetches for a job that is no longer Finished, want none", f.fetched)
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 {
		t.Errorf("parts were written: %+v", job.Segments)
	}
}

// TestGapFillCommitRefusesARowThatMoved: a job that leaves Finished while the
// fill runs — here as its span is muxed — keeps its parts exactly as they
// were, and the recovered file is removed: the parts are replaced only while
// the row is still Finished, checked in the transaction that writes them
// (ReplaceJobSegmentsIf).
//
// Mutant: the commit replacing the rows with ReplaceJobSegments, unguarded
// (the moved row gets the recording and the recovered part written over it).
func TestGapFillCommitRefusesARowThatMoved(t *testing.T) {
	f := newGapFillFixture(t)
	mux := f.filler.mux
	f.filler.mux = func(ctx context.Context, ts, out string) error {
		f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusDownloading})
		return mux(ctx, ts, out)
	}
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	job, _ := f.db.GetJob("tw_1")
	if len(job.Segments) != 0 || job.OutputFile != f.archive {
		t.Errorf("the moved row was rewritten: segments %+v, output %q", job.Segments, job.OutputFile)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
		t.Errorf("the refused commit left %v", matches)
	}
}

// TestGapFillSweepSurvivesAPanic: a plan whose look panics is dropped and the
// sweep goes on — the next plan is looked at in the same sweep — instead of
// the panic ending the gap-fill goroutine until the next start, where the
// same plan would panic again.
//
// Mutant: sweep calling step without stepRecovered (the panic escapes).
func TestGapFillSweepSurvivesAPanic(t *testing.T) {
	f := newGapFillFixture(t)
	if _, err := f.db.AddJob(&database.Job{ID: "tw_2", VideoID: "2", Platform: "twitch", Status: database.StatusFinished,
		OutputFile: f.archive, StreamStartTime: gapStart.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	second := &twitchGapFillPlan{JobID: "tw_2", Login: "streamer", StreamStart: gapStart.Format(time.RFC3339),
		Deadline: gapStart.Add(25 * time.Hour).Format(time.RFC3339), Spans: []gapSpan{{FromMs: ms(0), ToMs: ms(time.Second), Kind: gapKindLateStart, AfterPart: -1}}}
	if err := saveTwitchGapFillPlan(f.db, second, gapStart.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// tw_1 is due first; its lookup panics, tw_2's finds nothing listed.
	var looked []string
	f.filler.findVod = func(context.Context, string, time.Time, time.Time) (string, error) {
		looked = append(looked, "find")
		if len(looked) == 1 {
			panic("boom")
		}
		return "", nil
	}

	f.filler.sweep(context.Background(), gapStart.Add(70*time.Minute))

	if f.row(t) != nil {
		t.Error("the plan whose look panicked was kept")
	}
	if len(looked) != 2 {
		t.Errorf("%d looks, want the panicking plan and the next one", len(looked))
	}
	if g, _ := f.db.GetGapFill("tw_2"); g == nil {
		t.Error("the next plan was dropped too")
	}
}

// TestGapFillDistrustsAShortVod: an archive VOD shorter than the stretch the
// capture itself recorded cannot be on the broadcast's wall-clock timeline
// (it left something out, or does not start at createdAt), so nothing is cut
// from it — the archive is kept as recorded, with one line saying why —
// rather than inserting the wrong footage.
//
// Mutant: the timeline check removed (the opening is cut from the short VOD).
func TestGapFillDistrustsAShortVod(t *testing.T) {
	f := newGapFillFixture(t)
	// The VOD is 60 s; the capture recorded until 50 min in.
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, 50*time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	if f.row(t) != nil {
		t.Error("the plan was kept")
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 || job.OutputFile != f.archive {
		t.Errorf("a part was cut from a VOD whose timeline does not match: %+v", job)
	}
	if _, logged := f.log.field("gap-fill: the archive VOD is shorter than the capture's own recording, so its timeline cannot be placed; the archive is kept as recorded", "capturedSec"); !logged {
		t.Errorf("no line saying why; logged %v", f.log.lines())
	}
}

// TestNewTwitchGapFillerReadsTheDiskGate pins the worker's filler's disk seam:
// the output-volume reading the backlog scheduler admits on, critical at
// disk_critical_percent.
//
// Mutant: newTwitchGapFiller leaving diskCritical nil (the fill never waits).
func TestNewTwitchGapFillerReadsTheDiskGate(t *testing.T) {
	w, _ := testWorkerSetup(t)
	w.cfg.Disk.CriticalPercent = 95
	used := 99.0
	w.diskSpace = func(string) (*disk.DiskSpace, error) {
		return &disk.DiskSpace{Free: 1 << 30, Total: 100 << 30, UsedPct: used}, nil
	}
	f := w.newTwitchGapFiller()
	if f.diskCritical == nil || !f.diskCritical() {
		t.Fatal("a 99% full volume does not read critical")
	}
	used = 50
	if f.diskCritical() {
		t.Error("a half-full volume reads critical")
	}
}

// TestKeepTwitchGapFillPlanOnlyForAFinishedJob: a capture's plan is stored
// only when its finalize left the job Finished; one that failed or was
// cancelled has no archive to fill.
//
// Mutant: keepTwitchGapFillPlan's Finished check removed (the errored job's
// plan is stored).
func TestKeepTwitchGapFillPlanOnlyForAFinishedJob(t *testing.T) {
	w, db := testWorkerSetup(t)
	for id, st := range map[string]database.JobStatus{"tw_f": database.StatusFinished, "tw_e": database.StatusError} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: "twitch", Status: st}); err != nil {
			t.Fatal(err)
		}
		w.orchestrator.keepTwitchGapFillPlan(&twitchGapFillPlan{JobID: id, Spans: []gapSpan{{Kind: gapKindTail}}})
	}
	if g, _ := db.GetGapFill("tw_f"); g == nil {
		t.Error("the finished job's plan was not kept")
	}
	if g, _ := db.GetGapFill("tw_e"); g != nil {
		t.Error("the errored job's plan was kept")
	}
}

// TestGapSpansForAPartItCannotPlace: a part that holds footage but cannot
// say since when — no coverage file (a part a build without the gap-fill
// recorded), or one whose recorder found footage and no coverage on resume
// (StartUnknown) — makes no spans at all, since any span around it could be
// footage it already holds; an empty part with no coverage is still only
// left out.
//
// Mutants: a part with media and no coverage left out as an empty one is (a
// late start over its footage is planned); StartUnknown ignored (likewise);
// newCoverageRecorder not marking a resumed part that holds footage.
func TestGapSpansForAPartItCannotPlace(t *testing.T) {
	recorded := covPart(1, 62*time.Minute, 120*time.Minute, false)
	if got := gapSpansFor(gapStart, []stagedPartCoverage{{Index: 0, HasMedia: true}, recorded}, false, true); got != nil {
		t.Errorf("a part with footage and no coverage: spans %+v, want none", got)
	}
	unknown := covPart(0, 30*time.Minute, 61*time.Minute, false)
	unknown.Cov.StartUnknown = true
	if got := gapSpansFor(gapStart, []stagedPartCoverage{unknown, recorded}, false, true); got != nil {
		t.Errorf("a part whose start is unknown: spans %+v, want none", got)
	}
	if got := gapSpansFor(gapStart, []stagedPartCoverage{{Index: 0}, recorded}, false, false); len(got) != 1 || got[0].Kind != gapKindLateStart {
		t.Errorf("an empty part: spans %+v, want it left out and the late start kept", got)
	}

	// The resume: a part dir with footage and no coverage file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("TS"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newCoverageRecorder(dir)
	if c := readPartCoverage(dir); !c.StartUnknown {
		t.Fatalf("resumed part's coverage on disk %+v, want StartUnknown at once", c)
	}
	r.note(gapStart.Add(61*time.Minute), 2)
	r.close(false)
	parts := stagedPartCoverages(dir)
	if len(parts) != 1 || !parts[0].HasMedia || !parts[0].Cov.StartUnknown {
		t.Errorf("staged coverage %+v, want the part's footage and its unknown start", parts)
	}
	// A fresh dir is an ordinary part.
	fresh := t.TempDir()
	newCoverageRecorder(fresh).note(gapStart, 2)
	if c := readPartCoverage(fresh); c.StartUnknown || c.StartMs != ms(0) {
		t.Errorf("fresh part's coverage %+v", c)
	}
}

// TestMuxedTwitchCaptureKeepsAGapFillPlan: a live capture that did not
// finalize itself — its broadcast's end unconfirmed, muxed automatically once
// the end is confirmed (D-T4), or its broadcast over while Moombox was down,
// muxed by the operator — keeps a gap-fill plan from the coverage its
// staging still holds, its tail open (owner decision D17). So does any other
// Mux of a live capture's staging (owner decision FE-25): an Error row for
// any other reason, and a Cancelled row — the operator's Mux of either. A
// clip (a VOD-style row) keeps none. A Cancelled row's tail is closed
// whenever it was cancelled (owner decision FQ-2): cancelled while
// Downloading (no stream_end_time) as while Muxing, and whatever marker the
// Cancel left on the row.
//
// Mutants: muxJob not calling twitchMuxGapFillPlan (no plan on any path); the
// old error/marker gate restored (the plain Error and the Cancelled rows get
// no plan); the status gate dropping Cancelled (the Cancelled row gets none);
// the IsVod guard removed (the clip row gets a plan); the tail not left open;
// twitchMuxTailOpen without its Cancelled test (the row cancelled while
// Downloading plans a tail), or with it after the markers (the cancelled
// ended-offline row plans one).
func TestMuxedTwitchCaptureKeepsAGapFillPlan(t *testing.T) {
	cases := []struct {
		name      string
		marked    bool
		errMsg    string
		auto      bool
		cancelled bool
		vod       bool
		ended     bool // stream_end_time set: the capture reached its finalize
		wantKinds string
	}{
		{"end unconfirmed, auto-muxed", true, "", true, false, false, false, "late_start,tail"},
		{"ended while offline, Mux action", false, twitchEndedOfflineErrMsg, false, false, false, false, "late_start,tail"},
		{"another error, Mux action", false, "", false, false, false, false, "late_start,tail"},
		{"cancelled while downloading, Mux action", false, "", false, true, false, false, "late_start"},
		{"cancelled while muxing an ended-offline row, Mux action", false, twitchEndedOfflineErrMsg, false, true, false, false, "late_start"},
		{"a VOD-style row, Mux action", false, "", false, false, true, false, ""},
		// The capture reached its finalize after a normal end and only its
		// mux failed, or the operator cancelled it while Muxing: it holds
		// the broadcast to its end, so the Mux fills the late start alone —
		// an open tail would cut the VOD's last segments again as a
		// duplicate "recovered" part.
		{"mux failed after a normal end, Mux action", false, "ffmpeg mux failed", false, false, false, true, "late_start"},
		{"cancelled while muxing after a normal end, Mux action", false, "", false, true, false, true, "late_start"},
		// The markers keep the tail open whatever else the row carries.
		{"ended while offline with an end time, Mux action", false, twitchEndedOfflineErrMsg, false, false, false, true, "late_start,tail"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			id := "tw_3" + string(rune('0'+i))
			latchedJob(t, w, db, id, 10, tc.marked)
			fields := map[string]any{"stream_start_time": gapStart.Format(time.RFC3339), "twitch_quality": "chunked"}
			if tc.errMsg != "" {
				fields["error"] = tc.errMsg
			}
			if tc.cancelled {
				fields["status"] = database.StatusCancelled
			}
			if tc.vod {
				fields["is_vod"] = true
			}
			if tc.ended {
				fields["stream_end_time"] = gapStart.Add(6 * time.Minute).Format(time.RFC3339)
			}
			db.UpdateJobFields(id, fields)
			var stagingBase string
			w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
			cov, _ := json.Marshal(partCoverage{StartMs: ms(5 * time.Minute), EndMs: ms(5*time.Minute + 10*time.Second)})
			if err := os.WriteFile(filepath.Join(stagingBase, id, twitchCoverageFile), cov, 0o644); err != nil {
				t.Fatal(err)
			}
			w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) { return nil, nil }

			if tc.auto {
				w.AutoMuxEndedBroadcast(id)
			} else if err := w.MuxJob(id); err != nil {
				t.Fatal(err)
			}
			w.wg.Wait()

			if j, _ := db.GetJob(id); j == nil || j.Status != database.StatusFinished {
				t.Fatalf("muxed row = %v (error %q), want Finished", statusOf(j), errorOf(j))
			}
			g, _ := db.GetGapFill(id)
			if tc.wantKinds == "" {
				if g != nil {
					t.Errorf("a plan was kept: %s", g.Plan)
				}
				return
			}
			if g == nil {
				t.Fatal("no gap-fill plan was kept")
			}
			var p twitchGapFillPlan
			if err := json.Unmarshal([]byte(g.Plan), &p); err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for _, sp := range p.Spans {
				kinds = append(kinds, sp.Kind)
			}
			if strings.Join(kinds, ",") != tc.wantKinds || p.Login != "streamer" || p.VariantName != "chunked" {
				t.Errorf("plan %+v, want spans %s of streamer's chunked capture", p, tc.wantKinds)
			}
		})
	}
}

// TestTwitchMuxTailOpenClosesOnACancel pins owner decision FQ-2 on the rule
// itself: a Cancelled row's tail is closed whatever else it carries, and the
// close lasts only while the row is Cancelled — the Cancel is read off the
// status, which a Retry (ResumeJob, Downloading) or a Reinitialize (Upcoming)
// rewrites, so the same capture failing again after one is read by its
// markers and stream_end_time like any other row.
//
// Mutants: the Cancelled test dropped (the rows cancelled with no end time or
// with a marker read open); the end-time rule closing every unmarked row, as
// a close that outlived the Cancel would (the retried row cut off again
// reads closed).
func TestTwitchMuxTailOpenClosesOnACancel(t *testing.T) {
	end := gapStart.Add(6 * time.Minute).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		row  database.Job
		want bool
	}{
		{"cancelled while downloading", database.Job{Status: database.StatusCancelled}, false},
		{"cancelled while muxing", database.Job{Status: database.StatusCancelled, StreamEndTime: end}, false},
		{"cancelled, the ended-offline error left on it", database.Job{Status: database.StatusCancelled, Error: twitchEndedOfflineErrMsg}, false},
		{"cancelled, end unconfirmed left on it", database.Job{Status: database.StatusCancelled, ParkReason: database.ParkReasonTwitchEndUnconfirmed}, false},
		{"retried after a cancel, cut off again", database.Job{Status: database.StatusError}, true},
		{"retried after a cancel, its broadcast ended offline", database.Job{Status: database.StatusError, Error: twitchEndedOfflineErrMsg, StreamEndTime: end}, true},
		{"retried after a cancel, end unconfirmed", database.Job{Status: database.StatusError, ParkReason: database.ParkReasonTwitchEndUnconfirmed}, true},
		{"retried after a cancel, its mux failed after a normal end", database.Job{Status: database.StatusError, StreamEndTime: end}, false},
	} {
		if got := twitchMuxTailOpen(&tc.row); got != tc.want {
			t.Errorf("%s: tail open = %v, want %v", tc.name, got, tc.want)
		}
	}
}

package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestGapFillMeasuresTheVodDrift is owner decision PR-R6: with the
// broadcast's end known, the archive VOD's length is compared with the
// broadcast's span (createdAt to the last part's end), the drift logged with
// the fill, and a VOD more than gapFillDriftLimit longer or shorter is not
// cut from — the archive is kept as recorded, with the reason. With the end
// unknown the drift cannot be measured, and the fill goes on as before.
//
// The fixture's VOD is 60 s.
//
// Mutants: the drift check removed (the 8 s and the -6 s rows fill); its
// shorter side dropped (the -6 s row fills); EndKnown ignored (the end-unknown
// row is kept as drifted); the drift's log line removed; the limit raised
// past 8 s.
func TestGapFillMeasuresTheVodDrift(t *testing.T) {
	cases := []struct {
		name     string
		end      time.Duration // the last part's end, from the broadcast's start
		endKnown bool
		filled   bool
		drift    any
	}{
		{"on the wall clock", time.Minute, true, true, 0.0},
		{"4 s longer: within the limit", 56 * time.Second, true, true, 4.0},
		{"8 s longer", 52 * time.Second, true, false, 8.0},
		{"6 s shorter", 66 * time.Second, true, false, -6.0},
		{"end unknown", 52 * time.Second, false, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGapFillFixture(t)
			f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, c.end, false)},
				[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
				gapStart.Add(25*time.Hour))
			p := f.row(t)
			p.EndKnown = c.endKnown
			if err := saveTwitchGapFillPlan(f.db, p, gapStart); err != nil {
				t.Fatal(err)
			}
			row, _ := f.db.GetGapFill("tw_1")
			f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

			job, _ := f.db.GetJob("tw_1")
			o := f.outcome(t)
			if c.filled {
				if len(job.Segments) != 2 || o == nil || o.State != GapFillFilled {
					t.Errorf("segments %d, outcome %+v; want the fill", len(job.Segments), o)
				}
			} else {
				if len(job.Segments) != 0 || job.OutputFile != f.archive {
					t.Errorf("a part was cut from a drifted VOD: %+v", job.Segments)
				}
				if o == nil || o.State != GapFillKept || !strings.Contains(o.Reason, "differs from the broadcast's") {
					t.Errorf("outcome %+v, want kept with the drift as its reason", o)
				}
			}
			drift, logged := f.log.field("gap-fill: the archive VOD's length against the broadcast's", "driftSec")
			if !c.filled {
				drift, logged = f.log.field("gap-fill: the archive VOD's length differs from the broadcast's by more than 5 seconds, so its timeline cannot be trusted; the archive is kept as recorded", "driftSec")
			}
			if !logged || fmt.Sprint(drift) != fmt.Sprint(c.drift) {
				t.Errorf("logged drift %v (logged %v), want %v; lines %v", drift, logged, c.drift, f.log.lines())
			}
		})
	}
	if gapFillDriftLimit != 5*time.Second {
		t.Errorf("drift limit %v, want 5 s", gapFillDriftLimit)
	}
}

// TestGapFillPlanEndKnown: a plan knows its broadcast's end only where the
// capture saw it — ExecuteTwitch's finalize without an outage, or a Mux of an
// Error row whose capture reached that finalize (stream_end_time set) — and
// never with an open tail (owner decision PR-R6).
//
// Mutants: newTwitchGapFillPlan ignoring tailOpen; twitchMuxEndKnown true for
// a Cancelled row, for one whose Mux after a Cancel failed, or for an Error
// row without stream_end_time.
func TestGapFillPlanEndKnown(t *testing.T) {
	job := &database.Job{ID: "tw_1", StreamStartTime: gapStart.Format(time.RFC3339)}
	v := &TwitchVariantInfo{Name: "chunked", ChannelLogin: "streamer"}
	late := []stagedPartCoverage{covPart(0, 5*time.Minute, time.Hour, false)}
	if p := newTwitchGapFillPlan(job, v, late, false, true, gapStart); p == nil || !p.EndKnown {
		t.Errorf("plan %+v, want the end known", p)
	}
	if p := newTwitchGapFillPlan(job, v, late, true, true, gapStart); p == nil || p.EndKnown {
		t.Errorf("plan %+v with an open tail, want the end unknown", p)
	}
	for _, c := range []struct {
		name  string
		prior database.Job
		want  bool
	}{
		{"mux failed after a normal end", database.Job{Status: database.StatusError, StreamEndTime: "2026-10-01T13:00:00Z"}, true},
		{"cut off mid-broadcast", database.Job{Status: database.StatusError}, false},
		{"ended while down", database.Job{Status: database.StatusError, Error: twitchEndedOfflineErrMsg, StreamEndTime: "x"}, false},
		{"cancelled", database.Job{Status: database.StatusCancelled, StreamEndTime: "x"}, false},
		{"mux after a cancel failed", database.Job{Status: database.StatusError, ParkReason: database.ParkReasonMuxAfterCancel, StreamEndTime: "x"}, false},
	} {
		if got := twitchMuxEndKnown(&c.prior, twitchMuxTailOpen(&c.prior)); got != c.want {
			t.Errorf("%s: twitchMuxEndKnown = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTwitchGapFillStateOf is owner decision PR-R5's API: a Finished live
// Twitch job with a waiting plan reports pending, its spans, its deadline and
// why it waits; once the fill ends, filled and the spans recovered, or kept
// and why. A job without a row, a VOD, and a job that is not Finished report
// none.
//
// Mutants: the pending state's deadline or span count left out; the outcome
// not read (a settled row reads as pending); the Finished gate dropped (a
// re-run job shows its earlier archive's fill).
func TestTwitchGapFillStateOf(t *testing.T) {
	f := newGapFillFixture(t)
	job := func() *database.Job { j, _ := f.db.GetJob("tw_1"); return j }
	if s := TwitchGapFillStateOf(f.db, job()); s != nil {
		t.Errorf("no row: state %+v, want none", s)
	}
	deadline := gapStart.Add(25 * time.Hour)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}}, deadline)
	p := f.row(t)
	p.LastReason = "the archive VOD is still recording"
	saveTwitchGapFillPlan(f.db, p, gapStart)
	want := GapFillState{State: GapFillPending, Spans: 1, Until: deadline.Format(time.RFC3339), Reason: "the archive VOD is still recording"}
	if s := TwitchGapFillStateOf(f.db, job()); s == nil || *s != want {
		t.Errorf("pending: state %+v, want %+v", s, want)
	}

	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	if s := TwitchGapFillStateOf(f.db, job()); s == nil || *s != (GapFillState{State: GapFillFilled, Spans: 1}) {
		t.Errorf("filled: state %+v", s)
	}

	kept := &twitchGapFillPlan{JobID: "tw_1", Outcome: &gapFillOutcome{State: GapFillKept, Reason: "the archive VOD holds none of the lost spans"}}
	saveTwitchGapFillPlan(f.db, kept, gapStart)
	if s := TwitchGapFillStateOf(f.db, job()); s == nil || *s != (GapFillState{State: GapFillKept, Reason: "the archive VOD holds none of the lost spans"}) {
		t.Errorf("kept: state %+v", s)
	}

	f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusDownloading})
	if s := TwitchGapFillStateOf(f.db, job()); s != nil {
		t.Errorf("a re-run job: state %+v, want none", s)
	}
	vod := &database.Job{ID: "tw_1", Platform: "twitch", IsVod: true, Status: database.StatusFinished}
	if s := TwitchGapFillStateOf(f.db, vod); s != nil {
		t.Errorf("a VOD: state %+v, want none", s)
	}
}

// TestGapFillKeepsItsOutcomeUntilTheJobIsGone: a settled row is not a plan
// to run — a look at it fetches nothing and pushes it a week on — and it is
// dropped once its job is deleted, so rows do not outlive their jobs.
//
// Mutants: a settled row filled again at its next look; the job-gone drop
// removed (the row stays forever); the recheck not pushed (the row is due at
// every sweep).
func TestGapFillKeepsItsOutcomeUntilTheJobIsGone(t *testing.T) {
	f := newGapFillFixture(t)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	now := gapStart.Add(70 * time.Minute)
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, now)
	if o := f.outcome(t); o == nil || o.State != GapFillFilled {
		t.Fatalf("outcome %+v, want filled", o)
	}
	row, _ = f.db.GetGapFill("tw_1")
	if row.NextCheckAt != now.Add(gapFillOutcomeRecheck).Format(time.RFC3339) {
		t.Errorf("settled row due %s, want a week on", row.NextCheckAt)
	}

	fetched := f.fetched
	later := now.Add(gapFillOutcomeRecheck + time.Minute)
	if due, _ := f.db.DueGapFills(later.Format(time.RFC3339)); len(due) != 1 {
		t.Fatalf("due rows %d, want the settled one", len(due))
	}
	f.filler.sweep(context.Background(), later)
	if f.fetched != fetched {
		t.Errorf("a settled row was filled again (%d fetches)", f.fetched-fetched)
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 2 {
		t.Errorf("segments %d after the look at a settled row, want 2", len(job.Segments))
	}
	if o := f.outcome(t); o == nil || o.State != GapFillFilled || o.Spans != 1 {
		t.Errorf("outcome %+v after the look at a settled row, want it as it was", o)
	}
	if g, _ := f.db.GetGapFill("tw_1"); g == nil || g.NextCheckAt != later.Add(gapFillOutcomeRecheck).Format(time.RFC3339) {
		t.Errorf("settled row %+v, want it kept and due a week on", g)
	}

	if err := f.db.DeleteJob("tw_1"); err != nil {
		t.Fatal(err)
	}
	f.filler.sweep(context.Background(), later.Add(gapFillOutcomeRecheck+time.Minute))
	if g, _ := f.db.GetGapFill("tw_1"); g != nil {
		t.Errorf("the row outlived its job: %+v", g)
	}
	if gapFillOutcomeRecheck != 7*24*time.Hour {
		t.Errorf("recheck %v, want a week", gapFillOutcomeRecheck)
	}
}

// TestGapFillWritesToTheJobLog is owner decision PR-R5: a look's lines reach
// the Finished job's own log, which routes only tracked jobs (CORE-12), and
// the job is untracked again after — its buffer kept.
//
// Mutants: the tracking bracket removed (the job log holds nothing of the
// fill); the restore removed (the Finished job stays routed).
func TestGapFillWritesToTheJobLog(t *testing.T) {
	f := newGapFillFixture(t)
	f.filler.logger = routingLogger{db: f.db}
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	logs := strings.Join(f.db.GetJobLogs("tw_1"), "\n")
	if !strings.Contains(logs, "gap-fill: recovered a lost span from the archive VOD as its own part") ||
		!strings.Contains(logs, "gap-fill: the archive VOD's length against the broadcast's") {
		t.Errorf("job log %q, want the fill's lines", logs)
	}
	before := len(f.db.GetJobLogs("tw_1"))
	f.db.RouteLogToJobs("a later line about tw_1")
	if len(f.db.GetJobLogs("tw_1")) != before {
		t.Error("the Finished job is still routed after the look")
	}
}

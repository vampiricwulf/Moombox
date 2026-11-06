package worker

import (
	"context"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestKeptGapFillPlanIsDueAtOnce: a finished capture's plan is stored due
// now, not gapFillFirstCheck later, and the gap-fill sweep is woken, so an
// archive VOD Twitch has already completed is filled without a wait.
//
// Mutants: the plan saved for time.Now().Add(gapFillFirstCheck) (its first
// look waits 10 minutes); the kick not sent (the sweep waits for its tick).
func TestKeptGapFillPlanIsDueAtOnce(t *testing.T) {
	w, db := testWorkerSetup(t)
	if _, err := db.AddJob(&database.Job{ID: "tw_k", VideoID: "k", Platform: "twitch", Status: database.StatusFinished}); err != nil {
		t.Fatal(err)
	}
	w.orchestrator.keepTwitchGapFillPlan(&twitchGapFillPlan{JobID: "tw_k", Spans: []gapSpan{{Kind: gapKindLateStart}}})

	g, err := db.GetGapFill("tw_k")
	if err != nil || g == nil {
		t.Fatalf("the plan was not kept: %v", err)
	}
	due, err := time.Parse(time.RFC3339, g.NextCheckAt)
	if err != nil {
		t.Fatal(err)
	}
	if due.After(time.Now()) {
		t.Errorf("first look at %s, want it due at once", g.NextCheckAt)
	}
	select {
	case <-w.orchestrator.gapFillKick:
	default:
		t.Error("the gap-fill sweep was not woken")
	}
}

// TestGapFillSweepRunsOnAKick: runTwitchGapFills sweeps when kicked, without
// waiting gapFillPollInterval, and fills a plan whose VOD is complete.
//
// Mutant: the kick case removed from runTwitchGapFills' select (the plan
// waits for the 5-minute tick, past this test's deadline).
func TestGapFillSweepRunsOnAKick(t *testing.T) {
	f := newGapFillFixture(t)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		time.Now().Add(25*time.Hour))
	w := &DownloadWorker{db: f.db, logger: f.log, orchestrator: &DownloadOrchestrator{gapFillKick: make(chan struct{}, 1)}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.runTwitchGapFills(ctx, f.filler)
	}()
	t.Cleanup(func() { cancel(); <-done })

	w.orchestrator.gapFillKick <- struct{}{}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f.outcome(t) != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the kicked sweep did not fill the plan")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 2 {
		t.Errorf("segments %+v, want the recovered part and the recording", job.Segments)
	}
}

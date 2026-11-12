package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGapFillSegmentRetryPauses: a VOD segment that fails in passing is tried
// again after a pause — 1 s, 3 s, then 10 s — and the span it belongs to is
// cut once it answers, what was fetched before it kept. Back to back, a
// two-second blip spent every try and the look threw the whole fill away.
//
// Mutants: no pause between the tries (none recorded); the pauses not
// growing; the segment given up after its first failure (the span not cut).
func TestGapFillSegmentRetryPauses(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	var pauses []time.Duration
	f.filler.pause = func(_ context.Context, d time.Duration) bool { pauses = append(pauses, d); return true }
	fetch := f.filler.fetch
	fails := 3
	f.filler.fetch = func(ctx context.Context, url string) ([]byte, error) {
		if url == "https://vod.example/chunked/1.ts" && fails > 0 {
			fails--
			return nil, &httpError{StatusCode: 503}
		}
		return fetch(ctx, url)
	}
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	if want := []time.Duration{time.Second, 3 * time.Second, 10 * time.Second}; len(pauses) != 3 || pauses[0] != want[0] || pauses[1] != want[1] || pauses[2] != want[2] {
		t.Errorf("pauses %v, want %v", pauses, want)
	}
	if out := f.outcome(t); out == nil || out.State != GapFillFilled {
		t.Fatalf("outcome %+v, want the span filled once its segment answered", out)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "Streamer - Big day - recovered1.mp4"))
	if err != nil || string(data) != "MP4[v0][v1][v2]" {
		t.Errorf("recovered part %q, %v, want the three segments in order", data, err)
	}
}

// TestGapFillRefusedSegmentIsNotPausedFor: a 403 is the VOD's answer, not a
// passing failure. It is tried gapFillSegmentAttempts times with no pause and
// then left out of the span, as before.
//
// Mutant: the pause taken for every failure (a muted VOD costs 14 s a
// refused segment).
func TestGapFillRefusedSegmentIsNotPausedFor(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	paused := 0
	f.filler.pause = func(context.Context, time.Duration) bool { paused++; return true }
	f.refuse = map[string]bool{"https://vod.example/chunked/1.ts": true}
	tries := 0
	fetch := f.filler.fetch
	f.filler.fetch = func(ctx context.Context, url string) ([]byte, error) {
		if url == "https://vod.example/chunked/1.ts" {
			tries++
		}
		return fetch(ctx, url)
	}
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))

	if paused != 0 || tries != gapFillSegmentAttempts {
		t.Errorf("%d pauses and %d tries for a refused segment, want none and %d", paused, tries, gapFillSegmentAttempts)
	}
	if out := f.outcome(t); out == nil || out.State != GapFillFilled {
		t.Errorf("outcome %+v, want the rest of the span filled", out)
	}
}

// TestGapFillRetryPauseEndsWithItsContext: the wait between two tries ends
// when the worker's context does, and the fetch is not tried again.
//
// Mutant: a plain time.Sleep (a shutdown waits out the pause).
func TestGapFillRetryPauseEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	began := time.Now()
	if gapFillPause(ctx, time.Minute) {
		t.Error("the pause reported a full wait on a cancelled context")
	}
	if time.Since(began) > 10*time.Second {
		t.Error("the pause outlasted its context")
	}
	f := newGapFillFixture(t)
	tries := 0
	f.filler.fetch = func(context.Context, string) ([]byte, error) { tries++; return nil, errors.New("reset") }
	f.filler.pause = func(context.Context, time.Duration) bool { return false }
	if _, err := f.filler.fetchRetry(context.Background(), "u"); err == nil || tries != 1 {
		t.Errorf("err %v after %d tries, want an error after one", err, tries)
	}
}

// TestGapFillLookIsSkippedWhileOffline: while the connectivity monitor says
// offline a due plan is not looked at — no request, no attempt counted, its
// next look where it was — so an outage does not walk it up the backoff. A
// look that failed as the device went offline is not counted either.
//
// Mutants: the check before the look removed (requests are made and the look
// counted); the check after it removed (the second case counts a look).
func TestGapFillLookIsSkippedWhileOffline(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	online := false
	f.filler.online = func() bool { return online }
	before, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *before, gapStart.Add(70*time.Minute))

	after, _ := f.db.GetGapFill("tw_1")
	if p := f.row(t); p == nil || p.Attempts != 0 || p.CutFailures != 0 || after.NextCheckAt != before.NextCheckAt {
		t.Errorf("plan %+v, next look %s; want it untouched and still due at %s", p, after.NextCheckAt, before.NextCheckAt)
	}
	if f.fetched != 0 {
		t.Errorf("%d fetches while offline, want none", f.fetched)
	}

	// Online at the start of the look, offline by the time its segment fails.
	online = true
	f.failURL = "https://vod.example/chunked/1.ts"
	f.filler.pause = func(context.Context, time.Duration) bool { online = false; return true }
	f.filler.step(context.Background(), *before, gapStart.Add(70*time.Minute))
	after, _ = f.db.GetGapFill("tw_1")
	if p := f.row(t); p == nil || p.Attempts != 0 || p.CutFailures != 0 || after.NextCheckAt != before.NextCheckAt {
		t.Errorf("plan %+v, next look %s; want a look that failed into an outage not counted", p, after.NextCheckAt)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
		t.Errorf("the failed look left %v", matches)
	}
}

// TestGapFillCutFailureDoesNotClimbTheVodBackoff: a look that found the VOD
// complete and failed to download a span waits 10 minutes, then doubles to
// an hour at most, on a count of its own. It used to count as a look that
// found the VOD not ready, so a few failed downloads put the next try three
// hours away.
//
// Mutants: the failure counted in Attempts; gapFillCutBackoff uncapped.
func TestGapFillCutFailureDoesNotClimbTheVodBackoff(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	p := f.row(t)
	p.Attempts = 6 // the VOD took a while to complete: the next not-ready wait is 3 h
	if err := saveTwitchGapFillPlan(f.db, p, gapStart); err != nil {
		t.Fatal(err)
	}
	f.failURL = "https://vod.example/chunked/1.ts"
	row, _ := f.db.GetGapFill("tw_1")
	now := gapStart.Add(70 * time.Minute)
	f.filler.step(context.Background(), *row, now)

	p = f.row(t)
	if p == nil || p.Attempts != 6 || p.CutFailures != 1 || !strings.Contains(p.LastReason, "could not be cut") {
		t.Fatalf("plan %+v, want one cut failure, the attempts as they were", p)
	}
	g, _ := f.db.GetGapFill("tw_1")
	if want := now.Add(10 * time.Minute).Format(time.RFC3339); g.NextCheckAt != want {
		t.Errorf("next look %s, want %s", g.NextCheckAt, want)
	}
	for n, want := range map[int]time.Duration{1: 10 * time.Minute, 2: 20 * time.Minute, 3: 40 * time.Minute, 4: time.Hour, 9: time.Hour} {
		if got := gapFillCutBackoff(n); got != want {
			t.Errorf("gapFillCutBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}

// TestNewTwitchGapFillerReadsConnectivity pins the worker's filler's online
// seam: the worker's connectivity monitor, and always online without one.
//
// Mutant: newTwitchGapFiller leaving online nil with a monitor wired.
func TestNewTwitchGapFillerReadsConnectivity(t *testing.T) {
	w, _ := testWorkerSetup(t)
	w.conn = nil
	if f := w.newTwitchGapFiller(); f.offline() {
		t.Error("a worker without a connectivity monitor reads offline")
	}
	conn := &gapFillConn{}
	w.conn = conn
	f := w.newTwitchGapFiller()
	if !f.offline() {
		t.Error("the filler does not read the monitor's offline")
	}
	conn.online = true
	if f.offline() {
		t.Error("the filler does not read the monitor's online")
	}
}

type gapFillConn struct{ online bool }

func (c *gapFillConn) IsOnline() bool                  { return c.online }
func (c *gapFillConn) OnStateChange(func(bool)) func() { return func() {} }

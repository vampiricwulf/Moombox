package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
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

// TestGapFillMediaPlaylistIsRetried: the VOD's media playlist is fetched with
// the same pauses a segment is, so a passing failure there does not cost the
// look (and a step up the three-hour VOD backoff).
//
// Mutant: the playlist fetched once (the look ends "could not be read").
func TestGapFillMediaPlaylistIsRetried(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	fetch := f.filler.fetch
	fails := 2
	f.filler.fetch = func(ctx context.Context, url string) ([]byte, error) {
		if url == "https://vod.example/chunked/index-dvr.m3u8" && fails > 0 {
			fails--
			return nil, errors.New("connection reset")
		}
		return fetch(ctx, url)
	}
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	if out := f.outcome(t); out == nil || out.State != GapFillFilled {
		t.Fatalf("outcome %+v (plan %+v), want the span filled once the playlist answered", out, f.row(t))
	}
}

// TestGapFillCommitFailureIsACutFailure: a look that cut every span and then
// could not record the parts (the job left Finished while it downloaded)
// found the VOD complete, so it waits on the cut backoff, not the VOD's.
//
// Mutant: the commit failure counted in Attempts.
func TestGapFillCommitFailureIsACutFailure(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	mux := f.filler.mux
	f.filler.mux = func(ctx context.Context, ts, out string) error {
		f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusMuxing})
		return mux(ctx, ts, out)
	}
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	p := f.row(t)
	if p == nil || p.Attempts != 0 || p.CutFailures != 1 || !strings.Contains(p.LastReason, "could not be recorded") {
		t.Fatalf("plan %+v, want one cut failure and no attempt counted", p)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
		t.Errorf("the failed look left %v", matches)
	}
}

// TestGapFillLastLookWaitsOutAnUnreportedOutage (owner ruling R2-F1): the
// look past the deadline settles the plan for good, and the connectivity
// monitor needs a poll or two to report an outage. A last look whose Twitch
// request failed outright waits once and asks the monitor again: offline by
// then, the plan stays due and uncounted; still online, it is settled.
//
// Mutants: no second reading (the plan is settled inside the outage's first
// seconds); the wait taken for a look that reached Twitch (a VOD that is
// still recording costs every last look 14 s).
func TestGapFillLastLookWaitsOutAnUnreportedOutage(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	online := true
	f.filler.online = func() bool { return online }
	var waits []time.Duration
	f.filler.pause = func(_ context.Context, d time.Duration) bool { waits = append(waits, d); online = false; return true }
	down := true
	f.filler.findVod = func(context.Context, string, time.Time, time.Time) (string, error) {
		if down {
			return "", errors.New("dial tcp: no route to host")
		}
		return f.vodID, nil
	}
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(26*time.Hour))
	after, _ := f.db.GetGapFill("tw_1")
	if p := f.row(t); p == nil || p.Attempts != 0 || after.NextCheckAt != row.NextCheckAt {
		t.Fatalf("plan %+v, want it still due and uncounted after a last look that failed into an outage", p)
	}
	if len(waits) != 1 || waits[0] != 14*time.Second {
		t.Errorf("waits %v, want one of 14 s", waits)
	}

	// Still online after the wait: the failure was Twitch's, and the window
	// is final.
	waits = nil
	online = true
	f.filler.pause = func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }
	f.filler.step(context.Background(), *row, gapStart.Add(26*time.Hour))
	if out := f.outcome(t); out == nil || out.State != GapFillKept || !strings.Contains(out.Reason, "lookup failed") {
		t.Fatalf("outcome %+v, want the archive kept with the lookup's failure", out)
	}
	if len(waits) != 1 {
		t.Errorf("waits %v, want one", waits)
	}
}

// TestGapFillLastLookDoesNotWaitWhenTwitchAnswered: a last look that reached
// Twitch (the VOD is still recording) is settled without the outage wait.
func TestGapFillLastLookDoesNotWaitWhenTwitchAnswered(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	f.vodInfo.IsRecording = true
	waited := 0
	f.filler.pause = func(context.Context, time.Duration) bool { waited++; return true }
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(26*time.Hour))
	if out := f.outcome(t); out == nil || out.State != GapFillKept || waited != 0 {
		t.Fatalf("outcome %+v after %d waits, want it kept with no wait", out, waited)
	}
}

// TestGapFillLastLookIsNotSettledByAMuxThatBeganDuringIt: the look past the
// deadline settles the plan for good, but not when what failed it is a mux
// that began on the job while it downloaded (the operator's Mux of a
// Finished archive, which refuses the look's commit). The row is then that
// mux's to keep or drop, as it is for a look that finds the job Muxing: the
// plan stays due and uncounted, and the look after the mux is the last one.
//
// Mutant: the re-read before the settle removed (the plan is kept as
// recorded because of the Mux).
func TestGapFillLastLookIsNotSettledByAMuxThatBeganDuringIt(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	mux := f.filler.mux
	f.filler.mux = func(ctx context.Context, ts, out string) error {
		f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusMuxing})
		return mux(ctx, ts, out)
	}
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(26*time.Hour))
	after, _ := f.db.GetGapFill("tw_1")
	if p := f.row(t); p == nil || p.Outcome != nil || p.CutFailures != 0 || after.NextCheckAt != row.NextCheckAt {
		t.Fatalf("plan %+v, want it still due, unsettled and uncounted while the job is Muxing", p)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
		t.Errorf("the refused look left %v", matches)
	}

	// The mux over and the job Finished again, the next look is the last.
	f.filler.mux = mux
	f.db.UpdateJobFields("tw_1", map[string]any{"status": database.StatusFinished})
	f.filler.step(context.Background(), *after, gapStart.Add(26*time.Hour))
	if out := f.outcome(t); out == nil || out.State != GapFillFilled {
		t.Fatalf("outcome %+v, want the fill done by the look after the mux", out)
	}
}

// TestGapFillLastLookHeldOfflineSaysSo: a last look that waits for the device
// to come back online records why, once, with its next look where it was —
// the job's details would otherwise show a pending fill "until" a time
// already past beside a reason from hours earlier. Inside the window an
// offline sweep still leaves the plan untouched.
//
// Mutants: the reason not saved; the save moving the next look.
func TestGapFillLastLookHeldOfflineSaysSo(t *testing.T) {
	f := newGapFillFixture(t)
	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	f.filler.online = func() bool { return false }
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	if p := f.row(t); p == nil || p.LastReason == gapFillOfflineReason {
		t.Fatalf("plan %+v, want a look inside the window left untouched while offline", p)
	}
	f.filler.step(context.Background(), *row, gapStart.Add(26*time.Hour))
	after, _ := f.db.GetGapFill("tw_1")
	p := f.row(t)
	if p == nil || p.Outcome != nil || p.Attempts != 0 || p.LastReason != gapFillOfflineReason || after.NextCheckAt != row.NextCheckAt {
		t.Fatalf("plan %+v, next look %s; want it due at %s, uncounted, with the offline reason", p, after.NextCheckAt, row.NextCheckAt)
	}
	if f.fetched != 0 {
		t.Errorf("%d fetches while offline, want none", f.fetched)
	}
}

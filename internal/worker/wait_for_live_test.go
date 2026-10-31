package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The waiting loop driven for real: waitForLive inside a synctest bubble, its
// YouTube calls answered by the processor's seams (probeStatusFn,
// probeScheduleFn, getVideoInfoFn). The bubble's clock moves only while every
// goroutine in it is blocked, so hours of polling run in milliseconds, and
// every call one loop iteration makes lands on the same instant — which is
// how the tests tell one tick from the next.

// waitCall is one YouTube call the loop made: which, and when on the
// bubble's clock, counted from the start of the wait.
type waitCall struct {
	at   time.Duration
	kind string // "status", "status_auth", "schedule" or "full"
}

// waitFakes answers the loop's three calls. Each func gets the time since
// the wait began; a nil one answers as a stream still waiting on its start.
type waitFakes struct {
	status   func(at time.Duration) (*youtube.VideoInfo, error)
	schedule func(at time.Duration) (*youtube.VideoInfo, error)
	full     func(at time.Duration) (*youtube.VideoInfo, error)
	// signedIn gives the processor's service a jar holding an account, so
	// the B1 switch to the cookied probe can fire; empty otherwise.
	signedIn bool
}

// waitRun is what one driven wait did.
type waitRun struct {
	start    time.Time // the job's scheduled start, on the bubble's clock
	result   *StreamProcessResult
	returned time.Duration // when waitForLive returned; -1 when it was still waiting at the budget
	calls    []waitCall
	row      *database.Job // the row once the wait was over
	rec      *notificationtest.Recorder
}

// upcomingAnswer is what YouTube says about a stream still waiting to start
// at start: the status probes' shape when start is zero (they carry no
// schedule), the schedule probe's and the full fetch's otherwise.
func upcomingAnswer(start time.Time) *youtube.VideoInfo {
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, IsUpcoming: true, PlayabilityError: youtube.PlayabilityOK}
	if !start.IsZero() {
		info.ScheduledStartTime = start.UTC().Format(time.RFC3339)
	}
	return info
}

// liveAnswer is a stream that has begun, as the WEB-family calls see it.
func liveAnswer(start time.Time) *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: youtube.StreamLive, IsLive: true, PlayabilityError: youtube.PlayabilityOK,
		ScheduledStartTime: start.UTC().Format(time.RFC3339)}
}

// driveWaitForLive runs waitForLive on an Upcoming row scheduled startIn from
// now, for at most budget of the bubble's time, with fakes(start) answering
// its calls. Chat capture is off: the loop's YouTube calls are all the test
// is about.
func driveWaitForLive(t *testing.T, startIn, budget time.Duration, fakes func(start time.Time) waitFakes) waitRun {
	t.Helper()
	var run waitRun
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Now()
		run.start = t0.Add(startIn).UTC()
		f := fakes(run.start)

		db, err := database.Open(filepath.Join(t.TempDir(), "wait.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		defer db.Close()
		chID := "UC_wait"
		job := &database.Job{ID: "wait1", VideoID: "wait1", Platform: "youtube", Title: "T", ChannelName: "A Channel",
			Status: database.StatusUpcoming, ChannelID: &chID, StreamStartTime: run.start.Format(time.RFC3339)}
		if _, err := db.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		stored, err := db.GetJob(job.ID)
		if err != nil || stored == nil {
			t.Fatalf("GetJob: %v", err)
		}

		cfg := &config.MoomboxConfig{}
		cfg.Downloader.DownloadChat = false
		// A real service on an empty jar answers the B1 auth-flip check (no
		// cookies), or on a signed-in one when the test asks; every call
		// that would reach the network goes through a seam below.
		jar := cookies.NewCookieJar()
		if f.signedIn {
			path := filepath.Join(t.TempDir(), "cookies.txt")
			if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"+
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tsapisid-a\n"+
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tlogininfo-a\n"), 0o600); err != nil {
				t.Fatalf("write cookie file: %v", err)
			}
			if err := jar.Load(path); err != nil {
				t.Fatalf("load cookie file: %v", err)
			}
		}
		sp := NewStreamProcessor(youtube.NewService(jar, discardLogger{}), nil, cfg, db, discardLogger{})
		if f.signedIn && !sp.yt.Auth.HasAuthCookies() {
			t.Fatal("setup: the signed-in jar holds no account")
		}
		run.rec = notificationtest.New()
		sp.SetNotifier(run.rec)

		var mu sync.Mutex
		answer := func(kind string, fake func(time.Duration) (*youtube.VideoInfo, error), dflt *youtube.VideoInfo) (*youtube.VideoInfo, error) {
			at := time.Since(t0)
			mu.Lock()
			run.calls = append(run.calls, waitCall{at: at, kind: kind})
			mu.Unlock()
			if fake == nil {
				return dflt, nil
			}
			return fake(at)
		}
		sp.probeStatusFn = func(_ context.Context, _ string, authenticated bool) (*youtube.VideoInfo, error) {
			kind := "status"
			if authenticated {
				kind = "status_auth" // the cookied probe, once B1 has switched to it
			}
			return answer(kind, f.status, upcomingAnswer(time.Time{}))
		}
		sp.probeScheduleFn = func(context.Context, string) (*youtube.VideoInfo, error) {
			return answer("schedule", f.schedule, upcomingAnswer(run.start))
		}
		sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
			return answer("full", f.full, upcomingAnswer(run.start))
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type outcome struct {
			res *StreamProcessResult
			at  time.Duration
		}
		done := make(chan outcome, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("waitForLive panicked: %v", r)
					done <- outcome{at: time.Since(t0)}
				}
			}()
			res, err := sp.waitForLive(ctx, stored, upcomingAnswer(run.start))
			if err != nil {
				t.Errorf("waitForLive: %v", err)
			}
			done <- outcome{res: res, at: time.Since(t0)}
		}()
		select {
		case o := <-done:
			run.result, run.returned = o.res, o.at
		case <-time.After(budget):
			cancel()
			<-done
			run.returned = -1
		}

		mu.Lock()
		run.calls = append([]waitCall(nil), run.calls...)
		mu.Unlock()
		if run.row, err = db.GetJob(job.ID); err != nil || run.row == nil {
			t.Fatalf("GetJob after the wait: %v", err)
		}
	})
	return run
}

// ticks groups the calls by the instant they were made: one group per loop
// iteration that reached YouTube, in order.
func (r waitRun) ticks() [][]waitCall {
	var out [][]waitCall
	for i, c := range r.calls {
		if i == 0 || c.at != r.calls[i-1].at {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], c)
	}
	return out
}

// count is how many of the calls were of kind.
func (r waitRun) count(kind string) int {
	n := 0
	for _, c := range r.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// String lists the calls, for a failure message.
func (r waitRun) String() string {
	s := ""
	for _, tick := range r.ticks() {
		s += fmt.Sprintf("\n  %v:", tick[0].at.Round(time.Second))
		for _, c := range tick {
			s += " " + c.kind
		}
	}
	return s
}

// A start that moves reaches the ROW through the schedule probe, within
// minutes rather than on the half-hour full fetch, and is announced once:
// every later probe that repeats the moved start writes and sends nothing.
// Fifty minutes out, the probe asks every ~4 minutes, on the 5-minute
// status-probe ticks; the full fetch, 30 minutes in, never comes within the
// 20 minutes driven here, so the schedule probe is the only thing that can
// have seen the move.
//
// Mutants: the loop syncing scheduledStartTime but not the row (the
// updateJobMetadata call dropped); the schedule-probe block removed; its
// answer written only when it is NOT upcoming.
func TestWaitingLoopWritesAMovedStartToTheRow(t *testing.T) {
	const moveBy = 15 * time.Minute
	run := driveWaitForLive(t, 50*time.Minute, 20*time.Minute, func(start time.Time) waitFakes {
		return waitFakes{schedule: func(time.Duration) (*youtube.VideoInfo, error) {
			return upcomingAnswer(start.Add(moveBy)), nil
		}}
	})
	if run.returned >= 0 {
		t.Fatalf("waitForLive returned %+v at %v; the stream never began", run.result, run.returned)
	}
	if got := run.count("schedule"); got < 2 {
		t.Fatalf("the schedule probe ran %d times in 20 minutes, want at least 2 (one move, then repeats):%s", got, run)
	}
	if run.count("full") != 0 {
		t.Fatalf("a full fetch ran inside 20 minutes, so the schedule probe is not what saw the move:%s", run)
	}
	moved := run.start.Add(moveBy).Format(time.RFC3339)
	if !sameInstant(run.row.StreamStartTime, moved) {
		t.Errorf("row start = %q after 20 minutes, want the moved start %q", run.row.StreamStartTime, moved)
	}
	if got := run.rec.ByEvent("rescheduled"); len(got) != 1 {
		t.Errorf("recorded %d \"rescheduled\" embeds for one move, want 1: %+v", len(got), got)
	}
}

// A schedule probe that says the stream has begun takes the status probe's
// place in the loop's switch: the live arm confirms with a full fetch and the
// wait ends on that tick, although the status probe still reads the waiting
// room. One that says not_a_stream — what a refused WEB call reads as — does
// not: the switch's default arm would end the job on it, so the loop goes on
// waiting.
//
// Mutants: the hand-off (probeInfo = sched) dropped (the wait runs to the
// budget); scheduleProbeOvertakes widened to anything but upcoming (the
// refused answer ends the wait with "unexpected status").
func TestWaitingLoopActsOnABegunScheduleAnswer(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		run := driveWaitForLive(t, 10*time.Minute, time.Hour, func(start time.Time) waitFakes {
			return waitFakes{
				schedule: func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil },
				full:     func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil },
			}
		})
		if run.returned < 0 || run.result == nil || !run.result.ShouldDownload || run.result.IsVod {
			t.Fatalf("the wait ended with %+v at %v, want a live download:%s", run.result, run.returned, run)
		}
		ticks := run.ticks()
		last := ticks[len(ticks)-1]
		if len(last) != 3 || last[0].kind != "status" || last[1].kind != "schedule" || last[2].kind != "full" {
			t.Errorf("the last tick made %+v, want status, schedule, then the live arm's confirming full fetch:%s", last, run)
		}
		if run.row.Status != database.StatusLive {
			t.Errorf("row status = %q, want %q", run.row.Status, database.StatusLive)
		}
	})

	t.Run("refused", func(t *testing.T) {
		run := driveWaitForLive(t, 10*time.Minute, time.Hour, func(time.Time) waitFakes {
			return waitFakes{schedule: func(time.Duration) (*youtube.VideoInfo, error) {
				return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityLoginRequired}, nil
			}}
		})
		if run.returned >= 0 {
			t.Fatalf("a refused schedule answer ended the wait with %+v at %v, want it to go on waiting:%s", run.result, run.returned, run)
		}
		if run.count("schedule") == 0 {
			t.Fatalf("the schedule probe never ran, so nothing was tested:%s", run)
		}
		if run.row.Status != database.StatusUpcoming {
			t.Errorf("row status = %q, want %q", run.row.Status, database.StatusUpcoming)
		}
	})
}

// kinds is the order of the calls one tick made, for comparison.
func kinds(tick []waitCall) string {
	s := ""
	for i, c := range tick {
		if i > 0 {
			s += ","
		}
		s += c.kind
	}
	return s
}

// The schedule probe asks after the periodic full fetch, so on the tick the
// fetch comes due, a fetch that succeeds — it reads the schedule too — has
// already reset the probe's clock. Eight hours out the probe's cadence is
// capped at the fetch's own 30 minutes, and asking first sent one cookied
// WEB call every half hour just ahead of a fetch that read the same
// schedule again: a start six hours or more away now costs no call the full
// fetch was not already making.
//
// Mutants: the schedule-probe block back ahead of the full fetch; the full
// fetch no longer resetting the probe's clock (either way, a probe on every
// full-fetch tick).
func TestSuccessfulFullFetchStandsInForTheScheduleProbe(t *testing.T) {
	run := driveWaitForLive(t, 8*time.Hour, 2*time.Hour, func(time.Time) waitFakes { return waitFakes{} })
	if got := run.count("full"); got < 3 {
		t.Fatalf("%d full fetches in 2 hours, want at least 3:%s", got, run)
	}
	if got := run.count("schedule"); got != 0 {
		t.Errorf("%d schedule probe(s) with the start 8 h away, each on a tick whose full fetch read the schedule, want 0:%s", got, run)
	}
}

// A full fetch that FAILS leaves the schedule probe due, and the probe asks
// on that same tick, after it: the fallback that keeps a reschedule seen when
// the watch page will not load.
//
// Mutants: the probe skipped on any tick the full fetch ran (none on the
// failed tick); the probe back ahead of the full fetch (the order is wrong).
func TestScheduleProbeCoversAFailedFullFetch(t *testing.T) {
	run := driveWaitForLive(t, 8*time.Hour, 35*time.Minute, func(time.Time) waitFakes {
		return waitFakes{full: func(time.Duration) (*youtube.VideoInfo, error) {
			return nil, errors.New("innertube: 503")
		}}
	})
	ticks := run.ticks()
	if got := kinds(ticks[len(ticks)-1]); got != "status,full,schedule" {
		t.Errorf("the full-fetch tick made %s, want status,full,schedule:%s", got, run)
	}
}

// The full fetch on its tick is the WEB view the schedule probe would have
// been, so an answer from it that says the stream has begun goes to the
// switch the same way: the live arm confirms and the wait ends on that tick,
// though the status probe still reads the waiting room. With the probe asked
// after the fetch, a successful fetch takes the probe's place on that tick,
// and without this the hand-off would wait for the next probe.
//
// Mutant: the full fetch's begun answer not handed to the switch (the wait
// runs to the budget).
func TestFullFetchThatSeesTheStreamBegunEndsTheWait(t *testing.T) {
	run := driveWaitForLive(t, 8*time.Hour, 45*time.Minute, func(start time.Time) waitFakes {
		return waitFakes{full: func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil }}
	})
	if run.returned < 0 || run.result == nil || !run.result.ShouldDownload {
		t.Fatalf("the wait ended with %+v at %v, want a live download on the full-fetch tick:%s", run.result, run.returned, run)
	}
	ticks := run.ticks()
	if got := kinds(ticks[len(ticks)-1]); got != "status,full,full" {
		t.Errorf("the last tick made %s, want status, the periodic full fetch, then the live arm's confirming one:%s", got, run)
	}
}

// endedAnswer is a broadcast that is over, as the WEB-family calls see it:
// post_live, its schedule field carrying the ACTUAL start.
func endedAnswer(actualStart time.Time) *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayabilityError: youtube.PlayabilityOK,
		ScheduledStartTime: actualStart.UTC().Format(time.RFC3339)}
}

// A stream the waiting loop finds already over is not announced as
// rescheduled. Its answer carries the broadcast's actual start in the
// schedule field, and the row takes it; but a start that differed from the
// schedule — a stream begun five minutes late — sent "YouTube Schedule
// Changed", Old Time the schedule and New Time the start, because !IsLive
// was all the notice asked and a post_live or VOD answer is not live. The
// periodic full fetch that saw the stream ended sent it, and so did the
// go-live transition's write when the status probe saw it first. The notice
// is for a stream still upcoming.
//
// Mutant: the notice gated on !IsLive alone again (both runs send one).
func TestAnEndedStreamIsNotAnnouncedAsRescheduled(t *testing.T) {
	const late = 5 * time.Minute
	for _, tc := range []struct {
		name            string
		startIn, budget time.Duration
		fakes           func(start time.Time) waitFakes
	}{
		{"the periodic full fetch sees it ended", 10 * time.Minute, 45 * time.Minute, func(start time.Time) waitFakes {
			return waitFakes{full: func(time.Duration) (*youtube.VideoInfo, error) { return endedAnswer(start.Add(late)), nil }}
		}},
		{"the status probe sees it ended", 10 * time.Minute, 10 * time.Minute, func(start time.Time) waitFakes {
			return waitFakes{
				status: func(time.Duration) (*youtube.VideoInfo, error) {
					return &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayabilityError: youtube.PlayabilityOK}, nil
				},
				full: func(time.Duration) (*youtube.VideoInfo, error) { return endedAnswer(start.Add(late)), nil },
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, tc.startIn, tc.budget, tc.fakes)
			if run.returned < 0 || run.result == nil || !run.result.ShouldDownload || !run.result.IsVod {
				t.Fatalf("the wait ended with %+v at %v, want the ended stream downloaded:%s", run.result, run.returned, run)
			}
			actual := run.start.Add(late).Format(time.RFC3339)
			if !sameInstant(run.row.StreamStartTime, actual) {
				t.Errorf("row start = %q, want the broadcast's actual start %q", run.row.StreamStartTime, actual)
			}
			if got := run.rec.ByEvent("rescheduled"); len(got) != 0 {
				t.Errorf("an ended stream was announced as rescheduled: %+v", got)
			}
		})
	}
}

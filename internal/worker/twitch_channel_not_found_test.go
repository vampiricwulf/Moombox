package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// G53, offline: a manually added Twitch login that Twitch says does not exist
// no longer waits forever. waitForTwitchLive runs inside a synctest bubble, so
// its 15-20 s polls pass in no real time, and its lookups are answered by the
// twitchLookupFn seam from a script, one answer per poll.

// twitchAnswer is one scripted lookup outcome.
type twitchAnswer int

const (
	twMissing   twitchAnswer = iota // (nil, ErrChannelNotFound): user:null
	twOffline                       // (nil, nil): the login resolves, no stream
	twNetErr                        // a failure to reach Twitch at all
	twServerErr                     // a definitive refusal (classServer): counted
	twLive                          // the channel is live
)

// twitchWaitRun is what one driven wait did.
type twitchWaitRun struct {
	info  *twitch.TwitchStreamInfo
	err   error
	polls int  // lookups made
	ended bool // false: still waiting when the budget ran out
	row   *database.Job
}

// driveTwitchWait runs waitForTwitchLive on a manual job for at most an hour
// of the bubble's time, answering poll i with script[i] and every poll past the
// script's end with twMissing.
func driveTwitchWait(t *testing.T, script ...twitchAnswer) twitchWaitRun {
	t.Helper()
	return driveTwitchWaitFor(t, "nosuchstreamer", script...)
}

// driveTwitchWaitFor is driveTwitchWait for a login the test names.
func driveTwitchWaitFor(t *testing.T, login string, script ...twitchAnswer) twitchWaitRun {
	t.Helper()
	var run twitchWaitRun
	synctest.Test(t, func(t *testing.T) {
		db, err := database.Open(filepath.Join(t.TempDir(), "twitch-wait.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		defer db.Close()
		jobID := "tw_manual_" + login + "_1"
		job := &database.Job{ID: jobID, VideoID: jobID, URL: "https://www.twitch.tv/" + login,
			Platform: "twitch", Status: database.StatusUpcoming, ManuallyAdded: true, QualityPreference: "best"}
		if _, err := db.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		stored, err := db.GetJob(jobID)
		if err != nil || stored == nil {
			t.Fatalf("GetJob: %v", err)
		}

		sp := &StreamProcessor{db: db, cfg: config.Defaults(), twitchHints: newTwitchHintCache(), logger: discardLogger{}}
		polls := 0
		sp.twitchLookupFn = func(_ context.Context, got string) (*twitch.TwitchStreamInfo, error) {
			if got != login {
				t.Errorf("lookup for %q, want %q", got, login)
			}
			answer := twMissing
			if polls < len(script) {
				answer = script[polls]
			}
			polls++
			switch answer {
			case twOffline:
				return nil, nil
			case twNetErr:
				return nil, fmt.Errorf("gql request (op): %w", &netTimeoutError{})
			case twServerErr:
				return nil, fmt.Errorf("gql http 404")
			case twLive:
				return &twitch.TwitchStreamInfo{IsLive: true, StreamID: "42", ChannelLogin: login}, nil
			default:
				return nil, twitch.ErrChannelNotFound
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type outcome struct {
			info *twitch.TwitchStreamInfo
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("waitForTwitchLive panicked: %v", r)
					done <- outcome{}
				}
			}()
			info, err := sp.waitForTwitchLive(ctx, stored, login)
			done <- outcome{info, err}
		}()
		select {
		case o := <-done:
			run.info, run.err, run.ended = o.info, o.err, true
		case <-time.After(time.Hour):
			cancel()
			<-done
		}
		run.polls = polls
		if run.row, err = db.GetJob(jobID); err != nil || run.row == nil {
			t.Fatalf("GetJob after the wait: %v", err)
		}
	})
	return run
}

// netTimeoutError is a net.Error timeout: the shape a GQL request that never
// reached Twitch fails with, which the wait counts against nothing.
type netTimeoutError struct{}

func (*netTimeoutError) Error() string   { return "i/o timeout" }
func (*netTimeoutError) Timeout() bool   { return true }
func (*netTimeoutError) Temporary() bool { return true }

// repeat is n copies of a.
func repeat(a twitchAnswer, n int) []twitchAnswer {
	out := make([]twitchAnswer, n)
	for i := range out {
		out[i] = a
	}
	return out
}

// TestTwitchWaitGivesUpOnALoginThatDoesNotExist is the wave's third offline
// test, the wait-loop half: twitchNotFoundGiveUp "does not exist" answers in a
// row end the wait with an error naming the login, carrying both
// twitch.ErrChannelNotFound and ErrNonActionable (setJobError's suppression,
// as the probe budget's give-up carries), and the waiting line is cleared.
//
// Mutant: the ErrChannelNotFound arm deleted — the answer falls to
// applyProbeError, which classes the sentinel's text as network (never
// counted), and the wait polls on to the hour's end.
func TestTwitchWaitGivesUpOnALoginThatDoesNotExist(t *testing.T) {
	run := driveTwitchWait(t)
	if !run.ended {
		t.Fatalf("still waiting after an hour of \"does not exist\" answers (%d polls)", run.polls)
	}
	if run.polls != twitchNotFoundGiveUp {
		t.Errorf("gave up after %d polls, want %d", run.polls, twitchNotFoundGiveUp)
	}
	if !errors.Is(run.err, twitch.ErrChannelNotFound) || !errors.Is(run.err, ErrNonActionable) {
		t.Errorf("err = %v, want twitch.ErrChannelNotFound and ErrNonActionable", run.err)
	}
	if run.err == nil || !strings.HasPrefix(run.err.Error(), "nosuchstreamer does not exist on Twitch") {
		t.Errorf("err = %v, want it to lead with the login, as yt-dlp's \"<channel> does not exist\" does", run.err)
	}
	if run.row.Progress != "" {
		t.Errorf("progress = %q after the give-up, want the waiting line cleared", run.row.Progress)
	}
}

// TestTwitchWaitCountsOnlyNotFoundAnswersInARow: the give-up counts
// CONSECUTIVE answers. One that resolves the login — offline here — starts the
// count again, and a failed poll is no answer at all and leaves it as it was.
//
// Mutants: the count not reset on a resolved answer (the first row gives up at
// poll 10); the count reset on a failed poll (the second row is still waiting
// at its 13th poll, which must be its last).
func TestTwitchWaitCountsOnlyNotFoundAnswersInARow(t *testing.T) {
	t.Run("a resolved answer starts the count again", func(t *testing.T) {
		script := append(repeat(twMissing, twitchNotFoundGiveUp-1), twOffline)
		script = append(script, repeat(twMissing, twitchNotFoundGiveUp-1)...)
		script = append(script, twLive)
		run := driveTwitchWait(t, script...)
		if !run.ended || run.err != nil || run.info == nil || run.info.StreamID != "42" {
			t.Fatalf("wait = %+v, %v after %d polls (ended %v); want the live stream at poll %d",
				run.info, run.err, run.polls, run.ended, len(script))
		}
	})
	t.Run("a failed poll is no answer", func(t *testing.T) {
		script := append(repeat(twMissing, 5), repeat(twNetErr, 3)...)
		run := driveTwitchWait(t, script...)
		if !run.ended || !errors.Is(run.err, twitch.ErrChannelNotFound) {
			t.Fatalf("wait = %+v, %v after %d polls; want the not-found give-up", run.info, run.err, run.polls)
		}
		if want := twitchNotFoundGiveUp + 3; run.polls != want {
			t.Errorf("gave up after %d polls, want %d — the three failed polls neither count nor reset", run.polls, want)
		}
	})
}

// TestTwitchStreamLookupIsTheNotFoundAwareLookup pins the seam's production
// value: with no twitchLookupFn, waitForTwitchLive's lookup is the real
// Service's LookupStreamInfo, which keeps user:null as ErrChannelNotFound. A
// production arm that called GetStreamInfo would compile, pass every scripted
// test above, and make the give-up unreachable — its (nil, nil) is offline.
//
// Real Service, empty jar, GQL pointed at a local server (no fake clock, so an
// httptest server outside a bubble).
//
// Mutant: the production arm calling sp.tw.GetStreamInfo — (nil, nil) back.
func TestTwitchStreamLookupIsTheNotFoundAwareLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"data":{"user":null}},{"data":{"user":{"stream":null}}}]`)
	}))
	defer srv.Close()
	prev := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = srv.URL
	t.Cleanup(func() { constants.TwitchURLs.GQL = prev })

	sp := &StreamProcessor{tw: twitch.NewService(cookies.NewCookieJar(), discardLogger{}), logger: discardLogger{}}
	info, err := sp.twitchStreamLookup(context.Background(), "nosuchstreamer")
	if info != nil || !errors.Is(err, twitch.ErrChannelNotFound) {
		t.Errorf("twitchStreamLookup = %+v, %v; want twitch.ErrChannelNotFound", info, err)
	}
}

// probeTagReporter counts what the connectivity oracle is told, per verdict.
// Only the waiting goroutine reports, and the counts are read after it ended.
type probeTagReporter struct{ fails, oks int }

func (r *probeTagReporter) ReportFailure(string) { r.fails++ }
func (r *probeTagReporter) ReportSuccess(string) { r.oks++ }

// TestTwitchWaitNotFoundIsAnAnswerToTheBudgetAndTheOracle: a "does not exist"
// answer is Twitch answering, so it resets the definitive-error budget —
// which counts failures to GET an answer — and is reported to the
// connectivity oracle as a success.
//
// Mutants: `consecutiveErrors = 0` deleted from the not-found arm (nine
// server errors, the not-found answer, one more server error reach
// maxConsecutiveProbeErrors and the wait gives up before the live answer);
// the arm's reportProbeResult deleted (no success reported for the ten
// not-found answers).
func TestTwitchWaitNotFoundIsAnAnswerToTheBudgetAndTheOracle(t *testing.T) {
	t.Run("the budget", func(t *testing.T) {
		script := append(repeat(twServerErr, maxConsecutiveProbeErrors-1), twMissing, twServerErr, twLive)
		run := driveTwitchWait(t, script...)
		if !run.ended || run.err != nil || run.info == nil || run.info.StreamID != "42" {
			t.Fatalf("wait = %+v, %v after %d polls; want the live stream at poll %d — the not-found answer resets the error budget",
				run.info, run.err, run.polls, len(script))
		}
	})
	t.Run("the oracle", func(t *testing.T) {
		rep := &probeTagReporter{}
		t.Cleanup(func() { SetConnectivityReporter(nil) })
		SetConnectivityReporter(rep)
		run := driveTwitchWait(t)
		SetConnectivityReporter(nil)
		if !run.ended {
			t.Fatalf("still waiting after %d polls", run.polls)
		}
		if rep.oks != twitchNotFoundGiveUp || rep.fails != 0 {
			t.Errorf("oracle told %d successes, %d failures; want %d successes, none failed — each not-found answer reached Twitch",
				rep.oks, rep.fails, twitchNotFoundGiveUp)
		}
	})
}

// TestTwitchNotFoundGiveUpIsNoTransportFailure: the give-up error leads with
// the login, which is free text, and the worker's failure path reads that
// error through isTransportFailure (outageCalled, markTwitchUnreachable) and
// parkReasonForError. A login holding "eof", "tls" or "timeout" must still
// read as Twitch's answer: no transport failure, no unreachable wrapper, no
// park reason.
//
// Mutant: the twitch.ErrChannelNotFound arm deleted from probeVote — the
// string fallback reads "theofficialfoo" as an EOF, isTransportFailure is
// true and the row parks as ParkReasonTwitchUnreachable; the same arm
// deleted from classifyProbeErr — the give-up reads as classNetwork.
func TestTwitchNotFoundGiveUpIsNoTransportFailure(t *testing.T) {
	for _, login := range []string{"theofficialfoo", "tlsgamer", "timeoutking"} {
		t.Run(login, func(t *testing.T) {
			run := driveTwitchWaitFor(t, login)
			if !run.ended || !errors.Is(run.err, twitch.ErrChannelNotFound) {
				t.Fatalf("wait = %+v, %v after %d polls; want the not-found give-up", run.info, run.err, run.polls)
			}
			if !strings.HasPrefix(run.err.Error(), login+" does not exist on Twitch") {
				t.Fatalf("err = %v, want it to lead with the login (the case this test is about)", run.err)
			}
			if isTransportFailure(run.err) {
				t.Errorf("isTransportFailure(%q) = true, want false — Twitch answered", run.err)
			}
			if got := probeVote(run.err); got != reportSuccess {
				t.Errorf("probeVote = %v, want reportSuccess", got)
			}
			if got := classifyProbeErr(run.err); got != classServer {
				t.Errorf("classifyProbeErr = %v, want classServer", got)
			}
			w := &DownloadWorker{}
			job := &database.Job{ID: "tw_manual_" + login + "_1", VideoID: "tw_manual_" + login + "_1", Platform: "twitch"}
			marked := w.markTwitchUnreachable(job, run.err)
			if _, ok := errors.AsType[*twitchUnreachableError](marked); ok {
				t.Errorf("markTwitchUnreachable wrapped the not-found give-up as unreachable")
			}
			if got := parkReasonForError(marked); got != database.ParkReasonNone {
				t.Errorf("parkReasonForError = %q, want none", got)
			}
		})
	}
}

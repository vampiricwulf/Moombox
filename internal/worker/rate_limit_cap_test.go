package worker

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The 24-hour cap on a spell of rate-limit refusals (rateLimitCap,
// rate_limit.go), on the bubble's clock: a day of an hourly requeue, of a
// half-hourly wait in place, or of a waiting loop's refused full fetches
// ends the job in Error with the refusal as yt-dlp words it.

// runInBubble runs processJob for id on its own goroutine and returns once it
// does, or, still running at budget, cancels it and reports false.
func runInBubble(t *testing.T, w *DownloadWorker, id string, budget time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("processJob panicked: %v", r)
			}
		}()
		w.processJob(ctx, id)
	}()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		cancel()
		<-done
		return false
	}
}

// TestBacklogRateLimitEndsInErrorAfterADay: a backlog VOD YouTube refuses
// for a day straight goes back to Queued for each hour of it — 24 requeues —
// and the refusal that ends the day ends it in Error, worded as yt-dlp words
// the refusal and saying the job gave up. Measured on the wall clock from
// the spell's first refusal, so a re-admission that comes late does not
// stretch it: a VOD the scheduler admitted every two hours gives up at the
// same instant, on its 13th run. A backlog under a captcha nobody solved
// went back to Queued every hour for good.
//
// Mutants: drop the cap from outlastRateLimit — the run at 24 h is requeued;
// measure from the latest refusal (forget the spell on every requeue) —
// likewise; count runs instead of the clock (give up on the 25th) — the
// two-hourly VOD is requeued at 24 h.
func TestBacklogRateLimitEndsInErrorAfterADay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		every time.Duration // from one run to the scheduler's next admission
		runs  int           // the run that gives up
	}{
		{"admitted each hour", rateLimitBackoff, 25},
		{"admitted an hour late each time", 2 * rateLimitBackoff, 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				backlogRetryJob(t, db, "cap_vod", 1, true)
				refused := rateLimitedResult(t)
				w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
					return refused, nil
				}
				t0 := time.Now()
				for run := 1; ; run++ {
					w.processJob(context.Background(), "cap_vod")
					row, _ := db.GetJob("cap_vod")
					if run < tc.runs {
						if row.Status != database.StatusQueued {
							t.Fatalf("run %d at %s: status = %s (%q), want Queued", run, time.Since(t0), row.Status, row.Error)
						}
						time.Sleep(tc.every)
						db.UpdateJobFields("cap_vod", map[string]any{"status": database.StatusUpcoming}) // the admission
						continue
					}
					if at := time.Since(t0); at != rateLimitCap {
						t.Errorf("gave up at %s, want %s", at, rateLimitCap)
					}
					if row.Status != database.StatusError || row.Error != refused.Error+rateLimitGaveUpNote {
						t.Fatalf("run %d: status = %s, error %q; want Error, %q", run, row.Status, row.Error, refused.Error+rateLimitGaveUpNote)
					}
					return
				}
			})
		})
	}
}

// TestInPlaceRateLimitEndsInErrorAfterADay: a job that is not backlog waits
// a day of refusals out in place — 48 tries half an hour apart — and the
// 49th refusal, at 24 hours, ends it in Error, worded as the refusal was. A
// 429 whose Retry-After lengthens every wait to 45 minutes gives up at the
// same instant, on its 33rd fetch: the day is the clock's, not 48 tries.
// Before, the job fetched every half hour for as long as the limit lasted,
// which nothing bounded.
//
// Mutants: drop the cap from outlastRateLimit — the job is still waiting at
// 26 h; count 48 waits instead of the clock — the 429 row waits on past
// 24 h; leave the sentinel's "(rate-limited by YouTube)" on the error
// (rateLimitGaveUp) — the verdict row's error differs.
func TestInPlaceRateLimitEndsInErrorAfterADay(t *testing.T) {
	refusedMsg := func(t *testing.T) string { return rateLimitedResult(t).Error }
	for _, tc := range []struct {
		name    string
		refusal func(t *testing.T) (*StreamProcessResult, error)
		every   time.Duration
		want    func(t *testing.T, msg string)
	}{
		{"verdict", func(t *testing.T) (*StreamProcessResult, error) { return rateLimitedResult(t), nil }, rateLimitRetryInterval,
			func(t *testing.T, msg string) {
				if want := refusedMsg(t) + rateLimitGaveUpNote; msg != want {
					t.Errorf("error = %q, want %q", msg, want)
				}
			}},
		{"429 asking for 45 minutes", func(*testing.T) (*StreamProcessResult, error) { return nil, rateLimited429(45 * time.Minute) }, 45 * time.Minute,
			func(t *testing.T, msg string) {
				if !strings.Contains(msg, "HTTP 429") || !strings.HasSuffix(msg, rateLimitGaveUpNote) {
					t.Errorf("error = %q, want the 429, giving up", msg)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				backlogRetryJob(t, db, "cap_live", 0, false)
				t0 := time.Now()
				var mu sync.Mutex
				var fetches []time.Duration
				w.processStreamFn = func(ctx context.Context, _ *database.Job) (*StreamProcessResult, error) {
					if ctx.Err() != nil {
						return cancelledResult(), nil
					}
					mu.Lock()
					fetches = append(fetches, time.Since(t0))
					mu.Unlock()
					return tc.refusal(t)
				}
				if !runInBubble(t, w, "cap_live", rateLimitCap+2*time.Hour) {
					t.Fatal("the job is still waiting two hours past the day")
				}
				mu.Lock()
				defer mu.Unlock()
				if want := int(rateLimitCap/tc.every) + 1; len(fetches) != want || fetches[len(fetches)-1] != rateLimitCap {
					t.Errorf("%d fetches, the last at %s; want %d, the last at %s", len(fetches), fetches[len(fetches)-1], want, rateLimitCap)
				}
				row, _ := db.GetJob("cap_live")
				if row.Status != database.StatusError {
					t.Fatalf("status = %s, want Error", row.Status)
				}
				tc.want(t, row.Error)
				if row.Progress != "" {
					t.Errorf("progress = %q, want the wait's line cleared", row.Progress)
				}
			})
		})
	}
}

// TestRateLimitSpellEndsOnAnAnswer: a pass YouTube answers ends the spell,
// and a refusal after it starts the day afresh. Here a job refused for 23½
// hours is answered — a VOD, whose extraction is stale once its slots are
// held — and the re-extraction is refused again: half an hour later, at 24 h
// from the first refusal, it is still waiting, its new spell 30 minutes old.
//
// Mutant: drop rateLimitSpells.end from noteAnswered — the job ends in Error
// at 24 h, on a spell an answer had ended.
func TestRateLimitSpellEndsOnAnAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "cap_answer", 0, false)
		refused := rateLimitedResult(t)
		answerAt := rateLimitCap - rateLimitRetryInterval
		t0 := time.Now()
		w.processStreamFn = func(ctx context.Context, _ *database.Job) (*StreamProcessResult, error) {
			if ctx.Err() != nil {
				return cancelledResult(), nil
			}
			if time.Since(t0) == answerAt {
				// expire=1: stale once the slots are held, and no URL left to use.
				return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
			}
			return refused, nil
		}
		w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
			return judgeRefreshedVodInfo(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
		}
		if runInBubble(t, w, "cap_answer", rateLimitCap+time.Minute) {
			row, _ := db.GetJob("cap_answer")
			t.Fatalf("the run ended (%s, %q); it was to wait on, its spell begun at %s", row.Status, row.Error, answerAt)
		}
		// The cancel at the budget ends it as a shutdown does: the row keeps
		// its status.
		if row, _ := db.GetJob("cap_answer"); row.Status != database.StatusUpcoming {
			t.Errorf("status = %s (%q), want the Upcoming it waited in", row.Status, row.Error)
		}
	})
}

// TestRateLimitSpellEndsOnCancelAndRetry: a dashboard Retry starts the job
// over, its day with it. A backlog VOD refused hourly for 23 hours is
// cancelled while it waits in Queued — the backlog's wait — and retried; its
// next refusal, at 24 hours, sends it back to Queued, the first of a new
// day, not to Error.
//
// Mutant: forget only the retry count in forgetBacklogRetries (leave the
// spell) — the retried job ends in Error on its first refusal.
func TestRateLimitSpellEndsOnCancelAndRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "cap_retry", 1, true)
		refused := rateLimitedResult(t)
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return refused, nil
		}
		for run := 1; run <= 24; run++ { // at 0, 1, … 23 hours
			w.processJob(context.Background(), "cap_retry")
			if row, _ := db.GetJob("cap_retry"); row.Status != database.StatusQueued {
				t.Fatalf("run %d: status = %s (%q), want Queued", run, row.Status, row.Error)
			}
			time.Sleep(rateLimitBackoff)
			if run < 24 {
				db.UpdateJobFields("cap_retry", map[string]any{"status": database.StatusUpcoming})
			}
		}
		if cancelled, _ := w.CancelJob("cap_retry"); !cancelled {
			t.Fatal("the Cancel of the Queued row did not apply")
		}
		w.ReinitializeJob("cap_retry")
		w.processJob(context.Background(), "cap_retry")
		if row, _ := db.GetJob("cap_retry"); row.Status != database.StatusQueued {
			t.Errorf("the retried job's first refusal: status = %s (%q), want Queued", row.Status, row.Error)
		}
	})
}

// TestCancelAtTheRateLimitCapWins: the Error the day's last refusal writes
// never lands over an operator's Cancel. CancelJob writes the row before it
// flags the run; one whose write lands while that refusal is out is the
// job's end, for the backlog VOD and the job waiting in place alike, and no
// Job Failed goes out.
//
// Mutant: end the job at the cap with an unconditional Error write instead
// of setJobError — both rows read Error.
func TestCancelAtTheRateLimitCapWins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priority int
		partner  bool
		every    time.Duration
	}{
		{"backlog", 1, true, rateLimitBackoff},
		{"in place", 0, false, rateLimitRetryInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				rec := notificationtest.New()
				w.notifier = rec
				backlogRetryJob(t, db, "cap_cancel", tc.priority, tc.partner)
				refused := rateLimitedResult(t)
				t0 := time.Now()
				w.processStreamFn = func(ctx context.Context, job *database.Job) (*StreamProcessResult, error) {
					if ctx.Err() != nil {
						return cancelledResult(), nil
					}
					if time.Since(t0) >= rateLimitCap {
						// CancelJob's write, ahead of its flag.
						db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled})
					}
					return refused, nil
				}
				if tc.priority == 1 {
					for time.Since(t0) < rateLimitCap {
						w.processJob(context.Background(), "cap_cancel")
						time.Sleep(tc.every)
						db.UpdateJobFields("cap_cancel", map[string]any{"status": database.StatusUpcoming})
					}
					w.processJob(context.Background(), "cap_cancel")
				} else if !runInBubble(t, w, "cap_cancel", rateLimitCap+time.Hour) {
					t.Fatal("the job is still waiting an hour past the day")
				}
				if row, _ := db.GetJob("cap_cancel"); row.Status != database.StatusCancelled {
					t.Errorf("status = %s (%q), want the operator's Cancelled", row.Status, row.Error)
				}
				if n := len(rec.ByEvent("error")); n != 0 {
					t.Errorf("%d Job Failed sends for a cancelled job, want 0", n)
				}
			})
		})
	}
}

// TestWaitingLoopRateLimitEndsInErrorAfterADay: a broadcast in its waiting
// loop whose full fetches YouTube refuses for a day ends in Error too — once
// the stream has begun. Here the status probe reads live once, three minutes
// in, and its go-live confirmation is refused; from then on the probe cannot
// tell (inconclusive), and the periodic full fetch, half-hourly, is refused
// on: counted, since the last probe that could tell said live, so the loop
// gives up on the first refusal a day after the confirmation's. Its result
// carries no ErrRateLimited, which processJob would wait out again, and a
// Cancel that lands as the day's last refusal comes back still wins.
//
// While the probe still answers upcoming — or cannot tell, after a last
// conclusive answer of upcoming — the same refused periodic fetches do not
// count (the owner's decision on FA's second follow-up): a premiere days out
// under a captcha nobody solved waits on, its loop running, rather than
// ending in Error before it airs; it was ended at 24 h.
//
// Mutants: drop the outlasted check from the periodic fetch — the counted
// rows are still waiting at 26 h; count every refusal (counts returning true)
// — the upcoming rows end in Error; leave begun alone on a conclusive
// upcoming (noteStatus) or set it from the inconclusive stand-in — the
// "inconclusive after upcoming" row ends in Error, or the counted ones never
// do; give up with ErrRateLimited as the sentinel — processJob waits it out
// in place and the row is not Error; reset capSince on every refusal — the
// counted rows wait on.
func TestWaitingLoopRateLimitEndsInErrorAfterADay(t *testing.T) {
	inconclusive := &youtube.StatusProbeInconclusiveError{Playability: youtube.PlayabilityLoginRequired,
		Reason: "Sign in to confirm you're not a bot"}
	for _, tc := range []struct {
		name        string
		probe       func(at time.Duration, liveSeen *bool) (*youtube.VideoInfo, error)
		counted     bool
		cancelAtCap bool
	}{
		{name: "upcoming: never counted", probe: func(time.Duration, *bool) (*youtube.VideoInfo, error) {
			return upcomingAnswer(time.Time{}), nil
		}},
		{name: "inconclusive after upcoming: never counted", probe: func(at time.Duration, _ *bool) (*youtube.VideoInfo, error) {
			if at < 3*time.Minute {
				return upcomingAnswer(time.Time{}), nil
			}
			return nil, inconclusive
		}},
		{name: "inconclusive after live: counted", counted: true, probe: liveOnceThenInconclusive(inconclusive)},
		{name: "inconclusive after live: cancel at the cap wins", counted: true, cancelAtCap: true,
			probe: liveOnceThenInconclusive(inconclusive)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				backlogRetryJob(t, db, "cap_loop", 0, false)
				start := time.Now().Add(72 * time.Hour).UTC()
				sp := w.streamProc
				sp.yt = youtube.NewService(cookies.NewCookieJar(), discardLogger{})
				var mu sync.Mutex
				var fulls []time.Duration
				liveSeen := false
				t0 := time.Now()
				sp.getVideoInfoFn = func(_ context.Context, id string) (*youtube.VideoInfo, error) {
					at := time.Since(t0)
					mu.Lock()
					fulls = append(fulls, at)
					n := len(fulls)
					mu.Unlock()
					if n == 1 {
						return upcomingAnswer(start), nil // Process's own fetch answers
					}
					if tc.cancelAtCap && at-fulls[1] >= rateLimitCap {
						db.UpdateJobFieldsUnlessTerminal(id, map[string]any{"status": database.StatusCancelled})
					}
					return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
				}
				sp.probeStatusFn = func(context.Context, string, bool) (*youtube.VideoInfo, error) {
					mu.Lock()
					defer mu.Unlock()
					return tc.probe(time.Since(t0), &liveSeen)
				}
				sp.probeScheduleFn = func(context.Context, string) (*youtube.VideoInfo, error) {
					return upcomingAnswer(start), nil
				}
				returned := runInBubble(t, w, "cap_loop", rateLimitCap+2*time.Hour)
				mu.Lock()
				defer mu.Unlock()
				row, _ := db.GetJob("cap_loop")
				if !tc.counted {
					if returned || row.Status == database.StatusError {
						t.Fatalf("returned %v, status %s (%q): a broadcast still upcoming ended on refusals that do not count", returned, row.Status, row.Error)
					}
					if last := fulls[len(fulls)-1]; last < rateLimitCap {
						t.Errorf("the last full fetch came at %s; the loop was to keep fetching past the day", last)
					}
					return
				}
				if !returned {
					t.Fatal("the broadcast is still waiting two hours past a day of counted refusals")
				}
				if len(fulls) < 3 {
					t.Fatalf("full fetches at %v", fulls)
				}
				first, last, prev := fulls[1], fulls[len(fulls)-1], fulls[len(fulls)-2]
				if last-first < rateLimitCap || prev-first >= rateLimitCap {
					t.Errorf("gave up on the refusal %s after the first; want the first one past %s", last-first, rateLimitCap)
				}
				if tc.cancelAtCap {
					if row.Status != database.StatusCancelled {
						t.Errorf("status = %s (%q), want the operator's Cancelled", row.Status, row.Error)
					}
					return
				}
				msg, _ := playabilityVerdict(rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut))
				if row.Status != database.StatusError || row.Error != msg+rateLimitGaveUpNote {
					t.Errorf("status = %s, error %q; want Error, %q", row.Status, row.Error, msg+rateLimitGaveUpNote)
				}
			})
		})
	}
}

// liveOnceThenInconclusive is a status probe that reads upcoming for three
// minutes, live once — its go-live confirmation is refused — and then cannot
// tell for good (err).
func liveOnceThenInconclusive(err error) func(time.Duration, *bool) (*youtube.VideoInfo, error) {
	return func(at time.Duration, liveSeen *bool) (*youtube.VideoInfo, error) {
		switch {
		case at < 3*time.Minute:
			return upcomingAnswer(time.Time{}), nil
		case !*liveSeen:
			*liveSeen = true
			return liveAnswer(time.Time{}), nil
		}
		return nil, err
	}
}

// TestRateLimitCapNeedsThreeRefusals: a day of a spell is not enough to end
// a job — it needs rateLimitCapRefusals (3) refused attempts too, on every
// path (the owner's decision on FA's third follow-up). A backlog VOD the
// scheduler re-admits 25 hours after its first refusal is requeued again on
// its second, and ends on its third; and a broadcast whose go-live
// confirmation is refused once, three minutes in, that then reads upcoming
// for a day — its periodic refusals uncounted — and live again, ends not on
// that refusal, its second counted, a day after the first, but on the next,
// half an hour later (the spell's taper long spent). Before, either ended on
// its second look.
//
// Mutants: drop the refusal floor from outlastRateLimit — the VOD ends on its
// second run; from loopRateLimit.outlasted — the broadcast ends on its second
// counted refusal.
func TestRateLimitCapNeedsThreeRefusals(t *testing.T) {
	t.Run("backlog requeue", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			backlogRetryJob(t, db, "floor_vod", 1, true)
			refused := rateLimitedResult(t)
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				return refused, nil
			}
			for run := 1; run <= 3; run++ {
				w.processJob(context.Background(), "floor_vod")
				row, _ := db.GetJob("floor_vod")
				if run < 3 {
					if row.Status != database.StatusQueued {
						t.Fatalf("run %d: status = %s (%q), want Queued — %d refusals are not the cap's", run, row.Status, row.Error, run)
					}
					time.Sleep(25 * time.Hour)
					db.UpdateJobFields("floor_vod", map[string]any{"status": database.StatusUpcoming}) // the admission
					continue
				}
				if row.Status != database.StatusError || row.Error != refused.Error+rateLimitGaveUpNote {
					t.Fatalf("run 3: status = %s, error %q; want Error, %q", row.Status, row.Error, refused.Error+rateLimitGaveUpNote)
				}
			}
		})
	})
	t.Run("waiting loop", func(t *testing.T) {
		m := time.Minute
		liveAgain := rateLimitCap + 10*m
		run := driveWaitForLive(t, 2*m, rateLimitCap+2*time.Hour, func(start time.Time) waitFakes {
			return waitFakes{
				status: func(at time.Duration) (*youtube.VideoInfo, error) {
					if at >= 3*m && at < 4*m || at >= liveAgain {
						return liveAnswer(start), nil
					}
					return upcomingAnswer(time.Time{}), nil
				},
				full: func(time.Duration) (*youtube.VideoInfo, error) {
					return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
				},
			}
		})
		if run.result == nil {
			t.Fatalf("still waiting at the budget; calls:%s", run)
		}
		var confirmations []time.Duration
		for _, at := range fullCalls(run) {
			if at < 4*m || at >= liveAgain {
				confirmations = append(confirmations, at)
			}
		}
		if len(confirmations) != 3 || run.returned != confirmations[2] || confirmations[1]-confirmations[0] < rateLimitCap {
			t.Fatalf("confirmations at %v, returned at %s; want the give-up on the third, a day and more after the first", confirmations, run.returned)
		}
		if run.result.ShouldDownload || run.result.Error == "" {
			t.Errorf("result = %+v, want the give-up", run.result)
		}
	})
}

// TestPrunedRateLimitedBacklogLosesItsSpellAndHold: a backlog VOD the limit
// alone sent back to Queued holds a spell and a scheduler hold, but no retry
// count, and the channel prune's bulk delete — which fires one OnJobsChange,
// no OnJobDeleted — looked through the counts alone, so its hold widened
// every sweep's NextQueuedJobs for good and a re-created row inherited the
// spell.
//
// Mutant: drop the spells from endStreaksGoneFrom — the hold outlives the row
// (the wait times out).
func TestPrunedRateLimitedBacklogLosesItsSpellAndHold(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	backlogRetryJob(t, db, "spell_pruned", 1, true)
	refused := rateLimitedResult(t)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return refused, nil
	}
	w.processJob(context.Background(), "spell_pruned")
	if n, held := backlogStreak(w, "spell_pruned"); n != 0 || !held {
		t.Fatalf("after the requeue: count %d held %v, want no count and held", n, held)
	}
	if len(w.rateLimitSpells.ids()) != 1 {
		t.Fatal("the refusal started no spell")
	}
	if n, err := db.DeleteJobsAndHistoryForChannel("UC_retry",
		[]database.JobStatus{database.StatusQueued}, nil); err != nil || n != 1 {
		t.Fatalf("prune: deleted %d, err %v; want the one Queued row", n, err)
	}
	waitForCond(t, mustHappenWithin, "the pruned row's spell and hold to end", func() bool {
		_, held := backlogStreak(w, "spell_pruned")
		return !held && len(w.rateLimitSpells.ids()) == 0
	})
}

// TestRefusedConfirmationsEndInErrorAfterADay: a broadcast that has begun —
// or ended — while YouTube refuses every go-live confirmation ends in Error
// too, on the first refused confirmation a day or more after the spell's
// first, saying it gave up: in the live arm and the VOD arm, refused with
// the verdict or a 429 on the whole cascade, and in the default arm, whose
// full fetch confirms a stream the cookied probe cannot read. The error is
// the verdict as yt-dlp words it, or the 429, and the note; the waiting
// room's chat stops with it (TestGivingUpOnARateLimitStopsTheChat).
//
// Mutants: drop the outlasted check from the verdict or the 429 of the live
// arm, the VOD arm or the default arm — that row is still waiting at 26 h;
// reset since on every refusal — every row is.
func TestRefusedConfirmationsEndInErrorAfterADay(t *testing.T) {
	verdict, _ := playabilityVerdict(rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut))
	for _, tc := range []struct {
		name     string
		signedIn bool
		status   func(at time.Duration, start time.Time) (*youtube.VideoInfo, error)
		refusal  func() (*youtube.VideoInfo, error)
		want     string
	}{
		{name: "live arm, the verdict", status: liveFrom3m(liveAnswer),
			refusal: func() (*youtube.VideoInfo, error) {
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
			want: verdict + rateLimitGaveUpNote},
		{name: "live arm, a 429", status: liveFrom3m(liveAnswer),
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) },
			want:    rateLimited429(0).Error() + rateLimitGaveUpNote},
		{name: "VOD arm, the verdict", status: liveFrom3m(vodAnswer),
			refusal: func() (*youtube.VideoInfo, error) {
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
			want: verdict + rateLimitGaveUpNote},
		{name: "VOD arm, a 429", status: liveFrom3m(vodAnswer),
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) },
			want:    rateLimited429(0).Error() + rateLimitGaveUpNote},
		{name: "default arm, the verdict", signedIn: true, status: membersStatus,
			refusal: func() (*youtube.VideoInfo, error) {
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
			want: verdict + rateLimitGaveUpNote},
		{name: "default arm, a 429", signedIn: true, status: membersStatus,
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) },
			want:    rateLimited429(0).Error() + rateLimitGaveUpNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, rateLimitCap+2*time.Hour, func(start time.Time) waitFakes {
				return waitFakes{
					signedIn: tc.signedIn,
					status:   func(at time.Duration) (*youtube.VideoInfo, error) { return tc.status(at, start) },
					full:     func(time.Duration) (*youtube.VideoInfo, error) { return tc.refusal() },
				}
			})
			fulls := fullCalls(run)
			if run.result == nil {
				t.Fatalf("still waiting two hours past a day of refused confirmations (%d made)", len(fulls))
			}
			if len(fulls) < 3 {
				t.Fatalf("full fetches at %v; calls:%s", fulls, run)
			}
			first, last, prev := fulls[0], fulls[len(fulls)-1], fulls[len(fulls)-2]
			if last-first < rateLimitCap || prev-first >= rateLimitCap || run.returned != last {
				t.Errorf("gave up at %s on the confirmation %s after the first; want on the first one %s or more after it",
					run.returned, last-first, rateLimitCap)
			}
			if run.result.ShouldDownload || run.result.ErrSentinel != nil || run.result.Error != tc.want {
				t.Errorf("result = download %v, sentinel %v, error %q; want Error %q with no sentinel",
					run.result.ShouldDownload, run.result.ErrSentinel, run.result.Error, tc.want)
			}
			if run.row.Progress != "" {
				t.Errorf("row progress = %q once the loop gave up, want the wait's line cleared", run.row.Progress)
			}
		})
	}
}

// TestGivingUpOnARateLimitStopsTheChat: the waiting loop's give-up at the day
// stops the waiting room's chat, as its other give-ups do: left running, it
// went on capturing for a job that had ended in Error, beside whatever ran
// the job next.
//
// Mutant: drop stopEarlyChat from giveUpRateLimited — the chat is still
// tracked.
func TestGivingUpOnARateLimitStopsTheChat(t *testing.T) {
	sp, _, job := foundTestJob(t)
	early := chat.NewChatDownloader(chat.ChatDownloaderOptions{VideoID: job.VideoID,
		OutputFile: filepath.Join(t.TempDir(), "chat.json")})
	sp.trackChat(early)
	l := loopRateLimit{refusing: true, since: time.Now().Add(-rateLimitCap)}
	res := sp.giveUpRateLimited(job, &l, nil, rateLimited429(0), early)
	sp.mu.Lock()
	tracked := len(sp.activeChats)
	sp.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d chat downloader(s) still tracked after the give-up, want the waiting room's stopped", tracked)
	}
	if want := rateLimited429(0).Error() + rateLimitGaveUpNote; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
}

// TestLoopRateLimitCountsOnceTheStreamBegan pins noteStatus and counts on
// every answer the probe gives (the owner's decision on FA's second
// follow-up): a refused periodic fetch counts toward the cap once the last
// conclusive answer said the stream is live or over — live, a VOD, post-live
// — and not while it says upcoming, nor before any answer; an answer that
// tells nothing (not a stream) leaves the last one standing. A go-live
// confirmation's refusal counts whatever was said.
//
// Mutants: count only on live (the VOD and post-live rows do not count);
// leave begun set on upcoming (the "live, then upcoming" row counts).
func TestLoopRateLimitCountsOnceTheStreamBegan(t *testing.T) {
	for _, tc := range []struct {
		answers []youtube.StreamStatus
		want    bool
	}{
		{nil, false},
		{[]youtube.StreamStatus{youtube.StreamUpcoming}, false},
		{[]youtube.StreamStatus{youtube.StreamLive}, true},
		{[]youtube.StreamStatus{youtube.StreamVOD}, true},
		{[]youtube.StreamStatus{youtube.StreamPostLive}, true},
		{[]youtube.StreamStatus{youtube.StreamLive, youtube.StreamUpcoming}, false},
		{[]youtube.StreamStatus{youtube.StreamPostLive, youtube.StreamNotAStream}, true},
		{[]youtube.StreamStatus{youtube.StreamUpcoming, youtube.StreamNotAStream}, false},
	} {
		var l loopRateLimit
		for _, s := range tc.answers {
			l.noteStatus(s)
		}
		if got := l.counts(false); got != tc.want {
			t.Errorf("after %v a refused periodic fetch counts = %v, want %v", tc.answers, got, tc.want)
		}
		if !l.counts(true) {
			t.Errorf("after %v a refused confirmation does not count", tc.answers)
		}
	}
}

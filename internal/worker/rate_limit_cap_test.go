package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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
// loop whose full fetches YouTube refuses for a day ends in Error too — the
// periodic one here, half-hourly while the stream is days out. The loop
// waited it out for good: its refusals are no answer, and the probe kept it
// waiting. Its result carries no ErrRateLimited, which processJob would wait
// out again, and a Cancel that lands as the day's last refusal comes back
// still wins.
//
// Mutants: drop the outlasted check from the periodic fetch — the job is
// still waiting at 26 h; give up with ErrRateLimited as the sentinel —
// processJob waits it out in place and the row is not Error; reset since on
// every refusal — likewise.
func TestWaitingLoopRateLimitEndsInErrorAfterADay(t *testing.T) {
	for _, cancelAtCap := range []bool{false, true} {
		name := "error"
		if cancelAtCap {
			name = "cancel at the cap wins"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				backlogRetryJob(t, db, "cap_loop", 0, false)
				start := time.Now().Add(72 * time.Hour).UTC()
				sp := w.streamProc
				sp.yt = youtube.NewService(cookies.NewCookieJar(), discardLogger{})
				var mu sync.Mutex
				var fulls []time.Duration
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
					if cancelAtCap && at-fulls[1] >= rateLimitCap {
						db.UpdateJobFieldsUnlessTerminal(id, map[string]any{"status": database.StatusCancelled})
					}
					return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
				}
				sp.probeStatusFn = func(context.Context, string, bool) (*youtube.VideoInfo, error) {
					return upcomingAnswer(time.Time{}), nil
				}
				sp.probeScheduleFn = func(context.Context, string) (*youtube.VideoInfo, error) {
					return upcomingAnswer(start), nil
				}
				if !runInBubble(t, w, "cap_loop", rateLimitCap+2*time.Hour) {
					t.Fatal("the broadcast is still waiting two hours past a day of refusals")
				}
				mu.Lock()
				defer mu.Unlock()
				if len(fulls) < 3 {
					t.Fatalf("full fetches at %v", fulls)
				}
				first, last, prev := fulls[1], fulls[len(fulls)-1], fulls[len(fulls)-2]
				if last-first < rateLimitCap || prev-first >= rateLimitCap {
					t.Errorf("gave up on the refusal %s after the first; want the first one past %s", last-first, rateLimitCap)
				}
				row, _ := db.GetJob("cap_loop")
				if cancelAtCap {
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

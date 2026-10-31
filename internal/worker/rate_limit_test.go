package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// rateLimitReason is the reason yt-dlp names the rate limit by (26feac3dd).
const rateLimitReason = "This content isn't available, try again later."

// rateLimitedInfo is a player response YouTube refused for a rate limit:
// nothing in it but the refusal, which classifyStream reads as status.
func rateLimitedInfo(status youtube.StreamStatus, auth youtube.SessionAuthState) *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: status, PlayabilityError: youtube.PlayabilityRateLimited,
		PlayabilityReason: rateLimitReason, SessionAuth: auth}
}

// rateLimitedResult is Process's answer to that response: the verdict the
// worker's playabilityVerdict gives it, as Process returns it.
func rateLimitedResult(t *testing.T) *StreamProcessResult {
	t.Helper()
	info := rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut)
	msg, sentinel := playabilityVerdict(info)
	if msg == "" || !errors.Is(sentinel, ErrRateLimited) {
		t.Fatalf("playabilityVerdict(rate_limited) = %q, %v; want a refusal marked ErrRateLimited", msg, sentinel)
	}
	return &StreamProcessResult{VideoInfo: info, ShouldDownload: false, Error: msg, ErrSentinel: sentinel}
}

// TestPlayabilityVerdictWordsARateLimit: the verdict is transient
// (ErrRateLimited) and worded as yt-dlp words it (_video.py:4074-4081): the
// account when the session was signed in, the session otherwise, and a
// captcha's reason, which already carries upstream's sentence, as it is.
//
// Mutants: drop the rate_limited arm — no sentinel, the bare reason; swap
// the two subjects — the wording rows fail; append the rate-limit sentence
// to a captcha too — the captcha row fails.
func TestPlayabilityVerdictWordsARateLimit(t *testing.T) {
	captcha := "Video unavailable. " + youtube.CaptchaChallengeNote
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
		want string
	}{
		{"signed in", rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedIn),
			"This content isn't available, try again later. Your account has been rate-limited by YouTube for up to an hour"},
		{"signed out", rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut),
			"This content isn't available, try again later. The current session has been rate-limited by YouTube for up to an hour"},
		{"login state unobserved", rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthUnknown),
			"This content isn't available, try again later. The current session has been rate-limited by YouTube for up to an hour"},
		{"a captcha", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityRateLimited, PlayabilityReason: captcha,
			SessionAuth: youtube.SessionAuthLoggedIn}, captcha},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, sentinel := playabilityVerdict(tc.info)
			if msg != tc.want {
				t.Errorf("message = %q, want %q", msg, tc.want)
			}
			if !errors.Is(sentinel, ErrRateLimited) {
				t.Errorf("sentinel = %v, want ErrRateLimited", sentinel)
			}
		})
	}
	if isTerminalPlayability(youtube.PlayabilityRateLimited) {
		t.Error("isTerminalPlayability(rate_limited) = true; the waiting loop would give up on a limit that passes")
	}
}

// rateLimited429 is GetVideoInfo's error when the cascade's last client was
// answered 429 and nothing else answered (finishExtraction wraps the last
// client's error), as Process returns it.
func rateLimited429(retryAfter time.Duration) error {
	return fmt.Errorf("full fetch failed: %w", fmt.Errorf("failed to extract any player response: %w",
		&youtube.HTTPRateLimitError{Err: errors.New("ANDROID_VR API error: HTTP 429"), RetryAfter: retryAfter}))
}

// TestBacklogRateLimitRequeuesOutsideTheRetryBudget is G7's backlog half: a
// backlog VOD YouTube rate-limits — with its verdict, or with a 429 on every
// client, the HTTP form of the same limit — goes back to Queued, held for the
// hour the limit lasts, however many times in a row — the streak it already
// had is neither counted on nor reset — and the limit holds every backlog
// admission for as long. It ended in Error, and the slot it freed admitted
// the channel's next VOD into the same wall; the 429 spent the three
// retries a blip gets, in 35 minutes, and ended in Error with no hold.
//
// Mutants: route the verdict to setJobError — the first run ends in Error;
// requeue it through requeueBacklog — the fourth run ends in Error, "gave up
// after 3 retries", and the streak grows; hold it for backlogRetryBackoff —
// it is admissible again within the hour; drop holdForRateLimit — the
// scheduler admits backlog while the verdict stands; drop the 429 from
// isRateLimited, or its routing from processJob's failed stream processing —
// the 429 row is held five minutes, and its fourth run ends in Error.
func TestBacklogRateLimitRequeuesOutsideTheRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		process func(t *testing.T) (*StreamProcessResult, error)
	}{
		{"verdict", func(t *testing.T) (*StreamProcessResult, error) { return rateLimitedResult(t), nil }},
		{"429", func(*testing.T) (*StreamProcessResult, error) { return nil, rateLimited429(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			backlogRetryJob(t, db, "rl_vod", 1, true)
			result, err := tc.process(t)
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				return result, err
			}
			// A streak from two earlier transient failures.
			w.noteBacklogRetry("rl_vod")
			w.noteBacklogRetry("rl_vod")

			for run := 1; run <= backlogRetryLimit+2; run++ {
				w.processJob(context.Background(), "rl_vod")
				now := time.Now()
				row, _ := db.GetJob("rl_vod")
				if row.Status != database.StatusQueued || row.Error != "" {
					t.Fatalf("run %d: status = %s (%q), want Queued with no error", run, row.Status, row.Error)
				}
				if !w.scheduler.held("rl_vod", now.Add(rateLimitBackoff-time.Minute)) {
					t.Fatalf("run %d: the job is admissible again within the hour", run)
				}
				if w.scheduler.held("rl_vod", now.Add(rateLimitBackoff+time.Minute)) {
					t.Fatalf("run %d: the job is held past the hour", run)
				}
				if !w.scheduler.rateLimitGateClosed(now.Add(rateLimitBackoff - time.Minute)) {
					t.Fatalf("run %d: backlog admission is open while the limit stands", run)
				}
				// The scheduler's re-admission, once the hold is over.
				db.UpdateJobFields("rl_vod", map[string]any{"status": database.StatusUpcoming})
			}
			w.backlogRetryMu.Lock()
			streak := w.backlogRetries["rl_vod"]
			w.backlogRetryMu.Unlock()
			if streak != 2 {
				t.Errorf("retry streak = %d after the rate-limit requeues, want the 2 it had", streak)
			}
		})
	}
}

// TestRateLimitedBroadcastWaitsInPlace is G7's other half, on the bubble's
// clock: a job that is not backlog — here a broadcast the monitor just found
// — keeps its Upcoming row through a rate limit, says so on its progress
// line, and fetches again rateLimitRetryInterval later. Its first Process
// used to be its last: the job ended in Error before the stream aired, and
// nothing created it again. The verdict still holds backlog admission.
//
// Mutants: route the verdict to setJobError — the row reads Error and there
// is no second fetch; drop the wait — the second fetch comes at once; drop
// holdForRateLimit from the non-backlog path — the scheduler admits; never
// rewrite the line — a minute in it still says the whole half hour is left.
func TestRateLimitedBroadcastWaitsInPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "rl_live", 0, false)
		t0 := time.Now()
		var mu sync.Mutex
		var fetches []time.Duration
		var rowAtSecond *database.Job
		refused := rateLimitedResult(t)
		w.processStreamFn = func(_ context.Context, job *database.Job) (*StreamProcessResult, error) {
			mu.Lock()
			fetches = append(fetches, time.Since(t0))
			n := len(fetches)
			mu.Unlock()
			if n == 1 {
				return refused, nil
			}
			rowAtSecond, _ = db.GetJob(job.ID)
			return &StreamProcessResult{ShouldDownload: false, Error: "not a stream"}, nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("processJob panicked: %v", r)
				}
			}()
			w.processJob(context.Background(), "rl_live")
		}()

		synctest.Wait()
		row, _ := db.GetJob("rl_live")
		if row.Status != database.StatusUpcoming || !strings.HasPrefix(row.Progress, rateLimitWaitProgress+"... (") {
			t.Errorf("mid-wait: status = %s, progress = %q; want Upcoming saying it waits out the limit", row.Status, row.Progress)
		}
		if !w.scheduler.rateLimitGateClosed(time.Now()) {
			t.Error("backlog admission is open while a broadcast's rate-limit verdict stands")
		}
		time.Sleep(rateLimitWaitRefresh)
		synctest.Wait()
		if row, _ := db.GetJob("rl_live"); row.Progress != rateLimitWaitLine(rateLimitRetryInterval-rateLimitWaitRefresh) {
			t.Errorf("a minute in, progress = %q; want the time left rewritten", row.Progress)
		}
		<-done

		mu.Lock()
		defer mu.Unlock()
		if len(fetches) != 2 || fetches[1]-fetches[0] != rateLimitRetryInterval {
			t.Fatalf("fetches at %v, want two, %s apart", fetches, rateLimitRetryInterval)
		}
		if rowAtSecond == nil || rowAtSecond.Status != database.StatusUpcoming || rowAtSecond.Progress != "" {
			t.Errorf("at the second fetch the row was %+v; want Upcoming with the wait's line cleared", rowAtSecond)
		}
	})
}

// TestRateLimitedBroadcastWaitEndsOnCancel: the wait in place stops on its
// run's cancel, and the cancel is handled as one.
//
// Mutant: drop the ctx case from waitOutRateLimit — the run waits the full
// interval and fetches again after the cancel.
func TestRateLimitedBroadcastWaitEndsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "rl_cancel", 0, false)
		var fetches int
		refused := rateLimitedResult(t)
		w.processStreamFn = func(ctx context.Context, _ *database.Job) (*StreamProcessResult, error) {
			fetches++
			if ctx.Err() != nil {
				// As Process stops on a cancelled run.
				return cancelledResult(), nil
			}
			return refused, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("processJob panicked: %v", r)
				}
			}()
			w.processJob(ctx, "rl_cancel")
		}()
		synctest.Wait()
		time.Sleep(time.Minute)
		cancel()
		<-done
		if fetches != 1 {
			t.Errorf("fetched %d times, want 1 — the cancel ends the wait", fetches)
		}
		if row, _ := db.GetJob("rl_cancel"); row.Status != database.StatusUpcoming || row.Progress != "" {
			t.Errorf("row = %s %q, want Upcoming (a shutdown keeps the row) with the line cleared", row.Status, row.Progress)
		}
	})
}

// TestRateLimitedWaitStopsOnARowThatLeftTheQueue: the row is read again
// once the wait is over, and a job whose row left the states the queue
// processes meanwhile — written Cancelled by something that did not cancel
// the run, a channel removal's bulk write, say — is not fetched again, and
// ends as a cancelled run.
//
// Mutant: go on unless the row is gone — the Cancelled row is fetched again.
func TestRateLimitedWaitStopsOnARowThatLeftTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "rl_left", 0, false)
		var mu sync.Mutex
		fetches := 0
		refused := rateLimitedResult(t)
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			mu.Lock()
			fetches++
			n := fetches
			mu.Unlock()
			if n == 1 {
				return refused, nil
			}
			// A fetch there should not have been; it ends the run, so the
			// test fails rather than waiting forever.
			return &StreamProcessResult{ShouldDownload: false, Error: "fetched again"}, nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("processJob panicked: %v", r)
				}
			}()
			w.processJob(context.Background(), "rl_left")
		}()
		synctest.Wait()
		db.UpdateJobFields("rl_left", map[string]any{"status": database.StatusCancelled})
		<-done
		mu.Lock()
		defer mu.Unlock()
		if fetches != 1 {
			t.Errorf("fetched %d times, want 1 — the row had left the queue", fetches)
		}
		if row, _ := db.GetJob("rl_left"); row.Status != database.StatusCancelled {
			t.Errorf("status = %s, want the Cancelled it was left in", row.Status)
		}
	})
}

// TestBacklogRateLimitedReextractRequeues: a stale VOD's re-extraction that
// YouTube rate-limits — its verdict, or a 429 on the whole cascade — with
// URLs that have expired, sends a backlog VOD back to Queued for the hour
// too. Its verdict reaches processJob as a vodRefreshVerdict, which ended it
// in Error; the 429 was a blip's five-minute retry, one of three.
//
// Mutants: drop the rate-limit arm after refreshStaleVodInfo — the verdict
// ends in Error, and the 429 is held five minutes.
func TestBacklogRateLimitedReextractRequeues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh func() (*youtube.VideoInfo, error)
	}{
		{"verdict", func() (*youtube.VideoInfo, error) {
			return judgeRefreshedVodInfo(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
		}},
		{"429", func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			backlogRetryJob(t, db, "rl_stale", 1, true)
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				// expire=1: stale once the slots are held, and no URL left to use.
				return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
			}
			w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
				return tc.refresh()
			}
			w.processJob(context.Background(), "rl_stale")
			if row, _ := db.GetJob("rl_stale"); row.Status != database.StatusQueued {
				t.Errorf("status = %s (%q), want Queued for the hour", row.Status, row.Error)
			}
			if !w.scheduler.held("rl_stale", time.Now().Add(rateLimitBackoff-time.Minute)) {
				t.Error("the job is admissible again within the hour")
			}
			if n := w.queue.ActiveCount(); n != 0 {
				t.Errorf("download slots still held after the requeue: %d", n)
			}
		})
	}
}

// TestRateLimitedReextractKeepsUnexpiredURLs: a stale VOD's re-extraction is
// a precaution, and a rate limit on it is the one verdict that passes — the
// player call was refused, not the URLs — so a VOD whose URLs are still good
// for hours downloads on them, as after a blip. The limit still holds the
// backlog: the next VOD admitted would fetch into it. It held nothing, the
// one rate limit a job met that did not.
//
// Mutants: drop the rate-limit term from refreshStaleVodInfo — the
// re-extraction's verdict is returned; drop the hold — admission stays
// open; hold on the verdict alone — the 429 row's stays open.
func TestRateLimitedReextractKeepsUnexpiredURLs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh func() (*youtube.VideoInfo, error)
	}{
		{"verdict", func() (*youtube.VideoInfo, error) {
			return judgeRefreshedVodInfo(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
		}},
		{"429", func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := testWorkerSetup(t)
			w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
				return tc.refresh()
			}
			now := time.Now()
			info := vodInfoAt("https://r1.googlevideo.com", now.Add(4*time.Hour).Unix(), 1)
			result := &StreamProcessResult{IsVod: true, VideoInfo: info}
			if err := w.refreshStaleVodInfo(context.Background(), &database.Job{ID: "j", Platform: "youtube"}, result, now.Add(-2*time.Hour)); err != nil || result.VideoInfo != info {
				t.Errorf("refreshStaleVodInfo = %v; want nil with the stale extraction kept", err)
			}
			if !w.scheduler.rateLimitGateClosed(now.Add(rateLimitBackoff - time.Minute)) {
				t.Error("the rate limit placed no backlog hold")
			}
		})
	}
}

// TestRateLimitWithFormatsDownloads: a rate-limit verdict on an extraction
// that came back with formats is not a refusal — upstream words one only
// when there are none (`if not formats:`, _video.py:4045) — so Process
// downloads, announcing as it goes, and a stale VOD's re-extraction is
// taken. Both went to the wait, the formats already in hand.
//
// Mutants: drop the formats gate from playabilityVerdict — Process refuses
// and the re-extraction is a verdict; defer the announcement on the verdict
// alone — the download sends no "Stream Found".
func TestRateLimitWithFormatsDownloads(t *testing.T) {
	info := vodInfoAt("https://r1.googlevideo.com", time.Now().Add(4*time.Hour).Unix(), 1)
	info.Title, info.ChannelName = "Fetched Title", "A Channel"
	info.PlayabilityError, info.PlayabilityReason = youtube.PlayabilityRateLimited, rateLimitReason
	if msg, sentinel := playabilityVerdict(info); msg != "" || sentinel != nil {
		t.Errorf("playabilityVerdict = %q, %v; want playable", msg, sentinel)
	}
	if got, err := judgeRefreshedVodInfo(info); err != nil || got != info {
		t.Errorf("judgeRefreshedVodInfo = %v, %v; want the extraction", got, err)
	}

	sp, rec, job := foundTestJob(t)
	owe(t, sp.db, job)
	sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) { return info, nil }
	res, err := sp.Process(context.Background(), job)
	if err != nil || !res.ShouldDownload || !res.IsVod {
		t.Fatalf("Process = %+v, %v; want the VOD download", res, err)
	}
	if n := len(rec.ByEvent("found")); n != 1 {
		t.Errorf("%d found embeds, want the download's one", n)
	}
}

// TestRateLimitHoldsBacklogAdmission: while a rate-limit verdict stands the
// scheduler admits no backlog, a shorter hold never cuts a longer one short,
// and admission resumes once it has run out.
//
// Mutants: drop the gate from sweep — both jobs are admitted under the hold;
// let a hold shorten the one standing — the later, shorter hold reopens
// admission early.
func TestRateLimitHoldsBacklogAdmission(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 2)
	ch := "UC_rl"
	for _, id := range []string{"rl_a", "rl_b"} {
		addSchedJob(t, db, &ch, id, database.StatusQueued, 1)
		addFeedItemRow(t, db, ch, id, "2026-07-10T00:00:00Z")
	}
	now := time.Now()
	s.holdForRateLimit(now, time.Hour)
	s.holdForRateLimit(now, time.Minute)
	s.sweep()
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d backlog jobs under a rate-limit hold, want 0", n)
	}
	if !s.rateLimitGateClosed(now.Add(30 * time.Minute)) {
		t.Fatal("a one-minute hold cut the standing one-hour hold short")
	}

	s.rateMu.Lock()
	s.rateLimitedUntil = time.Now().Add(-time.Second)
	s.rateMu.Unlock()
	s.sweep()
	if n := log.enqueueCount(); n != 2 {
		t.Errorf("admitted %d once the hold ran out, want 2", n)
	}
	if s.rateLimitGateClosed(time.Now()) {
		t.Error("the gate still reads closed after its hold ran out")
	}
}

// TestRateLimited429WaitsInPlaceAtLeastItsRetryAfter: a job that is not
// backlog, whose whole cascade was answered 429, waits in place as a
// rate-limit verdict does — it ended in Error, a broadcast lost before it
// aired — and for at least the Retry-After YouTube named, up to the hour a
// limit lasts; the full-fetch cadence stands where it is longer.
//
// Mutants: drop the 429 from isRateLimited — the row ends in Error and there
// is no second fetch; drop the Retry-After floor — the 45-minute one fetches
// again at 30; drop the cap — the five-hour one waits five hours.
func TestRateLimited429WaitsInPlaceAtLeastItsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"no Retry-After", 0, rateLimitRetryInterval},
		{"shorter than the cadence", 2 * time.Minute, rateLimitRetryInterval},
		{"longer than the cadence", 45 * time.Minute, 45 * time.Minute},
		{"past the hour", 5 * time.Hour, rateLimitBackoff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				backlogRetryJob(t, db, "rl_429", 0, false)
				t0 := time.Now()
				var fetches []time.Duration
				w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
					fetches = append(fetches, time.Since(t0))
					if len(fetches) == 1 {
						return nil, rateLimited429(tc.retryAfter)
					}
					return &StreamProcessResult{ShouldDownload: false, Error: "not a stream"}, nil
				}
				w.processJob(context.Background(), "rl_429")
				if len(fetches) != 2 || fetches[1]-fetches[0] != tc.want {
					t.Fatalf("fetches at %v, want two, %s apart", fetches, tc.want)
				}
				if row, _ := db.GetJob("rl_429"); row.Error != "not a stream" {
					t.Errorf("row error = %q; want the second fetch's answer, not the 429", row.Error)
				}
			})
		})
	}
}

// vodAnswer is a stream that is over, as the WEB-family calls see it.
func vodAnswer(start time.Time) *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayabilityError: youtube.PlayabilityOK,
		ScheduledStartTime: start.UTC().Format(time.RFC3339)}
}

// fullCalls is when the driven wait made its full fetches.
func fullCalls(run waitRun) []time.Duration {
	var at []time.Duration
	for _, c := range run.calls {
		if c.kind == "full" {
			at = append(at, c.at)
		}
	}
	return at
}

// TestRateLimitedGoLiveKeepsWaiting: a go-live confirmation YouTube refuses
// for a rate limit leaves the broadcast in its waiting loop — early chat
// running, status probe polling — and the confirmation is made again once
// the limit has been waited out, rateLimitRetryInterval later, on the first
// probe past it; that one goes live. The refusal holds the backlog. Before,
// a refusal read as an answer either wrote Live and went on to a download
// with nothing to download (Error), or, read "still upcoming", sent the whole
// cascade into the limit on every 30 s probe; and then it was handed back to
// processJob, which stopped the waiting-room chat and waited half an hour
// with no probe at all.
//
// Mutants: drop noteFetch from the live arm — the not_a_stream row goes
// live on the refusal, the upcoming one confirms on every probe; from the
// VOD arm — the VOD row does; drop the arms' confirmationHeld — every probe
// confirms; never lift the hold — there is no second confirmation; drop the
// hook — no backlog hold.
func TestRateLimitedGoLiveKeepsWaiting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    func(start time.Time) *youtube.VideoInfo
		refusedAs youtube.StreamStatus
	}{
		{"live, refusal reads not_a_stream", liveAnswer, youtube.StreamNotAStream},
		{"live, refusal reads upcoming", liveAnswer, youtube.StreamUpcoming},
		{"VOD, refusal reads upcoming", vodAnswer, youtube.StreamUpcoming},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, 2*time.Hour, func(start time.Time) waitFakes {
				return waitFakes{
					status: func(at time.Duration) (*youtube.VideoInfo, error) {
						if at < 3*time.Minute {
							return upcomingAnswer(time.Time{}), nil
						}
						return tc.answer(start), nil
					},
					full: func(at time.Duration) (*youtube.VideoInfo, error) {
						if at < 10*time.Minute {
							return rateLimitedInfo(tc.refusedAs, youtube.SessionAuthLoggedOut), nil
						}
						return tc.answer(start), nil
					},
				}
			})
			fulls := fullCalls(run)
			if run.result == nil || !run.result.ShouldDownload {
				t.Fatalf("result = %+v, want the download once the limit was waited out; calls:%s", run.result, run)
			}
			if len(fulls) != 2 {
				t.Fatalf("full fetches at %v, want the refused confirmation and the one after the wait; calls:%s", fulls, run)
			}
			if gap := fulls[1] - fulls[0]; gap < rateLimitRetryInterval || gap > rateLimitRetryInterval+time.Minute {
				t.Errorf("confirmations %s apart, want the first probe past %s", gap, rateLimitRetryInterval)
			}
			if run.returned != fulls[1] {
				t.Errorf("the wait returned at %s; it was to wait in its loop until the confirmation at %s", run.returned, fulls[1])
			}
			if n := run.count("hold"); n != 1 {
				t.Errorf("%d backlog holds, want the refusal's one; calls:%s", n, run)
			}
			if wantVod := tc.answer(run.start).StreamStatus == youtube.StreamVOD; run.result.IsVod != wantVod {
				t.Errorf("IsVod = %v, want %v", run.result.IsVod, wantVod)
			}
			if run.row.Progress != "" {
				t.Errorf("row progress = %q once the confirmation answered, want the wait's line cleared", run.row.Progress)
			}
		})
	}
}

// TestRateLimitedPeriodicFetchHoldsTheBacklog: the waiting loop's periodic
// full fetch that YouTube refuses for a rate limit holds the backlog, on the
// first refusal of a spell: two in a row are one spell, and one after an
// answer is a new one. The loop's fetches held nothing; a broadcast waiting
// days was the one job that saw the limit, and the backlog went on being
// admitted into it.
//
// Mutants: drop noteFetch from the periodic fetch — no hold; hold on every
// refusal — three.
func TestRateLimitedPeriodicFetchHoldsTheBacklog(t *testing.T) {
	// Six hours out the status probe asks every ten minutes, so the full
	// fetch is due on the first of those ticks past each half hour.
	run := driveWaitForLive(t, 6*time.Hour, 2*time.Hour+20*time.Minute, func(start time.Time) waitFakes {
		n := 0
		return waitFakes{
			full: func(time.Duration) (*youtube.VideoInfo, error) {
				n++
				if n == 3 {
					return upcomingAnswer(start), nil
				}
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
		}
	})
	if n := run.count("full"); n != 4 {
		t.Fatalf("%d periodic full fetches, want 4; calls:%s", n, run)
	}
	if n := run.count("hold"); n != 2 {
		t.Errorf("%d backlog holds for refusals at the 1st, 2nd and 4th fetch, want 2 — one a spell; calls:%s", n, run)
	}
}

// TestWaitingLoopRateLimitLine: the progress line a held confirmation shows
// is the wait's own, with the time left, rewritten on each probe and cleared
// when the hold runs out, when a fetch answers, and when the loop returns.
//
// Mutants: leave the line on a hold that ran out; leave it on an answer;
// leave it on the loop's return; never rewrite the time left.
func TestWaitingLoopRateLimitLine(t *testing.T) {
	sp, _, job := foundTestJob(t)
	progress := func() string {
		t.Helper()
		row, _ := sp.db.GetJob(job.ID)
		return row.Progress
	}
	refused := rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut)
	var l loopRateLimit

	if !sp.noteFetch(job, &l, refused, true) || !l.confirmationHeld() {
		t.Fatal("a refused confirmation did not hold the next one")
	}
	if got := progress(); got != rateLimitWaitLine(rateLimitRetryInterval) {
		t.Errorf("line on the refusal = %q", got)
	}
	sp.tickRateLimit(job, &l, l.until.Add(-20*time.Minute))
	if got, want := progress(), rateLimitWaitLine(20*time.Minute); got != want {
		t.Errorf("line ten minutes in = %q, want %q", got, want)
	}
	sp.tickRateLimit(job, &l, l.until)
	if l.confirmationHeld() || progress() != "" {
		t.Errorf("at the hold's end: held %v, line %q; want lifted and cleared", l.confirmationHeld(), progress())
	}

	sp.noteFetch(job, &l, refused, true)
	if sp.noteFetch(job, &l, upcomingAnswer(time.Time{}), false) || l.confirmationHeld() || progress() != "" {
		t.Errorf("after an answer: held %v, line %q; want lifted and cleared", l.confirmationHeld(), progress())
	}

	sp.noteFetch(job, &l, refused, true)
	sp.endRateLimit(job, &l)
	if got := progress(); got != "" {
		t.Errorf("line after the loop returned = %q, want cleared", got)
	}
}

// TestWaitingLoopRateLimitHoldIsTheSchedulers pins the seam: in production
// the waiting loop's rate-limit hook is the backlog scheduler's hold, for
// the hour.
//
// Mutant: NewDownloadWorker does not wire it — the hook is nil.
func TestWaitingLoopRateLimitHoldIsTheSchedulers(t *testing.T) {
	w, _ := testWorkerSetup(t)
	if w.streamProc.onRateLimited == nil {
		t.Fatal("the waiting loop's rate-limit hook is not wired")
	}
	now := time.Now()
	w.streamProc.onRateLimited()
	if !w.scheduler.rateLimitGateClosed(now.Add(rateLimitBackoff - time.Minute)) {
		t.Error("the hook placed no hour-long backlog hold")
	}
}

// TestRateLimitedFetchDefersTheFoundAnnouncement: a discovery whose first
// fetch YouTube rate-limits — with its verdict, or with a 429 on the whole
// cascade — keeps its "Stream Found" owed, and the fetch that answers sends
// it, with what it learned. Taken on the refusal, the embed went out with
// the feed's title and nothing else.
//
// Mutants: take the flag on a rate-limited fetch, or on a 429 — that row's
// refusal announces and the answer has nothing left to announce.
func TestRateLimitedFetchDefersTheFoundAnnouncement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal func() (*youtube.VideoInfo, error)
	}{
		{"verdict", func() (*youtube.VideoInfo, error) {
			return rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut), nil
		}},
		{"429", func() (*youtube.VideoInfo, error) {
			return nil, errors.Unwrap(rateLimited429(0)) // Process adds its own "full fetch failed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, rec, job := foundTestJob(t)
			owe(t, sp.db, job)
			calls := 0
			sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
				calls++
				if calls == 1 {
					return tc.refusal()
				}
				return &youtube.VideoInfo{Title: "Fetched Title", ChannelName: "A Channel", StreamStatus: youtube.StreamVOD, PlayabilityError: youtube.PlayabilityOK}, nil
			}

			res, err := sp.Process(context.Background(), job)
			if refused := err != nil && isRateLimited(err) || err == nil && !res.ShouldDownload && errors.Is(res.ErrSentinel, ErrRateLimited); !refused {
				t.Fatalf("first Process = %+v, %v; want the rate limit", res, err)
			}
			if n := len(rec.ByEvent("found")); n != 0 {
				t.Fatalf("the refusal announced the discovery (%d embeds)", n)
			}
			if row, _ := sp.db.GetJob(job.ID); !row.AnnounceFound {
				t.Fatal("the refusal took the owed flag")
			}

			if res, err := sp.Process(context.Background(), job); err != nil || !res.ShouldDownload {
				t.Fatalf("second Process = %+v, %v; want the VOD download", res, err)
			}
			found := rec.ByEvent("found")
			if len(found) != 1 || found[0].Description != "Video: Fetched Title" {
				t.Errorf("found embeds = %+v, want one saying what the answer learned", found)
			}
		})
	}
}

// TestRateLimitWaitLineCarriesTheWaitMarker pins the cross-layer contract
// the TUI reads (internal/tui isWaitProgress): the line a job waiting out a
// rate limit shows carries the "... (" every wait line does, so its row reads
// as waiting, not hung, and says how long is left.
//
// Mutant: drop the marker from rateLimitWaitLine — both checks fail.
func TestRateLimitWaitLineCarriesTheWaitMarker(t *testing.T) {
	if got, want := rateLimitWaitLine(24*time.Minute), "Rate-limited by YouTube; fetching again... (24m 0s left)"; got != want {
		t.Errorf("rateLimitWaitLine(24m) = %q, want %q", got, want)
	}
	if msg := rateLimitWaitLine(0); !strings.Contains(msg, "... (") {
		t.Errorf("rateLimitWaitLine(0) = %q lacks the %q wait marker", msg, "... (")
	}
}

// TestRateLimitIntervalsAreUpstreamsHourAndTheFullFetchCadence pins the two
// waits: an hour for a backlog VOD and for backlog admission, the hour yt-dlp
// says the limit lasts, and the waiting loop's full-fetch cadence for a job
// waiting in place.
func TestRateLimitIntervalsAreUpstreamsHourAndTheFullFetchCadence(t *testing.T) {
	if rateLimitBackoff != time.Hour {
		t.Errorf("rateLimitBackoff = %s, want 1h", rateLimitBackoff)
	}
	if rateLimitRetryInterval != fullFetchInterval || fullFetchInterval != 30*time.Minute {
		t.Errorf("rateLimitRetryInterval = %s (fullFetchInterval %s), want the 30m full-fetch cadence", rateLimitRetryInterval, fullFetchInterval)
	}
}

// TestRateLimitHoldLeavesTheDiskGateItsReadings: the disk gate reads the
// volume, and closes, on the same sweeps whether or not a rate-limit hold
// stands, so a disk that filled during the hour is held — and recorded for
// the next process (RecordDiskHold) — the moment it does, not once the hold
// has run out.
//
// Mutant: read the rate-limit gate ahead of the disk gate — the disk is
// never read under the hold, and the gate stays open.
func TestRateLimitHoldLeavesTheDiskGateItsReadings(t *testing.T) {
	s, db, _ := testSchedulerSetup(t, 1)
	ch := "UC_rl_disk"
	addSchedJob(t, db, &ch, "rl_disk", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "rl_disk", "2026-07-10T00:00:00Z")
	var recorded []bool
	s.RecordDiskHold(func(held bool) { recorded = append(recorded, held) })
	s.readDisk = func() (diskReading, error) { return diskReading{critical: true}, nil }

	s.holdForRateLimit(time.Now(), time.Hour)
	s.sweep()
	if !s.diskHeld || len(recorded) != 1 || !recorded[0] {
		t.Errorf("disk gate held %v (recorded %v) after a critical reading under the rate-limit hold, want closed and recorded", s.diskHeld, recorded)
	}
}

// TestRateLimitedWaitHoldsTheBacklogOnce: a job that waits in place holds
// backlog admission on its first refusal, not on each it meets as it fetches
// again. Every half-hourly refusal used to push the hour on before it ran
// out, so one video YouTube kept refusing held every channel's backlog for
// as long as its job waited — which nothing bounds.
//
// Mutant: place the hold on every refusal — the hold the 60-minute refusal
// pushed on still stands at 61.
func TestRateLimitedWaitHoldsTheBacklogOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "rl_once", 0, false)
		var mu sync.Mutex
		fetches := 0
		refused := rateLimitedResult(t)
		w.processStreamFn = func(ctx context.Context, _ *database.Job) (*StreamProcessResult, error) {
			mu.Lock()
			fetches++
			mu.Unlock()
			if ctx.Err() != nil {
				return cancelledResult(), nil
			}
			return refused, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("processJob panicked: %v", r)
				}
			}()
			w.processJob(ctx, "rl_once")
		}()
		t0 := time.Now()
		synctest.Wait()
		if !w.scheduler.rateLimitGateClosed(time.Now()) {
			t.Fatal("the first refusal placed no backlog hold")
		}
		time.Sleep(rateLimitBackoff + time.Minute)
		synctest.Wait()
		mu.Lock()
		n := fetches
		mu.Unlock()
		if n != 3 {
			t.Fatalf("%d fetches by %s, want the refusals at 0, 30 and 60 minutes", n, time.Since(t0))
		}
		if w.scheduler.rateLimitGateClosed(time.Now()) {
			t.Error("backlog admission is still held an hour after the job's first refusal; its later refusals pushed it on")
		}
		cancel()
		<-done
	})
}

// TestRateLimitedReextractGivesBackItsSlots: a VOD that is not backlog,
// whose stale re-extraction YouTube rate-limits once both its slots are
// held, gives them back for the wait, and takes them again — once — when it
// runs again: kept, a download slot and a lifecycle slot sat idle through
// the half hour, and the next pass counted each of them a second time,
// leaking one of each for good. And a refusal after a pass that answered is
// a fresh spell: it holds the backlog again.
//
// Mutants: drop the ReleaseSlots before the wait — the wait holds a slot of
// each pool, and the next pass two; don't reset the spell once a pass
// answers — the re-extraction's refusal places no hold, and admission
// reopens at the hour.
func TestRateLimitedReextractGivesBackItsSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "rl_slots", 0, false)
		processed := 0
		refused := rateLimitedResult(t)
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			processed++
			if processed == 1 {
				return refused, nil
			}
			// expire=1: stale once the slots are held, and no URL left to use.
			return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
		}
		var mu sync.Mutex
		var atSecond []int
		refreshes := 0
		w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
			mu.Lock()
			refreshes++
			n := refreshes
			if n == 2 {
				atSecond = []int{w.queue.ActiveCount(), w.queue.LifecycleCount()}
			}
			mu.Unlock()
			if n == 1 {
				return judgeRefreshedVodInfo(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
			}
			return judgeRefreshedVodInfo(&youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream,
				PlayabilityError: youtube.PlayabilityPrivate, PlayabilityReason: "This video is private"})
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("processJob panicked: %v", r)
				}
			}()
			w.processJob(context.Background(), "rl_slots")
		}()

		// The re-extraction is refused on the second pass, at 30 minutes.
		time.Sleep(rateLimitRetryInterval + time.Minute)
		synctest.Wait()
		if dl, lc := w.queue.ActiveCount(), w.queue.LifecycleCount(); dl != 0 || lc != 0 {
			t.Errorf("mid-wait: %d download and %d lifecycle slots held, want 0 and 0", dl, lc)
		}
		<-done

		mu.Lock()
		defer mu.Unlock()
		if len(atSecond) != 2 || atSecond[0] != 1 || atSecond[1] != 1 {
			t.Errorf("slots at the next pass's re-extraction (download, lifecycle) = %v, want [1 1]", atSecond)
		}
		if dl, lc := w.queue.ActiveCount(), w.queue.LifecycleCount(); dl != 0 || lc != 0 {
			t.Errorf("after the run: %d download and %d lifecycle slots held, want 0 and 0", dl, lc)
		}
		// The first refusal's hour has run out at 61 minutes; the
		// re-extraction's, from 30, has not.
		if !w.scheduler.rateLimitGateClosed(time.Now().Add(rateLimitBackoff - rateLimitRetryInterval - time.Minute)) {
			t.Error("the re-extraction's refusal, after a pass that answered, placed no backlog hold")
		}
	})
}

// TestRateLimitHoldOpensAfreshOnceRunOut: a hold that ran out with no sweep
// to see it — sweep returns ahead of the gate while offline or while the disk
// gate is closed — opens afresh on the next limit, at Warn, rather than
// extending a hold that no longer stands at Debug.
//
// Mutant: open only when no hold is marked — the second limit logs at Debug.
func TestRateLimitHoldOpensAfreshOnceRunOut(t *testing.T) {
	log := &levelLogger{}
	s := &Scheduler{log: log}
	t0 := time.Now()
	s.holdForRateLimit(t0, time.Hour)
	s.holdForRateLimit(t0.Add(3*time.Hour), time.Hour)
	if n := log.count("WARN", "rate-limited this session"); n != 2 {
		t.Errorf("%d opening Warns for two holds three hours apart, want 2; log: %q", n, log.lines)
	}
}

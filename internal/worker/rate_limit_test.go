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

// TestBacklogRateLimitRequeuesOutsideTheRetryBudget is G7's backlog half: a
// backlog VOD YouTube rate-limits goes back to Queued, held for the hour the
// limit lasts, however many times in a row — the streak it already had is
// neither counted on nor reset — and the verdict holds every backlog
// admission for as long. It ended in Error, and the slot it freed admitted
// the channel's next VOD into the same wall.
//
// Mutants: route the verdict to setJobError — the first run ends in Error;
// requeue it through requeueBacklog — the fourth run ends in Error, "gave up
// after 3 retries", and the streak grows; hold it for backlogRetryBackoff —
// it is admissible again within the hour; drop holdForRateLimit — the
// scheduler admits backlog while the verdict stands.
func TestBacklogRateLimitRequeuesOutsideTheRetryBudget(t *testing.T) {
	w, db := testWorkerSetup(t)
	backlogRetryJob(t, db, "rl_vod", 1, true)
	refused := rateLimitedResult(t)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return refused, nil
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
			t.Fatalf("run %d: backlog admission is open while the verdict stands", run)
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
// holdForRateLimit from the non-backlog path — the scheduler admits.
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

// TestBacklogRateLimitedReextractRequeues: a stale VOD's re-extraction that
// YouTube rate-limits, with URLs that have expired, sends a backlog VOD back
// to Queued too. Its verdict reaches processJob as a vodRefreshVerdict,
// which ended it in Error.
//
// Mutant: drop the ErrRateLimited arm after refreshStaleVodInfo — the job
// ends in Error.
func TestBacklogRateLimitedReextractRequeues(t *testing.T) {
	w, db := testWorkerSetup(t)
	backlogRetryJob(t, db, "rl_stale", 1, true)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		// expire=1: stale once the slots are held, and no URL left to use.
		return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
	}
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		return judgeRefreshedVodInfo(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
	}
	w.processJob(context.Background(), "rl_stale")
	if row, _ := db.GetJob("rl_stale"); row.Status != database.StatusQueued {
		t.Errorf("status = %s (%q), want Queued for the hour", row.Status, row.Error)
	}
	if n := w.queue.ActiveCount(); n != 0 {
		t.Errorf("download slots still held after the requeue: %d", n)
	}
}

// TestRateLimitedReextractKeepsUnexpiredURLs: a stale VOD's re-extraction is
// a precaution, and a rate limit on it is the one verdict that passes — the
// player call was refused, not the URLs — so a VOD whose URLs are still good
// for hours downloads on them, as after a blip.
//
// Mutant: drop the ErrRateLimited term from refreshStaleVodInfo — the
// re-extraction's verdict is returned.
func TestRateLimitedReextractKeepsUnexpiredURLs(t *testing.T) {
	w, _ := testWorkerSetup(t)
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		return judgeRefreshedVodInfo(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
	}
	now := time.Now()
	info := vodInfoAt("https://r1.googlevideo.com", now.Add(4*time.Hour).Unix(), 1)
	result := &StreamProcessResult{IsVod: true, VideoInfo: info}
	if err := w.refreshStaleVodInfo(context.Background(), &database.Job{ID: "j", Platform: "youtube"}, result, now.Add(-2*time.Hour)); err != nil || result.VideoInfo != info {
		t.Errorf("refreshStaleVodInfo = %v; want nil with the stale extraction kept", err)
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

// TestRequeueBacklogWaitsAtLeastTheRetryAfter: a 429 that named its wait
// holds the requeued backlog VOD at least that long, up to the hour a rate
// limit is held; the backoff stands where it is longer.
//
// Mutants: drop the floor — the 40-minute Retry-After is held for the five
// minute backoff; drop the cap — the five-hour one holds past the hour.
func TestRequeueBacklogWaitsAtLeastTheRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"shorter than the backoff", 30 * time.Second, backlogRetryBackoff},
		{"longer than the backoff", 40 * time.Minute, 40 * time.Minute},
		{"past the hour", 5 * time.Hour, rateLimitBackoff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			backlogRetryJob(t, db, "ra_vod", 1, true)
			// As GetVideoInfo wraps the last client's error (finishExtraction).
			err := fmt.Errorf("failed to extract any player response: %w",
				&youtube.HTTPRateLimitError{Err: errors.New("TVHTML5 API error: HTTP 429"), RetryAfter: tc.retryAfter})
			job, _ := db.GetJob("ra_vod")
			now := time.Now()
			requeued, _ := w.requeueBacklogAfterTransientFailure(job, err)
			if !requeued {
				t.Fatal("not requeued")
			}
			if !w.scheduler.held("ra_vod", now.Add(tc.want-time.Second)) {
				t.Errorf("not held up to %s", tc.want)
			}
			if w.scheduler.held("ra_vod", now.Add(tc.want+time.Minute)) {
				t.Errorf("held past %s", tc.want)
			}
		})
	}
}

// TestRateLimitedGoLiveIsHandedBack: the waiting loop's go-live confirmation
// that YouTube rate-limits is handed back to processJob as the refusal it
// is, with the row still Upcoming. Read as an answer, it either wrote Live
// and went on to a download with nothing to download — Error — or, read
// "still upcoming", sent the whole cascade into the limit again on every
// 30 s probe.
//
// Mutants: drop rateLimitedFetch from completeStreamTransition — the
// not_a_stream row writes Live and downloads; drop it from the live arm's
// upcoming cross-check — the upcoming row is confirmed on every probe until
// the budget ends.
func TestRateLimitedGoLiveIsHandedBack(t *testing.T) {
	for _, status := range []youtube.StreamStatus{youtube.StreamNotAStream, youtube.StreamUpcoming} {
		t.Run(string(status), func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, time.Hour, func(start time.Time) waitFakes {
				return waitFakes{
					status: func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil },
					full: func(time.Duration) (*youtube.VideoInfo, error) {
						return rateLimitedInfo(status, youtube.SessionAuthLoggedOut), nil
					},
				}
			})
			if run.returned < 0 || run.result == nil {
				t.Fatalf("the wait never returned; calls:%s", run)
			}
			if run.result.ShouldDownload || !errors.Is(run.result.ErrSentinel, ErrRateLimited) {
				t.Errorf("result = %+v, want the rate-limit refusal", run.result)
			}
			if n := run.count("full"); n != 1 {
				t.Errorf("%d full fetches, want the one confirmation; calls:%s", n, run)
			}
			if run.row.Status != database.StatusUpcoming {
				t.Errorf("row status = %s, want Upcoming", run.row.Status)
			}
		})
	}
}

// TestRateLimitedFetchDefersTheFoundAnnouncement: a discovery whose first
// fetch YouTube rate-limits keeps its "Stream Found" owed, and the fetch that
// answers sends it, with what it learned. Taken on the refusal, the embed
// went out with the feed's title and nothing else.
//
// Mutant: take the flag on a rate-limited fetch — the refusal announces and
// the answer has nothing left to announce.
func TestRateLimitedFetchDefersTheFoundAnnouncement(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	owe(t, sp.db, job)
	answers := []*youtube.VideoInfo{
		rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut),
		{Title: "Fetched Title", ChannelName: "A Channel", StreamStatus: youtube.StreamVOD, PlayabilityError: youtube.PlayabilityOK},
	}
	sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		info := answers[0]
		answers = answers[1:]
		return info, nil
	}

	res, err := sp.Process(context.Background(), job)
	if err != nil || res.ShouldDownload || !errors.Is(res.ErrSentinel, ErrRateLimited) {
		t.Fatalf("first Process = %+v, %v; want the rate-limit refusal", res, err)
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

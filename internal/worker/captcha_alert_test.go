package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// captchaInfo is a player response YouTube refused with a captcha challenge:
// the reason ends with yt-dlp's sentence, as parsePlayabilityStatus words it.
func captchaInfo() *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityRateLimited,
		PlayabilityReason: "Video unavailable. " + youtube.CaptchaChallengeNote}
}

// captchaResult is Process's answer to it.
func captchaResult(t *testing.T) *StreamProcessResult {
	t.Helper()
	info := captchaInfo()
	msg, sentinel := playabilityVerdict(info)
	if !isCaptcha(sentinel) {
		t.Fatalf("playabilityVerdict(captcha) = %q, %v; want the captcha sentinel", msg, sentinel)
	}
	return &StreamProcessResult{VideoInfo: info, ShouldDownload: false, Error: msg, ErrSentinel: sentinel}
}

// TestCaptchaVerdictIsARateLimitThatSaysSo: a captcha verdict carries its own
// sentinel, which is an ErrRateLimited — every path that waits a rate limit
// out waits it out too — and reads as one, so the job's error says what it
// did before; a "try again later" is not a captcha.
//
// Mutants: return ErrRateLimited for the captcha — isCaptcha is false; give
// captchaRequired its own text — the error reads differently.
func TestCaptchaVerdictIsARateLimitThatSaysSo(t *testing.T) {
	res := captchaResult(t)
	if !errors.Is(res.ErrSentinel, ErrRateLimited) || !isRateLimited(res.AsError()) {
		t.Error("the captcha's sentinel is not an ErrRateLimited")
	}
	if got, want := res.AsError().Error(), res.Error+" (rate-limited by YouTube)"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	_, sentinel := playabilityVerdict(rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut))
	if isCaptcha(sentinel) {
		t.Error("a try-again-later verdict reads as a captcha")
	}
	if isCaptcha(rateLimited429(0)) {
		t.Error("a 429 reads as a captcha")
	}
}

// TestCaptchaTellsTheOperatorOncePerSpell: two jobs a captcha refuses in one
// spell send one "captcha_required" between them, and each waits under the
// captcha's own line, "YouTube asks for a captcha... (… left)", not the rate
// limit's. A fetch that answers — the first job's, half an hour on, a verdict
// on the video that ends it in Error, with nothing downloaded — ends the
// spell, and the second job's next refusal opens a new one, which tells the
// operator again. Both run the real stream processing, whose fetch is where
// an answer is read (Process). A notification per job was one per refused
// video every half hour, and the rate limit's line told the operator to wait
// for something only they could fix; and only a pass that went on to
// download ended the spell, so a captcha back after any other answer told
// no one.
//
// Mutants: raise with no open gate — two notifications in the first spell;
// drop the clear from Process — no second; show the rate limit's line for a
// captcha (waitLineFor) — the rows' lines; return ErrRateLimited for a
// captcha — no notification at all.
func TestCaptchaTellsTheOperatorOncePerSpell(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		rec := notificationtest.New()
		w.captcha.notify = rec
		backlogRetryJob(t, db, "captcha_a", 0, false)
		backlogRetryJob(t, db, "captcha_b", 0, false)
		t0 := time.Now()
		w.streamProc.getVideoInfoFn = func(_ context.Context, id string) (*youtube.VideoInfo, error) {
			if id == "captcha_a" && time.Since(t0) >= rateLimitRetryInterval {
				return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream,
					PlayabilityError: youtube.PlayabilityPrivate, PlayabilityReason: "This video is private"}, nil
			}
			return captchaInfo(), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		var done []chan struct{}
		start := func(id string) {
			ch := make(chan struct{})
			done = append(done, ch)
			go func() {
				defer close(ch)
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("processJob panicked: %v", r)
					}
				}()
				w.processJob(ctx, id)
			}()
		}
		start("captcha_a")
		time.Sleep(time.Minute)
		start("captcha_b")
		synctest.Wait()

		if n := len(rec.ByEvent("captcha_required")); n != 1 {
			t.Errorf("%d captcha notifications for two jobs in one spell, want 1", n)
		}
		for _, id := range []string{"captcha_a", "captcha_b"} {
			if row, _ := db.GetJob(id); !strings.HasPrefix(row.Progress, captchaWaitProgress+"... (") {
				t.Errorf("%s waits under %q, want the captcha's line", id, row.Progress)
			}
		}

		// captcha_a's fetch answers at 30 minutes; captcha_b's next refusal,
		// at 31, is a new spell's.
		time.Sleep(rateLimitRetryInterval + time.Minute)
		synctest.Wait()
		if row, _ := db.GetJob("captcha_a"); row.Status != database.StatusError {
			t.Errorf("captcha_a: status %s, want the Error its answer ended it in", row.Status)
		}
		if n := len(rec.ByEvent("captcha_required")); n != 2 {
			t.Errorf("%d captcha notifications once a fetch answered and the captcha came back, want 2", n)
		}
		cancel()
		for _, ch := range done {
			<-ch
		}
	})
}

// TestCaptchaSpellEndsOnEveryAnswer: whatever stream processing does with a
// fetch YouTube answered with no rate-limit verdict — downloads it, ends on a
// verdict on the video, or waits in its loop for a broadcast to begin, inside
// Process for as long as that lasts — the answer ends the session's captcha
// spell, at the fetch. A refusal ends nothing, and neither does an
// extraction that came back with formats under a rate-limit verdict: it is
// downloaded, but the verdict says the session is still walled.
//
// Mutants: drop the clear from Process — the answered rows' spell stays
// open; read any fetch that is not refused outright as an answer
// (answeredFetch returning info != nil) — the formats row's spell ends.
func TestCaptchaSpellEndsOnEveryAnswer(t *testing.T) {
	withFormats := vodInfoAt("https://r1.googlevideo.com", 4_000_000_000, 1)
	withFormats.PlayabilityError, withFormats.PlayabilityReason = youtube.PlayabilityRateLimited, rateLimitReason
	for _, tc := range []struct {
		name   string
		answer *youtube.VideoInfo
		ended  bool
	}{
		{"a VOD it downloads", vodInfoAt("https://r1.googlevideo.com", 4_000_000_000, 1), true},
		{"a private video", &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream,
			PlayabilityError: youtube.PlayabilityPrivate, PlayabilityReason: "This video is private"}, true},
		{"a members-only one", &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream,
			PlayabilityError: youtube.PlayabilityMembersOnly, PlayabilityReason: "Join this channel",
			SessionAuth: youtube.SessionAuthLoggedOut}, true},
		{"a broadcast it waits for", upcomingAnswer(time.Now().Add(72 * time.Hour)), true},
		{"formats under the verdict", withFormats, false},
		{"the captcha again", captchaInfo(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				db := openTestDB(t)
				chID := "UC_answer"
				job := &database.Job{ID: "answer1", VideoID: "answer1", Platform: "youtube", Title: "T",
					Status: database.StatusUpcoming, ChannelID: &chID}
				if _, err := db.AddJob(job); err != nil {
					t.Fatalf("AddJob: %v", err)
				}
				stored, _ := db.GetJob(job.ID)
				sp := NewStreamProcessor(youtube.NewService(cookies.NewCookieJar(), discardLogger{}), nil,
					&config.MoomboxConfig{}, db, discardLogger{})
				sp.captcha = newCaptchaAlert(nil, discardLogger{})
				sp.captcha.raise(stored)
				sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) { return tc.answer, nil }
				sp.probeStatusFn = func(context.Context, string, bool) (*youtube.VideoInfo, error) {
					return upcomingAnswer(time.Time{}), nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("Process panicked: %v", r)
						}
					}()
					_, _ = sp.Process(ctx, stored)
				}()
				synctest.Wait() // returned, or waiting in its loop
				sp.captcha.mu.Lock()
				open := sp.captcha.open
				sp.captcha.mu.Unlock()
				cancel()
				<-done
				if open == tc.ended {
					t.Errorf("captcha spell open = %v after the fetch, want %v", open, !tc.ended)
				}
			})
		})
	}
}

// TestWaitingLoopCaptchaLineAndAlert: a go-live confirmation refused with a
// captcha holds the next one off under the captcha's line, rewritten with
// the time left, and tells the operator once however many of the loop's
// fetches it refuses; a fetch that answers ends the spell, and the next
// captcha tells them again. A plain rate limit tells nobody.
//
// Mutants: rewrite the held line with the rate limit's (tickRateLimit) — the
// tick's line; drop the raise from noteFetch — no notification; drop the
// clear — no second; clear on any fetch that is not refused — the formats
// under the verdict end the spell, and the captcha after them tells again.
func TestWaitingLoopCaptchaLineAndAlert(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.captcha = newCaptchaAlert(rec, discardLogger{})
	progress := func() string {
		t.Helper()
		row, _ := sp.db.GetJob(job.ID)
		return row.Progress
	}
	var l loopRateLimit
	sp.noteFetch(job, &l, rateLimitedInfo(youtube.StreamNotAStream, youtube.SessionAuthLoggedOut), true)
	if n := len(rec.ByEvent("captcha_required")); n != 0 {
		t.Fatalf("a try-again-later refusal sent %d captcha notifications", n)
	}
	sp.noteFetch(job, &l, upcomingAnswer(time.Time{}), false)

	if !sp.noteFetch(job, &l, captchaInfo(), true) {
		t.Fatal("a captcha confirmation was read as an answer")
	}
	if got, want := progress(), captchaWaitLine(confirmationHoldAfter(1)); got != want {
		t.Errorf("line on the refusal = %q, want %q", got, want)
	}
	sp.tickRateLimit(job, &l, l.until.Add(-time.Minute))
	if got, want := progress(), captchaWaitLine(time.Minute); got != want {
		t.Errorf("line a minute in = %q, want %q", got, want)
	}
	sp.noteFetch(job, &l, captchaInfo(), false)
	if n := len(rec.ByEvent("captcha_required")); n != 1 {
		t.Errorf("%d captcha notifications for two refusals of one spell, want 1", n)
	}
	// An extraction with formats under the verdict is no refusal, but no
	// answer either: the spell stands, and the next captcha is still its.
	withFormats := vodInfoAt("https://r1.googlevideo.com", 4_000_000_000, 1)
	withFormats.PlayabilityError, withFormats.PlayabilityReason = youtube.PlayabilityRateLimited, rateLimitReason
	if sp.noteFetch(job, &l, withFormats, false) {
		t.Error("an extraction with formats under the verdict was read as a refusal")
	}
	sp.noteFetch(job, &l, captchaInfo(), false)
	if n := len(rec.ByEvent("captcha_required")); n != 1 {
		t.Errorf("%d captcha notifications once formats came back under the verdict, want the spell's 1", n)
	}
	sp.noteFetch(job, &l, upcomingAnswer(time.Time{}), false)
	sp.noteFetch(job, &l, captchaInfo(), false)
	if n := len(rec.ByEvent("captcha_required")); n != 2 {
		t.Errorf("%d captcha notifications once an answer ended the spell and the captcha came back, want 2", n)
	}
}

// TestCaptchaOnAReextractTellsTheOperator: a stale VOD's re-extraction a
// captcha refuses, whose URLs are still good, downloads on them — and the
// captcha, which the next job will meet, still tells the operator. A
// re-extraction that came back with formats under a rate-limit verdict
// downloads too, and ends nothing; one YouTube answered — with fresh URLs, or
// with a verdict on the video that ends the job — ends the spell.
//
// Mutants: drop the raise from refreshStaleVodInfo — no notification; drop
// the clear on its answer, or on its verdict — the spell stays open; clear on
// any re-extraction — the formats under the verdict end it; clear on any
// verdict, a rate limit's too — the captcha on expired URLs ends it.
func TestCaptchaOnAReextractTellsTheOperator(t *testing.T) {
	w, _ := testWorkerSetup(t)
	rec := notificationtest.New()
	w.captcha.notify = rec
	now := time.Now()
	stale := func() *StreamProcessResult {
		return &StreamProcessResult{IsVod: true, VideoInfo: vodInfoAt("https://r1.googlevideo.com", now.Add(4*time.Hour).Unix(), 1)}
	}
	job := &database.Job{ID: "captcha_vod", VideoID: "captcha_vod", Platform: "youtube"}
	open := func() bool {
		w.captcha.mu.Lock()
		defer w.captcha.mu.Unlock()
		return w.captcha.open
	}
	reextract := func(info *youtube.VideoInfo) error {
		w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
			return judgeRefreshedVodInfo(info)
		}
		return w.refreshStaleVodInfo(context.Background(), job, stale(), now.Add(-2*time.Hour))
	}
	if err := reextract(captchaInfo()); err != nil {
		t.Fatalf("refreshStaleVodInfo = %v, want the download on the stale extraction", err)
	}
	if n := len(rec.ByEvent("captcha_required")); n != 1 {
		t.Errorf("%d captcha notifications, want 1", n)
	}
	withFormats := vodInfoAt("https://r2.googlevideo.com", now.Add(6*time.Hour).Unix(), 1)
	withFormats.PlayabilityError, withFormats.PlayabilityReason = youtube.PlayabilityRateLimited, rateLimitReason
	if err := reextract(withFormats); err != nil {
		t.Fatalf("refreshStaleVodInfo = %v, want the download on the formats it came back with", err)
	}
	if !open() {
		t.Error("an extraction that came back with formats under a rate-limit verdict ended the captcha spell")
	}
	if err := reextract(vodInfoAt("https://r2.googlevideo.com", now.Add(6*time.Hour).Unix(), 1)); err != nil {
		t.Fatalf("refreshStaleVodInfo = %v", err)
	}
	if open() {
		t.Error("the captcha spell is still open after a re-extraction answered")
	}
	if err := reextract(captchaInfo()); err != nil {
		t.Fatalf("refreshStaleVodInfo = %v, want the download on the stale extraction", err)
	}
	// A captcha on one whose URLs have expired goes back to processJob, which
	// waits it out: no answer, so the spell stands.
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		return judgeRefreshedVodInfo(captchaInfo())
	}
	expired := &StreamProcessResult{IsVod: true, VideoInfo: vodInfoAt("https://r1.googlevideo.com", now.Add(-time.Hour).Unix(), 1)}
	if err := w.refreshStaleVodInfo(context.Background(), job, expired, now.Add(-7*time.Hour)); !isCaptcha(err) {
		t.Fatalf("refreshStaleVodInfo = %v, want the captcha back for processJob", err)
	}
	if !open() {
		t.Error("a captcha on a re-extraction whose URLs had expired ended the captcha spell")
	}
	private := &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream,
		PlayabilityError: youtube.PlayabilityPrivate, PlayabilityReason: "This video is private"}
	if err := reextract(private); !isVodRefreshVerdict(err) {
		t.Fatalf("refreshStaleVodInfo = %v, want the private verdict", err)
	}
	if open() {
		t.Error("the captcha spell is still open after a re-extraction YouTube answered with a verdict on the video")
	}
}

// TestCaptchaAlertIsTheWorkersAndItsProcessors pins the seam: in production
// the worker and its stream processor share one captcha spell, and it sends
// through the worker's notifier.
//
// Mutant: NewDownloadWorker gives the processor none — its captchas tell no
// one, and open no spell the worker's answers end.
func TestCaptchaAlertIsTheWorkersAndItsProcessors(t *testing.T) {
	db := openTestDB(t)
	rec := notificationtest.New()
	w := NewDownloadWorker(db, nil, &config.MoomboxConfig{}, discardLogger{}, &DownloadWorkerDeps{Notifier: rec})
	if w.captcha == nil || w.streamProc.captcha != w.captcha {
		t.Fatalf("worker captcha %p, processor's %p; want one, shared", w.captcha, w.streamProc.captcha)
	}
	if w.captcha.notify != rec {
		t.Error("the captcha alert does not send through the worker's notifier")
	}
}

// TestCaptchaWaitLineCarriesTheWaitMarker pins the cross-layer contract the
// TUI reads (internal/tui isWaitProgress) for the captcha's line, as
// TestRateLimitWaitLineCarriesTheWaitMarker does for the rate limit's.
//
// Mutant: drop the marker from captchaWaitLine — both checks fail.
func TestCaptchaWaitLineCarriesTheWaitMarker(t *testing.T) {
	if got, want := captchaWaitLine(24*time.Minute), "YouTube asks for a captcha... (24m 0s left)"; got != want {
		t.Errorf("captchaWaitLine(24m) = %q, want %q", got, want)
	}
	if msg := captchaWaitLine(0); !strings.Contains(msg, "... (") {
		t.Errorf("captchaWaitLine(0) = %q lacks the %q wait marker", msg, "... (")
	}
}

// TestRateLimitCapAndTaperAreTheOwners pins the follow-up's numbers: a day
// of refusals, the taper of 2, 5, 10 and 20 minutes and the half hour after
// it, and the words the give-up adds.
func TestRateLimitCapAndTaperAreTheOwners(t *testing.T) {
	if rateLimitCap != 24*time.Hour {
		t.Errorf("rateLimitCap = %s, want 24h", rateLimitCap)
	}
	want := []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, d := range want {
		if got := confirmationHoldAfter(i + 1); got != d {
			t.Errorf("confirmationHoldAfter(%d) = %s, want %s", i+1, got, d)
		}
	}
	if rateLimitGaveUpNote != " (gave up after 24 hours of refusals)" {
		t.Errorf("rateLimitGaveUpNote = %q", rateLimitGaveUpNote)
	}
}

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// refusedWithCookies is the stream-processing answer of a video the platform
// refuses on its credentials: the bot check, which YouTube sends to a session
// that verifies.
func refusedWithCookies(context.Context, *database.Job) (*StreamProcessResult, error) {
	return &StreamProcessResult{Error: "Login required: Sign in to confirm you're not a bot", ErrSentinel: ErrCookiesRequired}, nil
}

// cookieRetryLap runs the job once and waits for the tail of its run (the
// cookie-refresh resume's hand-off).
func cookieRetryLap(t *testing.T, w *DownloadWorker, id string) {
	t.Helper()
	w.processJob(context.Background(), id)
	w.wg.Wait()
}

// TestCookieRetryBudgetEndsTheRefreshLoop: a job the platform keeps refusing
// while its session verifies asked for a cookie refresh, was "restored" and
// retried, every lap, without limit — a full extraction, a headless-browser
// launch, a rewrite of cookies.txt and an "Authentication Required" embed,
// about once a minute. It now gets three automatic retries (owner ruling
// R2-C1). With none left it stays in COOKIES?, asks for no refresh, logs one
// Warn, and its error text says the cookies are not the cause; the embed is
// sent for the first park only.
//
// Mutants: the budget check dropped from attemptCookieRefresh — a fourth
// refresh is asked for and the row is resumed; NoteCookieRetry dropped from
// the Restored arm — the same; the repeat-park suppression dropped — four
// embeds; the spent text not written — the row keeps the bare refusal.
func TestCookieRetryBudgetEndsTheRefreshLoop(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	log := &captureLogger{}
	w.logger = log
	rec := notificationtest.New()
	w.notifier = rec
	const id = "bot_checked"
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Title: "T", Status: database.StatusUpcoming}); err != nil {
		t.Fatal(err)
	}
	w.processStreamFn = refusedWithCookies
	asked := 0
	w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome {
		asked++
		return CookieRefreshRestored
	}

	for lap := 1; lap <= jobstate.MaxCookieAutoRetries; lap++ {
		cookieRetryLap(t, w, id)
		if asked != lap {
			t.Fatalf("lap %d: %d refreshes asked for, want %d", lap, asked, lap)
		}
		row, _ := db.GetJob(id)
		if row.Status != database.StatusUpcoming {
			t.Fatalf("lap %d: status = %s (%q), want the job retried", lap, row.Status, row.Error)
		}
		if got := w.AdmissionHolds().CookieRetries[id]; got.Count != lap || got.Of != jobstate.MaxCookieAutoRetries {
			t.Fatalf("lap %d: snapshot = %+v, want retry %d of %d", lap, got, lap, jobstate.MaxCookieAutoRetries)
		}
	}

	cookieRetryLap(t, w, id) // the park with nothing left
	if asked != jobstate.MaxCookieAutoRetries {
		t.Errorf("%d refreshes asked for, want none past the budget of %d", asked, jobstate.MaxCookieAutoRetries)
	}
	row, _ := db.GetJob(id)
	if row.Status != database.StatusCookies || row.ParkReason != database.ParkReasonAuth {
		t.Fatalf("row = %s / %s, want it parked in COOKIES? as an auth park", row.Status, row.ParkReason)
	}
	if !strings.Contains(row.Error, "not a bot") || !strings.Contains(row.Error, "YouTube still refuses this video after 3 automatic retries") ||
		!strings.Contains(row.Error, "the cookies are not the cause") {
		t.Errorf("error text = %q, want the refusal and why nothing retries it", row.Error)
	}
	warns := 0
	for _, line := range log.lines() {
		if msg, _ := line[0].(string); strings.HasPrefix(msg, "no automatic cookie retry left for this job") {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("%d spent-budget lines logged, want 1", warns)
	}
	if n := len(rec.ByEvent("auth")); n != 1 {
		t.Errorf("%d Authentication Required embeds sent over the spell, want the first park's only", n)
	}
}

// TestCookieRetryBudgetResets: the count goes when a run passes the
// playability check, 24 hours after the last retry, and with an operator's
// Cancel or Delete (owner ruling R2-C1; Resume and Reinitialize call the same
// forgetCookieRetries).
//
// Mutants: forgetCookieRetries dropped from processJob's answered path, from
// CancelJob, or from the OnJobDeleted subscription — that row's count stands;
// the window check dropped from cookieRetryCount — the count stands a day on.
func TestCookieRetryBudgetResets(t *testing.T) {
	spend := func(w *DownloadWorker, id string) {
		for range jobstate.MaxCookieAutoRetries {
			w.NoteCookieRetry(id)
		}
	}

	t.Run("a run past the playability check", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		t.Cleanup(w.Stop)
		const id = "answered"
		addSchedJob(t, db, nil, id, database.StatusUpcoming, 0)
		spend(w, id)
		ctx, cancel := context.WithCancel(context.Background())
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			cancel() // the run ends right after the answer, before any download
			return &StreamProcessResult{ShouldDownload: true}, nil
		}
		w.processJob(ctx, id)
		if !w.CookieRetryLeft(id) {
			t.Error("the count outlived a run that passed the playability check")
		}
	})

	t.Run("24 hours after the last retry", func(t *testing.T) {
		w, _ := testWorkerSetup(t)
		t.Cleanup(w.Stop)
		now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
		w.cookieRetryClock = func() time.Time { return now }
		spend(w, "day")
		now = now.Add(jobstate.CookieRetryWindow - time.Minute)
		if w.CookieRetryLeft("day") {
			t.Fatal("a retry was left inside the window")
		}
		now = now.Add(time.Minute)
		if !w.CookieRetryLeft("day") {
			t.Fatal("no retry left once the window had passed")
		}
		if n := w.NoteCookieRetry("day"); n != 1 {
			t.Errorf("first retry after the window counted as %d, want 1", n)
		}
	})

	t.Run("a cancel", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		t.Cleanup(w.Stop)
		addSchedJob(t, db, nil, "cancelled", database.StatusCookies, 0)
		spend(w, "cancelled")
		if ok, _, err := w.CancelJob("cancelled"); !ok || err != nil {
			t.Fatalf("CancelJob = %v, %v", ok, err)
		}
		if !w.CookieRetryLeft("cancelled") {
			t.Error("the count outlived the operator's Cancel")
		}
	})

	t.Run("a delete", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		t.Cleanup(w.Stop)
		addSchedJob(t, db, nil, "deleted", database.StatusCookies, 0)
		spend(w, "deleted")
		if err := db.DeleteJob("deleted"); err != nil {
			t.Fatal(err)
		}
		if !w.CookieRetryLeft("deleted") {
			t.Error("the count outlived the deleted row")
		}
	})
}

// TestRefreshInFlightIsNotCalledAFailedRefresh: a job that parks while
// another job's refresh holds the refresh slot was answered NotRestored, and
// the worker logged "the cookie file has to be replaced by hand" for cookies
// that refresh was repairing. It is told the refresh is in flight now, stays
// parked without the advice, and uses none of its retries.
//
// Mutant: the InFlight arm removed — the replace-by-hand advice is logged.
func TestRefreshInFlightIsNotCalledAFailedRefresh(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	log := &captureLogger{}
	w.logger = log
	w.CookieFileInUse = func() string { return "/boot/cookies.txt" }
	const id = "waits_for_refresh"
	addSchedJob(t, db, nil, id, database.StatusUpcoming, 0)
	w.processStreamFn = refusedWithCookies
	w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome { return CookieRefreshInFlight }

	cookieRetryLap(t, w, id)

	if _, logged := log.field("the job stays parked — a cookie refresh is already running, and the job is resumed if it ends with working cookies", "jobID"); !logged {
		t.Error("the in-flight line was not logged")
	}
	for _, line := range log.lines() {
		if msg, _ := line[0].(string); strings.HasPrefix(msg, "auto cookie refresh failed") {
			t.Errorf("a refresh in flight logged %q", msg)
		}
	}
	if row, _ := db.GetJob(id); row.Status != database.StatusCookies {
		t.Errorf("status = %s, want the job still parked", row.Status)
	}
	if n := w.cookieRetryCount(id); n != 0 {
		t.Errorf("%d retries counted for a refresh that did not run, want 0", n)
	}
}

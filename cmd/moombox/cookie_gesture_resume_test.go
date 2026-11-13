package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// gestureResumeState is a runState with a real database and worker, and the
// jobs the tests below park: seed adds one job in COOKIES? under reason.
func gestureResumeState(t *testing.T) (s *runState, seed func(id, platform string, reason database.ParkReason, backlog bool)) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "gesture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log, err := logger.New(filepath.Join(t.TempDir(), "gesture.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	s = &runState{db: db, log: log,
		dlWorker: worker.NewDownloadWorker(db, nil, config.Defaults(), sweepTestLogger{}, nil)}
	seed = func(id, platform string, reason database.ParkReason, backlog bool) {
		t.Helper()
		job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: platform, Status: database.StatusUpcoming}
		if backlog {
			ch := "UC_gesture"
			job.ChannelID, job.QueuePriority = &ch, 1
		}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(id, map[string]any{
			"status": database.StatusCookies, "error": "Login required", "park_reason": reason, "park_identity": "account-A",
		})
	}
	return s, seed
}

func statusOf(t *testing.T, db *database.Database, id string) database.JobStatus {
	t.Helper()
	job, err := db.GetJob(id)
	if err != nil || job == nil {
		t.Fatalf("GetJob %s: %v", id, err)
	}
	return job.Status
}

// TestVerifiedCookieGestureResumesEveryAuthParkedJob: when a job, not the
// 30-minute check, was first to meet a dead session, the repair's re-check
// read "authenticated" against a baseline that never saw the failure and an
// unchanged account, so neither sweep edge fired: only the job that asked
// for the refresh was resumed, and every job that parked while it ran stayed
// in COOKIES? beside healthy cookie badges. A gesture that ends with the
// platform verified now resumes all of them (owner ruling R2-C2): a backlog
// VOD to Queued, the rest to Upcoming, each using one automatic retry. A
// membership park keeps its identity rule, another platform's jobs are not
// touched, and nothing moves for a verdict that is not OK.
//
// Mutants: resumeAfterVerifiedCookies sweeping on RefreshUnknown — the first
// half resumes; the sweep passing an identity — the membership park moves;
// the note dropped from the sweep — the counts stay 0; CookieResumeStatus
// bypassed — the backlog row goes to Upcoming.
func TestVerifiedCookieGestureResumesEveryAuthParkedJob(t *testing.T) {
	s, seed := gestureResumeState(t)
	seed("yt_live", "youtube", database.ParkReasonAuth, false)
	seed("yt_backlog", "youtube", database.ParkReasonAuth, true)
	seed("yt_legacy", "youtube", database.ParkReasonNone, false)
	seed("yt_member", "youtube", database.ParkReasonMembership, false)
	seed("tw_dead", "twitch", database.ParkReasonAuth, false)

	s.resumeAfterVerifiedCookies(cookies.RefreshUnknown, cookies.RefreshFailed, "a test gesture")
	for _, id := range []string{"yt_live", "yt_backlog", "yt_legacy", "yt_member", "tw_dead"} {
		if got := statusOf(t, s.db, id); got != database.StatusCookies {
			t.Fatalf("%s = %s after a gesture that verified nothing, want it still parked", id, got)
		}
	}

	s.resumeAfterVerifiedCookies(cookies.RefreshOK, cookies.RefreshUnknown, "a test gesture")
	for id, want := range map[string]database.JobStatus{
		"yt_live":    database.StatusUpcoming,
		"yt_backlog": database.StatusQueued,
		"yt_legacy":  database.StatusUpcoming,
		"yt_member":  database.StatusCookies,
		"tw_dead":    database.StatusCookies,
	} {
		if got := statusOf(t, s.db, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	retries := s.dlWorker.AdmissionHolds().CookieRetries
	for _, id := range []string{"yt_live", "yt_backlog", "yt_legacy"} {
		if retries[id].Count != 1 {
			t.Errorf("%s: %d automatic retries counted, want 1", id, retries[id].Count)
		}
	}
	if _, counted := retries["yt_member"]; counted {
		t.Error("the membership park was counted")
	}
}

// TestCookieGestureLeavesAJobWithNoRetryLeft: the gesture sweep resumes each
// job within its retry budget (owner rulings R2-C1, R2-C2). Without that,
// every job's refresh would hand one more extraction to every other job the
// platform refuses for a reason cookies cannot fix. The two edge sweeps are
// not gated: a real auth recovery or account change is new information, and
// it resumes the job whatever its count.
//
// Mutants: the gate dropped from sweepCookieParkedJobs — the spent job is
// resumed by the gesture; the edge sweep gated — it is not resumed by the
// recovery.
func TestCookieGestureLeavesAJobWithNoRetryLeft(t *testing.T) {
	s, seed := gestureResumeState(t)
	seed("spent", "youtube", database.ParkReasonAuth, false)
	seed("fresh", "youtube", database.ParkReasonAuth, false)
	for range jobstate.MaxCookieAutoRetries {
		s.dlWorker.NoteCookieRetry("spent")
	}

	s.resumeAfterCookieGesture("youtube", "a test gesture")
	if got := statusOf(t, s.db, "spent"); got != database.StatusCookies {
		t.Errorf("spent = %s, want it left parked by a gesture", got)
	}
	if got := statusOf(t, s.db, "fresh"); got != database.StatusUpcoming {
		t.Errorf("fresh = %s, want it resumed", got)
	}

	if n := sweepCookieParkedJobs(s.db, s.log, nil, "youtube", "", s.cookieBudget(), false); n != 1 {
		t.Errorf("the auth-recovered sweep resumed %d jobs, want the spent one", n)
	}
	if got := statusOf(t, s.db, "spent"); got != database.StatusUpcoming {
		t.Errorf("spent = %s after a real auth recovery, want it resumed", got)
	}
}

// TestAutomaticCookiePassResumesTheJobsItVerified: the periodic headless
// refresh and the boot profile seed hold the refresh slot like any other
// refresh, so a job that failed while one ran was left parked on the promise
// of a resume (worker.CookieRefreshInFlight), and their hook only re-checked:
// nothing resumed it when the pass verified. The hook now sweeps for each
// platform its pass verified (owner ruling R2-C2), and for none once its
// context has ended, which is the shutdown.
//
// Mutants: resumeAfterAutomaticPass sweeping under an ended context — the
// first step resumes; it reading the wrong verdict — the Twitch job moves.
func TestAutomaticCookiePassResumesTheJobsItVerified(t *testing.T) {
	s, seed := gestureResumeState(t)
	seed("yt_parked", "youtube", database.ParkReasonAuth, false)
	seed("tw_parked", "twitch", database.ParkReasonAuth, false)
	verified := cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshOK, Twitch: cookies.RefreshUnknown}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	s.resumeAfterAutomaticPass(ended, verified)
	if got := statusOf(t, s.db, "yt_parked"); got != database.StatusCookies {
		t.Fatalf("yt_parked = %s after a pass whose loop had ended, want it still parked", got)
	}

	s.resumeAfterAutomaticPass(context.Background(), verified)
	if got := statusOf(t, s.db, "yt_parked"); got != database.StatusUpcoming {
		t.Errorf("yt_parked = %s after an automatic pass that verified YouTube, want it resumed", got)
	}
	if got := statusOf(t, s.db, "tw_parked"); got != database.StatusCookies {
		t.Errorf("tw_parked = %s after a pass that could not tell for Twitch, want it still parked", got)
	}
}

// TestCookieRecoveryResumesTheJobsItVerified: the automatic recovery is a
// cookie-writing gesture, so a pass that ran and verified a platform resumes
// the jobs parked on it (owner ruling R2-C2), and a pass that declined to run
// resumes none.
//
// Mutants: the sweep dropped from runCookieRecovery's deferred tail — the job
// stays parked; the tail's Ran gate dropped — the declined pass is unchanged
// here (both its verdicts are Unknown), so that half only guards a decline
// that carries a verdict.
func TestCookieRecoveryResumesTheJobsItVerified(t *testing.T) {
	s, seed := gestureResumeState(t)
	seed("yt_parked", "youtube", database.ParkReasonAuth, false)
	notify := func(string, string, string, notifications.NotificationType) {}

	s.runCookieRecovery(context.Background(), "youtube",
		stubRefreshResult(cookies.RefreshResult{YouTube: cookies.RefreshOK}), notify)
	if got := statusOf(t, s.db, "yt_parked"); got != database.StatusCookies {
		t.Fatalf("yt_parked = %s after a recovery pass that did not run, want it still parked", got)
	}

	s.runCookieRecovery(context.Background(), "youtube",
		stubRefresh(cookies.RefreshOK, cookies.RefreshUnknown), notify)
	if got := statusOf(t, s.db, "yt_parked"); got != database.StatusUpcoming {
		t.Errorf("yt_parked = %s after a recovery that verified YouTube, want it resumed", got)
	}
}

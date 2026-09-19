package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

type sweepTestLogger struct{}

func (sweepTestLogger) Debug(msg string, args ...any) {}
func (sweepTestLogger) Info(msg string, args ...any)  {}
func (sweepTestLogger) Warn(msg string, args ...any)  {}
func (sweepTestLogger) Error(msg string, args ...any) {}

// TestSweepShouldResume pins the eligibility matrix for the COOKIES? recovery
// sweeps. The load-bearing row is the membership one:
//
// A job parks at ParkReasonMembership only when YouTube refused a session it
// had already confirmed was signed in. The auth-recovery sweep fires on a
// not-authenticated → authenticated transition, which by construction cannot
// be the event that fixes that job — the session was authenticated when it
// failed. Resuming it there bought a guaranteed-identical failure plus a full
// extraction attempt on every auth cycle, forever.
//
// The rest of the matrix is the pre-existing behavior, which must not move:
// dead-cookie parks are exactly what the sweep exists for, and a legacy row
// with no recorded reason (every COOKIES? row that predates the park_reason
// column) has to keep being resumed, because nothing can say retroactively
// whether it was a membership problem and stranding a real dead-cookie job is
// the one regression this change must not cause.
func TestSweepShouldResume(t *testing.T) {
	cookieJob := func(platform string, reason database.ParkReason) *database.Job {
		return &database.Job{
			ID: "j", Platform: platform, Status: database.StatusCookies, ParkReason: reason,
		}
	}
	// A membership park always records the account that refused it.
	memberJob := func(platform, parkedUnder string) *database.Job {
		j := cookieJob(platform, database.ParkReasonMembership)
		j.ParkIdentity = parkedUnder
		return j
	}

	cases := []struct {
		name            string
		job             *database.Job
		platform        string
		currentIdentity string
		want            bool
	}{
		{
			name:     "dead cookies resume on auth recovery",
			job:      cookieJob("youtube", database.ParkReasonAuth),
			platform: "youtube",
			want:     true,
		},
		{
			name:     "legacy row with no recorded reason resumes on auth recovery",
			job:      cookieJob("youtube", database.ParkReasonNone),
			platform: "youtube",
			want:     true,
		},
		{
			// THE FIX. Restoring the same account's session cannot add a
			// membership to it.
			name:     "not-a-member does NOT resume on auth recovery",
			job:      memberJob("youtube", "account-A"),
			platform: "youtube",
			want:     false,
		},
		{
			// ...but a genuine change of account is exactly the event that CAN
			// fix it, so the credential sweep does resume it.
			name:            "not-a-member resumes under a different account",
			job:             memberJob("youtube", "account-A"),
			platform:        "youtube",
			currentIdentity: "account-B",
			want:            true,
		},
		{
			// The steady state, and the one that makes this durable rather
			// than edge-triggered: re-evaluating against the SAME account is
			// free and must never move the job, however often it happens.
			name:            "not-a-member stays parked under the same account",
			job:             memberJob("youtube", "account-A"),
			platform:        "youtube",
			currentIdentity: "account-A",
			want:            false,
		},
		{
			// A membership park with no recorded account (pre-v19 row, or the
			// fingerprint could not be computed) resolves permissively: one
			// retry beats a permanent strand.
			name:            "not-a-member with unknown parked account resumes",
			job:             memberJob("youtube", ""),
			platform:        "youtube",
			currentIdentity: "account-B",
			want:            true,
		},
		{
			name:            "dead cookies also resume on a credential observation",
			job:             cookieJob("youtube", database.ParkReasonAuth),
			platform:        "youtube",
			currentIdentity: "account-B",
			want:            true,
		},
		{
			name:     "other platform is never touched by this platform's sweep",
			job:      cookieJob("twitch", database.ParkReasonAuth),
			platform: "youtube",
			want:     false,
		},
		{
			name:            "other platform is not touched by a credential observation either",
			job:             memberJob("twitch", "account-A"),
			platform:        "youtube",
			currentIdentity: "account-B",
			want:            false,
		},
		{
			name:     "a running job is never resumed",
			job:      &database.Job{ID: "j", Platform: "youtube", Status: database.StatusDownloading},
			platform: "youtube",
			want:     false,
		},
		{
			name:     "a cancelled job is a human decision — not overridden",
			job:      &database.Job{ID: "j", Platform: "youtube", Status: database.StatusCancelled},
			platform: "youtube",
			want:     false,
		},
		{
			name:     "a generic Error job is not a cookie park",
			job:      &database.Job{ID: "j", Platform: "youtube", Status: database.StatusError},
			platform: "youtube",
			want:     false,
		},
		{
			name:     "nil job",
			job:      nil,
			platform: "youtube",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sweepShouldResume(tc.job, tc.platform, tc.currentIdentity); got != tc.want {
				t.Errorf("sweepShouldResume = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResumeCookieParkedJobs drives the predicate through the real database
// loop the callbacks use, so a future edit cannot satisfy the table above
// while the loop ignores it (the loop is what actually ran in the field).
func TestResumeCookieParkedJobs(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "sweep.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	seed := func(id, platform string, status database.JobStatus, reason database.ParkReason) {
		t.Helper()
		if _, err := db.AddJob(&database.Job{
			ID: id, VideoID: id, URL: "https://example.invalid/" + id,
			Platform: platform, Status: status,
		}); err != nil {
			t.Fatalf("AddJob %s: %v", id, err)
		}
		if status == database.StatusCookies {
			fields := map[string]any{
				"error":       "parked: " + string(reason),
				"park_reason": reason,
			}
			if reason == database.ParkReasonMembership {
				fields["park_identity"] = "account-A"
			}
			db.UpdateJobFields(id, fields)
		}
	}

	seed("yt_member", "youtube", database.StatusCookies, database.ParkReasonMembership)
	seed("yt_dead", "youtube", database.StatusCookies, database.ParkReasonAuth)
	seed("yt_legacy", "youtube", database.StatusCookies, database.ParkReasonNone)
	seed("tw_dead", "twitch", database.StatusCookies, database.ParkReasonAuth)
	seed("yt_running", "youtube", database.StatusDownloading, database.ParkReasonNone)

	mustStatus := func(id string, want database.JobStatus) *database.Job {
		t.Helper()
		got, err := db.GetJob(id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("%s status = %q, want %q", id, got.Status, want)
		}
		return got
	}

	// --- Auth recovery (no identity on offer): dead cookies wake, the
	// membership park does not. ---
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, nil, "youtube", ""); n != 2 {
		t.Errorf("auth-recovery sweep resumed %d jobs, want 2 (dead + legacy)", n)
	}

	member := mustStatus("yt_member", database.StatusCookies)
	if member.ParkReason != database.ParkReasonMembership {
		t.Errorf("yt_member ParkReason = %q, want membership (must survive the sweep)", member.ParkReason)
	}
	if member.Error == "" {
		t.Error("yt_member lost its error text — the user can no longer see why it is parked")
	}

	for _, id := range []string{"yt_dead", "yt_legacy"} {
		got := mustStatus(id, database.StatusUpcoming)
		if got.ParkReason != database.ParkReasonNone {
			t.Errorf("%s ParkReason = %q, want cleared on resume", id, got.ParkReason)
		}
		if got.Error != "" {
			t.Errorf("%s Error = %q, want cleared on resume", id, got.Error)
		}
	}

	mustStatus("tw_dead", database.StatusCookies)
	mustStatus("yt_running", database.StatusDownloading)

	// --- Same account observed: still nothing. This is what makes the
	// mechanism safe to run on every check rather than only on an edge. ---
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, nil, "youtube", "account-A"); n != 0 {
		t.Errorf("sweep under the SAME account resumed %d jobs, want 0", n)
	}
	mustStatus("yt_member", database.StatusCookies)

	// --- A different account: now the membership park is eligible. ---
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, nil, "youtube", "account-B"); n != 1 {
		t.Errorf("different-account sweep resumed %d jobs, want 1 (the membership park)", n)
	}
	got := mustStatus("yt_member", database.StatusUpcoming)
	if got.ParkReason != database.ParkReasonNone {
		t.Errorf("yt_member ParkReason = %q, want cleared on resume", got.ParkReason)
	}
	if got.ParkIdentity != "" {
		t.Errorf("yt_member ParkIdentity = %q, want cleared on resume — a stale one would fake an account change", got.ParkIdentity)
	}
	mustStatus("tw_dead", database.StatusCookies)
}

// TestMembershipParkSurvivesRestart is the durability property the per-job
// comparison buys, and the reason it is preferred over a process-local
// "did the identity change since last check" edge.
//
// A restart resets any in-process baseline. The parked job still carries the
// account that refused it, so the first observation after the restart decides
// correctly in both directions: unchanged cookies leave it alone, an offline
// swap (stop Moombox, replace cookies, start) resumes it. An edge-triggered
// design sees neither, because a restart produces no transition at all.
func TestMembershipParkSurvivesRestart(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.AddJob(&database.Job{
		ID: "yt_r", VideoID: "r", URL: "u", Platform: "youtube", Status: database.StatusCookies,
	}); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("yt_r", map[string]any{
		"park_reason":   database.ParkReasonMembership,
		"park_identity": "account-A",
	})

	// Restart with the SAME cookies: the first observation must be a no-op.
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, nil, "youtube", "account-A"); n != 0 {
		t.Errorf("first observation after a same-cookies restart resumed %d jobs, want 0", n)
	}

	// Restart after an OFFLINE cookie swap: the first observation must resume.
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, nil, "youtube", "account-B"); n != 1 {
		t.Errorf("first observation after an offline account swap resumed %d jobs, want 1 — "+
			"this is the case a process-local edge can never see", n)
	}
}

// TestResumeCookieParkedJobs_RespectsQueuePriority is MON-4. A cookie repair
// used to bounce EVERY parked row straight to Upcoming, which the worker's
// heartbeat poller then processes — so a channel's whole members-only backlog
// was released at once, bypassing the archive-slots pacing that exists to stop
// exactly that. CountBacklogInFlight then over-counts and blocks further
// admission until they drain.
//
// Mutants:
//   - send priority-1 rows to Upcoming -> the backlog row's status is wrong.
//   - send priority-0 rows to Queued -> a live/upcoming job would be stranded:
//     the scheduler only admits rows that have a channel_id and priority 1.
//   - drop the wake call -> wakes == 0 and the resumed backlog waits up to
//     60 s for the heartbeat.
func TestResumeCookieParkedJobs_RespectsQueuePriority(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "priority.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	chID := "UC_park"
	for _, j := range []*database.Job{
		{ID: "backlog1", VideoID: "backlog1", URL: "u", Platform: "youtube",
			Status: database.StatusCookies, ChannelID: &chID, QueuePriority: 1},
		{ID: "broadcast1", VideoID: "broadcast1", URL: "u", Platform: "youtube",
			Status: database.StatusCookies, ChannelID: &chID, QueuePriority: 0},
	} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	// The feed_items partner NextQueuedJobs INNER-JOINs on. Every priority-1
	// row the archival pass creates has one, so this is the ordinary shape;
	// the row WITHOUT one has its own test below (a removed channel's
	// still-running backlog download).
	seedFeedPartner(t, db, chID, "backlog1")
	db.UpdateJobFields("backlog1", map[string]any{"error": "parked: auth"})

	var wakes int
	resumed := resumeCookieParkedJobs(db, sweepTestLogger{}, func() { wakes++ }, "youtube", "")
	if resumed != 2 {
		t.Fatalf("resumed = %d, want 2", resumed)
	}

	backlog, _ := db.GetJob("backlog1")
	if backlog.Status != database.StatusQueued {
		t.Errorf("priority-1 row resumed to %q, want %q — going straight to Upcoming releases the whole backlog at once and bypasses archive-slots",
			backlog.Status, database.StatusQueued)
	}
	broadcast, _ := db.GetJob("broadcast1")
	if broadcast.Status != database.StatusUpcoming {
		t.Errorf("priority-0 row resumed to %q, want %q — the scheduler never admits a priority-0 row, so Queued would strand it",
			broadcast.Status, database.StatusUpcoming)
	}
	if wakes == 0 {
		t.Error("the scheduler was not woken — the resumed backlog waits up to 60 s for the heartbeat")
	}
	if wakes != 1 {
		t.Errorf("wake called %d times, want exactly 1 — Wake coalesces into a capacity-1 channel, so one signal per sweep is all it can use", wakes)
	}
	if backlog.ParkReason != database.ParkReasonNone || backlog.ParkIdentity != "" || backlog.Error != "" {
		t.Errorf("the park fields were not cleared on the Queued arm: %+v", backlog)
	}
}

// TestResumeNotificationsSayTheBacklogIsPaced is the Task 2 review's M-2.
//
// MON-4 changed what the resumed count MEANS on the most visible surface
// Moombox has. Before it, a cookie repair sent every parked row to Upcoming
// and the worker's heartbeat poller started them all, so "Resumed 40 job(s)"
// and "40 downloads are starting" were the same sentence. Now a channel's
// parked backlog returns to Queued and drains archive_slots at a time, so an
// operator with 40 parked rows and archive_slots = 3 reads "Resumed 40 job(s)"
// and then watches three downloads start. The count is still exactly right;
// what it IMPLIES is not, and the notification text is the only thing that can
// say so — the adjacent log lines are accurate as they stand.
//
// Both bodies are pinned here rather than through notifyMgr because
// notifications.Manager builds its targets from config and accepts Discord
// webhook URLs only (notifications.parseTarget), so there is no fake target a
// cmd/moombox test can install; the constants are the seam.
//
// Mutant: drop the pacing clause from either body -> the operator is handed a
// number with nothing to reconcile it against when the dashboard starts three.
func TestResumeNotificationsSayTheBacklogIsPaced(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"auth recovered", fmt.Sprintf(authRecoveredResumedBody, 40, "youtube")},
		{"credentials observed", fmt.Sprintf(credentialsObservedResumedBody, 40, "youtube")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.body, "40 job(s)") {
				t.Errorf("%q does not report the resumed count", tc.body)
			}
			if !strings.Contains(tc.body, "youtube") {
				t.Errorf("%q does not name the platform — both platforms park jobs and the operator has to know which repaired", tc.body)
			}
			if !strings.Contains(tc.body, "archive_slots") {
				t.Errorf("%q does not say the resumed backlog is PACED — an operator reading a bare count expects that many downloads to start, and since MON-4 they do not", tc.body)
			}
		})
	}
}

// seedFeedPartner writes the feed_items row NextQueuedJobs INNER-JOINs against,
// so a priority-1 job created here has the partner the archival pass would have
// given it.
func seedFeedPartner(t *testing.T, db *database.Database, channelID, videoID string) {
	t.Helper()
	if _, err := db.UpsertFeedItem(database.FeedItem{
		ChannelID: channelID, VideoID: videoID, Title: videoID,
		Published: "2026-01-01T00:00:00Z", DatePrecision: "exact",
		CatalogPos: 1, Source: "rss", FirstSeen: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedItem(%s, %s): %v", channelID, videoID, err)
	}
}

// TestResumeCookieParkedJobs_APartnerlessBacklogRowGoesToUpcoming is the close
// review's B-1: MON-4's Queued arm strands a removed channel's parked backlog
// with no path out and nothing saying so.
//
// The sequence, every step an ordinary operator gesture:
//
//  1. CancelAndPrune (channel REMOVAL) deletes the channel's never-started
//     jobs — {Queued, Upcoming, COOKIES?} — and then its feed_items rows. A
//     job that was DOWNLOADING is deliberately left running, so it survives
//     with no feed_items partner.
//  2. That download hits an auth wall and parks in COOKIES?.
//  3. Cookies are repaired, the sweep runs, and priority 1 means Queued.
//  4. NextQueuedJobs INNER-JOINs feed_items, so the scheduler returns zero
//     rows for it on every sweep, forever — silently, because an empty answer
//     is indistinguishable from "nothing to admit".
//  5. /retry and /resume accept Error | Cancelled | COOKIES? only, and
//     ShouldProcess(Queued) is false, so neither the operator nor startup
//     recovery nor the heartbeat can move it. Delete-and-re-add is the only
//     way out and nothing says so.
//
// So a priority-1 row goes to Queued only when it HAS a partner; without one
// it takes the pre-MON-4 path to Upcoming, which the heartbeat poller drains.
// Pacing is not the property at stake for a channel that no longer exists.
//
// Mutants:
//   - drop the partner check -> the partnerless row lands in Queued and
//     NextQueuedJobs returns nothing for it: the strand, reproduced.
//   - check the partner for priority-0 rows too -> harmless but pointless; the
//     sibling assertions below still pass, which is why the Queued row is
//     asserted through NextQueuedJobs rather than through its status alone.
func TestResumeCookieParkedJobs_APartnerlessBacklogRowGoesToUpcoming(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "strand.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	chID := "UC_removed"
	for _, j := range []*database.Job{
		{ID: "orphan", VideoID: "orphan", URL: "u", Platform: "youtube",
			Status: database.StatusCookies, ChannelID: &chID, QueuePriority: 1},
		{ID: "partnered", VideoID: "partnered", URL: "u", Platform: "youtube",
			Status: database.StatusCookies, ChannelID: &chID, QueuePriority: 1},
	} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	// Only the second one keeps its feed row: the first is the job whose
	// channel was removed mid-download.
	seedFeedPartner(t, db, chID, "partnered")

	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, func() {}, "youtube", ""); n != 2 {
		t.Fatalf("resumed = %d, want 2", n)
	}

	orphan, _ := db.GetJob("orphan")
	if orphan.Status != database.StatusUpcoming {
		t.Errorf("a priority-1 row with no feed_items partner resumed to %q, want %q — "+
			"NextQueuedJobs INNER-JOINs feed_items, /retry and /resume both refuse Queued, and "+
			"nothing else moves a Queued row, so this row would be lost silently and permanently",
			orphan.Status, database.StatusUpcoming)
	}
	partnered, _ := db.GetJob("partnered")
	if partnered.Status != database.StatusQueued {
		t.Errorf("a priority-1 row WITH its partner resumed to %q, want %q — the partner check must "+
			"not cost the ordinary backlog row its archive-slots pacing", partnered.Status, database.StatusQueued)
	}
	for _, id := range []string{"orphan", "partnered"} {
		j, _ := db.GetJob(id)
		if j.ParkReason != database.ParkReasonNone || j.ParkIdentity != "" || j.Error != "" {
			t.Errorf("%s: the park fields were not cleared: %+v", id, j)
		}
	}

	// The strand, stated the way the scheduler sees it: the admitted set holds
	// the partnered row and nothing else. With the partner check dropped the
	// orphan is Queued too and this answer is still [partnered] — which is the
	// whole defect: the row is in Queued and the scheduler cannot see it.
	admit, err := db.NextQueuedJobs(chID, 10)
	if err != nil {
		t.Fatalf("NextQueuedJobs: %v", err)
	}
	if len(admit) != 1 || admit[0] != "partnered" {
		t.Errorf("NextQueuedJobs = %v, want exactly [partnered] — a Queued row the scheduler can "+
			"never admit has no exit at all", admit)
	}
}

package main

import (
	"path/filepath"
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
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, "youtube", ""); n != 2 {
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
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, "youtube", "account-A"); n != 0 {
		t.Errorf("sweep under the SAME account resumed %d jobs, want 0", n)
	}
	mustStatus("yt_member", database.StatusCookies)

	// --- A different account: now the membership park is eligible. ---
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, "youtube", "account-B"); n != 1 {
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
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, "youtube", "account-A"); n != 0 {
		t.Errorf("first observation after a same-cookies restart resumed %d jobs, want 0", n)
	}

	// Restart after an OFFLINE cookie swap: the first observation must resume.
	if n := resumeCookieParkedJobs(db, sweepTestLogger{}, "youtube", "account-B"); n != 1 {
		t.Errorf("first observation after an offline account swap resumed %d jobs, want 1 — "+
			"this is the case a process-local edge can never see", n)
	}
}

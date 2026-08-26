package worker

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestMembersOnlyDistinguishesSessionAuth is the diagnosability fix: a
// members-only verdict on a session YouTube confirmed was SIGNED IN is a
// membership problem, not a cookie problem, and blaming cookies for it sent
// a Docker operator round a refresh loop that could never have helped.
//
// The three session states must produce three different stories:
//   - signed in  → the account is not a member (wrong account of several?)
//   - signed out → the cookies are missing/dead, refresh them
//   - unknown    → the pre-existing generic wording, unchanged
func TestMembersOnlyDistinguishesSessionAuth(t *testing.T) {
	sp := &StreamProcessor{}
	const reason = "Join this channel to get access to members-only content"

	tests := []struct {
		name         string
		sessionAuth  youtube.SessionAuthState
		wantSentinel error
		wantFrags    []string
		notFrags     []string
	}{
		{
			// "cookies are alive" is the load-bearing half: it tells the
			// operator to stop looking at credentials. The remedy must name a
			// browser-side account switch, because a Netscape export carries
			// whatever account the profile is on — there is no account choice
			// at export time.
			name:         "signed in → not a member, do not blame cookies",
			sessionAuth:  youtube.SessionAuthLoggedIn,
			wantSentinel: ErrNotAMember,
			wantFrags:    []string{"Member-only", reason, "cookies are alive", "not a member", "switch the browser to the account"},
			notFrags:     []string{"not signed in", "session is dead"},
		},
		{
			name:         "signed out → cookies are dead",
			sessionAuth:  youtube.SessionAuthLoggedOut,
			wantSentinel: ErrCookiesRequired,
			wantFrags:    []string{"Member-only", reason, "not signed in"},
			notFrags:     []string{"not a member"},
		},
		{
			name:         "unknown → unchanged generic wording",
			sessionAuth:  youtube.SessionAuthUnknown,
			wantSentinel: ErrCookiesRequired,
			wantFrags:    []string{"Member-only", reason},
			notFrags:     []string{"not a member", "not signed in"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, sentinel := sp.checkPlayability(&youtube.VideoInfo{
				PlayabilityError:  youtube.PlayabilityMembersOnly,
				PlayabilityReason: reason,
				SessionAuth:       tt.sessionAuth,
			})
			if sentinel != tt.wantSentinel {
				t.Errorf("sentinel = %v, want %v", sentinel, tt.wantSentinel)
			}
			for _, frag := range tt.wantFrags {
				if !strings.Contains(msg, frag) {
					t.Errorf("message %q should contain %q", msg, frag)
				}
			}
			for _, frag := range tt.notFrags {
				if strings.Contains(msg, frag) {
					t.Errorf("message %q should NOT contain %q", msg, frag)
				}
			}
		})
	}
}

// TestNotAMemberStillParksAtCookiesStatus: supplying credentials for an
// account that HOLDS the membership is still the fix, so the job belongs in
// COOKIES? (where the auth-recovered sweep can retry it), not in a generic
// Error. Only the *automatic browser refresh* is pointless here — see
// TestNotAMemberSuppressesAutoRefresh.
func TestNotAMemberStillParksAtCookiesStatus(t *testing.T) {
	if !cookiesStatusError(ErrNotAMember) {
		t.Error("cookiesStatusError(ErrNotAMember) = false, want true")
	}
	wrapped := (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrNotAMember}).AsError()
	if !cookiesStatusError(wrapped) {
		t.Error("cookiesStatusError on the AsError-wrapped form = false, want true")
	}
}

// TestNotAMemberSuppressesAutoRefresh pins the predicate setJobError uses to
// decide whether the automatic cookie refresh is worth attempting. Rotating
// a live, valid session cannot add a channel membership to it, so the
// attempt (and its dead-end "re-run setup" advice) must not fire.
func TestNotAMemberSuppressesAutoRefresh(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"not-a-member", ErrNotAMember, false},
		{"not-a-member wrapped", (&StreamProcessResult{Error: "m", ErrSentinel: ErrNotAMember}).AsError(), false},
		{"plain cookies-required", ErrCookiesRequired, true},
		{"twitch auth expired", errors.New("boom"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cookieRefreshWorthAttempting(tc.err); got != tc.want {
				t.Errorf("cookieRefreshWorthAttempting = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSetJobErrorCookieRefreshWiring drives the predicate through the real
// call site. Two things are pinned:
//
//  1. ErrNotAMember parks the job at COOKIES? but must NOT fire the
//     automatic refresh (the session is alive; rotating it adds nothing).
//  2. A cookie failure fires the refresh even with no notifier configured.
//     The attempt used to sit inside the `w.notifier != nil` branch, so a
//     deployment without webhooks silently got neither the recovery nor any
//     log line explaining its absence.
//  3. The job's PLATFORM reaches the callback. The callback used to take no
//     arguments, so it could only report whether any platform ended up
//     authenticated — and a healthy Twitch sent a YouTube job back to
//     Upcoming to re-probe into a guaranteed-identical failure. The twitch
//     row below is what stops a future edit from hardcoding "youtube".
func TestSetJobErrorCookieRefreshWiring(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		platform    string
		wantCalled  bool
		wantStatus  database.JobStatus
		jobIDSuffix string
	}{
		{
			name:        "members-only on a live session skips the refresh",
			err:         (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrNotAMember}).AsError(),
			platform:    "youtube",
			wantCalled:  false,
			wantStatus:  database.StatusCookies,
			jobIDSuffix: "notmember",
		},
		{
			name:        "dead cookies fire the refresh with no notifier wired",
			err:         (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrCookiesRequired}).AsError(),
			platform:    "youtube",
			wantCalled:  true,
			wantStatus:  database.StatusCookies,
			jobIDSuffix: "deadcookies",
		},
		{
			name:        "a twitch job asks about twitch, not about youtube",
			err:         (&StreamProcessResult{Error: "auth expired", ErrSentinel: ErrCookiesRequired}).AsError(),
			platform:    "twitch",
			wantCalled:  true,
			wantStatus:  database.StatusCookies,
			jobIDSuffix: "twitchcookies",
		},
		{
			name:        "non-cookie failure never consults the refresh",
			err:         errors.New("ffmpeg exploded"),
			wantCalled:  false,
			wantStatus:  database.StatusError,
			jobIDSuffix: "generic",
		},
		{
			// A multi-%w error can satisfy cookiesStatusError AND
			// ErrNonActionable at once: stream_processor_twitch.go's probe
			// give-up wraps the underlying error alongside ErrNonActionable,
			// and that underlying error can carry twitch.ErrTwitchAuthExpired.
			// ErrNonActionable means "terminal, stop working this job", so the
			// refresh must not fire — it would set the job back to Upcoming,
			// re-enqueue it, and restart the probe budget from zero.
			//
			// Unreachable today only because classifyProbeErr's default routes
			// "gql auth failure (401)" to the network class, which is a string
			// heuristic over a Twitch response body, not an invariant.
			name: "terminal non-actionable error never resurrects the job",
			err: fmt.Errorf("max probe errors: %w (%w)",
				fmt.Errorf("gql auth failure (401): %w", twitch.ErrTwitchAuthExpired),
				ErrNonActionable),
			platform:    "youtube",
			wantCalled:  false,
			wantStatus:  database.StatusCookies,
			jobIDSuffix: "terminalauth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			jobID := "yt_" + tc.jobIDSuffix
			job := &database.Job{
				ID:       jobID,
				VideoID:  tc.jobIDSuffix,
				URL:      "https://youtube.com/watch?v=" + tc.jobIDSuffix,
				Platform: tc.platform,
				Status:   database.StatusDownloading,
			}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}

			called := false
			gotPlatform := ""
			w.OnCookieRefreshNeeded = func(platform string) bool {
				called = true
				gotPlatform = platform
				return false
			}

			w.setJobError(job, tc.err)

			if called != tc.wantCalled {
				t.Errorf("OnCookieRefreshNeeded called = %v, want %v", called, tc.wantCalled)
			}
			if called && gotPlatform != tc.platform {
				t.Errorf("OnCookieRefreshNeeded got platform %q, want %q — a refresh answered for "+
					"the wrong platform sends the job back to re-probe into the same failure", gotPlatform, tc.platform)
			}
			got, err := db.GetJob(jobID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
		})
	}
}

// TestParkReasonForError pins the sentinel → database.ParkReason mapping that
// setJobError persists. This is the durable record of WHY a job stopped at
// COOKIES?, and the auth-recovery sweep in cmd/moombox keys on it. The whole
// point is to keep that decision off the job's error TEXT, which is prose the
// user reads and which has already been reworded once.
func TestParkReasonForError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want database.ParkReason
	}{
		{
			// The session was alive and YouTube still said no. Rotating or
			// restoring THESE credentials cannot help; only a different
			// account can.
			name: "not-a-member",
			err:  ErrNotAMember,
			want: database.ParkReasonMembership,
		},
		{
			name: "not-a-member wrapped by AsError",
			err:  (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrNotAMember}).AsError(),
			want: database.ParkReasonMembership,
		},
		{
			name: "cookies required",
			err:  ErrCookiesRequired,
			want: database.ParkReasonAuth,
		},
		{
			name: "twitch auth expired",
			err:  fmt.Errorf("gql: %w", twitch.ErrTwitchAuthExpired),
			want: database.ParkReasonAuth,
		},
		{
			// Ambiguous by construction: usher's 403 does not say whether the
			// session was anonymous or simply un-entitled, so supplying
			// working credentials may well be the fix. It must keep being
			// swept — classifying it as "membership" would strand the
			// anonymous case.
			name: "twitch subscriber-only",
			err:  fmt.Errorf("usher: %w", twitch.ErrSubscriberOnly),
			want: database.ParkReasonAuth,
		},
		{
			// Not a cookie park at all — the job goes to Error, and the reason
			// must be cleared rather than left carrying a stale value.
			name: "unrelated failure",
			err:  errors.New("ffmpeg exploded"),
			want: database.ParkReasonNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parkReasonForError(tc.err); got != tc.want {
				t.Errorf("parkReasonForError = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetJobErrorPersistsParkReason drives the mapping through the real call
// site: the reason must reach the database, because the sweep reads it from
// there long after this process may have restarted.
func TestSetJobErrorPersistsParkReason(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus database.JobStatus
		wantReason database.ParkReason
		id         string
	}{
		{
			name:       "members-only on a live session records membership",
			err:        (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrNotAMember}).AsError(),
			wantStatus: database.StatusCookies,
			wantReason: database.ParkReasonMembership,
			id:         "prnotmember",
		},
		{
			name:       "dead cookies record auth",
			err:        (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrCookiesRequired}).AsError(),
			wantStatus: database.StatusCookies,
			wantReason: database.ParkReasonAuth,
			id:         "prdeadcookies",
		},
		{
			name:       "generic failure records no reason",
			err:        errors.New("ffmpeg exploded"),
			wantStatus: database.StatusError,
			wantReason: database.ParkReasonNone,
			id:         "prgeneric",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			jobID := "yt_" + tc.id
			job := &database.Job{
				ID:       jobID,
				VideoID:  tc.id,
				URL:      "https://youtube.com/watch?v=" + tc.id,
				Platform: "youtube",
				Status:   database.StatusDownloading,
			}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			w.OnCookieRefreshNeeded = func(string) bool { return false }

			w.setJobError(job, tc.err)

			got, err := db.GetJob(jobID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.ParkReason != tc.wantReason {
				t.Errorf("park_reason = %q, want %q", got.ParkReason, tc.wantReason)
			}
		})
	}
}

// TestSetJobErrorOverwritesStaleParkReason: a job that parked as "membership",
// was retried, and then failed for a different reason must not keep carrying
// the old classification — a stale "membership" would suppress a legitimate
// auth-recovery resume for the rest of that job's life.
func TestSetJobErrorOverwritesStaleParkReason(t *testing.T) {
	w, db := testWorkerSetup(t)
	const jobID = "yt_prstale"
	if _, err := db.AddJob(&database.Job{
		ID: jobID, VideoID: "prstale", URL: "u", Platform: "youtube",
		Status: database.StatusDownloading,
	}); err != nil {
		t.Fatal(err)
	}
	w.OnCookieRefreshNeeded = func(string) bool { return false }

	w.setJobError(&database.Job{ID: jobID, Platform: "youtube"},
		(&StreamProcessResult{Error: "m", ErrSentinel: ErrNotAMember}).AsError())
	if got, _ := db.GetJob(jobID); got.ParkReason != database.ParkReasonMembership {
		t.Fatalf("setup: park_reason = %q, want membership", got.ParkReason)
	}

	w.setJobError(&database.Job{ID: jobID, Platform: "youtube"},
		(&StreamProcessResult{Error: "m", ErrSentinel: ErrCookiesRequired}).AsError())
	if got, _ := db.GetJob(jobID); got.ParkReason != database.ParkReasonAuth {
		t.Errorf("park_reason = %q, want auth — a stale membership reason would suppress the sweep forever", got.ParkReason)
	}
}

// TestNotAMemberDistinctFromOtherSentinels: ErrNotAMember must be its own
// identity, or the suppression above would silently disable the refresh for
// every cookie failure.
func TestNotAMemberDistinctFromOtherSentinels(t *testing.T) {
	if errors.Is(ErrCookiesRequired, ErrNotAMember) {
		t.Error("ErrCookiesRequired should not match ErrNotAMember")
	}
	if errors.Is(ErrNotAMember, ErrNonActionable) {
		t.Error("ErrNotAMember should not match ErrNonActionable")
	}
}

// TestSetJobErrorRecordsParkIdentity: a membership park must record WHICH
// account refused it. That value is what lets the credential sweep decide,
// durably and after any number of restarts, whether the account has actually
// changed — without it the sweep can only guess.
//
// It is captured for membership parks ONLY. On any other failure the account
// is not what blocked the job, and a recorded identity there would later read
// as a meaningless "account changed" signal.
func TestSetJobErrorRecordsParkIdentity(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		id       string
		wantID   string
		wantCall bool
	}{
		{
			name:     "membership park records the current account",
			err:      (&StreamProcessResult{Error: "m", ErrSentinel: ErrNotAMember}).AsError(),
			id:       "pidmember",
			wantID:   "account-A",
			wantCall: true,
		},
		{
			name:     "dead-cookie park records nothing",
			err:      (&StreamProcessResult{Error: "m", ErrSentinel: ErrCookiesRequired}).AsError(),
			id:       "pidauth",
			wantID:   "",
			wantCall: false,
		},
		{
			name:     "generic failure records nothing",
			err:      errors.New("ffmpeg exploded"),
			id:       "pidgeneric",
			wantID:   "",
			wantCall: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			jobID := "yt_" + tc.id
			if _, err := db.AddJob(&database.Job{
				ID: jobID, VideoID: tc.id, URL: "u", Platform: "youtube",
				Status: database.StatusDownloading,
			}); err != nil {
				t.Fatal(err)
			}
			w.OnCookieRefreshNeeded = func(string) bool { return false }

			called := false
			w.CurrentCredentialIdentity = func(platform string) string {
				called = true
				if platform != "youtube" {
					t.Errorf("identity requested for platform %q, want youtube", platform)
				}
				return "account-A"
			}

			w.setJobError(&database.Job{ID: jobID, Platform: "youtube"}, tc.err)

			if called != tc.wantCall {
				t.Errorf("CurrentCredentialIdentity called = %v, want %v", called, tc.wantCall)
			}
			got, err := db.GetJob(jobID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ParkIdentity != tc.wantID {
				t.Errorf("park_identity = %q, want %q", got.ParkIdentity, tc.wantID)
			}
		})
	}
}

// TestSetJobErrorParkIdentityNilSlotIsSafe: the callback is optional (tests,
// and any wiring that has no jar). A nil slot must record "" rather than
// panic — "" resolves permissively at the sweep, which is the right default
// for "we could not tell".
func TestSetJobErrorParkIdentityNilSlotIsSafe(t *testing.T) {
	w, db := testWorkerSetup(t)
	const jobID = "yt_pidnil"
	if _, err := db.AddJob(&database.Job{
		ID: jobID, VideoID: "pidnil", URL: "u", Platform: "youtube",
		Status: database.StatusDownloading,
	}); err != nil {
		t.Fatal(err)
	}
	w.OnCookieRefreshNeeded = func(string) bool { return false }
	w.CurrentCredentialIdentity = nil

	w.setJobError(&database.Job{ID: jobID, Platform: "youtube"},
		(&StreamProcessResult{Error: "m", ErrSentinel: ErrNotAMember}).AsError())

	got, err := db.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParkReason != database.ParkReasonMembership {
		t.Errorf("park_reason = %q, want membership", got.ParkReason)
	}
	if got.ParkIdentity != "" {
		t.Errorf("park_identity = %q, want empty", got.ParkIdentity)
	}
}

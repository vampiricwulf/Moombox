package worker

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
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
			name:         "signed in → not a member, do not blame cookies",
			sessionAuth:  youtube.SessionAuthLoggedIn,
			wantSentinel: ErrNotAMember,
			wantFrags:    []string{"Member-only", reason, "signed in", "not a member"},
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
func TestSetJobErrorCookieRefreshWiring(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantCalled  bool
		wantStatus  database.JobStatus
		jobIDSuffix string
	}{
		{
			name:        "members-only on a live session skips the refresh",
			err:         (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrNotAMember}).AsError(),
			wantCalled:  false,
			wantStatus:  database.StatusCookies,
			jobIDSuffix: "notmember",
		},
		{
			name:        "dead cookies fire the refresh with no notifier wired",
			err:         (&StreamProcessResult{Error: "Member-only: x", ErrSentinel: ErrCookiesRequired}).AsError(),
			wantCalled:  true,
			wantStatus:  database.StatusCookies,
			jobIDSuffix: "deadcookies",
		},
		{
			name:        "non-cookie failure never consults the refresh",
			err:         errors.New("ffmpeg exploded"),
			wantCalled:  false,
			wantStatus:  database.StatusError,
			jobIDSuffix: "generic",
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
				Platform: "youtube",
				Status:   database.StatusDownloading,
			}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}

			called := false
			w.OnCookieRefreshNeeded = func() bool {
				called = true
				return false
			}

			w.setJobError(job, tc.err)

			if called != tc.wantCalled {
				t.Errorf("OnCookieRefreshNeeded called = %v, want %v", called, tc.wantCalled)
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

package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A private stream's refusal is the same for everyone signed out, including an
// account that was granted access whose cookies have lapsed. So a private
// verdict on a request that sent the jar's cookies and was still answered as
// signed out parks in COOKIES?, where the refresh and the sweep can bring it
// back. Any other private verdict is plain: Error, nothing to repair.

// privateTo is privateAnswer as YouTube answered a session it read as auth,
// made with the jar's cookies or without.
func privateTo(auth youtube.SessionAuthState, credentialed bool) *youtube.VideoInfo {
	info := privateAnswer()
	info.SessionAuth, info.SessionCredentialed = auth, credentialed
	return info
}

// Mutants: the private arm not reading SessionAuth (the signed-in and unknown
// rows park); not reading the credentialed fact (the signed-out install's row
// parks, on every public page).
func TestAPrivateVerdictParksOnlyASessionThatLooksDead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		info         *youtube.VideoInfo
		wantSentinel error
		wantMsg      string
	}{
		{"cookies sent, read signed out", privateTo(youtube.SessionAuthLoggedOut, true), ErrCookiesRequired, "signed out"},
		{"cookies sent, read signed in", privateTo(youtube.SessionAuthLoggedIn, true), nil, "Private video"},
		{"cookies sent, no word on the session", privateTo(youtube.SessionAuthUnknown, true), nil, "Private video"},
		{"no cookies, read signed out", privateTo(youtube.SessionAuthLoggedOut, false), nil, "Private video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, sentinel := playabilityVerdict(tc.info)
			if !errors.Is(sentinel, tc.wantSentinel) || (tc.wantSentinel == nil && sentinel != nil) {
				t.Errorf("sentinel = %v, want %v", sentinel, tc.wantSentinel)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("message %q does not say %q", msg, tc.wantMsg)
			}
		})
	}
}

// At Process, a backlog VOD's re-extraction and mid-capture, the verdict goes
// where a credential failure goes. Mid-capture keeps a capture with formats.
//
// Mutants: the private arm's sentinel dropped (every park row); credentialVerdict
// ignoring the format count (the formats row stops a capture that can carry on).
func TestADeadSessionsPrivateVerdictParksWhereACredentialFailureDoes(t *testing.T) {
	dead := func() *youtube.VideoInfo { return privateTo(youtube.SessionAuthLoggedOut, true) }

	t.Run("Process", func(t *testing.T) {
		sp, _, job := foundTestJob(t)
		sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) { return dead(), nil }
		res, err := sp.Process(context.Background(), job)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if res.ShouldDownload || !cookiesStatusError(res.AsError()) {
			t.Errorf("result = %+v, want it routed to COOKIES?", res)
		}
	})
	t.Run("Process with healthy cookies", func(t *testing.T) {
		sp, _, job := foundTestJob(t)
		sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
			return privateTo(youtube.SessionAuthLoggedIn, true), nil
		}
		res, err := sp.Process(context.Background(), job)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if res.ShouldDownload || res.Error == "" || cookiesStatusError(res.AsError()) {
			t.Errorf("result = %+v, want a plain Error", res)
		}
	})
	t.Run("a queued VOD's re-extraction", func(t *testing.T) {
		if _, err := judgeRefreshedVodInfo(dead()); !cookiesStatusError(err) {
			t.Errorf("err = %v, want it routed to COOKIES?", err)
		}
	})
	t.Run("mid-capture", func(t *testing.T) {
		if err := liveCredentialFailure(dead()); !errors.Is(err, ErrCookiesRequired) {
			t.Errorf("err = %v, want ErrCookiesRequired", err)
		}
		if err := liveCredentialFailure(privateTo(youtube.SessionAuthLoggedIn, true)); err != nil {
			t.Errorf("healthy session: err = %v, want nil", err)
		}
		withFormats := dead()
		withFormats.Formats = []youtube.Format{{Itag: 299}}
		if err := liveCredentialFailure(withFormats); err != nil {
			t.Errorf("formats still served: err = %v, want nil", err)
		}
	})
}

// In the waiting loop the two full fetches that give up on a private stream
// park a dead session's job instead, and stop polling. Everyone else gets the
// plain give-up.
//
// Mutants: either give-up left on giveUpUnavailable (that arm's dead row reads
// non-actionable); the park taken on any private verdict (the plain rows park).
func TestWaitingLoopParksADeadSessionsPrivateStream(t *testing.T) {
	const privateAt = 5 * time.Minute
	arms := []struct {
		name   string
		fakes  func(full func(time.Duration) (*youtube.VideoInfo, error)) waitFakes
		budget time.Duration
	}{
		{"the default arm's fetch", func(full func(time.Duration) (*youtube.VideoInfo, error)) waitFakes {
			return waitFakes{signedIn: true, status: unclearFrom(privateAt), full: full}
		}, 20 * time.Minute},
		{"the periodic fetch", func(full func(time.Duration) (*youtube.VideoInfo, error)) waitFakes {
			return waitFakes{status: func(time.Duration) (*youtube.VideoInfo, error) { return nil, botWalledProbe(errEscalation400) }, full: full}
		}, 2 * fullFetchInterval},
	}
	for _, arm := range arms {
		for _, tc := range []struct {
			name string
			info *youtube.VideoInfo
			park bool
		}{
			{"dead session", privateTo(youtube.SessionAuthLoggedOut, true), true},
			{"healthy session", privateTo(youtube.SessionAuthLoggedIn, true), false},
			{"unknown session", privateTo(youtube.SessionAuthUnknown, true), false},
			{"no cookies", privateTo(youtube.SessionAuthLoggedOut, false), false},
		} {
			t.Run(arm.name+", "+tc.name, func(t *testing.T) {
				run := driveWaitForLive(t, 2*time.Minute, arm.budget, func(time.Time) waitFakes {
					return arm.fakes(func(time.Duration) (*youtube.VideoInfo, error) { return tc.info, nil })
				})
				if run.result == nil || run.result.ShouldDownload {
					t.Fatalf("the wait ended with %+v at %v, want it over with nothing to download:%s", run.result, run.returned, run)
				}
				if n := run.count("full"); n != 1 {
					t.Errorf("%d full fetches, want polling to stop at the one that said private:%s", n, run)
				}
				if got := errors.Is(run.result.ErrSentinel, ErrCookiesRequired); got != tc.park {
					t.Errorf("sentinel = %v, want park %v", run.result.ErrSentinel, tc.park)
				}
				if tc.park && !strings.Contains(run.result.Error, "signed out") {
					t.Errorf("error %q does not say the session is signed out", run.result.Error)
				}
				if !tc.park && !errors.Is(run.result.ErrSentinel, ErrNonActionable) {
					t.Errorf("sentinel = %v, want the plain give-up", run.result.ErrSentinel)
				}
				if run.row.Status != database.StatusUpcoming {
					t.Errorf("row status = %q, want %q", run.row.Status, database.StatusUpcoming)
				}
			})
		}
	}
}

package cookies

import (
	"context"
	"errors"
	"testing"
)

// TestFinishSetupDetailedCarriesTheStateItAlreadyComputed is H4's remainder.
//
// checkPlatformAuth has produced three outcomes since the tri-state landed —
// verified, conclusively rejected, and "we did not find out" — and the
// acceptance predicate deliberately treats one half of the third as a success.
// FinishSetup returned a bool pair, so the distinction survived only as a
// server log line: a user whose network blipped during the check was told,
// through the same green badge as everyone else, that their cookies were
// configured, and a user whose extraction produced credentials that cannot
// sign a request was told no login was detected.
//
// The two "unknown" rows are the point, and they sit on OPPOSITE sides of
// acceptance:
//
//   - a 429 is not evidence against a login that happened thirty seconds ago,
//     so it is accepted — but nothing checked it, so the verdict is unknown.
//   - a jar that cannot build a SAPISIDHASH never made a request at all, so
//     there is no answer to extend the benefit of the doubt to. Not accepted,
//     and still unknown: the credentials were not found wanting, they were
//     never asked about.
//
// The verified and rejected rows are the premise. Without them a projection
// that answered "unknown" to everything would satisfy the two rows above by
// saying nothing at all.
func TestFinishSetupDetailedCarriesTheStateItAlreadyComputed(t *testing.T) {
	rateLimited := errors.New("youtube auth check: unexpected status 429")

	tests := []struct {
		name         string
		verify       func(context.Context) (bool, error)
		wantAccepted bool
		wantVerdict  RefreshVerdict
	}{
		{
			name:         "verified",
			verify:       func(context.Context) (bool, error) { return true, nil },
			wantAccepted: true,
			wantVerdict:  RefreshOK,
		},
		{
			// THE FIX. Accepted, and the caller must be able to see that
			// nothing confirmed it.
			name:         "could not reach the site",
			verify:       func(context.Context) (bool, error) { return false, rateLimited },
			wantAccepted: true,
			wantVerdict:  RefreshUnknown,
		},
		{
			// Its mirror image: also unknown, also not a finding about the
			// credentials, but NOT accepted — no request was ever made.
			name: "never attempted",
			verify: func(context.Context) (bool, error) {
				return false, ErrAuthCheckNotAttempted
			},
			wantAccepted: false,
			wantVerdict:  RefreshUnknown,
		},
		{
			// The premise: a conclusive negative is still reported as one.
			name:         "conclusively rejected",
			verify:       func(context.Context) (bool, error) { return false, nil },
			wantAccepted: false,
			wantVerdict:  RefreshFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
			s.VerifyYouTubeAuth = tt.verify

			result, err := s.FinishSetupDetailed(context.Background())
			if err != nil {
				t.Fatalf("FinishSetupDetailed: %v", err)
			}

			if result.YouTubeAccepted != tt.wantAccepted {
				t.Errorf("YouTubeAccepted = %v, want %v", result.YouTubeAccepted, tt.wantAccepted)
			}
			if result.YouTube != tt.wantVerdict {
				t.Errorf("YouTube verdict = %v, want %v — the dialog cannot tell a confirmed "+
					"sign-in from one nothing checked without it", result.YouTube, tt.wantVerdict)
			}

			// The bool wrapper must keep reporting exactly what it always did.
			// An older caller against this build has to behave identically.
			ytAuth, _, err := s.FinishSetup(context.Background())
			if !errors.Is(err, ErrNoSetupInProgress) {
				t.Fatalf("fixture drifted — the slot should be spent by now, got (%v, %v)", ytAuth, err)
			}
		})
	}
}

// TestFinishSetupDetailedReportsAnEmptyProfileAsFailedNotUnknown pins the one
// early return that never reaches checkPlatformAuth.
//
// The user opened the browser and closed it without signing in. Nothing was
// extracted, so no request this setup produced can be authenticated — the same
// conclusion checkPlatformAuth reaches for a platform with nothing on disk, and
// it is a conclusion rather than an assumption.
//
// Unknown would be actively wrong here, because the UI routes unknown to its
// "we could not check them" copy. That is the one thing not to say about a
// browser that plainly held no login, and it would send the user looking for a
// network fault instead of signing in.
func TestFinishSetupDetailedReportsAnEmptyProfileAsFailedNotUnknown(t *testing.T) {
	// A profile the browser wrote to, holding nothing Moombox would keep —
	// which is what "the user never signed in" looks like from the read side.
	// An entirely empty cookies.sqlite cannot be built by the WAL fixture,
	// which asserts its own sidecar is non-empty.
	neverSignedIn := []profileTestCookie{
		{name: "consent", value: "banner-dismissed", host: ".example.com", path: "/"},
	}
	s := finishSetupService(t, neverSignedIn, nopAutoCookieLogger{})
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		t.Error("the verify callback ran for a profile that held no cookies at all")
		return false, nil
	}

	result, err := s.FinishSetupDetailed(context.Background())
	if err != nil {
		t.Fatalf("an empty profile is not an error — the dialog renders it inline: %v", err)
	}

	if result.YouTubeAccepted || result.TwitchAccepted {
		t.Fatalf("fixture is broken — an empty profile must accept nothing: %+v", result)
	}
	if result.YouTube != RefreshFailed || result.Twitch != RefreshFailed {
		t.Errorf("empty profile reported (%v, %v), want (failed, failed) — unknown here renders "+
			"as \"Moombox could not check them\" for a browser that held no login",
			result.YouTube, result.Twitch)
	}
}

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// sentNotification is one call the recovery pass made to notifyAuthFailure.
type sentNotification struct {
	platform string
	title    string
	desc     string
	ntype    notifications.NotificationType
}

// recoveryTestState builds the smallest runState runCookieRecovery touches:
// a logger (file-backed and silenced so the suite stays readable) and a
// RefreshService for the success branch's UI re-check. The jar is empty, so
// that re-check short-circuits before any network call.
func recoveryTestState(t *testing.T) (*runState, *[]sentNotification) {
	t.Helper()
	log, err := logger.New(filepath.Join(t.TempDir(), "recovery.log"), "error", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)

	jar := cookies.NewCookieJar()
	s := &runState{
		log:           log,
		cookieRefresh: cookies.NewRefreshService(jar, time.Hour, log),
	}
	var sent []sentNotification
	return s, &sent
}

// recoveryNotifier records everything the pass tried to send.
func recoveryNotifier(sent *[]sentNotification) authFailureNotifier {
	return func(platform, title, desc string, ntype notifications.NotificationType) {
		*sent = append(*sent, sentNotification{platform, title, desc, ntype})
	}
}

// stubRefreshResult returns a cookieRefresher that hands back a fixed result.
func stubRefreshResult(r cookies.RefreshResult) cookieRefresher {
	return func(context.Context) (cookies.RefreshResult, error) { return r, nil }
}

// stubRefresh is the same for an install that HOLDS credentials for both
// platforms — the shape the 2026-08-20 field case had, and the one where a
// conclusive failure really does mean "these cookies are dead".
func stubRefresh(yt, tw cookies.RefreshVerdict) cookieRefresher {
	return stubRefreshResult(cookies.RefreshResult{
		Ran:     true,
		YouTube: yt, YouTubeStored: true,
		Twitch: tw, TwitchStored: true,
	})
}

// TestRecoveryNotifiesThePlatformItWasInvokedFor is the 2026-08-20 03:40:01
// field log turned into a regression test.
//
// Three lines, one second apart, in the owner's real install:
//
//	YouTube auth verification failed after refresh — manual re-login required
//	cookie refresh succeeded                       verified=Twitch
//	auto-cookie recovery succeeded                 platform=youtube
//
// The recovery had been invoked FOR YouTube. YouTube conclusively failed.
// Twitch verified — and because the callback branched on a whole-service
// "did anything verify" bool, the pass took the success branch, logged
// "recovery succeeded" for the platform that had just died, and sent no
// notification at all. The operator found out days later when a recording
// failed.
//
// Both directions are pinned. The mirror row is not padding: with only the
// YouTube case, hardcoding the platform passes.
func TestRecoveryNotifiesThePlatformItWasInvokedFor(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		yt, tw   cookies.RefreshVerdict
	}{
		{
			name:     "youtube dead behind a healthy twitch",
			platform: "youtube",
			yt:       cookies.RefreshFailed,
			tw:       cookies.RefreshOK,
		},
		{
			name:     "twitch dead behind a healthy youtube",
			platform: "twitch",
			yt:       cookies.RefreshOK,
			tw:       cookies.RefreshFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sent := recoveryTestState(t)
			s.runCookieRecovery(context.Background(), tc.platform,
				stubRefresh(tc.yt, tc.tw), recoveryNotifier(sent))

			if len(*sent) != 1 {
				t.Fatalf("sent %d notifications, want exactly 1 — the sibling platform verifying "+
					"is what used to swallow this entirely: %+v", len(*sent), *sent)
			}
			got := (*sent)[0]
			if got.platform != tc.platform {
				t.Errorf("notified about platform %q, want %q", got.platform, tc.platform)
			}
			if got.title != "Cookie Auto-Refresh Failed" {
				t.Errorf("title = %q, want the conclusive-failure title", got.title)
			}
			if got.ntype != notifications.TypeError {
				t.Errorf("type = %v, want TypeError — this is a conclusive failure", got.ntype)
			}
			if !strings.Contains(got.desc, tc.platform) {
				t.Errorf("description does not name the platform it is about: %q", got.desc)
			}
			// The copy leads with the cookie FILE, because the Settings wizard
			// is loopback-gated and unreachable from exactly the deployments
			// most likely to read this notification.
			file := strings.Index(got.desc, "cookies.txt")
			wizard := strings.Index(got.desc, "Settings")
			if file < 0 || wizard < 0 || file > wizard {
				t.Errorf("guidance must lead with the cookie file, not the Settings wizard: %q", got.desc)
			}
		})
	}
}

// TestRecoverySilentWhenTheTriggeringPlatformIsHealthy is the other half of
// the same property, and the reason the test above cannot be satisfied by
// "always notify". A verdict is per platform in both directions: a dead
// sibling must not raise an alarm about a platform that verified.
func TestRecoverySilentWhenTheTriggeringPlatformIsHealthy(t *testing.T) {
	s, sent := recoveryTestState(t)
	s.runCookieRecovery(context.Background(), "youtube",
		stubRefresh(cookies.RefreshOK, cookies.RefreshFailed), recoveryNotifier(sent))

	if len(*sent) != 0 {
		t.Fatalf("YouTube verified and still got notified: %+v", *sent)
	}
}

// TestRecoveryDeclineIsNotAFailureNotification pins the branch that must NOT
// move when a conclusive failure starts notifying.
//
// A declined pass — setup in progress, a refresh already in flight, nothing
// configured to refresh — learned nothing, and RefreshResult reports that as
// the zero value. Turning that into "your cookies are dead, recordings will
// fail" would be the same over-claiming this task exists to remove, one
// branch over. Once Arc 1 Task 1 lands, every non-200 verification becomes
// Unknown too, so this branch gets busier, not quieter.
func TestRecoveryDeclineIsNotAFailureNotification(t *testing.T) {
	declined := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, nil
	}

	for _, platform := range []string{"youtube", "twitch"} {
		t.Run(platform, func(t *testing.T) {
			s, sent := recoveryTestState(t)
			s.runCookieRecovery(context.Background(), platform, declined, recoveryNotifier(sent))

			if len(*sent) != 1 {
				t.Fatalf("sent %d notifications, want exactly 1: %+v", len(*sent), *sent)
			}
			got := (*sent)[0]
			if got.title != "Cookie Auto-Refresh Ineffective" {
				t.Errorf("title = %q, want the non-committal one — nothing was established", got.title)
			}
			if got.ntype != notifications.TypeWarning {
				t.Errorf("type = %v, want TypeWarning", got.ntype)
			}
			for _, forbidden := range []string{"are dead", "still not authenticated"} {
				if strings.Contains(got.desc, forbidden) {
					t.Errorf("an Unknown verdict must not assert a cause (%q): %q", forbidden, got.desc)
				}
			}
		})
	}
}

// TestRecoveryErrorStillNotifies keeps the pre-existing error branch intact:
// a refresh that blew up is reported the same way it always was, per
// platform, at error severity.
func TestRecoveryErrorStillNotifies(t *testing.T) {
	s, sent := recoveryTestState(t)
	boom := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, errors.New("browser exploded")
	}
	s.runCookieRecovery(context.Background(), "youtube", boom, recoveryNotifier(sent))

	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1: %+v", len(*sent), *sent)
	}
	got := (*sent)[0]
	if got.title != "Cookie Auto-Refresh Failed" || got.ntype != notifications.TypeError {
		t.Errorf("error branch = %q/%v, want the failure title at error severity", got.title, got.ntype)
	}
}

// TestRecoveryFailureOnAnUnconfiguredPlatformDoesNotBlameCookies pins the
// defence-in-depth split inside the RefreshFailed branch.
//
// A conclusive failure with no stored credentials is still a failure — under
// liveness-over-presence, "nothing there" and "rejected" both mean requests
// will not work — but only one of them was caused by a cookie going bad.
// Saying "the stored cookies are dead, replace them" about cookies that were
// never there names a cause that did not happen.
//
// shouldFireRecovery cannot currently deliver this shape (its startup arm is
// gated on cookiesPresent, its transition arm on prevAuth), so this guards an
// invariant that lives in another package rather than a live path.
func TestRecoveryFailureOnAnUnconfiguredPlatformDoesNotBlameCookies(t *testing.T) {
	s, sent := recoveryTestState(t)
	s.runCookieRecovery(context.Background(), "twitch",
		stubRefreshResult(cookies.RefreshResult{
			Ran:     true,
			YouTube: cookies.RefreshOK, YouTubeStored: true,
			Twitch: cookies.RefreshFailed, TwitchStored: false,
		}),
		recoveryNotifier(sent))

	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1: %+v", len(*sent), *sent)
	}
	got := (*sent)[0]
	if got.title != "Cookie Auto-Refresh Failed" {
		t.Errorf("title = %q — this is still a conclusive failure", got.title)
	}
	for _, forbidden := range []string{"are dead", "replaced"} {
		if strings.Contains(got.desc, forbidden) {
			t.Errorf("nothing was stored and nothing was rejected, so %q is an unearned cause: %q",
				forbidden, got.desc)
		}
	}
	if !strings.Contains(got.desc, "no twitch cookies") {
		t.Errorf("the description must say what is actually true — that there are none: %q", got.desc)
	}
}

// TestRecoveryUnrecognisedPlatformDoesNotAssertFailure guards the seam
// between the recovery pass and RefreshResult.Verdict. A platform string the
// result does not know about resolves to Unknown, so a wiring mistake or a
// future third platform produces a non-committal warning rather than telling
// the operator their credentials are dead.
func TestRecoveryUnrecognisedPlatformDoesNotAssertFailure(t *testing.T) {
	for _, platform := range []string{"", "kick"} {
		s, sent := recoveryTestState(t)
		s.runCookieRecovery(context.Background(), platform,
			stubRefresh(cookies.RefreshFailed, cookies.RefreshFailed), recoveryNotifier(sent))

		if len(*sent) != 1 {
			t.Fatalf("platform %q: sent %d notifications, want exactly 1: %+v", platform, len(*sent), *sent)
		}
		if got := (*sent)[0]; got.title != "Cookie Auto-Refresh Ineffective" {
			t.Errorf("platform %q: title = %q, want the non-committal one — a platform key we do "+
				"not recognise tells us nothing about it", platform, got.title)
		}
	}
}

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

// TestRecoveryNeededNotifiesWhenAutoRefreshIsDisabled pins the fourth path.
//
// The callback used to return early on cookies.auto_enabled = false with a
// Debug line and no notification at all, on the implicit reasoning that an
// install with no configured recovery has nothing worth being told. That is
// backwards: an install WITH auto-recovery has an attempt that may quietly
// fix the problem before anyone notices, and an install without one has
// nothing — so this is the only thing that will ever tell that operator their
// credentials are dead.
//
// The forbidden-phrase block at the bottom is downstream of a junction — copy
// that could never have contained the phrase satisfies it just as well as copy
// that deliberately omits it — so the test establishes the premise separately
// by driving a real refresh through the same notifier helper and requiring the
// phrase to appear there. Whether the refresher is called at all is a different
// question with its own junction, pinned in the test below.
func TestRecoveryNeededNotifiesWhenAutoRefreshIsDisabled(t *testing.T) {
	s, sent := recoveryTestState(t)
	s.handleRecoveryNeeded("youtube", false,
		stubRefreshResult(cookies.RefreshResult{}), recoveryNotifier(sent))

	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1 — this path used to send none: %+v", len(*sent), *sent)
	}
	got := (*sent)[0]
	if got.platform != "youtube" {
		t.Errorf("notified about platform %q, want %q", got.platform, "youtube")
	}
	if got.ntype != notifications.TypeError {
		t.Errorf("type = %v, want TypeError — recordings will fail and nothing will fix it", got.ntype)
	}
	if !strings.Contains(got.desc, "youtube") {
		t.Errorf("description does not name the platform it is about: %q", got.desc)
	}
	// Same rule the conclusive-failure copy follows: the Settings wizard is
	// loopback-gated, so a container or remote dashboard — the deployments
	// most likely to be reading this — cannot reach it.
	file := strings.Index(got.desc, "cookies.txt")
	wizard := strings.Index(got.desc, "Settings")
	if file < 0 || wizard < 0 || file > wizard {
		t.Errorf("guidance must lead with the cookie file, not the Settings wizard: %q", got.desc)
	}

	// Nothing ran, so nothing may be claimed about a run.
	//
	// Every phrase below is established before it is forbidden. Drive all four
	// of runCookieRecovery's branches and collect what they actually say; a
	// phrase that no longer appears in any of them is a dead assertion, and
	// forbidding it in the disabled path would prove nothing — the exact
	// junction this test exists to stay upstream of. Comparison is
	// case-insensitive in both directions so a future capitalisation change
	// cannot quietly retire either half.
	var refreshCopy []string
	collect := func(platform string, refresh cookieRefresher) {
		t.Helper()
		sc, sentC := recoveryTestState(t)
		sc.runCookieRecovery(context.Background(), platform, refresh, recoveryNotifier(sentC))
		for _, n := range *sentC {
			refreshCopy = append(refreshCopy, strings.ToLower(n.desc))
		}
	}
	collect("youtube", func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, errors.New("browser exploded") // error branch
	})
	collect("youtube", stubRefresh(cookies.RefreshFailed, cookies.RefreshOK)) // conclusive, credentials held
	collect("twitch", stubRefreshResult(cookies.RefreshResult{                // conclusive, nothing left
		Ran: true, Twitch: cookies.RefreshFailed, TwitchStored: false,
	}))
	collect("youtube", stubRefreshResult(cookies.RefreshResult{})) // declined / Unknown

	gotLower := strings.ToLower(got.desc)
	for _, forbidden := range []string{
		"automatic cookie refresh ran",
		"automatic cookie refresh for",
		"did not restore",
		"declined to run",
		"found nothing usable",
	} {
		established := false
		for _, c := range refreshCopy {
			if strings.Contains(c, forbidden) {
				established = true
				break
			}
		}
		if !established {
			t.Fatalf("premise failed: %q no longer appears in ANY runCookieRecovery branch, so "+
				"forbidding it on the disabled path pins nothing. Live copy: %q", forbidden, refreshCopy)
		}
		if strings.Contains(gotLower, forbidden) {
			t.Errorf("no refresh ran, so %q is an unearned claim: %q", forbidden, got.desc)
		}
	}
}

// TestRecoveryNeededDoesNotRefreshWhenDisabledButDoesWhenEnabled is the other
// half, and the reason the fix could not simply delete the early return.
//
// With cookies.auto_enabled off there is no recovery to attempt: falling
// through to RefreshCookiesDetailed would launch a headless browser the
// operator explicitly disabled and pay its two-minute timeout for it. The
// disabled row asserts the refresher is untouched.
//
// The enabled row is the junction guard and is not optional. "The refresher
// was not called" is downstream of everything — a broken seam, a wrongly-typed
// parameter or a method value that is never wired reads identically. Only by
// showing the SAME injected refresher is reached on the enabled path does the
// disabled row's silence mean the gate.
func TestRecoveryNeededDoesNotRefreshWhenDisabledButDoesWhenEnabled(t *testing.T) {
	t.Run("disabled does not refresh", func(t *testing.T) {
		s, sent := recoveryTestState(t)
		s.handleRecoveryNeeded("youtube", false,
			func(context.Context) (cookies.RefreshResult, error) {
				t.Error("RefreshCookiesDetailed was called with auto-cookies disabled — " +
					"that launches the headless browser the operator turned off")
				return cookies.RefreshResult{}, nil
			},
			recoveryNotifier(sent))
		// handleRecoveryNeeded's disabled arm is synchronous by contract
		// (notifyMgr.Send hands off to its own goroutines), so by the time it
		// returns the whole path has run — nothing is still in flight that
		// could call the refresher after this check.
		if len(*sent) != 1 {
			t.Fatalf("the disabled path did not run to completion, so \"refresher untouched\" is "+
				"vacuous: sent %d notifications: %+v", len(*sent), *sent)
		}
	})

	t.Run("enabled refreshes and routes the result through runCookieRecovery", func(t *testing.T) {
		s, _ := recoveryTestState(t)
		called := make(chan struct{})
		finished := make(chan struct{})
		var gotTitle, gotDesc string
		var gotType notifications.NotificationType
		s.handleRecoveryNeeded("youtube", true,
			func(context.Context) (cookies.RefreshResult, error) {
				close(called)
				return cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshFailed, YouTubeStored: true}, nil
			},
			// Records, then signals. The write happens before close(finished)
			// and every read below happens after <-finished, so the channel
			// supplies the happens-before edge and there is no race — and
			// joining the goroutine before the test's logger is closed is what
			// makes the whole subtest safe.
			func(_, title, desc string, ntype notifications.NotificationType) {
				gotTitle, gotDesc, gotType = title, desc, ntype
				close(finished)
			})
		select {
		case <-called:
		case <-time.After(10 * time.Second):
			t.Fatal("the enabled path never reached the injected refresher — the seam the disabled " +
				"row relies on is not wired")
		}
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("the enabled path refreshed but never notified")
		}

		// "Refreshed, then notified" is NOT enough: an enabled arm that called
		// the refresher, threw the result away and sent its own message would
		// satisfy it while bypassing runCookieRecovery entirely. What pins the
		// wiring is that the notification carries the copy only the
		// RefreshFailed + credentials-held branch produces for the verdict
		// this refresher returned. runCookieRecovery's own tests own the
		// contents of that branch; this owns the fact that the gate reaches it.
		if gotTitle != "Cookie Auto-Refresh Failed" || gotType != notifications.TypeError {
			t.Errorf("enabled path notified %q/%v — the RefreshFailed verdict did not reach "+
				"runCookieRecovery's conclusive-failure branch", gotTitle, gotType)
		}
		if !strings.Contains(gotDesc, "Automatic cookie refresh ran") ||
			!strings.Contains(gotDesc, "still not authenticated") {
			t.Errorf("enabled path's copy is not the conclusive-failure branch's, so the gate is no "+
				"longer wired to the three-verdict pass: %q", gotDesc)
		}
	})
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

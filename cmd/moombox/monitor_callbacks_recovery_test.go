package main

import (
	"context"
	"errors"
	"fmt"
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
// RefreshService for the post-pass auth re-check, which is deferred above the
// verdict switch and therefore runs on EVERY exit, not just the successful one.
// The jar is empty, so that re-check short-circuits before any network call.
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

// TestRecoveryDeclineSaysNothingAndRanUnknownStaysNonCommittal pins both
// halves of the Unknown branch, which are two different events wearing one
// verdict.
//
// A DECLINED pass — setup in progress, a refresh already in flight, nothing
// configured to refresh — did no work at all: refreshDeclined() is
// RefreshResult{}, so Ran is false. It has nothing to report, and reporting
// anyway is not merely noise; the notification stamps the platform's 30-minute
// cooldown, which is what TestDeclinedRecoveryDoesNotSpendTheCooldown below is
// about. This row used to require exactly one "Ineffective" notification.
//
// A pass that RAN and still could not establish an answer — it aborted before
// verifying, the verification could not reach the service, or the platform key
// is one the result does not recognise — is the real Unknown, and it keeps the
// non-committal warning it always had. Turning either into "your cookies are
// dead, recordings will fail" would be the over-claiming this arc exists to
// remove.
func TestRecoveryDeclineSaysNothingAndRanUnknownStaysNonCommittal(t *testing.T) {
	declined := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, nil // refreshDeclined(): Ran == false
	}
	// refreshAborted()'s shape: it ran, and learned nothing either way.
	ranUnknown := stubRefreshResult(cookies.RefreshResult{Ran: true})

	for _, platform := range []string{"youtube", "twitch"} {
		t.Run(platform+"/declined", func(t *testing.T) {
			s, sent := recoveryTestState(t)
			s.runCookieRecovery(context.Background(), platform, declined, recoveryNotifier(sent))

			if len(*sent) != 0 {
				t.Fatalf("a pass that declined to run notified the operator anyway — it did no work, "+
					"and the notification burns the platform's cooldown: %+v", *sent)
			}
		})

		t.Run(platform+"/ran without an answer", func(t *testing.T) {
			s, sent := recoveryTestState(t)
			s.runCookieRecovery(context.Background(), platform, ranUnknown, recoveryNotifier(sent))

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
			// And it must not offer "it declined to run" as one of the
			// possibilities either: the branch above takes every declined pass,
			// so naming it here would describe a case that cannot reach this
			// copy. Same rule, pointed the other way.
			if strings.Contains(strings.ToLower(got.desc), "declined") {
				t.Errorf("this pass RAN, so offering a decline as an explanation is a cause it cannot have: %q", got.desc)
			}
		})
	}
}

// TestDeclinedRecoveryDoesNotSpendTheCooldown is the reachable-today failure
// this split exists for, driven end to end through the REAL cooldown.
//
// Both platforms losing auth in one pass is all it takes. refresh.go fires
// OnRecoveryNeeded twice; handleRecoveryNeeded puts each on its own goroutine;
// AutoCookieService.RefreshCookiesDetailed single-flights on its refreshCmd
// sentinel, so whichever arrives second is handed refreshDeclined() —
// RefreshResult{} — immediately. Its verdict is the zero value, RefreshUnknown.
//
// Sending "Cookie Auto-Refresh Ineffective" for that decline stamps the
// platform's entry in withAuthFailureCooldown, and the next 30 minutes of
// accurate, actionable verdicts for that platform are then dropped. The
// operator is left with a vague warning about a condition Moombox created by
// racing itself, and never sees the one that names the remedy.
//
// The third step is deliberately not tied to a particular producer — a flap
// back to dead, the operator's own POST /api/cookies/auto-refresh, or a
// signed-out liveness verdict once cookies.livenessRecoveryArmed flips. What
// is being pinned is that a pass which did no work leaves the window unspent
// for whatever arrives next.
func TestDeclinedRecoveryDoesNotSpendTheCooldown(t *testing.T) {
	s, sent := recoveryTestState(t)
	notify := withAuthFailureCooldown(recoveryNotifier(sent))

	bothDead := stubRefresh(cookies.RefreshFailed, cookies.RefreshFailed)
	declined := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, nil
	}

	// 1. YouTube's pass claims the single-flight slot and reports the truth.
	s.runCookieRecovery(context.Background(), "youtube", bothDead, notify)
	// 2. Twitch's pass arrives while that one is still running: declined.
	s.runCookieRecovery(context.Background(), "twitch", declined, notify)
	// 3. The next real attempt for Twitch completes and conclusively fails.
	s.runCookieRecovery(context.Background(), "twitch", bothDead, notify)

	if len(*sent) != 2 {
		t.Fatalf("sent %d notifications, want exactly 2 (one accurate one per platform): %+v", len(*sent), *sent)
	}
	byPlatform := map[string]sentNotification{}
	for _, n := range *sent {
		if _, dup := byPlatform[n.platform]; dup {
			t.Fatalf("two notifications for %q — the decline was reported as an event of its own: %+v", n.platform, *sent)
		}
		byPlatform[n.platform] = n
	}
	for _, platform := range []string{"youtube", "twitch"} {
		got, ok := byPlatform[platform]
		if !ok {
			t.Fatalf("%s was never notified at all: %+v", platform, *sent)
		}
		if got.title != "Cookie Auto-Refresh Failed" {
			t.Errorf("%s got %q, want the conclusive-failure title — a declined pass spent the cooldown "+
				"and suppressed the accurate verdict", platform, got.title)
		}
		if got.ntype != notifications.TypeError {
			t.Errorf("%s got severity %v, want TypeError", platform, got.ntype)
		}
	}
}

// TestAuthFailureCooldownStillSuppressesARepeat is the premise the test above
// needs and cannot supply itself. "Both platforms got their accurate message"
// is satisfied just as well by a cooldown that suppresses nothing at all, in
// which case that test would pass with the Ran split deleted and the window
// removed. This shows the same wrapper does drop a second notification for a
// platform inside the window — so the survival above is attributable to the
// decline not spending it.
func TestAuthFailureCooldownStillSuppressesARepeat(t *testing.T) {
	s, sent := recoveryTestState(t)
	notify := withAuthFailureCooldown(recoveryNotifier(sent))
	bothDead := stubRefresh(cookies.RefreshFailed, cookies.RefreshFailed)

	s.runCookieRecovery(context.Background(), "youtube", bothDead, notify)
	s.runCookieRecovery(context.Background(), "youtube", bothDead, notify)

	if len(*sent) != 1 {
		t.Fatalf("two conclusive failures for one platform sent %d notifications, want 1 — the cooldown "+
			"is not suppressing anything, so nothing else about it can be concluded: %+v", len(*sent), *sent)
	}
	// And it is per platform, not global: the sibling must still get through.
	s.runCookieRecovery(context.Background(), "twitch", bothDead, notify)
	if len(*sent) != 2 {
		t.Fatalf("the cooldown suppressed a different platform's first notification: %+v", *sent)
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

// TestRecoveryReportsUnreadableCookieFileWithoutSolicitingItsDestruction is
// F6/F7: autocookies.go's S9 abort refuses to write to cookies.txt BECAUSE it
// could not read it — the file may hold a working credential for a platform
// this pass never touched — and the generic branch right above
// (TestRecoveryErrorStillNotifies) tells the operator "recordings will fail
// until the cookies are replaced". That is backwards for this one error: the
// code destroyed nothing, so the notification must not ask a human to.
//
// The stub error is wrapped the same shape autocookies.go actually produces
// — the sentinel AND the underlying read failure, both via %w — so this
// pins errors.Is resolving ErrCookieFileUnreadable from inside
// runCookieRecovery, one layer removed from where the cookies package
// returns it, rather than only at the point the error was constructed.
func TestRecoveryReportsUnreadableCookieFileWithoutSolicitingItsDestruction(t *testing.T) {
	s, sent := recoveryTestState(t)
	unreadable := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{Ran: true}, fmt.Errorf(
			"%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			cookies.ErrCookieFileUnreadable, errors.New("permission denied (simulated)"))
	}
	s.runCookieRecovery(context.Background(), "youtube", unreadable, recoveryNotifier(sent))

	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1: %+v", len(*sent), *sent)
	}
	got := (*sent)[0]
	if got.title != "Cookie File Unreadable" {
		t.Errorf("title = %q — took the generic catch-all branch instead of the dedicated one", got.title)
	}
	if got.ntype != notifications.TypeError {
		t.Errorf("type = %v, want TypeError", got.ntype)
	}
	lower := strings.ToLower(got.desc)
	// The generic branch's signature phrase — the one that asks the operator
	// to overwrite a file this abort exists to protect. Its absence is the
	// whole point of the dedicated branch.
	if strings.Contains(lower, "until the cookies are replaced") {
		t.Errorf("notification carries the generic branch's solicit-the-destruction phrasing: %q", got.desc)
	}
	if !strings.Contains(lower, "did not write") {
		t.Errorf("notification must say what actually happened — nothing was written: %q", got.desc)
	}
	if !strings.Contains(lower, "do not replace") {
		t.Errorf("notification must give the corrected instruction — do not replace the file: %q", got.desc)
	}
	if !strings.Contains(got.desc, "cookies.txt") {
		t.Errorf("notification does not name the file: %q", got.desc)
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
	// Ran, verdict Unknown. NOT RefreshResult{}: a declined pass now notifies
	// nothing at all (TestRecoveryDeclineSaysNothingAndRanUnknownStaysNonCommittal),
	// so collecting one would contribute no copy and silently retire every
	// phrase below that only the Unknown branch produces.
	collect("youtube", stubRefreshResult(cookies.RefreshResult{Ran: true}))

	gotLower := strings.ToLower(got.desc)
	for _, forbidden := range []string{
		"automatic cookie refresh ran",
		"automatic cookie refresh for",
		"did not restore",
		"could not establish why",
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

// countRecoveryPasses runs one recovery and reports how many in-process auth
// passes the deferred re-check actually started.
//
// Counted through OnAuthChange, which is the only pass-level observable this
// package can reach: refresh's own refreshPassHook is unexported and has no
// setter, by design. The count is exact for ONE pass over the empty jar this
// harness builds — the first pass moves both verdicts from the zero value
// (RefreshUnknown) to failed, so authStatusChanged is true and the callback
// fires; a second pass over the same jar would change nothing and fire nothing.
// Every case below therefore builds its own state and runs exactly one
// recovery, and the assertion is 1-or-0 rather than a running total.
func countRecoveryPasses(t *testing.T, platform string, refresh cookieRefresher) int {
	t.Helper()
	s, sent := recoveryTestState(t)
	passes := 0
	s.cookieRefresh.OnAuthChange = func(cookies.AuthStatus) { passes++ }
	s.runCookieRecovery(context.Background(), platform, refresh, recoveryNotifier(sent))
	return passes
}

// TestRecoveryRechecksExactlyWhenThePassRan is the assertion the Arc 10 brief
// said could not be written, and the review proved otherwise: this suite drives
// runCookieRecovery directly, over a real RefreshService, so the gate above the
// verdict switch is reachable from here.
//
// Three mutations, each caught by a different row:
//
//   - Delete the gate (`if result.Ran` -> `if true`) — the reviewer's own M4,
//     which SURVIVED the original commit. Caught by "declined".
//   - Delete the re-check entirely. Caught by "ran and concluded".
//   - Put the re-check back below the `err != nil` returns, i.e. undo the
//     defer. Caught by "ran, wrote, then aborted" — the refreshAborted() shape,
//     where cookies.txt was replaced and only the jar reload failed, and the
//     credential fingerprint has moved even though the caller sees an error.
func TestRecoveryRechecksExactlyWhenThePassRan(t *testing.T) {
	boom := errors.New("cookies.txt was written but the jar could not be reloaded")

	cases := []struct {
		name    string
		refresh cookieRefresher
		want    int
		why     string
	}{
		{
			name:    "ran and concluded",
			refresh: stubRefresh(cookies.RefreshFailed, cookies.RefreshFailed),
			want:    1,
			why: "a pass that ran rewrote cookies.txt, and refresh's status block is the only place " +
				"the Twitch credential fingerprint is compared and the auth mark cleared",
		},
		{
			name:    "declined",
			refresh: stubRefreshResult(cookies.RefreshResult{}),
			want:    0,
			why: "a declined pass wrote nothing at all, so there is nothing to re-read and the " +
				"skipped-line warning would describe a staleness that does not exist",
		},
		{
			name: "ran, wrote, then aborted",
			refresh: func(context.Context) (cookies.RefreshResult, error) {
				return cookies.RefreshResult{Ran: true}, boom
			},
			want: 1,
			why: "three of the four refreshAborted() exits happen AFTER the write — the fingerprint " +
				"moved and the caller still sees an error, which is exactly the path a re-check " +
				"placed below the error return would miss",
		},
		{
			name: "declined and errored",
			refresh: func(context.Context) (cookies.RefreshResult, error) {
				return cookies.RefreshResult{}, boom
			},
			want: 0,
			why:  "an error is not a licence to re-check: this pass never got far enough to write",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countRecoveryPasses(t, "twitch", tc.refresh); got != tc.want {
				t.Errorf("the recovery started %d auth re-check pass(es), want %d — %s", got, tc.want, tc.why)
			}
		})
	}
}

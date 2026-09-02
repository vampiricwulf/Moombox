package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// withAutoCookieService gives a recovery test state a real AutoCookieService
// pointed at a temp cookie path, so the relogin map has a real owner.
func withAutoCookieService(t *testing.T, s *runState) *cookies.AutoCookieService {
	t.Helper()
	dir := t.TempDir()
	svc := cookies.NewAutoCookieService(dir, filepath.Join(dir, "cookies.txt"), cookies.NewCookieJar(), s.log)
	s.autoCookieSvc = svc
	return svc
}

// TestDisabledRecoveryRaisesTheReloginPrompt is the FIRST of the two exits, and
// the one that covers the documented container: `auto_enabled = false`.
//
// That branch never calls runCookieRecovery at all — it logs, notifies and
// returns (monitor_callbacks.go:750-790) — so every assertion in the sibling
// test below is silent about it, and the install every doc in the tree tells a
// container operator to run would show a dead session with no prompt anywhere.
//
// `refresh` is nil, safely: the disabled branch returns before it would be
// called, which is exactly the property that makes this exit a separate case.
//
// The mutation: deleting the raise from that branch. Every other test here stays
// green, because they all drive the enabled path.
func TestDisabledRecoveryRaisesTheReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	s.handleRecoveryNeeded("youtube", false, nil, recoveryNotifier(sent))

	relogin := svc.ReloginStatus()
	if !relogin["youtube"] {
		t.Error("automatic refresh is disabled, the session is conclusively dead, and no re-login " +
			"prompt was raised — this is the container's documented configuration, and the header " +
			"warning that routes to the import panel is the only thing that would tell the operator")
	}
	if relogin["twitch"] {
		t.Error("a YouTube auth loss raised the Twitch prompt — the flag is per platform")
	}
	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1: %+v", len(*sent), *sent)
	}
	if got := (*sent)[0].title; got != "Cookie Re-Authentication Required" {
		t.Errorf("notification title = %q, want the disabled branch's own", got)
	}
}

// TestRecoveryThatCouldNotRunRaisesTheReloginPrompt is the SECOND exit: the
// browser path is on, and there is nothing for it to run.
//
// RefreshCookiesDetailed returns ErrNoBrowserFound before it verifies anything,
// so the existing producer of the relogin flag — the verify-failed arm inside
// that function — cannot fire. (With a mounted profile it CAN: the browser-free
// import verifies, and autocookies.go:2346 raises the flag on its own. This
// exit is for the install that has neither.) Recovery itself only runs on a
// conclusive signed-out verdict, so reaching this branch means the credentials
// are dead AND nothing automatic can fix them.
//
// The mutation: deleting the FlagManualRelogin call from the generic error
// branch. The badge disappears for a `auto_enabled = true` host with no browser
// and no profile, which the disabled-branch test above cannot see.
func TestRecoveryThatCouldNotRunRaisesTheReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	noBrowser := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, cookies.ErrNoBrowserFound
	}
	s.runCookieRecovery(context.Background(), "youtube", noBrowser, recoveryNotifier(sent))

	relogin := svc.ReloginStatus()
	if !relogin["youtube"] {
		t.Error("a recovery that ran and could not act left no re-login prompt — with no browser and " +
			"no profile nothing else raises one, so the operator has no indication the session is dead")
	}
	if relogin["twitch"] {
		t.Error("the Twitch prompt was raised by a YouTube recovery — the flag is per platform and " +
			"this pass concluded nothing about Twitch")
	}
}

// TestUnreadableCookieFileDoesNotRaiseTheReloginPrompt.
//
// The abort means Moombox could not READ cookies.txt: nothing was written,
// nothing was checked, and the credentials in that file may be perfectly good.
// "A human must sign in again" is a claim about the credentials, and this
// branch has no basis for it — the same reason its notification refuses to say
// "replace cookies.txt".
//
// The mutation: hoisting the raise above the ErrCookieFileUnreadable arm, which
// is exactly where a careless "flag it on every error" would put it.
func TestUnreadableCookieFileDoesNotRaiseTheReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	unreadable := func(context.Context) (cookies.RefreshResult, error) {
		return cookies.RefreshResult{}, fmt.Errorf("%w — simulated", cookies.ErrCookieFileUnreadable)
	}
	s.runCookieRecovery(context.Background(), "youtube", unreadable, recoveryNotifier(sent))

	if svc.ReloginStatus()["youtube"] {
		t.Error("an unreadable cookies.txt raised the re-login prompt. Nothing was read, nothing was " +
			"written and nothing was checked — the credentials may be fine, and the remedy is the " +
			"permission or the mount")
	}
}

// TestSuccessfulRecoveryRaisesNoReloginPrompt is the other direction, and it is
// not padding: with only the positive case, raising the flag unconditionally at
// the top of runCookieRecovery passes.
func TestSuccessfulRecoveryRaisesNoReloginPrompt(t *testing.T) {
	s, sent := recoveryTestState(t)
	svc := withAutoCookieService(t, s)

	s.runCookieRecovery(context.Background(), "youtube",
		stubRefresh(cookies.RefreshOK, cookies.RefreshOK), recoveryNotifier(sent))

	if svc.ReloginStatus()["youtube"] {
		t.Error("a recovery that RESTORED the session raised a re-login prompt")
	}
}

// TestRecoveryWithNoAutoCookieServiceDoesNotPanic. runState is assembled field
// by field and runCookieRecovery is driven from a nearly-zero one in four
// existing tests; a bare s.autoCookieSvc.FlagManualRelogin would panic there
// and take the monitor goroutine's recover with it.
//
// The mutation: dropping the nil guard.
func TestRecoveryWithNoAutoCookieServiceDoesNotPanic(t *testing.T) {
	s, sent := recoveryTestState(t) // no autoCookieSvc
	s.runCookieRecovery(context.Background(), "youtube",
		func(context.Context) (cookies.RefreshResult, error) {
			return cookies.RefreshResult{}, errors.New("simulated")
		}, recoveryNotifier(sent))
}

// TestAuthFailureGuidanceNamesTheDashboardImport.
//
// The guidance used to lead with the file on the volume because the only other
// remedy named was the interactive browser login, which drives a local headed
// browser and is unreachable from a container. The import is reachable from
// anywhere the dashboard is, so it leads now — and the operator most likely to
// be reading this notification is the one who cannot touch the volume.
//
// The mutation: reverting the copy. Asserted on the SENT notification rather
// than on the constant, because the constant is interpolated at four call sites
// and a %s dropped from one of them is its own defect.
func TestAuthFailureGuidanceNamesTheDashboardImport(t *testing.T) {
	s, sent := recoveryTestState(t)
	withAutoCookieService(t, s)

	s.runCookieRecovery(context.Background(), "youtube",
		stubRefresh(cookies.RefreshFailed, cookies.RefreshOK), recoveryNotifier(sent))

	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want 1: %+v", len(*sent), *sent)
	}
	desc := (*sent)[0].desc
	for _, want := range []string{"Settings", "Cookies", "paste"} {
		if !strings.Contains(strings.ToLower(desc), strings.ToLower(want)) {
			t.Errorf("the auth-failure notification does not mention %q — a container operator is "+
				"told only about a file they cannot reach:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "%!s") || strings.Contains(desc, "%!(EXTRA") {
		t.Errorf("the guidance's format arguments no longer match its verbs: %s", desc)
	}
}

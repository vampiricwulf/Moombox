package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// An open dashboard is told when a re-login flag or the sidecar's health
// changes, by a frame that carries nothing (owner ruling R2-D1): the tab
// reads /api/status itself. Before this a tab kept the badges of its last
// page load or reconnect.
//
// Mutants: nudgeDashboardStatus sending nothing; a payload added to the
// frame; the sidecar subscription dropped.
func TestStatusChangesNudgeTheDashboard(t *testing.T) {
	sidecar.ResetHealthForTesting()
	t.Cleanup(sidecar.ResetHealthForTesting)

	hub := web.NewWebSocketHub(sweepTestLogger{})
	next := connectDashboard(t, hub)
	s := &runState{wsHub: hub}
	wantNudge := func(after string) {
		t.Helper()
		f := next()
		if f.Type != statusChangedFrame {
			t.Fatalf("after %s: frame %q, want %q", after, f.Type, statusChangedFrame)
		}
		if string(f.Payload) != "null" {
			t.Errorf("after %s: payload %s, want none — the tab reads the status itself", after, f.Payload)
		}
	}

	svc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), sweepTestLogger{})
	svc.OnReloginChange = s.reloginChanged
	svc.FlagManualRelogin("youtube")
	wantNudge("a re-login flag was raised")

	unsub := s.wireSidecarStatusNudge()
	defer unsub()
	sidecar.PublishHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF"})
	wantNudge("the sidecar went down")
	sidecar.PublishHealth(sidecar.Health{Healthy: true, Restarts: 1})
	wantNudge("the sidecar came back")
}

// The TUI's badge carries the re-login flag too, and the flag can change with
// the auth status unchanged: the hook sends the TUI's cookie status again.
// Before this only the dashboard heard, and the TUI kept the plain badge
// until some unrelated auth transition.
//
// Mutant: reloginChanged without resendTUICookieStatus.
func TestReloginFlagChangeReachesTheTUI(t *testing.T) {
	svc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), sweepTestLogger{})
	s := &runState{
		autoCookieSvc: svc,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, sweepTestLogger{}),
	}
	// Signed out and checked, before and after: no auth transition happens.
	auth := cookies.AuthStatus{Checked: true}
	ch := make(chan tui.CookieStatusMsg, 1)
	send := newTUICookieStatusSender(
		func() cookies.AuthStatus { return auth },
		func() map[string]bool { return svc.ReloginStatus() },
		func() (bool, bool) { return true, true },
		ch,
	)
	tuiFn := func(cookies.AuthStatus) { send(auth) }
	s.authChangeTUI.Store(&tuiFn)
	svc.OnReloginChange = s.reloginChanged

	svc.FlagManualRelogin("youtube")
	select {
	case msg := <-ch:
		if msg.YT != tui.CookieStatusRelogin {
			t.Errorf("YouTube badge = %v after the flag was raised, want the re-login badge", msg.YT)
		}
		if msg.TW == tui.CookieStatusRelogin {
			t.Error("Twitch badge shows re-login; only YouTube's flag was raised")
		}
	default:
		t.Fatal("no cookie status reached the TUI when the re-login flag was raised")
	}

	svc.ClearManualRelogin("youtube")
	select {
	case msg := <-ch:
		if msg.YT == tui.CookieStatusRelogin {
			t.Error("YouTube badge still shows re-login after the flag was lowered")
		}
	default:
		t.Fatal("no cookie status reached the TUI when the re-login flag was lowered")
	}
}

// The three sources are wired where no test can drive them (initServices and
// run build the whole process), so the seam is which function each site
// names, the shape TestBothUIsShareOneHideFinishedAgeBroadcast pins.
//
// Mutants: the nudge dropped from the OnAuthChange dispatcher (a session the
// 30-minute check rejects leaves the pill green); OnReloginChange unwired;
// run never subscribing to the sidecar's health.
func TestStatusNudgeIsWiredToItsThreeSources(t *testing.T) {
	fset := token.NewFileSet()
	services, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatalf("parse services.go: %v", err)
	}
	var authChange, relogin bool
	ast.Inspect(services, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if selectorIs(as.Lhs[0], "cookieRefresh", "OnAuthChange") && hasCallTo(as.Rhs[0], "s", "nudgeDashboardStatus") {
			authChange = true
		}
		if selectorIs(as.Lhs[0], "autoCookieSvc", "OnReloginChange") && selectorIs(as.Rhs[0], "s", "reloginChanged") {
			relogin = true
		}
		return true
	})
	if !authChange {
		t.Error("cookieRefresh.OnAuthChange does not call s.nudgeDashboardStatus — an auth change reaches the TUI only")
	}
	if !relogin {
		t.Error("autoCookieSvc.OnReloginChange is not s.reloginChanged — a raised or lowered re-login flag reaches neither UI")
	}

	mainFile, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if !hasCallTo(mainFile, "s", "wireSidecarStatusNudge") {
		t.Error("main.go never calls s.wireSidecarStatusNudge — a sidecar that recovers leaves 'sidecar down' on every open dashboard")
	}
}

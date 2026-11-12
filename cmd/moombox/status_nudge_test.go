package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/cookies"
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
	svc.OnReloginChange = s.nudgeDashboardStatus
	svc.FlagManualRelogin("youtube")
	wantNudge("a re-login flag was raised")

	unsub := s.wireSidecarStatusNudge()
	defer unsub()
	sidecar.PublishHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF"})
	wantNudge("the sidecar went down")
	sidecar.PublishHealth(sidecar.Health{Healthy: true, Restarts: 1})
	wantNudge("the sidecar came back")
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
		if selectorIs(as.Lhs[0], "autoCookieSvc", "OnReloginChange") && selectorIs(as.Rhs[0], "s", "nudgeDashboardStatus") {
			relogin = true
		}
		return true
	})
	if !authChange {
		t.Error("cookieRefresh.OnAuthChange does not call s.nudgeDashboardStatus — an auth change reaches the TUI only")
	}
	if !relogin {
		t.Error("autoCookieSvc.OnReloginChange is not s.nudgeDashboardStatus — a raised or lowered re-login flag reaches no open dashboard")
	}

	mainFile, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if !hasCallTo(mainFile, "s", "wireSidecarStatusNudge") {
		t.Error("main.go never calls s.wireSidecarStatusNudge — a sidecar that recovers leaves 'sidecar down' on every open dashboard")
	}
}

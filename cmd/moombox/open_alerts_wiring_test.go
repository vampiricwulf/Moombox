package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"slices"
	"testing"
)

// wiringCall is one call expression inside a function: the called name (an
// identifier, or a selector's last part), the receiver as written ("" for a
// plain function), and where it sits.
type wiringCall struct {
	name, recv string
	args       []string
	pos        token.Pos
}

// callsInFunc returns every call in the body of the named top-level function
// (or method) of file, in source order, closures included.
func callsInFunc(t *testing.T, file, fn string) ([]wiringCall, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls []wiringCall
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			text := func(e ast.Node) string {
				var buf bytes.Buffer
				_ = printer.Fprint(&buf, fset, e)
				return buf.String()
			}
			var args []string
			for _, a := range call.Args {
				args = append(args, text(a))
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				calls = append(calls, wiringCall{name: fun.Name, args: args, pos: call.Pos()})
			case *ast.SelectorExpr:
				calls = append(calls, wiringCall{name: fun.Sel.Name, recv: text(fun.X), args: args, pos: call.Pos()})
			}
			return true
		})
	}
	if !found {
		t.Fatalf("%s has no function %s", file, fn)
	}
	return calls, fset
}

// seededFromState reports whether every call to name passes s.openAlerts.
func seededFromState(calls []wiringCall, name string) bool {
	seen := false
	for _, c := range calls {
		if c.name != name {
			continue
		}
		seen = true
		if !slices.Contains(c.args, "s.openAlerts") {
			return false
		}
	}
	return seen
}

// firstCall is the position of the first call to name on recv ("" matches any
// receiver), or NoPos.
func firstCall(calls []wiringCall, recv, name string) token.Pos {
	for _, c := range calls {
		if c.name == name && (recv == "" || c.recv == recv) {
			return c.pos
		}
	}
	return token.NoPos
}

// TestEveryAlerterIsSeededBeforeItCanObserve pins the boot order the open-alert
// state depends on: run() loads the state (and drops what nothing can close)
// before any alerter is wired or any observer starts, every alerter is seeded
// from it, and each is seeded before its first observation — the disk
// alerter before the boot reading, the sidecar alerter before it subscribes
// (SubscribeHealth delivers the current snapshot at once), the cookie
// refresh before its synchronous first check, and every monitor covering a
// platform before the monitors start. A seed placed after its observer's
// first look misses exactly the healthy observation that should have sent the
// close.
//
// Mutants: drop any one seeding call, or hand one nil instead of
// s.openAlerts, or move the state load below
// s.wireMonitorCallbacks(), or SetUnrecoveredPlatforms below
// cookieRefresh.Start, or the disk alerter's restoreFrom below its boot
// onReading, or the sidecar's below SubscribeHealth.
func TestEveryAlerterIsSeededBeforeItCanObserve(t *testing.T) {
	run, fset := callsInFunc(t, "main.go", "run")
	before := func(calls []wiringCall, what string, aRecv, aName, bRecv, bName string) {
		t.Helper()
		a, b := firstCall(calls, aRecv, aName), firstCall(calls, bRecv, bName)
		switch {
		case a == token.NoPos:
			t.Errorf("%s: no call to %s.%s", what, aRecv, aName)
		case b == token.NoPos:
			t.Errorf("%s: no call to %s.%s — this test's anchor moved", what, bRecv, bName)
		case a > b:
			t.Errorf("%s: %s.%s (%s) comes after %s.%s (%s)", what, aRecv, aName, fset.Position(a), bRecv, bName, fset.Position(b))
		}
	}
	before(run, "state load", "", "loadOpenAlerts", "s", "wireMonitorCallbacks")
	before(run, "drop unmonitored", "s.openAlerts", "dropUnmonitored", "s", "wireMonitorCallbacks")
	before(run, "state load before the sidecar", "", "loadOpenAlerts", "s", "wireSidecarAlerts")
	before(run, "monitors seeded before they start", "s", "wireMonitorCallbacks", "feedMon", "Start")
	before(run, "auth", "cookieRefresh", "SetUnrecoveredPlatforms", "cookieRefresh", "Start")
	before(run, "disk", "diskAlerter", "restoreFrom", "diskAlerter", "onReading")
	if !seededFromState(run, "restoreFrom") {
		t.Error("run(): the disk alerter is not seeded from s.openAlerts")
	}

	sc, scFset := callsInFunc(t, "sidecar_alerts.go", "wireSidecarAlerts")
	if a, b := firstCall(sc, "a", "restoreFrom"), firstCall(sc, "sidecar", "SubscribeHealth"); a == token.NoPos || b == token.NoPos || a > b {
		t.Errorf("wireSidecarAlerts: a.restoreFrom (%s) must come before sidecar.SubscribeHealth (%s)", scFset.Position(a), scFset.Position(b))
	}
	if !seededFromState(sc, "restoreFrom") {
		t.Error("wireSidecarAlerts: the alerter is not seeded from s.openAlerts")
	}

	mc, _ := callsInFunc(t, "monitor_callbacks.go", "wireMonitorCallbacks")
	for _, mon := range []string{"s.feedMon", "s.decapiMon", "s.twitchMon"} {
		if firstCall(mc, mon, "RestoreUnhealthy") == token.NoPos {
			t.Errorf("wireMonitorCallbacks never seeds %s — its tracker cannot close a restored outage", mon)
		}
	}
	restores := 0
	for _, c := range mc {
		if c.name == "restoreFrom" {
			restores++
		}
	}
	if restores != 2 {
		t.Errorf("wireMonitorCallbacks seeds %d incident sets, want 2 (YouTube and Twitch)", restores)
	}
	if !seededFromState(mc, "withPersistedAuthFailureCooldown") {
		t.Error("wireMonitorCallbacks uses a cooldown that is not persisted to s.openAlerts — an auth failure open across a restart never closes")
	}
	if !seededFromState(mc, "restoreFrom") {
		t.Error("wireMonitorCallbacks seeds an incident set from something other than s.openAlerts")
	}
}

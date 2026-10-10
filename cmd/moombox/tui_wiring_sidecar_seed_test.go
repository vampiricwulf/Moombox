package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestSidecarChipIsSeededBeforeRun pins the ORDER of three statements in
// runTUI, which is the one thing about this wiring that no behavioural test in
// either package can see.
//
// App.Send is a no-op until tui.Run stores the program, and initServices has
// already published the sidecar's health by the time main reaches runTUI (on a
// failed first start, and again from the supervisor a moment later). So
// SubscribeHealth's immediate snapshot — the callback added precisely so that
// "a TUI started after a dead sidecar still draws the alert" — is delivered
// into a program that does not exist yet and is dropped. The seed is what
// carries that snapshot into the model instead, and it is only a seed if it
// runs BEFORE Run.
//
// Shape and not behaviour: runTUI needs a whole live services struct and a
// terminal, so the model half is pinned in internal/tui
// (TestSetSidecarDownSeedsTheBarBeforeRun) and what can only be asserted HERE
// is that the call is placed where the snapshot can still reach it.
//
// Mutants this kills:
//   - the seed deleted (the subscription left to carry the first snapshot) →
//     no SetSidecarDown call in runTUI
//   - the seed moved below tui.Run → the order assertion
//   - the seed reading something other than the published snapshot →
//     no sidecar.CurrentHealth call before it
func TestSidecarChipIsSeededBeforeRun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runTUI" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("runTUI not found in tui_wiring.go")
	}

	// First position of each call inside runTUI, or 0 when absent.
	lineOf := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		name := recv.Name + "." + sel.Sel.Name
		if _, seen := lineOf[name]; !seen {
			lineOf[name] = fset.Position(call.Pos()).Line
		}
		return true
	})

	health, seed, run := lineOf["sidecar.CurrentHealth"], lineOf["app.SetSidecarDown"], lineOf["tui.Run"]
	if run == 0 {
		t.Fatal("tui.Run not called in runTUI — this test no longer describes the function it guards")
	}
	if health == 0 || seed == 0 {
		t.Fatalf("runTUI seeds no sidecar chip (sidecar.CurrentHealth at line %d, app.SetSidecarDown at line %d): "+
			"a sidecar that is already down when the TUI starts would draw nothing, because SubscribeHealth's "+
			"immediate snapshot is sent into a program tui.Run has not stored yet", health, seed)
	}
	if !(health < seed && seed < run) {
		t.Errorf("runTUI's order is sidecar.CurrentHealth:%d, app.SetSidecarDown:%d, tui.Run:%d — the seed must read "+
			"the published snapshot and reach the model BEFORE Run, or it is just another dropped Send",
			health, seed, run)
	}
}

// TestConnectivityIsSeededBeforeRun is the same pin for the OFFLINE alert.
// Monitor.Start probes synchronously inside initServices, so a boot with no
// network went offline before any subscriber existed, and OnStateChange
// replays nothing: the subscription alone left the TUI showing no OFFLINE
// until the network RETURNED, while the dashboard (initial_state) showed its
// banner. The model half is TestSetOfflineSeedsTheBarBeforeRun in
// internal/tui.
//
// Mutants this kills:
//   - the seed deleted → no app.SetOffline call in runTUI
//   - the seed moved below tui.Run → the order assertion
//   - the seed read before the subscription (a transition between the two
//     would reach neither) → the order assertion
//   - the seed reading something other than the monitor → no
//     connMon.IsOnline call before it
func TestConnectivityIsSeededBeforeRun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runTUI" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("runTUI not found in tui_wiring.go")
	}

	// First line of each call inside runTUI, keyed by its last receiver
	// segment and method ("connMon.IsOnline" for s.connMon.IsOnline()).
	lineOf := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		var recv string
		switch x := sel.X.(type) {
		case *ast.Ident:
			recv = x.Name
		case *ast.SelectorExpr:
			recv = x.Sel.Name
		default:
			return true
		}
		name := recv + "." + sel.Sel.Name
		if _, seen := lineOf[name]; !seen {
			lineOf[name] = fset.Position(call.Pos()).Line
		}
		return true
	})

	sub, read, seed, run := lineOf["connMon.OnStateChange"], lineOf["connMon.IsOnline"], lineOf["app.SetOffline"], lineOf["tui.Run"]
	if run == 0 || sub == 0 {
		t.Fatalf("runTUI no longer calls tui.Run (%d) or connMon.OnStateChange (%d) — this test no longer describes the function it guards", run, sub)
	}
	if read == 0 || seed == 0 {
		t.Fatalf("runTUI seeds no OFFLINE alert (connMon.IsOnline at line %d, app.SetOffline at line %d): "+
			"a boot with no network went offline before the subscription existed, and the TUI would show "+
			"nothing until the network returned", read, seed)
	}
	if !(sub < read && read <= seed && seed < run) {
		t.Errorf("runTUI's order is connMon.OnStateChange:%d, connMon.IsOnline:%d, app.SetOffline:%d, tui.Run:%d — "+
			"the seed must read the monitor after subscribing and reach the model BEFORE Run", sub, read, seed, run)
	}
}

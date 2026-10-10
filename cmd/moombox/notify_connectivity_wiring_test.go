package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestNotifierIsGivenTheConnectivityMonitor pins the one line that makes the
// notification manager hold through an outage (W27-10): without the monitor
// every target runs its ladder as before, and a notification raised offline
// spends its three attempts on the dead link within seconds and is dropped.
// The holding itself is internal/notifications' tests'; this is that the boot
// wiring hands the live monitor over, in initServices, where the manager is
// built.
//
// Mutants killed: the SetConnectivity call removed; handed something other
// than the runState's monitor.
func TestNotifierIsGivenTheConnectivityMonitor(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetConnectivity" {
			return true
		}
		if recv, ok := sel.X.(*ast.Ident); !ok || recv.Name != "notifyMgr" {
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok || arg.Sel.Name != "connMon" {
			return true
		}
		if s, ok := arg.X.(*ast.Ident); ok && s.Name == "s" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("services.go no longer calls notifyMgr.SetConnectivity(s.connMon) — notifications raised offline " +
			"would spend their attempts on the dead link and be dropped")
	}
}

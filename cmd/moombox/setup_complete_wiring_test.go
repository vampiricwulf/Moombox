package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTUISetupCompletesThroughTheStore pins the W27 review of W26-11's
// wiring: the TUI wizard's OnComplete — SetSetupCallbacks' first argument —
// is the config store's CompleteFirstRun, the save the Web route ends in
// (CompleteFirstRunLocked). Its own closure saved with no first-run check
// and left the live config unloaded, so a TUI save and a Web complete could
// both apply and both restart; config.TestCompleteFirstRunSavesOnce and
// routes.TestSetupCompletesOnceAcrossBothWizards pin the rule itself.
//
// Mutant killed: OnComplete back to a closure that calls config.Save.
func TestTUISetupCompletesThroughTheStore(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fn.Sel.Name != "SetSetupCallbacks" || len(call.Args) == 0 {
			return true
		}
		if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "CompleteFirstRun" {
			if store, ok := arg.X.(*ast.SelectorExpr); ok && store.Sel.Name == "configStore" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Error("the TUI wizard's OnComplete is not s.configStore.CompleteFirstRun")
	}
}

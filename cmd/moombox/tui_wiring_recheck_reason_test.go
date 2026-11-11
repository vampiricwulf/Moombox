package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestOnRecheckCookiesReturnsTheInconclusiveReasons guards the production call
// site of Arc 8 Task 12a item (v)'s TUI half, the one thing internal/tui
// cannot see.
//
// THE MUTANT THIS EXISTS FOR: R C's answer losing its two reasons (`"", ""`
// in place of the status's error strings). Every test in internal/tui stays
// green under it: those tests inject cookieRecheckResultMsg directly, so they
// exercise the RENDERER and never the wiring that fills it, and R C goes back
// to "could not establish" with no reason. The chords test next door only
// asserts the field is assigned at all, so it cannot see this either.
//
// The answer is built by recheckCookiesForTUI, and which field lands in which
// of the four positions is asserted by running it
// (TestRecheckCookiesForTUIWaitsOnlyForTheFirstCheck, four distinct values).
// What is left for this test is the closure: runTUI needs the whole runState
// service graph to call, so it is parsed, and its one return must hand that
// function's four results on untouched.
func TestOnRecheckCookiesReturnsTheInconclusiveReasons(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}

	// The closure assigned to app.OnRecheckCookies.
	var lit *ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "OnRecheckCookies" {
			return true
		}
		if fn, ok := assign.Rhs[0].(*ast.FuncLit); ok {
			lit = fn
		}
		return false
	})
	if lit == nil {
		t.Fatal("tui_wiring.go no longer assigns a function literal to app.OnRecheckCookies — this " +
			"test cannot guard a site that moved")
	}

	// Its return. One, and it is the call that builds the answer.
	var returns []*ast.ReturnStmt
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		// Do not descend into a nested closure: its return is not this
		// callback's.
		if inner, ok := n.(*ast.FuncLit); ok && inner != lit {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok {
			returns = append(returns, ret)
		}
		return true
	})
	if len(returns) != 1 {
		t.Fatalf("OnRecheckCookies has %d return statements, want 1 — with more than one, this "+
			"test would be guarding whichever the walk happened to find", len(returns))
	}
	results := returns[0].Results
	if len(results) != 1 {
		t.Fatalf("OnRecheckCookies returns %d expressions, want the one call to recheckCookiesForTUI: "+
			"anything built here is R C's answer with no test on it", len(results))
	}
	call, ok := results[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("OnRecheckCookies returns a %T, want the call to recheckCookiesForTUI", results[0])
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "recheckCookiesForTUI" {
		t.Errorf("OnRecheckCookies does not return recheckCookiesForTUI's results")
	}
}

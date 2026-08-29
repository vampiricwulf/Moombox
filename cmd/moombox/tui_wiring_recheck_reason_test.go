package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestOnRecheckCookiesReturnsTheInconclusiveReasons guards the production call
// site of Arc 8 Task 12a item (v)'s TUI half — the one thing internal/tui
// cannot see.
//
// THE MUTANT THIS EXISTS FOR: replace `status.YouTubeError, status.TwitchError`
// in this closure with `"", ""`. It compiles, every test in internal/tui stays
// green — those tests inject cookieRecheckResultMsg directly, so they exercise
// the RENDERER and never the wiring that fills it — and the whole feature
// silently disappears: R C goes back to "could not establish" with no reason,
// which is exactly the state item (v) was raised about. The chords test next
// door only asserts the field is assigned at all, so it cannot see this either.
//
// Parsed rather than pattern-matched, for the reasons findChordWirings gives:
// runTUI needs the whole runState service graph to call, and a substring search
// for "YouTubeError" would pass on the field appearing anywhere else in the file
// while proving nothing about THIS return.
//
// The verdicts are asserted alongside the reasons, in POSITION. The callback
// returns four values of which two are strings and two are verdicts; a
// transposition compiles for the pair of strings, and would report YouTube's
// reason against Twitch's verdict.
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

	// Its return. One, and it must still carry four values: the pair of verdicts
	// R C has always reported, and the pair of reasons that says why an
	// inconclusive one concluded nothing.
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
	if len(results) != 4 {
		t.Fatalf("OnRecheckCookies returns %d values, want 4 (two verdicts, two reasons)", len(results))
	}

	// Each position must read the field that belongs there. Selector NAME only:
	// the receiver is a local whose spelling is not this test's business, but
	// which AuthStatus field lands in which slot very much is.
	for i, want := range []string{
		"YouTubeVerification", "TwitchVerification", "YouTubeError", "TwitchError",
	} {
		sel, ok := results[i].(*ast.SelectorExpr)
		if !ok {
			t.Errorf("return value %d is %T, want a read of AuthStatus.%s. A literal here — `\"\"` "+
				"is the mutant — deletes that half of R C's answer with nothing failing: "+
				"internal/tui's tests inject the message directly and never run this closure",
				i, results[i], want)
			continue
		}
		if sel.Sel.Name != want {
			t.Errorf("return value %d reads .%s, want .%s — the four values are positional, and a "+
				"transposition reports one platform's reason against the other's verdict",
				i, sel.Sel.Name, want)
		}
	}
}

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// TestJSLessRankingIsWiredToTheCompositeSolver pins owner decision 4's one
// production wire: initServices hands the PlayerAPI the composite cipher
// solver's solvability predicate, ytService.PlayerAPI.SetJSChallengeSolvable(
// cipherSolver.ChallengesSolvable), after the composite is built (owner
// ruling RV-16: the sidecar ready, or goja there and not refusing the
// player; a sidecar outage alone is not unsolvable). With no hook the
// PlayerAPI ranks as if the player JS were solvable, so without this line
// every extraction keeps the healthy ranking while no solver can answer a
// player — no visionos-first ranking, no "some formats may be missing" Warn
// — and every behavioural test stays green, because internal/youtube's tests
// install the hook themselves. This package cannot drive initServices, so
// the call's presence and position are what is checked, as
// profiledir_verdict_callsite_test.go checks its call.
//
// Mutants: the call deleted (the "never" fatal); handed anything but the
// composite's ChallengesSolvable — a constant, a method of
// something else (the argument check); hoisted above the composite's
// construction, or moved out of initServices' top level (the order check, or
// the "never" fatal).
func TestJSLessRankingIsWiredToTheCompositeSolver(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatalf("parse services.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "initServices" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("services.go has no initServices with a body — re-anchor this test rather than deleting it")
	}

	builtIdx, wiredIdx := -1, -1
	var arg string
	for i, stmt := range body.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 1 && types.ExprString(s.Lhs[0]) == "cipherSolver" && len(s.Rhs) == 1 {
				if call, ok := s.Rhs[0].(*ast.CallExpr); ok && types.ExprString(call.Fun) == "cipher.NewCompositeSolver" {
					builtIdx = i
				}
			}
		case *ast.ExprStmt:
			call, ok := s.X.(*ast.CallExpr)
			if ok && types.ExprString(call.Fun) == "ytService.PlayerAPI.SetJSChallengeSolvable" {
				wiredIdx = i
				if len(call.Args) == 1 {
					arg = types.ExprString(call.Args[0])
				}
			}
		}
	}

	if builtIdx < 0 {
		t.Fatal("initServices no longer builds cipherSolver with cipher.NewCompositeSolver at its top level — " +
			"the wiring this test exists for cannot be checked")
	}
	if wiredIdx < 0 {
		t.Fatal("initServices never calls ytService.PlayerAPI.SetJSChallengeSolvable: with no hook every " +
			"extraction ranks as if the player JS were solvable, so a player no solver can answer never " +
			"turns on the JS-less ranking (owner decision 4)")
	}
	if arg != "cipherSolver.ChallengesSolvable" {
		t.Errorf("SetJSChallengeSolvable is handed %q, want cipherSolver.ChallengesSolvable — the composite's one "+
			"predicate for whether any solver can answer the player (RV-16), not SidecarReady", arg)
	}
	if wiredIdx < builtIdx {
		t.Errorf("SetJSChallengeSolvable is called at statement %d, before cipherSolver is built at %d",
			wiredIdx, builtIdx)
	}
}

package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"testing"
)

// TestTheLauncherAndTheChildShareTheInstanceLock pins the two call sites that
// make the instance lock outlive a killed launcher while its child runs
// (TestTheInstanceLockOutlivesTheLauncherWhileItsChildRuns drives the helpers
// themselves): the launcher loop hands every child its descriptor —
// shareInstanceLock(cmd) after cmd.Env is set, which it appends to, and before
// cmd.Start — and main keeps it in the child, once past the launcher gate and
// before run starts anything that could inherit it. Both are no-ops on
// Windows, where the launcher's Job Object ends the child with it.
//
// Mutants: dropping either call; moving shareInstanceLock ahead of the cmd.Env
// assignment (the descriptor's variable is overwritten) or after cmd.Start;
// moving holdInheritedInstanceLock after run.
func TestTheLauncherAndTheChildShareTheInstanceLock(t *testing.T) {
	fset, body := funcBody(t, "launcher.go", "launchAndSupervise")
	var envAt, shareAt, startAt token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && types.ExprString(x.Lhs[0]) == "cmd.Env" {
				envAt = x.Pos()
			}
		case *ast.CallExpr:
			switch types.ExprString(x) {
			case "shareInstanceLock(cmd)":
				shareAt = x.Pos()
			case "cmd.Start()":
				startAt = x.Pos()
			}
		}
		return true
	})
	if envAt == token.NoPos || shareAt == token.NoPos || startAt == token.NoPos {
		t.Fatalf("launchAndSupervise: cmd.Env set at %v, shareInstanceLock(cmd) at %v, cmd.Start() at %v; want all three",
			fset.Position(envAt), fset.Position(shareAt), fset.Position(startAt))
	}
	if !(envAt < shareAt && shareAt < startAt) {
		t.Errorf("launchAndSupervise calls shareInstanceLock(cmd) at %s, want it after cmd.Env is set (%s) and before cmd.Start() (%s)",
			fset.Position(shareAt), fset.Position(envAt), fset.Position(startAt))
	}

	fset, body = funcBody(t, "main.go", "main")
	var gateAt, holdAt, runAt token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch types.ExprString(call.Fun) {
		case "launchAndSupervise":
			gateAt = call.Pos()
		case "holdInheritedInstanceLock":
			holdAt = call.Pos()
		case "run":
			runAt = call.Pos()
		}
		return true
	})
	if gateAt == token.NoPos || holdAt == token.NoPos || runAt == token.NoPos || !(gateAt < holdAt && holdAt < runAt) {
		t.Errorf("main calls launchAndSupervise at %s, holdInheritedInstanceLock at %s and run at %s; want the lock kept past the launcher gate and before run",
			fset.Position(gateAt), fset.Position(holdAt), fset.Position(runAt))
	}
}

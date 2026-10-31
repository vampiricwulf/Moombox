package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTwitchServiceAuthLossIsTheWorkersHook pins the production value of the
// Twitch service's credential-loss seam (twitch.Service.SetOnAuthLoss, G21):
// initServices hands it the SAME value it hands the worker's
// SetOnTwitchAuthLoss, and that value comes from twitchAuthLossHook — the
// goroutine-spawning, recover-guarded hop into NoteTwitchAuthLoss whose
// delivery contract services_twitch_auth_loss_test.go drives. initServices
// cannot be driven from a test, so the wiring is read from the syntax tree,
// the package's technique for undrivable sites.
//
// Mutants: the SetOnAuthLoss call deleted (a status batch's refused token
// marks nothing, and the platform reads healthy while every capture parks);
// it handed a func of its own, not the hook (a mark on the monitor's goroutine
// that bypasses the hook's asynchrony and its recover).
func TestTwitchServiceAuthLossIsTheWorkersHook(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatalf("parse services.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "initServices" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("services.go has no initServices with a body — re-anchor this test rather than deleting it")
	}

	hooks := map[string]bool{} // identifiers assigned from twitchAuthLossHook(...)
	var workerArg, serviceArg string
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "twitchAuthLossHook" {
						if id, ok := n.Lhs[0].(*ast.Ident); ok {
							hooks[id.Name] = true
						}
					}
				}
			}
		case *ast.CallExpr:
			arg := ""
			if len(n.Args) == 1 {
				if id, ok := n.Args[0].(*ast.Ident); ok {
					arg = id.Name
				}
			}
			switch {
			case selectorIs(n.Fun, "dlWorker", "SetOnTwitchAuthLoss"):
				workerArg = arg
			case selectorIs(n.Fun, "twService", "SetOnAuthLoss"):
				serviceArg = arg
			}
		}
		return true
	})

	if serviceArg == "" {
		t.Fatal("initServices never hands twService.SetOnAuthLoss a named hook — a stream check whose token " +
			"Twitch refused is answered signed out and marks nothing")
	}
	if !hooks[serviceArg] {
		t.Errorf("twService.SetOnAuthLoss gets %q, which is not assigned from twitchAuthLossHook", serviceArg)
	}
	if serviceArg != workerArg {
		t.Errorf("twService.SetOnAuthLoss gets %q and dlWorker.SetOnTwitchAuthLoss %q — one hook, one mark", serviceArg, workerArg)
	}
}

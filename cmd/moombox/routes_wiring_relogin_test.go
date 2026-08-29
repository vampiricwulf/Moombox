package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestGetAutoCookieReloginNeededCallsReloginStatus guards wireRoutes' own
// StatusRouteDeps.GetAutoCookieReloginNeeded assignment — the one thing
// internal/web/routes' TestStatusRouteCarriesReloginStatus cannot see,
// because that test wires its OWN closure calling ReloginStatus directly
// rather than executing wireRoutes, which needs the whole runState service
// graph to call at all. Same reason tui_wiring_cookiechords_test.go parses
// runTUI instead of running it.
//
// Reverting this closure to `s.autoCookieSvc.GetStatus().NeedsManualRelogin`
// compiles fine and returns an identically-shaped value on every existing
// black-box test — the only difference is GetStatus's browser/registry
// detection scan running on every /api/status poll again, which no
// response-shape assertion can observe from outside.
//
// Parsed rather than pattern-matched: a raw string Contains for
// "ReloginStatus" would pass if that call appeared anywhere else in the
// file, proving nothing about THIS field, and would be defeated by a
// harmless reformat of the surrounding struct literal.
func TestGetAutoCookieReloginNeededCallsReloginStatus(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "routes_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse routes_wiring.go: %v", err)
	}

	var rhs ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "GetAutoCookieReloginNeeded" {
			return true
		}
		rhs = kv.Value
		return false
	})
	if rhs == nil {
		t.Fatal("wireRoutes no longer assigns GetAutoCookieReloginNeeded — this test cannot guard a " +
			"site that moved")
	}

	var callsGetStatus, callsReloginStatus bool
	ast.Inspect(rhs, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "GetStatus":
			callsGetStatus = true
		case "ReloginStatus":
			callsReloginStatus = true
		}
		return true
	})

	if callsGetStatus {
		t.Error("GetAutoCookieReloginNeeded calls GetStatus — the browser/registry detection scan runs " +
			"on every /api/status poll again, the exact cost this closure exists to avoid")
	}
	if !callsReloginStatus {
		t.Error("GetAutoCookieReloginNeeded never calls ReloginStatus — nothing here guards it reading " +
			"the cheap accessor rather than some other path entirely")
	}
}

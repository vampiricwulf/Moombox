package routes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestBrowserReadHandlersConsultTheSharedWriter guards the two production call
// sites of writeBrowserReadError.
//
// THE MUTANT THIS EXISTS FOR: delete `if writeBrowserReadError(rw, err) {
// return }` from either handler. It compiles, every routes test stays green —
// TestBrowserReadErrorsReachTheWireWithACause calls the writer directly, so it
// pins the MAPPING and not the wiring — and the endpoint silently goes back to
// flattening both browser-read sentinels into its default arm, which is the
// exact defect the sentinels were introduced to remove.
//
// One writer answering two handlers was chosen deliberately, to avoid the
// junction defect of teaching one of two consumers about a new sentinel. That
// choice is only worth anything if both junctions are held, so both are named
// here and a handler that stops consulting the writer fails by name.
//
// The shape asserted is the whole of it: the call must sit INSIDE the handler's
// `if err != nil` block, its result must be the condition of an `if`, and that
// `if` must return. Each half is a real mutant. Called outside the error block
// it runs with a nil error on the success path, answers false, and the error
// path is unguarded again. Called with its result discarded it writes a
// response and then lets the switch below write a second one onto the same
// request. Called without the return it does the same thing.
//
// Parsed rather than pattern-matched: the sentinels cannot be provoked through
// these handlers from this package (reaching FinishSetupDetailed's browser read
// needs a live setup slot with a Chromium in it, and the fields that hold one
// are unexported), so structure is the strongest assertion available at this
// site. Same technique, and the same reason, as cmd/moombox's
// TestGetAutoCookieReloginNeededCallsReloginStatus.
func TestBrowserReadHandlersConsultTheSharedWriter(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cookies.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cookies.go: %v", err)
	}

	for _, route := range []string{
		// The manual refresh button. Its pass drives a headless browser, so
		// cdpGetCookiesAsNetscape — and both sentinels — are reachable here.
		"/api/cookies/auto-refresh",
		// The setup wizard's "I'm logged in". The site the residual was
		// originally reported against.
		"/api/cookies/auto-setup/finish",
	} {
		t.Run(route, func(t *testing.T) {
			handler := routeHandlerLit(t, file, route)
			errBlock := errorBlockOf(t, handler, route)
			if !guardsWithBrowserReadWriter(errBlock) {
				t.Errorf("%s does not answer its error with `if writeBrowserReadError(rw, err) "+
					"{ return }`.\n\n"+
					"Without it both browser-read sentinels fall to this handler's default arm "+
					"again — a blocked ladder and a read no query answered become one "+
					"undifferentiated 500 with no cause on the wire, which is the state Arc 8 "+
					"Task 12a item (i) removed. The mapping test in this package calls the "+
					"writer directly and cannot see this.", route)
			}
		})
	}
}

// routeHandlerLit finds the handler function literal registered for one route
// path, whichever router value it is registered on (`r` or the rate-limited
// `heavy` sub-router — which one is not this test's business, and moving a
// route between them must not silently drop the guard).
func routeHandlerLit(t *testing.T, file *ast.File, route string) *ast.FuncLit {
	t.Helper()
	var lit *ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Post" {
			return true
		}
		path, ok := call.Args[0].(*ast.BasicLit)
		if !ok || path.Kind != token.STRING || path.Value != `"`+route+`"` {
			return true
		}
		if fn, ok := call.Args[1].(*ast.FuncLit); ok {
			lit = fn
		}
		return false
	})
	if lit == nil {
		t.Fatalf("no POST handler is registered for %s any more — this test cannot guard a route "+
			"that moved or was renamed", route)
	}
	return lit
}

// errorBlockOf returns the body of the handler's `if err != nil` block — the
// one place the guard may live.
func errorBlockOf(t *testing.T, handler *ast.FuncLit, route string) *ast.BlockStmt {
	t.Helper()
	var block *ast.BlockStmt
	ast.Inspect(handler, func(n ast.Node) bool {
		if block != nil {
			return false
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ {
			return true
		}
		x, okX := bin.X.(*ast.Ident)
		y, okY := bin.Y.(*ast.Ident)
		if okX && okY && x.Name == "err" && y.Name == "nil" {
			block = ifStmt.Body
			return false
		}
		return true
	})
	if block == nil {
		t.Fatalf("%s no longer has an `if err != nil` block — the handler was restructured and "+
			"this test cannot say where its error is answered", route)
	}
	return block
}

// guardsWithBrowserReadWriter reports whether the block answers its error with
// `if writeBrowserReadError(...) { return }` — condition AND early return, both
// required. See the test's doc for why each half is its own mutant.
func guardsWithBrowserReadWriter(block *ast.BlockStmt) bool {
	found := false
	ast.Inspect(block, func(n ast.Node) bool {
		if found {
			return false
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		call, ok := ifStmt.Cond.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "writeBrowserReadError" || len(call.Args) != 2 {
			return true
		}
		for _, stmt := range ifStmt.Body.List {
			if ret, ok := stmt.(*ast.ReturnStmt); ok && len(ret.Results) == 0 {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

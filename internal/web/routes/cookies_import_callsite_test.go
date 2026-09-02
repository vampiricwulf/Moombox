package routes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCookieImportHandlerEndsInADetachedFlushedRecheck.
//
// Arc 10 R4: refresh's status block is the ONLY place the Twitch credential
// fingerprint is compared, the auth mark cleared and OnCredentialsChanged
// fired, and that block runs only inside a refresh pass — so every gesture that
// can put new credentials on disk has to reach CheckNow, or the repair waits on
// the 30-minute ticker. This endpoint is the newest such gesture and the one
// built specifically for the deployment with no other repair path.
//
// Structural, and this is the strongest assertion available here for the same
// reason the wizard-finish site has none: driving a real CheckNow from this
// package means a live guide POST and a live oauth2/validate, because
// youtubeGuideURL and twitchValidateURL are unexported package vars in
// internal/cookies. The behavioural half is pinned inside that package
// (TestCheckNowObservesATwitchCredentialChange) over the same public entry
// point; what can only be asserted HERE is that this handler calls it.
//
// THE FOUR MUTANTS, each its own assertion:
//
//   - delete the defer: no CheckNow at all. The most likely regression, because
//     everything else about the endpoint still works.
//   - hand CheckNow the REQUEST's context, in any of its three spellings: a
//     client that closes the tab mid-pass cancels both auth checks,
//     shouldObserveCredentials bails on a check error, no live chat session is
//     told and the identity baseline never advances. The spellings are
//     `CheckNow(req.Context())`; `ctx := req.Context()` then `CheckNow(ctx)`;
//     and — the one that looks most like the real thing —
//     `context.WithTimeout(req.Context(), 45*time.Second)`, which is a detached
//     context in shape only: the deadline is not the only thing that can cancel
//     it. Catching all three takes THREE assertions, because each defeats a
//     different one: the argument must be an identifier (kills the first), the
//     defer must build its context from a ROOT (kills the second), and
//     `req.Context` must not appear inside the defer at all (kills the third,
//     which satisfies both of the others).
//   - drop the Flush: jsonResponse writes into net/http's bufio writer and does
//     not flush, and the handler does not return until the defer completes — so
//     the browser waits out the whole re-check on a request it has already been
//     answered for, and a fetch with a timeout aborts an import that succeeded.
//   - drop the `!result.Wrote` guard: a rejected paste spends a full in-process
//     re-check, two validate round-trips, on a file nobody touched.
func TestCookieImportHandlerEndsInADetachedFlushedRecheck(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cookies.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cookies.go: %v", err)
	}
	handler := routeHandlerLit(t, file, "/api/cookies/import")

	var deferred *ast.FuncLit
	ast.Inspect(handler, func(n ast.Node) bool {
		if deferred != nil {
			return false
		}
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if lit, ok := d.Call.Fun.(*ast.FuncLit); ok {
			ast.Inspect(lit, func(inner ast.Node) bool {
				if sel, ok := inner.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckNow" {
					deferred = lit
					return false
				}
				return true
			})
		}
		return true
	})
	if deferred == nil {
		t.Fatal("the import handler has no deferred CheckNow. A credential write that reaches no " +
			"refresh pass is invisible until the 30-minute ticker: the Twitch auth mark taken under " +
			"the old pair stands over a file that no longer has that problem, and no live chat " +
			"session is told to reconnect")
	}

	var callsFlush, guardsOnWrote, passesAnIdent, buildsOwnContext, derivesFromRequest bool
	ast.Inspect(deferred, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Flush" {
				callsFlush = true
			}
			// The context is built from a ROOT, and `context.WithTimeout` is
			// NOT one — deliberately, because it is the call that makes the
			// dangerous mutation look correct: `context.WithTimeout(req.Context(),
			// 45*time.Second)` is a timeout on a context that still cancels with
			// the tab, and accepting any WithTimeout would wave it straight
			// through. The parent is the whole question, so only the roots
			// themselves count.
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" &&
				(sel.Sel.Name == "Background" || sel.Sel.Name == "WithoutCancel") {
				buildsOwnContext = true
			}
			if sel.Sel.Name == "CheckNow" && len(v.Args) == 1 {
				// And the argument is an identifier rather than a call, so what
				// is handed over is the context built above rather than
				// req.Context() inline.
				_, isIdent := v.Args[0].(*ast.Ident)
				passesAnIdent = isIdent
			}
		case *ast.SelectorExpr:
			if v.Sel.Name == "Wrote" {
				guardsOnWrote = true
			}
			// req.Context ANYWHERE inside the defer, whatever it is wrapped in.
			// This is the assertion the other two cannot make: a WithTimeout
			// around the request context passes both of them, and only the
			// presence of this selector gives it away.
			if x, ok := v.X.(*ast.Ident); ok && x.Name == "req" && v.Sel.Name == "Context" {
				derivesFromRequest = true
			}
		}
		return true
	})

	if !guardsOnWrote {
		t.Error("the deferred re-check does not consult result.Wrote — a rejected paste spends two " +
			"validate round-trips on a file nobody touched")
	}
	if !callsFlush {
		t.Error("the deferred re-check does not Flush before running. jsonResponse writes into a " +
			"bufio writer that is not flushed and the handler does not return until this defer " +
			"completes, so the client waits out the whole re-check")
	}
	if !passesAnIdent {
		t.Error("CheckNow is called with an expression rather than a context identifier — if that is " +
			"req.Context(), a client that navigates away cancels the fingerprint comparison its own " +
			"import caused")
	}
	if !buildsOwnContext {
		t.Error("the deferred re-check never calls context.Background (or context.WithoutCancel), so " +
			"whatever identifier it hands CheckNow came from somewhere else. `ctx := req.Context(); " +
			"refreshSvc.CheckNow(ctx)` satisfies the identifier check above on its own, and is " +
			"exactly the mutation this assertion exists for. context.WithTimeout is deliberately not " +
			"accepted here: it says nothing about the parent, which is the whole question")
	}
	if derivesFromRequest {
		t.Error("the deferred re-check reaches req.Context() — a timeout wrapped around the REQUEST's " +
			"context is not a detached one: it still cancels when the client goes away, and " +
			"`context.WithTimeout(req.Context(), 45*time.Second)` satisfies both checks above while " +
			"doing exactly the thing they exist to prevent. Build from context.Background() instead. " +
			"(A deliberate context.WithoutCancel(req.Context()) — the request's VALUES without its " +
			"cancellation — would also trip this; that is a decision worth making explicitly here, " +
			"with a reason, rather than one that slips in.)")
	}
}

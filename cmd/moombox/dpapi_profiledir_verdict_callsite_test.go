package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDpapiProfileDirVerdictIsLoggedAfterTheFallbackFlagIsMirrored pins the
// ORDER of two statements in initServices, for the same reason its sibling
// TestProfileDirVerdictIsLoggedAfterTheModeIsWired pins a different pair: the
// seam is a statement's position, there is nothing to inject, and getting it
// wrong is silent.
//
// LogDpapiProfileDirVerdict has an arm that fires when cookies.dpapi_profile_dir
// is set while cookies.dpapi_fallback is off — the likeliest misconfiguration
// there is, since that flag defaults to false. The flag is mirrored onto the
// service by a configStore.Read block partway down initServices. Call the
// verdict before that block and it reads the zero value: every boot warns that
// the fallback is off, including the ones where the operator turned it on.
//
// THE MUTANTS:
//   - hoist the call above the DpapiFallback assignment: the index check fires.
//   - delete the call: the "not found" fatal fires and a misconfigured
//     directory boots silently again.
//   - move the call into some other function: initServices' body no longer
//     contains it, and the same fatal fires.
func TestDpapiProfileDirVerdictIsLoggedAfterTheFallbackFlagIsMirrored(t *testing.T) {
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

	// The assignment is nested inside a configStore.Read closure, so it is
	// found by walking the statement's subtree rather than by matching the
	// top-level statement itself; the INDEX recorded is still the top-level
	// statement that contains it, which is what the ordering is about.
	assignIdx, callIdx := -1, -1
	for i, stmt := range body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignStmt); ok {
				if len(a.Lhs) == 1 && selectorIs(a.Lhs[0], "autoCookieSvc", "DpapiFallback") {
					assignIdx = i
				}
			}
			return true
		})
		if e, ok := stmt.(*ast.ExprStmt); ok {
			if call, ok := e.X.(*ast.CallExpr); ok && selectorIs(call.Fun, "autoCookieSvc", "LogDpapiProfileDirVerdict") {
				callIdx = i
			}
		}
	}

	if assignIdx < 0 {
		t.Fatal("initServices no longer assigns autoCookieSvc.DpapiFallback — the ordering this test " +
			"exists for cannot be checked")
	}
	if callIdx < 0 {
		t.Fatal("initServices never calls autoCookieSvc.LogDpapiProfileDirVerdict, so a cookies.dpapi_profile_dir " +
			"that is unusable — or simply unread because dpapi_fallback is off — boots silently")
	}
	if callIdx < assignIdx {
		t.Errorf("LogDpapiProfileDirVerdict is called at statement %d, before autoCookieSvc.DpapiFallback is "+
			"mirrored at %d — the verdict then reads the zero value and warns \"dpapi_fallback is off\" on "+
			"every boot, including the ones where it is on", callIdx, assignIdx)
	}
}

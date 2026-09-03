package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProfileDirVerdictIsLoggedAfterTheModeIsWired pins the ORDER, which is the
// only thing that makes the fix a fix.
//
// LogProfileDirVerdict picks its level from resolvedAcquisition, which reads
// AcquisitionMode; a nil callback resolves to "auto". So calling it before the
// AcquisitionMode closure is assigned reproduces Arc 12c arc-close F1 exactly —
// an ERROR on every boot of the README's profile-mode recipe — while every
// behavioural test in internal/cookies stays green, because they wire the
// callback themselves. Nothing but the call ORDER in this file can catch it,
// and this package cannot drive initServices.
//
// Structural for the same reason internal/web/routes'
// cookies_import_callsite_test.go is: the seam is a statement's position, and
// there is no seam to inject.
//
// THE THREE MUTANTS:
//   - hoist the call above the AcquisitionMode assignment: the index check fails.
//   - delete the call: the "not found" fatal fires.
//   - move the call into some other function: initServices' body no longer
//     contains it, and the same fatal fires.
func TestProfileDirVerdictIsLoggedAfterTheModeIsWired(t *testing.T) {
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

	assignIdx, callIdx := -1, -1
	for i, stmt := range body.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 1 && selectorIs(s.Lhs[0], "autoCookieSvc", "AcquisitionMode") {
				assignIdx = i
			}
		case *ast.ExprStmt:
			call, ok := s.X.(*ast.CallExpr)
			if ok && selectorIs(call.Fun, "autoCookieSvc", "LogProfileDirVerdict") {
				callIdx = i
			}
		}
	}

	if assignIdx < 0 {
		t.Fatal("initServices no longer assigns autoCookieSvc.AcquisitionMode at its top level — " +
			"the ordering this test exists for cannot be checked")
	}
	if callIdx < 0 {
		t.Fatal("initServices never calls autoCookieSvc.LogProfileDirVerdict, so the launch guard's " +
			"verdict is never reported at all: a refused browser_profile_dir now boots silently in " +
			"both modes")
	}
	if callIdx < assignIdx {
		t.Errorf("LogProfileDirVerdict is called at statement %d, before AcquisitionMode is wired at "+
			"%d — resolvedAcquisition reads nil as \"auto\", so a cookies.acquisition = \"profile\" "+
			"install logs the launch refusal at ERROR on every boot (Arc 12c arc-close F1)",
			callIdx, assignIdx)
	}
}

// selectorIs reports whether e is exactly `recv.sel`.
func selectorIs(e ast.Expr, recv, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == recv
}

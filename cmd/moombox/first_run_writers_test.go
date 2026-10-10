package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The boot-time cookie-platform detection and PersistPlatforms must not be a
// first run's first Save: Save marks the config loaded, and both setup wizards
// key off that flag. A ConfigLoaded check INSIDE an Update closure is no
// guard — Update saves whatever the closure did, nothing included — so the
// pattern itself is refused here, and both writers must go through
// UpdateIfLoaded (internal/config/store.go, which its own test pins).
//
// Mutant: either writer back on configStore.Update.
func TestFirstRunWritersGoThroughUpdateIfLoaded(t *testing.T) {
	const name = "services.go"
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	ifLoaded := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		body := string(src[fset.Position(call.Pos()).Offset:fset.Position(call.End()).Offset])
		switch sel.Sel.Name {
		case "UpdateIfLoaded":
			ifLoaded++
		case "Update":
			if strings.Contains(body, "ConfigLoaded") {
				t.Errorf("%s: an Update closure checks ConfigLoaded — Update saves anyway; use UpdateIfLoaded",
					fset.Position(call.Pos()))
			}
			if strings.Contains(body, "c.Cookies.Platforms = detected") || strings.Contains(body, "c.Cookies.Platforms = platforms") {
				t.Errorf("%s: a cookie-platform bookkeeping write goes through Update, which saves during a first run",
					fset.Position(call.Pos()))
			}
		}
		return true
	})
	if ifLoaded < 2 {
		t.Errorf("found %d UpdateIfLoaded writers in %s, want the detection and PersistPlatforms", ifLoaded, name)
	}
}

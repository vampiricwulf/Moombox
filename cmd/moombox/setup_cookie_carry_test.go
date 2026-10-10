package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCarrySetupCookiesCarriesTheBootCookieFile pins W26-06's wiring: both
// setup wizards carry the cookies a browser login wrote — to the cookie file
// this run's cookie service was built with — into the cookie file they save,
// through the one call, the service's CarryCookieFileTo.
//
// Mutant killed: carrySetupCookies not calling CarryCookieFileTo (the saved
// cookie file is never written).
func TestCarrySetupCookiesCarriesTheBootCookieFile(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(boot, []byte("# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfrom-login\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &runState{autoCookieSvc: cookies.NewAutoCookieService(t.TempDir(), boot, cookies.NewCookieJar(), &nopLogger{})}
	custom := filepath.Join(dir, "data", "my-cookies.txt")

	if err := s.carrySetupCookies(custom); err != nil {
		t.Fatalf("carrySetupCookies: %v", err)
	}
	if data, err := os.ReadFile(custom); err != nil || !strings.Contains(string(data), "from-login") {
		t.Errorf("the saved cookie file holds %q (%v), want the login's cookies", data, err)
	}
	if err := (&runState{}).carrySetupCookies(custom); err != nil {
		t.Errorf("no cookie service: %v, want nothing to carry", err)
	}
}

// TestBothSetupWizardsCarryCookies: the Web wizard's SetupDeps and the TUI
// wizard are both handed carrySetupCookies — one rule for both, as the rest
// of the setup flow is.
//
// Mutants killed: dropping CarryCookies from SetupDeps (routes_wiring.go);
// dropping the SetupWizCarryCookies call (tui_wiring.go).
func TestBothSetupWizardsCarryCookies(t *testing.T) {
	isCarry := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "carrySetupCookies"
	}
	found := map[string]bool{}
	for _, name := range []string{"routes_wiring.go", "tui_wiring.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "CarryCookies" && isCarry(n.Value) {
					found["SetupDeps.CarryCookies"] = true
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetupWizCarryCookies" &&
					len(n.Args) == 1 && isCarry(n.Args[0]) {
					found["SetupWizCarryCookies"] = true
				}
			}
			return true
		})
	}
	for _, want := range []string{"SetupDeps.CarryCookies", "SetupWizCarryCookies"} {
		if !found[want] {
			t.Errorf("%s is not wired to carrySetupCookies", want)
		}
	}
}

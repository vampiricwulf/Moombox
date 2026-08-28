package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// chordWiring is one `app.On<Name> = func...` assignment inside runTUI, with
// the two facts this file is about.
type chordWiring struct {
	found       bool
	conditional bool // the assignment sits inside an if / for / switch / select
	readsFlag   bool // the assigned closure reads Cookies.AutoEnabled
}

// findChordWirings parses runTUI and reports, for each requested App callback,
// whether its assignment is nested inside a control-flow statement and whether
// the closure it assigns reads the auto_enabled flag.
//
// Parsed rather than pattern-matched, because both claims are about STRUCTURE
// and a substring check cannot express either. "The assignment is not gated"
// has no text of its own — the gate is a line that is absent — and a search for
// `if s.cfg.Cookies.AutoEnabled` would be defeated by any rename, by a hoisted
// local, or by a live read spelled through the config store. Nesting is the
// property that actually matters and it survives all three.
//
// runTUI is a 600-line method that needs the whole service graph to call, so
// executing it is not an option; this is the strongest assertion available at
// this site, and internal/tui's TestForceRefreshChordExistsWheneverItIsWired
// supplies the behaviour half — what a nil callback costs.
func findChordWirings(t *testing.T, names ...string) map[string]*chordWiring {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "runTUI" && fn.Recv != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("runTUI is no longer a method in tui_wiring.go — this test cannot find what it guards")
	}

	want := map[string]*chordWiring{}
	for _, n := range names {
		want[n] = &chordWiring{}
	}

	// Walked by hand rather than with ast.Inspect, because the property under
	// test is the DEPTH of control-flow statements above each assignment and
	// Inspect's single callback cannot carry it. An assignment at nesting 0 is
	// unconditional; at any depth above, a condition decides whether the
	// callback is assigned — which for these fields decides whether the chord
	// exists at all.
	var walk func(n ast.Node, nested int)
	walk = func(n ast.Node, nested int) {
		if n == nil {
			return
		}
		childNesting := nested
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt:
			childNesting = nested + 1
		}
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "app" {
					if w, watched := want[sel.Sel.Name]; watched {
						w.found = true
						w.conditional = nested > 0
						w.readsFlag = readsAutoEnabled(assign.Rhs[0])
					}
				}
			}
		}
		for _, child := range childrenOf(n) {
			walk(child, childNesting)
		}
	}
	walk(body, 0)

	for name, w := range want {
		if !w.found {
			t.Fatalf("runTUI no longer assigns app.%s — this test cannot guard a site that moved", name)
		}
	}
	return want
}

// childrenOf yields a node's children via ast.Inspect on that node alone.
func childrenOf(n ast.Node) []ast.Node {
	var out []ast.Node
	first := true
	ast.Inspect(n, func(c ast.Node) bool {
		if first {
			first = false
			return true
		}
		if c != nil {
			out = append(out, c)
		}
		return false
	})
	return out
}

// readsAutoEnabled reports whether an expression mentions the AutoEnabled
// config field anywhere inside it. Matched on the SELECTOR, so it holds however
// the surrounding read is spelled — a direct s.cfg read, a configStore.Read
// closure, a local copied out first.
func readsAutoEnabled(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "AutoEnabled" {
			found = true
		}
		return !found
	})
	return found
}

// TestCookieChordsAreWiredWithoutReadingAutoEnabled pins the whole of the
// auto_enabled decision at this site, for both manual cookie triggers.
//
// NOT CONDITIONAL. A nil callback does not make a chord inert, it DELETES it:
// dispatchAction, buildMenuItems and the help overlay all test the field (see
// internal/tui's TestForceRefreshChordExistsWheneverItIsWired, which measures
// exactly that). R F used to be assigned only `if s.cfg.Cookies.AutoEnabled`, a
// snapshot taken at process start, so a disabled install had no chord at all
// and an install where the flag was turned off later still had one.
//
// AND NOT FLAG-READING, which is the half a nesting check alone would miss. A
// live read that refused would move the same hiding behind a different
// mechanism, and it would be wrong on its own terms: R F means "refresh these
// cookies by the strongest means available", and with the headless browser off
// the strongest available means is an immediate import from the browser
// profile — precisely what an operator who has just hand-updated that profile
// wants. The flag belongs one level down, in AutoCookieService, where it drops
// the browser rather than the work. R C, the in-process trigger, is never gated
// by anything and is included so the rule is asserted for both.
//
// The AutoEnabled check is on the SELECTOR, so it holds however a reintroduced
// read is spelled — s.cfg directly, a configStore.Read closure, a local copied
// out first.
func TestCookieChordsAreWiredWithoutReadingAutoEnabled(t *testing.T) {
	wirings := findChordWirings(t, "OnForceRefreshCookies", "OnRecheckCookies")

	for _, tc := range []struct {
		field string
		chord string
		why   string
	}{
		{
			field: "OnForceRefreshCookies", chord: "R F",
			why: "R F runs the auto-cookie pass by the strongest means available. With the headless " +
				"browser disabled that is an immediate profile import, which is the whole point of " +
				"the gesture on a hand-updated profile — refusing here would deny it",
		},
		{
			field: "OnRecheckCookies", chord: "R C",
			why: "R C triggers the in-process Go refresh, which runs on every install and is gated " +
				"only on cookies.cookie_file",
		},
	} {
		t.Run(tc.chord, func(t *testing.T) {
			w := wirings[tc.field]
			if w.conditional {
				t.Errorf("app.%s is assigned inside a conditional, so whether the %s chord EXISTS "+
					"depends on that condition — a nil callback removes it from dispatch, the action "+
					"menu and help: %s", tc.field, tc.chord, tc.why)
			}
			if w.readsFlag {
				t.Errorf("app.%s reads Cookies.AutoEnabled. The flag governs which MECHANISM the "+
					"refresh uses, which AutoCookieService decides; reading it here can only refuse "+
					"the chord: %s", tc.field, tc.why)
			}
		})
	}
}

// TestBrowserLaunchGateIsWiredFromTheConfig pins the production end of the
// gate, and a mutation run is why it exists.
//
// AutoCookieService.BrowserLaunchAllowed defaults to permissive, deliberately,
// so that every caller and test written before it keeps working. That default
// also means the gate is INERT until something wires it: delete the assignment
// in initServices and internal/cookies' own tests stay green while a disabled
// install goes back to launching headless browsers from R F and the dashboard's
// shift+click. Removing that assignment survived the first mutation pass with
// nothing failing.
//
// Three claims, and the third is not decoration. A read of the cfg struct
// captured at startup would satisfy the first two while freezing the answer for
// the life of the process — so an operator who turns the setting on and presses
// R F would still get no browser. The live read goes through configStore, which
// is the seam a settings save writes to.
func TestBrowserLaunchGateIsWiredFromTheConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatalf("parse services.go: %v", err)
	}

	var rhs ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "BrowserLaunchAllowed" {
			rhs = assign.Rhs[0]
		}
		return true
	})
	if rhs == nil {
		t.Fatal("nothing in services.go assigns BrowserLaunchAllowed. The field defaults to " +
			"permissive, so cookies.auto_enabled now gates nothing at all: R F and the dashboard's " +
			"shift+click launch a headless browser on an install that switched them off")
	}
	if !readsAutoEnabled(rhs) {
		t.Error("BrowserLaunchAllowed is wired to something that never reads Cookies.AutoEnabled, " +
			"so the predicate cannot be reporting the setting it stands for")
	}

	live := false
	ast.Inspect(rhs, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if inner, ok := fn.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "configStore" && readsAutoEnabled(call) {
			live = true
		}
		return true
	})
	if !live {
		t.Error("BrowserLaunchAllowed's auto_enabled read does not go through s.configStore. A read " +
			"of the cfg struct captured at startup freezes the answer for the life of the process, " +
			"so turning the setting on would not reach R F until the next restart")
	}
}

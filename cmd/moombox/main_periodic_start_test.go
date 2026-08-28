package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// gateChain is every condition a call sits under: for each enclosing `if`, its
// Init statement (where `if _, err := os.Stat(dir); err == nil` hides its real
// work) and its Cond.
func gateChain(t *testing.T, filename, method string) []ast.Node {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	var found []ast.Node
	hits := 0

	var walk func(n ast.Node, gates []ast.Node)
	walk = func(n ast.Node, gates []ast.Node) {
		if n == nil {
			return
		}
		if ifStmt, ok := n.(*ast.IfStmt); ok {
			// Init and Cond are evaluated OUTSIDE the branch they guard, so
			// they carry the enclosing gates, not their own.
			if ifStmt.Init != nil {
				walk(ifStmt.Init, gates)
			}
			walk(ifStmt.Cond, gates)
			inner := append(append([]ast.Node{}, gates...), ifStmt.Cond)
			if ifStmt.Init != nil {
				inner = append(inner, ifStmt.Init)
			}
			walk(ifStmt.Body, inner)
			if ifStmt.Else != nil {
				// An else branch is guarded by the NEGATION, which none of the
				// assertions below are about. Walked with the outer gates so a
				// call moved there is reported as ungated rather than silently
				// inheriting a condition it does not have.
				walk(ifStmt.Else, gates)
			}
			return
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
				hits++
				found = gates
			}
		}
		for _, c := range childrenOf(n) {
			walk(c, gates)
		}
	}
	walk(file, nil)

	if hits == 0 {
		t.Fatalf("%s no longer calls %s — this test cannot guard a site that moved", filename, method)
	}
	if hits > 1 {
		t.Fatalf("%s calls %s %d times; this test reports the last and would hide the others",
			filename, method, hits)
	}
	return found
}

// rootIdent returns the leftmost identifier of a selector chain: `cfg` for
// `cfg.Cookies.RefreshInterval.AsDuration`, `os` for `os.Stat`.
func rootIdent(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.CallExpr:
			expr = e.Fun
		case *ast.IndexExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		default:
			return ""
		}
	}
}

// TestPeriodicRefreshStartsOnTheFlagAloneAndProbesNothing pins the start
// condition of the headless-browser refresh timer.
//
// cookies.auto_enabled IS that timer — it governs nothing else automatic — so
// the flag has to be the condition, and it has to be the WHOLE condition apart
// from an interval that means something.
//
// It used to be wrapped in `if _, err := os.Stat(browserProfileDir); err == nil`
// as well, which froze at boot the one input that changes at runtime.
// Completing setup is what CREATES that directory, so an operator who turned
// the setting on and then ran setup — the sequence an "auth lost" notification
// asks for — got no timer until the next restart, and nothing told them: no
// setting had changed by then, so the restart-required labelling never fired
// either. The question is asked per tick now, quietly, by
// AutoCookieService.periodicRefreshHasSource; internal/cookies'
// TestPeriodicLoopPicksUpAProfileThatAppearsAtRuntime holds the behaviour half.
//
// Asserted as STRUCTURE, because "the probe is gone" has no text of its own and
// a search for `os.Stat` would be defeated by any equivalent: os.Lstat,
// os.ReadDir, a helper called profileExists. What all of those share is that
// they are calls to something other than the config, so that is the rule —
// the start condition must be answerable from configuration alone.
func TestPeriodicRefreshStartsOnTheFlagAloneAndProbesNothing(t *testing.T) {
	gates := gateChain(t, "main.go", "StartPeriodicRefresh")
	if len(gates) == 0 {
		t.Fatal("StartPeriodicRefresh is called unconditionally. cookies.auto_enabled IS the " +
			"headless-browser refresh timer: with no gate at all, an install that switched the " +
			"setting off still runs one")
	}

	flagRead := false
	for _, g := range gates {
		if readsAutoEnabled(g) {
			flagRead = true
		}
		ast.Inspect(g, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Methods on the config are fine — cfg.Cookies.RefreshInterval.
			// AsDuration is a pure accessor. Anything else in a boot-time gate
			// is a probe of the world, and the world is what moves afterwards.
			if root := rootIdent(call.Fun); root == "cfg" || root == "s" {
				return true
			}
			t.Errorf("the start condition for the periodic cookie refresh calls %s. It must be "+
				"answerable from configuration alone: a filesystem probe here decides ONCE, at "+
				"boot, whether the timer ever exists — and completing setup, which is what creates "+
				"the browser profile directory, happens afterwards. Ask it per tick instead (see "+
				"periodicRefreshHasSource)", exprText(call.Fun))
			return false
		})
	}
	if !flagRead {
		t.Error("nothing in the start condition reads Cookies.AutoEnabled, so the flag no longer " +
			"decides whether the headless-browser refresh timer exists — which is the only thing " +
			"it governs")
	}
}

// TestProfileSeedIsWiredWithoutTheFlag pins the one-shot boot import's wiring.
//
// It used to live inside StartPeriodicRefresh, which coupled it to
// cookies.auto_enabled for no reason it could justify: the flag governs a
// REPEATING read of a profile that nothing changes between ticks, and a boot is
// the one moment a mounted profile plausibly did change — something replaced it
// while the process was down. The consequence was that a browserless install
// with the flag off (which is what the Docker docs now tell operators to run)
// mounted a profile, restarted, and imported nothing at all.
//
// UNCONDITIONAL here, and that is the assertion. The decision is not "don't
// gate it" — it is "gate it in ONE place", AutoCookieService.decideStartupSeed,
// which owns every condition including the one that makes it safe: no cookies
// to lose. A condition re-stated at this call site is a second copy of a rule
// that already exists, and the two drift.
func TestProfileSeedIsWiredWithoutTheFlag(t *testing.T) {
	gates := gateChain(t, "main.go", "StartProfileSeed")

	for _, g := range gates {
		if readsAutoEnabled(g) {
			t.Error("the one-shot boot import is gated on Cookies.AutoEnabled again. The flag owns " +
				"the periodic timer and nothing else: a boot is when a mounted profile most " +
				"plausibly changed, and the condition that makes the import safe is an absent " +
				"cookies.txt, not a setting")
		}
	}
	if len(gates) > 0 {
		t.Errorf("StartProfileSeed is called under %d condition(s) in main.go. Every condition it "+
			"needs already lives in decideStartupSeed — profile configured, no browser, an "+
			"importable profile, and nothing on disk to lose — and a second copy here is what "+
			"drifts from the first", len(gates))
	}
}

// TestExpectedPlatformSeedingStaysGatedOnTheFlag guards the block two above the
// one this file changed.
//
// SetExpectedPlatforms is called only when cookies.auto_enabled is on. That
// looks like it needlessly denies manual-cookie installs the cross-restart
// "auth lost" detection, and dropping it has been proposed for exactly that
// reason — it was analysed and OVERTURNED, with the derivation pinned in the
// comment above the line and mutation-checked by internal/cookies'
// TestSeedingIsUnnecessaryForStartupDeadAuthAndFiresFalselyWithoutCookies.
//
// Manual installs already get the detection without seeding, and seeding sets
// everConcluded for platforms this process never checked, which fires "auth
// lost" after every restart for a platform nobody configured. This asserts the
// gate is still there, because the next edit in this region will be near it.
func TestExpectedPlatformSeedingStaysGatedOnTheFlag(t *testing.T) {
	gates := gateChain(t, "main.go", "SetExpectedPlatforms")
	for _, g := range gates {
		if readsAutoEnabled(g) {
			return
		}
	}
	t.Error("SetExpectedPlatforms is no longer gated on Cookies.AutoEnabled. That gate was analysed " +
		"and kept, not overlooked: without it, seeding marks platforms this process never checked as " +
		"concluded, and every restart fires an 'auth lost' notification telling the operator to " +
		"re-export credentials for a platform they never configured. See the derivation in the " +
		"comment above the call")
}

// exprText renders an expression for an error message without a FileSet.
func exprText(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprText(e.X) + "." + e.Sel.Name
	default:
		return "a computed expression"
	}
}

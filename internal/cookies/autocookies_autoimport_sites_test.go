package cookies

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// callersOf returns the names of the package's functions and methods that call
// the named function, reading every non-test .go file in the package.
//
// Parsed rather than grepped because the claim is about WHICH FUNCTION a call
// sits in, which a substring search cannot express: a match tells you the file,
// not the caller, and the two automatic sites and the manual one live in the
// same two files.
func callersOf(t *testing.T, callee string) []string {
	t.Helper()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	var callers []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// Matched on the selector, so it holds for s.x(), the receiver
				// renamed, or a call through another value of the same type.
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == callee {
					if !slices.Contains(callers, fn.Name.Name) {
						callers = append(callers, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(callers)
	return callers
}

// refKind says how a name is mentioned: called outright, or taken as a value.
type refKind string

const (
	refCall        refKind = "call"
	refMethodValue refKind = "method value"
)

// referencesOutsidePackage walks the whole repository (excluding this package,
// tests, and anything hidden or vendored) and reports every mention of a
// selector name, keyed by repo-relative file, with how it was mentioned.
//
// REFERENCES, not calls, and that distinction is the entire point. A method
// value — `s.autoCookieSvc.RefreshCookiesDetailed` handed to another function —
// is a SelectorExpr that is NOT the Fun of a CallExpr, so a call-site search
// walks straight past it. That is not hypothetical: it is how the fourth caller
// of RefreshCookiesDetailed went uncounted, and the uncounted one was the only
// automatic caller in the list.
func referencesOutsidePackage(t *testing.T, name string) map[string]refKind {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}

	found := map[string]refKind{}
	scanned := 0
	fset := token.NewFileSet()

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			// Hidden dirs cover .git and .worktrees — the latter holds a full
			// copy of the tree and would double every count.
			if strings.HasPrefix(base, ".") || base == "references" || base == "node_modules" {
				return filepath.SkipDir
			}
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}

		// Positions of every selector that IS the callee of a call, so
		// everything else is a value.
		callee := map[token.Pos]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					callee[sel.Pos()] = true
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return true
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			key := filepath.ToSlash(rel)
			kind := refMethodValue
			if callee[sel.Pos()] {
				kind = refCall
			}
			// A file with both forms is reported as a call; no file has both
			// today, and the assertion below would fail loudly if one did.
			if prev, seen := found[key]; !seen || prev == refMethodValue {
				found[key] = kind
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	if scanned < 50 {
		t.Fatalf("only %d Go files were scanned from %s — the walk is not reaching the repository, "+
			"so an empty result below would mean nothing", scanned, root)
	}
	return found
}

// TestRefreshCookiesDetailedCallersAreEnumerated pins the list its own doc
// comment states, because that list was wrong once and the error was invisible.
//
// A review wrote "all three external callers go through the exported
// delegation". There are four. The missing one is
// cmd/moombox/monitor_callbacks.go, which passes the method as a VALUE rather
// than calling it — and the omission mattered: it is the only AUTOMATIC caller
// in the list, and noticing it is what surfaced the question of whether the
// recovery path should be subject to automaticImportGuard (it should not, on
// purpose; see the guard's doc).
//
// So this matches references rather than calls, and asserts the KIND of each,
// so the method-value shape cannot slip past again.
func TestRefreshCookiesDetailedCallersAreEnumerated(t *testing.T) {
	want := map[string]refKind{
		"internal/web/routes/cookies.go":   refCall,
		"cmd/moombox/tui_wiring.go":        refCall,
		"cmd/moombox/services.go":          refCall,
		"cmd/moombox/monitor_callbacks.go": refMethodValue,
	}
	got := referencesOutsidePackage(t, "RefreshCookiesDetailed")

	for file, wantKind := range want {
		gotKind, ok := got[file]
		if !ok {
			t.Errorf("%s no longer references RefreshCookiesDetailed. If the caller moved, move it "+
				"here and in the method's doc comment — the count in that doc is load-bearing",
				file)
			continue
		}
		if gotKind != wantKind {
			t.Errorf("%s references RefreshCookiesDetailed as a %s, expected a %s. If a call became "+
				"a method value, note it in the doc: that shape is invisible to a call-site search",
				file, gotKind, wantKind)
		}
	}
	for file, kind := range got {
		if _, expected := want[file]; !expected {
			t.Errorf("%s references RefreshCookiesDetailed (%s) and is not in the documented list of "+
				"four. Add it deliberately, and decide the question its presence raises: is it a "+
				"live operator gesture, or is it AUTOMATIC? An automatic one has to be weighed "+
				"against automaticImportGuard — that is the decision the last uncounted caller hid",
				file, kind)
		}
	}
}

// TestAutomaticImportGuardHasExactlyItsTwoAutomaticCallers is how "one rule"
// survives contact with the next change.
//
// The owner's rule — an automatic browser-free import runs only when there is
// no cookies.txt to lose — is written once, in automaticImportGuard, and used
// at exactly two AUTOMATIC sites:
//
//	decideStartupSeed     the one-shot import at boot
//	StartPeriodicRefresh  the timer's tick, when that tick would be browser-free
//
// Two failure modes, both structural, both invisible to a behavioural test that
// only exercises today's paths:
//
//   - A THIRD caller. Usually the same rule hand-copied somewhere and then
//     drifting from this one, which is the exact shape this arc exists to
//     remove. A new automatic site is legitimate — but it has to be added here
//     deliberately, with a reason, rather than appearing.
//   - A caller inside refreshCookiesDetailed. That is what ALL FOUR external
//     callers reach, and two different exemptions die together if the rule is
//     put there:
//     the MANUAL triggers (R F, the dashboard's shift+click, the Settings
//     page's profile-import button), where importing over a live cookies.txt
//     is the entire gesture and "update the mounted profile, then press R F" is
//     the designated workflow on a browserless host; and the RECOVERY path,
//     which is automatic but runs only on a conclusive not-authenticated, so
//     the rule would refuse the one automatic import most likely to fix the
//     problem. The behavioural half is
//     TestManualRefreshImportsRegardlessOfTheCookieFile; this half names the
//     mechanism, so a reviewer sees it in the diff rather than in a red test.
//
// PAIRED WITH TestManualRefreshImportsRegardlessOfTheCookieFile — DO NOT DELETE
// EITHER AS REDUNDANT. This half is evadable: a method value
// (`rule := s.automaticImportGuard; rule()`) is not a SelectorExpr call and
// slips straight past the walk below. That exact mutation was run; this test
// passed and the behavioural one killed it. The reverse also holds — the
// behavioural test only exercises paths that exist today, so a NEW automatic
// caller with the rule hand-copied into it is invisible to it and lands here.
// Neither half covers the other's blind spot.
func TestAutomaticImportGuardHasExactlyItsTwoAutomaticCallers(t *testing.T) {
	want := []string{"StartPeriodicRefresh", "decideStartupSeed"} // sorted
	got := callersOf(t, "automaticImportGuard")

	if len(got) == 0 {
		t.Fatal("nothing calls automaticImportGuard. The rule is inert: the boot seed and the " +
			"periodic timer both import over whatever cookies.txt holds")
	}
	if !slices.Equal(got, want) {
		t.Errorf("automaticImportGuard callers = %v, want %v.\n"+
			"A new AUTOMATIC site is a real decision — add it here with its reason. A caller on the "+
			"MANUAL path is a bug: R F and the dashboard's shift+click must import whatever the "+
			"cookie file holds, and on a browserless host that gesture is the only cookie path there "+
			"is", got, want)
	}

	// Named explicitly rather than left to the set comparison, so the failure
	// says what broke instead of printing two lists and letting the reader
	// diff them.
	for _, manual := range []string{"RefreshCookiesDetailed", "refreshCookiesDetailed"} {
		if slices.Contains(got, manual) {
			t.Errorf("%s calls automaticImportGuard. That is the function R F and the dashboard's "+
				"shift+click both reach, and gating it on an absent cookies.txt removes the "+
				"designated workflow for a browserless host: update the mounted profile, press R F",
				manual)
		}
	}
}

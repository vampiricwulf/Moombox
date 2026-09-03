package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// recheckSite is one production call to recheckAfterCookieWrite with what the
// walk could establish about where it sits.
type recheckSite struct {
	file                string
	line                int
	gesture             string
	deferred            bool
	inPassCompletedHook bool
}

// TestEveryCookieWriteRecheckIsDeferred pins the SHAPE of the five re-check
// sites the Arc 10 reload-site table names.
//
// Shape and not behaviour: refresh's status block is the only place the Twitch
// credential fingerprint is compared, the auth mark cleared and
// OnCredentialsChanged fired, and it runs only inside a refresh pass — so every
// gesture that can put new credentials on disk has to reach CheckNow, or a
// repaired cookie waits on the 30-minute ticker while a stale "Twitch needs
// re-authorization" stands over a file that no longer has that problem and no
// live chat session is told to reconnect. Driving any of these five from a test
// means a live guide POST and a live oauth2/validate (youtubeGuideURL and
// twitchValidateURL are unexported package vars in internal/cookies), so the
// behavioural half is pinned inside that package and what can only be asserted
// HERE is that the call is placed where it cannot be skipped.
//
// THE DEFER IS LOAD-BEARING. Three of the eight refreshAborted() exits happen
// AFTER cookies.txt was rewritten, so a call placed after the error check
// returns first on exactly those passes — the ones whose write nobody
// compared. Hoisting one out is the obvious "simplification" and is invisible
// to every behavioural test in the tree. THE MUTANT: move any of the four out
// of its defer and place it after the `if err != nil` block.
//
// The fifth site is OnPassCompleted, which is NOT deferred and must not be: it
// is a hook fired later by notePassCompleted() from inside internal/cookies,
// through postRefreshRecheckHook's recover guard. It is pinned by its own
// shape. Its mutant: assign the bare closure, losing the recover, and a panic
// on the auto-cookie goroutine takes the process down.
func TestEveryCookieWriteRecheckIsDeferred(t *testing.T) {
	var sites []recheckSite
	for _, name := range []string{"monitor_callbacks.go", "services.go", "tui_wiring.go"} {
		sites = append(sites, recheckSitesIn(t, name)...)
	}

	// The reload-site table, by the gesture each call names. A gesture missing
	// here is a site deleted or renamed; an unexpected one is a new
	// credential-writing gesture whose shape nobody has decided.
	wantGestures := []string{
		"an automatic cookie refresh",
		"browser refresh",
		"recovery",
		"the job-triggered cookie refresh",
		"the setup wizard",
	}
	var gotGestures []string
	for _, s := range sites {
		gotGestures = append(gotGestures, s.gesture)
	}
	sort.Strings(gotGestures)
	if len(gotGestures) != len(wantGestures) {
		t.Fatalf("found %d recheckAfterCookieWrite call sites %v, want the %d in the Arc 10 reload-site table %v",
			len(gotGestures), gotGestures, len(wantGestures), wantGestures)
	}
	for i := range wantGestures {
		if gotGestures[i] != wantGestures[i] {
			t.Fatalf("call sites are %v, want %v — a gesture was renamed, or a new credential-writing site appeared with no decision about its shape",
				gotGestures, wantGestures)
		}
	}

	hooks := 0
	for _, s := range sites {
		switch {
		case s.inPassCompletedHook:
			hooks++
			if s.deferred {
				t.Errorf("%s:%d (%q) is BOTH the OnPassCompleted hook and deferred — one reading is wrong", s.file, s.line, s.gesture)
			}
		case !s.deferred:
			t.Errorf("%s:%d (%q) calls recheckAfterCookieWrite outside a defer. Three of the eight "+
				"refreshAborted() exits happen after cookies.txt was rewritten, so a call placed after "+
				"the error check is skipped on exactly the passes whose write nobody compared: the Twitch "+
				"auth mark taken under the old pair stands for up to thirty minutes over a file that no "+
				"longer has that problem, and no live chat session is told to reconnect",
				s.file, s.line, s.gesture)
		}
	}
	if hooks != 1 {
		t.Errorf("%d call sites are shaped as the OnPassCompleted hook, want exactly 1 (%q). The periodic "+
			"timer and the boot profile seed have no caller outside internal/cookies, so they are the one "+
			"site with no defer to sit in — and the hook must stay wrapped in postRefreshRecheckHook, "+
			"whose recover is the only thing between a panic there and the process",
			hooks, "an automatic cookie refresh")
	}
}

// recheckSitesIn parses one file of package main and reports every call to
// recheckAfterCookieWrite in it with the enclosing shapes.
//
// A parent STACK rather than nested ast.Inspect calls: "is this inside a defer"
// is a question about ancestry at any depth, and the enclosing func literal may
// be several nodes up.
func recheckSitesIn(t *testing.T, filename string) []recheckSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	var stack []ast.Node
	var found []recheckSite

	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "recheckAfterCookieWrite" {
			return true
		}

		site := recheckSite{file: filename, line: fset.Position(call.Pos()).Line, gesture: gestureArg(call)}
		// stack[len(stack)-1] is the call itself; walk outward.
		for i := len(stack) - 2; i >= 0; i-- {
			if _, isDefer := stack[i].(*ast.DeferStmt); isDefer {
				site.deferred = true
			}
			if outer, isCall := stack[i].(*ast.CallExpr); isCall && isPassCompletedHook(stack, i, outer) {
				site.inPassCompletedHook = true
			}
		}
		found = append(found, site)
		return true
	})
	return found
}

// gestureArg returns the 4th argument's string literal —
// recheckAfterCookieWrite(ctx, checkNow, log, gesture, args...) — or "" if it is
// not a literal, which is itself a finding: the gesture names the site in the
// log and in this test's table.
func gestureArg(call *ast.CallExpr) string {
	if len(call.Args) < 4 {
		return ""
	}
	lit, ok := call.Args[3].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING || len(lit.Value) < 2 {
		return ""
	}
	return lit.Value[1 : len(lit.Value)-1]
}

// isPassCompletedHook reports whether `outer` is the postRefreshRecheckHook
// call AND its result is assigned to a selector named OnPassCompleted.
//
// Both halves are required: the wrapper's name alone would accept a hook wired
// to something else, and the assignment alone would accept a bare closure with
// no recover around it — the mutant that matters, since a panic on the
// auto-cookie goroutine has nothing else between it and the process.
func isPassCompletedHook(stack []ast.Node, i int, outer *ast.CallExpr) bool {
	fn, ok := outer.Fun.(*ast.Ident)
	if !ok || fn.Name != "postRefreshRecheckHook" {
		return false
	}
	for j := i - 1; j >= 0; j-- {
		assign, ok := stack[j].(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range assign.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "OnPassCompleted" {
				return true
			}
		}
	}
	return false
}

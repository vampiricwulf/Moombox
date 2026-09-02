package cookies

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Arc 10 Task 7a. The two credential-writing paths whose caller lives INSIDE
// this package — the periodic timer and the boot profile seed — are the two
// that need an injected seam rather than a call at the caller.

// TestNotePassCompletedFiresTheHook.
//
// The mutation: notePassCompleted not invoking the hook — an inverted nil
// guard, or a body that falls through. Firing once per PROCESS rather than
// once per pass is the second, which is why two calls are made and counted.
//
// What this does NOT catch, and structurally cannot: a tick or a seed failing
// to call notePassCompleted at all. Those branches need a browser profile, a
// browser and a network — the reason the seam is a named method in the first
// place — so they are pinned structurally instead, by
// TestNotePassCompletedHasExactlyItsTwoWritingCallers below.
func TestNotePassCompletedFiresTheHook(t *testing.T) {
	calls := 0
	s := &AutoCookieService{OnPassCompleted: func() { calls++ }}

	s.notePassCompleted()
	s.notePassCompleted()

	if calls != 2 {
		t.Errorf("the hook fired %d times for two completed passes, want 2", calls)
	}
}

// TestNotePassCompletedWithNoHookIsSafe. The hook is injected by cmd/moombox;
// every test in this package, and any embedding that does not wire it, has
// none.
//
// The mutation: dropping the nil guard — a panic inside the periodic
// goroutine, which is recovered but kills that timer for the life of the
// process.
func TestNotePassCompletedWithNoHookIsSafe(t *testing.T) {
	s := &AutoCookieService{}
	s.notePassCompleted() // must not panic
}

// TestNotePassCompletedHasExactlyItsTwoWritingCallers is the structural half
// the two behavioural tests above cannot reach.
//
// Both callers sit inside goroutines that need a browser profile, a browser and
// a network before they get anywhere near the seam, so "the tick forgot to call
// it" and "the boot seed forgot to call it" are invisible to any test that runs
// offline. Naming the call sites is what makes them visible in a diff instead.
//
// The two, and why each is here rather than calling CheckNow itself:
//
//	StartPeriodicRefresh  the 30-minute browser refresh
//	StartProfileSeed      the one-shot import at boot, ~15 s in
//
// They are the only two credential-writing paths whose caller lives INSIDE this
// package. Everything else — the recovery, the worker's auth-failure refresh,
// R F, both setup wizards — has a caller in cmd/moombox or internal/web/routes
// that runs the re-check itself, and firing this seam from
// refreshCookiesDetailed instead would double every one of those.
//
// The mutation: dropping s.notePassCompleted() from either goroutine.
//
// PAIRED WITH TestNotePassCompletedIsGatedOnRanAtEverySite — this half proves
// the call exists, that one proves it is still gated. Neither covers the other.
func TestNotePassCompletedHasExactlyItsTwoWritingCallers(t *testing.T) {
	want := []string{"StartPeriodicRefresh", "StartProfileSeed"} // sorted
	got := callersOf(t, "notePassCompleted")

	if len(got) == 0 {
		t.Fatal("nothing calls notePassCompleted. Both automatic credential writers are then invisible " +
			"to the in-process auth check: a repaired cookie file waits for the 30-minute ticker, and " +
			"the Twitch auth mark taken under the old pair stands over a file that no longer has that " +
			"problem")
	}
	if !slices.Equal(got, want) {
		t.Errorf("notePassCompleted callers = %v, want %v.\n"+
			"A MISSING one is a credential write that reaches no fingerprint comparison. A NEW one is "+
			"a real decision: this seam exists only for writers with no caller outside this package, "+
			"and adding it to refreshCookiesDetailed would double every external site", got, want)
	}
}

// TestNotePassCompletedIsGatedOnRanAtEverySite pins the gate itself — the
// assertion the Arc 10 reviewer's mutation walked straight through at the
// recovery site, because deleting an `if result.Ran` leaves every behavioural
// test green: the guarded call still happens.
//
// The gate is not decoration. Seven refreshDeclined() exits reach these two
// tails having written nothing at all — setup in progress, a refresh already in
// flight, no browser, no profile, the service stopped — and firing the seam on
// those spends a full in-process re-check, two validate round-trips, on a file
// nobody touched, then logs a staleness warning that describes nothing.
//
// The mutations: `if result.Ran` → `if true` at either site; hoisting the call
// out of the if entirely; and — the one the first version of this test let
// through — moving it into the `else`, which inverts the invariant exactly and
// still reports an enclosing `if` whose condition is `result.Ran`.
func TestNotePassCompletedIsGatedOnRanAtEverySite(t *testing.T) {
	sites := gatedCallsOf(t, "notePassCompleted")
	if len(sites) == 0 {
		t.Fatal("no call to notePassCompleted was found at all")
	}
	for site, cond := range sites {
		if cond != "Ran" {
			t.Errorf("%s calls notePassCompleted %s, want it inside `if result.Ran {`. A pass that "+
				"declined wrote nothing, so there is nothing to re-read — and a pass that RAN is the "+
				"only one whose write has to reach the fingerprint comparison", site, cond)
		}
	}
}

// The sentinels gatedCallsOf reports in place of a condition name. Each is a
// distinct failure so the message names the fix instead of leaving a reader to
// guess: a call with no gate, a gate that is not a field test, and a call under
// the WRONG BRANCH of a correct-looking gate are three different mistakes that
// all used to read as `under ""`.
const (
	gateNone         = "with no enclosing if at all"
	gateUnrecognised = "under an if whose condition is not a bare field selector (e.g. `if true`, or a compound condition)"
	gateElse         = "in the ELSE branch of its if — the invariant inverted: it fires on exactly the passes that wrote nothing"
)

// gatedCallsOf maps each call site of `callee` — keyed by enclosing function
// and source position, so two calls in one function cannot mask each other — to
// the field name its nearest enclosing `if` tests, or to one of the sentinels
// above.
//
// Parsed rather than grepped for the reason callersOf is: the claim is about
// the STRUCTURE around a call, which no substring search can express.
//
// WHAT IS RECOGNISED as a gate: the call sits inside the BODY of an `if` whose
// condition is a bare field selector, and the reported name is that field.
// `if result.Ran { s.notePassCompleted() }` reports "Ran".
//
// WHAT IS NOT, each with its own sentinel: no enclosing `if`; a condition that
// is not a bare selector, which covers `if true`, `if !result.Ran`, and
// `if result.Ran && x`; and a call reached through the `else` rather than the
// body. That last one is why the branch check exists — the first version of
// this helper walked to the nearest IfStmt and read its condition without
// asking WHICH SIDE the call was on, so `if result.Ran { … } else { call }`,
// the precise inversion of the invariant, reported "Ran" and passed.
//
// `else if` behaves correctly by construction: the nearest enclosing IfStmt of
// a call in an `else if` body is that inner `if`, and the check compares
// against the inner one's Body.
func gatedCallsOf(t *testing.T, callee string) map[string]string {
	t.Helper()

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, name := range names {
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
			// Balanced because every non-nil node returns true, so ast.Inspect
			// always follows it with the nil that pops it again.
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return false
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != callee {
					return true
				}
				site := fmt.Sprintf("%s (%s)", fn.Name.Name, fset.Position(call.Pos()))
				out[site] = gateOf(stack)
				return true
			})
		}
	}
	return out
}

// gateOf reads the gate around the call at the top of `stack`, which holds
// every ancestor node from the enclosing function body down to the call.
func gateOf(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		ifStmt, ok := stack[i].(*ast.IfStmt)
		if !ok {
			continue
		}
		// WHICH SIDE. stack[i+1] is the ancestor one level inside the if, so it
		// is the if's Body block for a call in the body and its Else for one in
		// the else. Comparing the pointers is what makes the inverted gate
		// visible; reading Cond alone cannot see it.
		if i+1 >= len(stack) || stack[i+1] != ast.Node(ifStmt.Body) {
			return gateElse
		}
		condSel, ok := ifStmt.Cond.(*ast.SelectorExpr)
		if !ok {
			return gateUnrecognised
		}
		return condSel.Sel.Name
	}
	return gateNone
}

// TestSetupResultWroteReportsWhetherTheFileWasReplaced pins the setup path's
// counterpart to RefreshResult.Ran.
//
// It is what the two wizard-finish callers gate their auth re-check on, and the
// reason they cannot gate on `err == nil`: the jar reload after a SUCCESSFUL
// write can fail, and that exit returns an error over a cookies.txt that has
// already been replaced.
//
// The mutations: dropping `Wrote: true` from the success return — the first row
// fails, and both wizards stop re-checking after the most deliberate credential
// change there is; or setting it unconditionally at the top of
// FinishSetupDetailed — the other two rows fail, and a stale double-click buys a
// wasted validate pass.
func TestSetupResultWroteReportsWhetherTheFileWasReplaced(t *testing.T) {
	t.Run("a completed setup replaced the file", func(t *testing.T) {
		s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }

		result, err := s.FinishSetupDetailed(context.Background())
		if err != nil {
			t.Fatalf("FinishSetupDetailed: %v", err)
		}
		if !result.Wrote {
			t.Error("a successful finish reports Wrote=false — the wizard's re-check is skipped and " +
				"the credential fingerprint is not compared until the next tick")
		}
	})

	t.Run("no setup in progress wrote nothing", func(t *testing.T) {
		s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
		s.setupProcess = nil

		result, err := s.FinishSetupDetailed(context.Background())
		if !errors.Is(err, ErrNoSetupInProgress) {
			t.Fatalf("want ErrNoSetupInProgress, got %v", err)
		}
		if result.Wrote {
			t.Error("a finish that never started reports Wrote=true")
		}
	})

	t.Run("the S9 abort wrote nothing", func(t *testing.T) {
		s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
		if err := os.WriteFile(s.cookiePath, []byte(previousCookieFile), 0o600); err != nil {
			t.Fatal(err)
		}
		failCookieRead(t, errors.New("permission denied (simulated)"))

		result, err := s.FinishSetupDetailed(context.Background())
		if !errors.Is(err, ErrCookieFileUnreadable) {
			t.Fatalf("want ErrCookieFileUnreadable, got %v", err)
		}
		if result.Wrote {
			t.Error("the abort that deliberately refuses to overwrite cookies.txt reports Wrote=true — " +
				"the one error path that must NOT claim a write is the one whose whole purpose is not " +
				"having made one")
		}
	})
}

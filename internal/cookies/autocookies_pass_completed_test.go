package cookies

import (
	"context"
	"errors"
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
// The mutation: `if result.Ran` → `if true` at either site, or hoisting the
// call out of the if entirely.
func TestNotePassCompletedIsGatedOnRanAtEverySite(t *testing.T) {
	sites := gatedCallsOf(t, "notePassCompleted")
	if len(sites) == 0 {
		t.Fatal("no call to notePassCompleted was found at all")
	}
	for fn, cond := range sites {
		if cond != "Ran" {
			t.Errorf("%s calls notePassCompleted under %q, want it gated on the pass result's Ran "+
				"field. A pass that declined wrote nothing, so there is nothing to re-read", fn, cond)
		}
	}
}

// gatedCallsOf maps each function that calls `callee` to the field name its
// nearest enclosing `if` tests, or "" when the call is not inside one, or when
// the condition is anything other than a bare field selector.
//
// Parsed rather than grepped for the reason callersOf is: the claim is about
// the STRUCTURE around a call, which no substring search can express. Only a
// bare selector condition is recognised — `if result.Ran {` — so `if true`, an
// inverted gate, a hoisted call and a gate on some other field all come back as
// something that is not "Ran".
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
				cond := ""
				for i := len(stack) - 1; i >= 0; i-- {
					ifStmt, ok := stack[i].(*ast.IfStmt)
					if !ok {
						continue
					}
					if condSel, ok := ifStmt.Cond.(*ast.SelectorExpr); ok {
						cond = condSel.Sel.Name
					}
					break
				}
				out[fn.Name.Name] = cond
				return true
			})
		}
	}
	return out
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

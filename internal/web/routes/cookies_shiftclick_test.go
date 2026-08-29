package routes

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// jsBlock slices one brace-delimited region out of a script, starting at an
// exact anchor and ending at the brace that closes the first `{` after it.
//
// Bracketing is necessary here for the same reason cookies_setup_threestate_test
// gives: app.js is 4,900 lines and a file-wide Contains would be satisfied by
// any of a dozen sibling handlers. The scanner tracks strings and comments so a
// brace inside either cannot end the region early.
func jsBlock(t *testing.T, src, anchor string) string {
	t.Helper()
	at := strings.Index(src, anchor)
	if at < 0 {
		t.Fatalf("app.js no longer contains %q — if that site was renamed, re-anchor this test on it "+
			"rather than deleting the assertion", anchor)
	}
	rest := src[at:]
	depth := 0
	started := false
	var quote byte
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '/' && i+1 < len(rest) && rest[i+1] == '/':
			if nl := strings.IndexByte(rest[i:], '\n'); nl < 0 {
				return rest
			} else {
				i += nl
			}
		case c == '/' && i+1 < len(rest) && rest[i+1] == '*':
			end := strings.Index(rest[i+2:], "*/")
			if end < 0 {
				return rest
			}
			i += 2 + end + 1
		case c == '{':
			depth++
			started = true
		case c == '}':
			depth--
			if started && depth == 0 {
				return rest[:i+1]
			}
		}
	}
	t.Fatalf("the block anchored at %q is unterminated", anchor)
	return ""
}

// jsCode returns a region with its comments removed.
//
// Required for the NEGATIVE assertions below, and not a nicety: the sites under
// test carry long comments that explain the very gate they must not contain, so
// a raw Contains for "auto_enabled" would fail on correct code and pass on
// nothing useful. Same scanner as above.
func jsCode(region string) string {
	var out strings.Builder
	var quote byte
	for i := 0; i < len(region); i++ {
		c := region[i]
		if quote != 0 {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(region) {
				i++
				out.WriteByte(region[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '"' || c == '\'' || c == '`':
			quote = c
			out.WriteByte(c)
		case c == '/' && i+1 < len(region) && region[i+1] == '/':
			nl := strings.IndexByte(region[i:], '\n')
			if nl < 0 {
				return out.String()
			}
			i += nl - 1
		case c == '/' && i+1 < len(region) && region[i+1] == '*':
			end := strings.Index(region[i+2:], "*/")
			if end < 0 {
				return out.String()
			}
			i += 2 + end + 1
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// TestShiftClickRefreshIsNotGatedOnAutoEnabled is the Web half of the same
// finding the TUI's R F carried, on the same flag, one surface over.
//
// The handler used to read `e.shiftKey && this.config?.cookies?.auto_enabled`,
// so on a disabled install shift+click silently fell through to the plain
// recheck: the gesture existed, quietly did something else, and said nothing
// about it. That is precisely what the previous arc removed from the Web
// re-login indicator and what this arc removed from the R F chord.
//
// The positive rows are the junction guard. "auto_enabled is absent" alone
// would be satisfied by a handler that had lost the shift branch altogether, or
// by one that no longer existed.
func TestShiftClickRefreshIsNotGatedOnAutoEnabled(t *testing.T) {
	block := jsBlock(t, readEmbeddedModule(t, "public/app.js"),
		`const refreshBtn = document.getElementById("btn-refresh-cookies");`)
	code := jsCode(block)

	if strings.Contains(code, "auto_enabled") {
		t.Errorf("the cookie refresh button reads cookies.auto_enabled again. On a disabled install "+
			"that makes shift+click fall through to the plain recheck without saying so — the same "+
			"hiding this arc removed from the TUI's R F chord: %s", code)
	}
	for _, want := range []string{"shiftKey", "autoCookieRefresh(", "recheckCookies("} {
		if !strings.Contains(code, want) {
			t.Errorf("the cookie refresh button no longer references %q, so the assertion above "+
				"pins nothing: %s", want, code)
		}
	}
}

// TestAutoCookieRefreshFallsBackToTheRecheck is the Web twin of R F's bottom
// rung (internal/tui's TestForceRefreshFallsBackToTheRecheck).
//
// Shift+click asks for the strongest refresh available. With no browser profile
// to work from, the server answers 404 (ErrProfileNotFound) or 424
// (ErrNoBrowserFound) — both raised from the same missing-directory check — and
// the operator still has the in-process Go refresh a plain click runs. Ending
// on "failed" there would be a dead end in front of a working remedy.
//
// Both statuses are required. Handling one is the shape of this defect: the two
// arrive from the same branch and differ only in whether a browser could have
// been launched, which is not a distinction the operator can act on when the
// profile is absent either way.
func TestAutoCookieRefreshFallsBackToTheRecheck(t *testing.T) {
	block := jsBlock(t, readEmbeddedModule(t, "public/app.js"), "async autoCookieRefresh() {")
	code := jsCode(block)

	for _, want := range []string{"404", "424"} {
		if !strings.Contains(code, want) {
			t.Errorf("autoCookieRefresh does not branch on HTTP %s. That status means there is no "+
				"browser profile to work from, and the in-process refresh is still available — "+
				"reporting a failure instead dead-ends the operator", want)
		}
	}
	if !strings.Contains(code, "recheckCookies(") {
		t.Error("autoCookieRefresh never calls recheckCookies, so the no-profile case has no fallback " +
			"and the two surfaces disagree: the TUI's R F runs the in-process refresh in exactly " +
			"this situation")
	}
	// The sentence, at THIS site. internal/tui's TestRungThreeSentencesDivergeByDesign
	// pins its exact wording and its deliberate difference from the TUI's; what
	// that test cannot see is where in app.js it lives, and a rung-3 message
	// sitting in some other handler would satisfy it just as well.
	const rungThree = "No browser profile found, running a normal cookie refresh instead..."
	if !strings.Contains(code, rungThree) {
		t.Errorf("autoCookieRefresh does not render %q. That is the owner's wording for this surface — "+
			"it names a normal cookie refresh rather than the TUI's R C chord, because a dashboard "+
			"user has no chord to press", rungThree)
	}
	// The premise: without a failure arm left, "it falls back" would be
	// trivially true and would say nothing about which failures it covers.
	if !strings.Contains(code, "Browser cookie refresh failed") {
		t.Error("autoCookieRefresh no longer reports any failure at all, so the fallback assertions " +
			"above cannot distinguish a targeted fallback from one that swallows everything")
	}
}

// TestNoProfileRefreshAnswersAStatusTheDashboardFallsBackOn closes the Go/JS
// seam the two tests above sit either side of.
//
// app.js decides whether to fall back by reading the HTTP STATUS, deliberately
// — the alternative is matching the route's prose, which breaks the moment
// either sentence is reworded. That makes the status code a contract, and
// nothing was holding it: changing the route's mapping for ErrProfileNotFound
// left every other assertion green while shift+click went back to dead-ending
// on "failed". A mutation run is how that was found.
//
// So the accepted set is READ OUT OF THE SHIPPED SCRIPT rather than written
// here twice. Change either side alone and this fails.
//
// Which of the two the server picks is host-dependent — it turns on whether
// DetectBrowser finds a browser on the machine running the test — which is
// exactly why the assertion is membership in the set rather than one value.
func TestNoProfileRefreshAnswersAStatusTheDashboardFallsBackOn(t *testing.T) {
	fallbackStatuses := map[string]bool{}
	code := jsCode(jsBlock(t, readEmbeddedModule(t, "public/app.js"), "async autoCookieRefresh() {"))
	for _, m := range regexp.MustCompile(`response\.status === (\d{3})`).FindAllStringSubmatch(code, -1) {
		fallbackStatuses[m[1]] = true
	}
	if len(fallbackStatuses) == 0 {
		t.Fatal("autoCookieRefresh branches on no HTTP status at all, so there is no fallback contract " +
			"left to hold the route to")
	}

	svc := cookies.NewAutoCookieService(
		filepath.Join(t.TempDir(), "no-such-profile"),
		filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})

	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil))

	if !fallbackStatuses[strconv.Itoa(rec.Code)] {
		t.Errorf("a refresh with no browser profile answered %d, and the dashboard only falls back on "+
			"%v — shift+click now dead-ends on \"failed\" for an operator who still has the "+
			"in-process refresh available. Body: %s",
			rec.Code, sortedKeys(fallbackStatuses), rec.Body.String())
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// httpStatusCodes maps the status constants the auto-refresh handler uses onto
// their numbers. Deliberately a closed table: an unmapped name fails loudly
// below rather than being skipped, so a status this test has never seen cannot
// slip through as "not asserted".
var httpStatusCodes = map[string]int{
	"StatusNotFound":            http.StatusNotFound,
	"StatusFailedDependency":    http.StatusFailedDependency,
	"StatusConflict":            http.StatusConflict,
	"StatusUnprocessableEntity": http.StatusUnprocessableEntity,
	"StatusInternalServerError": http.StatusInternalServerError,
	"StatusServiceUnavailable":  http.StatusServiceUnavailable,
}

// TestRungThreeAgreesAcrossBothSurfaces is the cross-surface pin for the manual
// refresh ladder's bottom rung.
//
// The TUI's R F and the dashboard's shift+click are ONE gesture — refresh by the
// strongest means available — so a failure that falls back to the in-process
// refresh on one surface must fall back on the other. They cannot share code:
// the TUI branches on a Go sentinel and the dashboard on an HTTP status, with
// this route's mapping in between. They had already drifted on
// ErrProfileNotADirectory, which the route grouped with ErrProfileNotFound
// under a 404 the dashboard falls back on, while the TUI's predicate excluded
// it — one state, two behaviours, both surfaces green.
//
// So the agreement is asserted over EVERY sentinel the handler branches on, in
// both directions, with all three parties read from source: the predicate from
// internal/cookies, the mapping from this file's AST, the fallback statuses from
// the shipped app.js. No value is written down twice, so changing any one of
// the three alone fails here.
func TestRungThreeAgreesAcrossBothSurfaces(t *testing.T) {
	fallback := map[int]bool{}
	js := jsCode(jsBlock(t, readEmbeddedModule(t, "public/app.js"), "async autoCookieRefresh() {"))
	for _, m := range regexp.MustCompile(`response\.status === (\d{3})`).FindAllStringSubmatch(js, -1) {
		n, _ := strconv.Atoi(m[1])
		fallback[n] = true
	}
	if len(fallback) == 0 {
		t.Fatal("autoCookieRefresh branches on no HTTP status, so the dashboard has no bottom rung " +
			"and there is nothing for the TUI to agree with")
	}

	// The sentinels a refresh pass can produce, by name, with the value the
	// predicate is asked about. Listed rather than reflected so a new sentinel
	// added to the handler shows up as an unmapped name below.
	sentinels := map[string]error{
		"ErrNoBrowserFound":       cookies.ErrNoBrowserFound,
		"ErrProfileNotFound":      cookies.ErrProfileNotFound,
		"ErrProfileDirUnreadable": cookies.ErrProfileDirUnreadable,
		"ErrProfileNotADirectory": cookies.ErrProfileNotADirectory,
		"ErrCookieDBNotFound":     cookies.ErrCookieDBNotFound,
		"ErrCookieDBLocked":       cookies.ErrCookieDBLocked,
		"ErrCookieDBUnreadable":   cookies.ErrCookieDBUnreadable,
		"ErrNoCookiesInProfile":   cookies.ErrNoCookiesInProfile,
		"ErrCookieFileUnreadable": cookies.ErrCookieFileUnreadable,
	}

	for name, statuses := range autoRefreshStatusBySentinel(t) {
		sentinel, known := sentinels[name]
		if !known {
			t.Errorf("the auto-refresh handler branches on cookies.%s, which this test does not know "+
				"about — add it above and confirm cookies.IsNoBrowserProfile and the dashboard's "+
				"fallback agree on it", name)
			continue
		}
		// EVERY status the sentinel can be answered with, not just one. A
		// sentinel listed in two cases is the shape the drift took: it kept the
		// arm that agreed while gaining one that did not, and reading a single
		// status let whichever case was walked last hide the other.
		tuiFallsBack := cookies.IsNoBrowserProfile(sentinel)
		for _, status := range statuses {
			checkAgreement(t, name, status, fallback, tuiFallsBack)
		}
	}
}

// checkAgreement compares one sentinel-to-status mapping against the predicate.
func checkAgreement(t *testing.T, name string, status int, fallback map[int]bool, tuiFallsBack bool) {
	t.Helper()
	webFallsBack := fallback[status]
	switch {
	case webFallsBack == tuiFallsBack:
	case webFallsBack:
		t.Errorf("%s answers %d, which the dashboard treats as the ladder's bottom rung and falls "+
			"back to the in-process refresh on — but cookies.IsNoBrowserProfile rejects it, so the "+
			"TUI's R F reports a failure for the same state. One gesture, two behaviours",
			name, status)
	default:
		t.Errorf("cookies.IsNoBrowserProfile accepts %s, so the TUI's R F falls back to the "+
			"in-process refresh — but the route answers %d, which is not one of the statuses the "+
			"dashboard falls back on (%v). One gesture, two behaviours",
			name, status, sortedKeys(fallbackStrings(fallback)))
	}
}

// autoRefreshStatusBySentinel reads the POST /api/cookies/auto-refresh error
// switch and returns EVERY HTTP status each cookies sentinel is answered with.
//
// A slice, not a single value, because a sentinel can appear in more than one
// case clause — and when it does, one of them is wrong. Keeping only the last
// one seen let exactly that mistake pass.
//
// Bracketed to that one handler: cookies.go has a second ErrNoBrowserFound case
// in auto-setup/start, whose status is a different question entirely.
func autoRefreshStatusBySentinel(t *testing.T) map[string][]int {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cookies.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cookies.go: %v", err)
	}

	var handler ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"/api/cookies/auto-refresh"` {
			handler = call.Args[1]
		}
		return true
	})
	if handler == nil {
		t.Fatal("cookies.go no longer registers POST /api/cookies/auto-refresh with an inline handler")
	}

	out := map[string][]int{}
	ast.Inspect(handler, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		var names []string
		for _, cond := range clause.List {
			ast.Inspect(cond, func(c ast.Node) bool {
				sel, ok := c.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "cookies" {
					names = append(names, sel.Sel.Name)
				}
				return true
			})
		}
		if len(names) == 0 {
			return true
		}
		status := 0
		for _, stmt := range clause.Body {
			ast.Inspect(stmt, func(c ast.Node) bool {
				sel, ok := c.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
					return true
				}
				code, known := httpStatusCodes[sel.Sel.Name]
				if !known {
					t.Errorf("the auto-refresh handler answers http.%s, whose number this test does "+
						"not know — add it to httpStatusCodes", sel.Sel.Name)
					return true
				}
				status = code
				return true
			})
		}
		if status == 0 {
			t.Errorf("the case for %v answers no HTTP status this test can read", names)
			return true
		}
		for _, name := range names {
			out[name] = append(out[name], status)
		}
		return true
	})

	if len(out) == 0 {
		t.Fatal("no sentinel cases were found in the auto-refresh handler — the switch was restructured " +
			"and this test is reading nothing")
	}
	return out
}

func fallbackStrings(codes map[int]bool) map[string]bool {
	out := map[string]bool{}
	for c := range codes {
		out[strconv.Itoa(c)] = true
	}
	return out
}

// TestNoBrowserFoundCopyHoldsForBothStatesItCoversNow is the 424's own version
// of this plan's most expensive recurring defect: a sentence that outlived the
// condition it was written for.
//
// The handler used to answer this sentinel with the static "no supported
// browser installed". That was true while the only way to reach it was a host
// with no browser. It no longer is: cookies.auto_enabled can switch headless
// runs off on a machine where Firefox is sitting right there, and the pass then
// arrives here with a browser installed and unused. Telling that operator to
// install one sends them at the wrong problem entirely.
//
// So the route renders the service's own message, and the service writes a
// different one per state. Asserted by driving a real refresh both ways through
// the wire — the response body, not the Go error — because the body is what the
// operator reads and substituting a static string is precisely the regression.
func TestNoBrowserFoundCopyHoldsForBothStatesItCoversNow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		allowed      bool
		said, unsaid []string
	}{
		{
			// Reachable on any ordinary desktop: the flag is off by default and
			// a browser is installed.
			name: "a browser is installed and the flag is off", allowed: false,
			said: []string{"auto_enabled"},
			// The instruction this operator must never be given.
			unsaid: []string{"install"},
		},
		{
			name: "no browser is installed", allowed: true,
			said:   []string{"no supported browser found"},
			unsaid: []string{"auto_enabled"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := cookies.NewAutoCookieService(
				filepath.Join(t.TempDir(), "no-such-profile"),
				filepath.Join(t.TempDir(), "cookies.txt"),
				cookies.NewCookieJar(), nopRouteLogger{})
			svc.BrowserLaunchAllowed = func() bool { return tc.allowed }

			r := chi.NewRouter()
			CookieRoutes(r, nil, svc, nil, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil))

			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("status %d with a body that is not the handler's JSON error: %s",
					rec.Code, rec.Body.String())
			}
			if body.Error == "" {
				t.Fatalf("status %d carried no message at all, so the operator is told nothing", rec.Code)
			}

			// The allowed row only reaches ErrNoBrowserFound on a host with no
			// browser installed. Skipping is honest here — asserting the other
			// state's copy would be asserting the wrong thing — and the gated
			// row above is host-independent, so the split is always exercised.
			if tc.allowed && rec.Code != http.StatusFailedDependency {
				t.Skipf("this host has a browser installed, so the no-browser state is not "+
					"reachable here (status %d: %s)", rec.Code, body.Error)
			}

			lower := strings.ToLower(body.Error)
			for _, want := range tc.said {
				if !strings.Contains(lower, want) {
					t.Errorf("the response does not say %q: %q", want, body.Error)
				}
			}
			for _, unwanted := range tc.unsaid {
				if strings.Contains(lower, unwanted) {
					t.Errorf("the response says %q, which is not true of this state: %q", unwanted, body.Error)
				}
			}
		})
	}
}

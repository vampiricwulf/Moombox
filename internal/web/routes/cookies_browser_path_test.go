package routes

import (
	"strings"
	"testing"
)

// S14: a validator that could not answer was being rendered as a verdict.
//
// saveConfig's custom-browser branch did `await resp.json()` with no `ok` check
// and then `if (!validateResult.valid)`. On a 429 off the heavy rate limiter, a
// 400, or a proxy's HTML error page, the parsed body has no `valid` key — so the
// message read "Invalid browser: undefined", the save RETURNED, and every
// unrelated setting the user had edited in the same form was discarded. The
// server saying "I could not check" is not the server saying "your path is
// wrong", and the difference is the whole arc.
//
// The decision now lives in browserPathValidationOutcome, a pure function, so
// the rows below are EXECUTED rather than matched. saveConfig itself is
// DOM-coupled and cannot be loaded, so its half is a bracketed source
// assertion — written as a count of the exits, which is the one shape a
// re-added abort cannot slip past.

// TestBrowserPathValidationBlocksOnlyAConclusiveRejection runs the helper.
//
// The invariant at the bottom is the assertion that matters: across every row,
// the save is blocked if and only if the server ran the check and returned
// `valid: false`. A row added later cannot start blocking on a non-answer, and
// — the direction the user pays for — a genuine rejection cannot quietly stop
// blocking.
func TestBrowserPathValidationBlocksOnlyAConclusiveRejection(t *testing.T) {
	vm := utilsVM(t)

	// `arg` is `any`, not a map, so the last row can pass a genuine absent
	// argument: jsCall turns a nil interface into JS `undefined`, which is what
	// a call site that forgot the object actually produces. A typed nil map
	// would arrive as `null`, and `{...} = {}` destructuring throws on null —
	// a different failure from the one that row is about.
	for _, tc := range []struct {
		name        string
		arg         any
		wantBlock   bool
		wantVariant string
		wantSaid    []string
		wantUnsaid  []string
	}{
		{
			name:      "the server validated the path",
			arg:       map[string]any{"reached": true, "body": map[string]any{"valid": true}},
			wantBlock: false, wantVariant: "success", wantSaid: []string{"validated"},
			wantUnsaid: []string{"could not check"},
		},
		{
			// The only row that earns the abort. The server ran the check and
			// said no, and it said why.
			name: "the server rejected the path",
			arg: map[string]any{"reached": true, "body": map[string]any{
				"valid": false, "error": "not an executable file",
			}},
			wantBlock: true, wantVariant: "danger",
			wantSaid:   []string{"invalid browser", "not an executable file"},
			wantUnsaid: []string{"saving anyway"},
		},
		{
			// THE FIX. Byte-identical user intent to the row above, and the
			// server told us nothing at all about the path.
			name:      "the rate limiter refused the check",
			arg:       map[string]any{"reached": false, "detail": "HTTP 429"},
			wantBlock: false, wantVariant: "warning",
			wantSaid:   []string{"could not check", "HTTP 429", "saving anyway"},
			wantUnsaid: []string{"invalid browser", "undefined"},
		},
		{
			name:      "the server could not be reached at all",
			arg:       map[string]any{"reached": false, "detail": "Failed to fetch"},
			wantBlock: false, wantVariant: "warning",
			wantSaid:   []string{"could not check", "Failed to fetch"},
			wantUnsaid: []string{"invalid browser", "undefined"},
		},
		{
			// A 200 whose body carries no verdict — a proxy, or a build that
			// does not have this endpoint. `!body.valid` folded this into the
			// blocking arm, which is precisely the conflation being removed;
			// it must not be reintroduced as a shorthand.
			name:      "a 200 with no verdict in it",
			arg:       map[string]any{"reached": true, "body": map[string]any{}},
			wantBlock: false, wantVariant: "warning",
			wantSaid:   []string{"could not check"},
			wantUnsaid: []string{"invalid browser", "undefined"},
		},
		{
			// The block still happens when the server declines to say why —
			// but it must not say the word "undefined" at the user.
			name:      "rejected with no reason given",
			arg:       map[string]any{"reached": true, "body": map[string]any{"valid": false}},
			wantBlock: true, wantVariant: "danger",
			wantSaid:   []string{"invalid browser"},
			wantUnsaid: []string{"undefined"},
		},
		{
			// Called with nothing at all, which is what a future call site that
			// forgets an argument does. "We do not know" is the safe default;
			// "your path is invalid" is not.
			name: "called with no argument", arg: nil,
			wantBlock: false, wantVariant: "warning",
			wantUnsaid: []string{"invalid browser", "undefined"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := jsCall(t, vm, "browserPathValidationOutcome", tc.arg)
			m, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("browserPathValidationOutcome returned %T, want the outcome object", raw)
			}
			message, _ := m["message"].(string)
			variant, _ := m["variant"].(string)
			block, _ := m["block"].(bool)
			if message == "" || variant == "" {
				t.Fatalf("outcome is missing message or variant: %v", m)
			}

			if block != tc.wantBlock {
				t.Errorf("block = %v, want %v: %q", block, tc.wantBlock, message)
			}
			if variant != tc.wantVariant {
				t.Errorf("variant = %q, want %q: %q", variant, tc.wantVariant, message)
			}
			assertCopy(t, message, tc.wantSaid, tc.wantUnsaid)

			// THE INVARIANT, on every row. Blocking is reserved for the one
			// answer that earns it: the server ran the check and said no.
			opts, _ := tc.arg.(map[string]any)
			body, _ := opts["body"].(map[string]any)
			reached, _ := opts["reached"].(bool)
			wantBlock := reached && body != nil && body["valid"] == false
			if block != wantBlock {
				t.Errorf("block = %v, want %v — only an explicit `valid: false` from a reached "+
					"server may cost the user the rest of their save: %q", block, wantBlock, message)
			}
			// No arm may ever render a missing field at the user. That string
			// is what the original defect looked like on screen.
			if strings.Contains(message, "undefined") {
				t.Errorf("the outcome renders a missing field verbatim: %q", message)
			}
		})
	}
}

// TestSaveConfigHasOneExitFromBrowserValidation is the call-site half.
//
// A presence check ("saveConfig calls the helper") cannot see the bug: the
// helper can be called and its verdict ignored, and the old `return` left in
// place beside it. Two counts carry this test, and it is worth being exact
// about which one carries what, because a future editor trimming "redundant"
// assertions will keep whichever the comment credits:
//
//   - The EXIT count catches an abort ADDED beside the block — a second
//     `return;` on the warning path, which is the shape the original defect had.
//   - The CALL count is what catches an abort that keeps one exit. Replacing the
//     non-ok branch with an inline `{ block: true, ... }` literal reintroduces
//     the whole of S14 — a 429 abandons the save — while leaving exactly one
//     `return;`, still guarded by `outcome.block`, and never naming
//     `validateResult`. Three calls means every way the check can end (a
//     verdict, a non-200, a throw) goes through the helper; a branch that
//     decides for itself what a non-answer means is a branch that skipped it.
//
// Bracketed twice over: to saveConfig, and then to the span between the
// validation fetch and the payload assignment it guards. saveConfig is long and
// contains plenty of unrelated returns.
func TestSaveConfigHasOneExitFromBrowserValidation(t *testing.T) {
	body := jsMethodBody(t, readEmbeddedModule(t, "public/modules/settings.js"), "saveConfig")

	const (
		startMark = "/api/auto-cookies/validate-browser-path"
		endMark   = "payload.cookies.browser_path = path;"
	)
	start := strings.Index(body, startMark)
	if start < 0 {
		t.Fatalf("saveConfig no longer validates the custom browser path at all")
	}
	end := strings.Index(body[start:], endMark)
	if end < 0 {
		t.Fatalf("saveConfig no longer writes the validated browser path into the payload — the " +
			"custom-path setting would silently never be saved")
	}
	span := body[start : start+end]

	if n := strings.Count(span, "return;"); n != 1 {
		t.Errorf("the browser-path validation has %d exits, want exactly 1. An abort added beside "+
			"the block is how a 429 — or any answer that is not a verdict — goes back to "+
			"discarding every other setting in the save:\n%s", n, span)
	}
	if !strings.Contains(span, "outcome.block") {
		t.Error("the single exit is no longer guarded by outcome.block — the save can be abandoned " +
			"over something other than a conclusive rejection")
	}
	if strings.Contains(span, "validateResult") {
		t.Error("saveConfig reads the raw validation body again. `!validateResult.valid` is true for " +
			"a 429, a 400 and an HTML error page alike, which is the defect S14 removes — the " +
			"three-way decision belongs in browserPathValidationOutcome")
	}
	if args := jsCallArgs(span, "browserPathValidationOutcome"); len(args) != 1 {
		t.Errorf("browserPathValidationOutcome is called with %d arguments (%v), want 1 options object",
			len(args), args)
	}
	// THE ASSERTION THAT CATCHES A DISGUISED ABORT — see the doc comment. The
	// exit count above does not: an inline `{ block: true, ... }` on the non-ok
	// branch reinstates the whole defect with one exit and no new `return;`.
	// Three calls means the 200, the non-200 and the throw each go through the
	// helper, which is the only way none of them can decide for itself what a
	// non-answer means.
	if n := strings.Count(span, "browserPathValidationOutcome("); n != 3 {
		t.Errorf("the validation has %d calls to browserPathValidationOutcome, want 3 — one per way "+
			"it can end (a verdict, a non-200, a throw). A branch that builds its own outcome is "+
			"a branch that has gone back to treating \"could not check\" as \"invalid\"", n)
	}
}

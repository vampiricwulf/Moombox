package routes

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// This file replaces source-text assertions with EXECUTION.
//
// Three rounds of review found three defects of one shape: an assertion about
// JS written as a string match, which passed while the behaviour it named was
// broken. A file-wide Contains that stayed green when one of two identical
// literals was inverted; a Contains on "probe.lastError" that survived
// `if (false && probe.lastError)`; a Contains on "lastRefresh" that survived
// `lastRefresh: null`. Each was written to fix the previous one.
//
// goja is already a direct dependency (it runs BotGuard and the cipher solver),
// and the copy helpers in utils.js are pure functions over plain objects. So
// they can simply be RUN, against the very asset the binary serves. A mutation
// that changes what the user reads now fails on what the user reads.
//
// WHAT STAYS A STRING MATCH, and why — these assert SOURCE SHAPE, which is the
// actual requirement, and no execution could show them:
//
//   - settings.js / setup.js calling these helpers at all. Both modules are
//     DOM-coupled (document, sl-alert, this.app) and are not loadable here;
//     what matters is that the call sites exist, in the right branch, with the
//     right arguments. Those assertions stay in cookies_setup_threestate_test
//     and are bracketed to the branch.
//   - the 60 s client cap, which is a constant inside one of those modules.
//   - the ORDER of the baseline probe against the finish POST, which is a fact
//     about statement sequence, not about a return value.
//
// Everything else about utils.js is asserted below by running it.

// utilsVM loads web/public/modules/utils.js into a goja runtime.
//
// The only transform is stripping the ES module `export` keyword, which goja
// does not parse; nothing else is rewritten, so what runs here is the shipped
// source. Line endings are normalised first because web/public is a mix of
// CRLF and LF.
//
// The DOM-dependent helpers in the module (isTypingInInput) and the network
// one (cookieSetupProbe) are never evaluated at load time, only at call time,
// so their missing globals cost nothing.
func utilsVM(t *testing.T) *goja.Runtime {
	t.Helper()
	src := readEmbeddedModule(t, "public/modules/utils.js")
	src = strings.ReplaceAll("\n"+src, "\nexport ", "\n")

	vm := goja.New()
	if _, err := vm.RunString(src); err != nil {
		t.Fatalf("utils.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// jsCall invokes one function from the module and returns its result as a Go
// value. A missing export fails loudly: the setup dialogs import these by name.
func jsCall(t *testing.T, vm *goja.Runtime, name string, args ...any) any {
	t.Helper()
	fnVal := vm.Get(name)
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		t.Fatalf("utils.js no longer exports a callable %s — the setup dialogs import it", name)
	}
	jsArgs := make([]goja.Value, 0, len(args))
	for _, a := range args {
		if a == nil {
			// Distinct from JS null: every real call site OMITS the optional
			// argument, and `{x = false} = {}` destructuring throws on null.
			jsArgs = append(jsArgs, goja.Undefined())
			continue
		}
		jsArgs = append(jsArgs, vm.ToValue(a))
	}
	out, err := fn(goja.Undefined(), jsArgs...)
	if err != nil {
		t.Fatalf("%s threw: %v", name, err)
	}
	return out.Export()
}

// jsAlert unpacks the {message, variant, icon} shape both copy helpers return.
func jsAlert(t *testing.T, raw any) (message, variant string) {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expected an alert object, got %T (%v)", raw, raw)
	}
	message, _ = m["message"].(string)
	variant, _ = m["variant"].(string)
	if message == "" || variant == "" {
		t.Fatalf("alert is missing message or variant: %v", m)
	}
	return message, variant
}

// probeShape builds the value cookieSetupProbe returns, so the rows below read
// as the server states they stand for.
func probeShape(ok, inProgress bool, lastError, lastRefresh any) map[string]any {
	return map[string]any{
		"ok": ok, "inProgress": inProgress,
		"lastError": lastError, "lastRefresh": lastRefresh,
	}
}

// TestCookieSetupCopySpeaksFromTheVerificationField runs the two copy helpers.
//
// The rows that matter are the `nil` ones. That is a NEWER FRONTEND AGAINST AN
// OLDER BINARY, which emits no verification field at all — the additive-field
// contract says those users must see exactly the copy they see today, and the
// property that delivers it is that the helpers branch POSITIVELY
// (`=== "unknown"`) rather than negatively (`!== "ok"`).
//
// The previous round asserted that by prohibiting the string `verification !==`
// in the source. This runs it instead: invert the comparison and the undefined
// rows render the hedged copy, which is a failure about what the user reads.
func TestCookieSetupCopySpeaksFromTheVerificationField(t *testing.T) {
	vm := utilsVM(t)

	t.Run("accepted toast", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			verification any
			wantVariant  string
			wantSaid     []string
			wantUnsaid   []string
		}{
			{"verified", "ok", "success", []string{"YouTube cookies configured"}, []string{"could not establish"}},
			{"could not check", "unknown", "warning",
				[]string{"cookies saved", "could not establish"}, []string{"configured", "failed"}},
			{"conclusively rejected", "failed", "danger",
				[]string{"auth verification failed"}, []string{"could not establish", "configured"}},
			{"older binary emits no field", nil, "success",
				[]string{"YouTube cookies configured"}, []string{"could not establish", "failed"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				msg, variant := jsAlert(t, jsCall(t, vm, "cookieSetupAcceptedToast", "YouTube", tc.verification))
				if variant != tc.wantVariant {
					t.Errorf("variant = %q, want %q: %q", variant, tc.wantVariant, msg)
				}
				assertCopy(t, msg, tc.wantSaid, tc.wantUnsaid)
			})
		}
	})

	t.Run("rejected message", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			verification any
			wantSaid     []string
			wantUnsaid   []string
		}{
			{"cannot sign a request", "unknown",
				[]string{"could not establish", "sign"}, []string{"No login detected"}},
			{"no credentials", "failed", []string{"No login detected"}, []string{"could not establish"}},
			{"older binary emits no field", nil, []string{"No login detected"}, []string{"could not establish"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				raw := jsCall(t, vm, "cookieSetupRejectedMessage", tc.verification)
				msg, ok := raw.(string)
				if !ok {
					t.Fatalf("cookieSetupRejectedMessage returned %T, want a string", raw)
				}
				assertCopy(t, msg, tc.wantSaid, tc.wantUnsaid)
			})
		}
	})
}

// TestCookieSetupAbortReportNeverClaimsAnUnknownSave is F1 asserted as
// behaviour, and it is the assertion this whole task turns on.
//
// `setupInProgress` goes false through cleanup() on every FinishSetup failure
// path — including the 422 that deliberately refuses to write cookies.txt, both
// early returns, and the server-side reap, which on this path is ordinary
// because the grace window and the client cap are both 60 s. A report built on
// that alone tells the user their cookies were saved in exactly the cases the
// code guaranteed they were not.
//
// Two rows carry the fix and neither can be expressed as a string match:
//
//   - "recorded an error AND stamped": the error must win. The previous round
//     approximated this with an index comparison on the source, which any
//     earlier textual mention of probe.lastError defeated.
//   - "stamped but the baseline was unreadable": without a trustworthy
//     baseline the stamp may have been there all along, and the two directions
//     do not cost the same — falsely claiming a save sends the user away
//     believing they are signed in.
//
// The invariant at the bottom is the real guard: across every row, the save
// sentence appears if and only if the row expects it. A sixth arm added later
// cannot quietly start claiming a save.
func TestCookieSetupAbortReportNeverClaimsAnUnknownSave(t *testing.T) {
	vm := utilsVM(t)

	readable := probeShape(true, false, nil, "2026-08-28T10:00:00Z")
	unreadable := probeShape(false, false, nil, nil)

	for _, tc := range []struct {
		name        string
		probe       map[string]any
		baseline    map[string]any
		opts        any
		wantVariant string
		wantSaved   bool
		wantSaid    []string
		wantUnsaid  []string
	}{
		{
			name: "the probe could not answer", probe: unreadable, baseline: readable,
			wantVariant: "warning", wantSaid: []string{"could not reach the server"},
		},
		{
			name:  "the setup is still running",
			probe: probeShape(true, true, nil, "2026-08-28T10:00:00Z"), baseline: readable,
			wantVariant: "warning", wantSaid: []string{"still working"},
		},
		{
			name:        "the server recorded an error",
			probe:       probeShape(true, false, "cookies.txt could not be read", "2026-08-28T10:00:00Z"),
			baseline:    readable,
			wantVariant: "danger", wantSaid: []string{"cookies.txt could not be read"},
		},
		{
			// THE ROW THE INDEX COMPARISON COULD NOT MAKE. A recorded error
			// outranks a moved stamp: the 422 frees the slot exactly like a
			// success does, and it is the case where nothing was written.
			name:        "recorded an error AND stamped",
			probe:       probeShape(true, false, "refusing to overwrite cookies.txt", "2026-08-28T10:05:00Z"),
			baseline:    readable,
			wantVariant: "danger", wantSaid: []string{"refusing to overwrite cookies.txt"},
		},
		{
			name:  "finished and committed",
			probe: probeShape(true, false, nil, "2026-08-28T10:05:00Z"), baseline: readable,
			wantVariant: "primary", wantSaved: true,
			wantUnsaid: []string{"cannot tell which platform"},
		},
		{
			// The stamp moved, but against a baseline we could not read it
			// proves nothing — the server may have carried it all along.
			name:  "stamped, but the baseline was unreadable",
			probe: probeShape(true, false, nil, "2026-08-28T10:05:00Z"), baseline: unreadable,
			wantVariant: "warning", wantSaid: []string{"nothing may have been saved"},
		},
		{
			// MkdirAll / atomic-write / jar-reload, both early returns, and the
			// reap. The slot is free and nothing at all was recorded.
			name:  "slot free, nothing recorded",
			probe: probeShape(true, false, nil, "2026-08-28T10:00:00Z"), baseline: readable,
			wantVariant: "warning", wantSaid: []string{"neither a result nor an error"},
		},
		{
			// F4. cookieYTDone/cookieTWDone are the sole source of the
			// active_platforms the wizard is about to write, and this path
			// cannot light either.
			name:  "wizard, finished and committed",
			probe: probeShape(true, false, nil, "2026-08-28T10:05:00Z"), baseline: readable,
			opts:        map[string]any{"wizard": true},
			wantVariant: "primary", wantSaved: true,
			wantSaid: []string{"cannot tell which platform"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, variant := jsAlert(t, jsCall(t, vm, "cookieSetupAbortReport", tc.probe, tc.baseline, tc.opts))

			if variant != tc.wantVariant {
				t.Errorf("variant = %q, want %q: %q", variant, tc.wantVariant, msg)
			}
			assertCopy(t, msg, tc.wantSaid, tc.wantUnsaid)

			// THE INVARIANT. Asserted on every row, not only the two that
			// expect a save, so a new arm cannot start claiming one silently.
			const saveClaim = "were saved"
			if got := strings.Contains(msg, saveClaim); got != tc.wantSaved {
				if tc.wantSaved {
					t.Errorf("the committed outcome no longer tells the user their cookies were saved: %q", msg)
				} else {
					t.Errorf("this outcome claims the cookies were saved, and the server said no such "+
						"thing — that is S17's own defect one level down: %q", msg)
				}
			}
			// Never the sentence the whole fix exists to delete.
			if strings.Contains(strings.ToLower(msg), "timed out") {
				t.Errorf("the abort report asserts a timeout again: %q", msg)
			}
		})
	}
}

// TestCookieSetupProbeNeverRejects pins the licence the unawaited baseline
// probe rests on.
//
// finishAutoCookieSetup dispatches the baseline WITHOUT awaiting it, so a
// rejection would surface as an unhandled rejection — and the abort arm awaits
// it later, where a rejection would throw straight past the alert, the recovery
// buttons and the countdown clear. That is F2's stranding, reintroduced.
//
// It was justified in a comment and pinned by nothing: deleting the catch left
// every assertion green. Here the module is run with a fetch that fails the way
// a dead server does, and the promise must still FULFIL with the unknown shape.
func TestCookieSetupProbeNeverRejects(t *testing.T) {
	vm := utilsVM(t)

	stubProbeHost(t, vm)
	vm.Set("fetch", func(goja.FunctionCall) goja.Value {
		panic(vm.NewGoError(errNetworkDown))
	})

	fn, ok := goja.AssertFunction(vm.Get("cookieSetupProbe"))
	if !ok {
		t.Fatal("utils.js no longer exports a callable cookieSetupProbe")
	}
	out, err := fn(goja.Undefined())
	if err != nil {
		t.Fatalf("cookieSetupProbe threw synchronously — the unawaited baseline would be an "+
			"unhandled rejection and the abort arm would throw past its own alert: %v", err)
	}

	promise, ok := out.Export().(*goja.Promise)
	if !ok {
		t.Fatalf("cookieSetupProbe no longer returns a promise, got %T", out.Export())
	}
	if promise.State() == goja.PromiseStateRejected {
		t.Fatalf("cookieSetupProbe REJECTED on a failed fetch (%v). The baseline is held unawaited "+
			"and the abort arm awaits it — a rejection strands the user with no alert, no recovery "+
			"buttons and the countdown still on screen, which is exactly F2", promise.Result())
	}
	if promise.State() != goja.PromiseStateFulfilled {
		t.Fatalf("cookieSetupProbe left its promise pending on a fetch that already failed")
	}

	result, ok := promise.Result().Export().(map[string]any)
	if !ok {
		t.Fatalf("probe fulfilled with %T, want the status object", promise.Result().Export())
	}
	if result["ok"] != false {
		t.Errorf("a failed probe reports ok = %v — \"we could not find out\" must not round to a "+
			"conclusion", result["ok"])
	}
}

// TestCookieSetupProbeCarriesTheFieldsTheReportBranchesOn runs the probe
// against a stubbed /api/cookies/auto-status body.
//
// `setupInProgress` alone cannot say WHICH conclusion a freed slot reached, so
// the two discriminators have to survive the parse. The previous round asserted
// that with a source match on "status.lastRefresh", after a plain match on
// "lastRefresh" was shown to survive `lastRefresh: null` — the field name still
// appeared in the literal beside it. Reading the value back is not defeatable
// that way.
//
// The empty-string row is not decoration: jsonError writes no `error` key on a
// 200, and an empty lastError must normalise to null or the report's error arm
// fires on every clean finish and never reports a save at all.
func TestCookieSetupProbeCarriesTheFieldsTheReportBranchesOn(t *testing.T) {
	for _, tc := range []struct {
		name            string
		body            map[string]any
		wantInProgress  bool
		wantLastError   any
		wantLastRefresh any
	}{
		{
			name: "a finish that recorded an error",
			body: map[string]any{
				"setupInProgress": false,
				"lastError":       "cookies.txt could not be read",
				"lastRefresh":     "2026-08-28T10:00:00Z",
			},
			wantLastError: "cookies.txt could not be read", wantLastRefresh: "2026-08-28T10:00:00Z",
		},
		{
			name:           "a setup still in flight",
			body:           map[string]any{"setupInProgress": true, "lastError": nil, "lastRefresh": nil},
			wantInProgress: true, wantLastError: nil, wantLastRefresh: nil,
		},
		{
			name: "an empty error is not an error",
			body: map[string]any{
				"setupInProgress": false, "lastError": "", "lastRefresh": "2026-08-28T10:00:00Z",
			},
			wantLastError: nil, wantLastRefresh: "2026-08-28T10:00:00Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := utilsVM(t)
			stubProbeHost(t, vm)
			body := tc.body
			vm.Set("fetch", func(goja.FunctionCall) goja.Value {
				response := vm.NewObject()
				response.Set("ok", true)
				response.Set("json", func(goja.FunctionCall) goja.Value { return vm.ToValue(body) })
				return response
			})

			result := runProbe(t, vm)
			if result["inProgress"] != tc.wantInProgress {
				t.Errorf("inProgress = %v, want %v", result["inProgress"], tc.wantInProgress)
			}
			if result["lastError"] != tc.wantLastError {
				t.Errorf("lastError = %v, want %v — without it a freed slot is one undivided "+
					"outcome and the report goes back to guessing", result["lastError"], tc.wantLastError)
			}
			if result["lastRefresh"] != tc.wantLastRefresh {
				t.Errorf("lastRefresh = %v, want %v — it is the only evidence a finish committed",
					result["lastRefresh"], tc.wantLastRefresh)
			}
			if result["ok"] != true {
				t.Errorf("ok = %v on a 200, want true", result["ok"])
			}
		})
	}
}

// stubProbeHost supplies the browser globals cookieSetupProbe reaches for.
// setTimeout deliberately never fires: these tests are about what the probe
// reports, and the bound itself is measured elsewhere.
func stubProbeHost(t *testing.T, vm *goja.Runtime) {
	t.Helper()
	if err := vm.Set("AbortController", func(c goja.ConstructorCall) *goja.Object {
		c.This.Set("signal", vm.NewObject())
		c.This.Set("abort", func() {})
		return nil
	}); err != nil {
		t.Fatalf("stub AbortController: %v", err)
	}
	vm.Set("setTimeout", func(goja.FunctionCall) goja.Value { return vm.ToValue(1) })
	vm.Set("clearTimeout", func(goja.FunctionCall) goja.Value { return goja.Undefined() })
}

// runProbe calls cookieSetupProbe and returns its fulfilled value.
func runProbe(t *testing.T, vm *goja.Runtime) map[string]any {
	t.Helper()
	fn, ok := goja.AssertFunction(vm.Get("cookieSetupProbe"))
	if !ok {
		t.Fatal("utils.js no longer exports a callable cookieSetupProbe")
	}
	out, err := fn(goja.Undefined())
	if err != nil {
		t.Fatalf("cookieSetupProbe threw: %v", err)
	}
	promise, ok := out.Export().(*goja.Promise)
	if !ok {
		t.Fatalf("cookieSetupProbe returned %T, want a promise", out.Export())
	}
	if promise.State() != goja.PromiseStateFulfilled {
		t.Fatalf("probe promise state = %v, want fulfilled (result: %v)", promise.State(), promise.Result())
	}
	result, ok := promise.Result().Export().(map[string]any)
	if !ok {
		t.Fatalf("probe fulfilled with %T, want the status object", promise.Result().Export())
	}
	return result
}

// errNetworkDown is what the stubbed fetch raises. Package-level so the closure
// above does not allocate one per call.
var errNetworkDown = errNetwork("network unreachable")

type errNetwork string

func (e errNetwork) Error() string { return string(e) }

// assertCopy is the said/unsaid check both tables share. Case-insensitive,
// because the assertions are about what the sentence claims, not its casing.
func assertCopy(t *testing.T, msg string, said, unsaid []string) {
	t.Helper()
	lower := strings.ToLower(msg)
	for _, want := range said {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("copy does not say %q: %q", want, msg)
		}
	}
	for _, unwanted := range unsaid {
		if strings.Contains(lower, strings.ToLower(unwanted)) {
			t.Errorf("copy asserts %q, which this outcome does not establish: %q", unwanted, msg)
		}
	}
}

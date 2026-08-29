package routes

import (
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The Web half of "a parked job escalates its platform's badge", asserted by
// EXECUTION for the reason cookies_setup_utilsvm_test.go states at length:
// three rounds of review found three defects where an assertion about JS was
// written as a string match and stayed green while the behaviour it named was
// broken.
//
// There are two independent claims here and neither can stand in for the other:
//
//   - the DECISION — which platforms a job list parks — is parkedCookiePlatforms
//     in utils.js, a pure function, run directly below;
//   - the EDGE — that a job event reaches the status bar at all, and only when
//     the answer changed — is app.js's own code, which is DOM-coupled and cannot
//     be imported. It is run anyway, by lifting the two method bodies out of the
//     shipped module and calling them against a stub `this`. That is the shipped
//     source executing: delete the change guard, or the call site, and the
//     assertions below fail on what the operator would see.
//
// THE JUNCTION (~18): "the status bar updated" can be produced by any of
// updateStatusBar's four pre-existing triggers — the config load, the status
// load, the manual recheck and the manual browser refresh. None of them is
// touched here. The harness constructs the app object itself and fires nothing
// but handleMessage, so a passing assertion can only have come from the job
// event.

// appMethodVM loads utils.js and then installs two app.js METHOD BODIES on the
// global object, verbatim, as callable functions.
//
// `new Function`-style installation rather than importing app.js: the module
// imports six DOM-coupled controllers at its top and constructs the whole
// dashboard on load, so it cannot be evaluated here. Lifting the bodies keeps
// the assertion on shipped source while leaving the unrunnable parts out. Each
// body is bracketed by jsMethodBody, and the bracket is checked below rather
// than trusted — a restructured module that silently narrowed the window is the
// failure mode this whole file exists to avoid.
func appMethodVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := utilsVM(t)
	app := readEmbeddedModule(t, "public/app.js")

	sync := jsMethodBody(t, app, "_syncParkedBadge")
	if !strings.Contains(sync, "parkedCookiePlatforms(this.jobs)") {
		t.Fatalf("the bracketed _syncParkedBadge body no longer computes the parked set: %q", sync)
	}

	handle := jsMethodBodyAt(t, app, "\n  handleMessage(message) {")
	// The bracket's own premise. handleMessage is a long switch and
	// jsMethodBody stops at the first line that closes at method indentation;
	// if that ever lands early, every assertion below would be about a
	// truncated method that happens to still parse.
	for _, want := range []string{`case "job_update"`, `case "jobs_update"`, `case "job_deleted"`, `case "pong"`} {
		if !strings.Contains(handle, want) {
			t.Fatalf("the bracketed handleMessage body is missing %s — the window closed early and "+
				"these assertions would be about a fragment of the method", want)
		}
	}

	if _, err := vm.RunString(
		"globalThis.__syncParkedBadge = function () {" + sync + "\n};\n" +
			"globalThis.__handleMessage = function (message) {" + handle + "\n};\n",
	); err != nil {
		t.Fatalf("app.js's method bodies do not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// jsMethodBodyAt is jsMethodBody for a method that takes arguments, which its
// zero-argument header patterns cannot name. Same bracket, same limitation, and
// the caller checks the window it produced rather than trusting it.
func jsMethodBodyAt(t *testing.T, js, header string) string {
	t.Helper()
	at := strings.Index(js, header)
	if at < 0 {
		t.Fatalf("no method matching %q — the module was restructured and this assertion is "+
			"reading nothing", strings.TrimSpace(header))
	}
	body := js[at+len(header):]
	if end := strings.Index(body, "\n  }\n"); end >= 0 {
		body = body[:end]
	}
	return body
}

// stubApp is the `this` the two lifted methods run against: every collaborator
// they can reach, stubbed to a no-op, plus a COUNTER on updateStatusBar. The
// counter is the whole measurement — the four pre-existing triggers are absent
// by construction, so any increment came from the job event.
const stubApp = `
globalThis.__makeApp = function (jobs, parkedBaseline) {
  const app = {
    jobs: jobs,
    archivedJobs: [],
    selectedJobId: null,
    hideFinishedAgeDays: -1,
    backfillStatus: {},
    _parkedPlatforms: parkedBaseline,
    statusBarCalls: 0,
    updateStatusBar() { this.statusBarCalls++; },
    renderJobs() {},
    renderArchivedJobs() {},
    renderLogs() {},
    updateCheckCountdown() {},
    updateJobCard() {},
    updateJobDetails() {},
    fetchArchivedJobs() {},
    handleConnectivityChange() {},
    handleBackfillStatus() {},
    addLog() {},
    updateVersionIndicator() {},
    _evaluateArchiveBoundary() { return false; },
    _pruneArchivedAgainstActive() { return false; },
    _preserveStagingFields() {},
    _verifyJobExists() {},
    settings: { refreshBackfillBadges() {} },
    stats: { updateActiveIndicator() {}, updateDiskIndicator() {} },
    _syncParkedBadge: globalThis.__syncParkedBadge,
    handleMessage: globalThis.__handleMessage,
  };
  return app;
};
`

// jobEvent fires one WebSocket message at the stub app and returns the running
// updateStatusBar count.
func jobEvent(t *testing.T, vm *goja.Runtime, app goja.Value, msgType string, payload any) int64 {
	t.Helper()
	fn, ok := goja.AssertFunction(vm.Get("__fire"))
	if !ok {
		t.Fatal("the job-event harness did not install")
	}
	out, err := fn(goja.Undefined(), app, vm.ToValue(msgType), vm.ToValue(payload))
	if err != nil {
		t.Fatalf("handleMessage threw on a %s event — the browser would fail the same way: %v", msgType, err)
	}
	n, ok := out.Export().(int64)
	if !ok {
		t.Fatalf("the harness returned %T, want the call count", out.Export())
	}
	return n
}

func parkedBadgeVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := appMethodVM(t)
	if _, err := vm.RunString(stubApp + `
globalThis.__fire = function (app, type, payload) {
  app.handleMessage({ type: type, payload: payload });
  return app.statusBarCalls;
};
`); err != nil {
		t.Fatalf("install the job-event harness: %v", err)
	}
	return vm
}

func makeStubApp(t *testing.T, vm *goja.Runtime, jobs []map[string]any, baseline any) goja.Value {
	t.Helper()
	fn, ok := goja.AssertFunction(vm.Get("__makeApp"))
	if !ok {
		t.Fatal("the stub app factory did not install")
	}
	out, err := fn(goja.Undefined(), vm.ToValue(jobs), vm.ToValue(baseline))
	if err != nil {
		t.Fatalf("build the stub app: %v", err)
	}
	return out
}

// noParked is the reconciled baseline: the badge has already been told that
// nothing is parked. Starting from it is what makes the FIRST event below a
// change rather than the initial reconcile.
var noParked = map[string]any{"youtube": false, "twitch": false}

// TestJobEventRepaintsTheBadgeOnlyWhenTheParkedSetChanges is finding (i),
// executed.
//
// updateStatusBar had four call sites and not one of them was a job event, so a
// job the worker parked for a membership or cookie reason showed its remedy in
// the list while the aggregate badge above it kept whatever it last had — with
// no fallback poll to correct it. The TUI has escalated on parked jobs since
// the per-platform split; this is the Web catching up.
//
// The second row is the other half of the requirement and the one a naive fix
// fails: job events arrive at roughly 60 Hz per active download and
// updateStatusBar writes DOM. The rule is to make updates cheaper, never rarer,
// so the scan runs every time and only the repaint is gated on the answer
// changing.
func TestJobEventRepaintsTheBadgeOnlyWhenTheParkedSetChanges(t *testing.T) {
	vm := parkedBadgeVM(t)

	jobs := []map[string]any{{"id": "a", "status": "Downloading", "platform": "youtube"}}
	app := makeStubApp(t, vm, jobs, noParked)

	// A progress tick on a job that is not parked, before anything else: the
	// badge must not be repainted at all. Without this row "the badge updated"
	// is satisfied by a fix that repaints on every job event.
	if n := jobEvent(t, vm, app, "job_update", map[string]any{
		"id": "a", "status": "Downloading", "platform": "youtube", "progress": "V:1 A:1",
	}); n != 0 {
		t.Fatalf("an ordinary progress tick repainted the status bar (%d times). These arrive at "+
			"~60 Hz per download and the repaint writes DOM — the scan may run every time, the "+
			"repaint may not", n)
	}

	// THE PARK. Same job, now stopped for want of credentials.
	if n := jobEvent(t, vm, app, "job_update", map[string]any{
		"id": "a", "status": string(database.StatusCookies), "platform": "youtube",
	}); n != 1 {
		t.Fatalf("a job parked in %s did not reach the status bar (calls = %d). The job list shows "+
			"its remedy and the aggregate badge above it stays whatever it last was — there is no "+
			"other jobs→updateStatusBar path and no fallback poll",
			database.StatusCookies, n)
	}

	// A second event with the SAME parked set. This is the mutation row: delete
	// the change guard in _syncParkedBadge and the count goes to 2.
	if n := jobEvent(t, vm, app, "job_update", map[string]any{
		"id": "a", "status": string(database.StatusCookies), "platform": "youtube",
		"error": "Needs cookie refresh",
	}); n != 1 {
		t.Errorf("a second event with an unchanged parked set repainted the status bar again "+
			"(calls = %d, want 1). The guard is what keeps this off the 60 Hz path", n)
	}

	// And the clearing edge, which is the half a one-way fix loses: the badge
	// has to come back down when the park is resolved.
	if n := jobEvent(t, vm, app, "job_update", map[string]any{
		"id": "a", "status": "Downloading", "platform": "youtube",
	}); n != 2 {
		t.Errorf("resolving the park did not repaint the badge (calls = %d, want 2) — the "+
			"escalation would survive the condition that caused it", n)
	}
}

// TestEveryJobSetEventReconcilesTheBadge covers the three other messages that
// replace or shrink the job list.
//
// job_update is the one the finding named, but it is not the only way a parked
// job arrives: a reconnect replays the whole list through initial_state, a
// server-side rebroadcast comes as jobs_update, and job_deleted is the one
// gesture that can remove the LAST parked job. A fix wired to job_update alone
// leaves the badge wrong after every reconnect.
func TestEveryJobSetEventReconcilesTheBadge(t *testing.T) {
	parked := map[string]any{"id": "p", "status": string(database.StatusCookies), "platform": "twitch"}

	for _, tc := range []struct {
		name     string
		msgType  string
		payload  any
		wantCall bool
	}{
		{"initial_state replays a parked job", "initial_state", map[string]any{
			"jobs": []map[string]any{parked},
		}, true},
		{"jobs_update carries a parked job", "jobs_update", []map[string]any{parked}, true},
		{"jobs_update with nothing parked", "jobs_update", []map[string]any{
			{"id": "q", "status": "Finished", "platform": "twitch"},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := parkedBadgeVM(t)
			app := makeStubApp(t, vm, []map[string]any{}, noParked)
			n := jobEvent(t, vm, app, tc.msgType, tc.payload)
			if (n > 0) != tc.wantCall {
				t.Errorf("%s produced %d repaints, want %v — the badge is reconciled on every "+
					"event that replaces the job list, and on no event that does not change it",
					tc.msgType, n, tc.wantCall)
			}
		})
	}

	t.Run("job_deleted removes the last parked job", func(t *testing.T) {
		vm := parkedBadgeVM(t)
		app := makeStubApp(t, vm,
			[]map[string]any{parked},
			map[string]any{"youtube": false, "twitch": true},
		)
		if n := jobEvent(t, vm, app, "job_deleted", map[string]any{"id": "p"}); n != 1 {
			t.Errorf("deleting the last parked job left the badge red (repaints = %d, want 1). "+
				"Deletion is the one gesture that clears this escalation without a status change", n)
		}
	})
}

// TestParkedCookiePlatformsAttributesToTheRightPlatform runs the decision
// itself.
//
// It is the port of the TUI's parkedCookieJobs, and every row here is one of
// that function's stated rulings. The Twitch row is the one the TUI shipped
// wrong first: a single unfiltered flag consumed inside the YouTube branch
// reddened YouTube for a parked Twitch job, which sends the operator to
// re-export credentials that were never the problem.
func TestParkedCookiePlatformsAttributesToTheRightPlatform(t *testing.T) {
	vm := utilsVM(t)

	for _, tc := range []struct {
		name      string
		jobs      []map[string]any
		wantYT    bool
		wantTW    bool
		rationale string
	}{
		{
			name:      "nothing parked",
			jobs:      []map[string]any{{"status": "Downloading", "platform": "youtube"}},
			rationale: "a running job is not evidence of anything about credentials",
		},
		{
			name:   "a youtube park",
			jobs:   []map[string]any{{"status": string(database.StatusCookies), "platform": "youtube"}},
			wantYT: true,
		},
		{
			name:   "a twitch park escalates twitch and NOT youtube",
			jobs:   []map[string]any{{"status": string(database.StatusCookies), "platform": "twitch"}},
			wantTW: true,
			rationale: "pointing the operator at YouTube for a Twitch park sends them to re-export " +
				"credentials that were never the problem",
		},
		{
			name:   "an unset platform counts as youtube",
			jobs:   []map[string]any{{"status": string(database.StatusCookies)}},
			wantYT: true,
			rationale: "pre-Twitch rows really carry \"\", the importer backfills exactly \"youtube\", " +
				"and every other platform test in app.js means YouTube by \"not twitch\"",
		},
		{
			name: "both platforms parked",
			jobs: []map[string]any{
				{"status": string(database.StatusCookies), "platform": "youtube"},
				{"status": string(database.StatusCookies), "platform": "twitch"},
			},
			wantYT: true, wantTW: true,
		},
		{
			name: "a membership park still escalates",
			jobs: []map[string]any{{
				"status": string(database.StatusCookies), "platform": "youtube",
				"parkReason": "membership",
			}},
			wantYT: true,
			rationale: "no ParkReason filter, matching the TUI: in all three park reasons the remedy " +
				"is credentials of some kind, and filtering any out loses a real alarm",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := jsCall(t, vm, "parkedCookiePlatforms", tc.jobs)
			got, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("parkedCookiePlatforms returned %T, want {youtube, twitch}", raw)
			}
			if got["youtube"] != tc.wantYT {
				t.Errorf("youtube = %v, want %v. %s", got["youtube"], tc.wantYT, tc.rationale)
			}
			if got["twitch"] != tc.wantTW {
				t.Errorf("twitch = %v, want %v. %s", got["twitch"], tc.wantTW, tc.rationale)
			}
		})
	}

	// Absent and empty inputs. The badge renders before the first WS payload
	// arrives, and a throw here would take updateStatusBar with it.
	for _, jobs := range []any{nil, []map[string]any{}} {
		raw := jsCall(t, vm, "parkedCookiePlatforms", jobs)
		got, ok := raw.(map[string]any)
		if !ok || got["youtube"] != false || got["twitch"] != false {
			t.Errorf("parkedCookiePlatforms(%v) = %v, want nothing parked — the badge renders "+
				"before the first job payload arrives", jobs, raw)
		}
	}
}

// TestWebParkedStatusMatchesGo is the third literal in the chain.
//
// utils.js has to spell the parked status itself; it cannot import Go. If
// database.StatusCookies is ever renamed, every Go call site moves with it and
// this one does not — parkedCookiePlatforms would then match nothing, the badge
// would stop escalating, and no Go test would notice.
//
// Asserted by EXECUTION, not by grepping the literal out of the module: a job
// carrying the Go constant's value must be seen as parked.
func TestWebParkedStatusMatchesGo(t *testing.T) {
	vm := utilsVM(t)
	raw := jsCall(t, vm, "parkedCookiePlatforms", []map[string]any{
		{"status": string(database.StatusCookies), "platform": "youtube"},
	})
	got, _ := raw.(map[string]any)
	if got["youtube"] != true {
		t.Errorf("a job whose status is database.StatusCookies (%q) is not seen as parked by "+
			"utils.js. The status literal in the module has drifted from the Go constant, and the "+
			"badge would silently stop escalating", database.StatusCookies)
	}
}

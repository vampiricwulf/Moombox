package routes

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// nopRouteLogger satisfies the anonymous logger interface
// cookies.NewAutoCookieService takes.
type nopRouteLogger struct{}

func (nopRouteLogger) Debug(string, ...any) {}
func (nopRouteLogger) Info(string, ...any)  {}
func (nopRouteLogger) Warn(string, ...any)  {}
func (nopRouteLogger) Error(string, ...any) {}

// TestCancelSetupRouteAnswers404WhenThereIsNothingToCancel is S18 at the wire.
//
// The handler used to answer {"success": true} unconditionally because
// CancelSetup returned nothing, so a second cancel — or a cancel with no setup
// ever started — told the caller it had cancelled something. The status code
// alone is not enough to assert here: chi answers an UNREGISTERED path with a
// bare 404 too, so a route rename would satisfy a status-only check while the
// endpoint had ceased to exist. The body is what distinguishes the two, and it
// has to carry the sentinel's own text.
func TestCancelSetupRouteAnswers404WhenThereIsNothingToCancel(t *testing.T) {
	svc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), nopRouteLogger{})

	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/cancel", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel with nothing in progress: status %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("cancel answered 404 with a body that is not the handler's JSON error — the "+
			"route may simply not be registered any more: %q", rec.Body.String())
	}
	if got, _ := body["error"].(string); !strings.Contains(got, cookies.ErrNoSetupInProgress.Error()) {
		t.Errorf("404 body = %q, want it to carry %q", got, cookies.ErrNoSetupInProgress.Error())
	}
	if _, ok := body["success"]; ok {
		t.Errorf("a refused cancel still reports a success field: %v", body)
	}
}

// TestCancelSetupRouteReusesTheFinishHandlerShape guards the instruction that
// came with S18: the finish handler already had an ErrNoSetupInProgress arm and
// the cancel handler had to reuse it rather than invent a second convention.
// Both endpoints answer the same question — "was there a setup here to act
// on?" — and a reader who learns one mapping should not have to check the
// other.
func TestCancelSetupRouteReusesTheFinishHandlerShape(t *testing.T) {
	svc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), nopRouteLogger{})

	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)

	// Neither endpoint has a setup to act on, so both must reach their
	// ErrNoSetupInProgress arm.
	statuses := map[string]int{}
	bodies := map[string]string{}
	for _, path := range []string{"/api/cookies/auto-setup/cancel", "/api/cookies/auto-setup/finish"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		statuses[path] = rec.Code
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		bodies[path], _ = body["error"].(string)
	}

	if statuses["/api/cookies/auto-setup/finish"] != http.StatusNotFound {
		t.Fatalf("fixture is broken — finish must already answer 404 here, got %d",
			statuses["/api/cookies/auto-setup/finish"])
	}
	if statuses["/api/cookies/auto-setup/cancel"] != statuses["/api/cookies/auto-setup/finish"] {
		t.Errorf("cancel answers %d where finish answers %d for the same missing setup",
			statuses["/api/cookies/auto-setup/cancel"], statuses["/api/cookies/auto-setup/finish"])
	}
	if bodies["/api/cookies/auto-setup/cancel"] != bodies["/api/cookies/auto-setup/finish"] {
		t.Errorf("cancel and finish word the same condition differently:\n\tcancel: %q\n\tfinish: %q",
			bodies["/api/cookies/auto-setup/cancel"], bodies["/api/cookies/auto-setup/finish"])
	}
}

// TestStartSetupRouteMapsServiceStopped covers the new sentinel's only wire
// mapping. 503 rather than the 409 the two in-progress conflicts get: those
// clear on their own and this one never does, so "try again shortly" is the
// wrong advice to encode in the status code.
func TestStartSetupRouteMapsServiceStopped(t *testing.T) {
	svc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), nopRouteLogger{})

	// Browser guard, and it is not decoration. A regression in the stopped
	// gate lets StartSetup fall through to browser detection, and on any
	// machine with Firefox or Chrome installed — every developer machine, and
	// the owner's, which runs real browser windows on other profiles — that
	// means this test OPENS ONE instead of failing. The cookies-package tests
	// stub the unexported detectBrowser seam; from here the reachable seam is
	// ConfiguredBrowserOverride, which resolvedBrowser consults first, so
	// pointing it at a path inside a fresh temp dir substitutes a browser that
	// provably cannot launch for whatever is really installed.
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser.exe")
	svc.ConfiguredBrowserOverride = func() (string, string) { return unlaunchable, "chrome" }

	svc.Stop()

	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/start", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("start on a stopped service: status %d, want %d — a 500 would read as a bug "+
			"rather than as shutdown", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	got, _ := body["error"].(string)
	if !strings.Contains(got, cookies.ErrServiceStopped.Error()) {
		t.Errorf("503 body = %q, want it to carry %q — the generic "+
			"\"auto-cookie service not configured\" 503 above means the same code has to be "+
			"told apart by its message", got, cookies.ErrServiceStopped.Error())
	}
}

// TestAutoStatusCarriesNoFieldNothingCanRead is V10.
//
// `configured` was computed as `profileDir != ""` and could not be false:
// cmd/moombox seeds the profile dir with a "./browser-profile" default before
// the service is constructed, so every install reported true from its first
// run. Nothing anywhere read it. A value that is always true and read by nobody
// is worse than an absent one — the next reader believes it — so it was
// deleted rather than made to mean something nobody asked for.
//
// The second half is the seam the deletion could half-miss: the no-service
// branch of the same endpoint hand-builds its body, so `configured` lived in
// two places and removing one would have left the frontend a field the real
// status never sends.
//
// The subset runs one way here, deliberately: this test is about `configured`
// specifically, and TestAutoStatusNoServiceMapMatchesRealStatusKeys below is
// the one that pins the full bidirectional key-set equality (including
// `availableBrowsers`, which used to be missing from this branch entirely —
// see that test's doc for the history).
func TestAutoStatusCarriesNoFieldNothingCanRead(t *testing.T) {
	raw, err := json.Marshal(cookies.AutoCookieStatus{})
	if err != nil {
		t.Fatalf("marshal AutoCookieStatus: %v", err)
	}
	var real map[string]any
	if err := json.Unmarshal(raw, &real); err != nil {
		t.Fatalf("unmarshal AutoCookieStatus: %v", err)
	}
	if _, ok := real["configured"]; ok {
		t.Error("AutoCookieStatus emits `configured` again. It cannot be false — the profile dir is " +
			"seeded with a default before the service exists — so any reader that branches on it " +
			"takes the same branch on every install, forever")
	}

	r := chi.NewRouter()
	CookieRoutes(r, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cookies/auto-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-status with no service: status %d, want %d", rec.Code, http.StatusOK)
	}
	var fallback map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fallback); err != nil {
		t.Fatalf("no-service auto-status body is not JSON: %q", rec.Body.String())
	}
	if len(fallback) == 0 {
		t.Fatal("the no-service branch returned an empty object — this assertion is reading nothing")
	}
	for key := range fallback {
		if _, ok := real[key]; !ok {
			t.Errorf("the no-service branch of /api/cookies/auto-status emits %q, which "+
				"cookies.AutoCookieStatus does not — the frontend would learn a field the real "+
				"status never sends", key)
		}
	}
}

// TestAutoStatusNoServiceMapMatchesRealStatusKeys pins the FULL key-set
// equality the no-service branch's own comment claims ("Every key here has
// to exist on cookies.AutoCookieStatus too") but nothing previously checked
// in the direction that actually drifted: `availableBrowsers` exists on
// AutoCookieStatus and populateBrowserSelector (web/public) reads it, but the
// no-service fallback map omitted it entirely — unreachable in production
// (services.go always constructs the service unconditionally) but exactly
// the kind of drift that becomes a live bug the day that stops being true.
//
// ConfiguredBrowserPath/ConfiguredBrowserType are deliberately NOT in the
// fallback map, and this test is what makes that safe to assert: both carry
// `omitempty` on AutoCookieStatus, and a zero-value struct — no configured
// override, which is exactly the no-service branch's situation — omits them
// from its JSON entirely. Adding them to the fallback map would have been
// the drift in the OTHER direction: keys the fallback sends that a real
// zero-value response does not.
//
// A zero-value AutoCookieStatus is the right comparison basis, not a live
// GetStatus() call: the no-service branch exists precisely because there is
// no service to call GetStatus on, so "what would an unconfigured install's
// status look like" is a zero value by construction.
func TestAutoStatusNoServiceMapMatchesRealStatusKeys(t *testing.T) {
	raw, err := json.Marshal(cookies.AutoCookieStatus{})
	if err != nil {
		t.Fatalf("marshal zero-value AutoCookieStatus: %v", err)
	}
	var real map[string]any
	if err := json.Unmarshal(raw, &real); err != nil {
		t.Fatalf("unmarshal AutoCookieStatus: %v", err)
	}

	r := chi.NewRouter()
	CookieRoutes(r, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cookies/auto-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-status with no service: status %d, want %d", rec.Code, http.StatusOK)
	}
	var fallback map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fallback); err != nil {
		t.Fatalf("no-service auto-status body is not JSON: %q", rec.Body.String())
	}

	for key := range real {
		if _, ok := fallback[key]; !ok {
			t.Errorf("cookies.AutoCookieStatus emits %q but the no-service branch of "+
				"/api/cookies/auto-status does not — a frontend that always reads this key "+
				"(without a missing-key fallback) breaks on an unconfigured install", key)
		}
	}
	for key := range fallback {
		if _, ok := real[key]; !ok {
			t.Errorf("the no-service branch of /api/cookies/auto-status emits %q, which a "+
				"zero-value cookies.AutoCookieStatus does not — the frontend would learn a "+
				"field the real status never sends on a fresh install", key)
		}
	}

	// The specific drift this test exists to catch, named directly so a
	// future key-set change that happens to keep the counts equal cannot
	// silently satisfy the loops above while dropping this one.
	if _, ok := fallback["availableBrowsers"]; !ok {
		t.Error("the no-service branch is missing availableBrowsers — populateBrowserSelector " +
			"(web/public) iterates this field unconditionally")
	}
	if list, ok := fallback["availableBrowsers"].([]any); !ok || list == nil {
		t.Errorf("availableBrowsers = %#v, want a non-nil (possibly empty) array — "+
			"the frontend iterates it with no null-check", fallback["availableBrowsers"])
	}
}

// TestCookieRefreshOutcomeSeparatesDeclinedFromFailed pins the wire fields the
// manual-refresh toast branches on.
//
// The defect this guards: `success` alone cannot distinguish "the credentials
// were checked and rejected" from "no check happened". The single refresh slot
// is held by the 30-minute periodic tick and by interactive setup, so clicking
// "Refresh now" during either returns refreshDeclined() — a pass that looked at
// nothing — and the toast read "auth verification failed" in the very same
// payload whose cookieStatus reported the session authenticated.
//
// `ran` and `verdict` are additive: `success` keeps its exact old meaning, so
// an older frontend against a newer binary behaves as it did. That is the same
// precedent `renewed` set.
//
// The "conclusively unauthenticated" row is the premise for the others. Without
// it, a payload that simply never reported a failure would satisfy every
// assertion here by saying nothing at all.
func TestCookieRefreshOutcomeSeparatesDeclinedFromFailed(t *testing.T) {
	tests := []struct {
		name        string
		result      cookies.RefreshResult
		wantSuccess bool
		wantRan     bool
		wantVerdict string
	}{
		{
			// refreshDeclined() is the zero value, by construction.
			name:        "declined — the slot was already held",
			result:      cookies.RefreshResult{},
			wantSuccess: false,
			wantRan:     false,
			wantVerdict: "unknown",
		},
		{
			// refreshAborted()'s shape, and every pass whose verification
			// could not reach the service.
			name:        "ran but learned nothing",
			result:      cookies.RefreshResult{Ran: true},
			wantSuccess: false,
			wantRan:     true,
			wantVerdict: "unknown",
		},
		{
			name:        "conclusively unauthenticated",
			result:      cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshFailed, YouTubeStored: true},
			wantSuccess: false,
			wantRan:     true,
			wantVerdict: "failed",
		},
		{
			// One healthy platform is enough for the whole-service verdict:
			// authenticated work is possible. Unchanged from before.
			name:        "one platform verified",
			result:      cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed},
			wantSuccess: true,
			wantRan:     true,
			wantVerdict: "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cookieRefreshOutcome(tt.result)
			if got["success"] != tt.wantSuccess {
				t.Errorf("success = %v, want %v", got["success"], tt.wantSuccess)
			}
			if got["ran"] != tt.wantRan {
				t.Errorf("ran = %v, want %v — the toast cannot tell a declined pass "+
					"from a failed one without it", got["ran"], tt.wantRan)
			}
			if got["verdict"] != tt.wantVerdict {
				t.Errorf("verdict = %v, want %q", got["verdict"], tt.wantVerdict)
			}
		})
	}
}

// TestCookieRefreshOutcomeKeepsRenewedIndependent guards the field that is a
// fact about the MECHANISM against being folded into the verdicts, which are
// facts about the CREDENTIALS. A pass can verify both platforms while renewing
// nothing (a browser that never ran, or any launch on a platform with no Job
// Object to drain).
func TestCookieRefreshOutcomeKeepsRenewedIndependent(t *testing.T) {
	verifiedNotRenewed := cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshOK, Renewed: false}
	got := cookieRefreshOutcome(verifiedNotRenewed)
	if got["success"] != true || got["verdict"] != "ok" {
		t.Errorf("a verified pass stopped reading as verified because it renewed nothing: %v", got)
	}
	if got["renewed"] != false {
		t.Errorf("renewed = %v, want false", got["renewed"])
	}
}

// TestCookieRefreshOutcomeCarriesTheMechanism pins the additive key the two
// post-flight surfaces read.
//
// The empty row is the one with teeth. A pass that declined before choosing a
// path has no mechanism, and the payload has to say so rather than default to
// "browser": the dashboard treats empty and absent identically and falls back
// to cookies.acquisition, and a payload that asserted "browser" there would
// override that fallback with a guess.
func TestCookieRefreshOutcomeCarriesTheMechanism(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a browser pass", cookies.RefreshMechanismBrowser, "browser"},
		{"a profile import", cookies.RefreshMechanismProfileImport, "profile-import"},
		{"a pass that declined before choosing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cookieRefreshOutcome(cookies.RefreshResult{Ran: true, Mechanism: tc.in})
			if got["mechanism"] != tc.want {
				t.Errorf("mechanism = %v, want %q — the dashboard's toast subject comes from this key",
					got["mechanism"], tc.want)
			}
		})
	}
}

// TestAppJSReadsTheFieldsTheHandlerEmits pins the one seam nothing else can
// see: the Web toast's branch conditions are JavaScript, and no JS harness
// exists in-tree.
//
// The realistic drift is a Go-side rename leaving app.js reading `undefined`,
// which is worse than a crash — `undefined === false` is false, so a renamed
// `ran` silently stops the declined arm from ever firing and the toast falls
// back to claiming the cookies could not be established. Reading the field
// names out of cookieRefreshOutcome's own output means a rename fails here
// rather than in the field.
//
// app.js is embedded straight from web/public with no build step, so the source
// and the asset cannot disagree; the only thing at risk is the contract, and
// that is exactly what this reads.
//
// Strict equality is load-bearing and asserted verbatim. It is what makes the
// additive claim hold: against an older binary that emits neither field,
// `undefined === false` and `undefined === "failed"` are both false, so the
// toast degrades to the hedged arm and never to the danger one.
func TestAppJSReadsTheFieldsTheHandlerEmits(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	js := string(raw)

	payload := cookieRefreshOutcome(cookies.RefreshResult{})
	for _, tc := range []struct{ key, expr string }{
		{"ran", "data.ran === false"},
		{"verdict", `data.verdict === "failed"`},
		// Read, not compared: app.js hands this to cookieRefreshMechanismLabel
		// rather than branching on it, and the label owns the fallback for an
		// absent or empty value. What must not drift is the NAME.
		{"mechanism", "data.mechanism"},
	} {
		if _, ok := payload[tc.key]; !ok {
			t.Fatalf("cookieRefreshOutcome no longer emits %q, but app.js still reads data.%s — "+
				"the toast would compare against undefined", tc.key, tc.key)
		}
		if !strings.Contains(js, tc.expr) {
			t.Errorf("app.js does not contain %q — the Web toast can no longer tell a declined "+
				"pass from a conclusively failed one", tc.expr)
		}
	}

	// success is the legacy field every non-success branch is still gated on;
	// losing it would make all four hedged arms unreachable.
	if !strings.Contains(js, "!data.success") {
		t.Error("app.js no longer gates the refresh toast on data.success")
	}
}

// TestAppJSMatchesTheDeclinedCauses keeps the Web copy of the declined-cause
// list in step with the Go one. The two Go surfaces share
// cookies.RefreshDeclinedCauses; app.js cannot import it, and the phrasing had
// already drifted once ("cookies to refresh" against "cookies worth
// refreshing") with nothing to catch it.
func TestAppJSMatchesTheDeclinedCauses(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	if !strings.Contains(string(raw), cookies.RefreshDeclinedCauses) {
		t.Errorf("app.js does not carry the shared declined-cause list verbatim:\n\t%q\n"+
			"Three surfaces render one exhaustive set; a fourth phrasing means one of them "+
			"is naming a different set of causes.", cookies.RefreshDeclinedCauses)
	}
}

// TestCookieSetupClientsTellTheServerWhenTheUserGivesUp is S16, pinned the only
// way Go can pin browser code: against the embedded asset the binary actually
// serves.
//
// Two abandonment paths existed with no POST behind either of them. Closing the
// tab fired nothing — the only unload handler in settings.js guards the
// unsaved-changes flag — and the Skip button on the extraction-timeout alert
// called dialog.hide() directly, which does NOT fire sl-request-close and so
// never reached the cancel handler wired to it. Both left the server holding a
// setup, which is what wedges acquisition; the server-side reap is the backstop
// for the cases with no client at all, and these are the fast path.
//
// The unload beacon is deliberately on pagehide rather than beforeunload: the
// beforeunload handler beside it can put a "Leave site?" confirm in front of
// the user, and a beacon fired from there would cancel a setup they chose to
// stay for. pagehide's other case — the back/forward cache — is pinned too,
// because it is the one that turns a fast path into a trap: a bfcache
// round-trip would cancel a live setup on the server host AND clear the flag on
// the way out, leaving the restored page with a dialog it can no longer cancel.
func TestCookieSetupClientsTellTheServerWhenTheUserGivesUp(t *testing.T) {
	for _, mod := range []struct {
		path     string
		cancelFn string
	}{
		{"public/modules/settings.js", "this.cancelAutoCookieSetup()"},
		{"public/modules/setup.js", "this.cancelCookieSetup()"},
	} {
		raw, err := webassets.PublicFS.ReadFile(mod.path)
		if err != nil {
			t.Fatalf("read embedded %s: %v", mod.path, err)
		}
		js := string(raw)

		if !strings.Contains(js, beaconEndpoint) {
			t.Errorf("%s never tells the server the tab is going away mid-setup — "+
				"an abandoned wizard then blocks every setup and every periodic refresh "+
				"until something releases the slot", mod.path)
		}
		// The endpoint is the finding, not a detail. /cancel is the user's
		// abort and closes the setup browser — true on the Firefox path too
		// since it gained a Job Object. This flow's instructions send the user
		// AWAY from this tab to sign in, so a beacon pointed at /cancel makes
		// closing the idle dashboard tab a remote kill of the window they are
		// typing into. Checked as an absence as well as a presence: adding
		// /abandon while leaving a /cancel beacon behind fixes nothing.
		if strings.Contains(js, `navigator.sendBeacon("/api/cookies/auto-setup/cancel")`) {
			t.Errorf("%s still beacons a CANCEL on unload. A click is consent to close the "+
				"browser; a tab unload is not — post %s instead", mod.path, beaconEndpoint)
		}
		// Matched on the event registration alone, not on the handler's
		// signature: a missing `e.persisted` must fail as a missing bfcache
		// guard, not as "this is not pagehide".
		const pagehide = `window.addEventListener("pagehide"`
		_, hideBody, hideFound := strings.Cut(js, pagehide)
		if !hideFound {
			t.Errorf("%s fires its cancel beacon from something other than pagehide; "+
				"beforeunload can be cancelled by the user and would kill a setup they kept", mod.path)
		} else {
			// Bracketed to the handler body, not the file: `e.persisted`
			// appearing anywhere else would prove nothing about this beacon.
			if end := strings.Index(hideBody, "});"); end >= 0 {
				hideBody = hideBody[:end]
			}
			// Whitespace-normalised and matched on the WHOLE guard, sense
			// included. Asking only whether "e.persisted" appears passes against
			// `if (!e.persisted) return;` — an inverted guard that fires the
			// beacon on exactly the case it exists to skip and skips every real
			// unload. Verified: the earlier version of this check did.
			compact := strings.Join(strings.Fields(hideBody), " ")
			const guard = "if (e.persisted) return"
			guardAt := strings.Index(compact, guard)
			if guardAt < 0 {
				t.Errorf("%s does not bail out of its pagehide beacon on the bfcache case "+
					"(want %q, sense and all). That page is coming back, the setup it just "+
					"cancelled is not, and the flag is cleared on the way out so the restored "+
					"page cannot cancel either:\n%s", mod.path, guard, compact)
			}
			beaconAt := strings.Index(compact, beaconEndpoint)
			if beaconAt < 0 {
				t.Errorf("%s's pagehide handler does not send the release beacon:\n%s", mod.path, compact)
			}
			if guardAt >= 0 && beaconAt >= 0 && guardAt > beaconAt {
				t.Errorf("%s checks e.persisted only AFTER firing the beacon, which is no check "+
					"at all — the release has already gone:\n%s", mod.path, compact)
			}
		}

		// The Skip button, in the handler the timeout alert wires up. Checked as
		// a window after the id rather than anywhere in the file, because the
		// cancel function is called from several other places and a match
		// elsewhere would prove nothing about this button.
		const skipHandler = `document.getElementById("cookie-skip-btn")?.addEventListener("click", () => {`
		_, body, found := strings.Cut(js, skipHandler)
		if !found {
			t.Fatalf("%s no longer wires a click handler on #cookie-skip-btn — "+
				"re-derive this test against the new shape", mod.path)
		}
		if end := strings.Index(body, "});"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, mod.cancelFn) {
			t.Errorf("%s: Skip does not route through %s. Hiding the dialog directly does not "+
				"fire sl-request-close, so the server is never told:\n%s",
				mod.path, mod.cancelFn, body)
		}
	}
}

// beaconEndpoint is the URL the unload beacon posts to, and the one thing the
// JS pin above and the route check below must agree on. Declared once so a
// change has to move both.
const beaconEndpoint = `navigator.sendBeacon("/api/cookies/auto-setup/abandon")`

// TestTheUnloadBeaconPostsToARouteThatExists closes the gap between the two
// halves of the beacon fix: the JS pin proves the client asks for /abandon, and
// this proves something is listening.
//
// Without it the two could drift into the worst combination — a beacon posting
// to a route nobody registered, which fails silently (sendBeacon reports
// nothing) and takes the Linux/Docker release with it, the one platform where
// the beacon is the ONLY thing that frees an abandoned setup slot.
//
// The discriminator is the BODY, not the status. chi answers an unregistered
// path with 404 too, so a status check alone would pass against a route that
// does not exist; only a registered handler produces this JSON. The service is
// real and has no setup in flight, so AbandonSetup returns ErrNoSetupInProgress
// and the handler renders it through the same arm /cancel and /finish use.
func TestTheUnloadBeaconPostsToARouteThatExists(t *testing.T) {
	// Re-derive the path from the JS rather than restating it, so this cannot
	// keep passing against a URL the client no longer sends.
	raw, err := webassets.PublicFS.ReadFile("public/modules/settings.js")
	if err != nil {
		t.Fatalf("read embedded settings.js: %v", err)
	}
	const prefix = `navigator.sendBeacon("`
	at := strings.Index(string(raw), prefix)
	if at < 0 {
		t.Fatal("settings.js sends no beacon at all")
	}
	rest := string(raw)[at+len(prefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("could not read the beacon's URL out of settings.js")
	}
	path := rest[:end]

	svc := cookies.NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})
	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))

	var body map[string]any
	if jsonErr := json.Unmarshal(rec.Body.Bytes(), &body); jsonErr != nil {
		t.Fatalf("the beacon's endpoint %q is not registered — chi answered %d with %q, "+
			"which is its own not-found page, not a handler's JSON. On Linux and in Docker "+
			"this beacon is the only thing that releases an abandoned setup slot, and "+
			"sendBeacon reports nothing when it lands nowhere.",
			path, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("%s with no setup in flight: got %d, want %d to match /cancel and /finish",
			path, rec.Code, http.StatusNotFound)
	}
	if body["error"] == nil {
		t.Errorf("%s answered a missing setup without an error field: %v", path, body)
	}
}

// TestCookieIndicatorNamesAnUnreadableFile is COOKIES-6's Web half, run out of
// the shipped utils.js. An unreadable cookies.txt used to fall into the
// `!found` arm and render as never-configured — the container operator was
// told "no cookies" about a file sitting on the volume.
//
// The arm sits AFTER `authenticated`, deliberately: a later reload failing over
// a jar that still holds working credentials is not a reason to redden a badge
// whose requests are succeeding. It is the "no cookies" misreport that is being
// corrected, not the green state.
//
// Mutants:
//   - drop the fileError arm -> row 1 renders the absent-copy (indicator-warn
//     or the platform's `absent` title) instead of naming the file.
//   - put the arm ahead of `authenticated` -> row 3 turns red while
//     authenticated requests are demonstrably working.
func TestCookieIndicatorNamesAnUnreadableFile(t *testing.T) {
	vm := utilsVM(t)
	for _, tc := range []struct {
		name         string
		status       map[string]any
		wantClass    string
		wantContains string
	}{
		{
			"unreadable file, nothing loaded",
			map[string]any{"found": false, "authenticated": false, "verification": "unknown",
				"fileError": "open /data/cookies.txt: permission denied"},
			"indicator-error", "could not be read",
		},
		{
			"ordinary never-configured",
			map[string]any{"found": false, "authenticated": false, "verification": "unknown", "fileError": ""},
			"", "",
		},
		{
			"unreadable file but the jar still authenticates",
			map[string]any{"found": true, "authenticated": true, "verification": "ok",
				"fileError": "open /data/cookies.txt: permission denied"},
			"indicator-ok", "Authenticated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := jsCall(t, vm, "cookieIndicatorState", "youtube", tc.status, false, false).(map[string]any)
			class, _ := got["className"].(string)
			title, _ := got["title"].(string)
			if tc.wantClass != "" && class != tc.wantClass {
				t.Errorf("className = %q, want %q (title %q)", class, tc.wantClass, title)
			}
			if tc.wantContains != "" && !strings.Contains(title, tc.wantContains) {
				t.Errorf("title = %q, want it to contain %q", title, tc.wantContains)
			}
			if tc.wantClass == "" && strings.Contains(title, "could not be read") {
				t.Errorf("title = %q — a file that is merely absent must not claim it could not be read", title)
			}
		})
	}
}

// TestCookieStatusPayloadsCarryTheFileError pins the wire half of COOKIES-6 on
// BOTH projections. The sentinel is platform-independent — one cookies.txt
// holds both platforms' rows — so either badge must be able to name it, and a
// key added to one payload and not the other is the junction defect
// CookieStatusPayload's own doc comment exists to prevent.
//
// Mutants:
//   - add `fileError` to CookieStatusPayload only -> the Twitch row fails.
//   - project it from a per-platform field -> the shared-sentinel row fails.
func TestCookieStatusPayloadsCarryTheFileError(t *testing.T) {
	const sentinel = "open /data/cookies.txt: permission denied"
	status := cookies.AuthStatus{CookieFileError: sentinel}

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"cookieStatus", CookieStatusPayload(status)},
		{"twitchAuthStatus", TwitchAuthStatusPayload(status)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.payload["fileError"]
			if !ok {
				t.Fatalf("%s carries no fileError key — the badge cannot name the file it "+
					"could not read, and renders a mounted file as never-configured", tc.name)
			}
			if got != sentinel {
				t.Errorf("fileError = %v, want %q", got, sentinel)
			}
		})
	}

	// Empty stays empty: a jar that loaded is not a jar that failed to.
	for name, payload := range map[string]map[string]any{
		"cookieStatus":     CookieStatusPayload(cookies.AuthStatus{}),
		"twitchAuthStatus": TwitchAuthStatusPayload(cookies.AuthStatus{}),
	} {
		if got := payload["fileError"]; got != "" {
			t.Errorf("%s fileError = %v for a jar that loaded, want empty", name, got)
		}
	}
}

// --- COOKIES-4 / owner decision O-L: Content-Length before the blocking re-check ---

// gzipIdentityCeiling is internal/web's gzipMinSize, repeated here because that
// const is unexported.
//
// It is the width at which CompressionMiddleware stops being harmless to this
// fix. Below it, a Flush before the threshold sends the buffered bytes through
// commitPlain, which does NOT touch Content-Length — the header the two
// handlers set survives and the body is identity and self-terminating. At or
// above it, startGzip deletes Content-Length, sets Content-Encoding: gzip and
// re-chunks, and the gzip trailer is written by the middleware's
// `defer gz.Close()` — which runs after the handler returns, i.e. after the
// re-check. Both halves are asserted by
// TestSizedCookieAnswersSurviveTheGzipWrapper.
const gzipIdentityCeiling = 1024

// recheckStandIn is how long the stand-in re-check blocks the handler goroutine
// in TestSizedCookieAnswerLandsBeforeTheBlockingRecheck, and
// sizedAnswerArrivalBound is how long the client may take to have the whole
// body in hand.
//
// The real thing is bounded at 45 s and a real pass spends two auth-check
// windows; a second is enough to make the two outcomes unmistakable — a body at
// single-digit milliseconds against one at the full block — while keeping the
// test cheap enough to run under -race -count=3.
const (
	recheckStandIn          = 1200 * time.Millisecond
	sizedAnswerArrivalBound = 300 * time.Millisecond
)

// sizedWriterRouter is importRouter's twin for the timing test, and it exists
// for the one thing importRouter cannot hand back: the RefreshService.
//
// The property under test is what the client holds WHILE that service's pass is
// still running on the handler goroutine, and OnAuthChange is the only seam
// this package can reach into a pass with — youtubeGuideURL, twitchValidateURL
// and refreshPassHook are all unexported in internal/cookies.
//
// The jar handed to the RefreshService is EMPTY and is not the import service's
// jar, so the pass costs no network however successful the import was:
// youtubeGuideExchange returns before it builds a request when
// HasAnyYouTubeAuthCookie is false, and checkTwitchAuth does the same on an
// empty auth-token. It still reaches OnAuthChange, because
// verdictFromCheck(false, nil) is RefreshFailed against a zero AuthStatus's
// RefreshUnknown and authStatusChanged compares that field — so the hook fires
// on the first pass of an empty jar, which is exactly the pass this test runs.
func sizedWriterRouter(t *testing.T, seed string) (chi.Router, *cookies.RefreshService) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	svc := cookies.NewAutoCookieService(dir, path, jar, nopRouteLogger{})
	svc.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	svc.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	rs := cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, nopRouteLogger{})

	r := chi.NewRouter()
	CookieRoutes(r, rs, svc, nil, nil)
	return r, rs
}

// driveImportSuccess answers 200 through cookieImportOutcome.
func driveImportSuccess(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r, _, _, _ := importRouter(t, importHeader+importTwitch, nil)
	return postImport(t, r, "text/plain", importPaste())
}

// driveImportRejected answers 422 through the ErrImportNotNetscape arm. This is
// the row that matters most after the success one: it is an error exit, and a
// fix applied to the success write alone would leave every refusal chunked.
func driveImportRejected(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r, _, _, _ := importRouter(t, importHeader+importTwitch, nil)
	return postImport(t, r, "text/plain", `[{"name":"SAPISID"}]`)
}

// driveImportDuringRefresh answers 409 through Task 3's ErrRefreshInProgress
// arm. The collision is reproduced through the REAL slot — refreshCmd is an
// unexported field of internal/cookies — by holding the first import inside its
// pre-write verification until the second has been answered.
func driveImportDuringRefresh(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r, _, _, svc := importRouter(t, importHeader+importYouTube+importTwitch, nil)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	gate := func(ctx context.Context) (bool, error) {
		first := false
		once.Do(func() { first = true })
		if !first {
			return true, nil
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return true, nil
	}
	svc.VerifyYouTubeAuth = gate
	svc.VerifyTwitchAuth = gate

	held := make(chan struct{})
	go func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("the holding import panicked: %v", p)
			}
			close(held)
		}()
		postImport(t, r, "text/plain", importPaste())
	}()

	select {
	case <-entered:
	case <-held:
		t.Fatal("the first import finished before it reached its pre-write verification — the " +
			"collision this row needs never happened")
	case <-time.After(60 * time.Second):
		t.Fatal("the first import never reached its pre-write verification")
	}

	rec := postImport(t, r, "text/plain", importPaste())
	close(release)
	<-held
	return rec
}

// driveImportNoService and driveFinishNoService answer 503 through each
// handler's `autoCookieSvc == nil` guard — the one exit on each that is taken
// before anything else runs.
func driveImportNoService(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	CookieRoutes(r, nil, nil, nil, nil)
	return postImport(t, r, "text/plain", importPaste())
}

func driveFinishNoService(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	CookieRoutes(r, nil, nil, nil, nil)
	return postFinish(t, r)
}

// driveFinishNoSetup answers 404 through the ErrNoSetupInProgress arm.
func driveFinishNoSetup(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	svc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), nopRouteLogger{})
	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)
	return postFinish(t, r)
}

func postFinish(t *testing.T, r chi.Router) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/finish", nil))
	return rec
}

// TestCookieWritersSetContentLength is COOKIES-4 / owner decision O-L. Both
// handlers Flush and then run a <=45 s auth re-check on the handler goroutine.
// Without Content-Length net/http uses chunked encoding and the terminating
// chunk is written only when the handler RETURNS, so the Flush released the
// headers and nothing else: fetch().json() awaited the body for the whole
// re-check, inside a dialog with a 60 s abort budget that also has to cover
// FinishSetup.
//
// The property is asserted on the RECORDED HEADER here rather than by timing:
// Content-Length is exactly what makes net/http send an identity body it can
// terminate without waiting for the handler, and a recorder can cover every
// exit cheaply. TestSizedCookieAnswerLandsBeforeTheBlockingRecheck pins the
// same property by execution over a real server and a real blocking re-check.
//
// SIX EXITS, chosen so each handler contributes a success (or its nearest
// reachable equivalent), an error switch arm and its no-service guard. A
// successful finish is not reachable from this package — FinishSetupDetailed
// needs a browser this test must never launch — so the import's
// ErrRefreshInProgress 409 takes that slot, exactly as the brief allows.
//
// Mutants:
//   - revert either handler to jsonResponse/jsonError -> that row's
//     Content-Length is empty.
//   - set Content-Length on the success exit only -> the four error rows fail,
//     and those are the exits the re-check matters most on (the jar-reload
//     error runs over a cookies.txt that has already been replaced).
//   - drop the trailing newline the sized writers keep -> nothing here fails,
//     which is why TestSizedCookieBodiesAreByteIdenticalToTheEncoder exists.
func TestCookieWritersSetContentLength(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  int
		drive func(*testing.T) *httptest.ResponseRecorder
	}{
		{"import/success", http.StatusOK, driveImportSuccess},
		{"import/rejected-paste", http.StatusUnprocessableEntity, driveImportRejected},
		{"import/refresh-in-progress", http.StatusConflict, driveImportDuringRefresh},
		{"import/no-service", http.StatusServiceUnavailable, driveImportNoService},
		{"finish/no-setup", http.StatusNotFound, driveFinishNoSetup},
		{"finish/no-service", http.StatusServiceUnavailable, driveFinishNoService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.drive(t)

			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d — this row no longer reaches the exit it names: %s",
					rec.Code, tc.want, rec.Body.String())
			}
			got := rec.Header().Get("Content-Length")
			if got == "" {
				t.Fatalf("no Content-Length on a %d from %s. net/http falls back to chunked "+
					"encoding and writes the terminating chunk only when the HANDLER returns, so "+
					"the deferred Flush releases the headers and fetch().json() — which awaits the "+
					"body — sits through the whole re-check", tc.want, tc.name)
			}
			if want := strconv.Itoa(rec.Body.Len()); got != want {
				t.Errorf("Content-Length = %q for a %d-byte body. A length that does not match the "+
					"bytes is worse than none: net/http truncates the body to it, or the client "+
					"blocks waiting for bytes that never come", got, rec.Body.Len())
			}
			if rec.Body.Len() >= gzipIdentityCeiling {
				t.Errorf("this exit's body is %d bytes, at or over internal/web's %d-byte gzip "+
					"threshold — CompressionMiddleware's startGzip deletes Content-Length above it "+
					"and the gzip trailer is written after the handler returns, which puts the "+
					"client back behind the re-check. See TestSizedCookieAnswersSurviveTheGzipWrapper",
					rec.Body.Len(), gzipIdentityCeiling)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("body is not JSON any more: %q", rec.Body.String())
			}
		})
	}
}

// TestSizedCookieBodiesAreByteIdenticalToTheEncoder is the differential O-L
// does not license: the fix is about a header, so not one byte of any exit's
// body may move.
//
// json.Encoder.Encode appends a newline and json.Marshal does not, so a sized
// writer built on Marshal alone silently drops the last byte of every body this
// endpoint has ever sent. Nothing would notice at the JSON layer, which is
// precisely why it is asserted here rather than left to a parse.
//
// Mutant: delete the `'\n'` from either sized writer -> the matching row's
// bytes differ from the encoder's by one.
func TestSizedCookieBodiesAreByteIdenticalToTheEncoder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(*testing.T) *httptest.ResponseRecorder
		want  func() *httptest.ResponseRecorder
	}{
		{
			name:  "error body",
			drive: driveFinishNoService,
			want: func() *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				jsonError(rec, "auto-cookie service not configured", http.StatusServiceUnavailable)
				return rec
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.drive(t).Body.String()
			want := tc.want().Body.String()
			if got != want {
				t.Errorf("the sized writer's body is %q where the unsized writer sent %q — the "+
					"Content-Length fix moved bytes it has no licence to move", got, want)
			}
		})
	}
}

// TestBothRecheckHandlersAnswerThroughTheSizedWriters is the exit sweep the
// wire table cannot be: several exits of both handlers have no fixture in this
// package at all — the jar-reload 500 needs a file that loads once and then
// does not, ErrCookieFileUnwritable needs an unexported package var stubbed,
// and every browser-profile arm needs a browser. Structure covers them.
//
// Both handlers run a deferred, BLOCKING re-check that holds the connection
// open, so the rule is per HANDLER and not per exit: every JSON any exit of
// these two writes must carry a length. A single surviving jsonResponse or
// jsonError is a single exit whose client waits out the re-check, and it is the
// likeliest regression — a new arm added by copying an older one.
//
// writeBrowserReadError and readCookieImportBody are deliberately not counted:
// they are CALLS from these handlers, shared with the refresh handler and with
// the request-shape refusals respectively, and every exit they own leaves
// result.Wrote false, so the deferred re-check returns without running and no
// body is held.
//
// Mutants:
//   - leave any one exit on jsonResponse/jsonError -> that handler's list of
//     unsized calls is non-empty and the failure names the function.
//   - delete a handler's success write -> the sized-response count is zero.
func TestBothRecheckHandlersAnswerThroughTheSizedWriters(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cookies.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cookies.go: %v", err)
	}

	for _, route := range []string{"/api/cookies/import", "/api/cookies/auto-setup/finish"} {
		t.Run(route, func(t *testing.T) {
			handler := routeHandlerLit(t, file, route)
			counts := map[string]int{}
			ast.Inspect(handler, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if fn, ok := call.Fun.(*ast.Ident); ok {
					counts[fn.Name]++
				}
				return true
			})

			for _, unsized := range []string{"jsonResponse", "jsonError"} {
				if counts[unsized] > 0 {
					t.Errorf("%s still answers %d exit(s) through %s. That writer streams through "+
						"a json.Encoder and sets no length, so net/http chunks the body and writes "+
						"the terminating chunk when the HANDLER returns — after the <=45 s re-check "+
						"this handler defers", route, counts[unsized], unsized)
				}
			}
			if counts["jsonResponseSized"] == 0 {
				t.Errorf("%s has no jsonResponseSized call — its success exit either went back to "+
					"the unsized writer or stopped answering at all", route)
			}
			if counts["jsonErrorSized"] == 0 {
				t.Errorf("%s has no jsonErrorSized call — its error exits either went back to the "+
					"unsized writer or stopped answering at all", route)
			}
		})
	}
}

// TestSizedCookieAnswerLandsBeforeTheBlockingRecheck is O-L pinned BY
// EXECUTION, over a real net/http server, a real client and a real blocking
// re-check on the handler goroutine.
//
// The re-check STAYS BLOCKING — that is the decision, not an accident: a
// goroutine would make the client fast and delete the property the AST
// call-site test protects (a detached pass whose result nothing waits for is a
// pass nothing can be sure ran). What changes is only that the body is complete
// on the wire before the re-check starts.
//
// The stand-in duration is injected through RefreshService.OnAuthChange, which
// the pass calls synchronously on this very goroutine. The unsized arm below is
// the mutant, run live rather than described: the same handler shape answering
// through jsonResponse, whose body the client cannot finish reading until the
// handler returns.
func TestSizedCookieAnswerLandsBeforeTheBlockingRecheck(t *testing.T) {
	t.Run("sized", func(t *testing.T) {
		r, rs := sizedWriterRouter(t, "")

		var once sync.Once
		entered := make(chan struct{})
		finished := make(chan time.Time, 1)
		rs.OnAuthChange = func(cookies.AuthStatus) {
			once.Do(func() {
				close(entered)
				time.Sleep(recheckStandIn)
				finished <- time.Now()
			})
		}

		srv := httptest.NewServer(r)
		defer srv.Close()

		start := time.Now()
		resp, err := http.Post(srv.URL+"/api/cookies/import", "text/plain", strings.NewReader(importPaste()))
		if err != nil {
			t.Fatalf("POST /api/cookies/import: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		bodyAt := time.Since(start)

		if resp.ContentLength < 0 {
			t.Errorf("the transport reports no Content-Length (transfer-encoding %v) — the answer "+
				"went out chunked, and a chunked body is only terminated when the handler returns",
				resp.TransferEncoding)
		}
		if len(resp.TransferEncoding) != 0 {
			t.Errorf("Transfer-Encoding = %v, want identity", resp.TransferEncoding)
		}
		if got := int64(len(body)); resp.ContentLength >= 0 && got != resp.ContentLength {
			t.Errorf("read %d body bytes against a Content-Length of %d", got, resp.ContentLength)
		}
		if bodyAt >= sizedAnswerArrivalBound {
			t.Errorf("the whole body took %v to arrive, over the %v bound — the client is still "+
				"being held by the re-check", bodyAt.Round(time.Millisecond), sizedAnswerArrivalBound)
		}

		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("the deferred re-check never ran, so this test proved nothing: the body it " +
				"timed was not racing anything")
		}
		var endedAt time.Time
		select {
		case endedAt = <-finished:
		case <-time.After(30 * time.Second):
			t.Fatal("the deferred re-check never finished")
		}
		if held := endedAt.Sub(start); held < recheckStandIn {
			t.Fatalf("the handler was inside its re-check for only %v — the fixture did not block "+
				"the handler goroutine, so the %v body arrival above is not evidence of anything",
				held.Round(time.Millisecond), bodyAt.Round(time.Millisecond))
		}
		t.Logf("body complete at %v; the handler stayed inside the re-check until %v",
			bodyAt.Round(time.Millisecond), endedAt.Sub(start).Round(time.Millisecond))
	})

	// THE MUTANT, executed. jsonResponse is the writer both handlers used
	// before O-L; everything else here is the production defer's shape.
	t.Run("unsized-is-the-bug", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			defer func() {
				if f, ok := rw.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(recheckStandIn)
			}()
			jsonResponse(rw, map[string]any{"success": true})
		}))
		defer srv.Close()

		start := time.Now()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		headersAt := time.Since(start)
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		bodyAt := time.Since(start)

		if bodyAt < recheckStandIn {
			t.Fatalf("the unsized answer's body arrived at %v, inside the %v block — net/http no "+
				"longer holds a flushed chunked body until the handler returns, so the mutant this "+
				"fix exists for is not reproducible and the assertions above guard nothing",
				bodyAt.Round(time.Millisecond), recheckStandIn)
		}
		t.Logf("unsized: headers at %v, body at %v (the %v block)",
			headersAt.Round(time.Millisecond), bodyAt.Round(time.Millisecond), recheckStandIn)
	})
}

// TestSizedCookieAnswersSurviveTheGzipWrapper is R3: the two handlers sit
// behind CompressionMiddleware in the real server (internal/web/server.go), and
// that wrapper is the one thing that can take the header away again.
//
// Below internal/web's 1024-byte threshold it cannot: a Flush before the
// threshold goes through commitPlain, which sends the buffered bytes identity
// and leaves Content-Length alone. At or above it, startGzip deletes the header
// by design — the length changes with compression — and the response is chunked
// again, with the gzip trailer written by the middleware's `defer gz.Close()`
// after the handler returns. So above the threshold the client is back behind
// the re-check.
//
// That is ACCEPTED rather than exempted, because no exit of either handler can
// reach it: the widest body they produce is a two-map import success at a few
// hundred bytes, and TestCookieWritersSetContentLength asserts every driven
// exit stays under the ceiling. The alternative — adding these two paths to
// shouldSkipCompression — would trade a real invariant for a skip nobody can
// see from the handler.
func TestSizedCookieAnswersSurviveTheGzipWrapper(t *testing.T) {
	t.Run("sub-threshold keeps the length", func(t *testing.T) {
		r, _, _, _ := importRouter(t, importHeader+importTwitch, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", strings.NewReader(importPaste()))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		web.CompressionMiddleware(r).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Fatalf("Content-Encoding = %q — this body is meant to be under the %d-byte threshold "+
				"and was compressed instead", enc, gzipIdentityCeiling)
		}
		got := rec.Header().Get("Content-Length")
		if want := strconv.Itoa(rec.Body.Len()); got != want {
			t.Errorf("Content-Length = %q through CompressionMiddleware for a %d-byte body — the "+
				"wrapper took the header away, and the import answer is chunked again behind the "+
				"re-check", got, rec.Body.Len())
		}
	})

	// The ceiling, stated by execution so the paragraph above is not a claim
	// nobody checked. Not a handler: no exit of either one can produce a body
	// this wide.
	t.Run("over-threshold loses it", func(t *testing.T) {
		wide := strings.Repeat("x", gzipIdentityCeiling*2)
		h := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			rw.Header().Set("Content-Length", strconv.Itoa(len(wide)))
			_, _ = io.WriteString(rw, wide)
		})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		web.CompressionMiddleware(h).ServeHTTP(rec, req)

		if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("Content-Encoding = %q for a %d-byte body, want gzip — internal/web's "+
				"threshold moved and this test's premise with it", enc, len(wide))
		}
		if got := rec.Header().Get("Content-Length"); got != "" {
			t.Errorf("Content-Length = %q survived startGzip. If that is now true, the ceiling "+
				"documented on gzipIdentityCeiling no longer exists and the accept-and-document "+
				"ruling for R3 should be revisited", got)
		}
	})
}

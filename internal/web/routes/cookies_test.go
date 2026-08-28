package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
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
		hideAt := strings.Index(js, pagehide)
		if hideAt < 0 {
			t.Errorf("%s fires its cancel beacon from something other than pagehide; "+
				"beforeunload can be cancelled by the user and would kill a setup they kept", mod.path)
		} else {
			// Bracketed to the handler body, not the file: `e.persisted`
			// appearing anywhere else would prove nothing about this beacon.
			hideBody := js[hideAt+len(pagehide):]
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
		at := strings.Index(js, skipHandler)
		if at < 0 {
			t.Fatalf("%s no longer wires a click handler on #cookie-skip-btn — "+
				"re-derive this test against the new shape", mod.path)
		}
		body := js[at+len(skipHandler):]
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

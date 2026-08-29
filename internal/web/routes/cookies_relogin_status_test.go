package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// reloginMapFromResponse extracts autoCookieReloginRequired from a decoded
// JSON body as a map[string]any, failing the test if it is missing or the
// wrong shape.
func reloginMapFromResponse(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	relogin, ok := body["autoCookieReloginRequired"].(map[string]any)
	if !ok {
		t.Fatalf("autoCookieReloginRequired missing or not an object: %#v", body["autoCookieReloginRequired"])
	}
	return relogin
}

// WHAT THESE TWO TESTS CAN AND CANNOT SAY, since Arc 8 Task 12a narrowed it.
//
// They used to raise a platform's flag with AutoCookieService.FlagManualRelogin
// and assert the raised value came back on the wire. That method was exported,
// documented, and called by NOTHING in production — it was written for the
// re-auth ingest path, which is unbuilt — so it was deleted as dead surface on
// a security-sensitive service, and these three tests were its only callers.
//
// From outside internal/cookies there is now no way to put a `true` in that
// map: every writer is a conclusion RefreshCookiesDetailed reaches after a real
// browser or profile pass, which the routes package has no fixture for (see the
// Stop() note on the auto-refresh test below for why even reaching the success
// branch takes a trick).
//
// So what survives here is: the key is PRESENT, and its value is exactly what
// the service reports. That still kills the mutant that matters — deleting the
// `response["autoCookieReloginRequired"] = …` line makes the key absent and
// reloginMapFromResponse fatal. What it no longer kills is a handler that
// replaced the service call with the nil-service fallback literal, because on a
// freshly constructed service the two are identical. That value's plumbing is
// pinned instead by TestStatusRouteWiresGetAutoCookieReloginNeededOntoTheWire
// below, which supplies its own non-default map.
//
// If the ingest path re-adds a writer, raise a flag here again and put the
// value assertions back.

// TestRecheckRouteCarriesReloginStatus drives H5's switched /recheck caller
// through the real HTTP handler and confirms the response still carries the
// needsManualRelogin map — now sourced from ReloginStatus rather than
// GetStatus, which this test cannot distinguish from the wire alone, which is
// the point: the response contract is unchanged even though the method backing
// it got cheaper.
func TestRecheckRouteCarriesReloginStatus(t *testing.T) {
	refreshSvc := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopRouteLogger{})
	autoSvc := cookies.NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})

	r := chi.NewRouter()
	CookieRoutes(r, refreshSvc, autoSvc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/recheck", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("recheck: status %d, want 200, body %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("recheck body is not JSON: %q", rec.Body.String())
	}
	assertReloginMatchesService(t, reloginMapFromResponse(t, body), autoSvc)
}

// assertReloginMatchesService checks a wire map against what the service
// reports, key for key and value for value. Both directions of the comparison
// matter: a missing key is a handler that stopped sending a platform, an extra
// one is a handler inventing a platform the frontend would render.
func assertReloginMatchesService(t *testing.T, wire map[string]any, svc *cookies.AutoCookieService) {
	t.Helper()
	want := svc.ReloginStatus()
	if len(wire) != len(want) {
		t.Fatalf("autoCookieReloginRequired = %v, want the service's %v — the key sets differ", wire, want)
	}
	for platform, flag := range want {
		if wire[platform] != flag {
			t.Errorf("autoCookieReloginRequired[%s] = %v, want %v (the service's own answer)",
				platform, wire[platform], flag)
		}
	}
}

// TestAutoRefreshRouteCarriesReloginStatus drives the /auto-refresh success
// path — required to reach the autoCookieReloginRequired line at all — via
// Stop(), not via a browser or profile fixture. RefreshCookiesDetailed
// returns (declined, nil error) the moment s.stopped is set, before it ever
// asks about a browser or a profile directory, so this reaches the
// handler's success branch without spawning a subprocess, touching a real
// browser, or needing a real Firefox/Chromium cookies.sqlite fixture (which
// only exists as unexported helpers in the cookies package's own tests).
func TestAutoRefreshRouteCarriesReloginStatus(t *testing.T) {
	refreshSvc := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopRouteLogger{})
	autoSvc := cookies.NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})
	autoSvc.Stop()

	r := chi.NewRouter()
	CookieRoutes(r, refreshSvc, autoSvc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-refresh on a stopped (declined, not errored) service: status %d, want 200, body %s",
			rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("auto-refresh body is not JSON: %q", rec.Body.String())
	}
	assertReloginMatchesService(t, reloginMapFromResponse(t, body), autoSvc)
}

// TestStatusRouteWiresGetAutoCookieReloginNeededOntoTheWire proves ONLY that
// StatusRoute plumbs whatever GetAutoCookieReloginNeeded returns onto the
// wire as autoCookieReloginRequired, unmangled.
//
// It does NOT guard cmd/moombox's own routes_wiring.go call site — that
// file wires GetAutoCookieReloginNeeded to a closure calling
// autoCookieSvc.ReloginStatus(), but this test supplies its OWN closure
// below rather than executing wireRoutes (which needs the whole runState
// service graph to call at all). Reverting routes_wiring.go's closure back
// to GetStatus().NeedsManualRelogin compiles fine and leaves this test
// green, since the value shape is identical either way — only the
// detection-scan cost differs, and nothing HTTP-observable can see that.
// cmd/moombox's TestGetAutoCookieReloginNeededCallsReloginStatus (an AST
// check, parsed the same way tui_wiring_cookiechords_test.go guards runTUI)
// is what actually guards that call site; this test's closure is
// deliberately named after what routes_wiring.go does today so a reader
// sees the intended shape, not as a claim that reverting it here would fail.
//
// The closure returns a LITERAL rather than a service's ReloginStatus(), and
// that is now the point rather than a shortcut: this test is the only one left
// that can put a raised flag on the wire at all (see the note above
// TestRecheckRouteCarriesReloginStatus), and a literal states the input
// directly instead of depending on a service reaching a state no exported call
// can put it in. The type is the real one, so the marshalled shape is the real
// shape.
func TestStatusRouteWiresGetAutoCookieReloginNeededOntoTheWire(t *testing.T) {
	raised := cookies.AutoCookieReloginRequired{"youtube": true, "twitch": false}

	r := chi.NewRouter()
	StatusRoute(r, &StatusRouteDeps{
		GetAutoCookieReloginNeeded: func() any {
			return raised
		},
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200, body %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status body is not JSON: %q", rec.Body.String())
	}
	relogin := reloginMapFromResponse(t, body)
	if relogin["youtube"] != true {
		t.Errorf("autoCookieReloginRequired[youtube] = %v, want true", relogin["youtube"])
	}
	if relogin["twitch"] != false {
		t.Errorf("autoCookieReloginRequired[twitch] = %v, want false", relogin["twitch"])
	}
}

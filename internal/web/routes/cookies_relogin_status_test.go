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

// TestRecheckRouteCarriesReloginStatus drives H5's switched /recheck caller
// through the real HTTP handler and confirms the response still carries the
// exact needsManualRelogin value — now sourced from ReloginStatus rather
// than GetStatus, which this test cannot distinguish from the wire alone,
// which is the point: the response contract is unchanged even though the
// method backing it got cheaper.
func TestRecheckRouteCarriesReloginStatus(t *testing.T) {
	refreshSvc := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopRouteLogger{})
	autoSvc := cookies.NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})
	autoSvc.FlagManualRelogin("youtube")

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
	relogin := reloginMapFromResponse(t, body)
	if relogin["youtube"] != true {
		t.Errorf("autoCookieReloginRequired[youtube] = %v, want true", relogin["youtube"])
	}
	if relogin["twitch"] != false {
		t.Errorf("autoCookieReloginRequired[twitch] = %v, want false", relogin["twitch"])
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
	autoSvc.FlagManualRelogin("twitch")
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
	relogin := reloginMapFromResponse(t, body)
	if relogin["twitch"] != true {
		t.Errorf("autoCookieReloginRequired[twitch] = %v, want true", relogin["twitch"])
	}
	if relogin["youtube"] != false {
		t.Errorf("autoCookieReloginRequired[youtube] = %v, want false", relogin["youtube"])
	}
}

// TestStatusRouteCarriesReloginStatus exercises cmd/moombox's
// GetAutoCookieReloginNeeded wiring pattern directly: StatusRoute is the same
// exported registration routes_wiring.go calls, and the closure below is
// byte-for-byte what that file wires GetAutoCookieReloginNeeded to. This is
// the closest an internal/web/routes test can get to routes_wiring.go's own
// switched caller, since cmd/moombox itself has no standalone HTTP harness.
func TestStatusRouteCarriesReloginStatus(t *testing.T) {
	autoSvc := cookies.NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})
	autoSvc.FlagManualRelogin("youtube")

	r := chi.NewRouter()
	StatusRoute(r, &StatusRouteDeps{
		GetAutoCookieReloginNeeded: func() any {
			return autoSvc.ReloginStatus()
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

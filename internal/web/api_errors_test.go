package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// apiErrorRequest sends method and target through the whole chain as the TUI
// would: from loopback, with the internal token, so CSRF passes a POST.
func apiErrorRequest(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:774"
	req.Header.Set("X-Internal-Token", s.InternalToken())
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	return rr
}

// wantAPIError checks the body is the {"error": msg} JSON the route handlers
// answer with.
func wantAPIError(t *testing.T, rr *httptest.ResponseRecorder, msg string) {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rr.Body, err)
	}
	if body["error"] != msg || len(body) != 1 {
		t.Errorf("body %v, want only error=%q", body, msg)
	}
}

// TestAnUnknownAPIPathAnswersJSON: an /api/ path no route claims answered
// net/http's plain-text "404 page not found", which a script's JSON error
// handling cannot parse. Paths outside /api/ still get the dashboard shell.
//
// Mutant: the NotFound handler calling http.NotFound for /api/ paths again.
func TestAnUnknownAPIPathAnswersJSON(t *testing.T) {
	s, _ := staticFixture(t, "")
	rr := apiErrorRequest(t, s, http.MethodGet, "/api/no-such-route")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rr.Code)
	}
	wantAPIError(t, rr, "not found")

	rr = apiErrorRequest(t, s, http.MethodGet, "/some/spa/route")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "<html>") {
		t.Errorf("a path outside /api/: %d %q, want the dashboard shell", rr.Code, rr.Body)
	}
}

// TestAWrongMethodOnAnAPIPathAnswersJSONAndAllow: a registered /api/ path
// asked with a method it does not take answered chi's 405 with no body. It
// now carries the handlers' JSON error body, and still the Allow header: a
// custom chi handler replaces the one that set it, so methodNotAllowed asks
// the router itself. A path outside /api/ keeps its empty 405.
//
// Mutants: no MethodNotAllowed handler registered (empty body); the handler
// without its Allow loop (no Allow header); the JSON body written for every
// path (the /ping case gains a body).
func TestAWrongMethodOnAnAPIPathAnswersJSONAndAllow(t *testing.T) {
	s, _ := staticFixture(t, "")
	ok := func(w http.ResponseWriter, r *http.Request) {}
	s.Router().Get("/api/things/{id}", ok)
	s.Router().Delete("/api/things/{id}", ok)

	rr := apiErrorRequest(t, s, http.MethodPost, "/api/things/42")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rr.Code)
	}
	wantAPIError(t, rr, "method not allowed")
	allow := rr.Header().Values("Allow")
	slices.Sort(allow)
	if !slices.Equal(allow, []string{http.MethodDelete, http.MethodGet}) {
		t.Errorf("Allow = %v, want DELETE and GET", allow)
	}

	rr = apiErrorRequest(t, s, http.MethodPost, "/ping")
	if rr.Code != http.StatusMethodNotAllowed || rr.Body.Len() != 0 {
		t.Errorf("POST /ping: %d %q, want the empty 405 it always was", rr.Code, rr.Body)
	}
	if got := rr.Header().Values("Allow"); !slices.Equal(got, []string{http.MethodGet}) {
		t.Errorf("POST /ping: Allow = %v, want GET", got)
	}
}

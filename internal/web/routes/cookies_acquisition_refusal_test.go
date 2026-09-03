package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestOptInRefusalAnswers422 pins the route's mapping for the new sentinel
// directly. TestRungThreeAgreesAcrossBothSurfaces reads the handler's cases
// from source and fails on a sentinel it does not know, but it cannot see a
// case that was DELETED — so the status is asserted over the wire here.
//
// Host-independent: the browser is gated rather than detected away, which is
// the only way a desktop reached the import path before this arc, and the
// profile tree exists so the pre-work missing-directory block does not answer
// first. The element carries Windows separators as a literal so the guard
// matches on Linux too (see existingDangerousProfileDir in internal/cookies).
func TestOptInRefusalAnswers422(t *testing.T) {
	dir := filepath.Join(t.TempDir(), `Mozilla\Firefox\Profiles\xxxxx.default-release`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := cookies.NewAutoCookieService(dir, filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})
	svc.BrowserLaunchAllowed = func() bool { return false }
	svc.AcquisitionMode = func() string { return cookies.AcquisitionAuto }

	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — the refusal carries the one sentence naming the setting, "+
			"and any other status either flattens it to \"cookie refresh failed\" or sends the "+
			"dashboard down the rung-3 fallback. Body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acquisition") {
		t.Errorf("the 422 body does not name cookies.acquisition: %s", rec.Body.String())
	}
}

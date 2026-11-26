package routes

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// A host with no display (Linux, owner ruling R4-7) answers the browser
// login's start with 424 and the sentence naming the browser-free routes,
// verbatim, as the other setup refusals are answered. Before, the default
// arm would have flattened it to "failed to start setup".
//
// Mutant it kills: the ErrNoDisplay arm dropped from the start route.
func TestAutoSetupStartRefusesAHostWithNoDisplay(t *testing.T) {
	t.Cleanup(cookies.SetHostLacksDisplayForTesting(true))
	svc := cookies.NewAutoCookieService(filepath.Join(t.TempDir(), "profile"),
		filepath.Join(t.TempDir(), "cookies.txt"), cookies.NewCookieJar(), nopRouteLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser.exe")
	svc.ConfiguredBrowserOverride = func() (string, string) { return unlaunchable, "chrome" }
	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/start", nil)))
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("status %d, want 424 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeErrorBody(t, rec)["error"]; got != cookies.ErrNoDisplay.Error() {
		t.Errorf("error %q, want the sentinel's sentence %q", got, cookies.ErrNoDisplay.Error())
	}
}

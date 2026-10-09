package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// inUseLockHost is a hostname no test machine has (".invalid" is reserved by
// RFC 2606), so the SingletonLock below always names ANOTHER machine.
const inUseLockHost = "moombox-other-host.invalid"

// heldProfileService builds an AutoCookieService whose profile directory
// carries a SingletonLock naming another machine's browser, with a YouTube
// sign-in in the jar (so a refresh takes the browser branch) and a
// "browser" at a path that does not exist (so nothing can launch if the lock
// rule regresses — the pass fails at exec instead of opening a window).
func heldProfileService(t *testing.T) *cookies.AutoCookieService {
	t.Helper()
	profileDir := t.TempDir()
	if err := os.Symlink(inUseLockHost+"-4242", filepath.Join(profileDir, "SingletonLock")); err != nil {
		t.Skipf("cannot create a symlink here (%v) — Chromium's SingletonLock is one", err)
	}
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfixture\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tfixture\n"
	if err := os.WriteFile(cookiePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	svc := cookies.NewAutoCookieService(profileDir, cookiePath, jar, nopRouteLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser")
	svc.ConfiguredBrowserOverride = func() (string, string) { return unlaunchable, "chrome" }
	t.Cleanup(svc.Stop)
	return svc
}

// TestProfileInUseReachesBothCookieRoutesVerbatim: a profile another
// machine's browser holds answers 409 with the service's own sentence — which
// names that machine — on the dashboard's refresh button and on the setup
// start, and the refresh's sentence is what auto-status then publishes as
// lastError, the field both UIs render.
//
// Mutants: delete either ErrProfileInUse arm in cookies.go — that route falls
// to its default 500 and a generic sentence that names no host.
func TestProfileInUseReachesBothCookieRoutesVerbatim(t *testing.T) {
	t.Run("auto-refresh", func(t *testing.T) {
		svc := heldProfileService(t)
		r := chi.NewRouter()
		CookieRoutes(r, nil, svc, nil, nil)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if got := decodeErrorBody(t, rec)["error"]; !strings.Contains(got, "in use by "+inUseLockHost) {
			t.Errorf("409 body %q does not name %q — the operator has nothing to go and close", got, inUseLockHost)
		}

		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodGet, "/api/cookies/auto-status", nil)))
		var status cookies.AutoCookieStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode auto-status: %v (body %q)", err, rec.Body.String())
		}
		if status.LastError == nil || !strings.Contains(*status.LastError, "in use by "+inUseLockHost) {
			t.Errorf("auto-status lastError = %v, want it to name %q", status.LastError, inUseLockHost)
		}
	})

	t.Run("auto-setup/start", func(t *testing.T) {
		svc := heldProfileService(t)
		r := chi.NewRouter()
		CookieRoutes(r, nil, svc, nil, nil)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/start", nil)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if got := decodeErrorBody(t, rec)["error"]; !strings.Contains(got, "in use by "+inUseLockHost) {
			t.Errorf("409 body %q does not name %q", got, inUseLockHost)
		}
	})
}

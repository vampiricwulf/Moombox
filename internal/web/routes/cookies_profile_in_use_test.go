package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
// rule regresses — the pass fails at exec instead of opening a window). It
// also returns the lock's full path, which the sentence names as the file to
// delete.
func heldProfileService(t *testing.T) (*cookies.AutoCookieService, string) {
	t.Helper()
	profileDir := t.TempDir()
	lock := filepath.Join(profileDir, "SingletonLock")
	if err := os.Symlink(inUseLockHost+"-4242", lock); err != nil {
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
	return svc, lock
}

// inUseWords are what the sentence must say wherever it reaches the operator:
// the machine holding the profile, and the lock to delete — by its full path —
// if no browser there is using it.
func inUseWords(lock string) []string {
	return []string{
		"in use by " + inUseLockHost,
		"delete " + strconv.Quote(lock) + " if no browser on " + inUseLockHost + " is using that profile",
	}
}

// checkInUseStatus reads auto-status as the dashboard does and asserts that its
// lastError carries the held profile's sentence.
func checkInUseStatus(t *testing.T, r http.Handler, lock string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodGet, "/api/cookies/auto-status", nil)))
	var status cookies.AutoCookieStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode auto-status: %v (body %q)", err, rec.Body.String())
	}
	if status.LastError == nil {
		t.Fatal("auto-status lastError is null — the held profile left no line for either UI to show")
	}
	for _, want := range inUseWords(lock) {
		if !strings.Contains(*status.LastError, want) {
			t.Errorf("auto-status lastError %q does not say %q", *status.LastError, want)
		}
	}
}

// TestProfileInUseReachesBothCookieRoutesVerbatim: a profile another
// machine's browser holds answers 409 with the service's own sentence — which
// names that machine, and the lock to delete if no browser there is using the
// profile — on the dashboard's refresh button and on the setup start, and each
// route's sentence is what auto-status then publishes as lastError, the field
// both UIs render.
//
// The refresh's 409 also carries cause "profile-in-use": the same status
// answers a locked cookie DB, and only the held profile is a skip, which the
// dashboard toasts as a warning rather than a failure.
//
// Mutants: delete either ErrProfileInUse arm in cookies.go — that route falls
// to its default 500 and a generic sentence that names no host; hand
// singletonLockHolder anything but the lock's own path in removeStaleLock —
// no body or lastError carries it; the refresh arm back on jsonError — its
// body carries no cause.
func TestProfileInUseReachesBothCookieRoutesVerbatim(t *testing.T) {
	t.Run("auto-refresh", func(t *testing.T) {
		svc, lock := heldProfileService(t)
		r := chi.NewRouter()
		CookieRoutes(r, nil, svc, nil, nil)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		body := decodeErrorBody(t, rec)
		got := body["error"]
		for _, want := range inUseWords(lock) {
			if !strings.Contains(got, want) {
				t.Errorf("409 body %q does not say %q — the operator has nothing to go and close, or to delete", got, want)
			}
		}
		if body["cause"] != causeProfileInUse {
			t.Errorf("409 cause = %q, want %q — without it the toast cannot tell this skip from a locked cookie DB",
				body["cause"], causeProfileInUse)
		}
		checkInUseStatus(t, r, lock)
	})

	t.Run("auto-setup/start", func(t *testing.T) {
		svc, lock := heldProfileService(t)
		r := chi.NewRouter()
		CookieRoutes(r, nil, svc, nil, nil)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, fromTheHost(httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/start", nil)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		got := decodeErrorBody(t, rec)["error"]
		for _, want := range inUseWords(lock) {
			if !strings.Contains(got, want) {
				t.Errorf("409 body %q does not say %q", got, want)
			}
		}
		checkInUseStatus(t, r, lock)
	})
}

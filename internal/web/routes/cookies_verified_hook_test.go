package routes

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCookieImportRouteReportsItsVerdictsToTheHost: an import that replaced
// cookies.txt ends by telling the host what it verified, per platform, so
// cmd/moombox can resume the jobs parked in COOKIES? on a platform that came
// back verified (owner ruling R2-C2). An import that wrote nothing reports
// nothing: there is no new credential for a parked job to retry with.
//
// Mutants: the cookiesVerified call dropped from the import's deferred
// re-check — the first half sees no call; the call moved ahead of the Wrote
// gate — the refused paste reports.
func TestCookieImportRouteReportsItsVerdictsToTheHost(t *testing.T) {
	type call struct {
		youtube, twitch cookies.RefreshVerdict
		gesture         string
	}
	build := func(t *testing.T) (chi.Router, *[]call) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "cookies.txt")
		if err := os.WriteFile(path, []byte(importHeader+importTwitch), 0o600); err != nil {
			t.Fatal(err)
		}
		log := &recordingLogger{}
		jar := cookies.NewCookieJar()
		if err := jar.Load(path); err != nil {
			t.Fatal(err)
		}
		svc := cookies.NewAutoCookieService(dir, path, jar, log)
		svc.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
		svc.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		var calls []call
		r := chi.NewRouter()
		// An empty jar for the refresh service, so the deferred CheckNow
		// returns before any network call (importRouter's arrangement).
		CookieRoutes(r, cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log), svc, nil, nil,
			func(youtube, twitch cookies.RefreshVerdict, gesture string) {
				calls = append(calls, call{youtube, twitch, gesture})
			})
		return r, &calls
	}

	t.Run("a written import", func(t *testing.T) {
		r, calls := build(t)
		if rec := postImport(t, r, "text/plain", importPaste()); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if len(*calls) != 1 {
			t.Fatalf("%d reports to the host, want 1", len(*calls))
		}
		got := (*calls)[0]
		if got.youtube != cookies.RefreshOK || got.twitch == cookies.RefreshOK {
			t.Errorf("reported youtube=%v twitch=%v, want YouTube verified and Twitch not", got.youtube, got.twitch)
		}
		if got.gesture != "a cookie file import" {
			t.Errorf("gesture = %q", got.gesture)
		}
	})

	t.Run("a refused paste", func(t *testing.T) {
		r, calls := build(t)
		if rec := postImport(t, r, "text/plain", importHeader+importSignedOut); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422", rec.Code)
		}
		if len(*calls) != 0 {
			t.Errorf("a paste that wrote nothing reported %+v", *calls)
		}
	})
}

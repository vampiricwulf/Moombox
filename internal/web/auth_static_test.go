package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// authStaticFixture builds a server that REQUIRES authentication and mounts
// AuthMiddleware in front of the REAL embedded assets, exactly as
// cmd/moombox/services.go and ws_wiring.go do (Use first, MountStaticFiles
// after). auth_test.go holds no HTTP fixture at all — it exercises
// AuthService — so this one is shaped like websocket_origin_test.go's
// wsOriginFixture instead.
//
// The real embedded FS, not a fstest.MapFS: half of what these tests assert is
// that /boot-theme.js and /login.js are files that actually ship.
func authStaticFixture(t *testing.T) *Server {
	t.Helper()
	store := config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{
			NetworkAccess: "external",
			// Any non-empty hash: IsAuthRequired only tests emptiness, and
			// no test here presents a session for it to verify.
			PasswordHash: "scrypt:0011:2233",
		},
	}, "")
	s := NewServer(store, testWSLogger{})
	s.Router().Use(s.AuthMiddleware)
	staticFS, err := fs.Sub(webassets.PublicFS, "public")
	if err != nil {
		t.Fatalf("sub embedded public FS: %v", err)
	}
	s.MountStaticFiles(staticFS)
	// chi builds its middleware chain only once a ROUTE is registered: with
	// none, Mux.ServeHTTP short-circuits straight to the NotFound handler
	// (mux.go:63-68) and every middleware — AuthMiddleware included — is
	// bypassed, so a fixture without one silently tests nothing. Production
	// registers dozens; this stands in for them, and is the real /ping.
	s.Router().Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("pong"))
	})
	return s
}

// getUnauthenticated drives one GET from an EXTERNAL peer carrying no session
// cookie — the visitor who has nothing but the login page.
func getUnauthenticated(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "203.0.113.5:1234"
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	return rr
}

// TestLoginPageAssetsAreReachableUnauthenticated is the trap WEB-10 sets.
//
// AuthMiddleware answers every non-allow-listed path with login.html itself.
// Moving the login page's script into /login.js therefore breaks the login page
// for external users unless the two new files join the allow-list: the browser
// would fetch /login.js, receive an HTML document, and the form would never
// wire up — for exactly the users who cannot get in any other way. The theme
// bootstrap is the same story one notch quieter: the login page would render
// with the default dark theme whatever the visitor chose.
//
// THE MUTANT: leave the allow-list alone. Both requests come back as text/html.
func TestLoginPageAssetsAreReachableUnauthenticated(t *testing.T) {
	s := authStaticFixture(t)

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/boot-theme.js", "moombox-theme"},
		{"/login.js", "function initLogin"},
	} {
		rec := getUnauthenticated(t, s, tc.path)
		if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Errorf("%s was answered with %q — an unauthenticated visitor gets the login page instead "+
				"of the login page's own script", tc.path, ct)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s: want 200 for an unauthenticated visitor, got %d", tc.path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, tc.want) {
			t.Errorf("%s: served body does not contain %q — it is not the script it should be", tc.path, tc.want)
		}
	}
}

// TestUnauthenticatedAllowListContents enumerates the allow-list itself,
// through the middleware rather than by reading source: exactly these eight
// paths reach the handler behind it, and nothing else does.
//
// The sentinel handler is what makes "reached" observable — a blocked request
// never gets there, it gets login.html (or 401 JSON under /api/).
//
// THE MUTANTS: drop /login.js or /boot-theme.js (their rows stop reaching the
// sentinel), or widen the list with, say, /app.js (that blocked row starts
// reaching it, which is the dashboard served to a visitor with no password).
func TestUnauthenticatedAllowListContents(t *testing.T) {
	s := authStaticFixture(t)
	sentinel := s.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("PASSED"))
	}))

	probe := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.5:1234"
		rr := httptest.NewRecorder()
		sentinel.ServeHTTP(rr, req)
		return rr.Body.String()
	}

	// The five that predate WEB-10, plus /login.html, plus the two it adds.
	for _, path := range []string{
		"/api/auth/login", "/api/auth/status",
		"/ping", "/minter_cache",
		"/favicon.svg", "/login.html",
		"/boot-theme.js", "/login.js",
	} {
		if got := probe(path); got != "PASSED" {
			t.Errorf("%s is NOT on the unauthenticated allow-list — the middleware answered it itself", path)
		}
	}

	// Everything else still stops here. /app.js and / are the dashboard.
	for _, path := range []string{
		"/", "/index.html", "/app.js", "/moombox.css",
		"/api/jobs", "/api/config", "/modules/settings.js",
	} {
		if got := probe(path); got == "PASSED" {
			t.Errorf("%s reached the handler unauthenticated — the allow-list was widened past the "+
				"login page's own assets", path)
		}
	}
}

// TestLoginPageAssetsRevalidate pins the caching policy for the two new files.
//
// Neither carries a ?v= cache-buster: MountStaticFiles substitutes one into
// /moombox.css and /app.js in the dashboard shell only, and login.html is
// served raw, so a third token would be inconsistent between the two pages.
// staticCacheHeaders is what makes that correct — no-cache plus an ETag, so a
// repeat visit revalidates in 304 bytes instead of re-downloading, and a new
// build is picked up immediately.
//
// THE MUTANT: serve either file outside staticCacheHeaders (or mark it
// immutable). A stale boot-theme.js survives an update for a year.
func TestLoginPageAssetsRevalidate(t *testing.T) {
	s := authStaticFixture(t)

	for _, path := range []string{"/boot-theme.js", "/login.js"} {
		rec := getUnauthenticated(t, s, path)
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want %q", path, cc, "no-cache")
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag — every repeat visit re-downloads the whole file", path)
		}

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.5:1234"
		req.Header.Set("If-None-Match", etag)
		rr := httptest.NewRecorder()
		s.Router().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotModified {
			t.Errorf("%s: conditional GET answered %d, want 304", path, rr.Code)
		}
	}
}

package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// sameOriginAssetRe finds every same-origin subresource URL in a page.
var sameOriginAssetRe = regexp.MustCompile(`(?i)\s(?:src|href)="(/[^"]*)"`)

// TestLoginPageSubresourcesAreAllowListed: every same-origin asset login.html
// fetches must reach the static handler unauthenticated, or the login page is
// dead for the only visitors who need it. TestLoginPageAssetsAreReachable-
// Unauthenticated above names today's two files; this one follows the PAGE, so
// a third one added later cannot be forgotten.
//
// It also closes the rename case: the allow-list entries are exact strings and
// the handler behind them ends in the SPA fallback, so an allow-listed path
// that stops being a file answers an unauthenticated visitor with the
// dashboard shell. A renamed boot-theme.js makes login.html's own
// src="/boot-theme.js" answer text/html, which is what this rejects.
//
// THE MUTANT: add <script src="/whatever.js"> to login.html and leave
// AuthMiddleware's allow-list alone — the browser is handed login.html for it.
func TestLoginPageSubresourcesAreAllowListed(t *testing.T) {
	s := authStaticFixture(t)
	login, err := fs.ReadFile(webassets.PublicFS, "public/login.html")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, m := range sameOriginAssetRe.FindAllStringSubmatch(string(login), -1) {
		found++
		if ct := getUnauthenticated(t, s, m[1]).Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Errorf("login.html fetches %s but an unauthenticated visitor is answered %q for it — "+
				"add it to AuthMiddleware's allow-list", m[1], ct)
		}
	}
	if found == 0 {
		t.Error("no same-origin subresource found in login.html — the scan matched nothing and this " +
			"test is vacuous")
	}
}

// cdnTagRe is one <link>/<script> element, opening tag only.
var cdnTagRe = regexp.MustCompile(`(?is)<(?:link|script)\b[^>]*>`)

// TestCDNSubresourcesCarryIntegrity — both pages pull Shoelace from
// cdn.jsdelivr.net, which is the one origin script-src and style-src admit
// besides 'self'. Subresource Integrity is what keeps that concession narrow:
// without it a compromised or MITM'd CDN response executes with the page's
// full authority, and login.html is the page served to UNAUTHENTICATED
// external visitors.
//
// login.html's light-theme stylesheet shipped without the attribute although
// index.html's identical URL carried it and the file's own comment claimed the
// hashes matched.
//
// THE MUTANT: drop any integrity= attribute from either page — that tag is
// named here.
func TestCDNSubresourcesCarryIntegrity(t *testing.T) {
	for _, page := range []string{"public/index.html", "public/login.html"} {
		body, err := fs.ReadFile(webassets.PublicFS, page)
		if err != nil {
			t.Fatal(err)
		}
		pinned := 0
		for _, tag := range cdnTagRe.FindAllString(string(body), -1) {
			if !strings.Contains(tag, "cdn.jsdelivr.net") {
				continue
			}
			pinned++
			if !strings.Contains(tag, "integrity=\"sha") {
				t.Errorf("%s: a cdn.jsdelivr.net subresource carries no integrity hash:\n%s", page, tag)
			}
		}
		if pinned == 0 {
			t.Errorf("%s: no cdn.jsdelivr.net tag matched — the scan is vacuous", page)
		}
	}
}

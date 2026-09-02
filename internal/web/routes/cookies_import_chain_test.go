package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// The chain tests. Everything else in this package mounts CookieRoutes on a
// bare chi.NewRouter(), which exercises the handler and NOTHING around it — so
// auth and CSRF could both be removed for this path with every test in
// ./internal/web/... and ./cmd/... still green. That was run and confirmed
// (review MS-3). These build the production chain instead.
//
// WHY THIS ENDPOINT AND NOT EVERY POST. The two content types the import
// accepts — text/plain and multipart/form-data — are exactly the two CORS-simple
// types an HTML form can post cross-origin with no preflight. Every other
// mutating endpoint here takes JSON, and the JSON content type is itself a
// second barrier a form cannot clear; here that barrier is absent by design, so
// CSRFMiddleware's Origin check is the only thing standing between a
// cross-origin form submission and an overwrite of the credential file.
//
// EVERY REFUSAL ALSO ASSERTS THE FILE IS BYTE-IDENTICAL. A status-code-only
// test passes just as happily on a handler that wrote first and refused second,
// which is the failure that actually matters here: the point of the chain is
// that the body is never read, not that the answer is red.

// importChainSeed is what every fixture below writes to cookies.txt first, so
// "the file did not change" is a claim about real content rather than about an
// absent file.
const importChainSeed = importHeader + importTwitch

// importChainPasswordHash stands in for a real scrypt hash.
//
// Nothing on this path ever VERIFIES it: AuthMiddleware reads the hash only
// through IsAuthRequired, which asks whether it is non-empty, and then
// validates the SESSION cookie. /api/auth/login is not exercised here. So a
// placeholder is exactly as faithful as a real one and skips a scrypt KDF per
// fixture — and, being obviously fake, it cannot be mistaken for a credential.
const importChainPasswordHash = "scrypt:0000000000000000:0000000000000000"

// importChain is one fully-wired Moombox HTTP chain with the cookie routes on
// it, reproducing the production wiring: cmd/moombox/services.go installs
// AuthMiddleware on webServer.Router(), and cmd/moombox/routes_wiring.go
// registers CookieRoutes on that same router. web.NewServer itself installs
// RequestID, Drain, Recovery, CORS, SecurityHeaders, CSRF, IPGate, MaxBodySize
// and Compression, so those come along for free — and MaxBodySize is what the
// large-import subtest below is really about.
type importChain struct {
	handler    http.Handler
	cookiePath string
	session    string
	remoteAddr string
	origin     string
}

// newImportChain builds the chain for one network_access policy.
//
// networkAccess and remoteAddr travel together and cannot be chosen
// independently, which is worth stating because it constrains what these tests
// can assert:
//
//   - AuthMiddleware WAIVES loopback and private IPs, and waives entirely when
//     no password is set. Driving it therefore needs a PUBLIC RemoteAddr plus a
//     password hash.
//   - IPGateMiddleware then only lets a public address through when
//     network_access is "external" or "public".
//   - But isAllowedOrigin returns true for EVERY origin under those two
//     policies.
//
// So on a fixture that can exercise auth, CSRF's invalid-origin arm is
// unreachable and its missing-origin arm is the one that fires; the
// invalid-origin arm needs a "lan" install, where AuthMiddleware waives. Both
// are driven below, on the fixture each is reachable from.
func newImportChain(t *testing.T, networkAccess, remoteAddr, origin string) *importChain {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(importChainSeed), 0o600); err != nil {
		t.Fatalf("seed cookies.txt: %v", err)
	}

	log := &recordingLogger{}

	cfg := config.Defaults()
	cfg.Network.NetworkAccess = networkAccess
	cfg.Network.PasswordHash = importChainPasswordHash
	store := config.NewStore(cfg, "")

	authSvc := web.NewAuthService() // deliberately not Start()ed: no cleanup goroutine is wanted here
	srv := web.NewServer(store, log)
	srv.SetAuth(authSvc)

	r := srv.Router()
	r.Use(srv.AuthMiddleware)

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	svc := cookies.NewAutoCookieService(dir, path, jar, log)
	svc.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	svc.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	// The RefreshService's jar is EMPTY, so the handler's deferred CheckNow
	// short-circuits before any network call.
	CookieRoutes(r, cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log), svc, nil, nil)

	session, err := authSvc.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &importChain{handler: r, cookiePath: path, session: session, remoteAddr: remoteAddr, origin: origin}
}

// post drives one request through the whole chain. withSession and origin are
// the two things each subtest varies; everything else is held fixed so a
// difference in the answer can only come from the control under test.
func (c *importChain) post(t *testing.T, body string, withSession bool, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", strings.NewReader(body))
	req.RemoteAddr = c.remoteAddr
	req.Header.Set("Content-Type", "text/plain")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if withSession {
		req.AddCookie(&http.Cookie{Name: "moombox_session", Value: c.session})
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func (c *importChain) read(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(c.cookiePath)
	if err != nil {
		t.Fatalf("read cookies.txt: %v", err)
	}
	return string(data)
}

// TestCookieImportChainRefusesUnauthenticatedAndCrossOrigin drives the real
// middleware stack, because "chain-wide by construction" is a property no test
// holds and the next refactor can therefore drop.
//
// THE MUTATIONS, each confirmed to survive the rest of the suite:
//
//   - add `p == "/api/cookies/import"` to AuthMiddleware's unauthenticated-path
//     list (internal/web/server.go, beside "/api/auth/login") — the
//     no-session subtest fails, and an unauthenticated stranger can replace the
//     credential file of a public install.
//   - add the same path to CSRFMiddleware's exempt list (internal/web/
//     middleware.go, beside "/get_pot") — both origin subtests fail, and a
//     cross-origin form post overwrites cookies.txt.
//   - register CookieRoutes on a router that does not carry AuthMiddleware —
//     the no-session subtest fails.
//
// The POSITIVE CONTROL is not optional. Without it a fixture that 401s for an
// unrelated reason — a mis-built store, a typo'd path, a route that no longer
// exists — reads as a clean pass, which is exactly how a test of this shape
// goes vacuous.
func TestCookieImportChainRefusesUnauthenticatedAndCrossOrigin(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737): a public address by every
	// predicate here, so it clears neither of AuthMiddleware's two waivers.
	const publicAddr = "203.0.113.7:1234"
	const goodOrigin = "https://moombox.example"

	t.Run("no session is refused before a byte is written", func(t *testing.T) {
		c := newImportChain(t, "public", publicAddr, goodOrigin)
		before := c.read(t)

		rec := c.post(t, importPaste(), false, goodOrigin)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401 — AuthMiddleware is not covering this path: %s",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Authentication required") {
			t.Errorf("body %q does not carry AuthMiddleware's own answer, so the 401 may be "+
				"coming from somewhere else entirely", rec.Body.String())
		}
		if got := c.read(t); got != before {
			t.Error("cookies.txt changed on an UNAUTHENTICATED request — the refusal happened after " +
				"the body was read and written, which is the failure a status-code-only test misses")
		}
	})

	t.Run("a request with no Origin is refused", func(t *testing.T) {
		// The CSRF arm that IS reachable on an auth-driving fixture: under
		// "public", isAllowedOrigin accepts every origin, so only the
		// missing-origin check can fire. It is the check that stops a
		// non-browser client replaying a stolen session cookie.
		c := newImportChain(t, "public", publicAddr, goodOrigin)
		before := c.read(t)

		rec := c.post(t, importPaste(), true, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — CSRFMiddleware is not covering this path: %s",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Forbidden: missing origin") {
			t.Errorf("body %q does not carry CSRFMiddleware's own answer", rec.Body.String())
		}
		if got := c.read(t); got != before {
			t.Error("cookies.txt changed on a request with no Origin")
		}
	})

	t.Run("a cross-origin form post is refused", func(t *testing.T) {
		// The invalid-origin arm, on the install where it is reachable. A "lan"
		// install with a private client is Moombox's Docker default;
		// AuthMiddleware waives a private IP, so here the Origin check is the
		// ONLY barrier — which is precisely the case this endpoint's two
		// CORS-simple content types create.
		c := newImportChain(t, "lan", "192.168.1.50:1234", "https://192.168.1.50")
		before := c.read(t)

		rec := c.post(t, importPaste(), true, "https://evil.example")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — a cross-origin form post reached the handler: %s",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Forbidden: invalid origin") {
			t.Errorf("body %q does not carry CSRFMiddleware's invalid-origin answer", rec.Body.String())
		}
		if got := c.read(t); got != before {
			t.Error("cookies.txt was overwritten by a cross-origin form post")
		}
	})

	t.Run("the positive control: session and origin are accepted", func(t *testing.T) {
		c := newImportChain(t, "public", publicAddr, goodOrigin)

		rec := c.post(t, importPaste(), true, goodOrigin)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 — the two refusals above prove nothing if a WELL-FORMED "+
				"request cannot get through this fixture either: %s", rec.Code, rec.Body.String())
		}
		onDisk := c.read(t)
		if !strings.Contains(onDisk, "fake-sapisid-aaaa") {
			t.Error("the pasted row never reached the file through the real chain")
		}
		if !strings.Contains(onDisk, "fake-authtoken-aaaa") {
			t.Error("the seeded Twitch row is gone — the merge did not survive the chain")
		}
	})
}

// TestCookieImportChainAcceptsALargeButLegitimateExport pins the ordering
// between this endpoint's 512 KiB cap and the chain-wide MaxBodySize, which was
// otherwise asserted only in a comment.
//
// THE MUTATION: drop maxCompressBodySize (internal/web/server.go) below
// maxCookieImportBytes — for example to 1<<18. MaxBytesReader wrappers NEST, so
// the OUTER, smaller limit then errors first; and because errors.As matches the
// outer *http.MaxBytesError just as happily, the endpoint answers 413 with its
// OWN sentence naming 512 KiB — a limit the install does not have — while
// refusing an export it would otherwise take. Silent under every other test in
// the tree (review MS-2).
//
// Behavioural rather than arithmetic on purpose: maxCompressBodySize is
// unexported and internal/web cannot import internal/web/routes, so the two
// constants cannot be compared where the comparison belongs. This pins the
// consequence instead, through the actual middleware stack.
//
// ~400 KB: comfortably above 1<<18 (262,144) so the mutation bites, and
// comfortably below 512 KiB so the correct configuration accepts it. The
// padding is COMMENT lines, which cleanNetscapeRows drops and mergeCookieFiles
// never sees, so the import that lands is the same one the other tests assert.
func TestCookieImportChainAcceptsALargeButLegitimateExport(t *testing.T) {
	const publicAddr = "203.0.113.7:1234"
	const goodOrigin = "https://moombox.example"

	c := newImportChain(t, "public", publicAddr, goodOrigin)
	body := importPaste() + strings.Repeat("# padding\n", 40000)
	if len(body) <= 1<<18 || len(body) >= maxCookieImportBytes {
		t.Fatalf("the fixture body is %d bytes; it must sit strictly between 1<<18 and the "+
			"endpoint's own %d-byte cap or it cannot tell the two limits apart",
			len(body), maxCookieImportBytes)
	}

	rec := c.post(t, body, true, goodOrigin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — a %d-byte export is inside this endpoint's %d-byte cap, so "+
			"a refusal here means the chain-wide MaxBodySize has dropped below it and the operator "+
			"is being told about a limit their install does not have: %s",
			rec.Code, len(body), maxCookieImportBytes, rec.Body.String())
	}
	if !strings.Contains(c.read(t), "fake-sapisid-aaaa") {
		t.Error("the large export answered 200 but its rows never reached the file")
	}
}

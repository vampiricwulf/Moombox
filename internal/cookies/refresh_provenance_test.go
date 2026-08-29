package cookies

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// redirectChain stands up ONE loopback server addressed under two hostnames
// and wires it as a three-hop chain that ENDS WHERE IT STARTED:
//
//	http://127.0.0.1:P/       → 302 → http://<wallHost>:P/wall
//	http://<wallHost>:P/wall  → 302 → http://127.0.0.1:P/back
//	http://127.0.0.1:P/back   → status + body
//
// The bounce back to the starting hostname is the whole point. A terminal-host
// comparison passes on it, so the only thing left that can show the round trip
// went elsewhere in between is whether the answering request still carried the
// credential header — which is exactly the rule under test.
//
// wallHost must be a HOSTNAME distinct from 127.0.0.1 for the strip to happen.
// Go's decision is hostname-based (client.go:688 →
// shouldCopyHeaderOnRedirect → isDomainOrSubdomain), so a second PORT strips
// nothing and a test built on one would prove nothing; "localhost" and
// "127.0.0.1" are one machine under two names, the smallest thing that trips
// the real rule. Passing "127.0.0.1" gives the control: the same three hops
// with the credential intact.
//
// One listener, bound before the handler closes over its address, so the URLs
// the handler reads are written before any request goroutine exists.
//
// Returns the server (for pointYouTubeGuideAt / pointTwitchValidateAt), a count
// of handled requests — 3 means the whole chain ran, and anything less means
// the chain broke rather than that the rule fired — and whether the FINAL hop
// still carried credHeader.
func redirectChain(t *testing.T, wallHost, credHeader string, status int, body string) (*httptest.Server, *atomic.Int64, *atomic.Bool) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		l.Close()
		t.Fatalf("split %q: %v", l.Addr(), err)
	}

	home := "http://" + net.JoinHostPort("127.0.0.1", port)
	wall := "http://" + net.JoinHostPort(wallHost, port)

	var handled atomic.Int64
	var carried atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		switch r.URL.Path {
		case "/wall":
			http.Redirect(w, r, home+"/back", http.StatusFound)
		case "/back":
			carried.Store(r.Header.Get(credHeader) != "")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			http.Redirect(w, r, wall+"/wall", http.StatusFound)
		}
	})

	srv := httptest.NewUnstartedServer(h)
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &handled, &carried
}

// requireBouncedChain asserts the two premises every bounce test rests on: the
// chain actually completed, and Go actually took the credential away. Without
// both, "the check returned an error" is downstream of a junction — a dead
// "localhost", a refused dial or a redirect loop would produce the same error
// while proving nothing about the rule.
func requireBouncedChain(t *testing.T, hops *atomic.Int64, carried *atomic.Bool, credHeader string) {
	t.Helper()
	if got := hops.Load(); got != 3 {
		t.Fatalf("premise broken: the chain handled %d request(s), want 3 — it did not run to completion, so the error under test is not the one being asserted", got)
	}
	if carried.Load() {
		t.Fatalf("premise broken: the final hop still carried the %s header — Go did not strip it, so this chain cannot exercise the rule", credHeader)
	}
}

// requireIntactChain is the control's premise: three hops again, credential
// still attached. It is what makes the control a control — the body, the
// status and the hop count are identical, and only the hostname of the middle
// hop differs.
func requireIntactChain(t *testing.T, hops *atomic.Int64, carried *atomic.Bool, credHeader string) {
	t.Helper()
	if got := hops.Load(); got != 3 {
		t.Fatalf("premise broken: the chain handled %d request(s), want 3", got)
	}
	if !carried.Load() {
		t.Fatalf("premise broken: the final hop lost the %s header on a SAME-hostname chain — the control is not controlling for anything", credHeader)
	}
}

// --- YouTube: a followed redirect never presents as non-200 ---

// TestGuideBouncedThroughAnotherHostIsInconclusive is the false-alarm this
// closes. Task 1 made a non-200 inconclusive, but a redirect chain is answered
// 200: an intercepting intermediary (captive portal, transparent or corporate
// proxy — http.ProxyFromEnvironment is on the shared transport) can bounce the
// guide POST off another host and hand back a page that carries neither
// `"logged_in":"1"` nor `"loggedIn":true`.
//
// Before the provenance rule that fell through to `return false, nil` — a
// CONCLUSIVE "not authenticated" that shouldFireRecovery acts on, sending an
// operator off to re-export credentials that were never wrong. In a container
// the remedy it names may not even be reachable.
//
// Go strips the Cookie header on the first cross-host hop and the decision is
// sticky, so the body is an anonymous fetch by construction. The answer has to
// be inconclusive: an error, not a verdict.
func TestGuideBouncedThroughAnotherHostIsInconclusive(t *testing.T) {
	srv, hops, carried := redirectChain(t, "localhost", "Cookie", http.StatusOK, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())

	requireBouncedChain(t, hops, carried, "Cookie")
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil — a page fetched without our cookies is not a verdict on our session")
	}
	// Same rule as TestTwitchValidateErrorNamesOnlyTheStatus: this string may
	// never carry cookie material. For where it actually goes, see
	// errGuideLoginMarkerUnreadable's doc comment — not the UIs.
	if strings.Contains(err.Error(), "sapisid-value") || strings.Contains(err.Error(), "login-info-value") {
		t.Errorf("the error carries cookie material: %q", err)
	}
}

// TestGuideSameHostRedirectStillReadsTheVerdict is the control. Identical body,
// identical status, identical hop count — only the middle hop's HOSTNAME
// changes, so the cookies survive and the page really is an observation of this
// session. It must still read as the conclusive logged-out verdict it is.
//
// Without this the rule above could be satisfied by refusing every redirect, or
// by refusing this body outright, and the suite could not tell the difference.
func TestGuideSameHostRedirectStillReadsTheVerdict(t *testing.T) {
	srv, hops, carried := redirectChain(t, "127.0.0.1", "Cookie", http.StatusOK, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())

	requireIntactChain(t, hops, carried, "Cookie")
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err != nil {
		t.Errorf("err = %v, want nil — our cookies fetched this page, so logged-out is a real verdict", err)
	}
}

// TestCheckYouTubeAuthBouncedThroughAnotherHostIsInconclusive is the same rule
// on the other entry point. checkYouTubeAuth is what AutoCookieService's
// VerifyYouTubeAuth calls, so its answer decides whether a freshly imported
// profile is committed or rolled back.
//
// The two used to be near-duplicate copies and this test existed because a rule
// applied to one copy is a rule with a hole in it. They share one exchange now,
// so what it pins is that this entry point still reaches that exchange and
// cannot drift from it — the provenance mutant has to kill this test and its
// twin above together, or one of the two wrappers has grown a path of its own.
func TestCheckYouTubeAuthBouncedThroughAnotherHostIsInconclusive(t *testing.T) {
	srv, hops, carried := redirectChain(t, "localhost", "Cookie", http.StatusOK, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.CheckYouTubeAuth(context.Background())

	requireBouncedChain(t, hops, carried, "Cookie")
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil — a page fetched without our cookies is not a verdict on our session")
	}
}

// --- Twitch: the 401 half of the same hole ---

// TestTwitchValidateBouncedThroughAnotherHostIsInconclusive: Go strips
// Authorization on the same cross-host hop it strips Cookie, and Task 1b made
// 401 DELIBERATELY conclusive — the one status that means "sign in again". Put
// together, an intermediary that bounces the validate call and answers 401
// produces a conclusive dead-token verdict about a token it never saw.
//
// The 401 rule stays; what it now requires is that the 401 came from the host
// we asked, carrying our token.
func TestTwitchValidateBouncedThroughAnotherHostIsInconclusive(t *testing.T) {
	srv, hops, carried := redirectChain(t, "localhost", "Authorization", http.StatusUnauthorized,
		`{"status":401,"message":"invalid access token"}`)
	pointTwitchValidateAt(t, srv)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	auth, err := rs.checkTwitchAuth(context.Background())

	requireBouncedChain(t, hops, carried, "Authorization")
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil — a 401 answered without our token is not a verdict on our token")
	}
	if strings.Contains(err.Error(), "auth-token-value") {
		t.Errorf("the error carries credential material: %q", err)
	}
}

// TestTwitchValidateSameHostRedirect401IsStillConclusive is the Twitch control,
// and it guards the boundary Task 1b was careful about: folding 401 into the
// inconclusive bucket would suppress recovery and the re-login prompt for every
// expired Twitch token. Same chain, same status, credential intact — still a
// verdict.
func TestTwitchValidateSameHostRedirect401IsStillConclusive(t *testing.T) {
	srv, hops, carried := redirectChain(t, "127.0.0.1", "Authorization", http.StatusUnauthorized,
		`{"status":401,"message":"invalid access token"}`)
	pointTwitchValidateAt(t, srv)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	auth, err := rs.checkTwitchAuth(context.Background())

	requireIntactChain(t, hops, carried, "Authorization")
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err != nil {
		t.Errorf("err = %v, want nil — 401 carrying our token is Twitch's documented invalid-token verdict", err)
	}
}

// --- the rule itself ---

// answeredBy builds the pair authResponseIsOurs compares: the request we
// dispatched, and a response shaped the way the stdlib client leaves one —
// Request is the FINAL hop's, carrying whatever headers survived the chain.
// finalCred=="" models the stripped case.
func answeredBy(t *testing.T, sentURL, finalURL, credHeader, sentCred, finalCred string) (*http.Response, *http.Request) {
	t.Helper()
	sent, err := http.NewRequest(http.MethodGet, sentURL, nil)
	if err != nil {
		t.Fatalf("build sent request for %q: %v", sentURL, err)
	}
	sent.Header.Set(credHeader, sentCred)

	final, err := http.NewRequest(http.MethodGet, finalURL, nil)
	if err != nil {
		t.Fatalf("build final request for %q: %v", finalURL, err)
	}
	if finalCred != "" {
		final.Header.Set(credHeader, finalCred)
	}
	return &http.Response{StatusCode: 200, Request: final}, sent
}

// TestAuthResponseIsOurs covers the ported rule directly, including the places
// it is deliberately STRICTER than the stdlib rule it defends against. Go would
// forward the credential on every one of those and we refuse anyway, so each
// disagreement resolves toward inconclusive — the safe direction.
func TestAuthResponseIsOurs(t *testing.T) {
	const asked = "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false"
	const cred = "SAPISID=redacted-in-tests"

	cases := []struct {
		name      string
		final     string
		finalCred string
		wantErr   string // substring; "" means the response must qualify
	}{
		{"exactly what we asked for", asked, cred, ""},
		{"same origin, different path and query", "https://www.youtube.com/youtubei/v1/guide?x=1", cred, ""},

		{"consent wall on another host", "https://consent.youtube.com/m?continue=x", cred, "consent.youtube.com"},
		// Go permits a subdomain hop (isDomainOrSubdomain) and keeps the
		// credential. We refuse: it is not the endpoint we asked.
		{"subdomain hop Go would allow", "https://m.www.youtube.com/youtubei/v1/guide", cred, "m.www.youtube.com"},
		// Go's strip decision compares Host only, so it never sees a scheme
		// downgrade. We refuse rather than read a verdict off a cleartext page.
		{"scheme downgrade Go would allow", "http://www.youtube.com/youtubei/v1/guide", cred, "http://"},
		// Go compares URL.Hostname(), port-stripped. We compare host:port.
		{"port change Go would allow", "https://www.youtube.com:8443/youtubei/v1/guide", cred, "www.youtube.com:8443"},

		// The bounce: terminal host is right, credentials are gone. The case a
		// host comparison alone cannot see.
		{"bounced back with the credential stripped", asked, "", "no longer carried"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, sent := answeredBy(t, asked, tc.final, "Cookie", cred, tc.finalCred)
			err := authResponseIsOurs(resp, sent, "Cookie")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), cred) {
				t.Errorf("error leaked the credential header value: %v", err)
			}
		})
	}
}

// TestCookiesHTTPClientCarriesNoCookieJar pins the premise the header half of
// authResponseIsOurs rests on, at the end where it can actually be broken.
//
// Same pin, same reasoning, as TestUtilsHTTPClientCarriesNoCookieJar in
// internal/utils — that one covers the client the tier-2 liveness probes use,
// this one covers the client the tier-1 auth checks use. Installing an
// http.CookieJar here would make the stdlib re-add a Cookie header on the final
// hop from the jar's own scope rules: the provenance check would then pass on a
// request that never carried OUR session, a bounced page would read as a
// verdict again, and NOTHING would fail. A silent regression needs a loud pin.
func TestCookiesHTTPClientCarriesNoCookieJar(t *testing.T) {
	if cookiesHTTPClient.Jar != nil {
		t.Error("cookiesHTTPClient has a CookieJar — this silently invalidates " +
			"authResponseIsOurs' credential-header rule, which is what stops a " +
			"redirected exchange being read as a conclusive auth verdict")
	}
}

// TestAuthResponseIsOursRejectsAnUnplaceableResponse: positive confirmation
// throughout. A response we cannot place is not an observation, so the nil-ish
// shapes must error rather than fall through to "close enough".
func TestAuthResponseIsOursRejectsAnUnplaceableResponse(t *testing.T) {
	_, sent := answeredBy(t, "https://www.youtube.com/x", "https://www.youtube.com/x", "Cookie", "c", "c")
	cases := map[string]*http.Response{
		"nil response":   nil,
		"no request":     {StatusCode: 200},
		"request no URL": {StatusCode: 200, Request: &http.Request{}},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			if err := authResponseIsOurs(resp, sent, "Cookie"); err == nil {
				t.Error("expected an error; got nil")
			}
		})
	}
}

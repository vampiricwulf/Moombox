package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// newTestServiceWithAuthCookies builds a Service whose jar holds a CONFIGURED
// YouTube session.
//
// halfClearedCookieFile (channel_membership_test.go) is the deliberate choice:
// it satisfies HasAnyYouTubeAuthCookie and fails the complete-set
// HasYouTubeAuthCookies, so every probe test built on it also pins that the
// permissive gate is the one in force. Switching the probe to HasAuthCookies
// turns all of them into zero-fetch runs.
func newTestServiceWithAuthCookies(t *testing.T) *Service {
	t.Helper()
	return jarServiceFromCookieFile(t, halfClearedCookieFile)
}

// aimProbeAt points accountProbeURL at u for the duration of the test and
// restores it afterwards.
func aimProbeAt(t *testing.T, u string) {
	t.Helper()
	orig := accountProbeURL
	accountProbeURL = u
	t.Cleanup(func() { accountProbeURL = orig })
}

// TestProbeAccountLivenessVerdicts: the probe must return a three-state
// verdict and must never claim LoggedOut for a page it does not recognise.
func TestProbeAccountLivenessVerdicts(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   SessionAuthState
		wantEr bool
	}{
		{"logged in", 200, `ytcfg.set({"LOGGED_IN":true});`, SessionAuthLoggedIn, false},
		{"session dead", 200, `ytcfg.set({"LOGGED_IN":false});`, SessionAuthLoggedOut, false},
		{"consent wall", 200, `Before you continue`, SessionAuthUnknown, false},
		{"rate limited", 429, ``, SessionAuthUnknown, true},
		{"server error", 503, ``, SessionAuthUnknown, true},
		// The camelCase spelling YouTube has also been observed using.
		{"camel-case marker", 200, `{"isLoggedIn":true}`, SessionAuthLoggedIn, false},
		// The case that separates livenessVerdict from sessionAuthFromBytes at
		// THIS call site: a shell carrying the ytcfg bootstrap but no login key
		// at all. The permissive detector calls that logged-out; the probe must
		// not, or a consent wall that ships a bootstrap reads as dead cookies.
		// Swapping this call site to sessionAuthFromBytes fails here and
		// nowhere else in this file.
		{"ytcfg shell with no login key", 200, `<script>ytcfg.set({"VISITOR_DATA":"x"});</script>`, SessionAuthUnknown, false},
		// A 200 with nothing in it is not an observation of anything.
		{"empty body", 200, ``, SessionAuthUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			orig := accountProbeURL
			accountProbeURL = srv.URL
			defer func() { accountProbeURL = orig }()

			svc := newTestServiceWithAuthCookies(t)
			got, err := svc.ProbeAccountLiveness(context.Background())
			if got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
			if (err != nil) != tt.wantEr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantEr)
			}
		})
	}
}

// TestProbeAccountLivenessSkipsWhenNeverConfigured: an install that was never
// signed in has nothing to report. It must not fetch, must not error, and must
// not claim a dead session.
func TestProbeAccountLivenessSkipsWhenNeverConfigured(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`ytcfg.set({"LOGGED_IN":false});`))
	}))
	defer srv.Close()
	aimProbeAt(t, srv.URL)

	s := jarServiceFromCookieFile(t, unconfiguredCookieFile)
	got, err := s.ProbeAccountLiveness(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != 0 {
		t.Errorf("fetched %d times, want 0 — nothing was ever configured", hits)
	}
	if got != SessionAuthUnknown {
		t.Errorf("verdict = %q, want %q", got, SessionAuthUnknown)
	}
}

// TestProbeAccountLivenessProbesAHalfClearedSession: the gate has to be the
// permissive predicate. A session holding SAPISID with LOGIN_INFO gone is
// exactly the state this probe exists to report on, and the complete-set
// predicate would skip it — making any verdict the probe could return dead
// code for the one case that matters.
func TestProbeAccountLivenessProbesAHalfClearedSession(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`<script>ytcfg.set({"LOGGED_IN":false});</script>`))
	}))
	defer srv.Close()
	aimProbeAt(t, srv.URL)

	s := newTestServiceWithAuthCookies(t)
	if s.HasAuthCookies() {
		t.Fatal("precondition: the complete-set predicate must reject a half-cleared session")
	}
	got, err := s.ProbeAccountLiveness(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != 1 {
		t.Fatalf("probe page fetched %d times, want 1 — the probe was gated out", hits)
	}
	if got != SessionAuthLoggedOut {
		t.Errorf("verdict = %q, want %q", got, SessionAuthLoggedOut)
	}
}

// deadSessionPage is a page that livenessVerdict reads as LoggedOut. The
// redirect test lands on it deliberately, so "the guard did not fire" and "the
// guard fired" produce different verdicts rather than both collapsing to
// Unknown for unrelated reasons.
const deadSessionPage = `<html><head><script>ytcfg.set({"LOGGED_IN":false});</script></head><body>Sign in</body></html>`

// TestProbeAccountLivenessRedirectGuard is half of H4: a probe answered by
// some OTHER host learned nothing about this session and must say so.
//
// Task 3 removed the ytcfg fallback so an IN-PLACE consent interstitial cannot
// read as a dead session. This covers the other shape — YouTube 302-ing to
// consent.youtube.com or accounts.google.com instead of interstitialing.
//
// SCOPE, precisely: this exercises the HOST comparison and nothing else. Both
// servers here are 127.0.0.1 on different ports, and Go's cookie-strip rule is
// hostname-based (shouldCopyHeaderOnRedirect → idnaASCIIFromURL → URL.Hostname(),
// port-stripped), so the Cookie header is NOT stripped in this test and the
// landing page below is fetched WITH credentials. The stripping — and the
// bounce-back case where the terminal host alone is not enough — is
// TestProbeAccountLivenessSurvivesACookieStrippingBounce.
func TestProbeAccountLivenessRedirectGuard(t *testing.T) {
	// Self-check: without the guard, this landing page yields LoggedOut, so
	// the assertions below genuinely exercise the guard rather than passing
	// because nothing was parseable.
	if v := livenessVerdict([]byte(deadSessionPage)); v != SessionAuthLoggedOut {
		t.Fatalf("landing page reads as %q, want %q — the test would not exercise the guard", v, SessionAuthLoggedOut)
	}

	landing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(deadSessionPage))
	}))
	defer landing.Close()

	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, landing.URL+"/ServiceLogin?continue=x", http.StatusFound)
	}))
	defer probe.Close()
	aimProbeAt(t, probe.URL+"/feed/subscriptions")

	s := newTestServiceWithAuthCookies(t)
	got, err := s.ProbeAccountLiveness(context.Background())
	if got != SessionAuthUnknown {
		t.Errorf("verdict = %q, want %q — a redirect off the probe host is not a login verdict", got, SessionAuthUnknown)
	}
	if err == nil {
		t.Fatal("expected an error naming the redirect; got nil")
	}
	// The error is a diagnostic, not a transcript. It may name the host it
	// landed on; it may never carry page content.
	if strings.Contains(err.Error(), "LOGGED_IN") || strings.Contains(err.Error(), "Sign in") {
		t.Errorf("error echoed page content: %v", err)
	}
	landingHost := strings.TrimPrefix(landing.URL, "http://")
	if !strings.Contains(err.Error(), landingHost) {
		t.Errorf("error = %v, want it to name the host it landed on (%s)", err, landingHost)
	}
}

// bounceChain wires an origin server that redirects off-host to a wall server,
// which redirects straight back to the origin. It returns the probe URL to
// aim at and a func reporting whether the final on-origin hop still carried a
// Cookie header. The landing body is deadSessionPage, so a probe that reads it
// answers LoggedOut.
//
// The two hops must differ by HOSTNAME, not merely by port: Go compares
// URL.Hostname(), so two ports on 127.0.0.1 are the same host to it and no
// strip occurs. "localhost" and "127.0.0.1" both reach the loopback listeners
// while being distinct hostnames, which is what makes the strip reproducible
// with no DNS setup.
func bounceChain(t *testing.T) (probeURL string, cookieSurvived func() bool) {
	t.Helper()
	var origin *httptest.Server
	var wall *httptest.Server
	var finalHopCookie string
	var sawFinalHop bool

	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("back") == "" {
			http.Redirect(w, r, wallURL(wall)+"/consent", http.StatusFound)
			return
		}
		sawFinalHop = true
		finalHopCookie = r.Header.Get("Cookie")
		w.Write([]byte(deadSessionPage))
	}))
	t.Cleanup(origin.Close)

	wall = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL+"/feed/subscriptions?back=1", http.StatusFound)
	}))
	t.Cleanup(wall.Close)

	return origin.URL + "/feed/subscriptions", func() bool {
		if !sawFinalHop {
			t.Fatal("the chain never bounced back to the origin; this test is not set up as intended")
		}
		return finalHopCookie != ""
	}
}

// wallURL re-addresses an httptest server (which listens on 127.0.0.1) as
// "localhost" so the redirect to it is a genuine cross-hostname hop.
func wallURL(s *httptest.Server) string {
	return strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
}

// TestProbeAccountLivenessSurvivesACookieStrippingBounce is the other half of
// H4, and the case a terminal-host check alone does NOT cover.
//
// Go's decision to strip a manually-set Cookie header is STICKY: client.go
// declares stripSensitiveHeaders once before the redirect loop and only ever
// sets it inside, guarded by !stripSensitiveHeaders — nothing clears it on a
// later hop. So origin → wall → origin ends on the host we asked for, with the
// credentials permanently gone. The host comparison passes; the body is an
// anonymous fetch; livenessVerdict reads LoggedOut off it and an operator whose
// cookies are fine is told they are dead.
//
// The assertion on cookieSurvived() is the load-bearing part: it observes the
// strip directly, so if a future Go release stops stripping (or starts
// restoring on return to the origin) this test says so out loud instead of
// silently ceasing to cover anything.
func TestProbeAccountLivenessSurvivesACookieStrippingBounce(t *testing.T) {
	if v := livenessVerdict([]byte(deadSessionPage)); v != SessionAuthLoggedOut {
		t.Fatalf("landing page reads as %q, want %q — the test would not exercise the guard", v, SessionAuthLoggedOut)
	}

	probeURL, cookieSurvived := bounceChain(t)
	aimProbeAt(t, probeURL)

	s := newTestServiceWithAuthCookies(t)
	got, err := s.ProbeAccountLiveness(context.Background())

	// Never print the header's value — only whether one was present.
	if cookieSurvived() {
		t.Fatal("the final hop still carried a Cookie header; Go no longer strips on this chain, " +
			"so this test has stopped reproducing the hazard it was written for")
	}
	if got != SessionAuthUnknown {
		t.Errorf("verdict = %q, want %q — the body was fetched with no credentials", got, SessionAuthUnknown)
	}
	if err == nil {
		t.Fatal("expected an error; got nil")
	}
	if strings.Contains(err.Error(), "LOGGED_IN") || strings.Contains(err.Error(), "working-sapisid") {
		t.Errorf("error echoed page content or a cookie value: %v", err)
	}
}

// TestProbeAccountLivenessFollowsSameHostRedirect keeps the guard from being
// over-broad: YouTube redirecting within its own origin (locale/path
// normalisation) is a page we asked for and can read.
func TestProbeAccountLivenessFollowsSameHostRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hl") == "" {
			http.Redirect(w, r, srv.URL+"/feed/subscriptions?hl=en", http.StatusFound)
			return
		}
		w.Write([]byte(`<script>ytcfg.set({"LOGGED_IN":true});</script>`))
	}))
	defer srv.Close()
	aimProbeAt(t, srv.URL+"/feed/subscriptions")

	s := newTestServiceWithAuthCookies(t)
	got, err := s.ProbeAccountLiveness(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SessionAuthLoggedIn {
		t.Errorf("verdict = %q, want %q — a same-host redirect must still be read", got, SessionAuthLoggedIn)
	}
}

// TestProbeAccountLivenessTransportFailureIsNotAVerdict: a page we never
// received says nothing about the session. This is the failure mode that
// reaches a container behind a flaky egress path.
func TestProbeAccountLivenessTransportFailureIsNotAVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing is listening on `dead` any more
	aimProbeAt(t, dead)

	s := newTestServiceWithAuthCookies(t)
	got, err := s.ProbeAccountLiveness(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unreachable probe host")
	}
	if got != SessionAuthUnknown {
		t.Errorf("verdict = %q, want %q — a transport failure is not a login verdict", got, SessionAuthUnknown)
	}
}

// TestAccountProbeURLIsTheMeasuredPage pins the probe to the page the arc's
// premise was actually measured on. /feed/subscriptions was verified in both
// directions on 2026-08-25 (anonymous → "LOGGED_IN":false, authenticated →
// true) and that measurement is the only empirical grounding this arc has;
// TestLiveLoginMarkersPresent re-checks it against the live site. Repointing
// this var at a different page throws that away, so do it deliberately and
// re-measure — not as a drive-by.
func TestAccountProbeURLIsTheMeasuredPage(t *testing.T) {
	if want := constants.YouTubeURLs.Base + "/feed/subscriptions"; accountProbeURL != want {
		t.Errorf("accountProbeURL = %q, want %q", accountProbeURL, want)
	}
}

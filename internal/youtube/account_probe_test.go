package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// newTestServiceWithCookieFile builds a real Service around a jar loaded from a
// Netscape cookie file written into the test's temp dir.
//
// It goes through NewService rather than composing a &Service{} literal:
// ProbeAccountLiveness's first act is s.Auth.HasAnyAuthCookie(), and
// newTestService (service_test.go) leaves Auth nil, so the literal shortcut
// nil-panics before the probe does anything.
func newTestServiceWithCookieFile(t *testing.T, cookieFile string) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(cookieFile), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	return NewService(jar, noopLogger{})
}

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
	return newTestServiceWithCookieFile(t, halfClearedCookieFile)
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

	s := newTestServiceWithCookieFile(t, unconfiguredCookieFile)
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

// TestProbeAccountLivenessRedirectGuard is H4: a probe that is answered by
// some OTHER host learned nothing about this session, and must say so.
//
// Task 3 removed the ytcfg fallback so an IN-PLACE consent interstitial cannot
// read as a dead session. This covers the other shape — YouTube 302-ing to
// consent.youtube.com or accounts.google.com instead of interstitialing. Go's
// http.Client drops a manually-set Cookie header when a redirect leaves the
// initial host (net/http/client.go shouldCopyHeaderOnRedirect), so the page
// that answers such a redirect is by construction an anonymous fetch: parsing
// it would report every EU/datacenter deployment's healthy cookies as dead.
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

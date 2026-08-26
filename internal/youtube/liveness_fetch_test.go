package youtube

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// answeredBy builds a *http.Response shaped the way the stdlib client leaves
// one: Request is the FINAL hop's request, carrying whatever headers survived
// the redirect chain. cookie=="" models the stripped case.
func answeredBy(t *testing.T, finalURL, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, finalURL, nil)
	if err != nil {
		t.Fatalf("build request for %q: %v", finalURL, err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return &http.Response{StatusCode: 200, Request: req}
}

// TestLivenessResponseIsOurs covers the admissibility rules directly, including
// the three places they are deliberately STRICTER than the stdlib rule they
// defend against. Each of those would be forwarded a Cookie by Go and is still
// refused here, so every disagreement resolves toward Unknown.
func TestLivenessResponseIsOurs(t *testing.T) {
	const probe = "https://www.youtube.com/feed/subscriptions"
	const sent = "SAPISID=redacted-in-tests"

	cases := []struct {
		name    string
		final   string
		cookie  string
		wantErr string // substring; "" means the response must qualify
	}{
		{"exactly what we asked for", probe, sent, ""},
		{"same origin, different path and query", "https://www.youtube.com/feed/subscriptions?hl=en", sent, ""},

		{"consent wall on another host", "https://consent.youtube.com/m?continue=x", sent, "consent.youtube.com"},
		{"login host", "https://accounts.google.com/ServiceLogin", sent, "accounts.google.com"},

		// Go permits a subdomain hop (isDomainOrSubdomain) and keeps the
		// cookie. We refuse: it is not the page we asked for.
		{"subdomain hop Go would allow", "https://m.www.youtube.com/feed/subscriptions", sent, "m.www.youtube.com"},
		// Go's strip decision compares Host only, so it never sees a scheme
		// downgrade. We refuse rather than read a verdict off a cleartext page.
		{"scheme downgrade Go would allow", "http://www.youtube.com/feed/subscriptions", sent, "answered by http://"},
		// Go compares URL.Hostname(), port-stripped, so a port change keeps the
		// cookie. We compare host:port.
		{"port change Go would allow", "https://www.youtube.com:8443/feed/subscriptions", sent, "www.youtube.com:8443"},

		// The bounce: terminal host is right, credentials are gone. This is the
		// case a host comparison alone cannot see.
		{"bounced back with cookies stripped", probe, "", "no longer carried the session cookies"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := livenessResponseIsOurs(answeredBy(t, tc.final, tc.cookie), mustURL(t, probe))
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
			if strings.Contains(err.Error(), sent) {
				t.Errorf("error leaked the cookie header: %v", err)
			}
		})
	}
}

// TestLivenessResponseIsOursRejectsAnUnplaceableResponse: positive
// confirmation. A response we cannot place is not an observation, so the
// nil-ish shapes must error rather than fall through to "close enough".
func TestLivenessResponseIsOursRejectsAnUnplaceableResponse(t *testing.T) {
	want := mustURL(t, "https://www.youtube.com/feed/subscriptions")
	cases := map[string]*http.Response{
		"nil response":   nil,
		"no request":     {StatusCode: 200},
		"request no URL": {StatusCode: 200, Request: &http.Request{}},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			if err := livenessResponseIsOurs(resp, want); err == nil {
				t.Error("expected an error; got nil")
			}
		})
	}
}

// TestFetchLivenessPageRefusesAnAnonymousFetch: with no cookies to send there
// is nothing to observe, and any LoggedOut read off the result would be a
// false alarm. It must not even make the request.
//
// The zero-hit assertion is the load-bearing one and it has to be counted, not
// inferred. Rule 3 rejects this same case AFTER fetching, so an error alone is
// consistent with rule 1 having been deleted; only the hit count distinguishes
// "refused up front" from "fetched, then refused". Pointing this at an
// unreachable address instead would have made the error assertion pass on the
// dial failure alone, testing nothing.
func TestFetchLivenessPageRefusesAnAnonymousFetch(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`ytcfg.set({"LOGGED_IN":false});`))
	}))
	defer srv.Close()

	s := jarServiceFromCookieFile(t, "# Netscape HTTP Cookie File\n")
	if got := s.Auth.GetCookieHeader(); got != "" {
		t.Fatal("precondition: expected an empty cookie header, got a non-empty one")
	}
	if _, err := s.fetchLivenessPage(t.Context(), srv.URL); err == nil {
		t.Error("expected an error when there are no session cookies to send")
	}
	if hits != 0 {
		t.Errorf("made %d request(s), want 0 — an anonymous fetch must not be attempted at all", hits)
	}
}

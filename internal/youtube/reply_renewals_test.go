package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	renewedSIDCC  = "sidcc-renewed-by-the-reply"
	signedInPage  = `ytcfg.set({"LOGGED_IN":true});`
	signedOutPage = `ytcfg.set({"LOGGED_IN":false});`
)

// renewalService is a Service whose jar holds a signed-in session and whose
// cookie store takes replies from srv, an httptest server standing in for
// www.youtube.com.
func renewalService(t *testing.T) *Service {
	t.Helper()
	s := jarServiceFromCookieFile(t, cookieFileRows(
		"SAPISID", "sap", "LOGIN_INFO", "li", "__Secure-1PSIDCC", "sidcc-old"))
	cookies.AllowReplyHostForTesting(t, func(scheme, host string) bool { return host == "127.0.0.1" })
	return s
}

// renewingServer answers every request with body and a renewed
// __Secure-1PSIDCC.
func renewingServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "__Secure-1PSIDCC="+renewedSIDCC+"; Domain=.youtube.com; Path=/; Max-Age=31536000")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// aimPagesAt sends fetchYouTubePage's requests to srv, whatever host they
// name.
func aimPagesAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	orig := pageGet
	pageGet = func(ctx context.Context, pageURL string, timeout time.Duration, headers map[string]string) (*http.Response, context.CancelFunc, error) {
		u, err := url.Parse(pageURL)
		if err != nil {
			return nil, nil, err
		}
		u.Scheme, u.Host = target.Scheme, target.Host
		return utils.FetchWithTimeout(ctx, u.String(), timeout, headers)
	}
	t.Cleanup(func() { pageGet = orig })
}

// Every request this package sends with the jar's session offers its reply
// to the cookie store, with the verdict the caller read: the next request
// then carries what YouTube last sent. A page read as signed out is the one
// reply whose renewal is refused, and a request sent without the session
// offers nothing the store keeps.
func TestRepliesToSessionRequestsRenewTheJar(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		request  func(t *testing.T, s *Service, srv *httptest.Server)
		wantKept bool
	}{
		{name: "membership page, signed in", body: signedInPage, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				orig := membershipPageBase
				membershipPageBase = srv.URL
				t.Cleanup(func() { membershipPageBase = orig })
				_, _, _, _ = s.FetchMembershipVideos(t.Context(), "UCxxxxxxxxxxxxxxxxxxxxxx")
			}},
		{name: "membership page, signed out", body: signedOutPage, wantKept: false,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				orig := membershipPageBase
				membershipPageBase = srv.URL
				t.Cleanup(func() { membershipPageBase = orig })
				_, _, _, _ = s.FetchMembershipVideos(t.Context(), "UCxxxxxxxxxxxxxxxxxxxxxx")
			}},
		{name: "account page, signed in", body: signedInPage, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimProbeAt(t, srv.URL)
				_, _ = s.ProbeAccountLiveness(t.Context())
			}},
		{name: "account page, signed out", body: signedOutPage, wantKept: false,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimProbeAt(t, srv.URL)
				_, _ = s.ProbeAccountLiveness(t.Context())
			}},
		{name: "browse", body: `{}`, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				s.browse.endpoint = srv.URL
				_, _ = s.browsePost(t.Context(), map[string]any{}, "")
			}},
		{name: "player", body: `{}`, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				headers := s.Auth.GenerateAPIHeaders(constants.WebClient, nil)
				_, _ = s.PlayerAPI.doRetryRequest(t.Context(), srv.URL, []byte(`{}`), headers, nil, "WEB", "abc12345678")
			}},
		{name: "player, a client sent without cookies", body: `{}`, wantKept: false,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				_, _ = s.PlayerAPI.doRetryRequest(t.Context(), srv.URL, []byte(`{}`), nil, nil, "ANDROID_VR", "abc12345678")
			}},
		{name: "watch page, signed in", body: signedInPage, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimPagesAt(t, srv)
				wp, err := FetchWatchPage(t.Context(), "abc12345678", s.Auth.PageCookieHeader())
				if err != nil {
					t.Fatal(err)
				}
				s.Auth.OfferReply(wp.reply)
			}},
		{name: "watch page, signed out", body: signedOutPage, wantKept: false,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimPagesAt(t, srv)
				wp, err := FetchWatchPage(t.Context(), "abc12345678", s.Auth.PageCookieHeader())
				if err != nil {
					t.Fatal(err)
				}
				s.Auth.OfferReply(wp.reply)
			}},
		{name: "watch page fetched with the consent cookies alone", body: signedInPage, wantKept: false,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimPagesAt(t, srv)
				wp, err := FetchWatchPage(t.Context(), "abc12345678", s.Auth.ConsentCookieHeader())
				if err != nil {
					t.Fatal(err)
				}
				s.Auth.OfferReply(wp.reply)
			}},
		{name: "embed page", body: `<html></html>`, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimPagesAt(t, srv)
				ep, err := FetchEmbedPage(t.Context(), "abc12345678", s.Auth.PageCookieHeader())
				if err != nil {
					t.Fatal(err)
				}
				s.Auth.OfferReply(ep.reply)
			}},
		{name: "chapter read, signed in", body: signedInPage, wantKept: true,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimPagesAt(t, srv)
				_ = s.FetchChapters(t.Context(), "abc12345678", "", 60_000)
			}},
		{name: "chapter read, signed out", body: signedOutPage, wantKept: false,
			request: func(t *testing.T, s *Service, srv *httptest.Server) {
				aimPagesAt(t, srv)
				_ = s.FetchChapters(t.Context(), "abc12345678", "", 60_000)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := renewalService(t)
			srv := renewingServer(t, tc.body)

			tc.request(t, s, srv)

			kept := strings.Contains(s.Auth.GetCookieHeader(), "__Secure-1PSIDCC="+renewedSIDCC)
			if kept != tc.wantKept {
				t.Errorf("the jar's next Cookie header carries the renewed value = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

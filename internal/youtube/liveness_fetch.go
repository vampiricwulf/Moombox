package youtube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// livenessFetchTimeout bounds one liveness page fetch. Both probes read a
// comparably sized first-party HTML page.
const livenessFetchTimeout = 20 * time.Second

// fetchLivenessPage GETs pageURL with the session cookies and returns the body
// ONLY when the response is one those cookies actually produced.
//
// Shared by both liveness probes (ProbeAccountLiveness and
// FetchMembershipVideos) because the qualifying rules below are not
// probe-specific: they are what makes a fetched page admissible as evidence
// about the session at all. A probe that skips them can hand livenessVerdict
// an anonymous page, and the only verdict the arc acts on — LoggedOut — is
// exactly the one an anonymous page produces.
//
// A page qualifies only if all of these hold:
//
//   - We had cookies to send. An anonymous fetch cannot report on a session.
//   - The response was produced by the scheme and host we asked for. A login
//     or consent wall answers on a different host (consent.youtube.com,
//     accounts.google.com) and answers 200, so a status check can never see
//     one.
//   - The request that finally answered STILL CARRIED the Cookie header.
//
// That last rule is the load-bearing one and it does not follow from the host
// check. Go's http.Client drops a manually-set Cookie header once a redirect
// leaves the initial host, and the decision is STICKY: client.go declares
// stripSensitiveHeaders once before the redirect loop (:618) and only ever
// sets it inside (:688, itself guarded by !stripSensitiveHeaders) — nothing
// clears it on a later hop. So origin → wall → origin lands back on the host
// we asked for, passes any terminal-host test, and delivers a body fetched
// with no credentials. Reading a verdict off it reports healthy cookies as
// dead. Checking resp.Request.Header (the final hop's request, from which
// copyHeaders at client.go:826 omits Cookie when stripped) observes the
// outcome directly instead of inferring it from the route.
//
// Verified against go1.26.1, C:\Program Files\Go\src\net\http\client.go.
//
// Errors name a status code, a URL, or a host — never response bytes and
// never a cookie value.
func (s *Service) fetchLivenessPage(ctx context.Context, pageURL string) ([]byte, error) {
	want, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("bad liveness page URL: %w", err)
	}

	// Refusing the empty case here is what lets the post-fetch check below be
	// unconditional: past this point a Cookie header was definitely set, so
	// finding none on the answering request means it was taken away.
	sentCookie := s.Auth.GetCookieHeader()
	if sentCookie == "" {
		return nil, fmt.Errorf("no session cookies to send to %s", pageURL)
	}

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
		"Cookie":          sentCookie,
	}

	// FetchWithTimeout, not FetchBody: FetchBody returns bytes only, and
	// whether those bytes mean anything depends on where they came from and
	// what the request that fetched them carried. Same reason FetchWatchPage
	// uses it. The non-2xx check FetchBody would have done is re-done below.
	resp, cancel, err := utils.FetchWithTimeout(ctx, pageURL, livenessFetchTimeout, headers)
	if err != nil {
		// A page we never received is not an observation of the session.
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()

	if err := livenessResponseIsOurs(resp, want); err != nil {
		io.Copy(io.Discard, resp.Body)
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, pageURL)
	}

	return io.ReadAll(io.LimitReader(resp.Body, utils.MaxFetchBodySize))
}

// livenessResponseIsOurs returns nil only when resp can be read as an
// observation of our own authenticated session, and otherwise an error saying
// which way it failed to qualify. Callers must have set a Cookie header on the
// request (fetchLivenessPage enforces that before fetching).
//
// Positive confirmation throughout: an answer we cannot place is not an
// answer. Both checks are deliberately STRICTER than the stdlib rule they
// defend against, so the direction of any mismatch is toward Unknown:
//
//   - Host is compared as host:port, raw. Go compares URL.Hostname() —
//     port-stripped, IDN-canonicalised, and permitting subdomains
//     (isDomainOrSubdomain, client.go:1028). A port change or a subdomain hop
//     therefore fails here while Go would still forward the cookie.
//   - Scheme is compared at all. Go's strip decision looks only at Host
//     (client.go:688), so an https→http downgrade on the same host keeps the
//     cookie; we refuse it anyway rather than read a verdict off a page
//     fetched in clear.
func livenessResponseIsOurs(resp *http.Response, want *url.URL) error {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return fmt.Errorf("could not determine what answered %s", want)
	}
	final := resp.Request.URL
	if !strings.EqualFold(final.Scheme, want.Scheme) || !strings.EqualFold(final.Host, want.Host) {
		return fmt.Errorf("%s was answered by %s://%s; not an observation of this session",
			want, final.Scheme, final.Host)
	}
	if resp.Request.Header.Get("Cookie") == "" {
		return fmt.Errorf("%s was answered by a request that no longer carried the session cookies; not an observation of this session", want)
	}
	return nil
}

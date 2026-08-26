package youtube

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// accountProbeURL is a first-party page that renders a recognisably logged-in
// shell for an authenticated session and a logged-out one otherwise.
// /feed/subscriptions is stable, needs no configuration, and — unlike a
// members-only video probe — has no ID that ages out.
//
// It is also the page the arc's premise was measured on: 2026-08-25, anonymous
// → HTTP 200 carrying "LOGGED_IN":false, authenticated → true.
// TestLiveLoginMarkersPresent re-checks the anonymous half against the live
// site, and TestAccountProbeURLIsTheMeasuredPage pins this value so a swap has
// to be deliberate.
//
// A var, not a const, purely so tests can point it at an httptest server.
var accountProbeURL = constants.YouTubeURLs.Base + "/feed/subscriptions"

// accountProbeTimeout bounds one probe. Matches FetchMembershipVideos, the
// sibling liveness fetch of a comparably sized HTML page.
const accountProbeTimeout = 20 * time.Second

// ProbeAccountLiveness answers "do these cookies still work" without reference
// to any channel.
//
// The membership probe (FetchMembershipVideos) is the preferred liveness
// source: it is already being made every monitor cycle and it proves
// CAPABILITY, not just recognition. But it is per-channel and gated on the
// membership-discovery toggle, so an install with no YouTube channels — or
// with that toggle off everywhere — gets no observation at all. This fills
// that gap and nothing else; the caller should skip it whenever a membership
// observation is recent, and is responsible for deduplicating: one call
// yields one verdict, and it is the caller that decides whether a verdict
// becomes an operator-visible alarm.
//
// Returns SessionAuthUnknown plus an error for any transport failure, non-200,
// or redirect off the probe host. That asymmetry is the whole point: a rate
// limit is not a verdict on the session, and reporting it as one is how an
// operator with healthy cookies gets told they are dead. An unrecognisable
// 200 is SessionAuthUnknown with a nil error — nothing went wrong, we simply
// have no observation.
//
// Nothing from the response body ever reaches the returned error or the log.
// The body is a signed-in account page; the only thing extracted from it is
// the three-state verdict.
func (s *Service) ProbeAccountLiveness(ctx context.Context) (SessionAuthState, error) {
	// "Was YouTube auth ever configured", not "is the set complete right now".
	// The complete-set predicate would skip the probe precisely when the
	// session is half-cleared — the state the probe exists to detect.
	if !s.Auth.HasAnyAuthCookie() {
		// Nothing configured — there is no session to have an opinion about.
		return SessionAuthUnknown, nil
	}
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed before account liveness probe", "error", err)
	}

	probeURL := accountProbeURL
	parsed, err := url.Parse(probeURL)
	if err != nil {
		return SessionAuthUnknown, fmt.Errorf("account liveness probe: bad probe URL: %w", err)
	}

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	if ch := s.Auth.GetCookieHeader(); ch != "" {
		headers["Cookie"] = ch
	}

	// FetchWithTimeout, not FetchBody: FetchBody returns bytes only, and the
	// verdict depends on WHERE those bytes came from (see the redirect guard
	// below). Same reason FetchWatchPage uses it.
	resp, cancel, err := utils.FetchWithTimeout(ctx, probeURL, accountProbeTimeout, headers)
	if err != nil {
		// A page we never received is not an observation of the session.
		return SessionAuthUnknown, fmt.Errorf("account liveness probe: %w", err)
	}
	defer cancel()
	defer resp.Body.Close()

	// The redirect guard. A login or consent wall answers on a DIFFERENT host
	// (consent.youtube.com, accounts.google.com), and Go's http.Client drops a
	// manually-set Cookie header once a redirect leaves the initial host —
	// net/http/client.go's shouldCopyHeaderOnRedirect permits it only for the
	// same domain or a subdomain of it, and "consent.youtube.com" is neither
	// for "www.youtube.com". So a body fetched after such a redirect is an
	// anonymous fetch by construction: reading a verdict off it would report
	// healthy cookies as dead. Positive confirmation, not absence-checking —
	// if the landing host cannot be determined the answer is still "no
	// observation".
	//
	// Measured 2026-08-25: /feed/subscriptions does NOT redirect from a
	// desktop IP in either direction, so this is insurance for the EU and
	// datacenter paths that were not measured — Docker being the target
	// deployment — rather than a present behaviour.
	landedHost := ""
	if resp.Request != nil && resp.Request.URL != nil {
		landedHost = resp.Request.URL.Host
	}
	if !strings.EqualFold(landedHost, parsed.Host) {
		io.Copy(io.Discard, resp.Body)
		return SessionAuthUnknown, fmt.Errorf(
			"account liveness probe: %s was answered by host %q; not an observation of this session",
			probeURL, landedHost)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return SessionAuthUnknown, fmt.Errorf("account liveness probe: HTTP %d from %s", resp.StatusCode, probeURL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, utils.MaxFetchBodySize))
	if err != nil {
		return SessionAuthUnknown, fmt.Errorf("account liveness probe: %w", err)
	}

	// livenessVerdict, NOT sessionAuthFromBytes: the strict variant refuses the
	// ytcfg fallback, so a shell carrying a bootstrap but no login key reads as
	// unknown instead of as a dead session.
	return livenessVerdict(body), nil
}

package youtube

import (
	"context"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// accountProbeURL is a first-party page that renders a recognisably logged-in
// shell for an authenticated session and a logged-out one otherwise.
// /feed/subscriptions is stable, needs no configuration, and — unlike a
// members-only video probe — has no ID that ages out.
//
// It is also the page the arc's premise was measured on: 2026-08-25, anonymous
// → HTTP 200 carrying "LOGGED_IN":false, authenticated → true.
// TestLiveLoginMarkersPresent re-checks the anonymous half against the live
// site, TestLiveAuthenticatedAccountProbe the authenticated half, and
// TestAccountProbeURLIsTheMeasuredPage pins this value so a swap has to be
// deliberate.
//
// A var, not a const, purely so tests can point it at an httptest server.
var accountProbeURL = constants.YouTubeURLs.Base + "/feed/subscriptions"

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
// Returns SessionAuthUnknown plus an error for every way a fetch can fail to
// qualify as evidence — transport failure, non-200, or a response that did not
// come from our own credentialed request (see fetchLivenessPage). That
// asymmetry is the whole point: a rate limit is not a verdict on the session,
// and reporting it as one is how an operator with healthy cookies gets told
// they are dead. An unrecognisable 200 is SessionAuthUnknown with a nil error
// — nothing went wrong, we simply have no observation.
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

	body, err := s.fetchLivenessPage(ctx, accountProbeURL)
	if err != nil {
		return SessionAuthUnknown, fmt.Errorf("account liveness probe: %w", err)
	}

	// livenessVerdict, NOT watchPageSessionAuth: the strict variant refuses the
	// ytcfg fallback, so a shell carrying a bootstrap but no login key reads as
	// unknown instead of as a dead session.
	return livenessVerdict(body), nil
}

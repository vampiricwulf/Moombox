package cookies

// autocookies_messages.go — user-facing wording shared across the setup and
// refresh paths: platform display names and the inconclusive/lost-cookies
// hedges.

import (
	"strings"
)

// platformDisplayName maps the lowercase platform keys used internally in
// this file (restoredPlatforms, importCheck, the map literal
// combinedInconclusiveHedge is called with) to the capitalized names
// `failed`/`lost` already render to the operator.
func platformDisplayName(platform string) string {
	switch platform {
	case "youtube":
		return "YouTube"
	case "twitch":
		return "Twitch"
	default:
		return platform
	}
}

// inconclusiveHedge renders the (network?) hedge's replacement wording for
// ONE platform's inconclusive check, disambiguated by attempted — see
// platformAuth's doc for what the two halves of verifyUnknown mean.
func inconclusiveHedge(p platformAuth) string {
	if p.attempted {
		// A request went out and came back unusable — network, an
		// intermediary, a timeout. Say that; no question mark, because this
		// is no longer a guess.
		return "the auth check did not complete"
	}
	// No request was ever attempted — the extracted cookies could not form
	// one. Same wording the Warn at checkPlatformAuth's caller (FinishSetup)
	// already uses for the same fact — see attempted's doc on platformAuth —
	// minus the "during setup" qualifier, which does not apply to a refresh
	// pass.
	return "the auth check was never attempted — the extracted cookies cannot form an authenticated request"
}

// combinedInconclusiveHedge renders the (network?) hedge across every
// platform in order whose check in checks landed on verifyUnknown.
//
// When every one of them agrees on attempted — true for a single inconclusive
// platform, which is the overwhelmingly common shape — it returns ONE hedge
// and allAgree=true, the joined-sentence form callers had before this
// existed. When they disagree — one platform's check went out and came back
// unusable while the other's cookies could never form a request at all —
// collapsing them to a single hedge asserts a cause about a platform the
// code knows is false (reviewer round 1, finding 1, caught this for the
// AND/OR tie-break this function replaced). allAgree=false then, and hedge
// is a full "Platform: cause; Platform: cause" breakdown naming each one, not
// a fragment meant to be embedded after a shared lead-in — see the two call
// sites for how each folds allAgree=false into its own sentence.
//
// order fixes iteration to a stable sequence (callers pass
// []string{"youtube", "twitch"} or restoredPlatforms, which is already built
// in that order) so the rendered message is deterministic.
func combinedInconclusiveHedge(order []string, checks map[string]platformAuth) (hedge string, allAgree bool) {
	var names []string
	var pairs []platformAuth
	for _, platform := range order {
		p, ok := checks[platform]
		if !ok || p.state != verifyUnknown {
			continue
		}
		names = append(names, platform)
		pairs = append(pairs, p)
	}
	if len(pairs) == 0 {
		return "", true
	}
	agreedHedge := inconclusiveHedge(pairs[0])
	allAgree = true
	for _, p := range pairs[1:] {
		if inconclusiveHedge(p) != agreedHedge {
			allAgree = false
			break
		}
	}
	if allAgree {
		return agreedHedge, true
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = platformDisplayName(names[i]) + ": " + inconclusiveHedge(p)
	}
	return strings.Join(parts, "; "), false
}

// cookiesLostMessage names the platforms whose credentials were on disk
// before a refresh and are not on disk after it. One wording for every exit
// that can observe the loss, so the operator sees the same sentence whether
// the sibling platform verified, was rejected, or was never there.
func cookiesLostMessage(lost []string) string {
	return strings.Join(lost, " + ") + " cookies are gone from cookies.txt after this refresh — " +
		"every stored credential had expired or was dropped, so nothing is left to authenticate with; sign in again"
}

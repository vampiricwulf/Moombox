package cookies

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/cookies/dpapi"
)

// dpapiFindBrowserProfiles and dpapiReadChromeCookiesStats are seams over
// the dpapi package's real (Windows-only) filesystem/SQLite I/O, so a test
// can substitute deterministic synthetic profiles and cookie sets instead
// of touching this machine's actual browser profiles — calling
// dpapi.FindBrowserProfiles directly in a test would enumerate whatever is
// really installed under %LOCALAPPDATA%, which is neither deterministic
// nor safe to assert on. Same seam convention as detectBrowserUncached /
// detectBrowsersUncached (autocookies_detect.go). Production never
// reassigns them.
var (
	dpapiFindBrowserProfiles    = dpapi.FindBrowserProfiles
	dpapiReadChromeCookiesStats = dpapi.ReadChromeCookiesStats
)

// dpapiExtractAsNetscape reads cookies from exactly ONE Chromium-family
// profile that dpapi.FindBrowserProfiles can see, runs its rows through
// deduplicateAndFormat (which keeps only the YouTube / Google / Twitch
// essentials and prefers youtube.com over google.com on dedup conflicts),
// and returns the Netscape cookies.txt content ready for writeFileAtomic.
//
// Used as a fallback when the CDP refresh path can't acquire the profile
// lock — DPAPI doesn't launch a browser, so it sidesteps the "Chromium
// already running our profile" failure mode entirely.
//
// H7 (Arc 8): this used to walk EVERY profile FindBrowserProfiles returned
// and merge all of them into one collected slice before dedup. With two
// signed-in Chromium profiles (Chrome "Default" + "Profile 1", or Chrome +
// Edge), deduplicateAndFormat's bare-name dedup meant whichever profile
// FindBrowserProfiles listed LAST silently won each cookie name — an
// order-dependent coin flip nothing logged, and the two halves of one
// session (SAPISID from profile A, LOGIN_INFO from profile B) could end up
// a credential pair YouTube rejects as inconsistent. Now exactly one
// profile is chosen — deliberately, and logged — and the rest are
// discarded whole; deduplicateAndFormat never sees more than one profile's
// rows. Selection:
//
//  1. configuredBrowserType is the operator's browser_type setting,
//     already gated to a genuine override by the caller (a type with no
//     path is not a real override — see autocookies.go's call site). Three
//     cases (Arc 8 fix round 1 / Finding 1 + 2):
//     - Empty (auto-detect): every profile is a candidate.
//     - "chrome": the Web UI's ONLY Chromium option is literally
//     `<sl-option value="chrome">Chromium-family (chrome, brave, edge,
//     vivaldi, thorium, opera)</sl-option>` (web/public/index.html) —
//     so "chrome" from config means "the operator picked SOME
//     Chromium browser", not literally Google Chrome. Narrowing to
//     profiles named exactly "chrome" here regressed every Brave/Edge/
//     Vivaldi user who set a custom path through the Web UI. Treated
//     as "every profile is a candidate", identically to empty —
//     dpapi.FindBrowserProfiles only ever returns Chromium-family
//     profiles in the first place, so "the whole family" and "no
//     filter" are the same set.
//     - Any other configured type (reachable only via the TUI's
//     free-text browser_type field — browser_validate.go's
//     knownBrowserTypes allows "brave"/"edge"/"vivaldi"/"opera"/
//     "thorium" individually): if dpapi.KnownBrowserFamilies() has NO
//     layout for it at all (Opera's profile layout differs and is
//     deliberately excluded from dpapi.chromiumBrowsers; Thorium has
//     simply never been added), filtering would always produce zero
//     candidates regardless of what's installed — that is a dpapi
//     coverage gap, not a "browser not found", so it falls back to
//     every profile being a candidate, logged at Debug. Otherwise
//     ("brave", "edge", "vivaldi", literal "chrome" if it were ever
//     reached this way) it narrows to that browser FAMILY —
//     dpapiBrowserMatchesConfigured also matches release-channel
//     siblings ("edge" matches "edge-beta"); non-matches are skipped
//     and logged at Debug; zero matches here IS a real "browser not
//     found" and is a hard error.
//  2. Each candidate is scored by dpapiProfileScore (the loose/strict auth
//     predicates jar.go already keeps, applied to the profile's own rows
//     without building a full CookieJar). Highest score wins; a tie keeps
//     FindBrowserProfiles' scan order (first candidate) and is logged at
//     Info naming every tied profile.
//  3. The winner's rows — and ONLY the winner's — go to
//     deduplicateAndFormat. The choice (browser, profile name, score) is
//     logged at Info; every other candidate is logged at Debug.
//
// Known limitation, not built here: the score sums BOTH platforms into one
// number, so a user signed in to YouTube on profile A and Twitch on
// profile B still loses one platform on this path — deterministically and
// logged now, instead of silently. A two-pass, per-platform selection
// would fix that; it is a larger change than this ruling and is not part
// of Arc 8.
//
// Returns "" + error when no profiles are found, when the configured
// browser has no matching profiles, when every candidate profile hits a
// fatal decode error (master-key load failure, SQLite open failure), or
// when the chosen profile holds no relevant cookies. Per-row decryption
// failures do not fail a profile's read — the underlying read is fail-soft
// on legacy / mismatched / App-Bound rows — but they are COUNTED BY REASON
// and reported, because "nothing came out" has several causes that need
// different responses and used to be indistinguishable.
//
// On non-Windows hosts, dpapi.FindBrowserProfiles returns an empty slice
// and dpapi.ReadChromeCookiesStats returns ErrNotSupported, so this
// function returns the "no profiles found" error without crashing.
func dpapiExtractAsNetscape(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}, configuredBrowserType string) (string, error) {
	allProfiles := dpapiFindBrowserProfiles()
	if len(allProfiles) == 0 {
		return "", fmt.Errorf("DPAPI fallback: no Chromium-family profiles found under LOCALAPPDATA")
	}

	profiles := allProfiles
	switch {
	case configuredBrowserType == "":
		// Auto-detect: every profile is a candidate.
	case configuredBrowserType == dpapiChromiumFamilyValue:
		// Finding 1 (Arc 8 fix round 1): the Web UI's ONLY Chromium option
		// stores this literal value for "some Chromium-family browser",
		// not "Google Chrome specifically" — see the doc comment above.
		// Narrowing to it would exclude every Brave/Edge/Vivaldi profile a
		// Web UI user configured. dpapi.FindBrowserProfiles only returns
		// Chromium-family profiles anyway, so "the whole family" and "no
		// filter" are the same set — no Debug line needed, this is the
		// expected common case, not a fallback from a broken one.
	case !slices.Contains(dpapi.KnownBrowserFamilies(), configuredBrowserType):
		// Finding 2: a browser_type browser_validate.go accepts (Opera,
		// Thorium) but dpapi has no profile layout for at all — filtering
		// would always yield zero candidates on every machine, which is a
		// dpapi coverage gap, not "this browser isn't installed". Falling
		// back to unfiltered scoring instead of a hard error here is what
		// the OLD code effectively did (it never filtered by browser at
		// all), so this restores that behavior for exactly the types H7
		// cannot filter for.
		if logger != nil {
			logger.Debug("DPAPI fallback: configured browser has no dpapi profile layout — scoring every profile instead of filtering",
				"configured", configuredBrowserType)
		}
	default:
		var filtered []dpapi.BrowserProfile
		for _, p := range allProfiles {
			if dpapiBrowserMatchesConfigured(configuredBrowserType, p.Browser) {
				filtered = append(filtered, p)
				continue
			}
			if logger != nil {
				logger.Debug("DPAPI fallback: skipping profile — does not match configured browser",
					"browser", p.Browser, "profile", p.Name, "configured", configuredBrowserType)
			}
		}
		if len(filtered) == 0 {
			return "", fmt.Errorf("DPAPI fallback: configured browser %q has no profiles under LOCALAPPDATA (found: %s)",
				configuredBrowserType, dpapiBrowsersFound(allProfiles))
		}
		profiles = filtered
	}

	type candidate struct {
		profile dpapi.BrowserProfile
		cookies []extractedCookie
		stats   dpapi.ChromeReadStats
		score   int
	}

	var candidates []candidate
	var lastErr error
	for _, p := range profiles {
		// Read with empty origin filter: deduplicateAndFormat does the
		// YouTube/Google/Twitch filtering downstream, and a per-profile
		// read costs ~50 ms even for a thousand-row Chrome profile.
		// Keeping the filter empty here avoids the multi-call pattern
		// that would re-open the SQLite DB once per host.
		cookies, profileStats, err := dpapiReadChromeCookiesStats(p.Path, "")
		if summary := profileStats.Summary(); summary != "" && logger != nil {
			logger.Warn("DPAPI fallback skipped undecryptable cookies",
				"browser", p.Browser, "profile", p.Name, "detail", summary)
		}
		if err != nil {
			lastErr = fmt.Errorf("read %s/%s: %w", p.Browser, p.Name, err)
			continue
		}
		extracted := make([]extractedCookie, 0, len(cookies))
		for _, c := range cookies {
			extracted = append(extracted, extractedCookie{
				domain:   c.Host,
				httpOnly: c.HttpOnly,
				path:     c.Path,
				secure:   c.Secure,
				expiry:   c.Expires,
				name:     c.Name,
				value:    c.Value,
			})
		}
		candidates = append(candidates, candidate{
			profile: p,
			cookies: extracted,
			stats:   profileStats,
			score:   dpapiProfileScore(extracted),
		})
	}

	if len(candidates) == 0 {
		// Every candidate failed to open. Surface the last error so the
		// operator sees something more useful than "no cookies".
		if lastErr != nil {
			return "", fmt.Errorf("DPAPI fallback: every profile failed: %w", lastErr)
		}
		return "", fmt.Errorf("DPAPI fallback: no readable profiles")
	}

	// H7 selection: highest score wins. Ties keep FindBrowserProfiles' scan
	// order — "first candidate" — and are logged at Info naming every tied
	// profile so the ambiguity is visible instead of silent.
	best := 0
	tied := []int{0}
	for i := 1; i < len(candidates); i++ {
		switch {
		case candidates[i].score > candidates[best].score:
			best = i
			tied = []int{i}
		case candidates[i].score == candidates[best].score:
			tied = append(tied, i)
		}
	}
	if len(tied) > 1 && logger != nil {
		names := make([]string, len(tied))
		for j, idx := range tied {
			names[j] = candidates[idx].profile.Browser + "/" + candidates[idx].profile.Name
		}
		logger.Info("DPAPI fallback: tied profile score — first in scan order wins",
			"score", candidates[best].score, "profiles", strings.Join(names, ", "))
	}

	chosen := candidates[best]
	if logger != nil {
		logger.Info("DPAPI fallback: chose one profile — cookies are never merged across profiles",
			"browser", chosen.profile.Browser, "profile", chosen.profile.Name, "score", chosen.score)
		for i, c := range candidates {
			if i == best {
				continue
			}
			logger.Debug("DPAPI fallback: passed over profile",
				"browser", c.profile.Browser, "profile", c.profile.Name, "score", c.score)
		}
	}

	filtered := deduplicateAndFormat(chosen.cookies)
	if len(filtered) == 0 {
		// This is where the whole pass reports as "nothing came out", and it
		// is the one message the operator gets. Naming WHY the rows were
		// skipped is the difference between "DPAPI cannot work on your
		// browser at all" (App-Bound), "this profile is not yours"
		// (master-key mismatch) and "you are simply not signed in". Only
		// the CHOSEN profile's stats are reported — the other candidates'
		// skip reasons don't explain why the profile that was actually
		// used came up empty.
		if summary := chosen.stats.Summary(); summary != "" {
			return "", fmt.Errorf("DPAPI fallback: no relevant cookies in chosen profile %s/%s (%s)",
				chosen.profile.Browser, chosen.profile.Name, summary)
		}
		return "", fmt.Errorf("DPAPI fallback: no relevant cookies in chosen profile %s/%s",
			chosen.profile.Browser, chosen.profile.Name)
	}

	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Extracted by Moombox auto-cookie service (DPAPI)")
	lines = append(lines, "")
	lines = append(lines, filtered...)
	return strings.Join(lines, "\n") + "\n", nil
}

// dpapiChromiumFamilyValue is the browser_type value the Web UI's
// cfg-cookies-browser-type dropdown stores for ANY Chromium-family choice
// (web/public/index.html: `<sl-option value="chrome">Chromium-family
// (chrome, brave, edge, vivaldi, thorium, opera)</sl-option>` — its only
// other option is "firefox" for the whole Firefox family). The Web UI has
// no way to express a narrower per-browser choice; only the TUI's
// free-text browser_type field can (browser_validate.go's
// knownBrowserTypes allows "brave"/"edge"/"vivaldi"/"opera"/"thorium"
// individually there). See dpapiExtractAsNetscape's Finding 1 note.
const dpapiChromiumFamilyValue = "chrome"

// dpapiBrowserMatchesConfigured reports whether one FindBrowserProfiles
// entry's Browser family matches the operator's configured browser type
// (H7). "chrome" matches "chrome", "chrome-beta", "chrome-dev", and
// "chrome-canary" — a release channel is still the same physical browser
// the operator picked in settings, and knownBrowserTypes (browser_validate.go)
// only offers the coarse family names, never the per-channel ones. "brave",
// "vivaldi", and "chromium" have no channel siblings in
// dpapi.chromiumBrowsers and match only themselves.
func dpapiBrowserMatchesConfigured(configuredType, profileBrowser string) bool {
	if profileBrowser == configuredType {
		return true
	}
	return strings.HasPrefix(profileBrowser, configuredType+"-")
}

// dpapiBrowsersFound lists the distinct Browser families FindBrowserProfiles
// actually found, in scan order, for the "configured browser has zero
// candidates" error message — naming what IS there is more useful to an
// operator than a bare "not found".
func dpapiBrowsersFound(profiles []dpapi.BrowserProfile) string {
	seen := make(map[string]bool, len(profiles))
	order := make([]string, 0, len(profiles))
	for _, p := range profiles {
		if seen[p.Browser] {
			continue
		}
		seen[p.Browser] = true
		order = append(order, p.Browser)
	}
	return strings.Join(order, ", ")
}

// dpapiProfileScore rates how complete an auth session one profile's
// cookies carry, for H7's "pick one profile" ranking — higher wins.
//
// Two tiers per platform, mirroring the loose/strict predicates jar.go
// already keeps for the identical reason (CookieJar.HasYouTubeAuthCookies
// vs HasAnyYouTubeAuthCookie, and the Twitch counterparts
// HasTwitchAuthCookies vs HasAnyTwitchAuthCookie): a COMPLETE working set
// outranks a merely-partial one, and a partial set outranks nothing. This
// is computed directly off the profile's own extractedCookie rows rather
// than by building a CookieJar — the same essential-name sets, no file
// round-trip needed for a throwaway score.
//
// The two platforms are summed into ONE score because this function picks
// a single profile for BOTH platforms at once — see
// dpapiExtractAsNetscape's doc comment for the known limitation that
// follows from that and why a per-platform split is not built here.
func dpapiProfileScore(cookies []extractedCookie) int {
	const (
		completeAuthScore = 10
		partialAuthScore  = 1
	)

	var hasSapisid, hasLoginInfo, hasAnyYouTubeAuth bool
	var hasAuthToken, hasTwilightUser bool

	for _, c := range cookies {
		if c.value == "" {
			continue
		}
		switch {
		case isYouTubeDomain(c.domain) || isGoogleDomain(c.domain):
			switch c.name {
			case "SAPISID", "__Secure-3PAPISID":
				hasSapisid = true
			case "LOGIN_INFO":
				hasLoginInfo = true
			}
			for _, n := range youtubeAuthCookieNames {
				if c.name == n {
					hasAnyYouTubeAuth = true
					break
				}
			}
		case isTwitchDomain(c.domain):
			switch c.name {
			case "auth-token":
				hasAuthToken = true
			case "twilight-user":
				hasTwilightUser = true
			}
		}
	}

	score := 0
	switch {
	case hasSapisid && hasLoginInfo:
		score += completeAuthScore
	case hasAnyYouTubeAuth:
		score += partialAuthScore
	}
	switch {
	case hasAuthToken:
		score += completeAuthScore
	case hasTwilightUser:
		score += partialAuthScore
	}
	return score
}

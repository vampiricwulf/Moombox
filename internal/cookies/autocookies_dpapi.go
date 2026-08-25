package cookies

import (
	"fmt"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/cookies/dpapi"
)

// dpapiExtractAsNetscape walks every Chromium-family profile that
// dpapi.FindBrowserProfiles can see, reads cookies from each, runs the
// merged result through deduplicateAndFormat (which keeps only the
// YouTube / Google / Twitch essentials and prefers youtube.com over
// google.com on dedup conflicts), and returns the Netscape cookies.txt
// content ready for writeFileAtomic.
//
// Used as a fallback when the CDP refresh path can't acquire the
// profile lock — DPAPI doesn't launch a browser, so it sidesteps the
// "Chromium already running our profile" failure mode entirely.
//
// Returns "" + error when no profiles are found, when none of the
// found profiles contain relevant cookies, or when every profile
// hits a fatal decode error (master-key load failure, SQLite open
// failure). Per-row decryption failures do not fail the pass — the
// underlying read is fail-soft on legacy / mismatched / App-Bound
// rows — but they are COUNTED BY REASON and reported, because
// "nothing came out" has several causes that need different
// responses and used to be indistinguishable.
//
// On non-Windows hosts, dpapi.FindBrowserProfiles returns an empty
// slice and dpapi.ReadChromeCookiesStats returns ErrNotSupported, so
// this function returns the "no profiles found" error without crashing.
func dpapiExtractAsNetscape(logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}) (string, error) {
	profiles := dpapi.FindBrowserProfiles()
	if len(profiles) == 0 {
		return "", fmt.Errorf("DPAPI fallback: no Chromium-family profiles found under LOCALAPPDATA")
	}

	var collected []extractedCookie
	var lastErr error
	var stats dpapi.ChromeReadStats
	profilesRead := 0
	for _, p := range profiles {
		// Read with empty origin filter: deduplicateAndFormat does the
		// YouTube/Google/Twitch filtering downstream, and a
		// per-profile read costs ~50 ms even for a thousand-row Chrome
		// profile. Keeping the filter empty here avoids the multi-call
		// pattern that would re-open the SQLite DB once per host.
		cookies, profileStats, err := dpapi.ReadChromeCookiesStats(p.Path, "")
		stats.Add(profileStats)
		if summary := profileStats.Summary(); summary != "" && logger != nil {
			logger.Warn("DPAPI fallback skipped undecryptable cookies",
				"browser", p.Browser, "profile", p.Name, "detail", summary)
		}
		if err != nil {
			lastErr = fmt.Errorf("read %s/%s: %w", p.Browser, p.Name, err)
			continue
		}
		profilesRead++
		for _, c := range cookies {
			collected = append(collected, extractedCookie{
				domain:   c.Host,
				httpOnly: c.HttpOnly,
				path:     c.Path,
				secure:   c.Secure,
				expiry:   c.Expires,
				name:     c.Name,
				value:    c.Value,
			})
		}
	}

	if profilesRead == 0 {
		// Every profile failed to open. Surface the last error so the
		// operator sees something more useful than "no cookies".
		if lastErr != nil {
			return "", fmt.Errorf("DPAPI fallback: every profile failed: %w", lastErr)
		}
		return "", fmt.Errorf("DPAPI fallback: no readable profiles")
	}

	filtered := deduplicateAndFormat(collected)
	if len(filtered) == 0 {
		// This is where the whole pass reports as "nothing came out", and
		// it is the one message the operator gets. Naming WHY the rows were
		// skipped is the difference between "DPAPI cannot work on your
		// browser at all" (App-Bound), "this profile is not yours"
		// (master-key mismatch) and "you are simply not signed in".
		if summary := stats.Summary(); summary != "" {
			return "", fmt.Errorf("DPAPI fallback: no relevant cookies in any profile (read %d profile(s); %s)", profilesRead, summary)
		}
		return "", fmt.Errorf("DPAPI fallback: no relevant cookies in any profile (read %d profile(s))", profilesRead)
	}

	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Extracted by Moombox auto-cookie service (DPAPI)")
	lines = append(lines, "")
	lines = append(lines, filtered...)
	return strings.Join(lines, "\n") + "\n", nil
}

package main

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCookieRefreshReportFor pins the worker-facing wording for every verdict,
// and in particular the split inside RefreshFailed.
//
// The load-bearing row is the YouTube-only install that meets a
// subscriber-only Twitch VOD. That job asks for a Twitch cookie refresh —
// attemptCookieRefresh has no cookies-present gate, and Usher's 403 cannot
// tell an anonymous session from an un-entitled one, so cookiesStatusError
// admits it. The install's YouTube cookies keep refreshPlatforms() non-empty
// so the refresh really runs, and checkPlatformAuth reports Twitch as
// {hasCookies: false, verifyFailed} — a conclusive RefreshFailed for a
// platform that was never configured.
//
// The verdict is right. "The stored cookies for this platform are dead —
// replace them" is not: there are none, nothing was rejected, and the remedy
// is to add credentials rather than replace them. Naming that cause is the
// same unearned assertion the rest of this arc exists to remove.
func TestCookieRefreshReportFor(t *testing.T) {
	// A YouTube-only install: YouTube verifies, Twitch has nothing.
	youTubeOnly := cookies.RefreshResult{
		Ran:     true,
		YouTube: cookies.RefreshOK, YouTubeStored: true,
		Twitch: cookies.RefreshFailed, TwitchStored: false,
	}
	// The same install once the YouTube cookies have actually expired.
	bothStoredBothDead := cookies.RefreshResult{
		Ran:     true,
		YouTube: cookies.RefreshFailed, YouTubeStored: true,
		Twitch: cookies.RefreshFailed, TwitchStored: true,
	}
	declined := cookies.RefreshResult{}

	cases := []struct {
		name     string
		platform string
		result   cookies.RefreshResult
		wantOK   bool
		// wantSaid / wantUnsaid are substrings of the msg+note pair.
		wantSaid   []string
		wantUnsaid []string
	}{
		{
			name:     "verified platform says nothing and retries",
			platform: "youtube",
			result:   youTubeOnly,
			wantOK:   true,
		},
		{
			// THE FIX.
			name:       "twitch job on a youtube-only install is not told its cookies died",
			platform:   "twitch",
			result:     youTubeOnly,
			wantOK:     false,
			wantSaid:   []string{"holds no cookies", "nothing was rejected"},
			wantUnsaid: []string{"are dead", "still rejected", "replace"},
		},
		{
			name:       "stored credentials that were rejected may be called dead",
			platform:   "youtube",
			result:     bothStoredBothDead,
			wantOK:     false,
			wantSaid:   []string{"still rejected", "are dead", "replace"},
			wantUnsaid: []string{"holds no cookies"},
		},
		{
			name:       "a declined pass asserts nothing at all",
			platform:   "youtube",
			result:     declined,
			wantOK:     false,
			wantSaid:   []string{"no usable cookies", "declined to run"},
			wantUnsaid: []string{"are dead", "rejected", "holds no cookies"},
		},
		{
			name:       "an unrecognised platform asserts nothing at all",
			platform:   "kick",
			result:     bothStoredBothDead,
			wantOK:     false,
			wantSaid:   []string{"no usable cookies"},
			wantUnsaid: []string{"are dead", "rejected", "holds no cookies"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cookieRefreshReportFor(tc.platform, tc.result)

			if got.ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", got.ok, tc.wantOK)
			}
			if tc.wantOK {
				if got.msg != "" {
					t.Errorf("a successful refresh must not log a complaint, got %q", got.msg)
				}
				return
			}
			if got.msg == "" {
				t.Fatal("a refresh that did not restore auth must say something")
			}
			said := got.msg + " " + got.note
			for _, want := range tc.wantSaid {
				if !strings.Contains(said, want) {
					t.Errorf("report does not say %q: %q", want, said)
				}
			}
			for _, unwanted := range tc.wantUnsaid {
				if strings.Contains(said, unwanted) {
					t.Errorf("report asserts %q, which this verdict does not establish: %q", unwanted, said)
				}
			}
		})
	}
}

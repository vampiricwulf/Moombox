package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/worker"
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
	// Ran, but stopped before it could verify — an I/O failure, or a check
	// that could not reach the service. Differs from `declined` in one bit.
	aborted := cookies.RefreshResult{Ran: true}

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
			name:     "twitch job on a youtube-only install is not told its cookies died",
			platform: "twitch",
			result:   youTubeOnly,
			wantOK:   false,
			// The same line also has to serve a total expiry, where every
			// stored row was pruned by this very refresh — so it names both
			// possibilities and asserts neither.
			wantSaid:   []string{"no credentials for this platform", "nothing was rejected", "or none were ever supplied"},
			wantUnsaid: []string{"are dead", "still rejected"},
		},
		{
			name:       "stored credentials that were rejected may be called dead",
			platform:   "youtube",
			result:     bothStoredBothDead,
			wantOK:     false,
			wantSaid:   []string{"still rejected", "are dead", "replace"},
			wantUnsaid: []string{"no credentials for this platform", "declined"},
		},
		{
			// Ran == false. The line may now say WHICH kind of nothing
			// happened, because that is exactly the distinction Ran draws.
			name:       "a declined pass says it declined and nothing more",
			platform:   "youtube",
			result:     declined,
			wantOK:     false,
			wantSaid:   []string{"declined to run", "nothing was learned"},
			wantUnsaid: []string{"are dead", "rejected", "no credentials for this platform"},
		},
		{
			// Ran == true, verdicts Unknown. Must NOT claim it declined — it
			// did run, it just could not conclude.
			name:       "an aborted pass says it ran and concluded nothing",
			platform:   "youtube",
			result:     aborted,
			wantOK:     false,
			wantSaid:   []string{"ran but could not establish", "may", "be perfectly fine"},
			wantUnsaid: []string{"declined", "are dead", "rejected"},
		},
		{
			name:       "an unrecognised platform asserts nothing at all",
			platform:   "kick",
			result:     bothStoredBothDead,
			wantOK:     false,
			wantSaid:   []string{"could not establish"},
			wantUnsaid: []string{"are dead", "rejected", "no credentials for this platform"},
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

// TestCookieRefreshErrorLine: a job-triggered refresh that skipped a profile
// another browser holds (cookies.ErrProfileInUse) was logged as "auto cookie
// refresh error". The pass declined and launched nothing, and its sentence
// names the host and the lock to delete, so the line says "skipped" and carries
// the sentence as the reason, and the worker is told it was skipped. Any other
// error keeps its line and is not a restore.
//
// Mutants (checked): the ErrProfileInUse arm removed — the held profile's line
// is the error one again; that arm answering CookieRefreshNotRestored — the
// worker follows the skip with its failed-refresh advice.
func TestCookieRefreshErrorLine(t *testing.T) {
	held := fmt.Errorf("%w by desktop-pc — close it there, or delete %q", cookies.ErrProfileInUse, "/profile/SingletonLock")
	msg, attr, outcome := cookieRefreshErrorLine(held)
	if msg != "automatic cookie refresh skipped — a browser holds the profile" {
		t.Errorf("held profile: message %q, want the skip line", msg)
	}
	if attr.Key != "reason" || attr.Value.String() != held.Error() {
		t.Errorf("held profile: attribute %s=%q, want reason=<the sentence>", attr.Key, attr.Value.String())
	}
	if outcome != worker.CookieRefreshSkipped {
		t.Errorf("held profile: outcome %v, want CookieRefreshSkipped", outcome)
	}

	other := errors.New("start headless browser: exec: no such file")
	msg, attr, outcome = cookieRefreshErrorLine(other)
	if msg != "auto cookie refresh error" || attr.Key != "error" || attr.Value.String() != other.Error() {
		t.Errorf("other error: %q %s=%q, want the error line unchanged", msg, attr.Key, attr.Value.String())
	}
	if outcome != worker.CookieRefreshNotRestored {
		t.Errorf("other error: outcome %v, want CookieRefreshNotRestored", outcome)
	}
}

// jobRefreshLogger records Warn lines with their attributes rendered, so a
// test can assert on what the job-triggered refresh logged. The other three
// levels are discarded.
type jobRefreshLogger struct {
	warns []string
}

func (l *jobRefreshLogger) Debug(string, ...any) {}
func (l *jobRefreshLogger) Info(string, ...any)  {}
func (l *jobRefreshLogger) Error(string, ...any) {}
func (l *jobRefreshLogger) Warn(msg string, args ...any) {
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		if attr, ok := a.(slog.Attr); ok {
			b.WriteString(" " + attr.Key + "=" + attr.Value.String())
		}
	}
	l.warns = append(l.warns, b.String())
}

// TestJobCookieRefreshOutcome pins the closure-to-worker contract of the
// job-triggered refresh: what OnCookieRefreshNeeded logs and what it tells
// the worker. The held profile is the row this exists for. The closure logged
// its skip line and answered false, and the worker, which could not tell that
// false from a failure, followed the skip with "auto cookie refresh failed —
// the cookie file has to be replaced by hand". It answers
// worker.CookieRefreshSkipped now, which the worker leaves parked without the
// advice (TestHeldProfileSkipIsNotCalledAFailedRefresh in internal/worker).
//
// Mutants (checked): the error arm answering CookieRefreshNotRestored instead
// of cookieRefreshErrorLine's outcome — the held row fails; the report's ok
// not mapped to CookieRefreshRestored — the verified row fails.
func TestJobCookieRefreshOutcome(t *testing.T) {
	held := fmt.Errorf("%w by desktop-pc — close it there, or delete %q", cookies.ErrProfileInUse, "/profile/SingletonLock")
	verified := cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshOK, YouTubeStored: true}
	rejected := cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshFailed, YouTubeStored: true}

	cases := []struct {
		name     string
		result   cookies.RefreshResult
		err      error
		want     worker.CookieRefreshOutcome
		wantWarn string // "" for no line
	}{
		{"held profile is a skip", cookies.RefreshResult{Mechanism: cookies.RefreshMechanismBrowser}, held,
			worker.CookieRefreshSkipped, "automatic cookie refresh skipped — a browser holds the profile platform=youtube reason=" + held.Error()},
		{"another error is not a restore", cookies.RefreshResult{}, errors.New("start headless browser: exec: no such file"),
			worker.CookieRefreshNotRestored, "auto cookie refresh error platform=youtube error=start headless browser: exec: no such file"},
		{"a verified platform is restored", verified, nil, worker.CookieRefreshRestored, ""},
		{"rejected credentials are not a restore", rejected, nil,
			worker.CookieRefreshNotRestored, "automatic cookie refresh ran and the credentials are still rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &jobRefreshLogger{}
			if got := jobCookieRefreshOutcome(log, "youtube", tc.result, tc.err); got != tc.want {
				t.Errorf("outcome %v, want %v", got, tc.want)
			}
			switch {
			case tc.wantWarn == "" && len(log.warns) != 0:
				t.Errorf("logged %q, want nothing", log.warns)
			case tc.wantWarn != "" && (len(log.warns) != 1 || !strings.HasPrefix(log.warns[0], tc.wantWarn)):
				t.Errorf("logged %q, want one line starting %q", log.warns, tc.wantWarn)
			}
		})
	}
}

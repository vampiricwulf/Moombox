package cookies

import (
	"context"
	"testing"
)

// The three shapes a browser refresh can hand back, as Netscape text.
//
// anonymousBrowserRows is the state this file exists for: real rows, none of
// them a credential. YSC and VISITOR_INFO1_LIVE are in essentialYouTubeCookies
// (so CookieJar.Load keeps them and countNetscapeCookieRows counts them) and in
// NEITHER youtubeAuthCookieNames nor twitchAuthCookieNames — which is exactly
// what an anonymous visit to youtube.com sets, and what a profile that clears
// cookies on exit is re-seeded with by the navigation the refresh just made.
const anonymousBrowserRows = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tFALSE\t0\tYSC\tfixture-ysc\n" +
	".youtube.com\tTRUE\t/\tFALSE\t0\tVISITOR_INFO1_LIVE\tfixture-visitor\n"

// signedInBrowserRows is the same read from a browser that IS signed in.
const signedInBrowserRows = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfixture-fetched-sapisid\n" +
	"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tfixture-fetched-login\n"

// stubBrowserRefresh points the Chromium refresh step at a fixed answer for one
// test. The seam is a package var, so the restore is not optional: leaving it
// reassigned would silently stub every later test in the package.
func stubBrowserRefresh(t *testing.T, netscape string, navigated bool, err error) {
	t.Helper()
	real := refreshChromiumCookies
	refreshChromiumCookies = func(_ *AutoCookieService, _ context.Context, _ *DetectedBrowser) (string, bool, error) {
		return netscape, navigated, err
	}
	t.Cleanup(func() { refreshChromiumCookies = real })
}

// signedOutRefreshService builds a service whose refresh will take the BROWSER
// path (not the import path) and whose verification conclusively fails.
//
// The pre-existing cookies.txt holds real YouTube credentials, and it has to:
// the browser branch is gated on refreshPlatforms(), so a jar with nothing in
// it declines the pass before any of this is reached. It is also what makes the
// case under test the interesting one — the credentials on disk survive the
// merge, so this is a browser that came back signed-out over a session that is
// still configured.
func signedOutRefreshService(t *testing.T) *AutoCookieService {
	t.Helper()
	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}
	s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath, jar,
		nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if len(s.refreshPlatforms()) == 0 {
		t.Fatal("fixture is broken — the jar must hold YouTube auth or the browser branch declines " +
			"before it fetches anything")
	}
	return s
}

// The three sentences the outcome switch can produce for a pass whose
// verification failed, spelled out so each test row can assert the one it means
// AND deny the other two. Written here rather than inlined because the whole
// point of the new arm is that these three are DIFFERENT answers; a row that
// only asserted its own string would pass just as happily if the switch
// collapsed to one message.
const (
	signedOutMessage = "the browser profile returned 2 cookies but none of them is a session " +
		"credential — the browser is signed out"
	emptyProfileMessage = "YouTube auth verification failed, and the browser profile contained " +
		"no cookies to refresh from — check whether the browser is clearing cookies on exit"
	reloginMessage = "YouTube auth verification failed — manual re-login required"
)

// TestRefreshNamesARowfulOfNonCredentials is the state Arc 2 found and the
// outcome switch could not say.
//
// The browser answered, it handed back rows, and NOT ONE of them is a session
// credential. Before this arm the pass had two things it could say and both
// were wrong in their own way:
//
//   - "the browser profile contained no cookies to refresh from" is FALSE.
//     Cookies came back; the read worked. That arm belongs to a read that
//     produced nothing at all, and telling this operator to check whether their
//     browser clears cookies on exit sends them after a setting that is not the
//     problem.
//   - "manual re-login required" is TRUE but uninformative, and it is the wrong
//     half of the truth: the thing to fix is that the BROWSER is signed out, so
//     the remedy is to sign in there — not to wonder why Moombox's check failed
//     over credentials that are still sitting in cookies.txt.
//
// Every row asserts its own message AND denies the other two, because the
// defect this arm removes is precisely three situations wearing one sentence.
func TestRefreshNamesARowfulOfNonCredentials(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fetched   string
		fetchErr  error
		want      string
		wantNot   []string
		mustBeNew bool
	}{
		{
			// The new arm.
			name:      "rows came back and none is a credential",
			fetched:   anonymousBrowserRows,
			want:      signedOutMessage,
			wantNot:   []string{emptyProfileMessage, reloginMessage},
			mustBeNew: true,
		},
		{
			// A credential DID come back and the site still said no. Nothing
			// about the browser to report; the session is simply dead.
			name:    "rows came back and one of them is a credential",
			fetched: signedInBrowserRows,
			want:    reloginMessage,
			wantNot: []string{signedOutMessage, emptyProfileMessage},
		},
		{
			// THE UNCHANGED ARM. emptyBrowserProfile must be untouched by the
			// new flag: the read produced no text at all, so there are no rows
			// to have judged, and the sentence about clearing cookies on exit is
			// the right one.
			name:     "no rows came back at all",
			fetched:  "",
			fetchErr: ErrNoCookiesInProfile,
			want:     emptyProfileMessage,
			wantNot:  []string{signedOutMessage, reloginMessage},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := signedOutRefreshService(t)
			stubBrowserRefresh(t, tc.fetched, true, tc.fetchErr)

			// The premise for the row that matters, checked against the same
			// predicates the flag is built from rather than by eye.
			if tc.mustBeNew {
				if countNetscapeCookieRows(tc.fetched) == 0 {
					t.Fatal("premise lost: the fixture has no rows, so this is the empty-profile " +
						"state and not the one under test")
				}
				if netscapeCookiesHoldACredential(tc.fetched) {
					t.Fatal("premise lost: the fixture carries a credential, so the flag is false " +
						"and the row below is testing the ordinary re-login arm")
				}
			}

			if _, err := s.RefreshCookiesDetailed(context.Background()); err != nil {
				t.Fatalf("RefreshCookiesDetailed: %v", err)
			}

			got := lastErrorSnapshot(s)
			if got != tc.want {
				t.Errorf("LastError =\n  %q\nwant\n  %q", got, tc.want)
			}
			for _, unwanted := range tc.wantNot {
				if got == unwanted {
					t.Errorf("LastError is the wrong arm's sentence: %q", got)
				}
			}
		})
	}
}

// TestSignedOutFlagDoesNotDisturbTheRowCounter keeps the two questions apart at
// the level the ticket insisted on: a NEW flag, never a redefinition.
//
// countNetscapeCookieRows over-counts by construction and the import guard's
// safety rests on that (TestRowCounterOverCountsByConstruction pins it). If the
// signed-out state had been folded into the counter — "count only credentials"
// being the obvious way to get one number to answer both questions — a
// freshly-mounted profile full of unparseable rows would read as "nothing to
// lose" and get imported over. So the two must disagree, and here they do.
func TestSignedOutFlagDoesNotDisturbTheRowCounter(t *testing.T) {
	if got := countNetscapeCookieRows(anonymousBrowserRows); got != 2 {
		t.Errorf("countNetscapeCookieRows(anonymous rows) = %d, want 2 — the counter answers "+
			"\"is there anything here\" and must keep counting rows that are not credentials", got)
	}
	if netscapeCookiesHoldACredential(anonymousBrowserRows) {
		t.Error("netscapeCookiesHoldACredential(anonymous rows) = true — YSC and " +
			"VISITOR_INFO1_LIVE are what an ANONYMOUS visit sets; reading them as a login is " +
			"how a signed-out browser stops being reported")
	}
	if !netscapeCookiesHoldACredential(signedInBrowserRows) {
		t.Error("netscapeCookiesHoldACredential(signed-in rows) = false — a false negative here " +
			"claims a signed-in browser is signed out, which sends the operator to re-do a " +
			"login that already worked")
	}
}

// TestNetscapeCredentialProbeUsesTheJarsOwnPredicates pins the delegation
// rather than the answers.
//
// The probe must agree with HasAnyYouTubeAuthCookie / HasAnyTwitchAuthCookie on
// every input, because those are what refreshPlatforms(), checkPlatformAuth and
// doRefresh's hadYTAuth/hadTWAuth already use — one question, one answer,
// across the package. A hand-rolled name match over the Netscape text would
// pass a table of its own while disagreeing with the jar the moment either name
// list changed.
func TestNetscapeCredentialProbeUsesTheJarsOwnPredicates(t *testing.T) {
	for _, tc := range []struct{ name, netscape string }{
		{"nothing", "# Netscape HTTP Cookie File\n"},
		{"anonymous youtube rows", anonymousBrowserRows},
		{"signed-in youtube rows", signedInBrowserRows},
		{"a twitch session", "# Netscape HTTP Cookie File\n" +
			"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tfixture-token\n"},
		{"a twitch profile with no token", "# Netscape HTTP Cookie File\n" +
			".twitch.tv\tTRUE\t/\tFALSE\t0\tname\tfixture-name\n"},
		{"rows for a site the jar does not track", ".example.com\tTRUE\t/\tFALSE\t0\tsid\tfixture\n"},
		{"a malformed row", "not\ta\tvalid\tcookie\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reference := NewCookieJar()
			reference.loadFrom([]byte(tc.netscape), "")
			want := reference.HasAnyYouTubeAuthCookie() || reference.HasAnyTwitchAuthCookie()
			if got := netscapeCookiesHoldACredential(tc.netscape); got != want {
				t.Errorf("netscapeCookiesHoldACredential = %v, want %v — the probe must be the "+
					"jar's own loose predicates over the same text, not a second reading of it",
					got, want)
			}
		})
	}
}

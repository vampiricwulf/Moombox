package youtube

import (
	"context"
	"crypto/sha1"
	"fmt"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// wantSidHash recomputes one SAPISIDHASH-family token from yt-dlp's
// _make_sid_authorization formula (_base.py:762-775) on the timestamp the
// header itself carries, so a test needs no clock: the hash input is
// "[<uid> ]<ts> <sid> <origin>", and "_u" follows the hash when a user
// session id is bound.
func wantSidHash(scheme, sid, uid, token string) string {
	ts, _, _ := strings.Cut(token, "_")
	input := ts + " " + sid + " https://www.youtube.com"
	suffix := ""
	if uid != "" {
		input = uid + " " + input
		suffix = "_u"
	}
	return fmt.Sprintf("%s %s_%x%s", scheme, ts, sha1.Sum([]byte(input)), suffix)
}

// GenerateAPIHeaders binds the Authorization header to the ytcfg's user
// session id as yt-dlp's _generate_cookie_auth_headers does
// (_extract_user_session_id, _base.py:970): "<uid> " leads the hash input and
// "_u" follows the hash. A ytcfg naming no user session id — or none at all —
// sends today's unbound form exactly.
//
// Mutants: GenerateAPIHeaders sending the unbound jar form
// (the bound row loses "_u"); passing the delegated id instead of the user
// session id (the bound row's hash differs).
func TestGenerateAPIHeadersBindsTheUserSessionID(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	auth := NewAuth(jar, noopLogger{})
	for _, tc := range []struct {
		name  string
		ytcfg *YtcfgData
		uid   string
	}{
		{"bound", &YtcfgData{UserSessionID: "user-u", DelegatedSessionID: "brand-d"}, "user-u"},
		{"no user session id", &YtcfgData{DelegatedSessionID: "brand-d"}, ""},
		{"no ytcfg", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := auth.GenerateAPIHeaders(constants.WebClient, tc.ytcfg)["Authorization"]
			scheme, token, ok := strings.Cut(got, " ")
			if !ok || scheme != "SAPISIDHASH" || strings.Contains(token, " ") {
				t.Fatalf("Authorization = %q, want one SAPISIDHASH part", got)
			}
			if want := wantSidHash("SAPISIDHASH", "sapisid-a", tc.uid, token); got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

// X-Youtube-Bootstrap-Logged-In follows the ytcfg's LOGGED_IN, as upstream's
// `if traverse_obj(ytcfg, 'LOGGED_IN', expected_type=bool)` does
// (_base.py:975-976), not the jar: a signed-in jar whose page said logged
// out, or stated nothing, makes no such claim.
//
// Mutants: the header set from jar.HasYouTubeAuthCookies() (the logged-out,
// unknown and nil rows send it); set for any LoginState but Unknown (the
// logged-out row sends it); never set (the logged-in row loses it).
func TestBootstrapLoggedInFollowsThePagesLoginState(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	auth := NewAuth(jar, noopLogger{})
	for _, tc := range []struct {
		name  string
		ytcfg *YtcfgData
		want  bool
	}{
		{"logged in", &YtcfgData{LoginState: SessionAuthLoggedIn}, true},
		{"logged out", &YtcfgData{LoginState: SessionAuthLoggedOut}, false},
		{"unknown", &YtcfgData{}, false},
		{"no ytcfg", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := auth.GenerateAPIHeaders(constants.WebClient, tc.ytcfg)["X-Youtube-Bootstrap-Logged-In"]
			if ok != tc.want || (ok && got != "true") {
				t.Errorf("X-Youtube-Bootstrap-Logged-In = %q (present %v), want present %v", got, ok, tc.want)
			}
		})
	}
}

// X-Youtube-Bootstrap-Logged-In goes only from the clients yt-dlp builds from
// a page ytcfg (_download_ytcfg's page map, _base.py:1013-1024; the watch
// page's own for web): web, web_safari, web_creator and web_embedded keep it
// on a logged-in page's ytcfg, while tv_samsung, tv_downgraded, android_vr
// and visionos — whose upstream ytcfg is {} — drop it. The session identity
// and the Authorization hash stay on every client: only the bootstrap claim
// is per client.
//
// Mutants: clientCarriesPageYtcfg always true (the page-less rows send it);
// always false, or WEB_EMBEDDED_PLAYER / WEB_CREATOR left out (those rows
// lose it); the session headers gated by it too (the page-less rows lose
// X-Goog-AuthUser).
func TestBootstrapLoggedInOnlyFromClientsCarryingAPageYtcfg(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	auth := NewAuth(jar, noopLogger{})
	idx := 1
	ytcfg := &YtcfgData{LoginState: SessionAuthLoggedIn, SessionIndex: &idx}
	for _, tc := range []struct {
		name   string
		client constants.YouTubeClientConfig
		want   bool
	}{
		{"web", constants.WebClient, true},
		{"web_safari", constants.WebSafariClient, true},
		{"web_creator", constants.WebCreatorClient, true},
		{"web_embedded", constants.WebEmbeddedClient, true},
		{"tv_samsung", constants.TVSamsungClient, false},
		{"tv_downgraded", constants.TVDowngradedClient, false},
		{"android_vr", constants.AndroidVRClient, false},
		{"visionos", constants.VisionOSClient, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := auth.GenerateAPIHeaders(tc.client, ytcfg)
			got, ok := h["X-Youtube-Bootstrap-Logged-In"]
			if ok != tc.want || (ok && got != "true") {
				t.Errorf("X-Youtube-Bootstrap-Logged-In = %q (present %v), want present %v", got, ok, tc.want)
			}
			if h["X-Goog-AuthUser"] != "1" {
				t.Errorf("X-Goog-AuthUser = %q, want 1 on every client", h["X-Goog-AuthUser"])
			}
			if h["Authorization"] == "" {
				t.Error("Authorization missing: the hash rides every client")
			}
		})
	}
}

// The watch page's own LOGGED_IN verdict is carried on the ytcfg the player
// calls' headers are built from, and the homepage's on the session it hands
// the probes; both keep logged out apart from unknown.
//
// Mutants: FetchWatchPage not setting LoginState (the logged-in row);
// homepageSession not setting it (its logged-in and logged-out rows).
func TestPagesCarryTheirLoginState(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want SessionAuthState
	}{
		{"logged in", `"LOGGED_IN":true`, SessionAuthLoggedIn},
		{"logged out", `"LOGGED_IN":false`, SessionAuthLoggedOut},
	} {
		t.Run("watch page "+tc.name, func(t *testing.T) {
			stubPageGet(t, `<html><script>ytcfg.set({`+tc.cfg+`,"VISITOR_DATA":"vd"});</script></html>`)
			wp, err := FetchWatchPage(context.Background(), "test1234567", "")
			if err != nil {
				t.Fatalf("FetchWatchPage: %v", err)
			}
			if wp.Ytcfg.LoginState != tc.want {
				t.Errorf("Ytcfg.LoginState = %q, want %q", wp.Ytcfg.LoginState, tc.want)
			}
		})
		t.Run("homepage "+tc.name, func(t *testing.T) {
			if got := homepageSession(`ytcfg.set({` + tc.cfg + `,"SESSION_INDEX":"0"});`).LoginState; got != tc.want {
				t.Errorf("homepageSession LoginState = %q, want %q", got, tc.want)
			}
		})
	}
	if got := homepageSession(`ytcfg.set({"SESSION_INDEX":"0"});`).LoginState; got != SessionAuthUnknown {
		t.Errorf("homepage stating no LOGGED_IN: LoginState = %q, want unknown", got)
	}
}

// The probes and chat carry the kept session's user session id and LOGGED_IN
// verdict (probeYtcfg) while the jar holds the account they were read under.
// A page that states only LOGGED_IN carries the verdict for its account but
// is not kept as that account's session, so the homepage read the account
// still wants (claimSessionRead) is still made. A later page of the same
// account that states it logged out and names no session withdraws the
// LOGGED_IN claim alone — the page id, index and user session id it did not
// restate stay — and a later logged-in page naming no session restores it the
// same way, while a page stating nothing, or another account's logged-out
// page, leaves the claim standing; another account's logged-in page carries
// that account's verdict, which this account's probes do not attach.
//
// Mutants: probeYtcfg not carrying UserSessionID, or not LoginState;
// captureVisitorData not keeping the verdict of a page that states only
// LOGGED_IN; such a page kept as its account's session (the homepage read is
// no longer wanted); the withdrawal dropped (the logged-out step keeps the
// claim); the withdrawal clearing the whole session (the page id goes); the
// withdrawal not checking the account (another account's page withdraws it);
// a logged-in page naming no session overwriting the kept one (the last step
// loses the page id); probeYtcfg attaching a verdict without checking whose
// it is (another account's logged-in page claims a login for this one).
func TestProbeSessionCarriesTheUserSessionAndLoginState(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	account := jar.YouTubeIdentity()

	p.captureVisitorData(&YtcfgData{LoginState: SessionAuthLoggedIn}, account)
	if got := p.probeYtcfg("").LoginState; got != SessionAuthLoggedIn {
		t.Fatalf("a page stating only LOGGED_IN: probe LoginState = %q, want logged in", got)
	}
	if !p.claimSessionRead(account) {
		t.Fatal("a page stating only LOGGED_IN stood in for the account's session: its homepage read is no longer wanted")
	}
	p.releaseSessionRead(account, true)

	idx := 1
	p.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-d", UserSessionID: "user-u", LoginState: SessionAuthLoggedIn}, account)
	got := p.probeYtcfg("vd")
	if got.UserSessionID != "user-u" || got.LoginState != SessionAuthLoggedIn || got.DelegatedSessionID != "brand-d" {
		t.Fatalf("probe ytcfg = %+v, want user-u, logged in, brand-d", got)
	}

	p.captureVisitorData(&YtcfgData{}, account)
	p.captureVisitorData(&YtcfgData{LoginState: SessionAuthLoggedOut}, "other-account")
	if got := p.probeYtcfg("").LoginState; got != SessionAuthLoggedIn {
		t.Errorf("after a silent page and another account's logged-out page: LoginState = %q, want logged in", got)
	}

	p.captureVisitorData(&YtcfgData{LoginState: SessionAuthLoggedOut}, account)
	got = p.probeYtcfg("")
	if got.LoginState != SessionAuthUnknown {
		t.Errorf("after the account's logged-out page: LoginState = %q, want no claim", got.LoginState)
	}
	if got.DelegatedSessionID != "brand-d" || got.UserSessionID != "user-u" || got.SessionIndex == nil || *got.SessionIndex != 1 {
		t.Errorf("after the account's logged-out page: probe ytcfg = %+v, want the session it did not restate kept", got)
	}

	p.captureVisitorData(&YtcfgData{LoginState: SessionAuthLoggedIn}, account)
	got = p.probeYtcfg("")
	if got.LoginState != SessionAuthLoggedIn || got.DelegatedSessionID != "brand-d" || got.UserSessionID != "user-u" {
		t.Errorf("after the account's logged-in page naming no session: probe ytcfg = %+v, want the claim back and the session kept", got)
	}

	// Another account's logged-in page naming no session carries that
	// account's verdict, not this one's: the jar still holds account a.
	p.captureVisitorData(&YtcfgData{LoginState: SessionAuthLoggedIn}, "other-account")
	got = p.probeYtcfg("")
	if got.LoginState != SessionAuthUnknown || got.DelegatedSessionID != "brand-d" {
		t.Errorf("after another account's logged-in page: probe ytcfg = %+v, want no claim and a's session kept", got)
	}
}

// A page that states a session replaces the kept one with its own verdict,
// whatever that verdict is: a session its page answered logged out, or whose
// page states no LOGGED_IN at all, claims no logged-in bootstrap on the
// probes, /browse or chat, though the ids it stated are kept.
//
// Mutant: the full-session branch of captureVisitorData storing
// sessionLoggedIn = true (both rows keep the claim).
func TestProbeSessionTakesTheVerdictOfThePageThatStatedIt(t *testing.T) {
	for _, state := range []SessionAuthState{SessionAuthLoggedOut, SessionAuthUnknown} {
		t.Run(string(state), func(t *testing.T) {
			jar, _ := loadAccountJar(t, accountCookies("a"))
			p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
			account := jar.YouTubeIdentity()

			idx := 0
			p.captureVisitorData(&YtcfgData{SessionIndex: &idx, UserSessionID: "user-u", LoginState: SessionAuthLoggedIn}, account)
			if got := p.probeYtcfg("").LoginState; got != SessionAuthLoggedIn {
				t.Fatalf("logged-in session: probe LoginState = %q, want logged in", got)
			}

			idx2 := 1
			p.captureVisitorData(&YtcfgData{SessionIndex: &idx2, UserSessionID: "user-v", LoginState: state}, account)
			got := p.probeYtcfg("")
			if got.LoginState != SessionAuthUnknown {
				t.Errorf("session restated with LoginState %q: probe LoginState = %q, want no claim", state, got.LoginState)
			}
			if got.UserSessionID != "user-v" || got.SessionIndex == nil || *got.SessionIndex != 1 {
				t.Errorf("session restated with LoginState %q: probe ytcfg = %+v, want user-v at index 1", state, got)
			}
		})
	}
}

// ChatAuthorizationHeader is bound to the kept session's user session id, as
// yt-dlp's chat downloader binds it through generate_api_headers, and is the
// unbound form without one; a Service with no Auth signs nothing.
//
// Mutants: ChatAuthorizationHeader returning the unbound jar form (the
// bound step loses "_u"); the nil-Auth
// guard dropped (panics).
func TestChatAuthorizationHeaderBindsTheKeptUserSession(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	s := NewService(jar, noopLogger{})

	got := s.ChatAuthorizationHeader()
	_, token, _ := strings.Cut(got, " ")
	if want := wantSidHash("SAPISIDHASH", "sapisid-a", "", token); got != want {
		t.Errorf("no kept session: Authorization = %q, want the unbound %q", got, want)
	}

	s.PlayerAPI.captureVisitorData(&YtcfgData{UserSessionID: "user-u", LoginState: SessionAuthLoggedIn}, jar.YouTubeIdentity())
	got = s.ChatAuthorizationHeader()
	_, token, _ = strings.Cut(got, " ")
	if want := wantSidHash("SAPISIDHASH", "sapisid-a", "user-u", token); got != want {
		t.Errorf("kept session: Authorization = %q, want the bound %q", got, want)
	}

	if got := (&Service{}).ChatAuthorizationHeader(); got != "" {
		t.Errorf("a Service with no Auth answered %q, want none", got)
	}
	if got := NewService(cookies.NewCookieJar(), noopLogger{}).ChatAuthorizationHeader(); got != "" {
		t.Errorf("an anonymous jar answered %q, want none", got)
	}
}

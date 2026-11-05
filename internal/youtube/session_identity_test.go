package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The datasync id splits as yt-dlp's _parse_data_sync_id splits it
// (_base.py:886-902): "delegated||user" for a brand channel, "user||" for the
// primary one, and a value with no "||" is the user session id alone.
//
// Mutants: parseDataSyncID returning (first, second) whatever second is (the
// primary channel's "user||" read as a delegated id); returning ("", second)
// for "D||U" (the brand channel's delegated half dropped).
func TestParseDataSyncID(t *testing.T) {
	for _, tc := range []struct {
		in, delegated, user string
	}{
		{"", "", ""},
		{"brand-d||user-u", "brand-d", "user-u"},
		{"user-u||", "", "user-u"},
		{"user-u", "", "user-u"},
		{"||user-u", "", "user-u"},
	} {
		d, u := parseDataSyncID(tc.in)
		if d != tc.delegated || u != tc.user {
			t.Errorf("parseDataSyncID(%q) = (%q, %q), want (%q, %q)", tc.in, d, u, tc.delegated, tc.user)
		}
	}
}

// A page's session identity is read off both spellings of the datasync id
// and the literal keys, in yt-dlp's precedence: DELEGATED_SESSION_ID before
// the datasync id's delegated half (_extract_delegated_session_id,
// _base.py:904-915), USER_SESSION_ID before its user half
// (_extract_user_session_id, _base.py:917-927), ytcfg's DATASYNC_ID before a
// response's datasyncId (_extract_data_sync_id, _base.py:929-939). Run on the
// watch page's parser and the homepage's, which must agree.
//
// Mutants: ytcfgDataSyncIDRegex not tried (DATASYNC_ID rows lose their
// datasync id); dataSyncID trying datasyncId first; fillSessionFromDataSync
// overwriting a literal DELEGATED_SESSION_ID, or not filling an absent one;
// USER_SESSION_ID not preferred; homepageSession not calling
// fillSessionFromDataSync.
func TestPageSessionIdentityFromDataSyncID(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		page                      string
		dataSync, delegated, user string
	}{
		{
			name:      "ytcfg DATASYNC_ID, brand channel, no literal delegated id",
			page:      `ytcfg.set({"DATASYNC_ID":"brand-d||user-u","SESSION_INDEX":"0"});`,
			dataSync:  "brand-d||user-u",
			delegated: "brand-d",
			user:      "user-u",
		},
		{
			name:     "ytcfg DATASYNC_ID, primary channel",
			page:     `ytcfg.set({"DATASYNC_ID":"user-u||","SESSION_INDEX":"0"});`,
			dataSync: "user-u||",
			user:     "user-u",
		},
		{
			name:      "responseContext datasyncId alone",
			page:      `var ytInitialData = {"responseContext":{"mainAppWebResponseContext":{"datasyncId":"brand-d||user-u","loggedOut":false}}};`,
			dataSync:  "brand-d||user-u",
			delegated: "brand-d",
			user:      "user-u",
		},
		{
			name:      "ytcfg spelling wins over the response's",
			page:      `var ytInitialData = {"responseContext":{"mainAppWebResponseContext":{"datasyncId":"other-d||other-u"}}}; ytcfg.set({"DATASYNC_ID":"brand-d||user-u"});`,
			dataSync:  "brand-d||user-u",
			delegated: "brand-d",
			user:      "user-u",
		},
		{
			name:      "literal DELEGATED_SESSION_ID and USER_SESSION_ID win",
			page:      `ytcfg.set({"DELEGATED_SESSION_ID":"literal-d","USER_SESSION_ID":"literal-u","DATASYNC_ID":"brand-d||user-u"});`,
			dataSync:  "brand-d||user-u",
			delegated: "literal-d",
			user:      "literal-u",
		},
		{
			name: "no datasync id",
			page: `ytcfg.set({"INNERTUBE_API_KEY":"k"});`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(from string, y *YtcfgData) {
				t.Helper()
				if y.DataSyncID != tc.dataSync || y.DelegatedSessionID != tc.delegated || y.UserSessionID != tc.user {
					t.Errorf("%s: datasync %q, delegated %q, user %q; want %q, %q, %q",
						from, y.DataSyncID, y.DelegatedSessionID, y.UserSessionID, tc.dataSync, tc.delegated, tc.user)
				}
			}
			watch, _ := extractYtcfgAndPlayerResponse([]byte(tc.page))
			check("watch page", watch)
			check("homepage", homepageSession(tc.page))
		})
	}
}

// A brand account's homepage that states its datasync id and not
// DELEGATED_SESSION_ID hands the probes the delegated half: the page id
// reaches X-Goog-PageId on the session the probes, /browse and chat carry.
//
// Mutant: homepageSession not calling fillSessionFromDataSync.
func TestADataSyncIDOnlyHomepageGivesTheProbesItsBrandPage(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	p.captureVisitorData(homepageSession(`ytcfg.set({"SESSION_INDEX":"0","DATASYNC_ID":"brand-d||user-u"});`), jar.YouTubeIdentity())
	got := accountSessionHeaders(p.probeYtcfg(""))
	if got["X-Goog-PageId"] != "brand-d" || got["X-Goog-AuthUser"] != "0" {
		t.Errorf("session headers = %v, want X-Goog-PageId brand-d and X-Goog-AuthUser 0", got)
	}
}

// /browse — the backlog scanner's channel tabs, the membership tab among them
// — carries the signed-in session the probes carry (probeYtcfg), as yt-dlp's
// tab requests pass the ytcfg's session into generate_api_headers
// (_tab.py:591-592, 833-834, 939-940): X-Goog-AuthUser and the brand
// account's X-Goog-PageId, beside the visitor id. Once the jar holds another
// account the old account's page id is not sent.
//
// Mutant: browsePost building its headers from DefaultYtcfg (no AuthUser, no
// PageId); browsePost dropping the visitor data.
func TestBrowseCarriesTheAccountsSession(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseVideosPage1))
	}))
	t.Cleanup(srv.Close)

	jar, path := loadAccountJar(t, accountCookies("a"))
	s := NewService(jar, noopLogger{})
	s.browse.endpoint = srv.URL
	idx := 1
	s.PlayerAPI.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-a"}, jar.YouTubeIdentity())
	s.SetVisitorData("vd-browse")

	if _, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", ""); err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	// Another account's cookies, as an import writes them: the tab's next
	// first page must not name account a's brand page.
	if err := os.WriteFile(path, []byte(accountCookies("bb")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", ""); err != nil {
		t.Fatalf("FetchChannelTabPage after the switch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d browse requests, want 2", len(seen))
	}
	first := seen[0]
	if got := first.Get("X-Goog-AuthUser"); got != "1" {
		t.Errorf("X-Goog-AuthUser = %q, want 1", got)
	}
	if got := first.Get("X-Goog-PageId"); got != "brand-a" {
		t.Errorf("X-Goog-PageId = %q, want brand-a", got)
	}
	if got := first.Get("X-Goog-Visitor-Id"); got != "vd-browse" {
		t.Errorf("X-Goog-Visitor-Id = %q, want vd-browse", got)
	}
	if got := first.Get("X-YouTube-Client-Name"); got != constants.WebClient.ClientID {
		t.Errorf("X-YouTube-Client-Name = %q, want %q", got, constants.WebClient.ClientID)
	}
	if got := seen[1].Get("X-Goog-PageId"); got != "" {
		t.Errorf("after the account switch X-Goog-PageId = %q, want none", got)
	}
}

// ChatSessionHeaders is the probes' session (probeYtcfg) as identity headers,
// read at call time: account a's while the jar holds a, nothing once the jar
// holds another account, and nothing on a Service with no player API.
//
// Mutants: ChatSessionHeaders returning accountSessionHeaders of a bare
// DefaultYtcfg; caching the first answer; the nil-PlayerAPI guard dropped.
func TestChatSessionHeadersFollowTheJarsAccount(t *testing.T) {
	jar, path := loadAccountJar(t, accountCookies("a"))
	s := NewService(jar, noopLogger{})
	idx := 2
	s.PlayerAPI.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-a"}, jar.YouTubeIdentity())
	got := s.ChatSessionHeaders()
	if got["X-Goog-AuthUser"] != "2" || got["X-Goog-PageId"] != "brand-a" {
		t.Errorf("chat session headers = %v, want X-Goog-AuthUser 2, X-Goog-PageId brand-a", got)
	}
	if err := os.WriteFile(path, []byte(accountCookies("bb")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Auth.SyncCookies(); err != nil {
		t.Fatal(err)
	}
	got = s.ChatSessionHeaders()
	if _, ok := got["X-Goog-AuthUser"]; ok {
		t.Errorf("after the account switch chat session headers = %v, want no X-Goog-AuthUser", got)
	}
	if _, ok := got["X-Goog-PageId"]; ok {
		t.Errorf("after the account switch chat session headers = %v, want no X-Goog-PageId", got)
	}
	if got := (&Service{}).ChatSessionHeaders(); len(got) != 0 {
		t.Errorf("a Service with no player API answered %v, want none", got)
	}
}

// accountSessionHeaders keeps _generate_cookie_auth_headers' rule
// (_base.py:960-978): X-Goog-PageId for a delegated session, X-Goog-AuthUser
// whenever either id is named — the session index, else 0. The delegated-only
// row is the shape a page that names its brand session only through its
// datasync id ("D||U", no SESSION_INDEX) now yields, so it must still say
// which login it is.
//
// Mutants: X-Goog-AuthUser only when SessionIndex is set (the delegated-only
// row sends a page id with no AuthUser); X-Goog-AuthUser always sent (the
// empty row); the index dropped for 0.
func TestAccountSessionHeaders(t *testing.T) {
	idx := 3
	for _, tc := range []struct {
		name  string
		ytcfg *YtcfgData
		want  map[string]string
	}{
		{"nil", nil, map[string]string{}},
		{"empty", &YtcfgData{}, map[string]string{}},
		{"session index only", &YtcfgData{SessionIndex: &idx}, map[string]string{"X-Goog-AuthUser": "3"}},
		{"delegated only", &YtcfgData{DelegatedSessionID: "brand-d"}, map[string]string{"X-Goog-PageId": "brand-d", "X-Goog-AuthUser": "0"}},
		{"both", &YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-d"}, map[string]string{"X-Goog-PageId": "brand-d", "X-Goog-AuthUser": "3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := accountSessionHeaders(tc.ytcfg)
			if len(got) != len(tc.want) {
				t.Fatalf("accountSessionHeaders = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("accountSessionHeaders[%s] = %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// A chat poll says it is logged in while the jar holds a signed-in set, as
// the player calls do (GenerateAPIHeaders) and as yt-dlp's
// generate_api_headers does from the ytcfg's LOGGED_IN (_base.py:975-976) —
// read at call time, so cookies removed mid-capture stop the claim, and an
// anonymous install never makes it.
//
// Mutants: ChatSessionHeaders never setting X-Youtube-Bootstrap-Logged-In;
// setting it unconditionally.
func TestChatSessionHeadersSayLoggedInWhileTheJarIs(t *testing.T) {
	jar, path := loadAccountJar(t, accountCookies("a"))
	s := NewService(jar, noopLogger{})
	if got := s.ChatSessionHeaders()["X-Youtube-Bootstrap-Logged-In"]; got != "true" {
		t.Errorf("signed in: X-Youtube-Bootstrap-Logged-In = %q, want true", got)
	}
	if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Auth.SyncCookies(); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.ChatSessionHeaders()["X-Youtube-Bootstrap-Logged-In"]; ok {
		t.Errorf("after the cookies were removed: X-Youtube-Bootstrap-Logged-In = %q, want absent", got)
	}
	if got, ok := NewService(cookies.NewCookieJar(), noopLogger{}).ChatSessionHeaders()["X-Youtube-Bootstrap-Logged-In"]; ok {
		t.Errorf("anonymous: X-Youtube-Bootstrap-Logged-In = %q, want absent", got)
	}
}

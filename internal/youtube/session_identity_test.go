package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
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

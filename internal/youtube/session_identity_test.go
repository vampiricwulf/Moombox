package youtube

import (
	"testing"
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

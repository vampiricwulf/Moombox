package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// wrapPage embeds a ytInitialData JSON literal in a minimal HTML page the way
// YouTube serves it, so tests exercise extractYtInitialData + parseMembershipTab
// end to end.
func wrapPage(jsonBody string) []byte {
	return []byte(`<!DOCTYPE html><html><head><script nonce="x">` +
		`var ytInitialData = ` + jsonBody + `;</script></head><body></body></html>`)
}

// membershipPage builds a /membership ytInitialData with a selected
// TAB_ID_SPONSORSHIPS tab whose grid uses the current lockupViewModel layout.
const lockupMembershipJSON = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Home", "selected": false, "tabIdentifier": "", "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "PUBLICvid01", "metadata": {"lockupMetadataViewModel": {"title": {"content": "a public video"}}}}}}}
    ]}}}},
    {"tabRenderer": {"title": "Membership", "selected": true, "tabIdentifier": "TAB_ID_SPONSORSHIPS", "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "K-rKAxqjAec", "metadata": {"lockupMetadataViewModel": {"title": {"content": "a short serenade [members only karaoke]"}}}}}}},
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "gr-ZTohjwnQ", "metadata": {"lockupMetadataViewModel": {"title": {"content": "short chat [MEMBERS]"}}}}}}},
      {"continuationItemRenderer": {"trigger": "x"}}
    ]}}}}
  ]}}
}`

// classicMembershipJSON uses the older gridVideoRenderer/videoRenderer layout.
const classicMembershipJSON = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Membership", "selected": true, "tabIdentifier": "TAB_ID_SPONSORSHIPS", "content": {"sectionListRenderer": {"contents": [
      {"itemSectionRenderer": {"contents": [{"gridRenderer": {"items": [
        {"gridVideoRenderer": {"videoId": "vodMember01", "title": {"runs": [{"text": "member "}, {"text": "vod"}]}}},
        {"videoRenderer": {"videoId": "liveMembr02", "title": {"simpleText": "members live now"}}}
      ]}}]}}
    ]}}}}
  ]}}
}`

func TestParseMembershipTab_Lockup(t *testing.T) {
	videos, ok := parseMembershipTab(wrapPage(lockupMembershipJSON))
	if !ok {
		t.Fatal("expected hasAccess=true for a selected membership tab")
	}
	if len(videos) != 2 {
		t.Fatalf("expected 2 members videos, got %d: %+v", len(videos), videos)
	}
	if videos[0].VideoID != "K-rKAxqjAec" || videos[1].VideoID != "gr-ZTohjwnQ" {
		t.Errorf("unexpected video IDs: %+v", videos)
	}
	if !strings.Contains(videos[0].Title, "short serenade") {
		t.Errorf("expected lockup title extracted, got %q", videos[0].Title)
	}
	// The public video from the non-selected Home tab must NOT leak in.
	for _, v := range videos {
		if v.VideoID == "PUBLICvid01" {
			t.Error("public video from non-selected tab leaked into members list")
		}
	}
}

func TestParseMembershipTab_ClassicRenderers(t *testing.T) {
	videos, ok := parseMembershipTab(wrapPage(classicMembershipJSON))
	if !ok {
		t.Fatal("expected hasAccess=true")
	}
	if len(videos) != 2 {
		t.Fatalf("expected 2 videos, got %d: %+v", len(videos), videos)
	}
	got := map[string]string{}
	for _, v := range videos {
		got[v.VideoID] = v.Title
	}
	if got["vodMember01"] != "member vod" {
		t.Errorf("title.runs not concatenated: %q", got["vodMember01"])
	}
	if got["liveMembr02"] != "members live now" {
		t.Errorf("title.simpleText not read: %q", got["liveMembr02"])
	}
}

func TestMembershipItemAge(t *testing.T) {
	json := `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[
		{"tabRenderer":{"selected":true,"tabIdentifier":"TAB_ID_SPONSORSHIPS","content":{"richGridRenderer":{"contents":[
			{"richItemRenderer":{"content":{"lockupViewModel":{"contentId":"liveVid0001","badge":"THUMBNAIL_OVERLAY_BADGE_STYLE_LIVE","metadata":{"lockupMetadataViewModel":{"title":{"content":"live now"}}}}}}},
			{"richItemRenderer":{"content":{"lockupViewModel":{"contentId":"oldVid00002","meta":"Streamed 2 years ago","metadata":{"lockupMetadataViewModel":{"title":{"content":"old vod"}}}}}}},
			{"richItemRenderer":{"content":{"lockupViewModel":{"contentId":"weekVid0003","meta":"Streamed 3 weeks ago","metadata":{"lockupMetadataViewModel":{"title":{"content":"week vod"}}}}}}},
			{"richItemRenderer":{"content":{"lockupViewModel":{"contentId":"upcomVid004","metadata":{"lockupMetadataViewModel":{"title":{"content":"upcoming stream"}}}}}}},
			{"richItemRenderer":{"content":{"lockupViewModel":{"contentId":"noSignal005","metadata":{"lockupMetadataViewModel":{"title":{"content":"no signal"}}}}}}}
		]}}}}
	]}}}`
	videos, ok := parseMembershipTab([]byte(wrapPage(json)))
	if !ok {
		t.Fatal("expected access")
	}
	age := map[string]time.Duration{}
	for _, v := range videos {
		age[v.VideoID] = v.Age
	}
	if age["liveVid0001"] != 0 {
		t.Errorf("live item should have Age 0, got %v", age["liveVid0001"])
	}
	if got, want := age["oldVid00002"], 2*365*24*time.Hour; got != want {
		t.Errorf("2 years ago: got %v want %v", got, want)
	}
	if got, want := age["weekVid0003"], 3*7*24*time.Hour; got != want {
		t.Errorf("3 weeks ago: got %v want %v", got, want)
	}
	// An upcoming stream (no live badge, no "Streamed N ago") and any item with
	// no recognizable timestamp must rank as "now" (Age 0), NOT sink — otherwise
	// upcoming/live members streams get crowded out of the cap.
	if age["upcomVid004"] != 0 {
		t.Errorf("upcoming item should rank as now (Age 0), got %v", age["upcomVid004"])
	}
	if age["noSignal005"] != 0 {
		t.Errorf("no-signal item should rank as now (Age 0), got %v", age["noSignal005"])
	}
}

func TestParseMembershipTab_NotAMember_HomeFallback(t *testing.T) {
	// Non-member: /membership falls back to the Home tab being selected.
	json := `{"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
		{"tabRenderer": {"title": "Home", "selected": true, "tabIdentifier": "", "content": {"richGridRenderer": {"contents": [
			{"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "PublicVid99", "metadata": {"lockupMetadataViewModel": {"title": {"content": "public"}}}}}}}
		]}}}}}
	]}}}`
	videos, ok := parseMembershipTab(wrapPage(json))
	if ok {
		t.Error("expected hasAccess=false when Home tab is selected (not a member)")
	}
	if videos != nil {
		t.Errorf("expected nil videos for non-member, got %+v", videos)
	}
}

func TestParseMembershipTab_NotAMember_NoTabSelected(t *testing.T) {
	// Join-upsell page: membership tab present but none selected.
	json := `{"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
		{"tabRenderer": {"title": "Membership", "selected": false, "tabIdentifier": "TAB_ID_SPONSORSHIPS", "content": {}}}
	]}}}`
	videos, ok := parseMembershipTab(wrapPage(json))
	if ok || videos != nil {
		t.Errorf("expected (nil,false) for unselected membership tab, got (%+v,%v)", videos, ok)
	}
}

func TestParseMembershipTab_Dedup(t *testing.T) {
	// Same video in two shelves within the membership tab → one entry.
	json := `{"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
		{"tabRenderer": {"selected": true, "tabIdentifier": "TAB_ID_SPONSORSHIPS", "content": {"richGridRenderer": {"contents": [
			{"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "dupVideo123", "metadata": {"lockupMetadataViewModel": {"title": {"content": "x"}}}}}}},
			{"shelfRenderer": {"content": {"lockupViewModel": {"contentId": "dupVideo123", "metadata": {"lockupMetadataViewModel": {"title": {"content": "x"}}}}}}}
		]}}}}
	]}}}`
	videos, ok := parseMembershipTab(wrapPage(json))
	if !ok {
		t.Fatal("expected hasAccess=true")
	}
	if len(videos) != 1 {
		t.Fatalf("expected dedup to 1 video, got %d", len(videos))
	}
}

func TestParseMembershipTab_NoInitialData(t *testing.T) {
	if v, ok := parseMembershipTab([]byte("<html>no data here</html>")); ok || v != nil {
		t.Errorf("expected (nil,false) when ytInitialData absent, got (%+v,%v)", v, ok)
	}
}

func TestExtractYtInitialData_BalancedBraces(t *testing.T) {
	// JSON containing braces and escaped quotes inside strings — the brace
	// scanner must not terminate early on a '}' inside a string literal.
	body := `{"a":"has } brace and \" quote","b":{"c":"}"},"d":1}`
	got, ok := extractYtInitialData(wrapPage(body))
	if !ok {
		t.Fatal("extraction failed")
	}
	if string(got) != body {
		t.Errorf("brace scan mismatch:\n got: %s\nwant: %s", got, body)
	}
}

func TestExtractYtInitialData_WindowBracketForm(t *testing.T) {
	// The `window["ytInitialData"] = {…}` assignment form must also match.
	page := `<script>window["ytInitialData"] = {"contents":{"x":1}};</script>`
	got, ok := extractYtInitialData([]byte(page))
	if !ok || string(got) != `{"contents":{"x":1}}` {
		t.Errorf("window-bracket form not extracted: ok=%v got=%s", ok, got)
	}
}

func TestExtractYtInitialData_Absent(t *testing.T) {
	if _, ok := extractYtInitialData([]byte("<html></html>")); ok {
		t.Error("expected extraction to fail when marker absent")
	}
}

func TestItemAgeTruncatedLowerBound(t *testing.T) {
	// Spec §12: itemAge returns n*unit — the LOWER bound of the true age — so
	// now - itemAge() is the NEWEST instant consistent with the text. Do NOT
	// "fix" this to a midpoint or upper bound: the window design depends on it.
	item := map[string]any{"title": map[string]any{"simpleText": "x"},
		"publishedTimeText": map[string]any{"simpleText": "1 week ago"}}
	if got := itemAge(item); got != 7*24*time.Hour {
		t.Fatalf("itemAge(1 week ago) = %v, want 168h", got)
	}
	// The live-badge short-circuit outranks the age regex: a live renderer
	// carrying "Started streaming 2 hours ago" must return 0, not 2h.
	live := map[string]any{"publishedTimeText": map[string]any{"simpleText": "Started streaming 2 hours ago"},
		"thumbnailOverlays": []any{map[string]any{"thumbnailOverlayTimeStatusRenderer": map[string]any{"style": "THUMBNAIL_OVERLAY_BADGE_STYLE_LIVE"}}}}
	if got := itemAge(live); got != 0 {
		t.Fatalf("live badge must short-circuit to 0, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// The probe's login verdict.
//
// FetchMembershipVideos already fetches an authenticated page with the real
// cookie header once per channel per monitor cycle. It used to collapse "the
// session is dead", "the account is not a member" and "there is no members
// content" into a single bare (nil, nil), which threw away the only one of
// the three that says anything about credential health.
// ---------------------------------------------------------------------------

// halfClearedCookieFile is a configured YouTube session with LOGIN_INFO gone.
// HasAnyYouTubeAuthCookie accepts it; HasYouTubeAuthCookies rejects it. That
// is exactly the state the probe exists to detect, so the probe has to run
// for it rather than being gated out by the complete-set predicate.
const halfClearedCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tworking-sapisid\n"

// unconfiguredCookieFile carries no YouTube auth cookie at all — the install
// was never signed in, which is not a health problem and must stay Unknown.
const unconfiguredCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tFALSE\t0\tPREF\tf6=40000000\n"

// membershipHTML wraps a ytInitialData literal in a page that also carries
// YouTube's own login marker, the way a real /membership response does.
func membershipHTML(loggedIn bool, initialData string) []byte {
	return []byte(`<!DOCTYPE html><html><head>` +
		`<script nonce="x">ytcfg.set({"LOGGED_IN":` + strconv.FormatBool(loggedIn) + `});</script>` +
		`<script nonce="x">var ytInitialData = ` + initialData + `;</script>` +
		`</head><body></body></html>`)
}

// homeFallbackJSON is what YouTube serves a signed-in NON-member: the
// /membership URL resolves with the Home tab selected instead.
const homeFallbackJSON = `{"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
	{"tabRenderer": {"title": "Home", "selected": true, "tabIdentifier": "", "content": {}}}
]}}}`

// newMembershipProbeService builds a Service whose jar holds cookieFile and
// whose /membership fetch is aimed at base, restoring the package seam on
// cleanup.
func newMembershipProbeService(t *testing.T, base, cookieFile string) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(cookieFile), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	orig := membershipPageBase
	membershipPageBase = base
	t.Cleanup(func() { membershipPageBase = orig })
	return NewService(jar, noopLogger{})
}

// TestFetchMembershipVideosReturnsVerdict pins the contract this probe now
// carries: the LOGIN verdict, not hasAccess. Most archived channels are
// legitimately not membered, so an empty video list carries no health
// information at all — a page YouTube answered as anonymous does.
func TestFetchMembershipVideosReturnsVerdict(t *testing.T) {
	cases := []struct {
		name        string
		body        []byte
		wantVerdict SessionAuthState
		wantVideos  int
	}{
		{
			name:        "logged in, not a member",
			body:        membershipHTML(true, homeFallbackJSON),
			wantVerdict: SessionAuthLoggedIn,
			wantVideos:  0,
		},
		{
			name:        "logged in, member",
			body:        membershipHTML(true, lockupMembershipJSON),
			wantVerdict: SessionAuthLoggedIn,
			wantVideos:  2,
		},
		{
			name:        "session is dead",
			body:        membershipHTML(false, homeFallbackJSON),
			wantVerdict: SessionAuthLoggedOut,
			wantVideos:  0,
		},
		{
			// A consent interstitial answers 200 with no login marker.
			// Reporting that as a dead session would send an operator whose
			// cookies are fine off to re-export them.
			name:        "unreadable page carries no verdict",
			body:        []byte(`<html><body>Before you continue to YouTube</body></html>`),
			wantVerdict: SessionAuthUnknown,
			wantVideos:  0,
		},
		{
			// The case that separates livenessVerdict from
			// sessionAuthFromBytes: a shell carrying a ytcfg bootstrap but no
			// login key at all. The permissive detector calls that logged-out
			// (sound for a watch page, which always stamps the key on a real
			// one); the probe must not, because a consent wall serving a
			// bootstrap would then read as dead cookies. Swapping this call
			// site back to sessionAuthFromBytes fails HERE and nowhere else.
			name:        "ytcfg shell with no login key is not a dead session",
			body:        []byte(`<html><head><script>ytcfg.set({"VISITOR_DATA":"x"});</script></head></html>`),
			wantVerdict: SessionAuthUnknown,
			wantVideos:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write(tc.body)
			}))
			defer srv.Close()

			s := newMembershipProbeService(t, srv.URL, halfClearedCookieFile)
			videos, verdict, err := s.FetchMembershipVideos(context.Background(), "UC_probe_channel")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", verdict, tc.wantVerdict)
			}
			if len(videos) != tc.wantVideos {
				t.Errorf("videos = %d, want %d", len(videos), tc.wantVideos)
			}
			if want := "/channel/UC_probe_channel/membership"; gotPath != want {
				t.Errorf("fetched %q, want %q", gotPath, want)
			}
		})
	}
}

// TestFetchMembershipVideosProbesAHalfClearedSession is the gate half of the
// change. The probe used to require the COMPLETE credential set, so with
// LOGIN_INFO cleared it never ran — it skipped precisely the session state it
// is there to report on, and any verdict it could return would be dead code.
func TestFetchMembershipVideosProbesAHalfClearedSession(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(membershipHTML(false, homeFallbackJSON))
	}))
	defer srv.Close()

	s := newMembershipProbeService(t, srv.URL, halfClearedCookieFile)
	if s.HasAuthCookies() {
		t.Fatal("precondition: the complete-set predicate must reject a half-cleared session")
	}
	_, verdict, err := s.FetchMembershipVideos(context.Background(), "UCabc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != 1 {
		t.Fatalf("membership page fetched %d times, want 1 — the probe was gated out", hits)
	}
	if verdict != SessionAuthLoggedOut {
		t.Errorf("verdict = %q, want %q", verdict, SessionAuthLoggedOut)
	}
}

// TestFetchMembershipVideosSkipsWhenNeverConfigured: an install that was never
// signed in has nothing to report. It must not fetch, and it must not claim a
// dead session.
func TestFetchMembershipVideosSkipsWhenNeverConfigured(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(membershipHTML(false, homeFallbackJSON))
	}))
	defer srv.Close()

	s := newMembershipProbeService(t, srv.URL, unconfiguredCookieFile)
	videos, verdict, err := s.FetchMembershipVideos(context.Background(), "UCabc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != 0 {
		t.Errorf("fetched %d times, want 0 — nothing was ever configured", hits)
	}
	if verdict != SessionAuthUnknown {
		t.Errorf("verdict = %q, want %q", verdict, SessionAuthUnknown)
	}
	if videos != nil {
		t.Errorf("videos = %+v, want nil", videos)
	}
}

// TestFetchMembershipVideosTransportFailureIsNotAVerdict: a page we never got
// says nothing about the session. This is the failure mode that reaches a
// container behind a flaky egress path, and it must not read as dead cookies.
func TestFetchMembershipVideosTransportFailureIsNotAVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusBadGateway)
	}))
	defer srv.Close()

	s := newMembershipProbeService(t, srv.URL, halfClearedCookieFile)
	videos, verdict, err := s.FetchMembershipVideos(context.Background(), "UCabc")
	if err == nil {
		t.Fatal("expected an error for a 502")
	}
	if verdict != SessionAuthUnknown {
		t.Errorf("verdict = %q, want %q — a transport failure is not a login verdict", verdict, SessionAuthUnknown)
	}
	if videos != nil {
		t.Errorf("videos = %+v, want nil", videos)
	}
}

// TestServiceHasAnyAuthCookieSeesAHalfClearedSession pins the predicate the
// monitor's membership gate now resolves against. HasAuthCookies answers "is
// the set complete", which is the wrong question for "should we even look".
func TestServiceHasAnyAuthCookieSeesAHalfClearedSession(t *testing.T) {
	s := newMembershipProbeService(t, "http://unused.invalid", halfClearedCookieFile)
	if s.HasAuthCookies() {
		t.Error("HasAuthCookies() = true, want false for a session with LOGIN_INFO cleared")
	}
	if !s.HasAnyAuthCookie() {
		t.Error("HasAnyAuthCookie() = false, want true — the session was configured, just broken")
	}

	none := newMembershipProbeService(t, "http://unused.invalid", unconfiguredCookieFile)
	if none.HasAnyAuthCookie() {
		t.Error("HasAnyAuthCookie() = true for an install that was never signed in")
	}
}

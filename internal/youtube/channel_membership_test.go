package youtube

import (
	"strings"
	"testing"
	"time"
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

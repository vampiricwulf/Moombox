package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// newBrowseTestService wires a full Service (empty cookie jar, noop logger)
// whose /browse endpoint points at the given httptest server, so tests
// exercise FetchChannelTabPage end to end: request building, both response
// envelope shapes, the cross-call continuation session, and the per-channel
// tab-strip params cache.
func newBrowseTestService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s := NewService(cookies.NewCookieJar(), noopLogger{})
	s.browse.endpoint = server.URL
	return s
}

// decodeBrowseRequest unmarshals a /browse POST body for request assertions.
func decodeBrowseRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode browse request body: %v", err)
	}
	return body
}

// assertHarvestRequest asserts the strip-harvest request shape: browseId
// present, NO params (bare-derived constants fail tab selection in production
// — the params must come from the channel's own strip, which this request
// fetches), no continuation.
func assertHarvestRequest(t *testing.T, body map[string]any, browseID string) {
	t.Helper()
	if got := body["browseId"]; got != browseID {
		t.Errorf("harvest browseId: want %q, got %v", browseID, got)
	}
	if p, has := body["params"]; has {
		t.Errorf("harvest request must carry NO params, got %v", p)
	}
	if _, has := body["continuation"]; has {
		t.Error("harvest request must not carry a continuation")
	}
}

// The per-tab params blobs the strip fixtures carry. Values mirror the real
// InnerTube shape — the bare field-2 path encoding PLUS trailing protobuf
// payload (the suffix whose absence broke tab selection in production). Tests
// only assert that what was harvested is echoed back verbatim.
const (
	fixtureVideosParams     = "EgZ2aWRlb3PyBgQKAjoA"
	fixtureStreamsParams    = "EgdzdHJlYW1z8gYECgJ6AA=="
	fixtureMembershipParams = "EgptZW1iZXJzaGlw8gYECgJIAA=="
)

// browseVideosPage1 is a page-1 /browse response with the Videos tab SELECTED
// inside a full tab strip (every page-1 envelope carries the strip; the
// non-selected Home/Live headers carry their own browseEndpoint params). Two
// lockupViewModel video renderers (item shape copied from
// channel_membership_test.go) and a trailing continuationItemRenderer carrying
// token "TOK2". The second item's "Streamed 2 years ago" text is the itemAge
// coarse-date signal; the first has no signal and must rank "now".
const browseVideosPage1 = `{
  "responseContext": {"visitorData": "vd-page1"},
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Home", "endpoint": {"browseEndpoint": {"params": "EghmZWF0dXJlZPIGBAoCMgA="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/featured"}}}}},
    {"tabRenderer": {"title": "Videos", "selected": true, "tabIdentifier": "", "endpoint": {"browseEndpoint": {"params": "EgZ2aWRlb3PyBgQKAjoA"}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/videos"}}}, "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "K-rKAxqjAec", "metadata": {"lockupMetadataViewModel": {"title": {"content": "newest upload"}}}}}}},
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "gr-ZTohjwnQ", "meta": "Streamed 2 years ago", "metadata": {"lockupMetadataViewModel": {"title": {"content": "old stream vod"}}}}}}},
      {"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": "TOK2"}}}}
    ]}}}},
    {"tabRenderer": {"title": "Live", "endpoint": {"browseEndpoint": {"params": "EgdzdHJlYW1z8gYECgJ6AA=="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/streams"}}}}}
  ]}}
}`

// browseStripHomeSelected is the harvest response for a channel with all
// three scannable tabs: Home SELECTED (what a no-params browse actually
// returns) with shelf content that must never leak into any scan, plus
// Videos / Live / Membership headers carrying the per-tab params blobs.
const browseStripHomeSelected = `{
  "responseContext": {"visitorData": "vd-harvest"},
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Home", "selected": true, "endpoint": {"browseEndpoint": {"params": "EghmZWF0dXJlZPIGBAoCMgA="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/featured"}}}, "content": {"sectionListRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "homeShelf01", "metadata": {"lockupMetadataViewModel": {"title": {"content": "home shelf video"}}}}}}}
    ]}}}},
    {"tabRenderer": {"title": "Videos", "endpoint": {"browseEndpoint": {"params": "EgZ2aWRlb3PyBgQKAjoA"}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/videos"}}}}},
    {"tabRenderer": {"title": "Live", "endpoint": {"browseEndpoint": {"params": "EgdzdHJlYW1z8gYECgJ6AA=="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/streams"}}}}},
    {"tabRenderer": {"title": "Membership", "tabIdentifier": "TAB_ID_SPONSORSHIPS", "endpoint": {"browseEndpoint": {"params": "EgptZW1iZXJzaGlw8gYECgJIAA=="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/membership"}}}}}
  ]}}
}`

// browseStripNoMembership is a harvest response whose strip LACKS a
// membership tab — the shape a non-member (or unauthenticated) browse of any
// channel returns. Streams-absence looks identical with the Live header gone.
const browseStripNoMembership = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Home", "selected": true, "endpoint": {"commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/featured"}}}, "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "homeShelf01", "metadata": {"lockupMetadataViewModel": {"title": {"content": "home shelf video"}}}}}}}
    ]}}}},
    {"tabRenderer": {"title": "Videos", "endpoint": {"browseEndpoint": {"params": "EgZ2aWRlb3PyBgQKAjoA"}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/videos"}}}}},
    {"tabRenderer": {"title": "Live", "endpoint": {"browseEndpoint": {"params": "EgdzdHJlYW1z8gYECgJ6AA=="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/streams"}}}}}
  ]}}
}`

// browseStreamsPage1 is a streams-tab-selected page-1 response: one dated
// stream VOD, no continuation — the tab is exhausted after one page.
const browseStreamsPage1 = `{
  "responseContext": {"visitorData": "vd-streams"},
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Live", "selected": true, "endpoint": {"browseEndpoint": {"params": "EgdzdHJlYW1z8gYECgJ6AA=="}, "commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/streams"}}}, "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "streamVod01", "meta": "Streamed 2 months ago", "metadata": {"lockupMetadataViewModel": {"title": {"content": "old stream"}}}}}}}
    ]}}}}
  ]}}
}`

// browseVideosPage2 is a page-2+ /browse response: items arrive under
// onResponseReceivedActions[].appendContinuationItemsAction.continuationItems[]
// with NO further continuationItemRenderer — the tab is exhausted.
const browseVideosPage2 = `{
  "responseContext": {"visitorData": "vd-page2"},
  "onResponseReceivedActions": [
    {"appendContinuationItemsAction": {"continuationItems": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "vodMember03", "meta": "Streamed 3 weeks ago", "metadata": {"lockupMetadataViewModel": {"title": {"content": "older vod"}}}}}}}
    ]}}
  ]
}`

// browseVideosPage2Loop is a continuation response that hands back the SAME
// token it was fetched with ("TOK2") — the feed-loop condition yt-dlp guards
// with its seen_continuations set.
const browseVideosPage2Loop = `{
  "onResponseReceivedActions": [
    {"appendContinuationItemsAction": {"continuationItems": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "loopedVid01", "meta": "Streamed 1 week ago", "metadata": {"lockupMetadataViewModel": {"title": {"content": "looped item"}}}}}}},
      {"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": "TOK2"}}}}
    ]}}
  ]
}`

// Page 1, wanted-tab-already-selected short-circuit: the no-params harvest
// request comes back with the Videos tab selected, so ONE request serves both
// the harvest and the page — and the envelope parses both video renderers
// (with itemAge semantics) plus the continuation token.
func TestFetchChannelTabPage_FirstPage(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		assertHarvestRequest(t, decodeBrowseRequest(t, r), "UCbrowsetest0000000000ab")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseVideosPage1))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("wanted tab already selected must short-circuit (1 request), got %d", got)
	}
	if page.Continuation != "TOK2" {
		t.Errorf("continuation: want TOK2, got %q", page.Continuation)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].VideoID != "K-rKAxqjAec" || page.Items[1].VideoID != "gr-ZTohjwnQ" {
		t.Errorf("unexpected video IDs: %+v", page.Items)
	}
	if page.Items[0].Title != "newest upload" {
		t.Errorf("lockup title not extracted: %q", page.Items[0].Title)
	}
	// Age must come from itemAge: no signal ⇒ 0 ("now"), "Streamed 2 years
	// ago" ⇒ the coarse lower bound.
	if page.Items[0].Age != 0 {
		t.Errorf("undated item should have Age 0, got %v", page.Items[0].Age)
	}
	if want := 2 * 365 * 24 * time.Hour; page.Items[1].Age != want {
		t.Errorf("2 years ago: want %v, got %v", want, page.Items[1].Age)
	}
}

// Two-request page 1: the streams tab is NOT selected by the no-params
// harvest, so its harvested strip params must be echoed back on a second
// request — and only the streams tab's items come back (the harvest page's
// Home shelf must not leak).
func TestFetchChannelTabPage_StreamsUsesHarvestedParams(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body := decodeBrowseRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			assertHarvestRequest(t, body, "UCbrowsetest0000000000ab")
			w.Write([]byte(browseStripHomeSelected))
		case 2:
			if got := body["browseId"]; got != "UCbrowsetest0000000000ab" {
				t.Errorf("request 2 browseId: want channel ID, got %v", got)
			}
			if got := body["params"]; got != fixtureStreamsParams {
				t.Errorf("params: want the HARVESTED streams params %q, got %v", fixtureStreamsParams, got)
			}
			w.Write([]byte(browseStreamsPage1))
		default:
			t.Errorf("unexpected request %d", n)
		}
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "streams", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected harvest + params requests (2), got %d", got)
	}
	if len(page.Items) != 1 || page.Items[0].VideoID != "streamVod01" {
		t.Fatalf("expected only the stream VOD (home shelf must not leak), got %+v", page.Items)
	}
	if want := 2 * 30 * 24 * time.Hour; page.Items[0].Age != want {
		t.Errorf("2 months ago: want %v, got %v", want, page.Items[0].Age)
	}
	if page.Continuation != "" {
		t.Errorf("expected exhausted tab, got continuation %q", page.Continuation)
	}
}

// Cache amortization: a videos → streams → membership page-1 sequence for
// ONE channel issues exactly one no-params harvest — the later tabs reuse
// the cached strip and go straight to their params requests.
func TestFetchChannelTabPage_StripCacheAmortized(t *testing.T) {
	var calls, harvests atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body := decodeBrowseRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		if _, has := body["params"]; !has {
			harvests.Add(1)
			w.Write([]byte(browseStripHomeSelected))
			return
		}
		switch got := body["params"]; got {
		case fixtureVideosParams:
			w.Write([]byte(browseVideosPage1))
		case fixtureStreamsParams:
			w.Write([]byte(browseStreamsPage1))
		case fixtureMembershipParams:
			w.Write([]byte(browseMembershipPage1))
		default:
			t.Errorf("request %d: unexpected params %v", n, got)
			w.Write([]byte(`{}`))
		}
	})

	ctx := context.Background()
	vids, err := s.FetchChannelTabPage(ctx, "UCbrowsetest0000000000ab", "videos", "")
	if err != nil {
		t.Fatalf("videos page 1: %v", err)
	}
	if len(vids.Items) != 2 {
		t.Fatalf("videos: expected 2 items, got %+v", vids.Items)
	}
	strms, err := s.FetchChannelTabPage(ctx, "UCbrowsetest0000000000ab", "streams", "")
	if err != nil {
		t.Fatalf("streams page 1: %v", err)
	}
	if len(strms.Items) != 1 || strms.Items[0].VideoID != "streamVod01" {
		t.Fatalf("streams: expected the stream VOD, got %+v", strms.Items)
	}
	memb, err := s.FetchChannelTabPage(ctx, "UCbrowsetest0000000000ab", "membership", "")
	if err != nil {
		t.Fatalf("membership page 1: %v", err)
	}
	if len(memb.Items) != 1 || memb.Items[0].VideoID != "K-rKAxqjAec" {
		t.Fatalf("membership: expected the members item, got %+v", memb.Items)
	}
	if got := harvests.Load(); got != 1 {
		t.Errorf("expected exactly ONE no-params harvest for the channel, got %d", got)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("expected 4 requests (harvest + one per tab), got %d", got)
	}
}

// Membership absent from the strip (non-member / unauthenticated): clean empty
// exhaustion after the harvest alone — NO params request may follow.
func TestFetchChannelTabPage_MembershipAbsentFromStrip(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assertHarvestRequest(t, decodeBrowseRequest(t, r), "UCbrowsetest0000000000ab")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseStripNoMembership))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "membership", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("strip lacks membership: nothing may follow the harvest (1 request), got %d", got)
	}
	if len(page.Items) != 0 {
		t.Errorf("expected no items, got %+v", page.Items)
	}
	if page.Continuation != "" {
		t.Errorf("expected exhausted tab, got continuation %q", page.Continuation)
	}
}

// Loop detection: a page returning an already-seen token errors with the
// loop-detected sentinel. Page 1 hands out TOK2; the TOK2 page hands back TOK2
// again. Also verifies visitorData is re-extracted per page: the continuation
// request must carry page 1's visitorData.
func TestFetchChannelTabPage_LoopDetected(t *testing.T) {
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBrowseRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		if _, isCont := body["continuation"]; !isCont {
			w.Write([]byte(browseVideosPage1))
			return
		}
		// The stale-token hazard the chat downloader guards against: the
		// visitorData extracted from page 1 must ride the next request.
		if got := r.Header.Get("X-Goog-Visitor-Id"); got != "vd-page1" {
			t.Errorf("X-Goog-Visitor-Id: want vd-page1 from page 1, got %q", got)
		}
		w.Write([]byte(browseVideosPage2Loop))
	})

	ctx := context.Background()
	page1, err := s.FetchChannelTabPage(ctx, "UCbrowsetest0000000000ab", "videos", "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if page1.Continuation != "TOK2" {
		t.Fatalf("page 1 continuation: want TOK2, got %q", page1.Continuation)
	}

	_, err = s.FetchChannelTabPage(ctx, "UCbrowsetest0000000000ab", "videos", page1.Continuation)
	if err == nil {
		t.Fatal("expected loop-detected error, got nil")
	}
	if !errors.Is(err, ErrContinuationLoop) {
		t.Errorf("expected errors.Is(err, ErrContinuationLoop), got %v", err)
	}
}

// Page 2: the appendContinuationItemsAction envelope parses, and the absence
// of a further token terminates the tab (Continuation == ""). Continuation
// requests are token-only — no browseId, no params.
func TestFetchChannelTabPage_ContinuationPage(t *testing.T) {
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBrowseRequest(t, r)
		if got := body["continuation"]; got != "TOK2" {
			t.Errorf("continuation: want TOK2, got %v", got)
		}
		if _, hasBrowseID := body["browseId"]; hasBrowseID {
			t.Error("continuation request must not carry a browseId")
		}
		if _, hasParams := body["params"]; hasParams {
			t.Error("continuation request must not carry params")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseVideosPage2))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", "TOK2")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if page.Continuation != "" {
		t.Errorf("exhausted tab must return empty continuation, got %q", page.Continuation)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 item, got %d: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].VideoID != "vodMember03" || page.Items[0].Title != "older vod" {
		t.Errorf("unexpected item: %+v", page.Items[0])
	}
	if want := 3 * 7 * 24 * time.Hour; page.Items[0].Age != want {
		t.Errorf("3 weeks ago: want %v, got %v", want, page.Items[0].Age)
	}
}

// browseHomeSelectedPage models a topic/auto-generated channel that lacks a
// /videos tab entirely: the harvest comes back Home-selected with a bare
// one-tab strip. The home shelf item must never leak into the videos scan.
const browseHomeSelectedPage = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Home", "selected": true, "endpoint": {"commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/featured"}}}, "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "homeShelf01", "metadata": {"lockupMetadataViewModel": {"title": {"content": "home shelf video"}}}}}}}
    ]}}}}
  ]}}
}`

// browseUploadsPlaylistPage is the VLUU… uploads-playlist browse response:
// one selected tab (no /videos identity — playlists have none), classic
// playlistVideoRenderer items plus a continuation.
const browseUploadsPlaylistPage = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"selected": true, "content": {"sectionListRenderer": {"contents": [
      {"itemSectionRenderer": {"contents": [{"playlistVideoListRenderer": {"contents": [
        {"playlistVideoRenderer": {"videoId": "uploadVid01", "title": {"runs": [{"text": "upload one"}]}}},
        {"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": "PLTOK2"}}}}
      ]}}]}}
    ]}}}}
  ]}}
}`

// browseEmptyVideosTabPage is a REAL videos tab (identity matches the
// request) that simply has no items — a channel that never uploaded.
const browseEmptyVideosTabPage = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Videos", "selected": true, "endpoint": {"commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/videos"}}}, "content": {"richGridRenderer": {"contents": []}}}}
  ]}}
}`

// browseMembershipPage1 is a members-visible membership tab: identified by
// TAB_ID_SPONSORSHIPS alone (no endpoint URL), exercising the identifier
// fallback of the tab-identity check.
const browseMembershipPage1 = `{
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Membership", "selected": true, "tabIdentifier": "TAB_ID_SPONSORSHIPS", "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "K-rKAxqjAec", "metadata": {"lockupMetadataViewModel": {"title": {"content": "members karaoke"}}}}}}}
    ]}}}}
  ]}}
}`

// A channel whose strip LACKS /videos (the topic-channel harvest above) must
// redirect to the UC→UU uploads playlist — and the Home items must not leak
// into the result.
func TestFetchChannelTabPage_NoVideosTab_UploadsPlaylistFallback(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body := decodeBrowseRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			assertHarvestRequest(t, body, "UCbrowsetest0000000000ab")
			w.Write([]byte(browseHomeSelectedPage))
		case 2:
			if got := body["browseId"]; got != "VLUUbrowsetest0000000000ab" {
				t.Errorf("request 2 browseId: want VLUU uploads playlist, got %v", got)
			}
			if _, hasParams := body["params"]; hasParams {
				t.Error("uploads-playlist browse must not carry tab params")
			}
			w.Write([]byte(browseUploadsPlaylistPage))
		default:
			t.Errorf("unexpected request %d", n)
		}
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected uploads-playlist redirect (2 requests), got %d", got)
	}
	if len(page.Items) != 1 || page.Items[0].VideoID != "uploadVid01" {
		t.Fatalf("expected only the playlist item (home items must not leak), got %+v", page.Items)
	}
	if page.Items[0].Title != "upload one" {
		t.Errorf("playlistVideoRenderer title.runs not read: %q", page.Items[0].Title)
	}
	if page.Continuation != "PLTOK2" {
		t.Errorf("continuation: want PLTOK2 from the playlist page, got %q", page.Continuation)
	}
}

// A real-but-EMPTY selected videos tab must NOT redirect to the uploads
// playlist — the tab exists and is simply exhausted. Redirecting would, on a
// streams-only channel, contaminate the videos scan with UU stream VODs.
func TestFetchChannelTabPage_EmptyVideosTab_NoFallback(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseEmptyVideosTabPage))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("real-but-empty videos tab must NOT redirect (1 request), got %d", got)
	}
	if len(page.Items) != 0 {
		t.Errorf("expected no items, got %+v", page.Items)
	}
	if page.Continuation != "" {
		t.Errorf("expected exhausted tab, got continuation %q", page.Continuation)
	}
}

// Membership short-circuit: the harvest response comes back with the
// membership tab already selected — identified by TAB_ID_SPONSORSHIPS alone —
// and parses in one request, no params ever sent.
func TestFetchChannelTabPage_MembershipSelectedShortCircuit(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assertHarvestRequest(t, decodeBrowseRequest(t, r), "UCbrowsetest0000000000ab")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseMembershipPage1))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "membership", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("membership already selected must short-circuit (1 request), got %d", got)
	}
	if len(page.Items) != 1 || page.Items[0].VideoID != "K-rKAxqjAec" {
		t.Fatalf("expected the members item, got %+v", page.Items)
	}
	if page.Continuation != "" {
		t.Errorf("expected exhausted tab, got continuation %q", page.Continuation)
	}
}

// A 200-OK body that carries NO tab strip at all — an error/alert or
// interstitial envelope — must surface as an ERROR from the page-1 harvest
// path, never as a nil-error empty page: upstream, an empty page with no
// continuation reads as clean natural exhaustion (the backfill scanner marks
// the tab Done and, on the last outstanding tab, records the channel as
// backfilled), silently classifying content YouTube never returned as
// absent. harvestTabStrip's own contract says ok=false must not be cached;
// this pins that it must not be *scanned* either.
func TestFetchChannelTabPage_AlertEnvelopeIsError(t *testing.T) {
	const alertEnvelope = `{
	  "responseContext": {"visitorData": "vd-alert"},
	  "alerts": [{"alertRenderer": {"type": "ERROR", "text": {"simpleText": "This channel does not exist."}}}]
	}`
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(alertEnvelope))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "streams", "")
	if err == nil {
		t.Fatalf("want error on a strip-less alert envelope, got nil (page %+v)", page)
	}
	// Nothing cached: the next page-1 call must retry the harvest.
	if _, ok := s.browse.cachedStrip("UCbrowsetest0000000000ab"); ok {
		t.Error("alert envelope must not be cached as the channel's tab strip")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 request (the failed harvest), got %d", got)
	}
}

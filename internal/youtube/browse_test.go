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
// envelope shapes, and the cross-call continuation session.
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

// browseVideosPage1 is a page-1 /browse response: the ytInitialData shape
// (contents.twoColumnBrowseResultsRenderer.tabs) with the Videos tab selected,
// two lockupViewModel video renderers (item shape copied from
// channel_membership_test.go), and a trailing continuationItemRenderer
// carrying token "TOK2". The second item's "Streamed 2 years ago" text is the
// itemAge coarse-date signal; the first has no signal and must rank "now".
const browseVideosPage1 = `{
  "responseContext": {"visitorData": "vd-page1"},
  "contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
    {"tabRenderer": {"title": "Videos", "selected": true, "tabIdentifier": "", "endpoint": {"commandMetadata": {"webCommandMetadata": {"url": "/channel/UCbrowsetest0000000000ab/videos"}}}, "content": {"richGridRenderer": {"contents": [
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "K-rKAxqjAec", "metadata": {"lockupMetadataViewModel": {"title": {"content": "newest upload"}}}}}}},
      {"richItemRenderer": {"content": {"lockupViewModel": {"contentId": "gr-ZTohjwnQ", "meta": "Streamed 2 years ago", "metadata": {"lockupMetadataViewModel": {"title": {"content": "old stream vod"}}}}}}},
      {"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": "TOK2"}}}}
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

// (a) Page 1: the ytInitialData envelope parses both video renderers (with
// itemAge semantics) and the continuation token, and the request carries
// browseId + the videos tab params.
func TestFetchChannelTabPage_FirstPage(t *testing.T) {
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body := decodeBrowseRequest(t, r)
		if got := body["browseId"]; got != "UCbrowsetest0000000000ab" {
			t.Errorf("browseId: want channel ID, got %v", got)
		}
		if got := body["params"]; got != "EgZ2aWRlb3M=" {
			t.Errorf("params: want videos tab params, got %v", got)
		}
		if _, hasCont := body["continuation"]; hasCont {
			t.Error("page-1 request must not carry a continuation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseVideosPage1))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "videos", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
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

// (b) Page 2: the appendContinuationItemsAction envelope parses, and the
// absence of a further token terminates the tab (Continuation == "").
func TestFetchChannelTabPage_ContinuationPage(t *testing.T) {
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBrowseRequest(t, r)
		if got := body["continuation"]; got != "TOK2" {
			t.Errorf("continuation: want TOK2, got %v", got)
		}
		if _, hasBrowseID := body["browseId"]; hasBrowseID {
			t.Error("continuation request must not carry a browseId")
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

// (c) Loop detection: a page returning an already-seen token errors with the
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

// browseHomeSelectedPage models a topic/auto-generated channel that lacks a
// /videos tab: the videos-params browse falls back to a Home-selected tab
// WITH content. The home shelf item must never leak into the videos scan.
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

// A channel LACKING /videos (Home-selected response) must redirect to the
// UC→UU uploads playlist — and the Home items must not leak into the result.
func TestFetchChannelTabPage_NoVideosTab_UploadsPlaylistFallback(t *testing.T) {
	var calls atomic.Int32
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body := decodeBrowseRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			if got := body["browseId"]; got != "UCbrowsetest0000000000ab" {
				t.Errorf("request 1 browseId: want channel ID, got %v", got)
			}
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

// The membership tab request must carry the derived membership params, and a
// TAB_ID_SPONSORSHIPS-selected response must parse.
func TestFetchChannelTabPage_MembershipParams(t *testing.T) {
	s := newBrowseTestService(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBrowseRequest(t, r)
		if got := body["browseId"]; got != "UCbrowsetest0000000000ab" {
			t.Errorf("browseId: want channel ID, got %v", got)
		}
		if got := body["params"]; got != "EgptZW1iZXJzaGlw" {
			t.Errorf("params: want membership tab params EgptZW1iZXJzaGlw, got %v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(browseMembershipPage1))
	})

	page, err := s.FetchChannelTabPage(context.Background(), "UCbrowsetest0000000000ab", "membership", "")
	if err != nil {
		t.Fatalf("FetchChannelTabPage: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].VideoID != "K-rKAxqjAec" {
		t.Fatalf("expected the members item, got %+v", page.Items)
	}
	if page.Continuation != "" {
		t.Errorf("expected exhausted tab, got continuation %q", page.Continuation)
	}
}

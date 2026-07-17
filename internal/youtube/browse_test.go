package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
    {"tabRenderer": {"title": "Videos", "selected": true, "tabIdentifier": "", "content": {"richGridRenderer": {"contents": [
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

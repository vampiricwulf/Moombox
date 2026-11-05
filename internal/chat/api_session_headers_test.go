package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// TestFetchChatSendsTheSessionHeaders: every chat API poll carries the
// Innertube headers yt-dlp's live chat downloader sends
// (downloader/youtube_live_chat.py:179, generate_api_headers) — the WEB
// client's name and version, the visitor data the context carries, and the
// signed-in account's identity off the SessionHeaders getter, re-read per
// poll. The getter is wired through ChatDownloaderOptions, as both worker
// sites set it, and its answer changes between the two polls, as it does when
// the jar comes to hold another account: the second poll must send the
// second answer, and a getter answering nothing sends no identity at all.
//
// Mutants: fetchChat not applying sessionHeaders; NewChatDownloader not
// wiring opts.SessionHeaders; the getter read once at construction; no
// X-YouTube-Client-Name/Version; no X-Goog-Visitor-Id; NewChatAPI not keeping
// visitorData.
func TestFetchChatSendsTheSessionHeaders(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	answers := []map[string]string{
		{"X-Goog-AuthUser": "1", "X-Goog-PageId": "brand-a"},
		{},
	}
	calls := 0
	dl := NewChatDownloader(ChatDownloaderOptions{
		VideoID:     "vid",
		OutputFile:  t.TempDir() + "/chat.json",
		VisitorData: "vd-chat",
		SessionHeaders: func() map[string]string {
			a := answers[min(calls, len(answers)-1)]
			calls++
			return a
		},
	})
	for _, cont := range []string{"cont-1", "cont-2"} {
		if _, err := dl.api.fetchChat(context.Background(), srv.URL, cont); err != nil {
			t.Fatalf("fetchChat(%s): %v", cont, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d polls, want 2", len(seen))
	}
	for i, h := range seen {
		if got := h.Get("X-YouTube-Client-Name"); got != constants.WebClient.ClientID {
			t.Errorf("poll %d: X-YouTube-Client-Name = %q, want %q", i, got, constants.WebClient.ClientID)
		}
		if got := h.Get("X-YouTube-Client-Version"); got != constants.WebClient.ClientVersion {
			t.Errorf("poll %d: X-YouTube-Client-Version = %q, want %q", i, got, constants.WebClient.ClientVersion)
		}
		if got := h.Get("X-Goog-Visitor-Id"); got != "vd-chat" {
			t.Errorf("poll %d: X-Goog-Visitor-Id = %q, want vd-chat", i, got)
		}
	}
	if got := seen[0].Get("X-Goog-AuthUser"); got != "1" {
		t.Errorf("poll 0: X-Goog-AuthUser = %q, want 1", got)
	}
	if got := seen[0].Get("X-Goog-PageId"); got != "brand-a" {
		t.Errorf("poll 0: X-Goog-PageId = %q, want brand-a", got)
	}
	if _, ok := seen[1][http.CanonicalHeaderKey("X-Goog-PageId")]; ok {
		t.Errorf("poll 1 sent X-Goog-PageId %q after the getter stopped naming one", seen[1].Get("X-Goog-PageId"))
	}
	if _, ok := seen[1][http.CanonicalHeaderKey("X-Goog-AuthUser")]; ok {
		t.Errorf("poll 1 sent X-Goog-AuthUser %q after the getter stopped naming one", seen[1].Get("X-Goog-AuthUser"))
	}
}

// A ChatAPI built without the getter or visitor data — tests, a cookieless
// install — sends neither identity nor visitor header, and does not panic.
//
// Mutant: fetchChat calling a nil sessionHeaders; sending an empty
// X-Goog-Visitor-Id.
func TestFetchChatWithoutASessionSendsNoIdentity(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	api := NewChatAPI("", "", nil)
	if _, err := api.fetchChat(context.Background(), srv.URL, "cont"); err != nil {
		t.Fatalf("fetchChat: %v", err)
	}
	for _, k := range []string{"X-Goog-Visitor-Id", "X-Goog-PageId", "X-Goog-AuthUser"} {
		if _, ok := seen[http.CanonicalHeaderKey(k)]; ok {
			t.Errorf("sent %s %q with no session and no visitor data", k, seen.Get(k))
		}
	}
}

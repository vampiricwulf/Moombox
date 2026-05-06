package utils

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractVideoID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Bare ID
		{"dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Standard watch URLs
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"http://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://m.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// With extra params
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=30s", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLtest", "dQw4w9WgXcQ"},
		// Short URLs
		{"https://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"http://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Live URLs
		{"https://www.youtube.com/live/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Shorts URLs
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Embed URLs
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// V URLs
		{"https://www.youtube.com/v/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// IDs with hyphens and underscores (must be exactly 11 chars)
		{"abc-_12-_AB", "abc-_12-_AB"},
		// Invalid inputs
		{"", ""},
		{"not-a-url", ""},
		{"https://example.com/watch?v=dQw4w9WgXcQ", ""},
		{"https://www.youtube.com/watch", ""},
		{"https://www.youtube.com/watch?v=tooshort", ""},
		{"https://www.youtube.com/watch?v=toolooooooong", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ExtractVideoID(tt.input)
			if result != tt.expected {
				t.Errorf("ExtractVideoID(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseYouTubeChannelURL(t *testing.T) {
	tests := []struct {
		input    string
		wantNil  bool
		wantID   string // expected ChannelID (direct extraction)
		wantPath string // expected Path (needs resolution)
	}{
		// /channel/UCxxx → direct extraction
		{"https://www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"https://youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"https://m.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		// /channel/UCxxx with trailing path
		{"https://www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx/videos", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},

		// /@Handle → needs resolution
		{"https://www.youtube.com/@SomeHandle", false, "", "/@SomeHandle"},
		{"youtube.com/@SomeHandle", false, "", "/@SomeHandle"},
		{"https://www.youtube.com/@SomeHandle/videos", false, "", "/@SomeHandle"},

		// /c/CustomName → needs resolution
		{"https://www.youtube.com/c/CustomChannel", false, "", "/c/CustomChannel"},
		{"https://www.youtube.com/c/CustomChannel/live", false, "", "/c/CustomChannel"},

		// /user/Username → needs resolution
		{"https://www.youtube.com/user/SomeUser", false, "", "/user/SomeUser"},
		{"https://www.youtube.com/user/SomeUser/videos", false, "", "/user/SomeUser"},

		// Video URLs → nil (not channel URLs)
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", true, "", ""},
		{"https://www.youtube.com/live/dQw4w9WgXcQ", true, "", ""},
		{"https://youtu.be/dQw4w9WgXcQ", true, "", ""},

		// Non-YouTube URLs → nil
		{"https://example.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", true, "", ""},
		{"https://twitch.tv/streamer", true, "", ""},

		// Bare strings → nil
		{"UCxxxxxxxxxxxxxxxxxxxxxx", true, "", ""},
		{"@SomeHandle", true, "", ""},
		{"", true, "", ""},

		// Invalid channel ID format
		{"https://www.youtube.com/channel/NotAChannelID", true, "", ""},

		// Empty handle
		{"https://www.youtube.com/@", true, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ParseYouTubeChannelURL(tt.input)
			if tt.wantNil {
				if result != nil {
					t.Errorf("ParseYouTubeChannelURL(%q) = %+v, want nil", tt.input, result)
				}
				return
			}
			if result == nil {
				t.Fatalf("ParseYouTubeChannelURL(%q) = nil, want non-nil", tt.input)
			}
			if result.ChannelID != tt.wantID {
				t.Errorf("ParseYouTubeChannelURL(%q).ChannelID = %q, want %q", tt.input, result.ChannelID, tt.wantID)
			}
			if result.Path != tt.wantPath {
				t.Errorf("ParseYouTubeChannelURL(%q).Path = %q, want %q", tt.input, result.Path, tt.wantPath)
			}
		})
	}
}

func TestIsVideoID(t *testing.T) {
	if !IsVideoID("dQw4w9WgXcQ") {
		t.Error("expected true for valid ID")
	}
	if IsVideoID("short") {
		t.Error("expected false for short string")
	}
	if IsVideoID("has spaces!!") {
		t.Error("expected false for invalid chars")
	}
}

// stubYouTubeBaseURL points youtubeBaseURL at the given test server for
// the duration of t, so ResolveYouTubeChannel hits the local server
// instead of the real YouTube origin.
func stubYouTubeBaseURL(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := youtubeBaseURL
	youtubeBaseURL = srv.URL
	t.Cleanup(func() { youtubeBaseURL = prev })
}

// TestResolveYouTubeChannelExtractsCanonical exercises the success path:
// a 200 OK page with a canonical channel link and an og:title meta.
func TestResolveYouTubeChannelExtractsCanonical(t *testing.T) {
	const channelID = "UCabc1234567890_-DEFGHIj"
	page := fmt.Sprintf(`<!doctype html><html><head>
<link rel="canonical" href="https://www.youtube.com/channel/%s">
<meta property="og:title" content="Some Channel">
</head><body></body></html>`, channelID)

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, page)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	info, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err != nil {
		t.Fatalf("ResolveYouTubeChannel: %v", err)
	}
	if info.ChannelID != channelID {
		t.Errorf("ChannelID: want %q, got %q", channelID, info.ChannelID)
	}
	if info.Name != "Some Channel" {
		t.Errorf("Name: want %q, got %q", "Some Channel", info.Name)
	}
}

// TestResolveYouTubeChannelFallsBackToExternalID covers the second
// regex branch: when the canonical link is missing, externalIDRe must
// pick the channel ID from ytInitialData JSON.
func TestResolveYouTubeChannelFallsBackToExternalID(t *testing.T) {
	const channelID = "UCxxxxxxxxxxxxxxxxxxxxxx"
	page := fmt.Sprintf(`<!doctype html><html><head>
<title>Some Channel - YouTube</title>
<script>var ytInitialData = {"externalId": "%s"};</script>
</head><body></body></html>`, channelID)

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, page)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	info, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err != nil {
		t.Fatalf("ResolveYouTubeChannel: %v", err)
	}
	if info.ChannelID != channelID {
		t.Errorf("ChannelID via fallback: want %q, got %q", channelID, info.ChannelID)
	}
	if info.Name != "Some Channel" {
		t.Errorf("Name from <title>: want %q, got %q", "Some Channel", info.Name)
	}
}

// TestResolveYouTubeChannelMissingChannelIDIsError ensures we don't
// silently return an empty ID — operators get a useful error instead.
func TestResolveYouTubeChannelMissingChannelIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `<!doctype html><html><body>nope</body></html>`)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	_, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err == nil {
		t.Fatal("ResolveYouTubeChannel with no channel ID: want error, got nil")
	}
}

// TestResolveYouTubeChannelRetriesOn5xx exercises the retry loop:
// transient 5xx errors should be retried up to twice before surfacing.
// (Note: the loop sleeps 1s + 2s, so this test takes ~3s.)
func TestResolveYouTubeChannelRetriesOn5xx(t *testing.T) {
	if testing.Short() {
		t.Skip("retry loop sleeps 1s+2s — skipping in -short mode")
	}
	const channelID = "UCabc1234567890_-DEFGHIj"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			http.Error(rw, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(rw, `<link rel="canonical" href="https://www.youtube.com/channel/%s">`, channelID)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	start := time.Now()
	info, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ResolveYouTubeChannel after retries: %v", err)
	}
	if info.ChannelID != channelID {
		t.Errorf("ChannelID: want %q, got %q", channelID, info.ChannelID)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits: want 3, got %d", got)
	}
	// 1s + 2s minimum backoff
	if elapsed < 2900*time.Millisecond {
		t.Errorf("retry elapsed: want ≥2.9s, got %v", elapsed)
	}
}

// TestResolveYouTubeChannelFastFails4xx covers the deterministic
// fast-fail branch: a 404 should NOT trigger the retry loop.
func TestResolveYouTubeChannelFastFails4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(rw, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	start := time.Now()
	_, err := ResolveYouTubeChannel(t.Context(), "/@Missing")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ResolveYouTubeChannel on 404: want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("4xx should fast-fail without retries: hits=%d, want 1", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("4xx fast-fail elapsed: want <500ms, got %v (retry loop fired?)", elapsed)
	}
}
